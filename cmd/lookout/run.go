package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pentacore/lookout/internal/agent"
	"github.com/pentacore/lookout/internal/buildinfo"
	"github.com/pentacore/lookout/internal/checks"
	"github.com/pentacore/lookout/internal/collect"
	"github.com/pentacore/lookout/internal/config"
	"github.com/pentacore/lookout/internal/platform"
	"github.com/pentacore/lookout/internal/selfupdate"
	"github.com/pentacore/lookout/internal/state"
)

// unraidPersistCopy is where the Unraid plugin keeps the binary it copies
// onto the RAM-backed root at boot; a self-update refreshes it too.
const unraidPersistCopy = "/boot/config/plugins/lookout/lookout"

// configPollInterval is how often run reloads a config that is missing or
// invalid. Tests shorten it.
var configPollInterval = 15 * time.Second

const (
	// runningMarker exists in the runtime directory while the agent runs and
	// is removed when it exits cleanly. Finding it at start means the
	// previous run crashed or was killed.
	runningMarker = "running"
	// earlyCheckMarker's mtime is the last early update check.
	earlyCheckMarker = "early-update-check"
	// earlyCheckInterval spaces early update checks out while the agent keeps
	// crashing, so a crash loop stays far below GitHub's API rate limit.
	earlyCheckInterval = 10 * time.Minute
)

// agentUpdater is the self-updater as runAgent uses it: *selfupdate.Updater.
type agentUpdater interface {
	Loop(ctx context.Context) error
	Check(ctx context.Context) (string, error)
}

// newCollector builds the host collector. Tests replace it so they do not
// query the real Docker socket and systemd.
var newCollector = func(logger *slog.Logger) agent.Collector {
	return collect.New(collect.Options{Logger: logger})
}

// runService is the `run` subcommand: it runs the agent until SIGTERM or
// SIGINT, or until a self-update has been installed.
func runService(args []string, stderr io.Writer) int {
	flags := newFlagSet("run", stderr)
	path := flags.String("config", defaultConfigPath(), "path to lookout.yaml")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	// Packages without an injected logger (internal/state) log through the default.
	slog.SetDefault(logger)

	cfgPath, err := leaveWorkingDir(*path)
	if err != nil {
		logger.Warn("staying in the current working directory", "error", err)
	}

	updater := &selfupdate.Updater{Logger: logger}
	if config.DetectPlatform("/") == config.PlatformUnraid {
		updater.PersistCopy = unraidPersistCopy
	}
	return runAgent(ctx, cfgPath, logger, updater)
}

// leaveWorkingDir changes to / so that the long-running agent never keeps
// the directory it was started from busy (an Unraid disk or the flash drive
// that is about to be unmounted). It returns cfgPath made absolute first. On
// an error the working directory and cfgPath are left as they were; that is
// not a reason to stop the agent.
func leaveWorkingDir(cfgPath string) (string, error) {
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return cfgPath, fmt.Errorf("resolve %s: %w", cfgPath, err)
	}
	if err := os.Chdir("/"); err != nil {
		return cfgPath, fmt.Errorf("change to /: %w", err)
	}
	return abs, nil
}

// runAgent waits for a valid config, then runs the report loop. It returns 0
// when ctx is cancelled and after a self-update was installed (so the service
// manager restarts the new binary). Nothing else ends it: a missing or
// invalid config, a missing enrollment, platform errors and an updater that
// stops without installing (a development build) keep it running.
//
// When the previous run did not exit cleanly (see runningMarker) and
// auto_update is on, it checks for an update before the first report, at
// most once per earlyCheckInterval: a release that crashes within minutes of
// starting would otherwise never reach the updater's first check and could
// not be fixed by publishing a new release.
func runAgent(ctx context.Context, cfgPath string, logger *slog.Logger, updater agentUpdater) int {
	cfg, ok := waitForConfig(ctx, cfgPath, logger)
	if !ok {
		return 0
	}

	// Not deferred: a panic must leave the marker behind.
	abnormal := markRunning(cfg.RuntimeDir, logger)
	if abnormal && cfg.AutoUpdate && updater != nil && claimEarlyCheck(cfg.RuntimeDir, logger) {
		if earlyUpdate(ctx, updater, logger) {
			clearRunning(cfg.RuntimeDir, logger)
			return 0
		}
	}
	code := runReportLoop(ctx, cfg, cfgPath, logger, updater)
	clearRunning(cfg.RuntimeDir, logger)
	return code
}

