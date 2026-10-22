package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pentacore/lookout/internal/platform"
	"github.com/pentacore/lookout/internal/state"
	"github.com/pentacore/lookout/internal/wire"
)

const testToken = "srv.secret"

var (
	testCreds = state.Credentials{ServerUUID: "srv", AgentToken: testToken}
	testNow   = time.Date(2026, 10, 22, 14, 0, 0, 0, time.FixedZone("CEST", 2*60*60))
	testHost  = wire.Host{Hostname: "babel", OS: "unraid 7.1.4", Kernel: "6.12.24-Unraid", Arch: "amd64", UptimeSeconds: 86400}
	check1    = wire.CheckConfig{ID: "c1", Type: "mount_present", Params: json.RawMessage(`{"path":"/mnt/backups"}`)}
)

type reportStep struct {
	resp wire.ReportResponse
	err  error
}

type credStep struct {
	creds state.Credentials
	err   error
}

// harness fakes every agent dependency and records what the agent did.
type harness struct {
	t *testing.T

	mu     sync.Mutex
	events []string

	credSteps  []credStep // consumed in order; afterwards testCreds
	cfg        state.PlatformConfig
	cfgErr     error
	saveErr    error
	saved      []state.PlatformConfig
	reports    []reportStep // consumed in order; afterwards an empty 200
	requests   []wire.ReportRequest
	tokens     []string
	runnerCfgs [][]wire.CheckConfig
	results    []wire.CheckResult
	nilResults bool

	// collectWaitsForRunner makes Collect block until the runner has started
	// (and vice versa), proving they run concurrently.
	collectWaitsForRunner bool
	runnerStarted         chan struct{}
	collectStarted        chan struct{}

	sleeps     []time.Duration
	maxSleeps  int
	onSleep    func(n int)
	onReport   func(n int)
	cancel     context.CancelFunc
	runtimeDir string
	backoff    *platform.Backoff
	log        bytes.Buffer
}

func newHarness(t *testing.T, maxSleeps int) *harness {
	t.Helper()
	return &harness{
		t:          t,
		maxSleeps:  maxSleeps,
		cfg:        state.PlatformConfig{ReportIntervalSeconds: 60, Checks: []wire.CheckConfig{}},
		results:    []wire.CheckResult{{ID: "c0", Type: "mount_present", Status: wire.StatusOK, Message: "fine", Observed: map[string]any{"mounted": true}}},
		runtimeDir: t.TempDir(),
		backoff:    &platform.Backoff{Rand: func() float64 { return 1 }},
	}
}

func (h *harness) event(s string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, s)
}

func (h *harness) LoadCredentials() (state.Credentials, error) {
	h.event("load-credentials")
	if len(h.credSteps) > 0 {
		s := h.credSteps[0]
		h.credSteps = h.credSteps[1:]
		return s.creds, s.err
	}
	return testCreds, nil
}

func (h *harness) LoadPlatformConfig() (state.PlatformConfig, error) {
	h.event("load-config")
	return h.cfg, h.cfgErr
}

func (h *harness) SavePlatformConfig(c state.PlatformConfig) error {
	h.event("save-config")
	h.saved = append(h.saved, c)
	return h.saveErr
}

func (h *harness) Collect(context.Context) (wire.Host, wire.Metrics) {
	if h.collectWaitsForRunner {
		close(h.collectStarted)
		select {
		case <-h.runnerStarted:
		case <-time.After(5 * time.Second):
			h.t.Error("Collect: runner did not start while collecting")
		}
	}
	return testHost, wire.Metrics{CPUPercent: 12.5, Filesystems: []wire.Filesystem{}}
}

func (h *harness) Run(_ context.Context, cfgs []wire.CheckConfig) []wire.CheckResult {
	if h.collectWaitsForRunner {
		close(h.runnerStarted)
		select {
		case <-h.collectStarted:
		case <-time.After(5 * time.Second):
			h.t.Error("Run: collector did not start while running checks")
		}
	}
	h.mu.Lock()
	h.runnerCfgs = append(h.runnerCfgs, cfgs)
	h.mu.Unlock()
	if h.nilResults {
		return nil
	}
	return h.results
}

