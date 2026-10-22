package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pentacore/lookout/internal/wire"
)

func TestCredentialsRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store := Store{Dir: dir}
	want := Credentials{ServerUUID: "abc", AgentToken: "abc.secret"}

	if err := store.SaveCredentials(want); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	got, err := store.LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	assertMode(t, filepath.Join(dir, "credentials.json"), 0o600)
	assertMode(t, dir, 0o700)
}

func TestLoadCredentialsMissing(t *testing.T) {
	_, err := Store{Dir: t.TempDir()}.LoadCredentials()
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got %v, want an error wrapping fs.ErrNotExist", err)
	}
}

func TestLoadCredentialsCorrupt(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"token does not belong to uuid", `{"server_uuid":"a","agent_token":"b.secret"}`, "agent_token"},
		{"empty token", `{"server_uuid":"a","agent_token":""}`, "agent_token"},
		{"not json", `{`, "credentials"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Store{Dir: dir}.LoadCredentials()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want an error containing %q", err, tt.want)
			}
			if errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a corrupt file must not look like a missing one: %v", err)
			}
		})
	}
}

func TestLoadCredentialsIgnoresUnknownFields(t *testing.T) {
	dir := t.TempDir()
	content := `{"server_uuid":"a","agent_token":"a.secret","future":true}`
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Store{Dir: dir}.LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if got.ServerUUID != "a" {
		t.Fatalf("got %+v", got)
	}
}

func TestPlatformConfigDefault(t *testing.T) {
	got, err := Store{Dir: t.TempDir()}.LoadPlatformConfig()
	if err != nil {
		t.Fatalf("LoadPlatformConfig: %v", err)
	}
	if got.ConfigVersion != 0 || got.ReportIntervalSeconds != 60 {
		t.Fatalf("got %+v", got)
	}
	if got.Checks == nil || len(got.Checks) != 0 {
		t.Fatalf("Checks = %#v, want a non-nil empty slice", got.Checks)
	}
}

func TestPlatformConfigRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store := Store{Dir: dir}
	want := PlatformConfig{
		ConfigVersion:         7,
		ReportIntervalSeconds: 30,
		Checks: []wire.CheckConfig{
			{ID: "c1", Type: "mount_present", Params: json.RawMessage(`{"path":"/mnt/user"}`)},
		},
	}

	if err := store.SavePlatformConfig(want); err != nil {
		t.Fatalf("SavePlatformConfig: %v", err)
	}
	got, err := store.LoadPlatformConfig()
	if err != nil {
		t.Fatalf("LoadPlatformConfig: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	assertMode(t, filepath.Join(dir, "platform-config.json"), 0o600)
}

func TestPlatformConfigNullChecksLoadAsEmpty(t *testing.T) {
	dir := t.TempDir()
	content := `{"config_version":3,"report_interval_seconds":45,"checks":null}`
	if err := os.WriteFile(filepath.Join(dir, "platform-config.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Store{Dir: dir}.LoadPlatformConfig()
	if err != nil {
		t.Fatalf("LoadPlatformConfig: %v", err)
	}
	if got.ConfigVersion != 3 || got.ReportIntervalSeconds != 45 {
		t.Fatalf("got %+v", got)
	}
	if got.Checks == nil || len(got.Checks) != 0 {
		t.Fatalf("Checks = %#v, want a non-nil empty slice", got.Checks)
	}
}

func TestLoadPlatformConfigCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "platform-config.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Store{Dir: dir}).LoadPlatformConfig(); err == nil {
		t.Fatal("want an error for corrupt platform config")
	}
}

func TestWriteFileAtomicReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.json")
	if err := os.WriteFile(path, []byte("old content that is longer"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WriteFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("content = %q, want %q", got, "new")
	}
	assertMode(t, path, 0o600)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "target.json" {
		t.Fatalf("dir should contain only the target, got %v", entries)
	}
}

func TestWriteFileAtomicMissingDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope", "target.json")
	if err := WriteFileAtomic(path, []byte("x"), 0o600); err == nil {
		t.Fatal("want an error when the directory does not exist")
	}
}

func assertMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}
