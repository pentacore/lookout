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
		mount         func(t *testing.T) string
		wantStatus    wire.CheckStatus
		wantImmutable any
		wantContains  string
	}{
		{
			name:          "immutable",
			mount:         func(t *testing.T) string { return mountOverDir(t, true) },
			wantStatus:    wire.StatusOK,
			wantImmutable: true,
		},
		{
			name:          "not immutable",
			mount:         func(t *testing.T) string { return mountOverDir(t, false) },
			wantStatus:    wire.StatusWarn,
			wantImmutable: false,
		},
		{
			name:          "underlying entry is a file",
			mount:         mountOverFile,
			wantStatus:    wire.StatusError,
			wantImmutable: nil,
			wantContains:  "not a directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.mount(t)
			runtimeDir, peerDir := sharedRuntimeDir(t)
			namespaceBefore := mainMountNamespace(t)

			hookCalls := 0
			inspectMountedHook = func(dir string) {
				hookCalls++
				assertNotInHostNamespace(t)
				assertReadOnlyInPrivateNamespace(t, dir)
			}
			t.Cleanup(func() { inspectMountedHook = nil })

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

			if !strings.Contains(got.Message, tt.wantContains) {
				t.Errorf("message = %q, want it to contain %q", got.Message, tt.wantContains)
			}

			if hookCalls != 1 {
				t.Errorf("inspect reached the mounted state %d times, want 1", hookCalls)
			}

			if after := mainMountNamespace(t); after != namespaceBefore {
				t.Errorf("main thread mount namespace changed from %s to %s", namespaceBefore, after)
			}

			assertNoLeftovers(t, runtimeDir)
			assertNoLeftovers(t, peerDir)
			assertNotInHostNamespace(t)
		})
	}
}

// mountOverDir mounts tmpfs at base/parent, creates base/parent/mp (with
// FS_IMMUTABLE_FL when immutable) and mounts another tmpfs on top of it. It
// returns the mp path; cleanup unmounts mp, clears the flag and unmounts
// parent.
func mountOverDir(t *testing.T, immutable bool) string {
	t.Helper()

	parent := mountTmpfs(t, filepath.Join(t.TempDir(), "parent"))
	mp := filepath.Join(parent, "mp")

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

	return mountTmpfs(t, mp)
}

// mountOverFile mounts tmpfs at base/parent, creates the file base/parent/mp
// and bind-mounts another file on top of it, so the underlying entry is not a
// directory.
func mountOverFile(t *testing.T) string {
	t.Helper()

	parent := mountTmpfs(t, filepath.Join(t.TempDir(), "parent"))
	mp := filepath.Join(parent, "mp")
	source := filepath.Join(t.TempDir(), "source")

	for _, file := range []string{mp, source} {
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := unix.Mount(source, mp, "", unix.MS_BIND, ""); err != nil {
		t.Fatalf("bind mount %s at %s: %v", source, mp, err)
	}

	t.Cleanup(func() { mustUnmount(t, mp) })

	return mp
}

// sharedRuntimeDir returns a runtime dir on a shared tmpfs plus a peer bind
// of it, so a mount made under the runtime dir in this namespace would show
// up under the peer too.
func sharedRuntimeDir(t *testing.T) (runtimeDir, peerDir string) {
	t.Helper()

	base := t.TempDir()
	runtimeDir = mountTmpfs(t, filepath.Join(base, "run"))

	if err := unix.Mount("", runtimeDir, "", unix.MS_SHARED, ""); err != nil {
		t.Fatalf("make %s shared: %v", runtimeDir, err)
	}

	peerDir = filepath.Join(base, "peer")

	if err := os.Mkdir(peerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := unix.Mount(runtimeDir, peerDir, "", unix.MS_BIND, ""); err != nil {
		t.Fatalf("bind %s at %s: %v", runtimeDir, peerDir, err)
	}

	t.Cleanup(func() { mustUnmount(t, peerDir) })

	return runtimeDir, peerDir
}

// mountTmpfs creates path, mounts a tmpfs there and unmounts it on cleanup.
func mountTmpfs(t *testing.T, path string) string {
	t.Helper()

	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := unix.Mount("tmpfs", path, "tmpfs", 0, ""); err != nil {
		t.Fatalf("mount tmpfs at %s: %v", path, err)
	}

	t.Cleanup(func() { mustUnmount(t, path) })

	return path
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

// mainMountNamespace identifies the mount namespace /proc/self resolves
// through, i.e. the main thread's.
func mainMountNamespace(t *testing.T) string {
	t.Helper()

	namespace, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		t.Fatal(err)
	}

	return namespace
}

// assertNotInHostNamespace fails when the main thread's namespace shows an
// inspection mount anywhere, including under a peer of the runtime dir.
func assertNotInHostNamespace(t *testing.T) {
	t.Helper()

	for _, line := range mountInfoLines(t, "/proc/self/mountinfo") {
		if strings.Contains(line, "/mp-") {
			t.Errorf("inspection mount visible in the host namespace: %s", line)
		}
	}
}

// assertReadOnlyInPrivateNamespace fails unless the calling thread's
// namespace has dir mounted read-only.
func assertReadOnlyInPrivateNamespace(t *testing.T, dir string) {
	t.Helper()

	for _, line := range mountInfoLines(t, "/proc/thread-self/mountinfo") {
		fields := strings.Fields(line)
		if len(fields) > 5 && collect.Unescape(fields[4]) == dir {
			if !strings.HasPrefix(fields[5], "ro") {
				t.Errorf("inspection mount is not read-only: %s", line)
			}

			return
		}
	}

	t.Errorf("inspection mount %s not found in the private namespace", dir)
}

func mountInfoLines(t *testing.T, path string) []string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Error(err)

		return nil
	}

	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// assertNoLeftovers fails when dir holds a leftover temp dir or the host
// namespace has a mount anywhere under dir.
func assertNoLeftovers(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 0 {
		t.Errorf("%s has %d leftover entries, want none", dir, len(entries))
	}

	for _, line := range mountInfoLines(t, "/proc/self/mountinfo") {
		if fields := strings.Fields(line); len(fields) > 4 && strings.HasPrefix(collect.Unescape(fields[4]), dir+"/") {
			t.Errorf("mount left behind under %s: %s", dir, line)
		}
	}
}
