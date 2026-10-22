package selfupdate

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func assertNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists (err %v), want it gone", path, err)
	}
}

func TestInstall(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "lookout")
	writeFile(t, exe, []byte("old"), 0o700)
	persistDir := filepath.Join(dir, "flash")
	if err := os.Mkdir(persistDir, 0o700); err != nil {
		t.Fatal(err)
	}
	persist := filepath.Join(persistDir, "lookout")
	writeFile(t, persist, []byte("old"), 0o600)

	if err := Install(exe, []byte("new"), persist, nil); err != nil {
		t.Fatalf("Install: %v", err)
	}

	assertFile(t, exe, []byte("new"))
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("exe mode = %v, want 0755", info.Mode().Perm())
	}
	assertNoFile(t, exe+".new")
	assertFile(t, persist, []byte("new"))
	assertNoFile(t, persist+".tmp")
}

func TestInstallWithoutPersistCopy(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "lookout")
	writeFile(t, exe, []byte("old"), 0o755)

	if err := Install(exe, []byte("new"), "", nil); err != nil {
		t.Fatalf("Install: %v", err)
	}
	assertFile(t, exe, []byte("new"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries, want only the exe", len(entries))
	}
}

func TestInstallReplacesStaleTempFile(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "lookout")
	writeFile(t, exe, []byte("old"), 0o755)
	// A leftover from a crashed attempt, and a symlink planted in its place
	// must not be followed.
	target := filepath.Join(dir, "target")
	writeFile(t, target, []byte("untouched"), 0o600)
	if err := os.Symlink(target, exe+".new"); err != nil {
		t.Fatal(err)
	}

	if err := Install(exe, []byte("new"), "", nil); err != nil {
		t.Fatalf("Install: %v", err)
	}
	assertFile(t, exe, []byte("new"))
	assertFile(t, target, []byte("untouched"))
	assertNoFile(t, exe+".new")
}

func TestInstallPersistCopyFailureLeavesExeUntouched(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "lookout")
	writeFile(t, exe, []byte("old"), 0o755)
	persist := filepath.Join(dir, "missing-dir", "lookout")

	err := Install(exe, []byte("new"), persist, nil)
	if err == nil || !strings.Contains(err.Error(), "persist") {
		t.Fatalf("Install error = %v, want a persistent copy error", err)
	}
	assertFile(t, exe, []byte("old"))
	assertNoFile(t, exe+".new")
}

func TestInstallFailureRemovesTempFile(t *testing.T) {
	dir := t.TempDir()
	// Renaming a file over a non-empty directory fails.
	exe := filepath.Join(dir, "lookout")
	if err := os.Mkdir(exe, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(exe, "keep"), []byte("x"), 0o600)

	if err := Install(exe, []byte("new"), "", nil); err == nil {
		t.Fatal("Install over a directory succeeded")
	}
	assertNoFile(t, exe+".new")
}
