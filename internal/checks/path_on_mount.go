package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/pentacore/lookout/internal/wire"
)

type pathOnMountParams struct {
	Path  string `json:"path"`
	Mount string `json:"mount"`
}

// pathOnMount verifies that the mount containing a path is the expected one,
// which catches a share that silently fell off and left writes on the root
// filesystem.
func pathOnMount(_ context.Context, env Env, raw json.RawMessage) Outcome {
	params, err := decodeParams[pathOnMountParams](raw)
	if err != nil {
		return invalidParams(err)
	}

	if params.Path == "" {
		return missingParam("path")
	}

	if params.Mount == "" {
		return missingParam("mount")
	}

	path := filepath.Clean(params.Path)
	wantMount := filepath.Clean(params.Mount)

	logical, outcome, ok := resolveLogicalPath(env, path)
	if !ok {
		return outcome
	}

	table, err := env.Mounts()
	if err != nil {
		return Outcome{Status: wire.StatusError, Message: fmt.Sprintf("cannot read mount table: %v", err)}
	}

	actual := table.Containing(logical)
	if actual.MountPoint == "" {
		return Outcome{Status: wire.StatusError, Message: "no mount contains " + path}
	}

	observed := map[string]any{"actual_mount": actual.MountPoint}

	if actual.MountPoint != wantMount {
		return Outcome{
			Status:   wire.StatusCrit,
			Message:  fmt.Sprintf("%s is on %s, expected %s", path, actual.MountPoint, wantMount),
			Observed: observed,
		}
	}

	return Outcome{
		Status:   wire.StatusOK,
		Message:  fmt.Sprintf("%s is on %s", path, actual.MountPoint),
		Observed: observed,
	}
}

// resolveLogicalPath follows symlinks for path under the env root and returns
// the result as a host path (root stripped). On failure it returns the Outcome
// to report and false.
func resolveLogicalPath(env Env, path string) (string, Outcome, bool) {
	resolved, err := filepath.EvalSymlinks(env.HostPath(path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", Outcome{Status: wire.StatusError, Message: path + " does not exist"}, false
		}

		return "", Outcome{Status: wire.StatusError, Message: fmt.Sprintf("cannot resolve %s: %v", path, err)}, false
	}

	// The root may itself sit behind a symlink (macOS temp dirs), in which case
	// the resolved path no longer starts with the configured root.
	root := env.Root
	if resolvedRoot, err := filepath.EvalSymlinks(root); err == nil {
		root = resolvedRoot
	}

	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", Outcome{Status: wire.StatusError, Message: path + " resolves outside the host root"}, false
	}

	return filepath.Join("/", relative), Outcome{}, true
}
