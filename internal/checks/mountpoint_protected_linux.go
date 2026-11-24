package checks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/pentacore/lookout/internal/collect"
	"golang.org/x/sys/unix"
)

// fsImmutableFl is FS_IMMUTABLE_FL from linux/fs.h, which golang.org/x/sys
// does not export.
const fsImmutableFl = 0x00000010

// Inspection modes, as passed to inspectMountedHook.
const (
	inspectDetached  = "detached"
	inspectNamespace = "namespace"
)

// inspectMountedHook, when set by a test, runs once the read-only copy of
// the parent mount is in place, with the mode in use and a file descriptor
// for the copy's root (valid only during the call).
var inspectMountedHook func(mode string, mountFD int)

// forceNamespaceInspection makes tests use the private-namespace path on
// kernels that support the detached one.
var forceNamespaceInspection bool

// atimeFlags maps the statfs atime flags to the mount flags that keep them.
var atimeFlags = []struct{ st, ms uintptr }{
	{unix.ST_NOATIME, unix.MS_NOATIME},
	{unix.ST_NODIRATIME, unix.MS_NODIRATIME},
	{unix.ST_RELATIME, unix.MS_RELATIME},
}

type inspectResult struct {
	immutable bool
	err       error
}

// inspectUnderlying reads FS_IMMUTABLE_FL from the directory underneath a
// mount point. ctx is checked only before starting: the call then blocks
// until the inspection finishes, even when it hangs on a hung parent
// filesystem, so the runner keeps the check in flight instead of starting
// another inspection every cycle.
//
// On Linux 5.12 and later it inspects a detached clone of the parent mount
// (see inspectDetachedClone), which references nothing but the parent
// filesystem. Older kernels use a dedicated OS thread in its own mount
// namespace (see inspectInPrivateNamespace).
func inspectUnderlying(ctx context.Context, env Env, parent collect.Mount, rel string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	if !forceNamespaceInspection {
		immutable, err := inspectDetachedClone(env, parent, rel)
		if !errors.Is(err, unix.ENOSYS) {
			return immutable, err
		}
	}

	results := make(chan inspectResult, 1)

	go func() { results <- inspectOnPrivateThread(env, parent, rel) }()

	result := <-results

	return result.immutable, result.err
}

// inspectDetachedClone clones parent without its submounts into a detached
// mount (open_tree), makes the clone read-only (mount_setattr, which leaves
// nosuid, nodev and the other flags as they are) and opens rel inside it. The
// clone belongs to no mount namespace, so nothing is mounted anywhere, and the
// kernel dissolves it when the last file descriptor closes. A hung inspection
// therefore pins only the parent filesystem, not every host mount. It returns
// an error wrapping ENOSYS on kernels without open_tree or mount_setattr.
func inspectDetachedClone(env Env, parent collect.Mount, rel string) (bool, error) {
	source := env.HostPath(parent.MountPoint)

	// No AT_RECURSIVE: submounts, including the checked path's own mount, are not carried.
	tree, err := unix.OpenTree(unix.AT_FDCWD, source, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return false, fmt.Errorf("clone mount %s: %w", source, err)
	}

	defer func() { _ = unix.Close(tree) }()

	err = unix.MountSetattr(tree, "", unix.AT_EMPTY_PATH, &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY})
	if err != nil {
		return false, fmt.Errorf("make the clone of %s read-only: %w", source, err)
	}

	if inspectMountedHook != nil {
		inspectMountedHook(inspectDetached, tree)
	}

	return readImmutable(tree, rel, filepath.Join(parent.MountPoint, rel))
}

// readImmutable opens rel below dirfd without following a final symlink and
// reports whether it carries FS_IMMUTABLE_FL. underlying names it in errors.
func readImmutable(dirfd int, rel, underlying string) (bool, error) {
	fd, err := unix.Openat(dirfd, rel, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", underlying, err)
	}

	defer func() { _ = unix.Close(fd) }()

	flags, err := unix.IoctlGetUint32(fd, unix.FS_IOC_GETFLAGS)
	if err != nil {
		return false, fmt.Errorf("read attributes of %s: %w", underlying, err)
	}

	return flags&fsImmutableFl != 0, nil
}

