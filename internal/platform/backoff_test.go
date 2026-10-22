package platform

import (
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"
)

func TestBackoffNext(t *testing.T) {
	const interval = 60 * time.Second
	serverErr := &StatusError{Code: 503}

	t.Run("nil returns the interval", func(t *testing.T) {
		b := &Backoff{Rand: func() float64 { return 0 }}
		if got := b.Next(nil, interval); got != interval {
			t.Fatalf("Next(nil) = %v, want %v", got, interval)
		}
	})

	t.Run("exponential bounds", func(t *testing.T) {
		steps := []struct {
			attempt  int
			lo, high time.Duration
		}{
			{1, 2500 * time.Millisecond, 5 * time.Second},
			{2, 5 * time.Second, 10 * time.Second},
			{3, 10 * time.Second, 20 * time.Second},
			{7, 150 * time.Second, 300 * time.Second},
			{10, 150 * time.Second, 300 * time.Second},
		}
		for _, rnd := range []float64{0, 1} {
			b := &Backoff{Rand: func() float64 { return rnd }}
			n := 0
			for _, s := range steps {
				for n < s.attempt-1 {
					b.Next(serverErr, interval)
					n++
				}
				got := b.Next(serverErr, interval)
				n++
				want := s.lo
				if rnd == 1 {
					want = s.high
				}
				if got != want {
					t.Errorf("rand=%v attempt %d = %v, want %v", rnd, s.attempt, got, want)
				}
			}
		}
	})

	t.Run("nil resets the attempt counter", func(t *testing.T) {
		b := &Backoff{Rand: func() float64 { return 0 }}
		for range 5 {
			b.Next(serverErr, interval)
		}
		b.Next(nil, interval)
		if got := b.Next(serverErr, interval); got != 2500*time.Millisecond {
			t.Fatalf("after reset = %v, want 2.5s", got)
		}
	})

	t.Run("many attempts do not overflow", func(t *testing.T) {
		b := &Backoff{Rand: func() float64 { return 1 }}
		var got time.Duration
		for range 500 {
			got = b.Next(serverErr, interval)
		}
		if got != 5*time.Minute {
			t.Fatalf("got %v, want 5m", got)
		}
	})

	t.Run("nil Rand falls back to a real source", func(t *testing.T) {
		b := &Backoff{}
		got := b.Next(serverErr, interval)
		if got < 2500*time.Millisecond || got > 5*time.Second {
			t.Fatalf("got %v, want within [2.5s, 5s]", got)
		}
	})

	policy := []struct {
		name string
		err  error
		want time.Duration // 0 means "exponential, first attempt: 2.5s with Rand 0"
	}{
		{"401", &StatusError{Code: 401}, 15 * time.Minute},
		{"422", &StatusError{Code: 422}, interval},
		{"413", &StatusError{Code: 413}, interval},
		{"404", &StatusError{Code: 404}, interval},
		{"429 with Retry-After", &StatusError{Code: 429, RetryAfter: 120 * time.Second}, 120 * time.Second},
		{"429 Retry-After above cap", &StatusError{Code: 429, RetryAfter: 2 * time.Hour}, time.Hour},
		{"429 without Retry-After", &StatusError{Code: 429}, 0},
		{"301", &StatusError{Code: 301, Location: "http://elsewhere/"}, 0},
		{"500", &StatusError{Code: 500}, 0},
		{"network", &url.Error{Op: "Post", URL: "http://x", Err: errors.New("refused")}, 0},
		{"wrapped status", fmt.Errorf("report: %w", &StatusError{Code: 401}), 15 * time.Minute},
		{"plain error", errors.New("decode failed"), 0},
	}
	for _, tc := range policy {
		t.Run(tc.name, func(t *testing.T) {
			b := &Backoff{Rand: func() float64 { return 0 }}
			want := tc.want
			if want == 0 {
				want = 2500 * time.Millisecond
			}
			if got := b.Next(tc.err, interval); got != want {
				t.Fatalf("Next(%v) = %v, want %v", tc.err, got, want)
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"seconds", "120", 120 * time.Second},
		{"padded seconds", " 7 ", 7 * time.Second},
		{"zero", "0", 0},
		{"negative", "-5", 0},
		{"http date gmt", now.Add(90 * time.Second).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"), 90 * time.Second},
		{"past date", now.Add(-time.Minute).UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"), 0},
		{"garbage", "soon", 0},
		{"empty", "", 0},
		{"huge", "99999999999999999999", maxParsedRetryAfter},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfter(tc.in, now); got != tc.want {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
