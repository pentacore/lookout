package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pentacore/lookout/internal/agent"
	"github.com/pentacore/lookout/internal/selfupdate"
	"github.com/pentacore/lookout/internal/wire"
)

// syncBuffer is a bytes.Buffer that is safe to log into from several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// reportServer fakes POST /report and signals each request on the channel.
func reportServer(t *testing.T) (*httptest.Server, <-chan string) {
	t.Helper()
	requests := make(chan string, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(wire.ReportResponse{ReportIntervalSeconds: 3600})
	}))
	t.Cleanup(srv.Close)
	return srv, requests
}

type runningAgent struct {
	cancel context.CancelFunc
	done   chan int
	log    *syncBuffer
}

// fakeCollector reports an empty host without touching Docker or systemd.
type fakeCollector struct{}

func (fakeCollector) Collect(context.Context) (wire.Host, wire.Metrics) {
	return wire.Host{Hostname: "test"}, wire.Metrics{}
}

// fakeUpdater stands in for *selfupdate.Updater. A nil check reports that
// the running version is the latest.
type fakeUpdater struct {
	loop   func(context.Context) error
	check  func(context.Context) (string, error)
	checks atomic.Int32
}

func (u *fakeUpdater) Loop(ctx context.Context) error {
	if u.loop == nil {
		<-ctx.Done()
		return nil
	}
	return u.loop(ctx)
}

func (u *fakeUpdater) Check(ctx context.Context) (string, error) {
	u.checks.Add(1)
	if u.check == nil {
		return "", nil
	}
	return u.check(ctx)
}

// startAgent runs runAgent in the background with a short config poll and,
// unless updaterLoop is nil, an updater whose Loop is updaterLoop.
func startAgent(t *testing.T, cfgPath string, updaterLoop func(context.Context) error) *runningAgent {
	t.Helper()
	if updaterLoop == nil {
		return startAgentWith(t, cfgPath, nil)
	}
	return startAgentWith(t, cfgPath, &fakeUpdater{loop: updaterLoop})
}

// startAgentWith runs runAgent in the background with a short config poll.
func startAgentWith(t *testing.T, cfgPath string, updater agentUpdater) *runningAgent {
	t.Helper()
	oldCollector := newCollector
	newCollector = func(*slog.Logger) agent.Collector { return fakeCollector{} }
	t.Cleanup(func() { newCollector = oldCollector })
	old := configPollInterval
	configPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { configPollInterval = old })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	log := &syncBuffer{}
	done := make(chan int, 1)
	go func() { done <- runAgent(ctx, cfgPath, slog.New(slog.NewTextHandler(log, nil)), updater) }()
	return &runningAgent{cancel: cancel, done: done, log: log}
}

func (a *runningAgent) wait(t *testing.T) int {
	t.Helper()
	select {
	case code := <-a.done:
		return code
	case <-time.After(5 * time.Second):
		t.Fatalf("runAgent did not return; log:\n%s", a.log.String())
		return -1
	}
}

func awaitRequest(t *testing.T, requests <-chan string, a *runningAgent) string {
	t.Helper()
	select {
	case auth := <-requests:
		return auth
	case <-time.After(5 * time.Second):
		t.Fatalf("no report arrived; log:\n%s", a.log.String())
		return ""
	}
}