// readOnlyRemountFlags returns the mount(2) flags that make a bind mount
// read-only while keeping the nosuid, nodev, noexec and atime flags statfs
// reported for it. A bind remount resets every flag it does not name, and
// the kernel refuses (EPERM) to clear flags that are locked, as they are in
// a user namespace or an LXC container.
func readOnlyRemountFlags(statfsFlags uint64) uintptr {
	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)

	strict := true

	for _, f := range atimeFlags {
		if uintptr(statfsFlags)&f.st != 0 {
			flags |= f.ms

			if f.ms != unix.MS_NODIRATIME {
				strict = false
			}
		}
	}

	if strict {
		flags |= unix.MS_STRICTATIME
	}

	return flags
}

// withCleanup adds a failed unmount or rmdir to err, joined with "; " so the
// outcome message stays on one line.
func withCleanup(err, cleanupErr error) error {
	cleanup := cleanupError{err: cleanupErr}
	if err == nil {
		return cleanup
	}

	return fmt.Errorf("%w; %w", err, cleanup)
}

// inspectOnPrivateThread locks the calling goroutine to its OS thread and
// never unlocks it, so the runtime terminates the thread, and the kernel
// discards its mount namespace and every mount in it, when the goroutine
// exits. The main thread is never moved: /proc/self resolves through it, and
// the runtime wedges rather than terminates it. On the main thread the work is
// handed to a new goroutine, which cannot land there while it stays locked.
func inspectOnPrivateThread(env Env, parent collect.Mount, rel string) inspectResult {
	runtime.LockOSThread()

	if unix.Gettid() == unix.Getpid() {
		defer runtime.UnlockOSThread()

		results := make(chan inspectResult, 1)

		go func() { results <- inspectOnPrivateThread(env, parent, rel) }()

		return <-results
	}

	immutable, err := inspectInPrivateNamespace(env, parent, rel)

	return inspectResult{immutable: immutable, err: err}
}

// inspectInPrivateNamespace must run on a locked thread that is discarded
// afterwards. It unshares the mount namespace and makes every mount in it a
// slave, so nothing done here reaches the host while host unmounts still
// reach the copies (which would otherwise keep every host filesystem busy for
// as long as an inspection hangs). It then bind-mounts parent
// non-recursively into a temp dir under env.RuntimeDir (tmpfs, never the
// Unraid flash state dir), so mounts on top of parent are not carried and rel
// shows the directory underneath them. The bind is made private, so no host
// mount can propagate onto it, and remounted read-only before the directory
// is opened. Unmount and rmdir still run for tidiness; their failures are
// added to err as cleanupError.
func inspectInPrivateNamespace(env Env, parent collect.Mount, rel string) (immutable bool, err error) {
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		return false, fmt.Errorf("unshare mount namespace: %w", err)
	}

	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
		return false, fmt.Errorf("make mounts slaves: %w", err)
	}

	if err := os.MkdirAll(env.RuntimeDir, 0o700); err != nil {
		return false, err
	}

	dir, err := os.MkdirTemp(env.RuntimeDir, "mp-")
	if err != nil {
		return false, err
	}

	defer func() {
		if removeErr := os.Remove(dir); removeErr != nil {
			err = withCleanup(err, removeErr)
		}
	}()

	source := env.HostPath(parent.MountPoint)

	// No MS_REC: submounts, including the checked path's own mount, are not carried.
	if err := unix.Mount(source, dir, "", unix.MS_BIND, ""); err != nil {
		return false, fmt.Errorf("bind mount %s: %w", source, err)
	}

	defer func() {
		unmountErr := unix.Unmount(dir, 0)
		if errors.Is(unmountErr, unix.EBUSY) {
			unmountErr = unix.Unmount(dir, unix.MNT_DETACH)
		}

		if unmountErr != nil {
			err = withCleanup(err, fmt.Errorf("unmount %s: %w", dir, unmountErr))
		}
	}()

	if err := unix.Mount("", dir, "", unix.MS_PRIVATE, ""); err != nil {
		return false, fmt.Errorf("make %s private: %w", dir, err)
	}

	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return false, fmt.Errorf("statfs %s: %w", dir, err)
	}

	if err := unix.Mount("", dir, "", readOnlyRemountFlags(uint64(st.Flags)), ""); err != nil {
		return false, fmt.Errorf("remount %s read-only: %w", dir, err)
	}

	root, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", dir, err)
	}

	defer func() { _ = unix.Close(root) }()

	if inspectMountedHook != nil {
		inspectMountedHook(inspectNamespace, root)
	}

	return readImmutable(root, rel, filepath.Join(parent.MountPoint, rel))
}
