package wire

import (
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/report.golden.json")

const goldenPath = "testdata/report.golden.json"

func fixtureReport() ReportRequest {
	health := "healthy"
	return ReportRequest{
		Schema:        SchemaVersion,
		AgentVersion:  "0.1.0",
		SentAt:        "2026-10-22T12:00:00Z",
		ConfigVersion: 3,
		Host: Host{
			Hostname:      "babel",
			OS:            "unraid 7.1.4",
			Kernel:        "6.12.24-Unraid",
			Arch:          "amd64",
			UptimeSeconds: 86400,
		},
		Metrics: Metrics{
			CPUPercent: 12.5,
			Load:       [3]float64{0.5, 0.7, 0.9},
			Memory: Memory{
				TotalBytes:     16000000000,
				AvailableBytes: 8000000000,
				SwapTotalBytes: 2000000000,
				SwapFreeBytes:  1500000000,
			},
			OOMKillsTotal: 0,
			Filesystems: []Filesystem{
				{
					Mount:          "/",
					Device:         "rootfs",
					FSType:         "rootfs",
					TotalBytes:     Bytes(1000000),
					UsedBytes:      Bytes(400000),
					AvailableBytes: Bytes(600000),
					Unresponsive:   false,
				},
				{
					Mount:        "/mnt/backups",
					Device:       "nas:/backups",
					FSType:       "nfs4",
					Unresponsive: true,
				},
			},
			FailedUnits: nil,
			Containers: []Container{
				{Name: "postfix-mailcow", State: "running", Health: &health, RestartCount: 4},
			},
		},
		Checks: []CheckResult{
			{
				ID:       "c1",
				Type:     "mount_present",
				Status:   StatusCrit,
				Message:  "/mnt/backups is not mounted",
				Observed: map[string]any{"mounted": false},
			},
		},
	}
}

func TestReportRequestMatchesGolden(t *testing.T) {
	got, err := json.MarshalIndent(fixtureReport(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')

	if *update {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("report JSON differs from %s (run with -update to rewrite)\n got: %s\nwant: %s", goldenPath, got, want)
	}
}

func keysOf(t *testing.T, v any) []string {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T", v)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func assertKeys(t *testing.T, name string, v any, want []string) {
	t.Helper()
	want = slices.Clone(want)
	slices.Sort(want)
	if got := keysOf(t, v); !reflect.DeepEqual(got, want) {
		t.Errorf("%s keys = %v, want %v", name, got, want)
	}
}

func TestReportRequestFieldNames(t *testing.T) {
	raw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	top := []string{"schema", "agent_version", "sent_at", "config_version", "host", "metrics", "checks"}
	host := []string{"hostname", "os", "kernel", "arch", "uptime_seconds"}
	metrics := []string{"cpu_percent", "load", "memory", "oom_kills_total", "filesystems", "failed_units", "containers"}
	memory := []string{"total_bytes", "available_bytes", "swap_total_bytes", "swap_free_bytes"}
	fs := []string{"mount", "device", "fstype", "total_bytes", "used_bytes", "available_bytes", "unresponsive"}
	fsUnresponsive := []string{"mount", "device", "fstype", "unresponsive"}
	container := []string{"name", "state", "health", "restart_count"}
	check := []string{"id", "type", "status", "message", "observed"}

	assertKeys(t, "top", doc, top)
	assertKeys(t, "host", doc["host"], host)
	m := doc["metrics"].(map[string]any)
	assertKeys(t, "metrics", m, metrics)
	assertKeys(t, "memory", m["memory"], memory)
	filesystems := m["filesystems"].([]any)
	assertKeys(t, "filesystems[0]", filesystems[0], fs)
	assertKeys(t, "filesystems[1]", filesystems[1], fsUnresponsive)
	assertKeys(t, "containers[0]", m["containers"].([]any)[0], container)
	assertKeys(t, "checks[0]", doc["checks"].([]any)[0], check)
}

func TestEnrollRequestFieldNames(t *testing.T) {
	raw, err := json.Marshal(EnrollRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, "enroll", doc, []string{"enrollment_token", "hostname", "os", "kernel", "arch", "agent_version"})
}

func TestNullVersusEmpty(t *testing.T) {
	health := "healthy"
	tests := []struct {
		name    string
		value   any
		want    string
		notWant string
	}{
		{"nil failed_units", Metrics{}, `"failed_units":null`, ""},
		{"empty failed_units", Metrics{FailedUnits: []string{}}, `"failed_units":[]`, ""},
		{"nil containers", Metrics{}, `"containers":null`, ""},
		{"empty containers", Metrics{Containers: []Container{}}, `"containers":[]`, ""},
		{"nil health", Container{}, `"health":null`, ""},
		{"set health", Container{Health: &health}, `"health":"healthy"`, ""},
		{"zero bytes present", Filesystem{AvailableBytes: Bytes(0)}, `"available_bytes":0`, ""},
		{"nil bytes absent", Filesystem{Unresponsive: true}, `"unresponsive":true`, `available_bytes`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.value)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), tt.want) {
				t.Errorf("%s does not contain %s", raw, tt.want)
			}
			if tt.notWant != "" && strings.Contains(string(raw), tt.notWant) {
				t.Errorf("%s unexpectedly contains %s", raw, tt.notWant)
			}
		})
	}
}

func TestReportResponseDecode(t *testing.T) {
	t.Run("null config", func(t *testing.T) {
		var resp ReportResponse
		in := `{"config_version":4,"config":null,"report_interval_seconds":60,"server_time":"2026-10-22T12:00:01Z","future":1}`
		if err := json.Unmarshal([]byte(in), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Config != nil {
			t.Errorf("Config = %+v, want nil", resp.Config)
		}
		if resp.ConfigVersion != 4 || resp.ReportIntervalSeconds != 60 || resp.ServerTime != "2026-10-22T12:00:01Z" {
			t.Errorf("unexpected decode: %+v", resp)
		}
	})

	t.Run("with config", func(t *testing.T) {
		var resp ReportResponse
		in := `{"config_version":4,"config":{"checks":[{"id":"c1","type":"mount_present","params":{"path":"/mnt/backups"},"extra":true}]},"report_interval_seconds":60,"server_time":"x"}`
		if err := json.Unmarshal([]byte(in), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Config == nil || len(resp.Config.Checks) != 1 {
			t.Fatalf("config = %+v", resp.Config)
		}
		c := resp.Config.Checks[0]
		if c.ID != "c1" || c.Type != "mount_present" {
			t.Errorf("check = %+v", c)
		}
		if string(c.Params) != `{"path":"/mnt/backups"}` {
			t.Errorf("Params = %s", c.Params)
		}
	})
}

func TestEnrollResponseDecode(t *testing.T) {
	var resp EnrollResponse
	in := `{"server_uuid":"u","agent_token":"u.s","report_interval_seconds":60,"future":true}`
	if err := json.Unmarshal([]byte(in), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ServerUUID != "u" || resp.AgentToken != "u.s" || resp.ReportIntervalSeconds != 60 {
		t.Errorf("unexpected decode: %+v", resp)
	}
}
