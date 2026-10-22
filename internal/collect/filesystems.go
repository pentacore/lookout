package collect

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pentacore/lookout/internal/wire"
)

// pseudoFSTypes are the contract's virtual filesystem types, never reported
// (except when mounted at /).
var pseudoFSTypes = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "tmpfs": true,
	"cgroup": true, "cgroup2": true, "overlay": true, "squashfs": true, "nsfs": true,
	"tracefs": true, "debugfs": true, "securityfs": true, "pstore": true, "bpf": true,
	"autofs": true, "mqueue": true, "hugetlbfs": true, "fusectl": true, "configfs": true,
	"binfmt_misc": true, "efivarfs": true, "ramfs": true,
}

// excludedMountPrefixes hide container internals. /var/lib/docker itself is
// still reported.
var excludedMountPrefixes = []string{"/var/lib/docker/", "/run/"}

// defaultStatfsTimeout bounds one statfs call when FilesystemCollector.Timeout
// is zero.
const defaultStatfsTimeout = 2 * time.Second

// SelectFilesystems picks the mounts to report: / first (its topmost entry,
// whatever its type), then the rest in table order. Only the topmost entry at
// each mount point counts, pseudo types and container paths are excluded, and
// bind mounts of the same device and root are reported once (first seen wins,
// / seeded).
func SelectFilesystems(t MountTable) []Mount {
	topmost := topmostPerMountPoint(t)

	selected := []Mount{}
	seen := map[string]bool{}
	if root, ok := t.At("/"); ok {
		selected = append(selected, root)
		seen[deviceRootKey(root)] = true
	}
	for _, m := range topmost {
		if m.MountPoint == "/" || isExcludedMount(m) {
			continue
		}
		key := deviceRootKey(m)
		if seen[key] {
			continue
		}
		seen[key] = true
		selected = append(selected, m)
	}
	return selected
}

// topmostPerMountPoint keeps, in table order, only the last entry at each
// mount point: statfs sees only that one.
func topmostPerMountPoint(t MountTable) []Mount {
	last := map[string]int{}
	for i, m := range t {
		last[m.MountPoint] = i
	}
	topmost := make([]Mount, 0, len(last))
	for i, m := range t {
		if last[m.MountPoint] == i {
			topmost = append(topmost, m)
		}
	}
	return topmost
}

func isExcludedMount(m Mount) bool {
	if pseudoFSTypes[m.FSType] {
		return true
	}
	for _, prefix := range excludedMountPrefixes {
		if strings.HasPrefix(m.MountPoint, prefix) {
			return true
		}
	}
	return false
}

func deviceRootKey(m Mount) string {
	return fmt.Sprintf("%d:%d+%s", m.Major, m.Minor, m.Root)
}

// StatfsResult is a filesystem's size in bytes.
type StatfsResult struct {
	Total, Free, Available uint64
}

// StatfsFunc reports the size of the filesystem at path.
type StatfsFunc func(path string) (StatfsResult, error)

// FilesystemCollector reports usage for the selected filesystems without ever
// blocking a report on a hung mount. Zero values default to RealStatfs, a 2 s
// timeout and slog.Default().
type FilesystemCollector struct {
	Root    string
	Statfs  StatfsFunc
	Timeout time.Duration
	Logger  *slog.Logger

	mu       sync.Mutex
	inFlight map[string]bool
}

type statfsOutcome struct {
	result StatfsResult
	err    error
}

// Collect selects the reportable filesystems from t and runs statfs on each
// concurrently under {Root}/{MountPoint}. A mount whose statfs times out,
// fails, or is still stuck from an earlier call is reported unresponsive with
// no byte fields; no entry is ever dropped. The result is never nil.
func (c *FilesystemCollector) Collect(t MountTable) []wire.Filesystem {
	mounts := SelectFilesystems(t)
	outcomes := make([]chan statfsOutcome, len(mounts))
	for i, m := range mounts {
		outcomes[i] = c.start(m.MountPoint)
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout())
	defer cancel()

	filesystems := make([]wire.Filesystem, 0, len(mounts))
	for i, m := range mounts {
		entry := wire.Filesystem{Mount: m.MountPoint, Device: m.Source, FSType: m.FSType, Unresponsive: true}
		if outcomes[i] == nil {
			c.logger().Debug("statfs still in flight", "mount", m.MountPoint)
			filesystems = append(filesystems, entry)
			continue
		}
		outcome, ok := awaitStatfs(ctx, outcomes[i])
		switch {
		case !ok:
			c.logger().Warn("statfs timed out", "mount", m.MountPoint, "timeout", c.timeout())
		case outcome.err != nil:
			c.logger().Warn("statfs failed", "mount", m.MountPoint, "err", outcome.err)
		default:
			result := outcome.result
			entry.TotalBytes = wire.Bytes(result.Total)
			entry.UsedBytes = wire.Bytes(result.Total - min(result.Free, result.Total))
			entry.AvailableBytes = wire.Bytes(result.Available)
			entry.Unresponsive = false
		}
		filesystems = append(filesystems, entry)
	}
	return filesystems
}

// awaitStatfs waits for outcome until ctx is done. A result that is already
// there when ctx is done still counts.
func awaitStatfs(ctx context.Context, outcome <-chan statfsOutcome) (statfsOutcome, bool) {
	select {
	case o := <-outcome:
		return o, true
	case <-ctx.Done():
	}
	select {
	case o := <-outcome:
		return o, true
	default:
		return statfsOutcome{}, false
	}
}

// start launches statfs for mountPoint in the background and marks it in
// flight until it returns. It returns nil, without calling statfs, when an
// earlier call for mountPoint is still in flight.
func (c *FilesystemCollector) start(mountPoint string) chan statfsOutcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inFlight[mountPoint] {
		return nil
	}
	if c.inFlight == nil {
		c.inFlight = map[string]bool{}
	}
	c.inFlight[mountPoint] = true

	statfs := c.Statfs
	if statfs == nil {
		statfs = RealStatfs
	}
	path := filepath.Join(c.Root, mountPoint)
	outcome := make(chan statfsOutcome, 1)
	go func() {
		result, err := statfs(path)
		c.mu.Lock()
		delete(c.inFlight, mountPoint)
		c.mu.Unlock()
		outcome <- statfsOutcome{result: result, err: err}
	}()
	return outcome
}

func (c *FilesystemCollector) isInFlight(mountPoint string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inFlight[mountPoint]
}

func (c *FilesystemCollector) timeout() time.Duration {
	if c.Timeout <= 0 {
		return defaultStatfsTimeout
	}
	return c.Timeout
}

func (c *FilesystemCollector) logger() *slog.Logger {
	if c.Logger == nil {
		return slog.Default()
	}
	return c.Logger
}
