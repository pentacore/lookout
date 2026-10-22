package checks

import (
	"testing"

	"github.com/pentacore/lookout/internal/wire"
)

func TestMountPresent(t *testing.T) {
	tests := []checkCase{
		{
			name:         "not mounted",
			params:       `{"path":"/mnt/other"}`,
			wantStatus:   wire.StatusCrit,
			wantMessage:  "/mnt/other is not mounted",
			wantObserved: map[string]any{"mounted": false, "fstype": nil, "source": nil},
		},
		{
			name:         "ok without expectations",
			params:       `{"path":"/mnt/backups"}`,
			wantStatus:   wire.StatusOK,
			wantMessage:  "/mnt/backups is mounted (nfs4 nas:/backups)",
			wantObserved: map[string]any{"mounted": true, "fstype": "nfs4", "source": "nas:/backups"},
		},
		{
			name:         "ok with expectations",
			params:       `{"path":"/mnt/backups","fstype":"nfs4","source":"nas:/backups"}`,
			wantStatus:   wire.StatusOK,
			wantMessage:  "/mnt/backups is mounted (nfs4 nas:/backups)",
			wantObserved: map[string]any{"mounted": true, "fstype": "nfs4", "source": "nas:/backups"},
		},
		{
			name:         "fstype mismatch",
			params:       `{"path":"/mnt/backups","fstype":"nfs"}`,
			wantStatus:   wire.StatusCrit,
			wantMessage:  "/mnt/backups is mounted as nfs4, expected nfs",
			wantObserved: map[string]any{"mounted": true, "fstype": "nfs4", "source": "nas:/backups"},
		},
		{
			name:         "source mismatch",
			params:       `{"path":"/mnt/backups","source":"nas:/old"}`,
			wantStatus:   wire.StatusCrit,
			wantMessage:  "/mnt/backups is mounted from nas:/backups, expected nas:/old",
			wantObserved: map[string]any{"mounted": true, "fstype": "nfs4", "source": "nas:/backups"},
		},
		{
			name:       "trailing slash",
			params:     `{"path":"/mnt/backups/"}`,
			wantStatus: wire.StatusOK,
		},
		{
			name:         "space in path",
			params:       `{"path":"/mnt/appdata cache"}`,
			wantStatus:   wire.StatusOK,
			wantObserved: map[string]any{"mounted": true, "fstype": "ext4", "source": "/dev/sdb1"},
		},
		{
			name:        "missing path",
			params:      `{}`,
			wantStatus:  wire.StatusError,
			wantMessage: "missing required param: path",
		},
		{
			name:         "invalid params",
			params:       `{"path":5}`,
			wantStatus:   wire.StatusError,
			wantContains: "invalid params: ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newTestEnv(t, testMountInfo)

			tt.assert(t, runCheck(mountPresent, env, tt.params))
		})
	}
}

func TestMountPresentUnreadableMountTable(t *testing.T) {
	env := newTestEnv(t, "garbage\n")

	got := runCheck(mountPresent, env, `{"path":"/mnt/backups"}`)

	if got.Status != wire.StatusError {
		t.Fatalf("status = %q, want error", got.Status)
	}
}
