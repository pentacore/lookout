package selfupdate

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// Install atomically replaces exe with bin: it writes {exe}.new (0755) in the
// same directory, fsyncs it, renames it over exe and fsyncs the directory.
// Once a rename has happened that file counts as replaced: a failed directory
// fsync is logged to logger (slog.Default() when nil), not returned.
// When persistCopy is set (Unraid's copy on the flash drive, restored at
// boot) it is replaced the same way through {persistCopy}.tmp, first: a
// failure there leaves the running install untouched, and a failure on exe
// afterwards leaves a verified newer binary on the flash, which is harmless.
func Install(exe string, bin []byte, persistCopy string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	if persistCopy != "" {
		// The flash is vfat, whose fixed file modes may refuse a chmod.
		if err := replaceFile(persistCopy+".tmp", persistCopy, bin, false, logger); err != nil {
			return fmt.Errorf("write persistent copy %s: %w", persistCopy, err)
		}
	}
	if err := replaceFile(exe+".new", exe, bin, true, logger); err != nil {
		return fmt.Errorf("replace %s: %w", exe, installDirError(exe, err))
	}
	return nil
}

func replaceFile(tmp, dst string, data []byte, requireMode bool, logger *slog.Logger) error {
	if err := writeExecutable(tmp, data, requireMode); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := syncDirFunc(filepath.Dir(dst)); err != nil {
		logger.Warn("self-update: replaced the file but could not fsync its directory", "path", dst, "error", err)
	}
	return nil
}

// syncDirFunc is syncDir; tests replace it to simulate fsync failures.
var syncDirFunc = syncDir

// installDirError labels a write that failed because the directory holding
// exe is read-only or not writable by the agent, which an operator has to fix.
func installDirError(exe string, err error) error {
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS) {
		return fmt.Errorf("install directory %s is not writable: %w", filepath.Dir(exe), err)
	}
	return err
}

// writeExecutable writes data to path as a new 0755 file and fsyncs it. A
// leftover file or symlink at path is removed first, and O_EXCL refuses to
// follow one planted in between. When requireMode is false a failed chmod
// (to undo the umask) is ignored.
func writeExecutable(path string, data []byte, requireMode bool) (err error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(path)
		}
	}()

	if _, err = f.Write(data); err != nil {
		return err
	}
	if chmodErr := f.Chmod(0o755); chmodErr != nil && requireMode {
		return chmodErr
	}
	if err = f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

// syncDir makes a rename durable. File systems that cannot fsync a
// directory are tolerated: the rename itself already happened.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	_ = d.Close()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, errors.ErrUnsupported) {
		return nil
	}
	return err
}
