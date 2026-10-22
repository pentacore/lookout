package collect

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/pentacore/lookout/internal/wire"
)

// fakeStatfs answers every path with sizes, except where errs or blocks say
// otherwise, and counts the calls per path.
type fakeStatfs struct {
	sizes  StatfsResult
	errs   map[string]error
	blocks map[string]chan struct{}

	mu    sync.Mutex
	calls map[string]int
}

func (f *fakeStatfs) statfs(path string) (StatfsResult, error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[path]++
	f.mu.Unlock()

	if ch, ok := f.blocks[path]; ok {
		<-ch
	}
	if err, ok := f.errs[path]; ok {
		return StatfsResult{}, err
	}
	return f.sizes, nil
}

func (f *fakeStatfs) callCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[path]
}

func newTestFilesystemCollector(statfs StatfsFunc, timeout time.Duration) (*FilesystemCollector, *bytes.Buffer) {
	var logs bytes.Buffer
	return &FilesystemCollector{
		Root:    "/",
		Statfs:  statfs,
		Timeout: timeout,
		Logger:  slog.New(slog.NewTextHandler(&logs, nil)),
	}, &logs
}

func findFilesystem(t *testing.T, filesystems []wire.Filesystem, mount string) wire.Filesystem {
	t.Helper()
	for _, fs := range filesystems {
		if fs.Mount == mount {
			return fs
		}
	}
	t.Fatalf("no filesystem %q in %+v", mount, filesystems)
	return wire.Filesystem{}
}

func assertUnresponsiveBackups(t *testing.T, filesystems []wire.Filesystem) {
	t.Helper()
	want := wire.Filesystem{Mount: "/mnt/backups", Device: "nas:/backups", FSType: "nfs4", Unresponsive: true}
	got := findFilesystem(t, filesystems, "/mnt/backups")
	if got != want {
		t.Errorf("/mnt/backups = %+v, want %+v", got, want)
	}
}

func assertSizes(t *testing.T, fs wire.Filesystem, total, used, available uint64) {
	t.Helper()
	if fs.Unresponsive {
		t.Errorf("%s: Unresponsive = true, want false", fs.Mount)
	}
	if fs.TotalBytes == nil || fs.UsedBytes == nil || fs.AvailableBytes == nil {
		t.Fatalf("%s: byte fields = %v %v %v, want all set", fs.Mount, fs.TotalBytes, fs.UsedBytes, fs.AvailableBytes)
	}
	if *fs.TotalBytes != total || *fs.UsedBytes != used || *fs.AvailableBytes != available {
		t.Errorf("%s: sizes = %d/%d/%d, want %d/%d/%d", fs.Mount, *fs.TotalBytes, *fs.UsedBytes, *fs.AvailableBytes, total, used, available)
	}
}

func TestFilesystemCollectorSizes(t *testing.T) {
	fake := &fakeStatfs{sizes: StatfsResult{Total: 1000, Free: 400, Available: 300}}
	collector, _ := newTestFilesystemCollector(fake.statfs, time.Second)

	got := collector.Collect(readUnraidMounts(t))
	if len(got) != 7 {
		t.Fatalf("len = %d, want 7", len(got))
	}
	for _, fs := range got {
		assertSizes(t, fs, 1000, 600, 300)
	}

	disk := findFilesystem(t, got, "/mnt/disk1")
	if disk.Device != "/dev/md1p1" || disk.FSType != "xfs" {
		t.Errorf("/mnt/disk1 device/fstype = %q/%q, want /dev/md1p1/xfs", disk.Device, disk.FSType)
	}
	root := findFilesystem(t, got, "/")
	if root.Device != "rootfs" || root.FSType != "rootfs" {
		t.Errorf("/ device/fstype = %q/%q, want rootfs/rootfs", root.Device, root.FSType)
	}
}

