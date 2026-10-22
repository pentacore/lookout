package platform

import (
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	unauthorizedDelay = 15 * time.Minute
	maxRetryAfter     = time.Hour
	backoffBase       = 5 * time.Second
	backoffCap        = 5 * time.Minute

	// maxParsedRetryAfter keeps absurd header values from overflowing a
	// Duration; Next caps the wait at maxRetryAfter anyway.
	maxParsedRetryAfter = 30 * 24 * time.Hour
	maxParsedSeconds    = int64(maxParsedRetryAfter / time.Second)
)

// Backoff decides how long to wait after each platform call. It is not safe
// for concurrent use.
type Backoff struct {
	// Rand returns a value in [0, 1) for jitter; nil uses math/rand/v2.
	Rand     func() float64
	attempts int
}

// Next returns the delay before the next call given the outcome of the last
// one (nil on success) and the normal report interval.
//
// Success resets the failure count and returns interval. A 401 waits 15
// minutes. Other 4xx responses (413, 422, ...) keep interval, since retrying
// sooner or later does not help. A 429 with Retry-After waits that long, up to
// an hour. Anything else (429 without Retry-After, redirects, 5xx, network and
// decode errors) backs off exponentially from 5 s to 5 min with equal jitter.
func (b *Backoff) Next(err error, interval time.Duration) time.Duration {
	if err == nil {
		b.attempts = 0
		return interval
	}
	var se *StatusError
	if errors.As(err, &se) {
		switch {
		case se.Code == http.StatusUnauthorized:
			return unauthorizedDelay
		case se.Code == http.StatusTooManyRequests && se.RetryAfter > 0:
			return min(se.RetryAfter, maxRetryAfter)
		case se.Code == http.StatusTooManyRequests:
		case se.Code >= 400 && se.Code < 500:
			return interval
		}
	}
	return b.exponential()
}

func (b *Backoff) exponential() time.Duration {
	b.attempts++
	base := backoffCap
	if shift := b.attempts - 1; shift < 6 {
		base = min(backoffBase<<shift, backoffCap)
	}
	rnd := rand.Float64
	if b.Rand != nil {
		rnd = b.Rand
	}
	return base/2 + time.Duration(rnd()*float64(base/2))
}

// parseRetryAfter reads a Retry-After header value, either delta-seconds or
// an HTTP-date, relative to now. Missing, malformed and past values give 0.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	secs, err := strconv.ParseInt(v, 10, 64)
	switch {
	case err == nil && secs > 0:
		return min(time.Duration(min(secs, maxParsedSeconds))*time.Second, maxParsedRetryAfter)
	case err == nil:
		return 0
	case errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(v, "-"):
		return maxParsedRetryAfter
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(t.Sub(now), 0), maxParsedRetryAfter)
	}
	return 0
}
