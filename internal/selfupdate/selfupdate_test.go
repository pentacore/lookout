package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aead.dev/minisign"
)

var newBinary = []byte("\x7fELF lookout 0.2.0")

// fakeGitHub serves a releases/latest document whose browser_download_urls
// point back at itself.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	tag       string
	assets    map[string][]byte
	assetBase string // browser_download_url prefix; the server itself when ""
	requests  []string
}

func newFakeGitHub(t *testing.T, tag string, assets map[string][]byte) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{t: t, tag: tag, assets: assets}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.Path)
	if got := r.Header.Get("User-Agent"); got != "lookout/dev" {
		f.t.Errorf("User-Agent = %q", got)
	}
	switch {
	case r.URL.Path == "/releases/latest":
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			f.t.Errorf("Accept = %q", got)
		}
		type asset struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}
		doc := struct {
			TagName string  `json:"tag_name"`
			Draft   bool    `json:"draft"`
			Assets  []asset `json:"assets"`
		}{TagName: f.tag, Assets: []asset{}}
		base := f.assetBase
		if base == "" {
			base = f.srv.URL
		}
		for name := range f.assets {
			doc.Assets = append(doc.Assets, asset{Name: name, URL: base + "/download/" + name})
		}
		_ = json.NewEncoder(w).Encode(doc)
	case strings.HasPrefix(r.URL.Path, "/download/"):
		data, ok := f.assets[strings.TrimPrefix(r.URL.Path, "/download/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) setTag(tag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tag = tag
}

func (f *fakeGitHub) downloads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.requests {
		if strings.HasPrefix(p, "/download/") {
			n++
		}
	}
	return n
}

func (f *fakeGitHub) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// signedAssets returns a release asset set for arch with an archive holding
// bin, signed with the trusted comment "lookout {version} {asset}".
func signedAssets(t *testing.T, priv minisign.PrivateKey, version, arch string, bin []byte) map[string][]byte {
	t.Helper()
	name := "lookout_linux_" + arch + ".tar.gz"
	archive := makeArchive(t, tarEntry{name: "README.md", body: []byte("# lookout")}, tarEntry{name: "lookout", body: bin})
	return map[string][]byte{
		name:              archive,
		name + ".minisig": minisign.SignWithComments(priv, archive, "lookout "+version+" "+name, "untrusted"),
		"checksums.txt":   []byte("irrelevant"),
	}
}

type fixture struct {
	gh      *fakeGitHub
	u       *Updater
	exe     string
	persist string
	execs   []string
}

