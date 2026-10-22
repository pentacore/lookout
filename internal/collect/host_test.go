package collect

import (
	"os"
	"runtime"
	"testing"

	"github.com/pentacore/lookout/internal/wire"
)

func TestReadHostUnraid(t *testing.T) {
	got := ReadHost(unraidRoot)
	want := wire.Host{
		Hostname:      "babel",
		OS:            "unraid 7.1.4",
		Kernel:        "6.12.24-Unraid",
		Arch:          runtime.GOARCH,
		UptimeSeconds: 86400,
	}
	if got != want {
		t.Errorf("host = %+v, want %+v", got, want)
	}
}

func TestReadHostDebian(t *testing.T) {
	got := ReadHost("testdata/debian")
	want := wire.Host{
		Hostname:      "mailserver",
		OS:            "debian 12",
		Kernel:        "6.1.0-26-amd64",
		Arch:          runtime.GOARCH,
		UptimeSeconds: 1234,
	}
	if got != want {
		t.Errorf("host = %+v, want %+v", got, want)
	}

	root := t.TempDir()
	writeProc(t, root, "etc/os-release", "NAME=\"Ubuntu\"\nID=ubuntu\nVERSION_ID=\"24.04\"\n")
	if got := ReadHost(root).OS; got != "ubuntu 24.04" {
		t.Errorf("os = %q, want %q", got, "ubuntu 24.04")
	}
}

func TestReadHostOSFallbacks(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name:  "unraid wins over os-release",
			files: map[string]string{"etc/unraid-version": "version=\"7.0.0\"\n", "etc/os-release": "ID=slackware\nVERSION_ID=\"15\"\n"},
			want:  "unraid 7.0.0",
		},
		{
			name:  "unraid file without version falls through",
			files: map[string]string{"etc/unraid-version": "nonsense\n", "etc/os-release": "ID=debian\nVERSION_ID=\"12\"\n"},
			want:  "debian 12",
		},
		{
			name:  "single quotes stripped",
			files: map[string]string{"etc/os-release": "ID='alpine'\nVERSION_ID='3.20'\n"},
			want:  "alpine 3.20",
		},
		{
			name:  "rolling release without version id",
			files: map[string]string{"etc/os-release": "ID=arch\n"},
			want:  "arch",
		},
		{
			name:  "no id",
			files: map[string]string{"etc/os-release": "NAME=Mystery\n"},
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for name, content := range tt.files {
				writeProc(t, root, name, content)
			}
			if got := ReadHost(root).OS; got != tt.want {
				t.Errorf("os = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadHostEmptyRoot(t *testing.T) {
	want, err := os.Hostname()
	if err != nil {
		t.Skipf("os.Hostname unavailable: %v", err)
	}
	got := ReadHost(t.TempDir())
	if got.Hostname != want {
		t.Errorf("hostname = %q, want %q", got.Hostname, want)
	}
	if got.OS != "" || got.Kernel != "" || got.UptimeSeconds != 0 {
		t.Errorf("expected empty os/kernel/uptime, got %+v", got)
	}
	if got.Arch != runtime.GOARCH {
		t.Errorf("arch = %q, want %q", got.Arch, runtime.GOARCH)
	}
}
