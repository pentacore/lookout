package agent

import (
	"encoding/json"
	"path/filepath"

	"github.com/pentacore/lookout/internal/state"
)

const (
	statusFile = "status.json"
	statusPerm = 0o644
)

// status is the content of {RuntimeDir}/status.json, read by the Unraid page
// and by humans. Times are RFC3339 UTC; LastSuccessAt is empty until the first
// accepted report and LastError is empty after a successful one.
type status struct {
	AgentVersion  string `json:"agent_version"`
	ServerUUID    string `json:"server_uuid"`
	LastAttemptAt string `json:"last_attempt_at"`
	LastSuccessAt string `json:"last_success_at"`
	LastError     string `json:"last_error"`
}

// writeStatus replaces the status file. A failure only logs at debug: the
// status file is a convenience and must never disturb reporting.
func (a *Agent) writeStatus() {
	data, err := json.Marshal(a.status)
	if err == nil {
		err = state.WriteFileAtomic(filepath.Join(a.d.RuntimeDir, statusFile), append(data, '\n'), statusPerm)
	}
	if err != nil {
		a.log.Debug("cannot write status file", "dir", a.d.RuntimeDir, "error", err)
	}
}