// newFixture builds an Updater at version 0.1.0 for amd64 against a fake
// GitHub. Its Exec fake checks that the preflight runs the new binary and
// prints "lookout 0.2.0".
func newFixture(t *testing.T, tag string, assets func(pub string, priv minisign.PrivateKey) (string, map[string][]byte)) *fixture {
	t.Helper()
	pub, priv := newKey(t)
	key, files := assets(pub, priv)
	f := &fixture{gh: newFakeGitHub(t, tag, files)}

	dir := t.TempDir()
	f.exe = filepath.Join(dir, "lookout")
	writeFile(t, f.exe, []byte("old binary"), 0o755)
	flash := filepath.Join(dir, "flash")
	if err := os.Mkdir(flash, 0o700); err != nil {
		t.Fatal(err)
	}
	f.persist = filepath.Join(flash, "lookout")
	writeFile(t, f.persist, []byte("old binary"), 0o600)

	f.u = &Updater{
		Current:     "0.1.0",
		PublicKey:   key,
		Arch:        "amd64",
		Exe:         f.exe,
		PersistCopy: f.persist,
		ReleaseURL:  f.gh.srv.URL + "/releases/latest",
		HTTP:        f.gh.srv.Client(),
		Exec: func(_ context.Context, path string, args ...string) ([]byte, error) {
			f.execs = append(f.execs, path+" "+strings.Join(args, " "))
			got, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			if string(got) != string(newBinary) {
				t.Errorf("preflight ran %q, want the new binary", got)
			}
			return []byte("lookout 0.2.0\n"), nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return f
}

func goodAssets(t *testing.T) func(string, minisign.PrivateKey) (string, map[string][]byte) {
	return func(pub string, priv minisign.PrivateKey) (string, map[string][]byte) {
		return pub, signedAssets(t, priv, "0.2.0", "amd64", newBinary)
	}
}

func (f *fixture) assertUnchanged(t *testing.T) {
	t.Helper()
	assertFile(t, f.exe, []byte("old binary"))
	assertFile(t, f.persist, []byte("old binary"))
	assertNoFile(t, f.exe+".new")
	assertNoFile(t, f.persist+".tmp")
}

func TestCheckAlreadyCurrent(t *testing.T) {
	for _, tag := range []string{"v0.1.0", "0.1.0", "v0.0.9", "v0.1.0-rc.1"} {
		t.Run(tag, func(t *testing.T) {
			f := newFixture(t, tag, goodAssets(t))
			got, err := f.u.Check(context.Background())
			if err != nil || got != "" {
				t.Fatalf("Check = %q, %v; want \"\", nil", got, err)
			}
			if n := f.gh.downloads(); n != 0 {
				t.Fatalf("%d asset downloads, want 0", n)
			}
			f.assertUnchanged(t)
		})
	}
}

func TestCheckUpdates(t *testing.T) {
	f := newFixture(t, "v0.2.0", goodAssets(t))

	got, err := f.u.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got != "0.2.0" {
		t.Fatalf("Check = %q, want 0.2.0", got)
	}
	assertFile(t, f.exe, newBinary)
	assertFile(t, f.persist, newBinary)
	assertNoFile(t, f.exe+".new")
	if want := []string{f.exe + ".new version"}; strings.Join(f.execs, "|") != strings.Join(want, "|") {
		t.Fatalf("Exec calls = %q, want %q", f.execs, want)
	}
}

func TestCheckRejectsBadSignature(t *testing.T) {
	tests := []struct {
		name   string
		assets func(*testing.T) func(string, minisign.PrivateKey) (string, map[string][]byte)
		want   string
	}{
		{"signed by another key", func(t *testing.T) func(string, minisign.PrivateKey) (string, map[string][]byte) {
			return func(_ string, priv minisign.PrivateKey) (string, map[string][]byte) {
				otherPub, _ := newKey(t)
				return otherPub, signedAssets(t, priv, "0.2.0", "amd64", newBinary)
			}
		}, "signature"},
		{"tampered archive", func(t *testing.T) func(string, minisign.PrivateKey) (string, map[string][]byte) {
			return func(pub string, priv minisign.PrivateKey) (string, map[string][]byte) {
				files := signedAssets(t, priv, "0.2.0", "amd64", newBinary)
				files["lookout_linux_amd64.tar.gz"] = makeArchive(t, tarEntry{name: "lookout", body: []byte("evil")})
				return pub, files
			}
		}, "signature"},
		{"old release replayed under a newer tag", func(t *testing.T) func(string, minisign.PrivateKey) (string, map[string][]byte) {
			return func(pub string, priv minisign.PrivateKey) (string, map[string][]byte) {
				return pub, signedAssets(t, priv, "0.0.5", "amd64", newBinary)
			}
		}, "trusted comment"},
		{"other arch archive under this arch's name", func(t *testing.T) func(string, minisign.PrivateKey) (string, map[string][]byte) {
			return func(pub string, priv minisign.PrivateKey) (string, map[string][]byte) {
				arm := signedAssets(t, priv, "0.2.0", "arm64", newBinary)
				return pub, map[string][]byte{
					"lookout_linux_amd64.tar.gz":         arm["lookout_linux_arm64.tar.gz"],
					"lookout_linux_amd64.tar.gz.minisig": arm["lookout_linux_arm64.tar.gz.minisig"],
				}
			}
		}, "trusted comment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, "v0.2.0", tt.assets(t))
			got, err := f.u.Check(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Check = %q, %v; want an error containing %q", got, err, tt.want)
			}
			if len(f.execs) != 0 {
				t.Fatalf("preflight ran %q before verification passed", f.execs)
			}
			f.assertUnchanged(t)
		})
	}
}

func TestCheckRejectsBinaryThatFailsPreflight(t *testing.T) {
	tests := []struct {
		name string
		exec func(context.Context, string, ...string) ([]byte, error)
	}{
		{"exec error", func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("exec format error")
		}},
		{"wrong version", func(context.Context, string, ...string) ([]byte, error) {
			return []byte("lookout 0.1.9\n"), nil
		}},
		{"missing newline", func(context.Context, string, ...string) ([]byte, error) {
			return []byte("lookout 0.2.0"), nil
		}},
		{"hangs past the timeout", func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 5*time.Second {
				t.Errorf("preflight deadline = %v (set %v), want at most 5s", time.Until(deadline), ok)
			}
			return nil, context.DeadlineExceeded
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, "v0.2.0", goodAssets(t))
			f.u.Exec = tt.exec
			got, err := f.u.Check(context.Background())
			if err == nil || !strings.Contains(err.Error(), "preflight") {
				t.Fatalf("Check = %q, %v; want a preflight error", got, err)
			}
			f.assertUnchanged(t)
		})
	}
}

