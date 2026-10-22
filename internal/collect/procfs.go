// Package collect reads host metrics from /proc and related files. Every
// reader takes a root path so tests can point it at fixtures.
package collect

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/pentacore/lookout/internal/wire"
)

// cpuFieldCount is the number of /proc/stat cpu fields summed into the total:
// user, nice, system, idle, iowait, irq, softirq, steal.
const cpuFieldCount = 8

const (
	cpuIdleField   = 3
	cpuIOWaitField = 4
)

// CPUSampler computes CPU utilisation between successive calls to Percent.
type CPUSampler struct {
	root string

	mu       sync.Mutex
	primed   bool
	lastBusy uint64
	lastAll  uint64
}

// NewCPUSampler returns a sampler reading root/proc/stat.
func NewCPUSampler(root string) *CPUSampler {
	return &CPUSampler{root: root}
}

// Percent returns the CPU busy percentage since the previous call, rounded to
// one decimal. The first call returns 0. A counter reset yields 0, not an error.
func (s *CPUSampler) Percent() (float64, error) {
	busy, total, err := readCPUTimes(s.root)
	if err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	primed, lastBusy, lastAll := s.primed, s.lastBusy, s.lastAll
	s.primed, s.lastBusy, s.lastAll = true, busy, total

	if !primed || total <= lastAll || busy < lastBusy {
		return 0, nil
	}
	percent := 100 * float64(busy-lastBusy) / float64(total-lastAll)
	return math.Round(percent*10) / 10, nil
}

// readCPUTimes returns the busy and total jiffies from the aggregate cpu line.
func readCPUTimes(root string) (busy, total uint64, err error) {
	data, err := os.ReadFile(filepath.Join(root, "proc/stat"))
	if err != nil {
		return 0, 0, err
	}
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "cpu" {
			continue
		}
		if len(fields) < 1+cpuFieldCount {
			return 0, 0, fmt.Errorf("proc/stat: cpu line has %d fields, want at least %d", len(fields)-1, cpuFieldCount)
		}
		var values [cpuFieldCount]uint64
		for i := range values {
			values[i], err = strconv.ParseUint(fields[1+i], 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("proc/stat: %w", err)
			}
			total += values[i]
		}
		return total - values[cpuIdleField] - values[cpuIOWaitField], total, nil
	}
	return 0, 0, errors.New("proc/stat: no cpu line")
}

// ReadLoad returns the 1, 5 and 15 minute load averages.
func ReadLoad(root string) ([3]float64, error) {
	var load [3]float64
	data, err := os.ReadFile(filepath.Join(root, "proc/loadavg"))
	if err != nil {
		return load, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < len(load) {
		return load, fmt.Errorf("proc/loadavg: %d fields, want at least %d", len(fields), len(load))
	}
	for i := range load {
		load[i], err = strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return [3]float64{}, fmt.Errorf("proc/loadavg: %w", err)
		}
	}
	return load, nil
}

// ReadMemory returns memory and swap figures in bytes from /proc/meminfo.
func ReadMemory(root string) (wire.Memory, error) {
	file, err := os.Open(filepath.Join(root, "proc/meminfo"))
	if err != nil {
		return wire.Memory{}, err
	}
	defer func() { _ = file.Close() }()

	var memory wire.Memory
	targets := map[string]*uint64{
		"MemTotal":     &memory.TotalBytes,
		"MemAvailable": &memory.AvailableBytes,
		"SwapTotal":    &memory.SwapTotalBytes,
		"SwapFree":     &memory.SwapFreeBytes,
	}
	found := 0

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, rest, ok := strings.Cut(scanner.Text(), ":")
		target, wanted := targets[key]
		if !ok || !wanted {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return wire.Memory{}, fmt.Errorf("proc/meminfo: %s has no value", key)
		}
		kilobytes, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return wire.Memory{}, fmt.Errorf("proc/meminfo: %s: %w", key, err)
		}
		*target = kilobytes * 1024
		found++
	}
	if err := scanner.Err(); err != nil {
		return wire.Memory{}, err
	}
	if found == 0 {
		return wire.Memory{}, errors.New("proc/meminfo: no memory fields found")
	}
	return memory, nil
}

// ReadOOMKills returns the cumulative oom_kill counter from /proc/vmstat, or 0
// when the kernel does not expose it.
func ReadOOMKills(root string) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(root, "proc/vmstat"))
	if err != nil {
		return 0, err
	}
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "oom_kill" {
			continue
		}
		count, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("proc/vmstat: oom_kill: %w", err)
		}
		return count, nil
	}
	return 0, nil
}
