package checks

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/pentacore/lookout/internal/wire"
)

type mountPresentParams struct {
	Path   string `json:"path"`
	FSType string `json:"fstype"`
	Source string `json:"source"`
}

// mountPresent verifies that a path is a mount point, optionally with a given
// filesystem type and source. Comparisons are exact.
func mountPresent(_ context.Context, env Env, raw json.RawMessage) Outcome {
	params, err := decodeParams[mountPresentParams](raw)
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

	mount, found := table.At(path)
	if !found {
		return Outcome{
			Status:   wire.StatusCrit,
			Message:  path + " is not mounted",
			Observed: map[string]any{"mounted": false, "fstype": nil, "source": nil},
		}
	}

	observed := map[string]any{"mounted": true, "fstype": mount.FSType, "source": mount.Source}

	switch {
	case params.FSType != "" && mount.FSType != params.FSType:
		return Outcome{
			Status:   wire.StatusCrit,
			Message:  fmt.Sprintf("%s is mounted as %s, expected %s", path, mount.FSType, params.FSType),
			Observed: observed,
		}
	case params.Source != "" && mount.Source != params.Source:
		return Outcome{
			Status:   wire.StatusCrit,
			Message:  fmt.Sprintf("%s is mounted from %s, expected %s", path, mount.Source, params.Source),
			Observed: observed,
		}
	}

	return Outcome{
		Status:   wire.StatusOK,
		Message:  fmt.Sprintf("%s is mounted (%s %s)", path, mount.FSType, mount.Source),
		Observed: observed,
	}
}
