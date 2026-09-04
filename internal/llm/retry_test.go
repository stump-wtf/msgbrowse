package llm

// Tests for transient-failure retry (issue #452): a gateway 502 twice then
// 200 must succeed on the third attempt; a 400 must fail on the first attempt
// with no retries; the backoff delays must respect the ceiling. A fake
// httptest server drives the real client, no sleeps beyond the configured
// microsecond-scale delays.
//
// @joestump-agent 09/04/2026 - Added with #452.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRetryRecoversFromTransient502(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,2],"index":0}]}`))
	}))
	defer srv.Close()

	c := New(Options{
		BaseURL:    srv.URL,
		EmbedModel: "m",
		Retry:      &RetryConfig{Attempts: 3, BaseDelay: time.Microsecond, MaxDelay: 2 * time.Microsecond},
	})
	out, err := c.Embed(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatalf("embed after retries: %v", err)
	}
	if len(out) != 1 || len(out[0]) != 2 {
		t.Fatalf("unexpected embedding: %v", out)
	}
	if calls != 3 {
		t.Fatalf("got %d calls, want 3 (two 502s then success)", calls)
	}
}

func TestRetryDoesNotRetryFatal400(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid model"}`))
	}))
	defer srv.Close()

	c := New(Options{
		BaseURL:    srv.URL,
		EmbedModel: "m",
		Retry:      &RetryConfig{Attempts: 3, BaseDelay: time.Microsecond, MaxDelay: 2 * time.Microsecond},
	})
	_, err := c.Embed(context.Background(), []string{"hello"})
	if err == nil {
		t.Fatal("400 must fail")
	}
	if calls != 1 {
		t.Fatalf("got %d calls, want 1 — a 400 is fatal on the first attempt", calls)
	}
}

func TestRetryGivesUpAfterAttempts(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(Options{
		BaseURL:    srv.URL,
		EmbedModel: "m",
		Retry:      &RetryConfig{Attempts: 3, BaseDelay: time.Microsecond, MaxDelay: 2 * time.Microsecond},
	})
	if _, err := c.Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("persistent 503 must fail")
	}
	if calls != 3 {
		t.Fatalf("got %d calls, want 3 (the configured attempt count)", calls)
	}
}

func TestRetryDelaysRespectCeiling(t *testing.T) {
	r := RetryConfig{Attempts: 10, BaseDelay: time.Second, MaxDelay: 30 * time.Second}.withDefaults()
	// Early failures: base doubling per attempt, ±25% jitter.
	for failure, want := range map[int]time.Duration{0: r.BaseDelay, 1: 2 * r.BaseDelay} {
		d := r.delay(failure)
		if d < want*3/4 || d > want*5/4 {
			t.Fatalf("delay %v after failure %d outside %v±25%% jitter", d, failure, want)
		}
	}
	// Saturated failures: at (or jittered just under) the ceiling, never over.
	for failure := 5; failure < 12; failure++ {
		d := r.delay(failure)
		if d > r.MaxDelay || d < r.MaxDelay*3/4 {
			t.Fatalf("delay %v after failure %d outside ceiling±25%% jitter", d, failure)
		}
	}
}

func TestRetryableClassification(t *testing.T) {
	for _, code := range []int{429, 502, 503, 504} {
		if !retryable(&statusError{code: code}) {
			t.Errorf("status %d should be retryable", code)
		}
	}
	for _, code := range []int{400, 401, 403, 404, 500} {
		if retryable(&statusError{code: code}) {
			t.Errorf("status %d should NOT be retryable", code)
		}
	}
	if retryable(nil) {
		t.Error("nil must not be retryable")
	}
	if retryable(context.Canceled) {
		t.Error("cancellation must never be retried")
	}
	if retryable(errors.New("plain")) {
		t.Error("unknown errors must not be retried")
	}
}
