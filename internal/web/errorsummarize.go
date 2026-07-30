package web

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SummarizeEmbedError converts a raw embedding pipeline error string into a
// one-sentence human-readable summary. It parses provider JSON error shapes
// (LiteLLM wrappers, OpenAI-style errors, vLLM context-window rejections) and
// falls back to the raw string (truncated) when parsing fails.
//
// The summary is plain text — it passes through html/template escaping at the
// call site, never as pre-encoded HTML.
func SummarizeEmbedError(raw string) string {
	if raw == "" {
		return ""
	}

	// Try to parse as a provider error JSON. The llm.OpenAIClient.do() method
	// formats non-2xx responses as "llm: /path returned CODE: BODY" where BODY
	// may be JSON. Extract the body and try to parse it.
	var providerErr struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}

	// Extract the JSON body from the "llm: /path returned CODE: ..." wrapper.
	body := extractJSONBody(raw)
	if body != "" && json.Unmarshal([]byte(body), &providerErr) == nil {
		msg := providerErr.Error.Message
		if msg == "" {
			msg = providerErr.Error.Type
		}
		if msg == "" {
			return truncate(raw, 200)
		}
		return classifyErrorMessage(msg)
	}

	// Not JSON — return the raw string, truncated.
	return truncate(raw, 200)
}

// extractJSONBody pulls the JSON payload out of the "llm: /path returned CODE:
// {json}" format the OpenAIClient produces, or returns the original string if
// it doesn't match that pattern.
func extractJSONBody(s string) string {
	// Look for the first '{' — if the string contains JSON, it starts there.
	idx := strings.Index(s, "{")
	if idx < 0 {
		return ""
	}
	return s[idx:]
}

// classifyErrorMessage maps known error message patterns to human-readable
// summaries.
func classifyErrorMessage(msg string) string {
	lower := strings.ToLower(msg)

	if strings.Contains(lower, "contextwindowexceedederror") ||
		strings.Contains(lower, "maximum context length") {
		return "The embedding model's context window is too small for the batch. Try reducing the embed batch size."
	}

	if strings.Contains(lower, "rate_limit") || strings.Contains(lower, "rate limit") {
		return "The embedding endpoint rate-limited the request. Try again or reduce concurrency."
	}

	if strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded") {
		return "The embedding endpoint timed out. Check that the model is running and responsive."
	}

	if strings.Contains(lower, "connection refused") || strings.Contains(lower, "no such host") {
		return "Could not connect to the embedding endpoint. Check that the URL is correct and the service is running."
	}

	if strings.Contains(lower, "unauthorized") || strings.Contains(lower, "401") {
		return "The embedding endpoint rejected the API key. Check the key on the LLM settings tab."
	}

	if strings.Contains(lower, "model not found") || strings.Contains(lower, "404") {
		return "The embedding model name was not found on the endpoint. Check the model name on the LLM settings tab."
	}

	// Unknown error — return the innermost message, truncated.
	return truncate(msg, 200)
}

// truncate clips a string to maxChars, adding an ellipsis if truncated.
func truncate(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "…"
}

// ErrorSummary is the structured output for template rendering: a one-sentence
// summary plus the full raw error for the <details> disclosure.
type ErrorSummary struct {
	Summary string
	Raw     string
}

// NewErrorSummary builds an ErrorSummary from a raw error string.
func NewErrorSummary(raw string) ErrorSummary {
	return ErrorSummary{
		Summary: SummarizeEmbedError(raw),
		Raw:     raw,
	}
}

// String returns the summary for fmt.Sprintf compatibility.
func (e ErrorSummary) String() string {
	return e.Summary
}

// GoString implements fmt.GoStringer for debugging.
func (e ErrorSummary) GoString() string {
	return fmt.Sprintf("ErrorSummary(summary=%q)", e.Summary)
}
