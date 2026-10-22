package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pentacore/lookout/internal/config"
	"github.com/pentacore/lookout/internal/state"
	"github.com/pentacore/lookout/internal/wire"
)

const enrollToken = "enr-0123456789abcdef"

// enrolledOutput is what a successful enroll prints on a systemd host.
const enrolledOutput = "enrolled as srv-1\nrestart lookout (systemctl restart lookout) to apply\n"

// testEnv is a config file plus state and runtime directories in a temp dir.
type testEnv struct {
	t      *testing.T
	dir    string
	config string
	store  state.Store
}

// newTestEnv writes a config with the given platform_url ("" for none yet)
// and any extra YAML lines.
func newTestEnv(t *testing.T, platformURL string, extra ...string) testEnv {
	t.Helper()
	dir := t.TempDir()
	e := testEnv{t: t, dir: dir, config: filepath.Join(dir, "lookout.yaml"), store: state.Store{Dir: filepath.Join(dir, "state")}}
	lines := append([]string{
		fmt.Sprintf("platform_url: %q", platformURL),
		"state_dir: " + filepath.Join(dir, "state"),
		"runtime_dir: " + filepath.Join(dir, "run"),
	}, extra...)
	e.writeConfig(strings.Join(lines, "\n") + "\n")
	return e
}