func TestCheckMissingAsset(t *testing.T) {
	f := newFixture(t, "v0.2.0", goodAssets(t))
	f.u.Arch = "arm64"
	_, err := f.u.Check(context.Background())
	if err == nil || err.Error() != "asset lookout_linux_arm64.tar.gz not found" {
		t.Fatalf("Check error = %v", err)
	}
	f.assertUnchanged(t)

	t.Run("signature missing", func(t *testing.T) {
		f := newFixture(t, "v0.2.0", func(pub string, priv minisign.PrivateKey) (string, map[string][]byte) {
			files := signedAssets(t, priv, "0.2.0", "amd64", newBinary)
			delete(files, "lookout_linux_amd64.tar.gz.minisig")
			return pub, files
		})
		_, err := f.u.Check(context.Background())
		if err == nil || err.Error() != "asset lookout_linux_amd64.tar.gz.minisig not found" {
			t.Fatalf("Check error = %v", err)
		}
		if n := f.gh.downloads(); n != 0 {
			t.Fatalf("%d downloads, want 0", n)
		}
	})
}

func TestCheckDownloadTooLarge(t *testing.T) {
	f := newFixture(t, "v0.2.0", func(pub string, priv minisign.PrivateKey) (string, map[string][]byte) {
		files := signedAssets(t, priv, "0.2.0", "amd64", newBinary)
		files["lookout_linux_amd64.tar.gz"] = make([]byte, 4096)
		return pub, files
	})
	f.u.MaxDownload = 1024
	_, err := f.u.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("Check error = %v, want a size limit error", err)
	}
	f.assertUnchanged(t)
}

func TestCheckRejectsInsecureDownloadURL(t *testing.T) {
	f := newFixture(t, "v0.2.0", goodAssets(t))
	f.gh.mu.Lock()
	f.gh.assetBase = "http://example.invalid"
	f.gh.mu.Unlock()
	_, err := f.u.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("Check error = %v, want an https error", err)
	}
	f.assertUnchanged(t)
}

func TestCheckRefusesWithoutKeyOrOnDevBuild(t *testing.T) {
	f := newFixture(t, "v0.2.0", goodAssets(t))
	f.u.PublicKey = ""
	if _, err := f.u.Check(context.Background()); !errors.Is(err, ErrNoPublicKey) {
		t.Fatalf("Check without key = %v, want ErrNoPublicKey", err)
	}
	f = newFixture(t, "v0.2.0", goodAssets(t))
	f.u.Current = "dev"
	if _, err := f.u.Check(context.Background()); !errors.Is(err, ErrDevBuild) {
		t.Fatalf("Check on dev = %v, want ErrDevBuild", err)
	}
	if n := f.gh.requestCount(); n != 0 {
		t.Fatalf("%d requests, want 0", n)
	}
	f.assertUnchanged(t)
}

