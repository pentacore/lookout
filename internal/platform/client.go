// Package platform is the agent's HTTP client for the media-manager platform
// API (enroll and report) and the retry policy that goes with it.
package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pentacore/lookout/internal/buildinfo"
	"github.com/pentacore/lookout/internal/wire"
)

const (
	requestTimeout = 30 * time.Second
	maxResponse    = 1 << 20 // successful response bodies
	maxErrorBody   = 4 << 10 // StatusError.Body
	maxLocation    = 200     // StatusError.Location
	apiPath        = "/api/agent/v1"
)

var (
	// ErrEnrollTokenGone means the enrollment token expired or was already used (410).
	ErrEnrollTokenGone = errors.New("enrollment token expired or already used")
	// ErrEnrollTokenUnknown means the platform does not know the enrollment token (404).
	ErrEnrollTokenUnknown = errors.New("enrollment token unknown")
)

// StatusError is an unexpected HTTP status from the platform. Error() leaves
// the body out; Body has the agent and enrollment tokens redacted.
type StatusError struct {
	Code       int
	Body       string // at most 4 KiB
	RetryAfter time.Duration
	Location   string // at most 200 bytes, control characters removed
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("platform returned HTTP %d", e.Code)
	if e.Location != "" {
		msg += " redirecting to " + e.Location
	}
	return msg
}

// Client talks to the platform API. Redirects are never followed, so the
// bearer token cannot be sent to another host.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// NewClient returns a Client for baseURL (the platform URL, with or without a
// trailing slash).
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTP: &http.Client{
			Timeout: requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Enroll exchanges an enrollment token for agent credentials. A 410 or 404
// answer wraps ErrEnrollTokenGone or ErrEnrollTokenUnknown (and the
// *StatusError); any other unexpected status is a *StatusError.
func (c *Client) Enroll(ctx context.Context, req wire.EnrollRequest) (wire.EnrollResponse, error) {
	var resp wire.EnrollResponse
	err := c.post(ctx, "/enroll", "", req, http.StatusCreated, &resp, req.EnrollmentToken)
	var se *StatusError
	if errors.As(err, &se) {
		switch se.Code {
		case http.StatusGone:
			err = fmt.Errorf("%w: %w", ErrEnrollTokenGone, se)
		case http.StatusNotFound:
			err = fmt.Errorf("%w: %w", ErrEnrollTokenUnknown, se)
		}
	}
	return resp, err
}

// Report sends one report with the agent token. A status other than 200 is a
// *StatusError.
func (c *Client) Report(ctx context.Context, token string, req wire.ReportRequest) (wire.ReportResponse, error) {
	var resp wire.ReportResponse
	err := c.post(ctx, "/report", token, req, http.StatusOK, &resp, token)
	return resp, err
}

func (c *Client) post(ctx context.Context, path, bearer string, body any, wantStatus int, out any, secrets ...string) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	url := strings.TrimRight(c.BaseURL, "/") + apiPath + path
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", buildinfo.UserAgent())
	if bearer != "" {
		httpReq.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != wantStatus {
		return newStatusError(resp, data, secrets)
	}
	if len(data) > maxResponse {
		return fmt.Errorf("response body exceeds %d bytes", maxResponse)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func newStatusError(resp *http.Response, data []byte, secrets []string) *StatusError {
	text := Redact(string(data[:min(len(data), maxResponse)]), secrets...)
	location := printable(Redact(resp.Header.Get("Location"), secrets...))
	if len(location) > maxLocation {
		location = cutUTF8(location, maxLocation) + "…"
	}
	return &StatusError{
		Code:       resp.StatusCode,
		Body:       cutUTF8(text, maxErrorBody),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		Location:   location,
	}
}
