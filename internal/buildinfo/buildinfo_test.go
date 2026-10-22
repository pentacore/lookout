package buildinfo_test

import (
	"testing"

	"github.com/pentacore/lookout/internal/buildinfo"
)

func TestUserAgent(t *testing.T) {
	if got := buildinfo.UserAgent(); got != "lookout/dev" {
		t.Fatalf("got %q", got)
	}
}

func TestDefaults(t *testing.T) {
	if buildinfo.Version != "dev" {
		t.Fatalf("Version = %q", buildinfo.Version)
	}
	if buildinfo.MinisignPublicKey != "" {
		t.Fatalf("MinisignPublicKey = %q", buildinfo.MinisignPublicKey)
	}
}