func (h *harness) Report(_ context.Context, token string, req wire.ReportRequest) (wire.ReportResponse, error) {
	h.event("report")
	h.requests = append(h.requests, req)
	h.tokens = append(h.tokens, token)
	if h.onReport != nil {
		h.onReport(len(h.requests))
	}
	if len(h.reports) > 0 {
		s := h.reports[0]
		h.reports = h.reports[1:]
		return s.resp, s.err
	}
	return wire.ReportResponse{ReportIntervalSeconds: 60}, nil
}

func (h *harness) sleep(ctx context.Context, d time.Duration) error {
	h.event("sleep " + d.String())
	h.sleeps = append(h.sleeps, d)
	if h.onSleep != nil {
		h.onSleep(len(h.sleeps))
	}
	if len(h.sleeps) >= h.maxSleeps {
		h.cancel()
	}
	return ctx.Err()
}

// run starts the agent and waits for Run to return.
func (h *harness) run() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.runWith(ctx, cancel)
}

func (h *harness) runWith(ctx context.Context, cancel context.CancelFunc) {
	h.t.Helper()
	h.cancel = cancel
	a := New(Deps{
		Reporter:   h,
		Collector:  h,
		Checks:     h,
		Store:      h,
		RuntimeDir: h.runtimeDir,
		Now:        func() time.Time { return testNow },
		Sleep:      h.sleep,
		Backoff:    h.backoff,
		Logger:     slog.New(slog.NewTextHandler(&h.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			h.t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		h.t.Fatal("Run did not return after the context was cancelled")
	}
}

// readStatus returns the status file. It reports failures with Errorf, not
// Fatalf, because onSleep hooks call it from the Run goroutine.
func (h *harness) readStatus() status {
	h.t.Helper()
	var s status
	data, err := os.ReadFile(filepath.Join(h.runtimeDir, "status.json"))
	if err != nil {
		h.t.Errorf("read status.json: %v", err)
		return s
	}
	if err := json.Unmarshal(data, &s); err != nil {
		h.t.Errorf("parse status.json %q: %v", data, err)
	}
	return s
}

func statusErr(code int, body string) error {
	return &platform.StatusError{Code: code, Body: body}
}

func TestAgentWaitsForEnrollment(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantLog    string
		level      string
		wantStatus string
	}{
		{"not enrolled", fmt.Errorf("open credentials.json: %w", fs.ErrNotExist), "not enrolled yet; run: lookout enroll --url … --token …", "level=WARN", "not enrolled"},
		{"corrupt credentials", errors.New("parse credentials.json: unexpected end of JSON input"), "cannot load credentials", "level=ERROR", "cannot load credentials"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, 3)
			h.credSteps = []credStep{{err: tt.err}, {err: tt.err}}
			var waiting status
			h.onSleep = func(n int) {
				if n == 1 {
					waiting = h.readStatus()
				}
			}
			h.run()

			if !strings.Contains(waiting.LastError, tt.wantStatus) || waiting.AgentVersion != "dev" {
				t.Errorf("status while waiting = %+v, want last_error mentioning %q", waiting, tt.wantStatus)
			}

			if want := []time.Duration{15 * time.Second, 15 * time.Second, time.Minute}; !reflect.DeepEqual(h.sleeps, want) {
				t.Errorf("sleeps = %v, want %v", h.sleeps, want)
			}
			if len(h.requests) != 1 {
				t.Errorf("reports = %d, want 1", len(h.requests))
			}
			logs := h.log.String()
			if n := strings.Count(logs, tt.wantLog); n != 1 {
				t.Errorf("log has %q %d times, want once:\n%s", tt.wantLog, n, logs)
			}
			if !strings.Contains(logs, tt.level) {
				t.Errorf("log lacks %s:\n%s", tt.level, logs)
			}
		})
	}
}

