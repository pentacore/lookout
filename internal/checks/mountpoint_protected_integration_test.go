//go:build linux && integration

package checks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pentacore/lookout/internal/collect"
	"github.com/pentacore/lookout/internal/wire"
	"golang.org/x/sys/unix"
)

func TestIntegrationMountpointProtected(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to mount tmpfs and bind mounts")
	}

	tests := []struct {
		name          string
		immutable     bool
		wantStatus    wire.CheckStatus
		wantImmutable bool
	}{
		{name: "immutable", immutable: true, wantStatus: wire.StatusOK, wantImmutable: true},
		{name: "not immutable", immutable: false, wantStatus: wire.StatusWarn, wantImmutable: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := mountOverDir(t, tt.immutable)
			runtimeDir := t.TempDir()

			env := Env{
				Root:       "/",
				RuntimeDir: runtimeDir,
				Mounts:     func() (collect.MountTable, error) { return collect.ReadMountTable("/") },
				Now:        time.Now,
			}

			params, err := json.Marshal(map[string]string{"path": path})
			if err != nil {
				t.Fatal(err)
			}

			got := newMountpointProtected(inspectUnderlying).Run(context.Background(), env, params)

			if got.Status != tt.wantStatus || got.Observed["immutable"] != tt.wantImmutable {
				t.Errorf("got %q %v (%s), want %q immutable=%v",
					got.Status, got.Observed, got.Message, tt.wantStatus, tt.wantImmutable)
			}

			assertNoLeftovers(t, runtimeDir)
		})
	}
}

// mountOverDir mounts tmpfs at base/parent, creates base/parent/mp (with
// FS_IMMUTABLE_FL when immutable) and mounts another tmpfs on top of it. It
// returns the mp path; cleanup unmounts mp, clears the flag and unmounts
// parent.
func mountOverDir(t *testing.T, immutable bool) string {
	t.Helper()

	parent := filepath.Join(t.TempDir(), "parent")
	mp := filepath.Join(parent, "mp")

	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := unix.Mount("tmpfs", parent, "tmpfs", 0, ""); err != nil {
		t.Fatalf("mount tmpfs at %s: %v", parent, err)
	}

	t.Cleanup(func() { mustUnmount(t, parent) })

	if err := os.Mkdir(mp, 0o755); err != nil {
		t.Fatal(err)
	}

	if immutable {
		if err := setImmutable(mp, true); err != nil {
			if errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EOPNOTSUPP) {
				t.Skipf("tmpfs does not support file attributes on this kernel (needs >= 6.0): %v", err)
			}

			t.Fatalf("set immutable on %s: %v", mp, err)
		}

		t.Cleanup(func() {
			if err := setImmutable(mp, false); err != nil {
				t.Errorf("clear immutable on %s: %v", mp, err)
			}
		})
	}

	if err := unix.Mount("tmpfs", mp, "tmpfs", 0, ""); err != nil {
		t.Fatalf("mount tmpfs at %s: %v", mp, err)
	}

	t.Cleanup(func() { mustUnmount(t, mp) })

	return mp
}

func setImmutable(path string, on bool) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}

	defer func() { _ = unix.Close(fd) }()

	flags, err := unix.IoctlGetUint32(fd, unix.FS_IOC_GETFLAGS)
	if err != nil {
		return err
	}

	if on {
		flags |= fsImmutableFl
	} else {
		flags &^= fsImmutableFl
	}

	return unix.IoctlSetPointerInt(fd, unix.FS_IOC_SETFLAGS, int(flags))
}

func mustUnmount(t *testing.T, path string) {
	t.Helper()

	if err := unix.Unmount(path, 0); err != nil {
		t.Errorf("unmount %s: %v", path, err)
	}
}

// assertNoLeftovers fails when the check left a temp dir in runtimeDir or a
// mount anywhere under it.
func assertNoLeftovers(t *testing.T, runtimeDir string) {
	t.Helper()

	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 0 {
		t.Errorf("runtime dir has %d leftover entries, want none", len(entries))
	}

	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}

	for line := range strings.Lines(string(mountinfo)) {
		if strings.Contains(line, runtimeDir) {
			t.Errorf("mount left behind: %s", strings.TrimSpace(line))
		}
	}
}