func TestRunAgentWaitsThenReports(t *testing.T) {
	srv, requests := reportServer(t)
	env := newTestEnv(t, "")
	a := startAgent(t, env.config, nil)

	select {
	case <-requests:
		t.Fatal("reported without a configured platform_url")
	case <-time.After(100 * time.Millisecond):
	}
	if got := strings.Count(a.log.String(), "waiting for configuration"); got != 1 {
		t.Errorf("logged the wait %d times, want once:\n%s", got, a.log.String())
	}

	env.saveCredentials("srv-1")
	env.writeConfig("platform_url: " + srv.URL + "\nstate_dir: " + env.store.Dir + "\nruntime_dir: " + env.dir + "/run\n")

	if auth := awaitRequest(t, requests, a); auth != "Bearer srv-1.secret" {
		t.Errorf("Authorization = %q", auth)
	}
	a.cancel()
	if code := a.wait(t); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestRunAgentLogsInvalidConfigOnce(t *testing.T) {
	env := newTestEnv(t, "http://x.example")
	a := startAgent(t, env.config, nil)

	time.Sleep(100 * time.Millisecond)
	a.cancel()
	if code := a.wait(t); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if got := strings.Count(a.log.String(), "invalid configuration"); got != 1 || !strings.Contains(a.log.String(), "level=ERROR") {
		t.Errorf("log:\n%s", a.log.String())
	}
}

func TestRunAgentWarnsAboutUnknownKeys(t *testing.T) {
	srv, requests := reportServer(t)
	env := newTestEnv(t, srv.URL, "plattform_url: typo")
	env.saveCredentials("srv-1")
	a := startAgent(t, env.config, nil)

	awaitRequest(t, requests, a)
	a.cancel()
	a.wait(t)
	if log := a.log.String(); !strings.Contains(log, "level=WARN") || !strings.Contains(log, "ignoring unknown config keys") || !strings.Contains(log, "plattform_url") {
		t.Errorf("log:\n%s", log)
	}
}

func TestRunAgentExitsZeroAfterUpdate(t *testing.T) {
	srv, _ := reportServer(t)
	env := newTestEnv(t, srv.URL)
	a := startAgent(t, env.config, func(context.Context) error { return selfupdate.ErrRestart })

	if code := a.wait(t); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(a.log.String(), "updated; exiting so the service manager restarts the new binary") {
		t.Errorf("log:\n%s", a.log.String())
	}
}

func TestRunAgentKeepsRunningWhenUpdaterStops(t *testing.T) {
	srv, requests := reportServer(t)
	env := newTestEnv(t, srv.URL)
	env.saveCredentials("srv-1")
	a := startAgent(t, env.config, func(context.Context) error { return nil })

	awaitRequest(t, requests, a)
	select {
	case code := <-a.done:
		t.Fatalf("runAgent returned %d after the updater stopped", code)
	case <-time.After(100 * time.Millisecond):
	}
	a.cancel()
	if code := a.wait(t); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestRunAgentSkipsUpdaterWhenAutoUpdateOff(t *testing.T) {
	srv, requests := reportServer(t)
	env := newTestEnv(t, srv.URL, "auto_update: false")
	env.saveCredentials("srv-1")
	called := make(chan struct{}, 1)
	a := startAgent(t, env.config, func(context.Context) error {
		called <- struct{}{}
		return selfupdate.ErrRestart
	})

	awaitRequest(t, requests, a)
	a.cancel()
	a.wait(t)
	select {
	case <-called:
		t.Error("updater ran with auto_update: false")
	default:
	}
}

// run leaves its working directory for / so that it never keeps a disk or
// the flash drive busy; a relative --config is resolved first.
func TestLeaveWorkingDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	got, err := leaveWorkingDir("lookout.yaml")
	if err != nil {
		t.Fatalf("leaveWorkingDir: %v", err)
	}
	if filepath.Base(got) != "lookout.yaml" || !filepath.IsAbs(got) {
		t.Fatalf("config path = %q, want an absolute path to lookout.yaml", got)
	}
	gotDir, err := os.Stat(filepath.Dir(got))
	if err != nil {
		t.Fatal(err)
	}
	wantDir, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(gotDir, wantDir) {
		t.Fatalf("config path = %q, want it under %q", got, dir)
	}
	cwd, err := os.Getwd()
	if err != nil || cwd != "/" {
		t.Fatalf("working dir = %q, %v; want /", cwd, err)
	}
}

// markAbnormalExit leaves the marker a run that did not end cleanly leaves.
func markAbnormalExit(t *testing.T, env testEnv) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(env.dir, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.dir, "run", runningMarker), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A release that crashes soon after starting never reaches the updater's
// first check five minutes in. After a run that did not end cleanly, the
// next start checks for an update before it reports, and exits for the
// service manager to start the update.
func TestRunAgentChecksForUpdateAfterAbnormalExit(t *testing.T) {
	srv, requests := reportServer(t)
	env := newTestEnv(t, srv.URL)
	env.saveCredentials("srv-1")
	markAbnormalExit(t, env)
	u := &fakeUpdater{check: func(context.Context) (string, error) { return "0.2.0", nil }}
	a := startAgentWith(t, env.config, u)

	if code := a.wait(t); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := u.checks.Load(); got != 1 {
		t.Errorf("update checks = %d, want 1", got)
	}
	select {
	case <-requests:
		t.Error("reported before installing the update")
	default:
	}
	if log := a.log.String(); !strings.Contains(log, "previous run did not exit cleanly") || !strings.Contains(log, "updated; exiting") {
		t.Errorf("log:\n%s", log)
	}
}

func TestRunAgentReportsWhenEarlyCheckFindsNothing(t *testing.T) {
	srv, requests := reportServer(t)
	env := newTestEnv(t, srv.URL)
	env.saveCredentials("srv-1")
	markAbnormalExit(t, env)
	u := &fakeUpdater{check: func(context.Context) (string, error) { return "", errors.New("HTTP 403") }}
	a := startAgentWith(t, env.config, u)

	awaitRequest(t, requests, a)
	a.cancel()
	a.wait(t)
	if got := u.checks.Load(); got != 1 {
		t.Errorf("update checks = %d, want 1", got)
	}
	if !strings.Contains(a.log.String(), "HTTP 403") {
		t.Errorf("log lacks the failed check:\n%s", a.log.String())
	}
}

// A clean exit removes the marker, so the next start does not check early.
func TestRunAgentCleanExitSkipsEarlyCheck(t *testing.T) {
	srv, requests := reportServer(t)
	env := newTestEnv(t, srv.URL)
	env.saveCredentials("srv-1")

	for range 2 {
		u := &fakeUpdater{}
		a := startAgentWith(t, env.config, u)
		awaitRequest(t, requests, a)
		a.cancel()
		a.wait(t)
		if got := u.checks.Load(); got != 0 {
			t.Errorf("update checks = %d, want 0", got)
		}
	}
	if _, err := os.Stat(filepath.Join(env.dir, "run", runningMarker)); !os.IsNotExist(err) {
		t.Errorf("marker after a clean exit: %v, want it removed", err)
	}
}

// A release stuck in a crash loop checks at most once per earlyCheckInterval,
// which keeps it well inside GitHub's unauthenticated API rate limit.
func TestRunAgentThrottlesEarlyChecks(t *testing.T) {
	srv, requests := reportServer(t)
	env := newTestEnv(t, srv.URL)
	env.saveCredentials("srv-1")

	u := &fakeUpdater{}
	for range 2 {
		markAbnormalExit(t, env)
		a := startAgentWith(t, env.config, u)
		awaitRequest(t, requests, a)
		a.cancel()
		a.wait(t)
	}
	if got := u.checks.Load(); got != 1 {
		t.Errorf("update checks = %d, want 1 within the throttle window", got)
	}
}

func TestRunAgentNoEarlyCheckWhenAutoUpdateOff(t *testing.T) {
	srv, requests := reportServer(t)
	env := newTestEnv(t, srv.URL, "auto_update: false")
	env.saveCredentials("srv-1")
	markAbnormalExit(t, env)
	u := &fakeUpdater{}
	a := startAgentWith(t, env.config, u)

	awaitRequest(t, requests, a)
	a.cancel()
	a.wait(t)
	if got := u.checks.Load(); got != 0 {
		t.Errorf("update checks = %d, want 0 with auto_update: false", got)
	}
}

// The reporter is built from the platform_url in the file at the time it is
// called, so credentials loaded after an enroll --force that switched the
// URL go to the new platform.
func TestReporterForReadsCurrentPlatformURL(t *testing.T) {
	first, firstRequests := reportServer(t)
	second, secondRequests := reportServer(t)
	env := newTestEnv(t, first.URL)
	newReporter := reporterFor(env.config)

	send := func() {
		t.Helper()
		r, err := newReporter()
		if err != nil {
			t.Fatalf("reporterFor: %v", err)
		}
		if _, err := r.Report(context.Background(), "srv.secret", wire.ReportRequest{}); err != nil {
			t.Fatalf("Report: %v", err)
		}
	}
	send()
	env.writeConfig("platform_url: " + second.URL + "\n")
	send()

	if len(firstRequests) != 1 || len(secondRequests) != 1 {
		t.Errorf("requests: first %d, second %d; want one each", len(firstRequests), len(secondRequests))
	}

	env.writeConfig("platform_url: \"\"\n")
	if _, err := newReporter(); err == nil {
		t.Error("reporterFor accepted an empty platform_url")
	}
}
