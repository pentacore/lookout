package platform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/pentacore/lookout/internal/wire"
)

func TestEnrollRequest(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodPost || r.URL.Path != "/api/agent/v1/enroll" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "lookout/dev" {
			t.Errorf("User-Agent = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body["enrollment_token"] != "tok123" || body["hostname"] != "babel" {
			t.Errorf("body = %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"server_uuid":"u1","agent_token":"u1.secret","report_interval_seconds":60,"extra":true}`)
	}))
	defer srv.Close()

	resp, err := NewClient(srv.URL).Enroll(context.Background(), wire.EnrollRequest{EnrollmentToken: "tok123", Hostname: "babel"})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if !called {
		t.Fatal("server not called")
	}
	want := wire.EnrollResponse{ServerUUID: "u1", AgentToken: "u1.secret", ReportIntervalSeconds: 60}
	if resp != want {
		t.Fatalf("resp = %+v, want %+v", resp, want)
	}
}

func TestEnrollErrors(t *testing.T) {
	const enrollToken = "AbCdEfGhIjKlMnOpQrStUvWxYz012345"
	tests := []struct {
		name   string
		status int
		body   string
		check  func(t *testing.T, err error)
	}{
		{"410 gone", http.StatusGone, `{"message":"expired"}`, func(t *testing.T, err error) {
			if !errors.Is(err, ErrEnrollTokenGone) {
				t.Fatalf("err = %v, want ErrEnrollTokenGone", err)
			}
		}},
		{"404 unknown", http.StatusNotFound, `{"message":"unknown"}`, func(t *testing.T, err error) {
			if !errors.Is(err, ErrEnrollTokenUnknown) {
				t.Fatalf("err = %v, want ErrEnrollTokenUnknown", err)
			}
		}},
		{"500", http.StatusInternalServerError, "boom", func(t *testing.T, err error) {
			var se *StatusError
			if !errors.As(err, &se) || se.Code != 500 {
				t.Fatalf("err = %v, want *StatusError{500}", err)
			}
			if errors.Is(err, ErrEnrollTokenGone) || errors.Is(err, ErrEnrollTokenUnknown) {
				t.Fatalf("500 matched an enrollment sentinel")
			}
		}},
		{"201 expected, 200 is an error", http.StatusOK, `{}`, func(t *testing.T, err error) {
			var se *StatusError
			if !errors.As(err, &se) || se.Code != 200 {
				t.Fatalf("err = %v, want *StatusError{200}", err)
			}
		}},
		{"body echoing the enrollment token is redacted", http.StatusUnprocessableEntity, "bad " + enrollToken, func(t *testing.T, err error) {
			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v", err)
			}
			if strings.Contains(se.Body, enrollToken) || strings.Contains(err.Error(), enrollToken) {
				t.Fatalf("token leaked: body=%q err=%q", se.Body, err)
			}
		}},
		{"undecodable 201 body", http.StatusCreated, "not json", func(t *testing.T, err error) {
			var se *StatusError
			if err == nil || errors.As(err, &se) {
				t.Fatalf("err = %v, want a non-status decode error", err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			_, err := NewClient(srv.URL).Enroll(context.Background(), wire.EnrollRequest{EnrollmentToken: enrollToken})
			tc.check(t, err)
		})
	}
}

func TestReportRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/agent/v1/report" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer srv.secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "lookout/dev" {
			t.Errorf("User-Agent = %q", got)
		}
		var body wire.ReportRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ConfigVersion != 3 {
			t.Errorf("body = %+v, err = %v", body, err)
		}
		_, _ = io.WriteString(w, `{"config_version":3,"config":null,"report_interval_seconds":30,"server_time":"2026-10-22T12:00:00Z"}`)
	}))
	defer srv.Close()

	resp, err := NewClient(srv.URL+"/").Report(context.Background(), "srv.secret", wire.ReportRequest{Schema: 1, ConfigVersion: 3})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if resp.Config != nil || resp.ConfigVersion != 3 || resp.ReportIntervalSeconds != 30 {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestReportDecodesConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"config_version":4,"config":{"checks":[{"id":"c1","type":"mount_present","params":{"path":"/mnt"}}]},"report_interval_seconds":60,"server_time":"x"}`)
	}))
	defer srv.Close()
	resp, err := NewClient(srv.URL).Report(context.Background(), "a.b", wire.ReportRequest{})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if resp.Config == nil || len(resp.Config.Checks) != 1 || resp.Config.Checks[0].ID != "c1" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestReportStatusErrors(t *testing.T) {
	const token = "srv.TheSecretPart"
	date := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(http.TimeFormat) }
	tests := []struct {
		name       string
		status     int
		body       string
		retryAfter string
		wantBody   string
		minRetry   time.Duration
		maxRetry   time.Duration
	}{
		{name: "401", status: 401, body: `{"message":"Unauthenticated."}`, wantBody: `{"message":"Unauthenticated."}`},
		{name: "413", status: 413, body: "too big", wantBody: "too big"},
		{name: "422 body kept", status: 422, body: `{"errors":{"host":["bad"]}}`, wantBody: `{"errors":{"host":["bad"]}}`},
		{name: "422 body redacts token and secret", status: 422, body: "echo " + token + " and TheSecretPart", wantBody: "echo [redacted] and [redacted]"},
		{name: "429 delta seconds", status: 429, retryAfter: "120", minRetry: 120 * time.Second, maxRetry: 120 * time.Second},
		{name: "429 http date", status: 429, retryAfter: date(90 * time.Second), minRetry: 85 * time.Second, maxRetry: 90 * time.Second},
		{name: "429 without header", status: 429},
		{name: "503", status: 503, body: "down", wantBody: "down"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			_, err := NewClient(srv.URL).Report(context.Background(), token, wire.ReportRequest{})
			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want *StatusError", err)
			}
			if se.Code != tc.status || se.Body != tc.wantBody {
				t.Fatalf("got code=%d body=%q, want code=%d body=%q", se.Code, se.Body, tc.status, tc.wantBody)
			}
			if se.RetryAfter < tc.minRetry || se.RetryAfter > tc.maxRetry {
				t.Fatalf("RetryAfter = %v, want within [%v, %v]", se.RetryAfter, tc.minRetry, tc.maxRetry)
			}
			if strings.Contains(err.Error(), tc.body) && tc.body != "" {
				t.Fatalf("Error() contains the body: %q", err.Error())
			}
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Fatalf("Error() = %q, want the status code", err.Error())
			}
		})
	}
}

func TestStatusErrorBodyCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, strings.Repeat("x", 100_000))
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL).Report(context.Background(), "a.b", wire.ReportRequest{})
	var se *StatusError
	if !errors.As(err, &se) || len(se.Body) != 4096 {
		t.Fatalf("err = %v, body len = %d, want 4096", err, len(se.Body))
	}
}

func TestReportOversizedSuccessBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"server_time":"`+strings.Repeat("x", 2<<20)+`"}`)
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL).Report(context.Background(), "a.b", wire.ReportRequest{})
	if err == nil {
		t.Fatal("want an error for a body above 1 MiB")
	}
}

func TestReportDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	var gotAuth atomic.Value
	second := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotAuth.Store(r.Header.Get("Authorization"))
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/api/agent/v1/report", http.StatusMovedPermanently)
	}))
	defer first.Close()

	_, err := NewClient(first.URL).Report(context.Background(), "srv.secret", wire.ReportRequest{})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 301 || se.Location != second.URL+"/api/agent/v1/report" {
		t.Fatalf("err = %v, se = %+v", err, se)
	}
	if hits.Load() != 0 {
		t.Fatalf("redirect target was hit %d times (auth %v)", hits.Load(), gotAuth.Load())
	}
}

func TestEnrollDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL, http.StatusTemporaryRedirect)
	}))
	defer first.Close()
	_, err := NewClient(first.URL).Enroll(context.Background(), wire.EnrollRequest{})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 307 || hits.Load() != 0 {
		t.Fatalf("err = %v, hits = %d", err, hits.Load())
	}
}

func TestNewClientDefaults(t *testing.T) {
	c := NewClient("https://example.test")
	if c.HTTP.Timeout != 30*time.Second {
		t.Fatalf("Timeout = %v", c.HTTP.Timeout)
	}
	if c.HTTP.CheckRedirect == nil {
		t.Fatal("CheckRedirect not set")
	}
}

func TestReportContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewClient(srv.URL).Report(ctx, "a.b", wire.ReportRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestStatusErrorBodyCutOnRuneBoundary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "x"+strings.Repeat("é", 5000))
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL).Report(context.Background(), "a.b", wire.ReportRequest{})
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want a *StatusError", err)
	}
	if len(se.Body) > 4096 || len(se.Body) < 4090 || !utf8.ValidString(se.Body) {
		t.Fatalf("body len = %d, valid UTF-8 = %v; want at most 4096 bytes of valid UTF-8", len(se.Body), utf8.ValidString(se.Body))
	}
}

// The Location header is platform-supplied text that ends up in logs and on
// the Unraid page: it is capped and stripped of control characters.
func TestStatusErrorLocationSanitized(t *testing.T) {
	tests := []struct {
		name, location, want string
	}{
		{"plain", "https://other.example/api", "https://other.example/api"},
		{"control characters", "https://x.example/\x1b[31mred\r\n\x00", "https://x.example/[31mred"},
		{"long", "https://x.example/" + strings.Repeat("é", 300), "https://x.example/" + strings.Repeat("é", 91) + "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{tt.location}}}
			se := newStatusError(resp, nil, nil)
			if se.Location != tt.want {
				t.Fatalf("Location = %q, want %q", se.Location, tt.want)
			}
			if !strings.Contains(se.Error(), tt.want) || !utf8.ValidString(se.Error()) {
				t.Fatalf("Error() = %q", se.Error())
			}
		})
	}
}
