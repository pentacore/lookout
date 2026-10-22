package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/pentacore/lookout/internal/collect"
	"github.com/pentacore/lookout/internal/wire"
	"golang.org/x/sys/unix"
)

// mountpointProtectedTTL bounds how often one path is bind-mounted and
// inspected: the underlying directory only changes while the mount is down.
const mountpointProtectedTTL = 15 * time.Minute

// sharedMountpointProtected is the instance DefaultRegistry hands out, so the
// cache outlives any one registry.
var sharedMountpointProtected = newMountpointProtected(inspectUnderlying)

type mountpointProtectedParams struct {
	Path string `json:"path"`
}

// inspectFunc reports whether the directory rel inside parent, as seen with
// every mount on top of parent removed, carries the immutable attribute.
type inspectFunc func(ctx context.Context, env Env, parent collect.Mount, rel string) (immutable bool, err error)

// cached is one successful inspection.
type cached struct {
	immutable bool
	at        time.Time
}

// mountpointProtected is the mountpoint_protected prevention lint: the
// directory underneath a mount point should be immutable, so writes fail
// instead of silently filling the parent filesystem when the mount drops.
type mountpointProtected struct {
	inspect inspectFunc
	ttl     time.Duration
	cache   map[string]cached
	mu      sync.Mutex
}

func newMountpointProtected(inspect inspectFunc) *mountpointProtected {
	return &mountpointProtected{
		inspect: inspect,
		ttl:     mountpointProtectedTTL,
		cache:   map[string]cached{},
	}
}

// Run implements Func. Every outcome carries observed.immutable, nil when
// the attribute could not be determined.
func (m *mountpointProtected) Run(ctx context.Context, env Env, raw json.RawMessage) Outcome {
	out := m.run(ctx, env, raw)
	if out.Observed == nil {
		out.Observed = map[string]any{"immutable": nil}
	}

	return out
}

func (m *mountpointProtected) run(ctx context.Context, env Env, raw json.RawMessage) Outcome {
	params, err := decodeParams[mountpointProtectedParams](raw)
	if err != nil {
		return invalidParams(err)
	}

	if params.Path == "" {
		return missingParam("path")
	}

	table, err := env.Mounts()
	if err != nil {
		return Outcome{Status: wire.StatusError, Message: fmt.Sprintf("cannot read mount table: %v", err)}
	}

	path := filepath.Clean(params.Path)

	parent, rel, err := planUnderlying(table, path)
	if err != nil {
		return Outcome{Status: wire.StatusError, Message: err.Error()}
	}

	immutable, err := m.immutable(ctx, env, path, parent, rel)

	switch {
	case errors.Is(err, unix.ENOTTY), errors.Is(err, unix.EOPNOTSUPP):
		return Outcome{
			Status:  wire.StatusError,
			Message: fmt.Sprintf("filesystem at %s (%s) does not support file attributes", parent.MountPoint, parent.FSType),
		}
	case err != nil:
		return Outcome{
			Status:  wire.StatusError,
			Message: fmt.Sprintf("cannot inspect the directory under %s: %v", path, err),
		}
	case immutable:
		return Outcome{
			Status:   wire.StatusOK,
			Message:  fmt.Sprintf("the directory under %s is immutable", path),
			Observed: map[string]any{"immutable": true},
		}
	default:
		return Outcome{
			Status: wire.StatusWarn,
			Message: fmt.Sprintf("the directory under %s is not immutable; writes would land on %s "+
				"if the mount drops (fix: chattr +i on it while unmounted)", path, parent.MountPoint),
			Observed: map[string]any{"immutable": false},
		}
	}
}

// immutable answers from the cache while it is fresh and otherwise inspects,
// caching only successful inspections. The lock is not held across the
// inspection, so one hung bind mount cannot block checks for other paths.
func (m *mountpointProtected) immutable(ctx context.Context, env Env, path string, parent collect.Mount, rel string) (bool, error) {
	now := env.Now()

	m.mu.Lock()
	entry, ok := m.cache[path]
	m.mu.Unlock()

	if ok && now.Sub(entry.at) < m.ttl {
		return entry.immutable, nil
	}

	immutable, err := m.inspect(ctx, env, parent, rel)
	if err != nil {
		return false, err
	}

	m.mu.Lock()
	m.cache[path] = cached{immutable: immutable, at: now}
	m.mu.Unlock()

	return immutable, nil
}

// planUnderlying finds the mount that path is mounted on top of and path's
// location relative to that mount's root.
func planUnderlying(t collect.MountTable, path string) (parent collect.Mount, rel string, err error) {
	path = filepath.Clean(path)

	if _, found := t.At(path); !found {
		return collect.Mount{}, "", fmt.Errorf("%s is not a mount point", path)
	}

	if path == "/" {
		return collect.Mount{}, "", errors.New("/ has no underlying directory")
	}

	dir := filepath.Dir(path)

	parent = t.Containing(dir)
	if parent.MountPoint == "" {
		return collect.Mount{}, "", fmt.Errorf("no mount contains %s", dir)
	}

	rel, err = filepath.Rel(parent.MountPoint, path)
	if err != nil {
		return collect.Mount{}, "", err
	}

	return parent, rel, nil
}
