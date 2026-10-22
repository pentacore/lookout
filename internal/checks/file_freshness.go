package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/pentacore/lookout/internal/wire"
)

type fileFreshnessParams struct {
	Path      string   `json:"path"`
	Pattern   string   `json:"pattern"`
	WarnHours *float64 `json:"warn_hours"`
	CritHours *float64 `json:"crit_hours"`
}

// fileFreshness checks how old the newest matching file is. A file path uses
// its own mtime; a directory is scanned non-recursively.
func fileFreshness(_ context.Context, env Env, raw json.RawMessage) Outcome {
	params, err := decodeParams[fileFreshnessParams](raw)
	if err != nil {
		return invalidParams(err)
	}

	switch {
	case params.Path == "":
		return missingParam("path")
	case params.WarnHours == nil:
		return missingParam("warn_hours")
	case params.CritHours == nil:
		return missingParam("crit_hours")
	}

	pattern := params.Pattern
	if pattern == "" {
		pattern = "*"
	}

	path := filepath.Clean(params.Path)
	hostPath := env.HostPath(path)

	info, err := os.Stat(hostPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Outcome{Status: wire.StatusCrit, Message: path + " does not exist"}
		}

		return Outcome{Status: wire.StatusError, Message: fmt.Sprintf("cannot stat %s: %v", path, err)}
	}

	newestFile, newestTime := path, info.ModTime()

	if info.IsDir() {
		var found bool

		newestFile, newestTime, found, err = newestMatch(hostPath, path, pattern)
		if err != nil {
			return Outcome{Status: wire.StatusError, Message: err.Error()}
		}

		if !found {
			return Outcome{
				Status:   wire.StatusCrit,
				Message:  fmt.Sprintf("no files matching %s in %s", pattern, path),
				Observed: map[string]any{"newest_file": nil, "age_hours": nil},
			}
		}
	}

	age := ageHours(env.Now(), newestTime)
	observed := map[string]any{"newest_file": newestFile, "age_hours": age}
	described := fmt.Sprintf("newest file %s is %s hours old", newestFile, strconv.FormatFloat(age, 'f', -1, 64))

	switch {
	case age > *params.CritHours:
		return Outcome{
			Status:   wire.StatusCrit,
			Message:  fmt.Sprintf("%s, crit after %s", described, strconv.FormatFloat(*params.CritHours, 'f', -1, 64)),
			Observed: observed,
		}
	case age > *params.WarnHours:
		return Outcome{
			Status:   wire.StatusWarn,
			Message:  fmt.Sprintf("%s, warn after %s", described, strconv.FormatFloat(*params.WarnHours, 'f', -1, 64)),
			Observed: observed,
		}
	}

	return Outcome{Status: wire.StatusOK, Message: described, Observed: observed}
}

// newestMatch returns the most recently modified regular file directly inside
// hostDir whose name matches pattern. Symlinks are followed and broken links
// (whose target does not exist) are skipped; any other stat failure is an
// error, as skipping the entry could report an older file as the newest. The
// returned name is under logicalDir. Ties go to the first name in directory
// order.
func newestMatch(hostDir, logicalDir, pattern string) (string, time.Time, bool, error) {
	if _, err := filepath.Match(pattern, ""); err != nil {
		return "", time.Time{}, false, fmt.Errorf("invalid pattern %q: %w", pattern, err)
	}

	entries, err := os.ReadDir(hostDir)
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("cannot read %s: %w", logicalDir, err)
	}

	var (
		newestName string
		newestTime time.Time
		found      bool
	)

	for _, entry := range entries {
		matched, err := filepath.Match(pattern, entry.Name())
		if err != nil || !matched {
			continue
		}

		info, err := os.Stat(filepath.Join(hostDir, entry.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", time.Time{}, false, fmt.Errorf("cannot stat %s: %w", filepath.Join(logicalDir, entry.Name()), err)
		}
		if !info.Mode().IsRegular() {
			continue
		}

		if !found || info.ModTime().After(newestTime) {
			newestName, newestTime, found = entry.Name(), info.ModTime(), true
		}
	}

	return filepath.Join(logicalDir, newestName), newestTime, found, nil
}

// ageHours is the time since mtime in hours, never negative, rounded to two
// decimals.
func ageHours(now, mtime time.Time) float64 {
	hours := math.Max(now.Sub(mtime).Hours(), 0)

	return math.Round(hours*100) / 100
}
