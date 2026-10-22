package collect

import "golang.org/x/sys/unix"

// RealStatfs calls statfs(2) on path. Sizes use the fragment size, or the
// block size when the kernel reports no fragment size.
func RealStatfs(path string) (StatfsResult, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return StatfsResult{}, err
	}
	blockSize := uint64(st.Frsize)
	if blockSize == 0 {
		blockSize = uint64(st.Bsize)
	}
	return StatfsResult{
		Total:     st.Blocks * blockSize,
		Free:      st.Bfree * blockSize,
		Available: st.Bavail * blockSize,
	}, nil
}