func TestLoopDisabledWithoutKey(t *testing.T) {
	tests := []struct {
		name, key, current, wantLog string
	}{
		{"no key", "", "0.1.0", "self-update disabled: no signing key built in"},
		{"dev build", "RWQkey", "dev", "self-update disabled: development build"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, "v0.2.0", goodAssets(t))
			var logs strings.Builder
			f.u.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			f.u.PublicKey = tt.key
			f.u.Current = tt.current
			f.u.Sleep = func(context.Context, time.Duration) error {
				t.Error("Loop slept while disabled")
				return nil
			}

			if err := f.u.Loop(context.Background()); err != nil {
				t.Fatalf("Loop = %v, want nil", err)
			}
			if n := f.gh.requestCount(); n != 0 {
				t.Fatalf("%d HTTP requests, want 0", n)
			}
			if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), tt.wantLog) {
				t.Fatalf("log = %q, want a warning %q", logs.String(), tt.wantLog)
			}
		})
	}
}

func TestLoopSchedule(t *testing.T) {
	f := newFixture(t, "v0.1.0", goodAssets(t))
	f.u.Rand = func() float64 { return 0.5 }
	var sleeps []time.Duration
	f.u.Sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		switch len(sleeps) {
		case 2:
			// The second check fails: a missing release is logged and retried.
			f.gh.setTag("not-a-version")
		case 3:
			f.gh.setTag("v0.2.0")
		}
		return nil
	}
	var logs strings.Builder
	f.u.Logger = slog.New(slog.NewTextHandler(&logs, nil))

	err := f.u.Loop(context.Background())
	if !errors.Is(err, ErrRestart) {
		t.Fatalf("Loop = %v, want ErrRestart", err)
	}
	period := 6*time.Hour + 15*time.Minute
	want := []time.Duration{5 * time.Minute, period, period}
	if len(sleeps) != len(want) {
		t.Fatalf("sleeps = %v, want %v", sleeps, want)
	}
	for i := range want {
		if sleeps[i] != want[i] {
			t.Fatalf("sleeps = %v, want %v", sleeps, want)
		}
	}
	assertFile(t, f.exe, newBinary)
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "not-a-version") {
		t.Fatalf("log = %q, want the failed check logged at warn", logs.String())
	}
}

func TestLoopReturnsNilOnCancel(t *testing.T) {
	f := newFixture(t, "v0.1.0", goodAssets(t))
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	f.u.Sleep = func(ctx context.Context, _ time.Duration) error {
		calls++
		if calls == 3 {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	if err := f.u.Loop(ctx); err != nil {
		t.Fatalf("Loop = %v, want nil", err)
	}
	f.assertUnchanged(t)
}

func TestLoopRealSleepStopsOnCancel(t *testing.T) {
	f := newFixture(t, "v0.1.0", goodAssets(t))
	f.u.Sleep = nil // the default sleep honours ctx
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.u.Loop(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Loop = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Loop did not return after cancel")
	}
}

func TestCheckPreflightExecsTheNewBinary(t *testing.T) {
	tests := []struct {
		name    string
		script  string
		wantErr string // "" means the update installs
	}{
		{"prints the expected version", "#!/bin/sh\n[ \"$1\" = version ] && echo 'lookout 0.2.0'\n", ""},
		{"exits non-zero", "#!/bin/sh\necho 'lookout 0.2.0'\nexit 1\n", "preflight"},
		{"not executable code", "\x7fELF truncated", "preflight"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := []byte(tt.script)
			f := newFixture(t, "v0.2.0", func(pub string, priv minisign.PrivateKey) (string, map[string][]byte) {
				return pub, signedAssets(t, priv, "0.2.0", "amd64", bin)
			})
			f.u.Exec = nil // the real os/exec runner

			got, err := f.u.Check(context.Background())
			if tt.wantErr == "" {
				if err != nil || got != "0.2.0" {
					t.Fatalf("Check = %q, %v; want 0.2.0", got, err)
				}
				assertFile(t, f.exe, bin)
				assertNoFile(t, f.exe+".new")
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Check = %q, %v; want an error containing %q", got, err, tt.wantErr)
			}
			f.assertUnchanged(t)
		})
	}
}
