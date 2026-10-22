package collect

import (
	"slices"
	"strings"
	"testing"
)

func TestUnescape(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{`a\040b`, "a b"},
		{`a\011b`, "a\tb"},
		{`a\012b`, "a\nb"},
		{`a\134b`, `a\b`},
		{"plain", "plain"},
		{`bad\04`, `bad\04`},
		{`\040lead`, " lead"},
		{`not\8oct`, `not\8oct`},
	}
	for _, tt := range tests {
		if got := Unescape(tt.in); got != tt.want {
			t.Errorf("Unescape(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func readUnraidMounts(t *testing.T) MountTable {
	t.Helper()
	table, err := ReadMountTable(unraidRoot)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func mountByID(t *testing.T, table MountTable, id int) Mount {
	t.Helper()
	for _, m := range table {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("no mount with ID %d", id)
	return Mount{}
}

func TestParseMountInfoUnraid(t *testing.T) {
	table := readUnraidMounts(t)
	if len(table) != 16 {
		t.Fatalf("len = %d, want 16", len(table))
	}

	tests := []struct {
		name string
		id   int
		want Mount
	}{
		{"escaped mount point", 34, Mount{ID: 34, ParentID: 1, Major: 9, Minor: 1, Root: "/appdata", MountPoint: "/mnt/appdata cache", Options: "rw", FSType: "xfs", Source: "/dev/md1p1"}},
		{"optional fields skipped", 33, Mount{ID: 33, ParentID: 1, Major: 0, Minor: 41, Root: "/", MountPoint: "/mnt/backups", Options: "rw", FSType: "nfs4", Source: "nas:/backups"}},
		{"root", 1, Mount{ID: 1, ParentID: 0, Major: 0, Minor: 2, Root: "/", MountPoint: "/", Options: "rw,relatime", FSType: "rootfs", Source: "rootfs"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mountByID(t, table, tt.id); got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseMountInfoMalformed(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{"too few fields", "1 0 0:2 / / rw - rootfs rootfs rw\nx y\n", "mountinfo line 2"},
		{"no separator", "1 0 0:2 / / rw shared:1 rootfs rootfs rw\n", "mountinfo line 1"},
		{"bad device", "1 0 02 / / rw - rootfs rootfs rw\n", "mountinfo line 1"},
		{"bad id", "a 0 0:2 / / rw - rootfs rootfs rw\n", "mountinfo line 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseMountInfo(strings.NewReader(tt.input))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func mountPoints(mounts []Mount) []string {
	points := make([]string, 0, len(mounts))
	for _, m := range mounts {
		points = append(points, m.MountPoint)
	}
	return points
}

func parseTable(t *testing.T, input string) MountTable {
	t.Helper()
	table, err := ParseMountInfo(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func TestSelectFilesystemsUnraid(t *testing.T) {
	got := mountPoints(SelectFilesystems(readUnraidMounts(t)))
	want := []string{"/", "/boot", "/mnt/disk1", "/mnt/user", "/mnt/backups", "/mnt/appdata cache", "/var/lib/docker"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSelectFilesystemsRootAlwaysIncluded(t *testing.T) {
	table := parseTable(t, ""+
		"1 0 0:2 / / rw - tmpfs tmpfs rw\n"+
		"2 1 0:3 / /tmp rw - tmpfs tmpfs rw\n")
	got := mountPoints(SelectFilesystems(table))
	if want := []string{"/"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSelectFilesystemsRootFirst(t *testing.T) {
	table := parseTable(t, ""+
		"30 1 8:1 / /boot rw - vfat /dev/sda1 rw\n"+
		"1 0 8:2 / / rw - ext4 /dev/sda2 rw\n")
	got := mountPoints(SelectFilesystems(table))
	if want := []string{"/", "/boot"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSelectFilesystemsStackedMountPoint(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string // "mountpoint source" pairs
	}{
		{
			name: "later mount at the same point wins",
			input: "" +
				"1 0 8:2 / / rw - ext4 /dev/sda2 rw\n" +
				"50 1 8:3 / /mnt/x rw - ext4 /dev/sda3 rw\n" +
				"51 1 8:4 / /mnt/x rw - ext4 /dev/sda4 rw\n",
			want: []string{"/ /dev/sda2", "/mnt/x /dev/sda4"},
		},
		{
			name: "stacked root keeps its topmost entry",
			input: "" +
				"1 0 0:2 / / rw - rootfs rootfs rw\n" +
				"2 1 8:2 / / rw - ext4 /dev/sda2 rw\n",
			want: []string{"/ /dev/sda2"},
		},
		{
			name: "shadowed mount does not hide a bind of its device",
			input: "" +
				"1 0 8:2 / / rw - ext4 /dev/sda2 rw\n" +
				"50 1 8:3 / /mnt/x rw - ext4 /dev/sda3 rw\n" +
				"51 1 8:4 / /mnt/x rw - ext4 /dev/sda4 rw\n" +
				"52 1 8:3 / /mnt/y rw - ext4 /dev/sda3 rw\n",
			want: []string{"/ /dev/sda2", "/mnt/x /dev/sda4", "/mnt/y /dev/sda3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, m := range SelectFilesystems(parseTable(t, tt.input)) {
				got = append(got, m.MountPoint+" "+m.Source)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMountTableContaining(t *testing.T) {
	table := readUnraidMounts(t)
	tests := []struct {
		path, want string
	}{
		{"/mnt/backups/daily", "/mnt/backups"},
		{"/mnt/backups", "/mnt/backups"},
		{"/mnt/backupsX", "/"},
		{"/mnt/disk1/x", "/mnt/disk1"},
		{"/var/lib/docker/btrfs/subvolumes/abc/etc", "/var/lib/docker/btrfs/subvolumes/abc"},
	}
	for _, tt := range tests {
		if got := table.Containing(tt.path).MountPoint; got != tt.want {
			t.Errorf("Containing(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}

	stacked := parseTable(t, ""+
		"1 0 8:2 / / rw - ext4 /dev/sda2 rw\n"+
		"50 1 8:3 / /mnt/x rw - ext4 /dev/sda3 rw\n"+
		"51 1 8:4 / /mnt/x rw - ext4 /dev/sda4 rw\n")
	if got := stacked.Containing("/mnt/x/file").ID; got != 51 {
		t.Errorf("stacked Containing ID = %d, want 51", got)
	}
}

func TestMountTableAt(t *testing.T) {
	table := readUnraidMounts(t)
	tests := []struct {
		path      string
		wantFound bool
		wantID    int
	}{
		{"/mnt/backups/", true, 33},
		{"/mnt/appdata cache", true, 34},
		{"/mnt", false, 0},
	}
	for _, tt := range tests {
		got, found := table.At(tt.path)
		if found != tt.wantFound || got.ID != tt.wantID {
			t.Errorf("At(%q) = (ID %d, %v), want (ID %d, %v)", tt.path, got.ID, found, tt.wantID, tt.wantFound)
		}
	}

	stacked := parseTable(t, ""+
		"50 1 8:3 / /mnt/x rw - ext4 /dev/sda3 rw\n"+
		"51 1 8:4 / /mnt/x rw - ext4 /dev/sda4 rw\n")
	if got, _ := stacked.At("/mnt/x"); got.ID != 51 {
		t.Errorf("stacked At ID = %d, want 51", got.ID)
	}
}
