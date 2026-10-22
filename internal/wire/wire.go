// Package wire holds the JSON types exchanged with the platform. The shapes
// follow the host agent contract and must not drift from it.
package wire

import "encoding/json"

// SchemaVersion is the report schema the agent sends.
const SchemaVersion = 1

// CheckStatus is the outcome of a single check.
type CheckStatus string

const (
	StatusOK          CheckStatus = "ok"
	StatusWarn        CheckStatus = "warn"
	StatusCrit        CheckStatus = "crit"
	StatusUnsupported CheckStatus = "unsupported"
	StatusError       CheckStatus = "error"
)

// EnrollRequest is the body of POST /enroll.
type EnrollRequest struct {
	EnrollmentToken string `json:"enrollment_token"`
	Hostname        string `json:"hostname"`
	OS              string `json:"os"`
	Kernel          string `json:"kernel"`
	Arch            string `json:"arch"`
	AgentVersion    string `json:"agent_version"`
}

// EnrollResponse is the body of a successful POST /enroll.
type EnrollResponse struct {
	ServerUUID            string `json:"server_uuid"`
	AgentToken            string `json:"agent_token"`
	ReportIntervalSeconds int    `json:"report_interval_seconds"`
}

// Host identifies the machine being reported.
type Host struct {
	Hostname      string `json:"hostname"`
	OS            string `json:"os"`
	Kernel        string `json:"kernel"`
	Arch          string `json:"arch"`
	UptimeSeconds int64  `json:"uptime_seconds"`
}

// Memory holds host memory figures in bytes.
type Memory struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	SwapTotalBytes uint64 `json:"swap_total_bytes"`
	SwapFreeBytes  uint64 `json:"swap_free_bytes"`
}

// Filesystem describes one mounted filesystem. The byte fields are pointers so
// a normal entry can still send a real 0 while an unresponsive entry omits them.
type Filesystem struct {
	Mount          string  `json:"mount"`
	Device         string  `json:"device"`
	FSType         string  `json:"fstype"`
	TotalBytes     *uint64 `json:"total_bytes,omitempty"`
	UsedBytes      *uint64 `json:"used_bytes,omitempty"`
	AvailableBytes *uint64 `json:"available_bytes,omitempty"`
	Unresponsive   bool    `json:"unresponsive"`
}

// Bytes returns a pointer to v, for the optional Filesystem byte fields.
func Bytes(v uint64) *uint64 {
	return &v
}

// Container describes one Docker container. Health is nil when the container
// has no healthcheck.
type Container struct {
	Name         string  `json:"name"`
	State        string  `json:"state"`
	Health       *string `json:"health"`
	RestartCount int     `json:"restart_count"`
}

// Metrics is the host metrics block of a report. A nil FailedUnits or
// Containers slice marshals as null and means "not available"; an empty slice
// marshals as [] and means "available, nothing to list".
type Metrics struct {
	CPUPercent    float64      `json:"cpu_percent"`
	Load          [3]float64   `json:"load"`
	Memory        Memory       `json:"memory"`
	OOMKillsTotal uint64       `json:"oom_kills_total"`
	Filesystems   []Filesystem `json:"filesystems"`
	FailedUnits   []string     `json:"failed_units"`
	Containers    []Container  `json:"containers"`
}

// CheckConfig is one platform-defined check; Params stays raw until the check
// type decodes it.
type CheckConfig struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Params json.RawMessage `json:"params"`
}

// CheckResult is the agent's verdict for one CheckConfig.
type CheckResult struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Status   CheckStatus    `json:"status"`
	Message  string         `json:"message"`
	Observed map[string]any `json:"observed"`
}

// ReportRequest is the body of POST /report.
type ReportRequest struct {
	Schema        int           `json:"schema"`
	AgentVersion  string        `json:"agent_version"`
	SentAt        string        `json:"sent_at"`
	ConfigVersion int64         `json:"config_version"`
	Host          Host          `json:"host"`
	Metrics       Metrics       `json:"metrics"`
	Checks        []CheckResult `json:"checks"`
}

// PlatformConfig is the config block of a report response.
type PlatformConfig struct {
	Checks []CheckConfig `json:"checks"`
}

// ReportResponse is the body of a successful POST /report. Config is nil when
// the platform has nothing newer than the reported config_version.
type ReportResponse struct {
	ConfigVersion         int64           `json:"config_version"`
	Config                *PlatformConfig `json:"config"`
	ReportIntervalSeconds int             `json:"report_interval_seconds"`
	ServerTime            string          `json:"server_time"`
}
