package llm

// Transient-failure retry for the OpenAI-compatible client (issue #452). One
// 502 from the gateway used to abort a multi-hour embed/journal/sentiment
// run; the pass resumes from its cursor, but every restart was manual. The
// client now retries retryable failures — 429 / 502 / 503 / 504 and client
// timeouts — with exponential backoff and jitter before returning the error.
// Any other 4xx stays fatal on the first attempt: a 400 "invalid model" is a
// configuration mistake, and retrying it just burns hours.
//
// Governing: epic #431 / issue #452.
//
// @joestump-agent 09/04/2026 - Added with #452.

import (
	"context"
	"errors"
	"math/rand"
	"net"
	"net/http"
	"time"
)

// RetryConfig bounds the client's per-request retry behavior. The zero value
// is valid and means the defaults: 3 total attempts (2 tries after the
// first failure), 2s base delay, 30s ceiling.
type RetryConfig struct {
	// Attempts is the TOTAL number of attempts, including the first. 1
	// disables retrying entirely.
	Attempts int
	// BaseDelay is the pre-jitter delay after the first failure; it doubles
	// per attempt (2s → 4s → 8s …).
	BaseDelay time.Duration
	// MaxDelay caps the computed delay.
	MaxDelay time.Duration
}

// DefaultRetry is the sane default the client applies when Options.Retry is
// nil: three attempts at 2s → 4s → 8s, capped at 30s.
func DefaultRetry() RetryConfig {
	return RetryConfig{Attempts: 3, BaseDelay: 2 * time.Second, MaxDelay: 30 * time.Second}
}

func (r RetryConfig) withDefaults() RetryConfig {
	if r.Attempts <= 0 {
		r.Attempts = 3
	}
	if r.BaseDelay <= 0 {
		r.BaseDelay = 2 * time.Second
	}
	if r.MaxDelay <= 0 {
		r.MaxDelay = 30 * time.Second
	}
	return r
}

// delay computes the wait before attempt n (0-based failure count): base ×
// 2^n with ±25% jitter, capped at MaxDelay AFTER jitter so the ceiling is
// never exceeded.
func (r RetryConfig) delay(failure int) time.Duration {
	d := r.BaseDelay << failure // overflow-safe for any sane attempt count
	jitter := d / 4
	d = d - jitter + time.Duration(rand.Int63n(int64(2*jitter+1)))
	if d <= 0 || d > r.MaxDelay {
		d = r.MaxDelay
	}
	return d
}

// retryable reports whether an error from do() is worth retrying: 429 rate
// limiting and the transient gateway statuses (502/503/504), plus client-side
// timeouts (net.Error with Timeout, including http.Client's budget and
// context deadline exceeded) — the exact failure that killed the v0.8.3 runs
// mid-pass. Cancellation (the user stopping the run) is NEVER retried.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	var se *statusError
	if errors.As(err, &se) {
		switch se.code {
		case http.StatusTooManyRequests, http.StatusBadGateway,
			http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}
