package checks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pentacore/lookout/internal/collect"
	"golang.org/x/sys/unix"
)

// fsImmutableFl is FS_IMMUTABLE_FL from linux/fs.h, which golang.org/x/sys
// does not export.
const fsImmutableFl = 0x00000010

// inspectUnderlying bind-mounts parent non-recursively into a private,
// read-only temp dir under env.RuntimeDir (tmpfs, never the Unraid flash
// state dir), so mounts on top of parent are not carried and rel shows the
// directory underneath them. It reads FS_IMMUTABLE_FL from that directory and
// always unmounts and removes the temp dir before returning; a failed cleanup
// is reported when the inspection itself succeeded.
func inspectUnderlying(ctx context.Context, env Env, parent collect.Mount, rel string) (immutable bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	if err := os.MkdirAll(env.RuntimeDir, 0o700); err != nil {
		return false, err
	}

	dir, err := os.MkdirTemp(env.RuntimeDir, "mp-")
	if err != nil {
		return false, err
	}

	defer func() {
		if removeErr := os.Remove(dir); removeErr != nil && err == nil {
			err = removeErr
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

		if unmountErr != nil && err == nil {
			err = fmt.Errorf("unmount %s: %w", dir, unmountErr)
		}
	}()

	// Stop propagation to peers of the runtime dir's mount.
	if err := unix.Mount("", dir, "", unix.MS_PRIVATE, ""); err != nil {
		return false, fmt.Errorf("make %s private: %w", dir, err)
	}

	if err := unix.Mount("", dir, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		return false, fmt.Errorf("remount %s read-only: %w", dir, err)
	}

	target := filepath.Join(dir, rel)

	fd, err := unix.Open(target, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", filepath.Join(parent.MountPoint, rel), err)
	}

	defer func() { _ = unix.Close(fd) }()

	flags, err := unix.IoctlGetUint32(fd, unix.FS_IOC_GETFLAGS)
	if err != nil {
		return false, fmt.Errorf("read attributes of %s: %w", filepath.Join(parent.MountPoint, rel), err)
	}

	return flags&fsImmutableFl != 0, nil
}
