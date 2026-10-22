package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"strings"
	"testing"

	"aead.dev/minisign"
)

const testTrusted = "lookout 0.2.0 lookout_linux_amd64.tar.gz"

func newKey(t *testing.T) (string, minisign.PrivateKey) {
	t.Helper()
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub.String(), priv
}

func TestVerifyArchive(t *testing.T) {
	pub, priv := newKey(t)
	otherPub, otherPriv := newKey(t)
	data := []byte("archive bytes")

	prehashed := func(trusted string) []byte {
		r := minisign.NewReader(bytes.NewReader(data))
		if _, err := r.Read(make([]byte, len(data)+1)); err != nil {
			t.Fatal(err)
		}
		return r.SignWithComments(priv, trusted, "")
	}

	tests := []struct {
		name    string
		key     string
		data    []byte
		sig     []byte
		wantErr string // "" means success
	}{
		{"legacy Ed signature", pub, data, minisign.SignWithComments(priv, data, testTrusted, ""), ""},
		{"prehashed ED signature", pub, data, prehashed(testTrusted), ""},
		{"public key file text", "untrusted comment: minisign public key\n" + pub + "\n", data, minisign.SignWithComments(priv, data, testTrusted, ""), ""},
		{"tampered data", pub, []byte("archive bytez"), minisign.SignWithComments(priv, data, testTrusted, ""), "signature"},
		{"tampered prehashed data", pub, []byte("archive bytez"), prehashed(testTrusted), "signature"},
		{"other arch", pub, data, minisign.SignWithComments(priv, data, "lookout 0.2.0 lookout_linux_arm64.tar.gz", ""), "trusted comment"},
		{"older version", pub, data, minisign.SignWithComments(priv, data, "lookout 0.1.0 lookout_linux_amd64.tar.gz", ""), "trusted comment"},
		{"prehashed other version", pub, data, prehashed("lookout 0.1.0 lookout_linux_amd64.tar.gz"), "trusted comment"},
		{"other key", pub, data, minisign.SignWithComments(otherPriv, data, testTrusted, ""), "signature"},
		{"signed by expected key, verified with other", otherPub, data, minisign.SignWithComments(priv, data, testTrusted, ""), "signature"},
		{"forged trusted comment", pub, data, forgeTrustedComment(t, minisign.SignWithComments(priv, data, "lookout 0.1.0 lookout_linux_amd64.tar.gz", "")), "signature"},
		{"garbage signature", pub, data, []byte("not a signature"), "signature"},
		{"empty key", "", data, minisign.SignWithComments(priv, data, testTrusted, ""), "public key"},
		{"malformed key", "RWQnot-a-key", data, minisign.SignWithComments(priv, data, testTrusted, ""), "public key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifyArchive(tt.key, tt.data, tt.sig, testTrusted)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("VerifyArchive: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("VerifyArchive error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// forgeTrustedComment rewrites the trusted comment line of a valid signature
// to the expected one without re-signing it.
func forgeTrustedComment(t *testing.T, sig []byte) []byte {
	t.Helper()
	lines := strings.SplitN(string(sig), "\n", 4)
	if len(lines) != 4 || !strings.HasPrefix(lines[2], "trusted comment: ") {
		t.Fatalf("unexpected signature layout: %q", sig)
	}
	lines[2] = "trusted comment: " + testTrusted
	return []byte(strings.Join(lines, "\n"))
}

type tarEntry struct {
	name     string
	body     []byte
	typeflag byte
	linkname string
}

func makeArchive(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typeflag := e.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Typeflag: typeflag, Linkname: e.linkname, Mode: 0o755}
		if typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typeflag == tar.TypeReg {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractBinary(t *testing.T) {
	bin := []byte("\x7fELF new lookout binary")
	tests := []struct {
		name    string
		archive []byte
		maxSize int64
		wantErr string // "" means bin is returned
	}{
		{"binary and readme", makeArchive(t, tarEntry{name: "README.md", body: []byte("# lookout")}, tarEntry{name: "lookout", body: bin}), 1024, ""},
		{"no binary", makeArchive(t, tarEntry{name: "README.md", body: []byte("# lookout")}), 1024, "no \"lookout\" entry"},
		{"binary in a directory", makeArchive(t, tarEntry{name: "dir/lookout", body: bin}), 1024, "no \"lookout\" entry"},
		{"dot-slash binary", makeArchive(t, tarEntry{name: "./lookout", body: bin}), 1024, "no \"lookout\" entry"},
		{"path traversal", makeArchive(t, tarEntry{name: "../lookout", body: bin}), 1024, "no \"lookout\" entry"},
		{"binary over max size", makeArchive(t, tarEntry{name: "lookout", body: bin}), int64(len(bin) - 1), "limit"},
		{"other entry over max size", makeArchive(t, tarEntry{name: "README.md", body: bytes.Repeat([]byte("x"), 2048)}, tarEntry{name: "lookout", body: bin}), 1024, "limit"},
		{"binary is a symlink", makeArchive(t, tarEntry{name: "lookout", typeflag: tar.TypeSymlink, linkname: "/bin/sh"}), 1024, "not a regular file"},
		{"binary is a hard link", makeArchive(t, tarEntry{name: "lookout", typeflag: tar.TypeLink, linkname: "README.md"}), 1024, "not a regular file"},
		{"binary is a directory", makeArchive(t, tarEntry{name: "lookout", typeflag: tar.TypeDir}), 1024, "not a regular file"},
		{"duplicate binary", makeArchive(t, tarEntry{name: "lookout", body: bin}, tarEntry{name: "lookout", body: []byte("evil")}), 1024, "more than one"},
		{"not gzip", []byte("plain bytes"), 1024, "gzip"},
		{"truncated", makeArchive(t, tarEntry{name: "lookout", body: bytes.Repeat([]byte("x"), 900)})[:60], 1024, "archive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractBinary(tt.archive, "lookout", tt.maxSize)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ExtractBinary: %v", err)
				}
				if !bytes.Equal(got, bin) {
					t.Fatalf("ExtractBinary = %q, want %q", got, bin)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ExtractBinary error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestExtractBinaryCapsDecompressedSize(t *testing.T) {
	// Many entries each within maxSize: the total decompressed stream is
	// still bounded, so a gzip bomb cannot keep the agent busy.
	var entries []tarEntry
	for range 10 {
		entries = append(entries, tarEntry{name: "filler", body: bytes.Repeat([]byte{0}, 1000)})
	}
	entries = append(entries, tarEntry{name: "lookout", body: []byte("bin")})
	_, err := ExtractBinary(makeArchive(t, entries...), "lookout", 1024)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("ExtractBinary error = %v, want a size limit error", err)
	}
}
