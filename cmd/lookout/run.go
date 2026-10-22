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
	return runAgent(ctx, cfgPath, logger, updater.Loop)
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
func runAgent(ctx context.Context, cfgPath string, logger *slog.Logger, updaterLoop func(context.Context) error) int {
	cfg, ok := waitForConfig(ctx, cfgPath, logger)
	if !ok {
		return 0
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	a := agent.New(agent.Deps{
		Reporter:   platform.NewClient(cfg.BaseURL()),
		Collector:  newCollector(logger),
		Checks:     &checks.Runner{Registry: checks.DefaultRegistry(), Env: checks.DefaultEnv("/", cfg.RuntimeDir), Version: buildinfo.Version},
		Store:      state.Store{Dir: cfg.StateDir},
		RuntimeDir: cfg.RuntimeDir,
		Logger:     logger,
	})

	var updater sync.WaitGroup
	if cfg.AutoUpdate && updaterLoop != nil {
		updater.Go(func() {
			err := updaterLoop(ctx)
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
	updater.Wait()
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
