package checks

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestReadOnlyRemountFlags(t *testing.T) {
	const base = unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC

	tests := []struct {
		name   string
		statfs uint64
		want   uintptr
	}{
		{name: "strictatime", statfs: 0, want: base | unix.MS_STRICTATIME},
		{name: "relatime", statfs: unix.ST_RELATIME, want: base | unix.MS_RELATIME},
		{name: "noatime", statfs: unix.ST_NOATIME | unix.ST_NODIRATIME, want: base | unix.MS_NOATIME | unix.MS_NODIRATIME},
		{name: "nodiratime alone keeps strictatime", statfs: unix.ST_NODIRATIME, want: base | unix.MS_NODIRATIME | unix.MS_STRICTATIME},
		{
			name:   "locked nosuid, nodev and noexec are kept",
			statfs: unix.ST_NOSUID | unix.ST_NODEV | unix.ST_NOEXEC | unix.ST_RELATIME,
			want:   base | unix.MS_RELATIME,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readOnlyRemountFlags(tt.statfs); got != tt.want {
				t.Errorf("readOnlyRemountFlags(%#x) = %#x, want %#x", tt.statfs, got, tt.want)
			}
		})
	}
}
