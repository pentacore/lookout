//go:build !linux

package checks

import (
	"context"
	"errors"

	"github.com/pentacore/lookout/internal/collect"
)

// inspectUnderlying is only implemented on Linux.
func inspectUnderlying(context.Context, Env, collect.Mount, string) (bool, error) {
	return false, errors.ErrUnsupported
}
