package platform

import (
	"strings"
	"testing"
	"time"
)

func TestClampInterval(t *testing.T) {
	tests := []struct {
		name    string
		seconds int
		current time.Duration
		want    time.Duration
	}{
		{"in range", 60, 0, 60 * time.Second},
		{"lower bound", 10, 0, 10 * time.Second},
		{"upper bound", 3600, 0, time.Hour},
		{"below minimum", 1, 0, 10 * time.Second},
		{"above maximum", 99999, 0, time.Hour},
		{"zero keeps current", 0, 45 * time.Second, 45 * time.Second},
		{"negative keeps current", -3, 45 * time.Second, 45 * time.Second},
		{"zero without current uses default", 0, 0, 60 * time.Second},
		{"zero with out of range current clamps it", 0, time.Second, 10 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClampInterval(tc.seconds, tc.current); got != tc.want {
				t.Fatalf("ClampInterval(%d, %v) = %v, want %v", tc.seconds, tc.current, got, tc.want)
			}
		})
	}
}

func TestRedact(t *testing.T) {
	const (
		uuid   = "0e1f2a3b-0000-4000-8000-000000000001"
		secret = "Sup3rS3cretValueThatIsLongEnough"
		agent  = uuid + "." + secret
		enroll = "AbCdEfGhIjKlMnOpQrStUvWxYz012345"
	)
	tests := []struct {
		name    string
		in      string
		tokens  []string
		want    string
		notWant []string
	}{
		{"full agent token", "bad token " + agent, []string{agent}, "bad token [redacted]", nil},
		{"secret only", "echo: " + secret, []string{agent}, "echo: [redacted]", []string{secret}},
		{"uuid part is kept", "server " + uuid, []string{agent}, "server " + uuid, nil},
		{"enrollment token", "no such token " + enroll, []string{enroll}, "no such token [redacted]", nil},
		{"several tokens and repeats", enroll + " " + agent + " " + secret, []string{agent, enroll}, "[redacted] [redacted] [redacted]", nil},
		{"empty tokens are ignored", "plain text", []string{"", agent[:0]}, "plain text", nil},
		{"no tokens", "plain text", nil, "plain text", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.in, tc.tokens...)
			if got != tc.want {
				t.Fatalf("Redact = %q, want %q", got, tc.want)
			}
			for _, s := range tc.notWant {
				if strings.Contains(got, s) {
					t.Fatalf("Redact left %q in %q", s, got)
				}
			}
		})
	}
}
