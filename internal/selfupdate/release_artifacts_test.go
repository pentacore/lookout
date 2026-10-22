//go:build release

package selfupdate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestReleaseArtifacts checks a goreleaser dist directory the way a running
// agent checks a release: each archive's minisign signature and exact trusted
// comment, then the lookout binary inside it. It runs only with
// -tags release, against LOOKOUT_DIST (and MINISIGN_PUBLIC_KEY), from
// packaging/test/release_test.sh and from the release workflow.
// LOOKOUT_RELEASE_VERSION, when set, must equal the version goreleaser built.
func TestReleaseArtifacts(t *testing.T) {
	dist := os.Getenv("LOOKOUT_DIST")
	publicKey := os.Getenv("MINISIGN_PUBLIC_KEY")
	if dist == "" || publicKey == "" {
		t.Fatal("LOOKOUT_DIST and MINISIGN_PUBLIC_KEY must be set")
	}

	raw, err := os.ReadFile(filepath.Join(dist, "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	var metadata struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("parse metadata.json: %v", err)
	}
	if metadata.Version == "" {
		t.Fatal("metadata.json has no version")
	}
	if want := os.Getenv("LOOKOUT_RELEASE_VERSION"); want != "" && metadata.Version != want {
		t.Fatalf("built version %q, want %q", metadata.Version, want)
	}

	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			name := "lookout_linux_" + arch + ".tar.gz"
			archive, err := os.ReadFile(filepath.Join(dist, name))
			if err != nil {
				t.Fatalf("read archive: %v", err)
			}
			sig, err := os.ReadFile(filepath.Join(dist, name+".minisig"))
			if err != nil {
				t.Fatalf("read signature: %v", err)
			}
			if err := VerifyArchive(publicKey, archive, sig, "lookout "+metadata.Version+" "+name); err != nil {
				t.Fatalf("VerifyArchive: %v", err)
			}
			bin, err := ExtractBinary(archive, binaryName, maxBinary)
			if err != nil {
				t.Fatalf("ExtractBinary: %v", err)
			}
			if !bytes.Contains(bin, []byte(publicKey)) {
				t.Error("binary does not embed MINISIGN_PUBLIC_KEY, so it would never self-update")
			}
			if !bytes.Contains(bin, []byte(metadata.Version)) {
				t.Errorf("binary does not embed version %q", metadata.Version)
			}
		})
	}
}
