package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lookout.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaultsSystemd(t *testing.T) {
	path := writeFile(t, "platform_url: https://media.pentacore.se\n")
	cfg, err := Load(path, PlatformSystemd)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlatformURL != "https://media.pentacore.se" {
		t.Errorf("PlatformURL = %q", cfg.PlatformURL)
	}
	if cfg.StateDir != "/var/lib/lookout" {
		t.Errorf("StateDir = %q", cfg.StateDir)
	}
	if cfg.RuntimeDir != "/run/lookout" {
		t.Errorf("RuntimeDir = %q", cfg.RuntimeDir)
	}
	if !cfg.AutoUpdate {
		t.Error("AutoUpdate = false, want true")
	}
	if cfg.Platform != PlatformSystemd || cfg.Path != path {
		t.Errorf("Platform/Path = %q/%q", cfg.Platform, cfg.Path)
	}
}

func TestLoadDefaultsUnraid(t *testing.T) {
	path := writeFile(t, "platform_url: https://media.pentacore.se\n")
	cfg, err := Load(path, PlatformUnraid)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateDir != "/boot/config/plugins/lookout/state" {
		t.Errorf("StateDir = %q", cfg.StateDir)
	}
	if cfg.RuntimeDir != "/run/lookout" {
		t.Errorf("RuntimeDir = %q", cfg.RuntimeDir)
	}
}

func TestLoadOverrides(t *testing.T) {
	path := writeFile(t, "platform_url: https://a\nstate_dir: /s\nruntime_dir: /r\n")
	cfg, err := Load(path, PlatformSystemd)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateDir != "/s" || cfg.RuntimeDir != "/r" {
		t.Errorf("StateDir/RuntimeDir = %q/%q", cfg.StateDir, cfg.RuntimeDir)
	}
}

func TestLoadAutoUpdateFalse(t *testing.T) {
	path := writeFile(t, "platform_url: https://a\nauto_update: false\n")
	cfg, err := Load(path, PlatformSystemd)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AutoUpdate {
		t.Error("AutoUpdate = true, want false")
	}
}

func TestLoadUnknownKeys(t *testing.T) {
	const typoKey = "platfrom_url" //nolint:misspell // deliberate typo of platform_url
	path := writeFile(t, typoKey+": x\n")
	cfg, err := Load(path, PlatformSystemd)
	if err != nil {
		t.Fatalf("unknown keys must not be an error: %v", err)
	}
	if want := []string{typoKey}; !reflect.DeepEqual(cfg.UnknownKeys, want) {
		t.Errorf("UnknownKeys = %v, want %v", cfg.UnknownKeys, want)
	}
}

func TestLoadEmptyFile(t *testing.T) {
	cfg, err := Load(writeFile(t, ""), PlatformSystemd)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateDir != "/var/lib/lookout" || !cfg.AutoUpdate {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	if _, err := Load(writeFile(t, "platform_url: [\n"), PlatformSystemd); err == nil {
		t.Error("want error for malformed YAML")
	}
}

func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.yaml")
	cfg, err := Load(path, PlatformUnraid)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
	if cfg.StateDir != "/boot/config/plugins/lookout/state" || !cfg.AutoUpdate {
		t.Errorf("defaults not returned: %+v", cfg)
	}
	if cfg.Path != path {
		t.Errorf("Path = %q", cfg.Path)
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]string{ // url -> error substring ("" = valid)
		"":                            "platform_url is required",
		"http://media.example":        "https",
		"ftp://x":                     "https",
		"https://":                    "host",
		"http://127.0.0.1:8080":       "",
		"http://localhost":            "",
		"http://[::1]:9000":           "",
		"https://media.pentacore.se/": "",
	}
	for url, want := range cases {
		t.Run(url, func(t *testing.T) {
			err := Config{PlatformURL: url}.Validate()
			if want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Validate() = %v, want error containing %q", err, want)
			}
		})
	}
}

func TestBaseURL(t *testing.T) {
	if got := (Config{PlatformURL: "https://media.pentacore.se/"}).BaseURL(); got != "https://media.pentacore.se" {
		t.Errorf("BaseURL() = %q", got)
	}
}

func TestDetectPlatform(t *testing.T) {
	unraid := t.TempDir()
	if err := os.MkdirAll(filepath.Join(unraid, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unraid, "etc", "unraid-version"), []byte("version=\"7.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DetectPlatform(unraid); got != PlatformUnraid {
		t.Errorf("unraid root = %q", got)
	}
	if got := DetectPlatform(t.TempDir()); got != PlatformSystemd {
		t.Errorf("empty root = %q", got)
	}
}

func TestDefaultPath(t *testing.T) {
	cases := map[Platform]string{
		PlatformSystemd: "/etc/lookout/lookout.yaml",
		PlatformUnraid:  "/boot/config/plugins/lookout/lookout.yaml",
	}
	for p, want := range cases {
		if got := DefaultPath(p); got != want {
			t.Errorf("DefaultPath(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestSetPlatformURL(t *testing.T) {
	t.Run("missing file is created", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sub", "lookout.yaml")
		if err := SetPlatformURL(path, "https://a"); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(data); got != "platform_url: https://a\n" {
			t.Errorf("file = %q", got)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Errorf("mode = %v", info.Mode().Perm())
		}
		dir, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if dir.Mode().Perm() != 0o755 {
			t.Errorf("dir mode = %v", dir.Mode().Perm())
		}
	})

	t.Run("existing value is replaced and the rest survives", func(t *testing.T) {
		path := writeFile(t, "# keep\nplatform_url: \"\"\nstate_dir: /x\n")
		if err := SetPlatformURL(path, "https://b"); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got := string(data)
		for _, want := range []string{"# keep", "state_dir: /x", "platform_url: https://b"} {
			if !strings.Contains(got, want) {
				t.Errorf("file %q lacks %q", got, want)
			}
		}
		cfg, err := Load(path, PlatformSystemd)
		if err != nil || cfg.PlatformURL != "https://b" || cfg.StateDir != "/x" {
			t.Errorf("reload = %+v, %v", cfg, err)
		}
	})

	t.Run("key absent is appended", func(t *testing.T) {
		path := writeFile(t, "state_dir: /x\n")
		if err := SetPlatformURL(path, "https://c"); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path, PlatformSystemd)
		if err != nil || cfg.PlatformURL != "https://c" || cfg.StateDir != "/x" {
			t.Errorf("reload = %+v, %v", cfg, err)
		}
	})

	t.Run("empty file", func(t *testing.T) {
		path := writeFile(t, "")
		if err := SetPlatformURL(path, "https://d"); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path, PlatformSystemd)
		if err != nil || cfg.PlatformURL != "https://d" {
			t.Errorf("reload = %+v, %v", cfg, err)
		}
	})
}

func TestSetPlatformURLKeepsSymlinkAndLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.yaml")
	link := filepath.Join(dir, "lookout.yaml")
	if err := os.WriteFile(target, []byte("state_dir: /x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.yaml", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := SetPlatformURL(link, "https://e"); err != nil {
		t.Fatal(err)
	}

	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link was replaced: %v, %v", info, err)
	}
	cfg, err := Load(target, PlatformSystemd)
	if err != nil || cfg.PlatformURL != "https://e" || cfg.StateDir != "/x" {
		t.Errorf("reload target = %+v, %v", cfg, err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("target mode = %v, %v; want the existing 0600 kept", info, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("directory has %d entries, want only the file and its link", len(entries))
	}
}
