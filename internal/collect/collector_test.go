package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newCollectorLogger() (*slog.Logger, *bytes.Buffer) {
	var logs bytes.Buffer
	return slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})), &logs
}

func okStatfs(string) (StatfsResult, error) {
	return StatfsResult{Total: 1000, Free: 400, Available: 300}, nil
}

func TestCollectorUnraidFixture(t *testing.T) {
	logger, _ := newCollectorLogger()
	c := New(Options{
		Root:         "testdata/unraid",
		Statfs:       okStatfs,
		DockerSocket: filepath.Join(t.TempDir(), "none.sock"),
		Logger:       logger,
	})

	host, metrics := c.Collect(context.Background())

	if host.Hostname != "babel" {
		t.Errorf("hostname = %q, want babel", host.Hostname)
	}
	if len(metrics.Filesystems) != 7 {
		t.Errorf("got %d filesystems, want 7: %+v", len(metrics.Filesystems), metrics.Filesystems)
	}
	if metrics.FailedUnits != nil {
		t.Errorf("FailedUnits = %#v, want nil", metrics.FailedUnits)
	}
	if metrics.Containers != nil {
		t.Errorf("Containers = %#v, want nil", metrics.Containers)
	}
	if metrics.OOMKillsTotal != 3 {
		t.Errorf("OOMKillsTotal = %d, want 3", metrics.OOMKillsTotal)
	}
	if metrics.Memory.TotalBytes == 0 {
		t.Error("memory was not collected")
	}

	encoded, err := json.Marshal(metrics)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"failed_units":null`, `"containers":null`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("metrics JSON lacks %s: %s", want, encoded)
		}
	}
}

func TestCollectorSourceErrorsNeverAbort(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "run/systemd/system"), 0o755); err != nil {
		t.Fatal(err)
	}
	logger, logs := newCollectorLogger()
	c := New(Options{
		Root:       root,
		Statfs:     okStatfs,
		RunCommand: func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("systemctl broke") },
		Logger:     logger,
	})

	_, metrics := c.Collect(context.Background())

	if metrics.Filesystems == nil || len(metrics.Filesystems) != 0 {
		t.Errorf("Filesystems = %#v, want empty non-nil slice", metrics.Filesystems)
	}
	if metrics.FailedUnits != nil {
		t.Errorf("FailedUnits = %#v, want nil on a systemctl error", metrics.FailedUnits)
	}
	if metrics.Containers != nil {
		t.Errorf("Containers = %#v, want nil", metrics.Containers)
	}
	encoded, err := json.Marshal(metrics)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"filesystems":[]`) {
		t.Errorf("metrics JSON lacks filesystems []: %s", encoded)
	}
	for _, source := range []string{"filesystems", "failed units", "cpu", "load", "memory", "oom kills"} {
		got := strings.Count(logs.String(), "source="+source+" ") + strings.Count(logs.String(), `source="`+source+`"`)
		if got != 1 {
			t.Errorf("source %q logged %d warnings, want 1\n%s", source, got, logs.String())
		}
	}
}

func TestCollectorReportsFailedUnitsAndContainers(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "run/systemd/system"), 0o755); err != nil {
		t.Fatal(err)
	}
	socket := newDockerFake(t, dockerFakeHandler(t, func(w http.ResponseWriter) { inspectHealthy(w) }))
	logger, _ := newCollectorLogger()
	c := New(Options{
		Root:   root,
		Statfs: okStatfs,
		RunCommand: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("backup.service loaded failed failed\n"), nil
		},
		DockerSocket: socket,
		Logger:       logger,
	})

	_, metrics := c.Collect(context.Background())

	if len(metrics.FailedUnits) != 1 || metrics.FailedUnits[0] != "backup.service" {
		t.Errorf("FailedUnits = %v", metrics.FailedUnits)
	}
	healthy := "healthy"
	assertContainers(t, metrics.Containers, 4, &healthy)
}

func TestNewDefaults(t *testing.T) {
	c := New(Options{})

	if c.root != "/" {
		t.Errorf("root = %q, want /", c.root)
	}
	if c.docker.SocketPath != "/var/run/docker.sock" {
		t.Errorf("docker socket = %q", c.docker.SocketPath)
	}
	if c.runCommand == nil || c.filesystem.Statfs == nil || c.logger == nil {
		t.Error("defaults not filled in")
	}
}
