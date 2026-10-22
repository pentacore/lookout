package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

// redirectingServer is an https server whose /asset redirects to target.
func redirectingServer(t *testing.T, target string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGetRefusesRedirectToPlainHTTP(t *testing.T) {
	var plainHits int
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plainHits++
		_, _ = io.WriteString(w, "payload")
	}))
	t.Cleanup(plain.Close)
	srv := redirectingServer(t, plain.URL+"/asset")

	tests := []struct {
		name   string
		client func() *http.Client
	}{
		{"default client", func() *http.Client { return nil }},
		{"caller client without a redirect policy", func() *http.Client {
			return &http.Client{Transport: srv.Client().Transport}
		}},
		{"caller client that follows any redirect", func() *http.Client {
			return &http.Client{
				Transport:     srv.Client().Transport,
				CheckRedirect: func(*http.Request, []*http.Request) error { return nil },
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := tt.client()
			u := &Updater{HTTP: client}
			u.setDefaults()
			if client == nil {
				// The default client does not trust the test certificate;
				// give it the test transport but keep its redirect policy.
				u.HTTP.Transport = srv.Client().Transport
			}
			_, err := u.get(context.Background(), srv.URL+"/asset", "application/octet-stream", 1024)
			if err == nil || !strings.Contains(err.Error(), "non-https") {
				t.Fatalf("get = %v, want a non-https redirect error", err)
			}
		})
	}
}

func TestGetRejectsOversizedContentLengthBeforeReading(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Announce far more than is sent: reading the body would fail with
		// an unexpected EOF, so a "limit" error proves nothing was read.
		w.Header().Set("Content-Length", "1048576")
		_, _ = io.WriteString(w, "short")
	}))
	t.Cleanup(srv.Close)
	u := &Updater{HTTP: srv.Client()}
	u.setDefaults()

	_, err := u.get(context.Background(), srv.URL+"/asset", "application/octet-stream", 1024)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("get = %v, want a size limit error", err)
	}
}

func TestCheckRetriesPreflightOnTextFileBusy(t *testing.T) {
	busy := &fs.PathError{Op: "fork/exec", Path: "lookout.new", Err: syscall.ETXTBSY}
	tests := []struct {
		name      string
		busyTimes int
		wantErr   bool
	}{
		{"busy once", 1, false},
		{"busy twice", 2, false},
		{"always busy", 100, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, "v0.2.0", goodAssets(t))
			calls := 0
			f.u.Exec = func(context.Context, string, ...string) ([]byte, error) {
				calls++
				if calls <= tt.busyTimes {
					return nil, busy
				}
				return []byte("lookout 0.2.0\n"), nil
			}
			got, err := f.u.Check(context.Background())
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "preflight") || calls != 3 {
					t.Fatalf("Check = %q, %v after %d execs; want a preflight error after 3", got, err, calls)
				}
				f.assertUnchanged(t)
				return
			}
			if err != nil || got != "0.2.0" {
				t.Fatalf("Check = %q, %v; want 0.2.0", got, err)
			}
			assertFile(t, f.exe, newBinary)
		})
	}
}

func TestCheckRejectsRelativeExe(t *testing.T) {
	f := newFixture(t, "v0.2.0", goodAssets(t))
	// Before the fix, a relative Exe installed into the working directory.
	t.Chdir(t.TempDir())
	f.u.Exe = "lookout"
	_, err := f.u.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("Check = %v, want an absolute path error", err)
	}
	if n := f.gh.requestCount(); n != 0 {
		t.Fatalf("%d requests, want 0", n)
	}
}

func TestCheckReadOnlyInstallDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newFixture(t, "v0.2.0", goodAssets(t))
	dir := filepath.Dir(f.exe)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	_, err := f.u.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "install directory "+dir+" is not writable") {
		t.Fatalf("Check = %v, want an install directory error", err)
	}
	assertFile(t, f.exe, []byte("old binary"))
}

// Once the new binary has been renamed into place the update happened; a
// failed directory fsync is logged and the caller still restarts.
func TestInstallDirSyncFailureAfterRename(t *testing.T) {
	orig := syncDirFunc
	t.Cleanup(func() { syncDirFunc = orig })
	syncDirFunc = func(string) error { return fmt.Errorf("sync: %w", syscall.EIO) }

	dir := t.TempDir()
	exe := filepath.Join(dir, "lookout")
	writeFile(t, exe, []byte("old"), 0o755)
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	if err := Install(exe, []byte("new"), "", logger); err != nil {
		t.Fatalf("Install = %v, want nil after a successful rename", err)
	}
	assertFile(t, exe, []byte("new"))
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("log = %q, want a warning", logs.String())
	}
}

func TestLoopRestartsDespiteDirSyncFailure(t *testing.T) {
	orig := syncDirFunc
	t.Cleanup(func() { syncDirFunc = orig })
	syncDirFunc = func(string) error { return errors.New("sync: input/output error") }

	f := newFixture(t, "v0.2.0", goodAssets(t))
	sleeps := 0
	f.u.Sleep = func(context.Context, time.Duration) error {
		// Only the initial delay: a second sleep means the first check failed.
		sleeps++
		if sleeps > 1 {
			return context.Canceled
		}
		return nil
	}
	if err := f.u.Loop(context.Background()); !errors.Is(err, ErrRestart) {
		t.Fatalf("Loop = %v, want ErrRestart", err)
	}
	assertFile(t, f.exe, newBinary)
}

func TestTruncateKeepsUTF8Valid(t *testing.T) {
	got := truncate("x" + strings.Repeat("é", 300))
	if !utf8.ValidString(got) || len(got) > 200+len("…") {
		t.Fatalf("truncate = %q (%d bytes), want valid UTF-8 of at most 200 bytes plus an ellipsis", got, len(got))
	}
}
