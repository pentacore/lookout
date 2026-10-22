package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pentacore/lookout/internal/collect"
	"github.com/pentacore/lookout/internal/wire"
	"golang.org/x/sys/unix"
)

func TestPlanUnderlying(t *testing.T) {
	table := collect.MountTable{
		{MountPoint: "/", FSType: "ext4"},
		{MountPoint: "/mnt/backups", FSType: "nfs4"},
		{MountPoint: "/mnt/disk1", FSType: "xfs"},
		{MountPoint: "/mnt/disk1/backups", FSType: "nfs4"},
	}

	tests := []struct {
		name       string
		path       string
		wantParent string
		wantRel    string
		wantErr    string
	}{
		{name: "parent is root", path: "/mnt/backups", wantParent: "/", wantRel: "mnt/backups"},
		{name: "nested mount", path: "/mnt/disk1/backups", wantParent: "/mnt/disk1", wantRel: "backups"},
		{name: "trailing slash", path: "/mnt/disk1/backups/", wantParent: "/mnt/disk1", wantRel: "backups"},
		{name: "not a mount point", path: "/mnt/nothing", wantErr: "/mnt/nothing is not a mount point"},
		{name: "root", path: "/", wantErr: "/ has no underlying directory"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent, rel, err := planUnderlying(table, tt.path)

			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if parent.MountPoint != tt.wantParent || rel != tt.wantRel {
				t.Errorf("got parent %q rel %q, want %q %q", parent.MountPoint, rel, tt.wantParent, tt.wantRel)
			}
		})
	}
}

func TestPlanUnderlyingWithoutContainingMount(t *testing.T) {
	table := collect.MountTable{{MountPoint: "/mnt/backups", FSType: "nfs4"}}

	_, _, err := planUnderlying(table, "/mnt/backups")
	if err == nil || err.Error() != "no mount contains /mnt" {
		t.Fatalf("err = %v, want %q", err, "no mount contains /mnt")
	}
}

// fakeInspect returns an inspectFunc that answers immutable/err and counts
// its calls. It records the parent and rel it was asked about.
type fakeInspect struct {
	immutable bool
	err       error
	calls     int
	parent    string
	rel       string
}

func (f *fakeInspect) inspect(_ context.Context, _ Env, parent collect.Mount, rel string) (bool, error) {
	f.calls++
	f.parent, f.rel = parent.MountPoint, rel

	return f.immutable, f.err
}

func TestMountpointProtectedStatuses(t *testing.T) {
	tests := []struct {
		checkCase
		immutable bool
		err       error
	}{
		{
			checkCase: checkCase{
				name:         "immutable",
				params:       `{"path":"/mnt/backups"}`,
				wantStatus:   wire.StatusOK,
				wantMessage:  "the directory under /mnt/backups is immutable",
				wantObserved: map[string]any{"immutable": true},
			},
			immutable: true,
		},
		{
			checkCase: checkCase{
				name:       "not immutable",
				params:     `{"path":"/mnt/backups/"}`,
				wantStatus: wire.StatusWarn,
				wantMessage: "the directory under /mnt/backups is not immutable; writes would land on / " +
					"if the mount drops (fix: chattr +i on it while unmounted)",
				wantObserved: map[string]any{"immutable": false},
			},
		},
		{
			checkCase: checkCase{
				name:         "ENOTTY",
				params:       `{"path":"/mnt/backups"}`,
				wantStatus:   wire.StatusError,
				wantMessage:  "filesystem at / (ext4) does not support file attributes",
				wantObserved: map[string]any{"immutable": nil},
			},
			err: unix.ENOTTY,
		},
		{
			checkCase: checkCase{
				name:         "EOPNOTSUPP wrapped",
				params:       `{"path":"/mnt/backups"}`,
				wantStatus:   wire.StatusError,
				wantMessage:  "filesystem at / (ext4) does not support file attributes",
				wantObserved: map[string]any{"immutable": nil},
			},
			err: fmt.Errorf("ioctl: %w", unix.EOPNOTSUPP),
		},
		{
			checkCase: checkCase{
				name:         "other inspect error",
				params:       `{"path":"/mnt/backups"}`,
				wantStatus:   wire.StatusError,
				wantMessage:  "cannot inspect the directory under /mnt/backups: bind mount /: operation not permitted",
				wantObserved: map[string]any{"immutable": nil},
			},
			err: errors.New("bind mount /: operation not permitted"),
		},
		{
			checkCase: checkCase{
				name:       "cleanup failure after unsupported",
				params:     `{"path":"/mnt/backups"}`,
				wantStatus: wire.StatusError,
				wantMessage: "cannot inspect the directory under /mnt/backups: read attributes of /mnt/backups: " +
					"inappropriate ioctl for device; clean up: unmount /run/lookout/mp-1: device or resource busy",
				wantObserved: map[string]any{"immutable": nil},
			},
			err: fmt.Errorf("%w; %w",
				fmt.Errorf("read attributes of /mnt/backups: %w", unix.ENOTTY),
				cleanupError{err: errors.New("unmount /run/lookout/mp-1: device or resource busy")},
			),
		},
		{
			checkCase: checkCase{
				name:         "not a mount point",
				params:       `{"path":"/mnt/other"}`,
				wantStatus:   wire.StatusError,
				wantMessage:  "/mnt/other is not a mount point",
				wantObserved: map[string]any{"immutable": nil},
			},
		},
		{
			checkCase: checkCase{
				name:         "root",
				params:       `{"path":"/"}`,
				wantStatus:   wire.StatusError,
				wantMessage:  "/ has no underlying directory",
				wantObserved: map[string]any{"immutable": nil},
			},
		},
		{
			checkCase: checkCase{
				name:         "missing path",
				params:       `{}`,
				wantStatus:   wire.StatusError,
				wantMessage:  "missing required param: path",
				wantObserved: map[string]any{"immutable": nil},
			},
		},
		{
			checkCase: checkCase{
				name:         "invalid params",
				params:       `{"path":5}`,
				wantStatus:   wire.StatusError,
				wantContains: "invalid params: ",
				wantObserved: map[string]any{"immutable": nil},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t, testMountInfo)
			fake := &fakeInspect{immutable: tt.immutable, err: tt.err}
			check := newMountpointProtected(fake.inspect)

			tt.assert(t, runCheck(check.Run, env, tt.params))
		})
	}
}

