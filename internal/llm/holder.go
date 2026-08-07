// The live-settings seam (issue #191): a thread-safe, swappable LLM provider
// so the Settings → LLM tab can repoint the app's single egress endpoint with
// NO restart. Consumers (the MCP server's semantic search today; facts and the
// journal digest as they land on desktop) hold one *Holder for the process
// lifetime and read the CURRENT client + model names through it per call; a
// save swaps the client behind the same handle.
package llm

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"
)

// Settings are the user-configurable LLM endpoint values the Settings → LLM
// tab edits: the base URL, the two model names, and the API key. The key is
// editable from the tab and persisted to the config file (a deliberate
// product choice — a desktop user has no convenient env var; the config file
// is 0600, ADR-0010's loopback single-user trust).
type Settings struct {
	// BaseURL is the OpenAI-compatible endpoint — the only network egress
	// msgbrowse performs (ADR-0010).
	BaseURL string
	// EmbedModel embeds messages and queries for semantic search. Empty means
	// semantic search is off.
	EmbedModel string
	// ChatModel is the completion model ("Facts model" in the UI: the facts
	// feature consumes it today, the journal digest later).
	ChatModel string
	// APIKey authenticates to a keyed endpoint. Empty for a local proxy that
	// needs none. Held in memory and persisted to the config file; it is never
	// rendered back into the tab's HTML (the form shows only whether one is set).
	APIKey string
	// APIKeyFromEnv reports that the effective key came from the
	// MSGBROWSE_LLM_API_KEY environment variable rather than the config file or
	// the Settings tab. When true the key is used LIVE but is NEVER written to
	// the config file on save — persisting an env-provided secret to disk would
	// leak it out of the environment the operator deliberately scoped it to
	// (ADR-0009). It flips to false the moment the user types a key into the tab
	// (an explicit choice to store it, Option A).
	APIKeyFromEnv bool
}

// Holder is a swappable Client: it implements the Client interface by
// delegating every call to the client it currently holds, and Swap replaces
// that client (plus the Settings it was built from) atomically. All methods
// are safe for concurrent use, so in-flight requests finish against the old
// client while new calls see the new one — the live-apply contract of the
// Settings → LLM tab.
type Holder struct {
	mu       sync.RWMutex
	client   Client
	settings Settings
}

// NewHolder wraps client (which must be non-nil) and the settings it was
// built from.
func NewHolder(client Client, s Settings) *Holder {
	return &Holder{client: client, settings: s}
}

// Swap atomically replaces the held client and settings. In-flight calls on
// the previous client are unaffected; every subsequent call goes to client.
func (h *Holder) Swap(client Client, s Settings) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.client = client
	h.settings = s
}

// Settings returns the settings the current client was built from.
func (h *Holder) Settings() Settings {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.settings
}

// EmbedModel returns the CURRENT embedding model name — the per-call getter
// the MCP server reads so semantic search follows a live settings swap
// (mcp.Options.EmbedModelFunc). Empty means semantic search is off.
func (h *Holder) EmbedModel() string { return h.Settings().EmbedModel }

// ChatModel returns the current completion-model name.
func (h *Holder) ChatModel() string { return h.Settings().ChatModel }

// current snapshots the held client under the read lock.
func (h *Holder) current() Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.client
}

// Embed implements Client by delegating to the current client.
func (h *Holder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	return h.current().Embed(ctx, inputs)
}

// Chat implements Client by delegating to the current client.
func (h *Holder) Chat(ctx context.Context, req ChatRequest) (string, error) {
	return h.current().Chat(ctx, req)
}

// Transcribe implements Client by delegating to the current client.
func (h *Holder) Transcribe(ctx context.Context, audio []byte, filename string) (string, error) {
	return h.current().Transcribe(ctx, audio, filename)
}

// Vision implements Client by delegating to the current client.
func (h *Holder) Vision(ctx context.Context, image []byte, mimeType, prompt string) (string, error) {
	return h.current().Vision(ctx, image, mimeType, prompt)
}

// ListModels implements Client by delegating to the current client.
func (h *Holder) ListModels(ctx context.Context) ([]string, error) {
	return h.current().ListModels(ctx)
}

// Applier binds a Holder to a persistence function: it is the object the web
// layer's Settings → LLM tab drives (web.LLMConfigurator). ApplyLLM persists
// the settings FIRST and only then swaps the live client, so a failed write
// leaves the running provider untouched and the page can report the error
// honestly.
//
// timeout is a process-lifetime value captured at wiring time, reused for
// every rebuilt client. The API key now travels in Settings (editable from
// the tab), so each swap rebuilds the client with the settings' own key.
type Applier struct {
	holder  *Holder
	timeout time.Duration
	persist func(Settings) error
}

// NewApplier builds an Applier over holder. persist writes the settings to
// the mode-appropriate config file (config.SaveLLM behind a path the wiring
// layer resolved); a nil persist skips persistence (tests).
func NewApplier(holder *Holder, timeout time.Duration, persist func(Settings) error) *Applier {
	return &Applier{holder: holder, timeout: timeout, persist: persist}
}