// earlyUpdate runs one update check and reports whether runAgent should exit:
// an update was installed or ctx was cancelled.
func earlyUpdate(ctx context.Context, updater agentUpdater, logger *slog.Logger) bool {
	logger.Warn("the previous run did not exit cleanly; checking for an update before reporting")
	version, err := updater.Check(ctx)
	switch {
	case err == nil && version != "":
		logger.Info("updated; exiting so the service manager restarts the new binary", "version", version)
		return true
	case ctx.Err() != nil:
		return true
	case errors.Is(err, selfupdate.ErrDevBuild), errors.Is(err, selfupdate.ErrNoPublicKey):
		// The updater loop logs why updates are disabled.
	case err != nil:
		logger.Warn("early self-update check failed", "error", err)
	}
	return false
}

// markRunning creates runningMarker and reports whether it was already there.
func markRunning(dir string, logger *slog.Logger) bool {
	path := filepath.Join(dir, runningMarker)
	_, err := os.Stat(path)
	existed := err == nil
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger.Debug("cannot create runtime directory", "dir", dir, "error", err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		logger.Debug("cannot write run marker", "path", path, "error", err)
	}
	return existed
}

func clearRunning(dir string, logger *slog.Logger) {
	if err := os.Remove(filepath.Join(dir, runningMarker)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logger.Debug("cannot remove run marker", "dir", dir, "error", err)
	}
}

// claimEarlyCheck reports whether an early update check is due, recording it
// when it is. Without a record of the last check there is no early check, so
// an unwritable runtime directory cannot turn a crash loop into a stream of
// API requests.
func claimEarlyCheck(dir string, logger *slog.Logger) bool {
	path := filepath.Join(dir, earlyCheckMarker)
	if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) < earlyCheckInterval {
		return false
	}
	now := time.Now()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		logger.Debug("cannot record the early update check", "path", path, "error", err)
		return false
	}
	if err := os.Chtimes(path, now, now); err != nil {
		logger.Debug("cannot record the early update check", "path", path, "error", err)
		return false
	}
	return true
}

// reporterFor returns a function that builds a platform client for the
// platform_url in the config at path as it is when called.
func reporterFor(path string) func() (agent.Reporter, error) {
	return func() (agent.Reporter, error) {
		cfg, err := config.Load(path, config.DetectPlatform("/"))
		if err == nil {
			err = cfg.Validate()
		}
		if err != nil {
			return nil, err
		}
		return platform.NewClient(cfg.BaseURL()), nil
	}
}

// runReportLoop runs the agent and, with auto_update, the updater loop until
// ctx is cancelled or an update was installed.
func runReportLoop(ctx context.Context, cfg config.Config, cfgPath string, logger *slog.Logger, updater agentUpdater) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	a := agent.New(agent.Deps{
		NewReporter: reporterFor(cfgPath),
		Collector:   newCollector(logger),
		Checks:      &checks.Runner{Registry: checks.DefaultRegistry(), Env: checks.DefaultEnv("/", cfg.RuntimeDir), Version: buildinfo.Version},
		Store:       state.Store{Dir: cfg.StateDir},
		RuntimeDir:  cfg.RuntimeDir,
		Logger:      logger,
	})

	var updating sync.WaitGroup
	if cfg.AutoUpdate && updater != nil {
		updating.Go(func() {
			err := updater.Loop(ctx)
			switch {
			case errors.Is(err, selfupdate.ErrRestart):
				logger.Info("updated; exiting so the service manager restarts the new binary")
				cancel()
			case err != nil:
				logger.Warn("self-update stopped", "error", err)
			}
		})
	}

	_ = a.Run(ctx)
	cancel()
	updating.Wait()
	return 0
}

// waitForConfig loads the config until it is usable, polling every
// configPollInterval. Each distinct problem is logged once. It returns false
// when ctx is cancelled first.
func waitForConfig(ctx context.Context, path string, logger *slog.Logger) (config.Config, bool) {
	var lastProblem, lastUnknown string
	for {
		cfg, err := config.Load(path, config.DetectPlatform("/"))
		notConfigured := errors.Is(err, fs.ErrNotExist)
		if err == nil {
			err = cfg.Validate()
			notConfigured = cfg.PlatformURL == ""
		}

		if unknown := strings.Join(cfg.UnknownKeys, ","); unknown != lastUnknown {
			lastUnknown = unknown
			if unknown != "" {
				logger.Warn("ignoring unknown config keys", "path", path, "keys", cfg.UnknownKeys)
			}
		}
		if err == nil {
			return cfg, true
		}

		if problem := err.Error(); problem != lastProblem {
			lastProblem = problem
			if notConfigured {
				logger.Warn("waiting for configuration; set platform_url or run: lookout enroll --url … --token …", "path", path, "reason", problem)
			} else {
				logger.Error("invalid configuration; retrying every 15s", "path", path, "error", problem)
			}
		}

		timer := time.NewTimer(configPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return config.Config{}, false
		case <-timer.C:
		}
	}
}
