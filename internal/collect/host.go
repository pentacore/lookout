package collect

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/pentacore/lookout/internal/wire"
)

// ReadHost identifies the host. It never fails: anything unreadable is left
// empty, except the hostname, which falls back to os.Hostname.
func ReadHost(root string) wire.Host {
	host := wire.Host{
		Hostname:      readTrimmed(root, "proc/sys/kernel/hostname"),
		OS:            readOS(root),
		Kernel:        readTrimmed(root, "proc/sys/kernel/osrelease"),
		Arch:          runtime.GOARCH,
		UptimeSeconds: readUptime(root),
	}
	if host.Hostname == "" {
		host.Hostname, _ = os.Hostname()
	}
	return host
}

func readTrimmed(root, name string) string {
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func readUptime(root string) int64 {
	fields := strings.Fields(readTrimmed(root, "proc/uptime"))
	if len(fields) == 0 {
		return 0
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || seconds < 0 || math.IsNaN(seconds) || seconds > math.MaxInt64 {
		return 0
	}
	return int64(math.Floor(seconds))
}

// readOS prefers Unraid's own version file, then os-release.
func readOS(root string) string {
	if version := parseEnvFile(readTrimmed(root, "etc/unraid-version"))["version"]; version != "" {
		return "unraid " + version
	}
	release := parseEnvFile(readTrimmed(root, "etc/os-release"))
	id := release["ID"]
	if id == "" {
		return ""
	}
	if versionID := release["VERSION_ID"]; versionID != "" {
		return id + " " + versionID
	}
	return id
}

// parseEnvFile parses KEY=value lines, stripping one layer of surrounding
// single or double quotes from values.
func parseEnvFile(content string) map[string]string {
	values := make(map[string]string)
	for line := range strings.Lines(content) {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		values[strings.TrimSpace(key)] = value
	}
	return values
}
