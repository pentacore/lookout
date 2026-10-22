package checks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pentacore/lookout/internal/wire"
)

const rootOnlyMountInfo = "1 0 8:1 / / rw - ext4 /dev/sda1 rw\n"

func mkdirUnder(t *testing.T, env Env, path string) {
	t.Helper()

	if err := os.MkdirAll(env.HostPath(path), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestPathOnMount(t *testing.T) {
	tests := []struct {
		checkCase
		mountinfo string
		setup     func(t *testing.T, env Env)
	}{
		{
			checkCase: checkCase{
				name:         "on mount",
				params:       `{"path":"/mnt/backups/daily","mount":"/mnt/backups"}`,
				wantStatus:   wire.StatusOK,
				wantMessage:  "/mnt/backups/daily is on /mnt/backups",
				wantObserved: map[string]any{"actual_mount": "/mnt/backups"},
			},
			mountinfo: testMountInfo,
			setup:     func(t *testing.T, env Env) { mkdirUnder(t, env, "/mnt/backups/daily") },
		},
		{
			checkCase: checkCase{
				name:         "fell back to root",
				params:       `{"path":"/mnt/backups/daily","mount":"/mnt/backups"}`,
				wantStatus:   wire.StatusCrit,
				wantMessage:  "/mnt/backups/daily is on /, expected /mnt/backups",
				wantObserved: map[string]any{"actual_mount": "/"},
			},
			mountinfo: rootOnlyMountInfo,
			setup:     func(t *testing.T, env Env) { mkdirUnder(t, env, "/mnt/backups/daily") },
		},
		{
			checkCase: checkCase{
				name:         "trailing slashes are cleaned",
				params:       `{"path":"/mnt/backups/daily/","mount":"/mnt/backups/"}`,
				wantStatus:   wire.StatusOK,
				wantObserved: map[string]any{"actual_mount": "/mnt/backups"},
			},
			mountinfo: testMountInfo,
			setup:     func(t *testing.T, env Env) { mkdirUnder(t, env, "/mnt/backups/daily") },
		},
		{
			checkCase: checkCase{
				name:         "missing path",
				params:       `{"path":"/mnt/backups/nope","mount":"/mnt/backups"}`,
				wantStatus:   wire.StatusError,
				wantContains: "does not exist",
			},
			mountinfo: testMountInfo,
		},
		{
			checkCase: checkCase{
				name:         "relative symlink",
				params:       `{"path":"/srv/backups","mount":"/mnt/backups"}`,
				wantStatus:   wire.StatusOK,
				wantObserved: map[string]any{"actual_mount": "/mnt/backups"},
			},
			mountinfo: testMountInfo,
			setup: func(t *testing.T, env Env) {
				mkdirUnder(t, env, "/mnt/backups/daily")
				mkdirUnder(t, env, "/srv")

				if err := os.Symlink("../mnt/backups/daily", env.HostPath("/srv/backups")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			checkCase: checkCase{
				name:         "symlink escaping the root",
				params:       `{"path":"/srv/out","mount":"/"}`,
				wantStatus:   wire.StatusError,
				wantContains: "outside",
			},
			mountinfo: testMountInfo,
			setup: func(t *testing.T, env Env) {
				mkdirUnder(t, env, "/srv")

				if err := os.Symlink(t.TempDir(), env.HostPath("/srv/out")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			checkCase: checkCase{
				name:         "path with a space",
				params:       `{"path":"/mnt/appdata cache/x","mount":"/mnt/appdata cache"}`,
				wantStatus:   wire.StatusOK,
				wantObserved: map[string]any{"actual_mount": "/mnt/appdata cache"},
			},
			mountinfo: testMountInfo,
			setup:     func(t *testing.T, env Env) { mkdirUnder(t, env, "/mnt/appdata cache/x") },
		},
		{
			checkCase: checkCase{
				name:        "missing path param",
				params:      `{"mount":"/mnt/backups"}`,
				wantStatus:  wire.StatusError,
				wantMessage: "missing required param: path",
			},
			mountinfo: testMountInfo,
		},
		{
			checkCase: checkCase{
				name:        "missing mount param",
				params:      `{"path":"/mnt/backups"}`,
				wantStatus:  wire.StatusError,
				wantMessage: "missing required param: mount",
			},
			mountinfo: testMountInfo,
		},
		{
			checkCase: checkCase{
				name:         "invalid params",
				params:       `[]`,
				wantStatus:   wire.StatusError,
				wantContains: "invalid params: ",
			},
			mountinfo: testMountInfo,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t, tt.mountinfo)

			if tt.setup != nil {
				tt.setup(t, env)
			}

			tt.assert(t, runCheck(pathOnMount, env, tt.params))
		})
	}
}

// TestPathOnMountSymlinkedRoot covers a Root that is itself reached through a
// symlink, as macOS temp dirs are (/var -> /private/var).
func TestPathOnMountSymlinkedRoot(t *testing.T) {
	env := newTestEnv(t, testMountInfo)
	mkdirUnder(t, env, "/mnt/backups/daily")

	link := filepath.Join(t.TempDir(), "rootlink")
	if err := os.Symlink(env.Root, link); err != nil {
		t.Fatal(err)
	}

	env.Root = link

	got := runCheck(pathOnMount, env, `{"path":"/mnt/backups/daily","mount":"/mnt/backups"}`)

	if got.Status != wire.StatusOK {
		t.Fatalf("status = %q (%s), want ok", got.Status, got.Message)
	}
}