// CurrentLLM returns the settings behind the live client.
func (a *Applier) CurrentLLM() Settings { return a.holder.Settings() }

// ListModels fetches model ids from the current endpoint via the holder.
func (a *Applier) ListModels(ctx context.Context) ([]string, error) {
	return a.holder.ListModels(ctx)
}

// ApplyLLM persists s and then swaps the live client to one built from it —
// including its API key. On a persist error nothing is swapped.
func (a *Applier) ApplyLLM(s Settings) error {
	if a.persist != nil {
		if err := a.persist(s); err != nil {
			return err
		}
	}
	a.holder.Swap(a.build(s), s)
	return nil
}

// llmTestTimeout is the fallback cap for a TestLLM probe when no timeout was
// wired into the Applier. When the Applier has a configured timeout (the user's
// llm.timeout), the probe honours that instead (#230) — a slow endpoint that is
// usable in production should also pass its own connection test.
const llmTestTimeout = 5 * time.Second

// TestFailure classifies a probe error into a fixed enum the web layer can
// render as an actionable banner without echoing raw error text (which may
// carry the endpoint URL).
type TestFailure string

const (
	TestOK            TestFailure = ""
	TestUnreachable   TestFailure = "unreachable"
	TestUnauthorized  TestFailure = "unauthorized"
	TestModelNotFound TestFailure = "model-not-found"
	TestTimeout       TestFailure = "timeout"
	TestBadResponse   TestFailure = "bad-response"
	TestUnavailable   TestFailure = "unavailable"
	TestNoModel       TestFailure = "no-model"
)

// TestResult carries per-model probe outcomes so the UI can report which model
// failed and why, instead of collapsing everything into one opaque error.
type TestResult struct {
	EmbedOK bool
	ChatOK  bool
	Failure TestFailure
}

// classifyError maps a probe error to a TestFailure enum. HTTP failures carry
// a typed *statusError, so the real status code decides the class — substring
// checks like "401" would also match ports, paths, and response-body text.
// Transport errors have no typed shape, so those fall back to the well-known
// net/http error strings.
func classifyError(err error) TestFailure {
	if err == nil {
		return TestOK
	}
	var se *statusError
	if errors.As(err, &se) {
		switch se.code {
		case 401, 403:
			return TestUnauthorized
		case 404:
			return TestModelNotFound
		}
		// Some gateways (LiteLLM, Ollama) report an unknown model as a 400/422
		// with an explanatory body rather than a 404.
		body := strings.ToLower(se.body)
		if strings.Contains(body, "model") && (strings.Contains(body, "not found") || strings.Contains(body, "does not exist")) {
			return TestModelNotFound
		}
		return TestBadResponse
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return TestTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return TestTimeout
	}
	msg := err.Error()
	if strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") || strings.Contains(msg, "dial") {
		return TestUnreachable
	}
	if strings.Contains(msg, "decode response") || strings.Contains(msg, "invalid character") || strings.Contains(msg, "json") {
		return TestBadResponse
	}
	return TestUnreachable
}

// TestLLM probes the endpoint described by s WITHOUT persisting or swapping the
// live client — the Settings → LLM tab's "Test connection" affordance, so a
// user can verify a LiteLLM/Ollama endpoint before saving. It builds a
// transient client from s (same builder ApplyLLM uses) and makes one cheap real
// call per configured model to prove reachability + model validity: a
// single-string embed when an embed model is set AND a minimal chat when a facts
// model is set. Probing every configured model keeps the "the model is valid"
// banner honest — a valid embed model no longer masks a typo'd facts model.
// Returns a TestResult with per-model outcomes; the web layer maps Failure to
// a fixed-enum banner and never echoes raw error text into the page.
func (a *Applier) TestLLM(ctx context.Context, s Settings) TestResult {
	timeout := a.timeout
	if timeout == 0 {
		timeout = llmTestTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := a.build(s)
	result := TestResult{EmbedOK: true, ChatOK: true}
	if s.EmbedModel == "" && s.ChatModel == "" {
		// Nothing configured to probe — a distinct failure so the banner can
		// say so instead of falsely claiming the endpoint was unreachable.
		result.Failure = TestNoModel
		return result
	}
	if s.EmbedModel != "" {
		if _, err := c.Embed(ctx, []string{"ping"}); err != nil {
			result.EmbedOK = false
			result.Failure = classifyError(err)
			return result
		}
	}
	if s.ChatModel != "" {
		if _, err := c.Chat(ctx, ChatRequest{
			Messages: []Message{{Role: RoleUser, Content: "ping"}},
		}); err != nil {
			result.ChatOK = false
			result.Failure = classifyError(err)
			return result
		}
	}
	return result
}

// build constructs a fresh client from s, reused by ApplyLLM's live swap and
// TestLLM's transient probe so both go through identical Options.
func (a *Applier) build(s Settings) *OpenAIClient {
	return New(Options{
		BaseURL:    s.BaseURL,
		APIKey:     s.APIKey,
		ChatModel:  s.ChatModel,
		EmbedModel: s.EmbedModel,
		Timeout:    a.timeout,
	})
}
