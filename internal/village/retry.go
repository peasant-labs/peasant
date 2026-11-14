package village

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryPolicy bounds how VillageClient re-sends a publish after a transient
// failure. A publish is idempotent by operation fingerprint on the village, so
// re-sending the held request bytes is safe: a replay is answered with the
// receipt the village already holds.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts, including the first.
	MaxAttempts int
	// BaseDelay is the backoff before the first retry; each later retry doubles it.
	BaseDelay time.Duration
	// MaxDelay caps the exponential backoff.
	MaxDelay time.Duration
	// MaxRetryAfter is the longest Retry-After the client honors. A longer
	// server-requested wait ends the retries and returns the failure.
	MaxRetryAfter time.Duration
	// Sleep waits for d or until ctx ends.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now resolves an HTTP-date Retry-After.
	Now func() time.Time
}

// DefaultRetryPolicy is the production publish retry policy.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:   4,
		BaseDelay:     500 * time.Millisecond,
		MaxDelay:      8 * time.Second,
		MaxRetryAfter: 30 * time.Second,
		Sleep:         sleepContext,
		Now:           time.Now,
	}
}

// SetRetryPolicy replaces the publish retry policy. It must be configured
// before the client is used concurrently.
func (c *VillageClient) SetRetryPolicy(policy RetryPolicy) {
	c.retry = policy
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// attemptOutcome is what one publish attempt tells the retry loop.
type attemptOutcome struct {
	retryable     bool
	retryAfter    time.Duration
	hasRetryAfter bool
}

func (p RetryPolicy) backoff(retry int) time.Duration {
	delay := p.BaseDelay
	for i := 1; i < retry && delay < p.MaxDelay; i++ {
		delay *= 2
	}
	return min(delay, p.MaxDelay)
}

// delay returns how long to wait before retry number `retry`, and false when
// the outcome must not be retried.
func (p RetryPolicy) delay(retry int, outcome attemptOutcome) (time.Duration, bool) {
	if !outcome.retryable {
		return 0, false
	}
	if outcome.hasRetryAfter {
		if outcome.retryAfter > p.MaxRetryAfter {
			return 0, false
		}
		return outcome.retryAfter, true
	}
	return p.backoff(retry), true
}

// retryableStatus reports the HTTP statuses that signal a transient condition:
// request timeout, rate limiting, and gateway or availability failures. A 400,
// 413 or 422 is a property of the request and is never retried.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// retryableTransportError reports a connection-level failure: a reset, refused
// or dropped connection, a phase timeout, or a stalled transfer. A TLS
// verification failure or a malformed request is not transient.
func retryableTransportError(err error) bool {
	if errors.Is(err, errTransferStalled) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// parseRetryAfter reads a Retry-After header as delta-seconds or an HTTP-date.
func parseRetryAfter(header http.Header, now time.Time) (time.Duration, bool) {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	at, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return max(at.Sub(now), 0), true
}
