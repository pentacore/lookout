package checks

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pentacore/lookout/internal/wire"
)

// writeAged creates a file under the env root whose mtime is age before testNow.
func writeAged(t *testing.T, env Env, path string, age time.Duration) {
	t.Helper()

	host := env.HostPath(path)

	if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(host, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	mtime := testNow.Add(-age)
	if err := os.Chtimes(host, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func hours(h float64) time.Duration {
	return time.Duration(h * float64(time.Hour))
}

const freshnessParams = `{"path":"/backups","warn_hours":26,"crit_hours":48}`

func TestFileFreshness(t *testing.T) {
	tests := []struct {
		checkCase
		setup func(t *testing.T, env Env)
	}{
		{
			checkCase: checkCase{
				name:         "fresh 2h",
				params:       freshnessParams,
				wantStatus:   wire.StatusOK,
				wantMessage:  "newest file /backups/b.tar is 2 hours old",
				wantObserved: map[string]any{"newest_file": "/backups/b.tar", "age_hours": float64(2)},
			},
			setup: func(t *testing.T, env Env) { writeAged(t, env, "/backups/b.tar", hours(2)) },
		},
		{
			checkCase: checkCase{
				name:         "30h warns",
				params:       freshnessParams,
				wantStatus:   wire.StatusWarn,
				wantMessage:  "newest file /backups/b.tar is 30 hours old, warn after 26",
				wantObserved: map[string]any{"newest_file": "/backups/b.tar", "age_hours": float64(30)},
			},
			setup: func(t *testing.T, env Env) { writeAged(t, env, "/backups/b.tar", hours(30)) },
		},
		{
			checkCase: checkCase{
				name:         "50h is critical",
				params:       freshnessParams,
				wantStatus:   wire.StatusCrit,
				wantMessage:  "newest file /backups/b.tar is 50 hours old, crit after 48",
				wantObserved: map[string]any{"newest_file": "/backups/b.tar", "age_hours": float64(50)},
			},
			setup: func(t *testing.T, env Env) { writeAged(t, env, "/backups/b.tar", hours(50)) },
		},
		{
			checkCase: checkCase{
				name:         "exactly at the warn threshold is ok",
				params:       freshnessParams,
				wantStatus:   wire.StatusOK,
				wantObserved: map[string]any{"newest_file": "/backups/b.tar", "age_hours": float64(26)},
			},
			setup: func(t *testing.T, env Env) { writeAged(t, env, "/backups/b.tar", hours(26)) },
		},
		{
			checkCase: checkCase{
				name:         "age rounds to 2 decimals",
				params:       freshnessParams,
				wantStatus:   wire.StatusOK,
				wantObserved: map[string]any{"newest_file": "/backups/b.tar", "age_hours": 1.51},
			},
			setup: func(t *testing.T, env Env) {
				writeAged(t, env, "/backups/b.tar", time.Hour+30*time.Minute+30*time.Second)
			},
		},
		{
			checkCase: checkCase{
				name:         "newest of several",
				params:       freshnessParams,
				wantStatus:   wire.StatusOK,
				wantObserved: map[string]any{"newest_file": "/backups/new.tar", "age_hours": float64(1)},
			},
			setup: func(t *testing.T, env Env) {
				writeAged(t, env, "/backups/old.tar", hours(40))
				writeAged(t, env, "/backups/new.tar", hours(1))
			},
		},
		{
			checkCase: checkCase{
				name:         "empty dir",
				params:       freshnessParams,
				wantStatus:   wire.StatusCrit,
				wantMessage:  "no files matching * in /backups",
				wantObserved: map[string]any{"newest_file": nil, "age_hours": nil},
			},
			setup: func(t *testing.T, env Env) { mkdirUnder(t, env, "/backups") },
		},
		{
			checkCase: checkCase{
				name:         "pattern ignores newer non-matching file",
				params:       `{"path":"/backups","pattern":"*.tar","warn_hours":26,"crit_hours":48}`,
				wantStatus:   wire.StatusOK,
				wantObserved: map[string]any{"newest_file": "/backups/b.tar", "age_hours": float64(2)},
			},
			setup: func(t *testing.T, env Env) {
				writeAged(t, env, "/backups/b.tar", hours(2))
				writeAged(t, env, "/backups/a.log", hours(1))
			},
		},
		{
			checkCase: checkCase{
				name:        "pattern with no match",
				params:      `{"path":"/backups","pattern":"*.tar","warn_hours":26,"crit_hours":48}`,
				wantStatus:  wire.StatusCrit,
				wantMessage: "no files matching *.tar in /backups",
			},
			setup: func(t *testing.T, env Env) { writeAged(t, env, "/backups/a.log", hours(1)) },
		},
		{
			checkCase: checkCase{
				name:         "newer subdirectory is ignored",
				params:       freshnessParams,
				wantStatus:   wire.StatusWarn,
				wantObserved: map[string]any{"newest_file": "/backups/b.tar", "age_hours": float64(30)},
			},
			setup: func(t *testing.T, env Env) {
				writeAged(t, env, "/backups/b.tar", hours(30))
				writeAged(t, env, "/backups/sub/c.tar", hours(1))
			},
		},
		{
			checkCase: checkCase{
				name:         "symlinked file is followed",
				params:       freshnessParams,
				wantStatus:   wire.StatusOK,
				wantObserved: map[string]any{"newest_file": "/backups/link.tar", "age_hours": float64(3)},
			},
			setup: func(t *testing.T, env Env) {
				writeAged(t, env, "/elsewhere/real.tar", hours(3))
				mkdirUnder(t, env, "/backups")

				if err := os.Symlink(env.HostPath("/elsewhere/real.tar"), env.HostPath("/backups/link.tar")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			checkCase: checkCase{
				name:         "broken symlink is skipped",
				params:       freshnessParams,
				wantStatus:   wire.StatusOK,
				wantObserved: map[string]any{"newest_file": "/backups/b.tar", "age_hours": float64(2)},
			},
			setup: func(t *testing.T, env Env) {
				writeAged(t, env, "/backups/b.tar", hours(2))

				if err := os.Symlink(env.HostPath("/nowhere"), env.HostPath("/backups/dead.tar")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			checkCase: checkCase{
				name:         "path is a file",
				params:       `{"path":"/backups/b.tar","pattern":"*.zip","warn_hours":26,"crit_hours":48}`,
				wantStatus:   wire.StatusWarn,
				wantObserved: map[string]any{"newest_file": "/backups/b.tar", "age_hours": float64(30)},
			},
			setup: func(t *testing.T, env Env) { writeAged(t, env, "/backups/b.tar", hours(30)) },
		},
		{
			checkCase: checkCase{
				name:        "path missing",
				params:      freshnessParams,
				wantStatus:  wire.StatusCrit,
				wantMessage: "/backups does not exist",
			},
		},
		{
			checkCase: checkCase{
				name:         "bad pattern",
				params:       `{"path":"/backups","pattern":"[","warn_hours":26,"crit_hours":48}`,
				wantStatus:   wire.StatusError,
				wantContains: "invalid pattern",
			},
			setup: func(t *testing.T, env Env) { writeAged(t, env, "/backups/b.tar", hours(1)) },
		},
		{
			checkCase: checkCase{
				name:        "missing path param",
				params:      `{"warn_hours":26,"crit_hours":48}`,
				wantStatus:  wire.StatusError,
				wantMessage: "missing required param: path",
			},
		},
		{
			checkCase: checkCase{
				name:        "missing warn_hours",
				params:      `{"path":"/backups","crit_hours":48}`,
				wantStatus:  wire.StatusError,
				wantMessage: "missing required param: warn_hours",
			},
		},
		{
			checkCase: checkCase{
				name:        "missing crit_hours",
				params:      `{"path":"/backups","warn_hours":26}`,
				wantStatus:  wire.StatusError,
				wantMessage: "missing required param: crit_hours",
			},
		},
		{
			checkCase: checkCase{
				name:         "future mtime",
				params:       freshnessParams,
				wantStatus:   wire.StatusOK,
				wantObserved: map[string]any{"newest_file": "/backups/b.tar", "age_hours": float64(0)},
			},
			setup: func(t *testing.T, env Env) { writeAged(t, env, "/backups/b.tar", -hours(3)) },
		},
		{
			checkCase: checkCase{
				name:         "invalid params",
				params:       `{"path":"/backups","warn_hours":"soon","crit_hours":48}`,
				wantStatus:   wire.StatusError,
				wantContains: "invalid params: ",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t, testMountInfo)

			if tt.setup != nil {
				tt.setup(t, env)
			}

			tt.assert(t, runCheck(fileFreshness, env, tt.params))
		})
	}
}

func TestFileFreshnessPermissionDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}

	env := newTestEnv(t, testMountInfo)
	writeAged(t, env, "/backups/b.tar", hours(1))

	if err := os.Chmod(env.HostPath("/backups"), 0o000); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(env.HostPath("/backups"), 0o755) })

	got := runCheck(fileFreshness, env, freshnessParams)

	if got.Status != wire.StatusError {
		t.Fatalf("status = %q (%s), want error", got.Status, got.Message)
	}
}
