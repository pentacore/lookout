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

	for _, mode := range inspectionModes(t) {
		for _, tt := range tests {
			t.Run(mode+"/"+tt.name, func(t *testing.T) {
				useMode(mode)
				path := tt.mount(t)
				runtimeDir, peerDir := sharedRuntimeDir(t)
				namespaceBefore := mainMountNamespace(t)

				hookCalls := 0
				inspectMountedHook = func(gotMode string, mountFD int) {
					hookCalls++
					if gotMode != mode {
						t.Errorf("inspection mode = %s, want %s", gotMode, mode)
					}
					assertNotInHostNamespace(t)
					assertMountFlags(t, mountFD, unix.ST_RDONLY)
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
}

// inspectionModes lists the modes this kernel can run. Each one sets
// forceNamespaceInspection for the subtests that follow it.
func inspectionModes(t *testing.T) []string {
	t.Helper()

	t.Cleanup(func() { forceNamespaceInspection = false })

	modes := []string{inspectNamespace}

	tree, err := unix.OpenTree(unix.AT_FDCWD, "/", unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	if err == nil {
		_ = unix.Close(tree)

		var attr unix.MountAttr
		if unix.MountSetattr(-1, "", 0, &attr) != unix.ENOSYS {
			modes = append(modes, inspectDetached)
		}
	}

	if len(modes) == 1 {
		t.Logf("kernel lacks open_tree or mount_setattr; testing the namespace mode only")
	}

	return modes
}

// useMode makes inspectUnderlying take mode's path.
func useMode(mode string) {
	forceNamespaceInspection = mode == inspectNamespace
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

// assertMountFlags fails unless the mount mountFD belongs to has every
// statfs flag in want.
func assertMountFlags(t *testing.T, mountFD int, want uint64) {
	t.Helper()

	var st unix.Statfs_t
	if err := unix.Fstatfs(mountFD, &st); err != nil {
		t.Errorf("fstatfs the inspection mount: %v", err)

		return
	}

	if got := uint64(st.Flags); got&want != want {
		t.Errorf("inspection mount flags = %#x, want %#x set", got, want)
	}
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

// TestIntegrationMountpointProtectedHungInspection hangs the real inspection
// past the runner's budget: the check ID must stay in flight, so the next
// cycle reports it instead of starting another inspection.
func TestIntegrationMountpointProtectedHungInspection(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to mount tmpfs and bind mounts")
	}

	for _, mode := range inspectionModes(t) {
		t.Run(mode, func(t *testing.T) {
			useMode(mode)

			path := mountOverDir(t, false)
			runtimeDir, _ := sharedRuntimeDir(t)

			release := make(chan struct{})
			hookCalls := make(chan struct{}, 10)
			inspectMountedHook = func(string, int) {
				hookCalls <- struct{}{}
				<-release
			}
			t.Cleanup(func() { inspectMountedHook = nil })

			runner := &Runner{
				Registry: Registry{"mountpoint_protected": newMountpointProtected(inspectUnderlying).Run},
				Env: Env{
					Root:       "/",
					RuntimeDir: runtimeDir,
					Mounts:     func() (collect.MountTable, error) { return collect.ReadMountTable("/") },
					Now:        time.Now,
				},
				Budget: 100 * time.Millisecond,
			}

			params, err := json.Marshal(map[string]string{"path": path})
			if err != nil {
				t.Fatal(err)
			}

			cfgs := []wire.CheckConfig{{ID: "mp", Type: "mountpoint_protected", Params: params}}

			first := runner.Run(context.Background(), cfgs)

			// Give an early-returning check time to release its in-flight slot.
			time.Sleep(100 * time.Millisecond)

			second := runner.Run(context.Background(), cfgs)

			if !strings.HasPrefix(first[0].Message, "timed out") {
				t.Errorf("first run = %q, want a timeout", first[0].Message)
			}

			if !strings.Contains(second[0].Message, "previous run still in progress") {
				t.Errorf("second run = %q, want the previous run still in progress", second[0].Message)
			}

			close(release)

			if got := len(hookCalls); got != 1 {
				t.Errorf("inspections started = %d, want 1", got)
			}

			deadline := time.Now().Add(5 * time.Second)
			for runner.Run(context.Background(), cfgs)[0].Status == wire.StatusError && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}

			assertNoLeftovers(t, runtimeDir)
			assertNotInHostNamespace(t)
		})
	}
}

// The read-only copy keeps the parent's nosuid, nodev, noexec and noatime.
// A bind remount that dropped them fails with EPERM where they are locked (a
// user namespace or an LXC container).
func TestIntegrationMountpointProtectedKeepsMountFlags(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to mount tmpfs and bind mounts")
	}

	for _, mode := range inspectionModes(t) {
		t.Run(mode, func(t *testing.T) {
			useMode(mode)

			parent := filepath.Join(t.TempDir(), "parent")
			if err := os.MkdirAll(parent, 0o755); err != nil {
				t.Fatal(err)
			}

			flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC | unix.MS_NOATIME)
			if err := unix.Mount("tmpfs", parent, "tmpfs", flags, ""); err != nil {
				t.Fatalf("mount tmpfs at %s: %v", parent, err)
			}

			t.Cleanup(func() { mustUnmount(t, parent) })

			mp := filepath.Join(parent, "mp")
			if err := os.Mkdir(mp, 0o755); err != nil {
				t.Fatal(err)
			}

			mountTmpfs(t, mp)

			runtimeDir, _ := sharedRuntimeDir(t)

			inspectMountedHook = func(_ string, mountFD int) {
				assertMountFlags(t, mountFD, unix.ST_RDONLY|unix.ST_NOSUID|unix.ST_NODEV|unix.ST_NOEXEC|unix.ST_NOATIME)
			}
			t.Cleanup(func() { inspectMountedHook = nil })

			env := Env{
				Root:       "/",
				RuntimeDir: runtimeDir,
				Mounts:     func() (collect.MountTable, error) { return collect.ReadMountTable("/") },
				Now:        time.Now,
			}

			params, err := json.Marshal(map[string]string{"path": mp})
			if err != nil {
				t.Fatal(err)
			}

			got := newMountpointProtected(inspectUnderlying).Run(context.Background(), env, params)
			if got.Status != wire.StatusWarn {
				t.Errorf("got %q (%s), want warn", got.Status, got.Message)
			}
		})
	}
}

// A hung inspection must not keep other host filesystems busy: once the host
// unmounts one, no thread of the agent may still have it mounted. On Unraid
// a pinned array disk cannot be released when the array stops.
func TestIntegrationMountpointProtectedHungInspectionDoesNotPinMounts(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to mount tmpfs and bind mounts")
	}

	for _, mode := range inspectionModes(t) {
		t.Run(mode, func(t *testing.T) {
			useMode(mode)

			path := mountOverDir(t, false)
			runtimeDir, _ := sharedRuntimeDir(t)

			// An unrelated filesystem below a shared mount, as systemd sets
			// up the host: an unmount there propagates to its peers and slaves.
			base := mountTmpfs(t, filepath.Join(t.TempDir(), "base"))
			if err := unix.Mount("", base, "", unix.MS_SHARED, ""); err != nil {
				t.Fatalf("make %s shared: %v", base, err)
			}

			other := filepath.Join(base, "other")
			if err := os.Mkdir(other, 0o755); err != nil {
				t.Fatal(err)
			}

			if err := unix.Mount("tmpfs", other, "tmpfs", 0, "size=1m"); err != nil {
				t.Fatalf("mount tmpfs at %s: %v", other, err)
			}

			mounted := make(chan struct{})
			release := make(chan struct{})
			inspectMountedHook = func(string, int) {
				close(mounted)
				<-release
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

			done := make(chan Outcome, 1)

			go func() {
				done <- newMountpointProtected(inspectUnderlying).Run(context.Background(), env, params)
			}()

			select {
			case <-mounted:
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("inspection did not start")
			}

			if err := unix.Unmount(other, 0); err != nil {
				close(release)
				t.Fatalf("unmount %s while an inspection hangs: %v", other, err)
			}

			for _, task := range taskMountInfos(t) {
				for _, line := range mountInfoLines(t, task) {
					if fields := strings.Fields(line); len(fields) > 4 && collect.Unescape(fields[4]) == other {
						t.Errorf("%s still has %s mounted after the host unmounted it: %s", task, other, line)
					}
				}
			}

			close(release)

			select {
			case got := <-done:
				if got.Status != wire.StatusWarn {
					t.Errorf("got %q (%s), want warn", got.Status, got.Message)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("inspection did not finish after release")
			}
		})
	}
}

// taskMountInfos lists the mountinfo file of every thread of this process.
func taskMountInfos(t *testing.T) []string {
	t.Helper()

	paths, err := filepath.Glob("/proc/self/task/*/mountinfo")
	if err != nil || len(paths) == 0 {
		t.Fatalf("list thread mountinfo files: %v", err)
	}

	return paths
}
