package collect

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Mount is one line of /proc/self/mountinfo. Root, MountPoint and Source are
// unescaped.
type Mount struct {
	ID, ParentID int
	Major, Minor uint32
	Root         string
	MountPoint   string
	Options      string
	FSType       string
	Source       string
}

// MountTable is the parsed mountinfo, in kernel order (later entries are
// mounted on top of earlier ones).
type MountTable []Mount

// mountinfoFixedFields is the number of fields before the optional fields:
// mount ID, parent ID, major:minor, root, mount point, mount options.
const mountinfoFixedFields = 6

// ReadMountTable reads and parses {root}/proc/self/mountinfo.
func ReadMountTable(root string) (MountTable, error) {
	data, err := os.ReadFile(filepath.Join(root, "proc/self/mountinfo"))
	if err != nil {
		return nil, err
	}
	return ParseMountInfo(bytes.NewReader(data))
}

// ParseMountInfo parses the mountinfo format described in proc(5). Optional
// fields up to the lone "-" separator are skipped. Blank lines are ignored.
func ParseMountInfo(r io.Reader) (MountTable, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var table MountTable
	lineNumber := 0
	for line := range strings.Lines(string(data)) {
		lineNumber++
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		mount, err := parseMountFields(fields)
		if err != nil {
			return nil, fmt.Errorf("mountinfo line %d: %w", lineNumber, err)
		}
		table = append(table, mount)
	}
	return table, nil
}

func parseMountFields(fields []string) (Mount, error) {
	separator := -1
	for i := mountinfoFixedFields; i < len(fields); i++ {
		if fields[i] == "-" {
			separator = i
			break
		}
	}
	if separator < 0 {
		return Mount{}, fmt.Errorf("no \"-\" separator after %d fields", mountinfoFixedFields)
	}
	if len(fields) < separator+3 {
		return Mount{}, fmt.Errorf("want fstype and source after the separator")
	}

	id, err := strconv.Atoi(fields[0])
	if err != nil {
		return Mount{}, fmt.Errorf("mount ID: %w", err)
	}
	parentID, err := strconv.Atoi(fields[1])
	if err != nil {
		return Mount{}, fmt.Errorf("parent ID: %w", err)
	}
	majorText, minorText, ok := strings.Cut(fields[2], ":")
	if !ok {
		return Mount{}, fmt.Errorf("device %q is not major:minor", fields[2])
	}
	major, err := strconv.ParseUint(majorText, 10, 32)
	if err != nil {
		return Mount{}, fmt.Errorf("device major: %w", err)
	}
	minor, err := strconv.ParseUint(minorText, 10, 32)
	if err != nil {
		return Mount{}, fmt.Errorf("device minor: %w", err)
	}

	return Mount{
		ID:         id,
		ParentID:   parentID,
		Major:      uint32(major),
		Minor:      uint32(minor),
		Root:       Unescape(fields[3]),
		MountPoint: Unescape(fields[4]),
		Options:    fields[5],
		FSType:     fields[separator+1],
		Source:     Unescape(fields[separator+2]),
	}, nil
}

// Unescape decodes the kernel's \ooo octal escapes (\040 space, \011 tab,
// \012 newline, \134 backslash). Anything that isn't a valid three-digit
// octal byte escape is left as-is.
func Unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctalByte(s[i+1:i+4]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// isOctalByte reports whether digits is three octal digits no greater than 0377.
func isOctalByte(digits string) bool {
	return digits[0] >= '0' && digits[0] <= '3' &&
		digits[1] >= '0' && digits[1] <= '7' &&
		digits[2] >= '0' && digits[2] <= '7'
}

// At returns the topmost (last) entry mounted exactly at path.
func (t MountTable) At(path string) (Mount, bool) {
	path = filepath.Clean(path)
	for i := len(t) - 1; i >= 0; i-- {
		if t[i].MountPoint == path {
			return t[i], true
		}
	}
	return Mount{}, false
}

// Containing returns the topmost entry whose mount point is the longest
// component-wise prefix of path, so /mnt/backupsX is not under /mnt/backups.
// It returns the zero Mount when nothing contains path.
func (t MountTable) Containing(path string) Mount {
	path = filepath.Clean(path)
	var best Mount
	bestLength := -1
	for _, m := range t {
		if isUnderMountPoint(path, m.MountPoint) && len(m.MountPoint) >= bestLength {
			best, bestLength = m, len(m.MountPoint)
		}
	}
	return best
}

func isUnderMountPoint(path, mountPoint string) bool {
	if mountPoint == "/" {
		return strings.HasPrefix(path, "/")
	}
	return path == mountPoint || strings.HasPrefix(path, mountPoint+"/")
}