func TestMountpointProtectedPassesPlanToInspect(t *testing.T) {
	env := newTestEnv(t, testMountInfo)
	fake := &fakeInspect{immutable: true}
	check := newMountpointProtected(fake.inspect)

	runCheck(check.Run, env, `{"path":"/mnt/appdata cache"}`)

	if fake.parent != "/" || fake.rel != "mnt/appdata cache" {
		t.Errorf("inspect got parent %q rel %q, want %q %q", fake.parent, fake.rel, "/", "mnt/appdata cache")
	}
}

func TestMountpointProtectedUnreadableMountTable(t *testing.T) {
	env := newTestEnv(t, testMountInfo)
	env.Mounts = func() (collect.MountTable, error) { return nil, errors.New("boom") }
	check := newMountpointProtected((&fakeInspect{}).inspect)

	checkCase{
		wantStatus:   wire.StatusError,
		wantMessage:  "cannot read mount table: boom",
		wantObserved: map[string]any{"immutable": nil},
	}.assert(t, runCheck(check.Run, env, `{"path":"/mnt/backups"}`))
}

func TestMountpointProtectedCache(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		secondAt  time.Duration
		wantCalls int
	}{
		{name: "cached within ttl", secondAt: 0, wantCalls: 1},
		{name: "cached just before expiry", secondAt: 15*time.Minute - time.Second, wantCalls: 1},
		{name: "expired after ttl", secondAt: 16 * time.Minute, wantCalls: 2},
		{name: "unsupported is cached", err: unix.ENOTTY, secondAt: 0, wantCalls: 1},
		{name: "unsupported expires", err: fmt.Errorf("ioctl: %w", unix.EOPNOTSUPP), secondAt: 16 * time.Minute, wantCalls: 2},
		{name: "other errors are not cached", err: errors.New("bind mount /: operation not permitted"), wantCalls: 2},
		{
			name:      "cleanup failures are not cached",
			err:       fmt.Errorf("%w; %w", unix.ENOTTY, cleanupError{err: errors.New("unmount: device or resource busy")}),
			wantCalls: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t, testMountInfo)
			fake := &fakeInspect{immutable: true, err: tt.err}
			check := newMountpointProtected(fake.inspect)

			first := runCheck(check.Run, env, `{"path":"/mnt/backups"}`)

			env.Now = func() time.Time { return testNow.Add(tt.secondAt) }
			second := runCheck(check.Run, env, `{"path":"/mnt/backups/"}`)

			if fake.calls != tt.wantCalls {
				t.Errorf("inspect called %d times, want %d", fake.calls, tt.wantCalls)
			}

			if first.Status != second.Status || first.Message != second.Message {
				t.Errorf("second run = %q %q, want it to match the first %q %q",
					second.Status, second.Message, first.Status, first.Message)
			}
		})
	}
}

func TestMountpointProtectedCacheIsPerPath(t *testing.T) {
	env := newTestEnv(t, testMountInfo)
	fake := &fakeInspect{immutable: true}
	check := newMountpointProtected(fake.inspect)

	runCheck(check.Run, env, `{"path":"/mnt/backups"}`)
	runCheck(check.Run, env, `{"path":"/mnt/appdata cache"}`)

	if fake.calls != 2 {
		t.Errorf("inspect called %d times, want 2", fake.calls)
	}
}

func TestDefaultRegistryHasMountpointProtected(t *testing.T) {
	if DefaultRegistry()["mountpoint_protected"] == nil {
		t.Error(`DefaultRegistry has no "mountpoint_protected"`)
	}
}

// TestMountpointProtectedHungInspectionStaysInFlight runs the check under a
// Runner whose budget expires while inspect hangs: the next run must report
// the check as still in progress rather than start a second inspection.
func TestMountpointProtectedHungInspectionStaysInFlight(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 10)
	hung := func(context.Context, Env, collect.Mount, string) (bool, error) {
		started <- struct{}{}
		<-release

		return true, nil
	}

	runner := &Runner{
		Registry: Registry{"mountpoint_protected": newMountpointProtected(hung).Run},
		Env:      newTestEnv(t, testMountInfo),
		Budget:   20 * time.Millisecond,
	}
	cfgs := []wire.CheckConfig{{ID: "mp", Type: "mountpoint_protected", Params: json.RawMessage(`{"path":"/mnt/backups"}`)}}

	first := runner.Run(context.Background(), cfgs)

	time.Sleep(50 * time.Millisecond)

	second := runner.Run(context.Background(), cfgs)

	close(release)

	if !strings.HasPrefix(first[0].Message, "timed out") {
		t.Errorf("first run = %q, want a timeout", first[0].Message)
	}

	if !strings.Contains(second[0].Message, "previous run still in progress") {
		t.Errorf("second run = %q, want the previous run still in progress", second[0].Message)
	}

	if got := len(started); got != 1 {
		t.Errorf("inspections started = %d, want 1", got)
	}
}
