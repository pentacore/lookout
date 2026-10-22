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

// inspectMountedHook, when set by a test, runs inside the private namespace
// once the read-only bind mount at dir is in place.
var inspectMountedHook func(dir string)

type inspectResult struct {
	immutable bool
	err       error
}

// inspectUnderlying reads FS_IMMUTABLE_FL from the directory underneath a
// mount point. The work happens on a dedicated OS thread in its own private
// mount namespace (see inspectInPrivateNamespace), so the temp bind mount is
// never visible to, or propagated into, any other namespace. ctx is checked
// only before starting: the call then blocks until the inspection finishes,
// even when it hangs on a hung parent filesystem, so the runner keeps the
// check in flight instead of starting another locked thread every cycle.
func inspectUnderlying(ctx context.Context, env Env, parent collect.Mount, rel string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	results := make(chan inspectResult, 1)

	go func() { results <- inspectOnPrivateThread(env, parent, rel) }()

	result := <-results

	return result.immutable, result.err
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
// afterwards. It unshares the mount namespace and makes every mount in it
// private, then bind-mounts parent non-recursively into a temp dir under
// env.RuntimeDir (tmpfs, never the Unraid flash state dir), so mounts on top
// of parent are not carried and rel shows the directory underneath them. The
// bind is remounted read-only before the directory is opened. Unmount and
// rmdir still run for tidiness; their failures are added to err as
// cleanupError.
func inspectInPrivateNamespace(env Env, parent collect.Mount, rel string) (immutable bool, err error) {
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		return false, fmt.Errorf("unshare mount namespace: %w", err)
	}

	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return false, fmt.Errorf("make mounts private: %w", err)
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

	if err := unix.Mount("", dir, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		return false, fmt.Errorf("remount %s read-only: %w", dir, err)
	}

	if inspectMountedHook != nil {
		inspectMountedHook(dir)
	}

	underlying := filepath.Join(parent.MountPoint, rel)

	fd, err := unix.Open(filepath.Join(dir, rel), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
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
