package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// startAgent runs runAgent in the background with a short config poll.
func startAgent(t *testing.T, cfgPath string, updaterLoop func(context.Context) error) *runningAgent {
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
	go func() { done <- runAgent(ctx, cfgPath, slog.New(slog.NewTextHandler(log, nil)), updaterLoop) }()
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
