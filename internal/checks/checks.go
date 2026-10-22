// Package checks runs the platform-configured checks against the local host.
package checks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"time"

	"github.com/pentacore/lookout/internal/collect"
	"github.com/pentacore/lookout/internal/wire"
)

// Env is everything a check may touch outside its own params. Tests replace
// the fields to point at fixtures or fakes.
type Env struct {
	// Root is the filesystem root every host path is resolved against.
	Root string
	// RuntimeDir is the agent's runtime directory (default /run/lookout).
	RuntimeDir string
	// Mounts reads the current mount table.
	Mounts func() (collect.MountTable, error)
	// Now returns the current time.
	Now func() time.Time
	// Dial opens a network connection.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// HostPath resolves p underneath the configured root.
func (e Env) HostPath(p string) string {
	return filepath.Join(e.Root, p)
}

// DefaultEnv returns the production environment rooted at root.
func DefaultEnv(root, runtimeDir string) Env {
	dialer := &net.Dialer{}

	return Env{
		Root:       root,
		RuntimeDir: runtimeDir,
		Mounts: func() (collect.MountTable, error) {
			return collect.ReadMountTable(root)
		},
		Now:  time.Now,
		Dial: dialer.DialContext,
	}
}

// Outcome is a check's verdict before the runner attaches ID and type.
type Outcome struct {
	Status   wire.CheckStatus
	Message  string
	Observed map[string]any
}

// Func implements one check type.
type Func func(ctx context.Context, env Env, params json.RawMessage) Outcome

// Registry maps a check type to its implementation.
type Registry map[string]Func

// DefaultRegistry returns the check types this agent supports.
func DefaultRegistry() Registry {
	return Registry{
		"mount_present":  mountPresent,
		"path_on_mount":  pathOnMount,
		"file_freshness": fileFreshness,
	}
}

// decodeParams decodes check params into T. Empty or null params mean {} and
// unknown fields are ignored.
func decodeParams[T any](raw json.RawMessage) (T, error) {
	var out T

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		trimmed = []byte("{}")
	}

	if err := json.Unmarshal(trimmed, &out); err != nil {
		return out, err
	}

	return out, nil
}

// missingParam reports a required param that was not supplied.
func missingParam(name string) Outcome {
	return Outcome{
		Status:  wire.StatusError,
		Message: "missing required param: " + name,
	}
}

// invalidParams reports params that could not be decoded.
func invalidParams(err error) Outcome {
	return Outcome{
		Status:  wire.StatusError,
		Message: fmt.Sprintf("invalid params: %v", err),
	}
}
