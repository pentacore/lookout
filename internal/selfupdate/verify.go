package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strings"

	"aead.dev/minisign"
)

// VerifyArchive checks that sig is a valid minisign signature of archive by
// publicKey (legacy Ed or prehashed ED) and that its signed trusted comment
// is exactly expectedTrusted. The trusted comment binds the signature to one
// version and asset, so an older release or another arch's archive served
// under this release's name is rejected.
func VerifyArchive(publicKey string, archive, sig []byte, expectedTrusted string) error {
	var key minisign.PublicKey
	if err := key.UnmarshalText([]byte(strings.TrimSpace(publicKey))); err != nil {
		return fmt.Errorf("parse public key: %w", err)
	}
	if !minisign.Verify(key, archive, sig) {
		return errors.New("signature verification failed")
	}
	var s minisign.Signature
	if err := s.UnmarshalText(sig); err != nil {
		return fmt.Errorf("parse signature: %w", err)
	}
	if s.TrustedComment != expectedTrusted {
		return fmt.Errorf("trusted comment %q does not match %q", truncate(s.TrustedComment), expectedTrusted)
	}
	return nil
}

// ExtractBinary returns the contents of the regular-file entry called
// exactly name in a gzipped tar archive. Nothing is written to disk. It fails
// when the entry is missing, duplicated or not a regular file (links
// included), when any entry is larger than maxSize, or when the archive
// decompresses to more than 4×maxSize.
func ExtractBinary(archive []byte, name string, maxSize int64) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open gzip archive: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(&capReader{r: gz, left: 4 * maxSize})

	var bin []byte
	found := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if hdr.Size > maxSize {
			return nil, fmt.Errorf("archive entry %q is %d bytes, over the %d byte limit", truncate(hdr.Name), hdr.Size, maxSize)
		}
		if hdr.Name != name {
			continue
		}
		if found {
			return nil, fmt.Errorf("archive has more than one %q entry", name)
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("archive entry %q is not a regular file", name)
		}
		bin, err = io.ReadAll(io.LimitReader(tr, maxSize))
		if err != nil {
			return nil, fmt.Errorf("read archive entry %q: %w", name, err)
		}
		found = true
	}
	if !found {
		return nil, fmt.Errorf("archive has no %q entry", name)
	}
	return bin, nil
}

// capReader fails once more than left bytes have been read, so a
// decompression bomb ends in an error instead of a silent truncation.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errors.New("archive exceeds the decompressed size limit")
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// truncate shortens untrusted text before it goes into an error message.
func truncate(s string) string {
	const maxLen = 200
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}