func TestAgentFirstReport(t *testing.T) {
	h := newHarness(t, 1)
	h.run()

	if len(h.requests) != 1 {
		t.Fatalf("reports = %d, want 1", len(h.requests))
	}
	req := h.requests[0]
	if req.Schema != 1 || req.ConfigVersion != 0 || req.AgentVersion != "dev" || req.SentAt != "2026-10-22T12:00:00Z" {
		t.Errorf("request header = schema %d, config_version %d, agent_version %q, sent_at %q",
			req.Schema, req.ConfigVersion, req.AgentVersion, req.SentAt)
	}
	if h.tokens[0] != testToken {
		t.Errorf("token = %q, want %q", h.tokens[0], testToken)
	}
	if req.Host != testHost || req.Metrics.CPUPercent != 12.5 {
		t.Errorf("host/metrics = %+v / %+v, want the collector output", req.Host, req.Metrics)
	}
	if !reflect.DeepEqual(req.Checks, h.results) {
		t.Errorf("checks = %+v, want the runner output %+v", req.Checks, h.results)
	}
	if got := h.runnerCfgs[0]; got == nil || len(got) != 0 {
		t.Errorf("runner got %#v, want an empty non-nil slice", got)
	}
}

func TestAgentCollectsAndChecksConcurrently(t *testing.T) {
	h := newHarness(t, 1)
	h.collectWaitsForRunner = true
	h.runnerStarted = make(chan struct{})
	h.collectStarted = make(chan struct{})
	h.run()

	if len(h.requests) != 1 {
		t.Fatalf("reports = %d, want 1", len(h.requests))
	}
}

