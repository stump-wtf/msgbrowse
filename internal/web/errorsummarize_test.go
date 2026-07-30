package web

import (
	"strings"
	"testing"
)

func TestSummarizeEmbedError(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantSub string
	}{
		{
			name:    "empty",
			raw:     "",
			wantSub: "",
		},
		{
			name:    "litellm context window",
			raw:     `llm: /v1/embeddings returned 400: {"error":{"message":"litellm.ContextWindowExceededError: maximum context length is 8192 tokens"}}`,
			wantSub: "context window",
		},
		{
			name:    "rate limit",
			raw:     `llm: /v1/embeddings returned 429: {"error":{"message":"Rate limit exceeded"}}`,
			wantSub: "rate-limited",
		},
		{
			name:    "timeout",
			raw:     `llm: /v1/embeddings returned 500: {"error":{"message":"Request timeout"}}`,
			wantSub: "timed out",
		},
		{
			name:    "connection refused",
			raw:     `llm: request to /v1/embeddings: dial tcp: connection refused`,
			wantSub: "connection refused",
		},
		{
			name:    "unauthorized",
			raw:     `llm: /v1/embeddings returned 401: {"error":{"message":"Unauthorized"}}`,
			wantSub: "API key",
		},
		{
			name:    "plain text error",
			raw:     `some random error that is not JSON`,
			wantSub: "some random error",
		},
		{
			name:    "long error gets truncated",
			raw:     strings.Repeat("x", 300),
			wantSub: "…",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SummarizeEmbedError(c.raw)
			if c.wantSub == "" {
				if got != "" {
					t.Errorf("expected empty summary, got %q", got)
				}
				return
			}
			if !strings.Contains(strings.ToLower(got), strings.ToLower(c.wantSub)) {
				t.Errorf("summary %q does not contain %q", got, c.wantSub)
			}
		})
	}
}
