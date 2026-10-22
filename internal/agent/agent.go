// Package agent runs the report loop: it waits for enrollment, collects
// metrics and runs the platform-defined checks, reports them, applies the
// configuration the platform hands back and paces itself by the platform's
// interval and retry policy.
package agent

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/pentacore/lookout/internal/buildinfo"
	"github.com/pentacore/lookout/internal/platform"
	"github.com/pentacore/lookout/internal/state"
	"github.com/pentacore/lookout/internal/wire"
)

const (
	// DefaultRuntimeDir holds status.json when Deps.RuntimeDir is empty.
	DefaultRuntimeDir = "/run/lookout"

	credentialsRetry = 15 * time.Second
	runtimeDirPerm   = 0o755

	notEnrolledMsg = "not enrolled yet; run: lookout enroll --url … --token …"
	rejectedMsg    = "agent token rejected by platform; re-enroll this server"
)

// Reporter sends a report to the platform.
type Reporter interface {
	Report(ctx context.Context, token string, req wire.ReportRequest) (wire.ReportResponse, error)
}

// Collector gathers host facts and metrics. It is never called concurrently.
type Collector interface {
	Collect(ctx context.Context) (wire.Host, wire.Metrics)
}

// CheckRunner runs the platform-defined checks and returns one result per
// config, in config order.
type CheckRunner interface {
	Run(ctx context.Context, cfgs []wire.CheckConfig) []wire.CheckResult
}

// Store reads the agent's credentials and reads and writes the applied
// platform config.
type Store interface {
	LoadCredentials() (state.Credentials, error)
	LoadPlatformConfig() (state.PlatformConfig, error)
	SavePlatformConfig(state.PlatformConfig) error
}

// Deps are the agent's collaborators. Reporter, Collector, Checks and Store
// are required; New fills the rest with real defaults when they are zero.
type Deps struct {
	Reporter   Reporter
	Collector  Collector
	Checks     CheckRunner
	Store      Store
	RuntimeDir string
	Now        func() time.Time
	Sleep      func(ctx context.Context, d time.Duration) error
	Backoff    *platform.Backoff
	Logger     *slog.Logger
}

// Agent is the report loop. It is not safe for concurrent use.
type Agent struct {
	d   Deps
	log *slog.Logger

	creds         state.Credentials
	configVersion int64
	checks        []wire.CheckConfig
	interval      time.Duration
	failures      int // consecutive failed reports
	status        status
}

