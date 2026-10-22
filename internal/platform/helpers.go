package platform

import (
	"strings"
	"time"
)

const (
	minInterval     = 10 * time.Second
	maxInterval     = time.Hour
	defaultInterval = 60 * time.Second

	redacted = "[redacted]"
)

// ClampInterval turns a report_interval_seconds value from the enroll
// response, the saved config or a report response into a report interval
// within 10 s to 1 h. A value of zero or less is ignored: current is kept (and
// clamped), or 60 s when current is unset.
func ClampInterval(seconds int, current time.Duration) time.Duration {
	d := time.Duration(seconds) * time.Second
	if seconds <= 0 {
		d = current
	}
	if d <= 0 {
		d = defaultInterval
	}
	return min(max(d, minInterval), maxInterval)
}

// Redact replaces every occurrence of each token in text with "[redacted]".
// For an agent token of the form "{server_uuid}.{secret}" the secret part is
// replaced as well, so a body echoing only the secret is covered. Use it
// before logging or embedding any platform-supplied text.
func Redact(text string, tokens ...string) string {
	for _, token := range tokens {
		if token == "" {
			continue
		}
		text = strings.ReplaceAll(text, token, redacted)
		if _, secret, ok := strings.Cut(token, "."); ok && secret != "" {
			text = strings.ReplaceAll(text, secret, redacted)
		}
	}
	return text
}