func (e testEnv) writeConfig(content string) {
	e.t.Helper()
	if err := os.WriteFile(e.config, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e testEnv) saveCredentials(uuid string) {
	e.t.Helper()
	if err := e.store.SaveCredentials(state.Credentials{ServerUUID: uuid, AgentToken: uuid + ".secret"}); err != nil {
		e.t.Fatal(err)
	}
}

func (e testEnv) corruptCredentials() {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.store.Dir, "credentials.json"), []byte("{broken"), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// enrollServer fakes POST /api/agent/v1/enroll and counts the requests.
func enrollServer(t *testing.T, status int, intervalSeconds int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/agent/v1/enroll" {
			http.NotFound(w, r)
			return
		}
		var req wire.EnrollRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.EnrollmentToken != enrollToken {
			t.Errorf("enrollment_token = %q", req.EnrollmentToken)
		}
		w.WriteHeader(status)
		if status == http.StatusCreated {
			_ = json.NewEncoder(w).Encode(wire.EnrollResponse{ServerUUID: "srv-1", AgentToken: "srv-1.s3cr3t", ReportIntervalSeconds: intervalSeconds})
			return
		}
		// Platforms must not echo the token, but a broken one might.
		_, _ = fmt.Fprintf(w, `{"message":"bad token %s"}`, enrollToken)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func runCLI(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestEnrollCreatesCredentials(t *testing.T) {
	srv, _ := enrollServer(t, http.StatusCreated, 120)
	env := newTestEnv(t, "")

	code, stdout, stderr := runCLI("enroll", "--config", env.config, "--url", srv.URL, "--token", enrollToken)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if stdout != enrolledOutput {
		t.Errorf("stdout = %q, want %q", stdout, enrolledOutput)
	}

	creds, err := env.store.LoadCredentials()
	if err != nil || creds.ServerUUID != "srv-1" || creds.AgentToken != "srv-1.s3cr3t" {
		t.Errorf("credentials = %+v, %v", creds, err)
	}
	info, err := os.Stat(filepath.Join(env.store.Dir, "credentials.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("credentials.json = %v, %v; want mode 0600", info, err)
	}
	pc, err := env.store.LoadPlatformConfig()
	if err != nil || pc.ConfigVersion != 0 || pc.ReportIntervalSeconds != 120 || len(pc.Checks) != 0 {
		t.Errorf("platform config = %+v, %v", pc, err)
	}
	cfg, err := os.ReadFile(env.config)
	if err != nil || !strings.Contains(string(cfg), "platform_url: "+srv.URL) {
		t.Errorf("config = %q, %v", cfg, err)
	}
	if strings.Contains(stdout+stderr, "s3cr3t") {
		t.Error("output contains the agent token")
	}
}

func TestEnrollClampsInterval(t *testing.T) {
	srv, _ := enrollServer(t, http.StatusCreated, 2)
	env := newTestEnv(t, "")

	if code, _, stderr := runCLI("enroll", "--config", env.config, "--url", srv.URL, "--token", enrollToken); code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if pc, err := env.store.LoadPlatformConfig(); err != nil || pc.ReportIntervalSeconds != 10 {
		t.Errorf("platform config = %+v, %v; want interval clamped to 10", pc, err)
	}
}

func TestEnrollSameURLNeedsNoForce(t *testing.T) {
	srv, _ := enrollServer(t, http.StatusCreated, 60)
	env := newTestEnv(t, srv.URL+"/")

	code, _, stderr := runCLI("enroll", "--config", env.config, "--url", srv.URL, "--token", enrollToken)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
}

func TestEnrollRefusesWhenEnrolled(t *testing.T) {
	srv, calls := enrollServer(t, http.StatusCreated, 60)
	env := newTestEnv(t, srv.URL)
	env.saveCredentials("old-uuid")

	code, _, stderr := runCLI("enroll", "--config", env.config, "--url", srv.URL, "--token", enrollToken)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "already enrolled as old-uuid; pass --force to re-enroll") {
		t.Errorf("stderr = %q", stderr)
	}
	if calls.Load() != 0 {
		t.Error("the platform was called")
	}
}

func TestEnrollRefusesUnreadableCredentialsWithoutForce(t *testing.T) {
	srv, calls := enrollServer(t, http.StatusCreated, 60)
	env := newTestEnv(t, srv.URL)
	env.saveCredentials("old-uuid")
	env.corruptCredentials()

	code, _, stderr := runCLI("enroll", "--config", env.config, "--url", srv.URL, "--token", enrollToken)
	if code != 1 || !strings.Contains(stderr, "--force") || calls.Load() != 0 {
		t.Fatalf("exit code = %d, stderr = %q, calls = %d", code, stderr, calls.Load())
	}
	if code, _, stderr := runCLI("enroll", "--force", "--config", env.config, "--url", srv.URL, "--token", enrollToken); code != 0 {
		t.Fatalf("with --force: exit code = %d, stderr = %q", code, stderr)
	}
}

func TestEnrollForceResetsConfigVersion(t *testing.T) {
	srv, _ := enrollServer(t, http.StatusCreated, 60)
	env := newTestEnv(t, srv.URL)
	env.saveCredentials("old-uuid")
	err := env.store.SavePlatformConfig(state.PlatformConfig{
		ConfigVersion:         7,
		ReportIntervalSeconds: 30,
		Checks:                []wire.CheckConfig{{ID: "c1", Type: "mount_present"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCLI("enroll", "--force", "--config", env.config, "--url", srv.URL, "--token", enrollToken)
	if code != 0 || stdout != enrolledOutput {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	pc, err := env.store.LoadPlatformConfig()
	if err != nil || pc.ConfigVersion != 0 || len(pc.Checks) != 0 {
		t.Errorf("platform config = %+v, %v; want version 0 and no checks", pc, err)
	}
	if creds, err := env.store.LoadCredentials(); err != nil || creds.ServerUUID != "srv-1" {
		t.Errorf("credentials = %+v, %v", creds, err)
	}
}

func TestEnrollURLMismatch(t *testing.T) {
	srv, calls := enrollServer(t, http.StatusCreated, 60)
	env := newTestEnv(t, "https://a.example")

	code, _, stderr := runCLI("enroll", "--config", env.config, "--url", srv.URL, "--token", enrollToken)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	want := fmt.Sprintf("platform_url in %s is https://a.example; pass --force to switch to %s", env.config, srv.URL)
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if calls.Load() != 0 {
		t.Error("the platform was called")
	}
	if cfg, _ := os.ReadFile(env.config); !strings.Contains(string(cfg), "https://a.example") {
		t.Errorf("config was changed: %q", cfg)
	}
}

func TestEnrollForceSwitchesURL(t *testing.T) {
	srv, _ := enrollServer(t, http.StatusCreated, 60)
	env := newTestEnv(t, "https://a.example")

	code, _, stderr := runCLI("enroll", "--force", "--config", env.config, "--url", srv.URL, "--token", enrollToken)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if cfg, _ := os.ReadFile(env.config); !strings.Contains(string(cfg), "platform_url: "+srv.URL) {
		t.Errorf("config = %q", cfg)
	}
}

func TestEnrollTokenRejected(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   string
	}{
		{"gone", http.StatusGone, "enrollment token expired or already used; create a new one in the platform"},
		{"unknown", http.StatusNotFound, "enrollment token not recognised"},
		{"server error", http.StatusInternalServerError, "enroll failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := enrollServer(t, tt.status, 60)
			env := newTestEnv(t, "")

			code, stdout, stderr := runCLI("enroll", "--config", env.config, "--url", srv.URL, "--token", enrollToken)
			if code != 1 || stdout != "" || !strings.Contains(stderr, tt.want) {
				t.Fatalf("exit code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
			}
			if strings.Contains(stderr, enrollToken) {
				t.Errorf("stderr leaks the enrollment token: %q", stderr)
			}
			if _, err := env.store.LoadCredentials(); err == nil {
				t.Error("credentials were saved")
			}
		})
	}
}

func TestEnrollRejectsMalformedCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"server_uuid":"a","agent_token":"b.c","report_interval_seconds":60}`))
	}))
	defer srv.Close()
	env := newTestEnv(t, "")

	code, _, stderr := runCLI("enroll", "--config", env.config, "--url", srv.URL, "--token", enrollToken)
	if code != 1 || !strings.Contains(stderr, "malformed credentials") {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
	if _, err := env.store.LoadCredentials(); err == nil {
		t.Error("credentials were saved")
	}
}

func TestEnrollRejectsPlainHTTP(t *testing.T) {
	env := newTestEnv(t, "")

	code, _, stderr := runCLI("enroll", "--config", env.config, "--url", "http://example.com", "--token", enrollToken)
	if code != 1 || !strings.Contains(stderr, "https") {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}
}

func TestEnrollMissingFlags(t *testing.T) {
	t.Setenv("LOOKOUT_TOKEN", "")
	env := newTestEnv(t, "")
	tests := []struct {
		name string
		args []string
	}{
		{"no token", []string{"enroll", "--config", env.config, "--url", "https://x.example"}},
		{"no url", []string{"enroll", "--config", env.config, "--token", enrollToken}},
		{"nothing", []string{"enroll"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runCLI(tt.args...)
			if code != 2 || !strings.Contains(stderr, "--url and --token (or LOOKOUT_TOKEN) are required") {
				t.Fatalf("exit code = %d, stderr = %q", code, stderr)
			}
		})
	}
}

// A failed re-enroll against a new platform must leave the config untouched:
// otherwise the old credentials would be sent to the new platform after the
// next restart.
func TestEnrollForceFailureKeepsConfig(t *testing.T) {
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachableURL := unreachable.URL
	unreachable.Close()

	tests := []struct {
		name string
		url  func(t *testing.T) string
	}{
		{"token gone", func(t *testing.T) string { srv, _ := enrollServer(t, http.StatusGone, 60); return srv.URL }},
		{"token unknown", func(t *testing.T) string { srv, _ := enrollServer(t, http.StatusNotFound, 60); return srv.URL }},
		{"unreachable", func(*testing.T) string { return unreachableURL }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t, "https://old.example", "# keep this comment")
			env.saveCredentials("old-uuid")
			before, err := os.ReadFile(env.config)
			if err != nil {
				t.Fatal(err)
			}

			code, _, stderr := runCLI("enroll", "--force", "--config", env.config, "--url", tt.url(t), "--token", enrollToken)
			if code != 1 {
				t.Fatalf("exit code = %d, stderr = %q", code, stderr)
			}
			after, err := os.ReadFile(env.config)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Errorf("config changed:\nbefore %q\nafter  %q", before, after)
			}
			if creds, err := env.store.LoadCredentials(); err != nil || creds.ServerUUID != "old-uuid" {
				t.Errorf("credentials = %+v, %v; want the old ones kept", creds, err)
			}
		})
	}
}

// install.sh hands the token over in LOOKOUT_TOKEN so that it never shows in
// the process list or in sudo's log.
func TestEnrollTokenFromEnvironment(t *testing.T) {
	srv, calls := enrollServer(t, http.StatusCreated, 60)
	env := newTestEnv(t, "")
	t.Setenv("LOOKOUT_TOKEN", enrollToken)

	code, stdout, stderr := runCLI("enroll", "--config", env.config, "--url", srv.URL)
	if code != 0 || stdout != enrolledOutput || calls.Load() != 1 {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q, calls = %d", code, stdout, stderr, calls.Load())
	}
	if creds, err := env.store.LoadCredentials(); err != nil || creds.ServerUUID != "srv-1" {
		t.Errorf("credentials = %+v, %v", creds, err)
	}
}

// The Unraid enroll wrapper restarts the agent itself, so only systemd hosts
// get the restart hint.
func TestEnrollRestartHintOnlyOnSystemd(t *testing.T) {
	srv, _ := enrollServer(t, http.StatusCreated, 60)
	env := newTestEnv(t, "")
	orig := detectPlatform
	detectPlatform = func() config.Platform { return config.PlatformUnraid }
	t.Cleanup(func() { detectPlatform = orig })

	code, stdout, stderr := runCLI("enroll", "--config", env.config, "--url", srv.URL, "--token", enrollToken)
	if code != 0 || stdout != "enrolled as srv-1\n" {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

// When platform_url cannot be saved after a successful enroll, the new
// credentials are removed so that they are never sent to the old platform.
func TestEnrollURLWriteFailureRemovesCredentials(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	srv, _ := enrollServer(t, http.StatusCreated, 60)
	env := newTestEnv(t, "https://old.example")
	env.saveCredentials("old-uuid")
	before, err := os.ReadFile(env.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(env.dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(env.dir, 0o755) })

	code, stdout, stderr := runCLI("enroll", "--force", "--config", env.config, "--url", srv.URL, "--token", enrollToken)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "cannot save platform_url") {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if after, _ := os.ReadFile(env.config); !bytes.Equal(before, after) {
		t.Errorf("config changed: %q", after)
	}
	if creds, err := env.store.LoadCredentials(); err == nil {
		t.Errorf("credentials %+v kept, want them removed", creds)
	}
}