// New returns an Agent, filling unset optional dependencies: the system clock,
// a context-aware sleep, a fresh Backoff, a text logger on stderr and
// /run/lookout.
func New(d Deps) *Agent {
	if d.RuntimeDir == "" {
		d.RuntimeDir = DefaultRuntimeDir
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Sleep == nil {
		d.Sleep = sleep
	}
	if d.Backoff == nil {
		d.Backoff = &platform.Backoff{}
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Agent{d: d, log: d.Logger, status: status{AgentVersion: buildinfo.Version}}
}

// Run reports until ctx is cancelled, then returns nil. Platform errors,
// config problems and a missing enrollment never end it.
func (a *Agent) Run(ctx context.Context) error {
	if err := os.MkdirAll(a.d.RuntimeDir, runtimeDirPerm); err != nil {
		a.log.Warn("cannot create runtime directory", "dir", a.d.RuntimeDir, "error", err)
	}
	if !a.waitForCredentials(ctx) {
		return nil
	}
	a.loadPlatformConfig()

	for ctx.Err() == nil {
		at, err := a.report(ctx)
		if ctx.Err() != nil {
			return nil
		}
		a.recordOutcome(at, err)
		if a.d.Sleep(ctx, a.d.Backoff.Next(err, a.interval)) != nil {
			return nil
		}
		if isStatus(err, http.StatusUnauthorized) {
			// Re-enrolling (enroll --force) also resets the saved config, so
			// the new identity must not keep reporting the old one's checks.
			if !a.waitForCredentials(ctx) {
				return nil
			}
			a.loadPlatformConfig()
		}
	}
	return nil
}

// waitForCredentials loads the credentials, retrying every 15 s. Each kind of
// failure (not enrolled, unreadable) is logged and written to the status file
// once per wait. It returns false when ctx is cancelled first.
func (a *Agent) waitForCredentials(ctx context.Context) bool {
	var loggedMissing, loggedBroken bool
	for {
		creds, err := a.d.Store.LoadCredentials()
		if err == nil {
			a.creds = creds
			a.status.ServerUUID = creds.ServerUUID
			return true
		}
		missing := errors.Is(err, fs.ErrNotExist)
		switch {
		case missing && !loggedMissing:
			loggedMissing = true
			a.log.Warn(notEnrolledMsg)
			a.status.LastError = notEnrolledMsg
			a.writeStatus()
		case !missing && !loggedBroken:
			loggedBroken = true
			a.log.Error("cannot load credentials; retrying every 15s", "error", err)
			a.status.LastError = "cannot load credentials: " + err.Error()
			a.writeStatus()
		}
		if a.d.Sleep(ctx, credentialsRetry) != nil {
			return false
		}
	}
}

// loadPlatformConfig restores the last applied config. An unreadable file
// falls back to version 0 with no checks, so the platform resends the config.
func (a *Agent) loadPlatformConfig() {
	cfg, err := a.d.Store.LoadPlatformConfig()
	if err != nil {
		a.log.Error("cannot load platform config; starting from defaults", "error", err)
		cfg = state.PlatformConfig{}
	}
	a.configVersion = cfg.ConfigVersion
	a.checks = nonNil(cfg.Checks)
	a.interval = platform.ClampInterval(cfg.ReportIntervalSeconds, 0)
	a.log.Info("reporting to platform", "server_uuid", a.creds.ServerUUID,
		"config_version", a.configVersion, "checks", len(a.checks), "interval", a.interval)
}

// report runs one cycle: collect and checks concurrently, then one report.
// It returns the RFC3339 send time and the report error.
func (a *Agent) report(ctx context.Context) (string, error) {
	var (
		results []wire.CheckResult
		wg      sync.WaitGroup
	)
	checks := a.checks
	wg.Go(func() { results = a.d.Checks.Run(ctx, checks) })
	host, metrics := a.d.Collector.Collect(ctx)
	wg.Wait()

	sentAt := a.d.Now().UTC().Format(time.RFC3339)
	resp, err := a.d.Reporter.Report(ctx, a.creds.AgentToken, wire.ReportRequest{
		Schema:        wire.SchemaVersion,
		AgentVersion:  buildinfo.Version,
		SentAt:        sentAt,
		ConfigVersion: a.configVersion,
		Host:          host,
		Metrics:       metrics,
		Checks:        reportable(results),
	})
	if err == nil {
		a.apply(resp)
	}
	return sentAt, err
}

// apply takes the interval and, when present, the new config from a report
// response. The config is used in memory even when persisting it fails.
func (a *Agent) apply(resp wire.ReportResponse) {
	a.interval = platform.ClampInterval(resp.ReportIntervalSeconds, a.interval)
	if resp.Config == nil {
		return
	}
	a.configVersion = resp.ConfigVersion
	a.checks = nonNil(resp.Config.Checks)
	a.log.Info("applied platform config", "config_version", a.configVersion, "checks", len(a.checks))

	err := a.d.Store.SavePlatformConfig(state.PlatformConfig{
		ConfigVersion:         a.configVersion,
		ReportIntervalSeconds: int(a.interval / time.Second),
		Checks:                a.checks,
	})
	if err != nil {
		a.log.Error("cannot persist platform config; using it until restart", "config_version", a.configVersion, "error", err)
	}
}

// recordOutcome logs a failed report and updates the status file. Nothing it
// logs or writes contains the agent token.
func (a *Agent) recordOutcome(at string, err error) {
	a.status.LastAttemptAt = at
	if err == nil {
		a.failures = 0
		a.status.LastSuccessAt = at
		a.status.LastError = ""
		a.writeStatus()
		return
	}

	a.failures++
	token := a.creds.AgentToken
	redacted := platform.Redact(err.Error(), token)
	a.status.LastError = redacted
	if isStatus(err, http.StatusUnauthorized) {
		a.status.LastError = rejectedMsg
	}
	a.writeStatus()

	var se *platform.StatusError
	switch {
	case !errors.As(err, &se):
		a.log.Warn("report failed", "attempt", a.failures, "error", redacted)
	case se.Code == http.StatusUnauthorized:
		a.log.Error(rejectedMsg)
	case se.Code == http.StatusUnprocessableEntity:
		a.log.Warn("platform rejected the report", "status", se.Code, "body", platform.Redact(se.Body, token))
	default:
		a.log.Warn("report failed", "status", se.Code, "attempt", a.failures, "error", redacted)
	}
}

// reportable copies the check results so the report never sends null:
// checks is [] when there are none and every observed is at least {}.
func reportable(results []wire.CheckResult) []wire.CheckResult {
	out := make([]wire.CheckResult, len(results))
	for i, r := range results {
		if r.Observed == nil {
			r.Observed = map[string]any{}
		}
		out[i] = r
	}
	return out
}

func nonNil(checks []wire.CheckConfig) []wire.CheckConfig {
	if checks == nil {
		return []wire.CheckConfig{}
	}
	return checks
}

func isStatus(err error, code int) bool {
	var se *platform.StatusError
	return errors.As(err, &se) && se.Code == code
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