func TestAgentReportNeverSendsNull(t *testing.T) {
	tests := []struct {
		name       string
		nilResults bool
		results    []wire.CheckResult
		wantChecks string
	}{
		{"nil runner output", true, nil, `"checks":[]`},
		{"nil observed", false, []wire.CheckResult{{ID: "c1", Type: "x", Status: wire.StatusUnsupported}}, `"observed":{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, 1)
			h.nilResults = tt.nilResults
			h.results = tt.results
			h.run()

			data, err := json.Marshal(h.requests[0])
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), tt.wantChecks) {
				t.Errorf("request %s lacks %s", data, tt.wantChecks)
			}
		})
	}
}

func TestAgentAppliesNewConfig(t *testing.T) {
	h := newHarness(t, 2)
	h.reports = []reportStep{{resp: wire.ReportResponse{
		ConfigVersion:         4,
		Config:                &wire.PlatformConfig{Checks: []wire.CheckConfig{check1}},
		ReportIntervalSeconds: 60,
	}}}
	h.run()

	if len(h.requests) != 2 {
		t.Fatalf("reports = %d, want 2", len(h.requests))
	}
	if got := h.requests[1].ConfigVersion; got != 4 {
		t.Errorf("second config_version = %d, want 4", got)
	}
	if want := []wire.CheckConfig{check1}; !reflect.DeepEqual(h.runnerCfgs[1], want) {
		t.Errorf("runner got %+v, want %+v", h.runnerCfgs[1], want)
	}
	want := []state.PlatformConfig{{ConfigVersion: 4, ReportIntervalSeconds: 60, Checks: []wire.CheckConfig{check1}}}
	if !reflect.DeepEqual(h.saved, want) {
		t.Errorf("saved = %+v, want %+v", h.saved, want)
	}
}

func TestAgentAppliesEmptyConfig(t *testing.T) {
	h := newHarness(t, 2)
	h.cfg = state.PlatformConfig{ConfigVersion: 3, ReportIntervalSeconds: 60, Checks: []wire.CheckConfig{check1}}
	h.reports = []reportStep{{resp: wire.ReportResponse{ConfigVersion: 5, Config: &wire.PlatformConfig{}, ReportIntervalSeconds: 60}}}
	h.run()

	if got := h.requests[0].ConfigVersion; got != 3 {
		t.Errorf("first config_version = %d, want the loaded 3", got)
	}
	if got := h.runnerCfgs[1]; got == nil || len(got) != 0 {
		t.Errorf("runner got %#v after a null checks list, want an empty non-nil slice", got)
	}
	if len(h.saved) != 1 || h.saved[0].Checks == nil || h.saved[0].ConfigVersion != 5 {
		t.Errorf("saved = %#v, want version 5 with an empty checks list", h.saved)
	}
}

func TestAgentNullConfigDoesNotPersist(t *testing.T) {
	h := newHarness(t, 3)
	h.run()

	if len(h.requests) != 3 {
		t.Fatalf("reports = %d, want 3", len(h.requests))
	}
	if len(h.saved) != 0 {
		t.Errorf("SavePlatformConfig called %d times, want 0", len(h.saved))
	}
}

func TestAgentPersistFailureStillApplies(t *testing.T) {
	h := newHarness(t, 2)
	h.saveErr = errors.New("read-only file system")
	h.reports = []reportStep{{resp: wire.ReportResponse{ConfigVersion: 4, Config: &wire.PlatformConfig{Checks: []wire.CheckConfig{check1}}}}}
	h.run()

	if got := h.requests[1].ConfigVersion; got != 4 {
		t.Errorf("second config_version = %d, want 4", got)
	}
	if !reflect.DeepEqual(h.runnerCfgs[1], []wire.CheckConfig{check1}) {
		t.Errorf("runner got %+v, want [c1]", h.runnerCfgs[1])
	}
	logs := h.log.String()
	if !strings.Contains(logs, "persist") || !strings.Contains(logs, "level=ERROR") {
		t.Errorf("log lacks an error about persisting:\n%s", logs)
	}
}

func TestAgentReportInterval(t *testing.T) {
	tests := []struct {
		name        string
		cfgInterval int
		respSeconds int
		want        time.Duration
	}{
		{"below minimum", 60, 5, 10 * time.Second},
		{"above maximum", 60, 7200, 3600 * time.Second},
		{"zero keeps previous", 60, 0, 60 * time.Second},
		{"negative keeps previous", 60, -5, 60 * time.Second},
		{"in range", 60, 30, 30 * time.Second},
		{"saved interval is clamped", 1, 0, 10 * time.Second},
		{"saved zero interval defaults", 0, 0, 60 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, 1)
			h.cfg.ReportIntervalSeconds = tt.cfgInterval
			h.reports = []reportStep{{resp: wire.ReportResponse{ReportIntervalSeconds: tt.respSeconds}}}
			h.run()

			if want := []time.Duration{tt.want}; !reflect.DeepEqual(h.sleeps, want) {
				t.Errorf("sleeps = %v, want %v", h.sleeps, want)
			}
		})
	}
}

func TestAgentCorruptPlatformConfigUsesDefaults(t *testing.T) {
	h := newHarness(t, 1)
	h.cfg = state.PlatformConfig{}
	h.cfgErr = errors.New("parse platform-config.json: unexpected EOF")
	h.run()

	if len(h.requests) != 1 || h.requests[0].ConfigVersion != 0 {
		t.Fatalf("requests = %+v, want one report with config_version 0", h.requests)
	}
	if got := h.runnerCfgs[0]; got == nil || len(got) != 0 {
		t.Errorf("runner got %#v, want an empty non-nil slice", got)
	}
	if want := []time.Duration{time.Minute}; !reflect.DeepEqual(h.sleeps, want) {
		t.Errorf("sleeps = %v, want %v", h.sleeps, want)
	}
	if !strings.Contains(h.log.String(), "level=ERROR") {
		t.Errorf("log lacks an error for the unreadable config:\n%s", h.log.String())
	}
}

func TestAgent401(t *testing.T) {
	h := newHarness(t, 2)
	h.reports = []reportStep{{err: statusErr(401, `{"message":"Unauthenticated."}`)}}
	var rejected status
	h.onSleep = func(n int) {
		if n == 1 {
			rejected = h.readStatus()
		}
	}
	h.run()

	if h.sleeps[0] != 15*time.Minute {
		t.Errorf("sleep after 401 = %v, want 15m", h.sleeps[0])
	}
	if !strings.Contains(rejected.LastError, "re-enroll") {
		t.Errorf("status after 401 = %+v, want last_error telling to re-enroll", rejected)
	}
	want := []string{"load-credentials", "load-config", "report", "sleep 15m0s", "load-credentials", "load-config", "report", "sleep 1m0s"}
	if !reflect.DeepEqual(h.events, want) {
		t.Errorf("events = %v, want %v", h.events, want)
	}
	logs := h.log.String()
	if !strings.Contains(logs, "agent token rejected by platform; re-enroll this server") || !strings.Contains(logs, "level=ERROR") {
		t.Errorf("log lacks the re-enroll error:\n%s", logs)
	}
}

func TestAgent401PicksUpNewCredentials(t *testing.T) {
	h := newHarness(t, 4)
	h.cfg = state.PlatformConfig{ConfigVersion: 3, ReportIntervalSeconds: 60, Checks: []wire.CheckConfig{check1}}
	h.reports = []reportStep{{err: statusErr(401, "")}}
	newCreds := state.Credentials{ServerUUID: "new", AgentToken: "new.token"}
	h.credSteps = []credStep{
		{creds: testCreds},
		{err: fs.ErrNotExist},
		{creds: newCreds},
	}
	// lookout enroll --force resets the saved config while the agent sleeps.
	check2 := wire.CheckConfig{ID: "c2", Type: "path_on_mount", Params: json.RawMessage(`{"path":"/a","mount":"/"}`)}
	h.onSleep = func(n int) {
		if n == 1 {
			h.cfg = state.PlatformConfig{ConfigVersion: 0, ReportIntervalSeconds: 60, Checks: []wire.CheckConfig{check2}}
		}
	}
	h.run()

	want := []time.Duration{15 * time.Minute, 15 * time.Second, time.Minute}
	if !reflect.DeepEqual(h.sleeps[:3], want) {
		t.Errorf("sleeps = %v, want prefix %v", h.sleeps, want)
	}
	if !reflect.DeepEqual(h.tokens, []string{testToken, "new.token", "new.token"}) {
		t.Errorf("tokens = %v, want the new token after re-enrolling", h.tokens)
	}
	if got := h.requests[0].ConfigVersion; got != 3 {
		t.Errorf("first config_version = %d, want 3", got)
	}
	if got := h.requests[1].ConfigVersion; got != 0 {
		t.Errorf("config_version after re-enrolling = %d, want the reloaded 0", got)
	}
	if !reflect.DeepEqual(h.runnerCfgs[1], []wire.CheckConfig{check2}) {
		t.Errorf("runner got %+v after re-enrolling, want the reloaded [c2]", h.runnerCfgs[1])
	}
}

func TestAgentBackoffOn5xx(t *testing.T) {
	h := newHarness(t, 4)
	h.reports = []reportStep{
		{err: statusErr(503, "")},
		{err: statusErr(503, "")},
		{err: statusErr(503, "")},
	}
	h.run()

	for i := 1; i < 3; i++ {
		if h.sleeps[i] <= h.sleeps[i-1] {
			t.Errorf("sleeps = %v, want strictly increasing for the 503s", h.sleeps)
		}
	}
	if h.sleeps[3] != time.Minute {
		t.Errorf("sleep after recovery = %v, want the 1m interval", h.sleeps[3])
	}
	logs := h.log.String()
	for _, want := range []string{"status=503 attempt=1", "status=503 attempt=3"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
}

func TestAgent422LogsRedactedBody(t *testing.T) {
	h := newHarness(t, 1)
	h.reports = []reportStep{{err: statusErr(422, `{"message":"metrics.load is invalid","token":"srv.secret"}`)}}
	h.run()

	logs := h.log.String()
	if !strings.Contains(logs, "metrics.load is invalid") || !strings.Contains(logs, "[redacted]") || !strings.Contains(logs, "level=WARN") {
		t.Errorf("log lacks the redacted 422 body at warn:\n%s", logs)
	}
	if h.sleeps[0] != time.Minute {
		t.Errorf("sleep after 422 = %v, want the 1m interval", h.sleeps[0])
	}
}

func TestAgentNeverLogsToken(t *testing.T) {
	h := newHarness(t, 5)
	h.reports = []reportStep{
		{err: statusErr(401, "Authorization: Bearer srv.secret")},
		{err: statusErr(422, `{"headers":{"authorization":["Bearer srv.secret"]},"echo":"secret"}`)},
		{err: statusErr(503, "upstream said srv.secret")},
		{err: errors.New("dial tcp: connection reset while sending srv.secret")},
	}
	h.run()

	logs := h.log.String()
	for _, leak := range []string{testToken, "secret"} {
		if strings.Contains(logs, leak) {
			t.Errorf("log contains %q:\n%s", leak, logs)
		}
	}
	data, err := os.ReadFile(filepath.Join(h.runtimeDir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") {
		t.Errorf("status.json contains the token: %s", data)
	}
}

func TestAgentWritesStatusFile(t *testing.T) {
	h := newHarness(t, 2)
	h.runtimeDir = filepath.Join(t.TempDir(), "run", "lookout") // F7: Run creates it
	h.reports = []reportStep{{}, {err: statusErr(503, "")}}
	var seen []status
	h.onSleep = func(int) { seen = append(seen, h.readStatus()) }
	h.run()

	if len(seen) != 2 {
		t.Fatalf("status snapshots = %d, want 2", len(seen))
	}
	ok := seen[0]
	want := status{AgentVersion: "dev", ServerUUID: "srv", LastAttemptAt: "2026-10-22T12:00:00Z", LastSuccessAt: "2026-10-22T12:00:00Z"}
	if ok != want {
		t.Errorf("status after success = %+v, want %+v", ok, want)
	}
	failed := seen[1]
	if failed.LastSuccessAt != "2026-10-22T12:00:00Z" || !strings.Contains(failed.LastError, "503") {
		t.Errorf("status after 503 = %+v, want last_success_at kept and last_error mentioning 503", failed)
	}
	info, err := os.Stat(filepath.Join(h.runtimeDir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("status.json mode = %o, want 644", perm)
	}
	data, err := os.ReadFile(filepath.Join(h.runtimeDir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"agent_version", "server_uuid", "last_attempt_at", "last_success_at", "last_error"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("status.json lacks %q: %s", k, data)
		}
	}
}

func TestAgentStatusWriteFailureKeepsReporting(t *testing.T) {
	h := newHarness(t, 2)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.runtimeDir = blocker
	h.run()

	if len(h.requests) != 2 {
		t.Errorf("reports = %d, want 2", len(h.requests))
	}
	logs := h.log.String()
	if !strings.Contains(logs, `level=WARN msg="cannot create runtime directory"`) {
		t.Errorf("log lacks the runtime directory warning:\n%s", logs)
	}
	if !strings.Contains(logs, "level=DEBUG") || strings.Contains(logs, "level=ERROR") {
		t.Errorf("status write failure should log at debug only:\n%s", logs)
	}
}

func TestAgentStopsOnCancel(t *testing.T) {
	t.Run("while waiting for enrollment", func(t *testing.T) {
		h := newHarness(t, 1)
		h.credSteps = []credStep{{err: fs.ErrNotExist}}
		h.run()
		if len(h.requests) != 0 {
			t.Errorf("reports = %d, want 0", len(h.requests))
		}
	})

	t.Run("already cancelled", func(t *testing.T) {
		h := newHarness(t, 1)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		h.runWith(ctx, cancel)
		if len(h.requests) != 0 {
			t.Errorf("reports = %d, want 0", len(h.requests))
		}
	})

	t.Run("during a report", func(t *testing.T) {
		h := newHarness(t, 10)
		h.onReport = func(int) { h.cancel() }
		h.reports = []reportStep{{err: context.Canceled}}
		h.run()
		if len(h.requests) != 1 || len(h.sleeps) != 0 {
			t.Errorf("reports = %d, sleeps = %d, want 1 and 0", len(h.requests), len(h.sleeps))
		}
		if strings.Contains(h.log.String(), "level=WARN") {
			t.Errorf("a cancelled report should not warn:\n%s", h.log.String())
		}
	})
}

func TestNewFillsDefaults(t *testing.T) {
	a := New(Deps{})
	if a.d.Now == nil || a.d.Sleep == nil || a.d.Backoff == nil || a.d.Logger == nil || a.d.RuntimeDir == "" {
		t.Errorf("New left defaults unset: %+v", a.d)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.d.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("default Sleep on a cancelled ctx = %v, want context.Canceled", err)
	}
	if err := a.d.Sleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("default Sleep = %v, want nil", err)
	}
}
