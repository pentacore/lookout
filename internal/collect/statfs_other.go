//go:build !linux

package collect

import "errors"

// RealStatfs is only implemented on Linux.
func RealStatfs(string) (StatfsResult, error) {
	return StatfsResult{}, errors.ErrUnsupported
}