func TestFilesystemCollectorStatfsUsesRoot(t *testing.T) {
	fake := &fakeStatfs{sizes: StatfsResult{Total: 1, Free: 1, Available: 1}}
	collector, _ := newTestFilesystemCollector(fake.statfs, time.Second)
	collector.Root = "/host"

	collector.Collect(readUnraidMounts(t))
	if got := fake.callCount("/host/mnt/appdata cache"); got != 1 {
		t.Errorf("calls for /host/mnt/appdata cache = %d, want 1", got)
	}
}

func TestFilesystemCollectorEmptyTable(t *testing.T) {
	collector, _ := newTestFilesystemCollector((&fakeStatfs{}).statfs, time.Second)

	got := collector.Collect(nil)
	if got == nil {
		t.Fatal("Collect(nil) = nil, want empty non-nil slice")
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "[]" {
		t.Errorf("json = %s, want []", data)
	}
}

func TestFilesystemCollectorStatfsError(t *testing.T) {
	fake := &fakeStatfs{
		sizes: StatfsResult{Total: 1000, Free: 400, Available: 300},
		errs:  map[string]error{"/mnt/backups": unix.ESTALE},
	}
	collector, logs := newTestFilesystemCollector(fake.statfs, time.Second)

	got := collector.Collect(readUnraidMounts(t))
	if len(got) != 7 {
		t.Fatalf("len = %d, want 7", len(got))
	}
	assertUnresponsiveBackups(t, got)
	for _, fs := range got {
		if fs.Mount != "/mnt/backups" {
			assertSizes(t, fs, 1000, 600, 300)
		}
	}
	if !strings.Contains(logs.String(), "stale") {
		t.Errorf("logs = %q, want the errno text containing %q", logs.String(), "stale")
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("logs = %q, want a warn entry", logs.String())
	}
}

func TestFilesystemCollectorHungStatfs(t *testing.T) {
	release := make(chan struct{})
	fake := &fakeStatfs{
		sizes:  StatfsResult{Total: 1000, Free: 400, Available: 300},
		blocks: map[string]chan struct{}{"/mnt/backups": release},
	}
	collector, logs := newTestFilesystemCollector(fake.statfs, 50*time.Millisecond)
	table := readUnraidMounts(t)

	start := time.Now()
	first := collector.Collect(table)
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("Collect #1 took %v, want < 500ms", elapsed)
	}
	assertUnresponsiveBackups(t, first)
	data, err := json.Marshal(findFilesystem(t, first, "/mnt/backups"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "total_bytes") {
		t.Errorf("json = %s, want no total_bytes key", data)
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("logs = %q, want a warn entry for the timeout", logs.String())
	}
	assertSizes(t, findFilesystem(t, first, "/mnt/disk1"), 1000, 600, 300)

	second := collector.Collect(table)
	assertUnresponsiveBackups(t, second)
	if got := fake.callCount("/mnt/backups"); got != 1 {
		t.Errorf("statfs calls for /mnt/backups = %d, want 1 while in flight", got)
	}

	close(release)
	waitNotInFlight(t, collector, "/mnt/backups")

	third := collector.Collect(table)
	assertSizes(t, findFilesystem(t, third, "/mnt/backups"), 1000, 600, 300)
}

// waitNotInFlight waits until the collector's background statfs for mount has
// returned.
func waitNotInFlight(t *testing.T, c *FilesystemCollector, mount string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for c.isInFlight(mount) {
		if time.Now().After(deadline) {
			t.Fatalf("%s still in flight after 2s", mount)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRealStatfs(t *testing.T) {
	got, err := RealStatfs(t.TempDir())
	if runtime.GOOS != "linux" {
		if !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("err = %v, want errors.ErrUnsupported", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if got.Total == 0 || got.Free > got.Total || got.Available > got.Free {
		t.Errorf("RealStatfs = %+v, want Total > 0 and Available <= Free <= Total", got)
	}
}

func TestRealStatfsMissingPath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("RealStatfs is Linux-only")
	}
	if _, err := RealStatfs("/does/not/exist"); err == nil {
		t.Error("err = nil, want an error for a missing path")
	}
}
