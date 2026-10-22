package collect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pentacore/lookout/internal/wire"
)

const unraidRoot = "testdata/unraid"

func writeProc(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCPUSamplerFirstCallZero(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "proc/stat", "cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 2 3 4 5 6 7 8 0 0\n")

	got, err := NewCPUSampler(root).Percent()
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("first call = %v, want 0", got)
	}
}

func TestCPUSamplerDelta(t *testing.T) {
	root := t.TempDir()
	sampler := NewCPUSampler(root)

	writeProc(t, root, "proc/stat", "cpu  100 0 100 700 100 0 0 0 0 0\n")
	if _, err := sampler.Percent(); err != nil {
		t.Fatal(err)
	}

	// busy 200 -> 300 (+100) over total 1000 -> 1200 (+200).
	writeProc(t, root, "proc/stat", "cpu  150 0 150 750 150 0 0 0 0 0\n")
	got, err := sampler.Percent()
	if err != nil {
		t.Fatal(err)
	}
	if got != 50.0 {
		t.Errorf("delta = %v, want 50.0", got)
	}
}

func TestCPUSamplerRounding(t *testing.T) {
	root := t.TempDir()
	sampler := NewCPUSampler(root)

	writeProc(t, root, "proc/stat", "cpu  0 0 0 0 0 0 0 0\n")
	if _, err := sampler.Percent(); err != nil {
		t.Fatal(err)
	}
	// busy 1, idle 2 -> 33.333...
	writeProc(t, root, "proc/stat", "cpu  1 0 0 2 0 0 0 0\n")
	got, err := sampler.Percent()
	if err != nil {
		t.Fatal(err)
	}
	if got != 33.3 {
		t.Errorf("rounded = %v, want 33.3", got)
	}
}

func TestCPUSamplerCounterReset(t *testing.T) {
	root := t.TempDir()
	sampler := NewCPUSampler(root)

	writeProc(t, root, "proc/stat", "cpu  150 0 150 750 150 0 0 0 0 0\n")
	if _, err := sampler.Percent(); err != nil {
		t.Fatal(err)
	}
	writeProc(t, root, "proc/stat", "cpu  100 0 100 700 100 0 0 0 0 0\n")
	got, err := sampler.Percent()
	if err != nil {
		t.Fatalf("counter reset returned error: %v", err)
	}
	if got != 0 {
		t.Errorf("counter reset = %v, want 0", got)
	}
}

func TestCPUSamplerErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		missing bool
	}{
		{name: "missing file", missing: true},
		{name: "no cpu line", content: "intr 1 2 3\n"},
		{name: "too few fields", content: "cpu  1 2 3\n"},
		{name: "non numeric", content: "cpu  1 2 x 4 5 6 7 8\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if !tt.missing {
				writeProc(t, root, "proc/stat", tt.content)
			}
			if _, err := NewCPUSampler(root).Percent(); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestReadLoad(t *testing.T) {
	got, err := ReadLoad(unraidRoot)
	if err != nil {
		t.Fatal(err)
	}
	if want := [3]float64{0.5, 0.7, 0.9}; got != want {
		t.Errorf("load = %v, want %v", got, want)
	}
}

func TestReadLoadErrors(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadLoad(root); err == nil {
		t.Error("missing file: expected error")
	}
	writeProc(t, root, "proc/loadavg", "0.5 0.7\n")
	if _, err := ReadLoad(root); err == nil {
		t.Error("short file: expected error")
	}
}

func TestReadMemory(t *testing.T) {
	got, err := ReadMemory(unraidRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := wire.Memory{
		TotalBytes:     16777216000,
		AvailableBytes: 8388608000,
		SwapTotalBytes: 0,
		SwapFreeBytes:  0,
	}
	if got != want {
		t.Errorf("memory = %+v, want %+v", got, want)
	}
}

func TestReadMemorySwap(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, "proc/meminfo",
		"MemTotal: 1000 kB\nMemAvailable: 500 kB\nSwapTotal: 200 kB\nSwapFree: 100 kB\n")
	got, err := ReadMemory(root)
	if err != nil {
		t.Fatal(err)
	}
	want := wire.Memory{
		TotalBytes:     1000 * 1024,
		AvailableBytes: 500 * 1024,
		SwapTotalBytes: 200 * 1024,
		SwapFreeBytes:  100 * 1024,
	}
	if got != want {
		t.Errorf("memory = %+v, want %+v", got, want)
	}
}

func TestReadMemoryErrors(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadMemory(root); err == nil {
		t.Error("missing file: expected error")
	}
	writeProc(t, root, "proc/meminfo", "MemTotal: lots kB\n")
	if _, err := ReadMemory(root); err == nil {
		t.Error("malformed value: expected error")
	}
}

// A missing MemTotal or MemAvailable must be an error, not 0: a zero
// available figure would look like memory exhaustion to the platform.
func TestReadMemoryRequiresTotalAndAvailable(t *testing.T) {
	tests := []struct {
		name, meminfo, want string
	}{
		{"no MemAvailable", "MemTotal: 1000 kB\nMemFree: 10 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n", "MemAvailable"},
		{"no MemTotal", "MemFree: 10 kB\nMemAvailable: 500 kB\n", "MemTotal"},
		{"only swap", "SwapTotal: 200 kB\nSwapFree: 100 kB\n", "MemTotal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeProc(t, root, "proc/meminfo", tt.meminfo)
			got, err := ReadMemory(root)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ReadMemory = %+v, %v; want an error naming %s", got, err, tt.want)
			}
		})
	}
}

func TestReadOOMKills(t *testing.T) {
	got, err := ReadOOMKills(unraidRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Errorf("oom_kill = %d, want 3", got)
	}

	root := t.TempDir()
	writeProc(t, root, "proc/vmstat", "pgfault 1\npgmajfault 2\n")
	got, err = ReadOOMKills(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("absent oom_kill = %d, want 0", got)
	}
}

func TestReadOOMKillsErrors(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadOOMKills(root); err == nil {
		t.Error("missing file: expected error")
	}
	writeProc(t, root, "proc/vmstat", "oom_kill many\n")
	if _, err := ReadOOMKills(root); err == nil {
		t.Error("malformed value: expected error")
	}
}
