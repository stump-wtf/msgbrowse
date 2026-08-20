package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/llm"
)

// fakeLLMConfigurator is a test double for the LLMConfigurator seam. It
// records every ApplyLLM call so the security tests can assert a rejected
// POST applied NOTHING (the checkSetupPOST contract).
type fakeLLMConfigurator struct {
	cur      llm.Settings
	applied  []llm.Settings
	applyErr error
	// tested records every TestLLM probe so the tests can assert a rejected
	// POST probed NOTHING (the checkSetupPOST contract); testFailure is the
	// classified failure the probe returns (empty = success).
	tested      []llm.Settings
	testFailure llm.TestFailure
	// models, when non-nil, is what ListModels returns; nil falls through to
	// llm.ErrModelsNotSupported (the pre-#271 default).
	models []string
}

func (f *fakeLLMConfigurator) CurrentLLM() llm.Settings { return f.cur }
func (f *fakeLLMConfigurator) ApplyLLM(s llm.Settings) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied = append(f.applied, s)
	f.cur = s
	return nil
}

func (f *fakeLLMConfigurator) TestLLM(_ context.Context, s llm.Settings) llm.TestResult {
	f.tested = append(f.tested, s)
	return llm.TestResult{EmbedOK: f.testFailure == "", ChatOK: f.testFailure == "", Failure: f.testFailure}
}

func (f *fakeLLMConfigurator) ListModels(_ context.Context) ([]string, error) {
	if f.models != nil {
		return f.models, nil
	}
	return nil, llm.ErrModelsNotSupported
}

// modelsStub serves an OpenAI-compatible /v1/models listing, for the tests that
// drive a real llm.Applier instead of the fake configurator.
func modelsStub(t *testing.T, models []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		data := make([]map[string]string, 0, len(models))
		for _, m := range models {
			data = append(data, map[string]string{"id": m})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// llmPOST builds a POST /settings/llm with the given origin, token, and form
// values, mirroring enablePOST. An empty value is SENT as an empty field (the
// explicit "off" choice); to model an absent field — what a disabled select
// posts — leave the key out of the map entirely.
func llmPOST(t *testing.T, srv *Server, origin, token string, fields map[string]string) *httptest.ResponseRecorder {
	return llmPOSTTo(t, srv, "/settings/llm", origin, token, fields)
}

// llmTestPOST builds a POST /settings/llm/test — the "Test connection" probe.
func llmTestPOST(t *testing.T, srv *Server, origin, token string, fields map[string]string) *httptest.ResponseRecorder {
	return llmPOSTTo(t, srv, "/settings/llm/test", origin, token, fields)
}

// llmPOSTTo posts the given form to path with the given origin/token.
func llmPOSTTo(t *testing.T, srv *Server, path, origin, token string, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	for k, v := range fields {
		form.Set(k, v)
	}
	if token != "" {
		form.Set(setupTokenField, token)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// validLLMForm is a happy-path form body.
func validLLMForm() map[string]string {
	return map[string]string{
		"base_url":    "http://127.0.0.1:11434/v1",
		"embed_model": "nomic-embed-text",
		"facts_model": "llama3",
	}
}

// validLLMModels is the listing an endpoint must offer for validLLMForm() to
// save. Under SPEC-0004 REQ-0004-011 a save may only name a model the endpoint
// actually lists, so a test about persistence or secret handling has to hand
// the accept-list its inputs — otherwise it is really a test of the rejection
// path wearing the wrong name.
func validLLMModels() []string {
	return []string{"llama3", "local-chat", "local-embed", "nomic-embed-text"}
}

// TestLLMTabRenders is the template acceptance (#191): the tab renders with
// exactly the three fields showing the current effective values, the API-key
// HINT (env var) with no API-key input, and the local-first posture line.
func TestLLMTabRenders(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{cur: llm.Settings{
		BaseURL: "http://llm.test:4000/v1", EmbedModel: "test-embed", ChatModel: "test-chat",
	}}
	srv.SetLLMConfig(fc)

	rec := get(t, srv, "/settings/llm")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()

	// The sub-nav renders with the LLM tab active.
	if !contains(body, `href="/settings/llm" class="settings-tab settings-tab-active"`) {
		t.Error("LLM tab not active on its own page")
	}
	// Exactly the three fields, pre-filled with the current effective values.
	for _, want := range []string{
		`name="base_url"`, `name="embed_model"`, `name="facts_model"`,
		`value="http://llm.test:4000/v1"`, `value="test-embed"`, `value="test-chat"`,
	} {
		if !contains(body, want) {
			t.Errorf("LLM tab missing %q", want)
		}
	}
	// The facts-model hint says what it maps to.
	if !contains(body, "llm.chat_model") {
		t.Error("facts-model field missing its llm.chat_model hint")
	}
	// The API-key input is present as a write-only password field (Option A).
	for _, want := range []string{`name="api_key"`, `type="password"`} {
		if !contains(body, want) {
			t.Errorf("LLM tab missing the API-key field marker %q", want)
		}
	}
	// The env var is still documented as an override.
	if !contains(body, "MSGBROWSE_LLM_API_KEY") {
		t.Error("LLM tab missing the MSGBROWSE_LLM_API_KEY env-var hint")
	}
	// The quiet local-first posture line (ADR-0010).
	if !contains(body, "only network egress") || !contains(body, "ADR-0010") {
		t.Error("LLM tab missing the local-first posture line")
	}
	// The save form is armed with a live setup token.
	if !contains(body, `name="setup_token"`) {
		t.Error("LLM save form missing the setup token")
	}
}

// TestLLMTabShowsBootConfigWithoutConfigurator: with no configurator wired
// (plain NewServer), the tab still renders the boot config's effective
// values — file + defaults merged.
func TestLLMTabShowsBootConfigWithoutConfigurator(t *testing.T) {
	st, cfg, _ := newTestStoreAndConfig(t)
	cfg.LLM.BaseURL = "http://boot.test/v1"
	cfg.LLM.EmbedModel = "boot-embed"
	cfg.LLM.ChatModel = "boot-chat"
	srv, err := NewServer(st, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	body := get(t, srv, "/settings/llm").Body.String()
	for _, want := range []string{`value="http://boot.test/v1"`, `value="boot-embed"`, `value="boot-chat"`} {
		if !contains(body, want) {
			t.Errorf("boot-config fallback missing %q", want)
		}
	}
}

// TestLLMTabBoostedPartial: an HX-Request gets the *_content partial —
// <title> + #main-content, no full-document shell — per REQ-0008-006.
func TestLLMTabBoostedPartial(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/settings/llm", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, `id="main-content"`) || !contains(body, "<title>") {
		t.Error("boosted response missing the swap unit")
	}
	if contains(body, "<!doctype html") || contains(body, "<html") {
		t.Error("boosted response carried the full document shell")
	}
}

// TestLLMSaveCrossOriginRejected: a cross-origin POST — even with a valid
// token — is rejected 403 and applies nothing.
func TestLLMSaveCrossOriginRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := llmPOST(t, srv, "http://evil.example", tok, validLLMForm())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST status = %d, want 403", rec.Code)
	}
	if len(fc.applied) != 0 {
		t.Fatalf("cross-origin POST applied %d settings, want 0", len(fc.applied))
	}
}

// TestLLMSaveMissingTokenRejected: same-origin but tokenless → 403, nothing
// applied.
func TestLLMSaveMissingTokenRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{}
	srv.SetLLMConfig(fc)

	rec := llmPOST(t, srv, selfOrigin, "", validLLMForm())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing-token POST status = %d, want 403", rec.Code)
	}
	if len(fc.applied) != 0 {
		t.Fatalf("missing-token POST applied %d settings, want 0", len(fc.applied))
	}
}

// TestLLMSaveInvalidTokenRejected: a well-formed but never-minted token → 403.
func TestLLMSaveInvalidTokenRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{}
	srv.SetLLMConfig(fc)

	rec := llmPOST(t, srv, selfOrigin, strings.Repeat("ab", 32), validLLMForm())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("invalid-token POST status = %d, want 403", rec.Code)
	}
	if len(fc.applied) != 0 {
		t.Fatalf("invalid-token POST applied %d settings, want 0", len(fc.applied))
	}
}

// TestLLMSaveGETNeverMutates: the GET route renders and applies nothing, even
// when a token and form-shaped query values ride along.
func TestLLMSaveGETNeverMutates(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{cur: llm.Settings{BaseURL: "http://keep.test/v1"}}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := get(t, srv, "/settings/llm?base_url=http://evil.example/v1&setup_token="+tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(fc.applied) != 0 {
		t.Fatalf("GET applied %d settings, want 0", len(fc.applied))
	}
	if fc.cur.BaseURL != "http://keep.test/v1" {
		t.Fatalf("GET changed the settings: %+v", fc.cur)
	}
}

// TestLLMSaveValidationRejections: bad scheme, control characters, and
// oversized values re-render with a field error and apply NOTHING.
func TestLLMSaveValidationRejections(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		marker string
	}{
		{"scheme", map[string]string{"base_url": "ftp://files.example/v1"}, "must start with http:// or https://"},
		{"empty base URL", map[string]string{"base_url": ""}, "required"},
		{"no host", map[string]string{"base_url": "http://"}, "does not look like a valid URL"},
		{"control chars in URL", map[string]string{"base_url": "http://ok.test/v1\x00"}, "does not look like a valid URL"},
		{"oversize base URL", map[string]string{"base_url": "http://ok.test/" + strings.Repeat("a", llmBaseURLMaxLen)}, "too long"},
		{"control chars in embed model", map[string]string{"base_url": "http://ok.test/v1", "embed_model": "bad\x1bmodel"}, "characters that are not allowed"},
		{"oversize facts model", map[string]string{"base_url": "http://ok.test/v1", "facts_model": strings.Repeat("m", llmModelMaxLen+1)}, "too long"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			fc := &fakeLLMConfigurator{}
			srv.SetLLMConfig(fc)

			tok := mintToken(t, srv)
			rec := llmPOST(t, srv, selfOrigin, tok, c.fields)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (re-rendered form)", rec.Code)
			}
			if len(fc.applied) != 0 {
				t.Fatalf("invalid input applied %d settings, want 0", len(fc.applied))
			}
			body := rec.Body.String()
			if !contains(body, "Nothing was saved.") {
				t.Error("missing the nothing-saved banner")
			}
			if !contains(body, c.marker) {
				t.Errorf("missing field error %q", c.marker)
			}
		})
	}
}

// TestLLMSaveHappyPathWritesKeys drives the REAL stack — handler → llm.Applier
// → config.SaveLLM → llm.Holder — against a pre-existing config file: the llm
// keys (base URL, both models, AND the submitted api_key, Option A) are
// written, the unrelated pre-existing key survives, and the live holder swaps
// with the new key (#191's no-restart contract).
func TestLLMSaveHappyPathWritesKeys(t *testing.T) {
	srv, _, _ := newTestServer(t)

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("data_dir: /custom/data\nllm:\n  max_concurrency: 9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The live client's endpoint, not the submitted one, is what discovery
	// asks — so the stub stands in for the CURRENT endpoint and must list the
	// models the form is about to save (REQ-0004-011: a save may only name a
	// discovered model). Without it this test would exercise the rejection
	// path while claiming to test persistence.
	models := modelsStub(t, validLLMModels())
	holder := llm.NewHolder(llm.New(llm.Options{BaseURL: models.URL + "/v1"}), llm.Settings{
		BaseURL: models.URL + "/v1", EmbedModel: "old-embed", ChatModel: "old-chat",
	})
	srv.SetLLMConfig(llm.NewApplier(holder, 0, func(s llm.Settings) error {
		return config.SaveLLM(path, s.BaseURL, s.EmbedModel, s.ChatModel, s.APIKey)
	}))

	tok := mintToken(t, srv)
	rec := llmPOST(t, srv, selfOrigin, tok, map[string]string{
		"base_url":    "  http://127.0.0.1:11434/v1  ", // trimmed
		"embed_model": "nomic-embed-text",
		"facts_model": "llama3",
		"api_key":     "sk-test-key-123",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "LLM settings saved.") {
		t.Error("missing the saved confirmation")
	}
	// The re-rendered form shows the now-effective (trimmed) values...
	if !contains(body, `value="http://127.0.0.1:11434/v1"`) {
		t.Error("saved render missing the trimmed effective base URL")
	}
	// ...but NEVER the secret key value.
	if contains(body, "sk-test-key-123") {
		t.Error("the API key value must never be rendered back into the page")
	}

	// Live swap: the holder now reports the new settings, including the key.
	if got := holder.Settings(); got.BaseURL != "http://127.0.0.1:11434/v1" || got.EmbedModel != "nomic-embed-text" || got.ChatModel != "llama3" || got.APIKey != "sk-test-key-123" {
		t.Errorf("holder after save = %+v", got)
	}

	// The file: llm keys (incl. api_key) written, unrelated keys preserved.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{
		"base_url: http://127.0.0.1:11434/v1",
		"embed_model: nomic-embed-text",
		"chat_model: llama3",
		"api_key: sk-test-key-123",
		"data_dir: /custom/data", // unrelated top-level key preserved
		"max_concurrency: 9",     // unrelated llm key preserved
	} {
		if !contains(out, want) {
			t.Errorf("config file missing %q:\n%s", want, out)
		}
	}
}

// TestLLMSaveEmptyEmbedModelAllowed: an empty embed model is valid and means
// semantic search off.
func TestLLMSaveEmptyEmbedModelAllowed(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{models: validLLMModels()}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := llmPOST(t, srv, selfOrigin, tok, map[string]string{
		"base_url":    "http://127.0.0.1:4000/v1",
		"embed_model": "",
		"facts_model": "local-chat",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(fc.applied) != 1 {
		t.Fatalf("applied %d settings, want 1", len(fc.applied))
	}
	if got := fc.applied[0]; got.EmbedModel != "" || got.BaseURL != "http://127.0.0.1:4000/v1" {
		t.Errorf("applied = %+v", got)
	}
	if !contains(rec.Body.String(), "LLM settings saved.") {
		t.Error("missing the saved confirmation")
	}
}

// TestLLMSaveUnavailableWithoutConfigurator: with no configurator wired the
// gate-passing POST changes nothing and says so.
func TestLLMSaveUnavailableWithoutConfigurator(t *testing.T) {
	srv, _, _ := newTestServer(t)
	tok := mintToken(t, srv)
	rec := llmPOST(t, srv, selfOrigin, tok, validLLMForm())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !contains(rec.Body.String(), "Saving is not available here.") {
		t.Error("missing the unavailable banner")
	}
}

// TestLLMSaveApplyErrorReported: a failed persist/swap renders the error
// banner and the running config stays visible as submitted (no fake success).
func TestLLMSaveApplyErrorReported(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{applyErr: errors.New("disk full"), models: validLLMModels()}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := llmPOST(t, srv, selfOrigin, tok, validLLMForm())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "Saving failed.") {
		t.Error("missing the save-error banner")
	}
	if contains(body, "LLM settings saved.") {
		t.Error("rendered success despite an apply error")
	}
}

// TestLLMTabHasTestButton: the tab renders a "Test connection" button that
// targets /settings/llm/test via hx-post. The button is type="button" (not
// type="submit") to prevent double-firing the boosted form's save action.
func TestLLMTabHasTestButton(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetLLMConfig(&fakeLLMConfigurator{})

	body := get(t, srv, "/settings/llm").Body.String()
	if !contains(body, "Test connection") {
		t.Error("LLM tab missing the Test connection button")
	}
	if !contains(body, `hx-post="/settings/llm/test"`) {
		t.Error("Test connection button missing its hx-post route")
	}
	if contains(body, `formaction="/settings/llm/test"`) {
		t.Error("Test connection button should not have formaction (type=button prevents double-fire)")
	}
}

// TestLLMTestConnectionOK: a probe that returns nil renders the success banner,
// records the ENTERED values, and applies/persists NOTHING.
func TestLLMTestConnectionOK(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := llmTestPOST(t, srv, selfOrigin, tok, validLLMForm())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !contains(rec.Body.String(), "Connection succeeded.") {
		t.Error("missing the test-ok banner")
	}
	if len(fc.applied) != 0 {
		t.Fatalf("test probe applied %d settings, want 0", len(fc.applied))
	}
	if len(fc.tested) != 1 {
		t.Fatalf("test probed %d times, want 1", len(fc.tested))
	}
	if got := fc.tested[0]; got.BaseURL != "http://127.0.0.1:11434/v1" || got.EmbedModel != "nomic-embed-text" || got.ChatModel != "llama3" {
		t.Errorf("probed settings = %+v", got)
	}
}

// TestLLMTestConnectionUnreachable: a probe that errors renders the failure
// banner and NEVER echoes the raw error (which can carry the endpoint URL).
func TestLLMTestConnectionUnreachable(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{testFailure: llm.TestUnreachable}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := llmTestPOST(t, srv, selfOrigin, tok, validLLMForm())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "Connection failed.") {
		t.Error("missing the test-unreachable banner")
	}
	if contains(body, "connection refused") || contains(body, "10.0.0.9") {
		t.Error("the raw probe error must never be rendered into the page")
	}
	if contains(body, "Connection succeeded.") {
		t.Error("rendered success despite a probe error")
	}
}

// TestLLMTestConnectionBannerPerFailure: every classified probe failure maps
// to its own banner — the actionable-enum contract of #230.
func TestLLMTestConnectionBannerPerFailure(t *testing.T) {
	cases := []struct {
		failure llm.TestFailure
		banner  string
	}{
		{llm.TestUnauthorized, "Authentication failed."},
		{llm.TestModelNotFound, "Model not found."},
		{llm.TestTimeout, "Connection timed out."},
		{llm.TestBadResponse, "Unexpected response."},
		{llm.TestNoModel, "Nothing to test."},
	}
	for _, tc := range cases {
		t.Run(string(tc.failure), func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			srv.SetLLMConfig(&fakeLLMConfigurator{testFailure: tc.failure})
			tok := mintToken(t, srv)
			rec := llmTestPOST(t, srv, selfOrigin, tok, validLLMForm())
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if !contains(rec.Body.String(), tc.banner) {
				t.Errorf("failure %q: missing banner %q", tc.failure, tc.banner)
			}
		})
	}
}

// TestLLMTestUnavailableWithoutConfigurator: no configurator wired → the
// gate-passing probe reports itself unavailable and probes nothing.
func TestLLMTestUnavailableWithoutConfigurator(t *testing.T) {
	srv, _, _ := newTestServer(t)
	tok := mintToken(t, srv)
	rec := llmTestPOST(t, srv, selfOrigin, tok, validLLMForm())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !contains(rec.Body.String(), "Testing is not available here.") {
		t.Error("missing the test-unavailable banner")
	}
}

// TestLLMTestValidationRejected: an invalid form re-renders with the field
// error and probes NOTHING (validation runs before the probe).
func TestLLMTestValidationRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := llmTestPOST(t, srv, selfOrigin, tok, map[string]string{"base_url": "ftp://nope/v1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(fc.tested) != 0 {
		t.Fatalf("invalid input probed %d times, want 0", len(fc.tested))
	}
	if !contains(rec.Body.String(), "must start with http:// or https://") {
		t.Error("missing the base-URL field error")
	}
}

// TestLLMTestCrossOriginRejected: a cross-origin probe — even with a valid
// token — is rejected 403 and probes nothing (the checkSetupPOST gate).
func TestLLMTestCrossOriginRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := llmTestPOST(t, srv, "http://evil.example", tok, validLLMForm())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin probe status = %d, want 403", rec.Code)
	}
	if len(fc.tested) != 0 {
		t.Fatalf("cross-origin probe ran %d times, want 0", len(fc.tested))
	}
}

// TestLLMTestMissingTokenRejected: same-origin but tokenless → 403, nothing
// probed.
func TestLLMTestMissingTokenRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{}
	srv.SetLLMConfig(fc)

	rec := llmTestPOST(t, srv, selfOrigin, "", validLLMForm())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing-token probe status = %d, want 403", rec.Code)
	}
	if len(fc.tested) != 0 {
		t.Fatalf("missing-token probe ran %d times, want 0", len(fc.tested))
	}
}

// TestLLMSaveBlankKeepsConfigKey: a blank api_key field with a config-sourced
// key set keeps the current key and applies it with APIKeyFromEnv=false (it is
// legitimately stored in config, Option A).
func TestLLMSaveBlankKeepsConfigKey(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{models: validLLMModels(), cur: llm.Settings{
		BaseURL: "http://old/v1", APIKey: "sk-in-config", APIKeyFromEnv: false,
	}}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := llmPOST(t, srv, selfOrigin, tok, validLLMForm()) // no api_key field
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(fc.applied) != 1 {
		t.Fatalf("applied %d, want 1", len(fc.applied))
	}
	if got := fc.applied[0]; got.APIKey != "sk-in-config" || got.APIKeyFromEnv {
		t.Errorf("applied = %+v, want kept config key with APIKeyFromEnv=false", got)
	}
}

// TestLLMSaveBlankKeepsEnvKeyUnpersisted is the env-key-leak regression: when
// the current key came from MSGBROWSE_LLM_API_KEY (APIKeyFromEnv=true), a blank
// save keeps it LIVE but carries the env flag through so the persist layer
// never writes the env secret to the config file.
func TestLLMSaveBlankKeepsEnvKeyUnpersisted(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{models: validLLMModels(), cur: llm.Settings{
		BaseURL: "http://old/v1", APIKey: "sk-from-env", APIKeyFromEnv: true,
	}}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := llmPOST(t, srv, selfOrigin, tok, validLLMForm()) // no api_key field
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(fc.applied) != 1 {
		t.Fatalf("applied %d, want 1", len(fc.applied))
	}
	got := fc.applied[0]
	if got.APIKey != "sk-from-env" {
		t.Errorf("live key = %q, want the env key kept for the live client", got.APIKey)
	}
	if !got.APIKeyFromEnv {
		t.Error("APIKeyFromEnv must stay true so persist suppresses the on-disk copy")
	}
}

// TestLLMSaveTypedKeyOverridesEnv: typing a key when the current one is
// env-sourced flips APIKeyFromEnv off — the user chose to store it (Option A),
// so it must now persist.
func TestLLMSaveTypedKeyOverridesEnv(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{models: validLLMModels(), cur: llm.Settings{APIKey: "sk-from-env", APIKeyFromEnv: true}}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	form := validLLMForm()
	form["api_key"] = "sk-typed-in-tab"
	rec := llmPOST(t, srv, selfOrigin, tok, form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := fc.applied[0]; got.APIKey != "sk-typed-in-tab" || got.APIKeyFromEnv {
		t.Errorf("applied = %+v, want typed key with APIKeyFromEnv=false", got)
	}
}

// TestLLMSaveClearKeyWipes: the "Clear saved key" checkbox wipes the key even
// when the field is blank, and marks it not-from-env so the empty value is
// persisted (clearing the config copy).
func TestLLMSaveClearKeyWipes(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{models: validLLMModels(), cur: llm.Settings{APIKey: "sk-old", APIKeyFromEnv: false}}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	form := validLLMForm()
	form["clear_api_key"] = "1"
	rec := llmPOST(t, srv, selfOrigin, tok, form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := fc.applied[0]; got.APIKey != "" || got.APIKeyFromEnv {
		t.Errorf("applied = %+v, want wiped key (APIKey=\"\", APIKeyFromEnv=false)", got)
	}
}

// llmModelsPOST posts to /settings/llm/models with the given origin/token/fields.
func llmModelsPOST(t *testing.T, srv *Server, origin, token string, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return llmPOSTTo(t, srv, "/settings/llm/models", origin, token, fields)
}

// TestLLMModelsRefreshUnsupported: ListModels returns ErrModelsNotSupported,
// the banner says so and the fields remain text inputs.
func TestLLMModelsRefreshUnsupported(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := llmModelsPOST(t, srv, selfOrigin, tok, validLLMForm())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "Model listing not supported") {
		t.Error("missing the listing-not-supported banner")
	}
}

// TestLLMModelsRefreshCrossOriginRejected: cross-origin → 403, nothing listed.
func TestLLMModelsRefreshCrossOriginRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := llmModelsPOST(t, srv, "http://evil.example", tok, validLLMForm())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin models POST status = %d, want 403", rec.Code)
	}
}

// TestLLMModelsRefreshMissingTokenRejected: tokenless → 403.
func TestLLMModelsRefreshMissingTokenRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{}
	srv.SetLLMConfig(fc)

	rec := llmModelsPOST(t, srv, selfOrigin, "", validLLMForm())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing-token models POST status = %d, want 403", rec.Code)
	}
}

// TestLLMModelsRefreshUnavailable: no configurator → unavailable banner.
func TestLLMModelsRefreshUnavailable(t *testing.T) {
	srv, _, _ := newTestServer(t)
	tok := mintToken(t, srv)
	rec := llmModelsPOST(t, srv, selfOrigin, tok, validLLMForm())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !contains(rec.Body.String(), "Model listing is not available here.") {
		t.Error("missing the models-unavailable banner")
	}
}

// TestLLMModelsRefreshValidationRejected: invalid form → re-renders with error,
// no ListModels call.
func TestLLMModelsRefreshValidationRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := llmModelsPOST(t, srv, selfOrigin, tok, map[string]string{"base_url": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !contains(rec.Body.String(), "A base URL is required") {
		t.Error("missing the base-URL field error")
	}
}

// TestLLMTabAutoLoadsModels (#271, tightened by REQ-0004-011): GET
// /settings/llm probes the endpoint on first paint and renders both model
// fields as live <select>s — no button click required, and no text input
// anywhere on the page.
func TestLLMTabAutoLoadsModels(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{
		cur:    llm.Settings{BaseURL: "http://llm.test:4000/v1", EmbedModel: "beta", ChatModel: "gamma"},
		models: []string{"alpha", "beta", "gamma"},
	}
	srv.SetLLMConfig(fc)

	body := llmTabBody(t, srv)
	for _, id := range []string{"llm-embed-model", "llm-facts-model"} {
		if !contains(body, `<select id="`+id+`"`) {
			t.Errorf("%s must render as a <select>", id)
		}
	}
	assertNoModelTextInput(t, body)
	if contains(body, "<datalist") || contains(body, `list="llm-model-options"`) {
		t.Error("a datalist-backed input is a text input — REQ-0004-011 rules it out explicitly")
	}
	for _, m := range []string{"alpha", "beta", "gamma"} {
		if !contains(body, `<option value="`+m+`"`) {
			t.Errorf("missing option for model %q", m)
		}
	}
	// The saved values are the selected ones, not merely present.
	if !contains(body, `<option value="beta" selected>`) || !contains(body, `<option value="gamma" selected>`) {
		t.Error("the saved models must be the selected options")
	}
	// Both fields are optional, so each carries an explicit off entry — an
	// optional <select> has no empty-text-box equivalent.
	if !contains(body, "Off — semantic search disabled") || !contains(body, "Off — AI features disabled") {
		t.Error("both model fields must offer an explicit off option")
	}
	// No ModelsResult banner on initial load — the dropdowns are the signal.
	if contains(body, "Models refreshed.") {
		t.Error("initial GET must not render the 'Models refreshed' banner (that's the explicit refresh path)")
	}
}

// TestLLMTabDiscoveryFailureDisablesSelects is the heart of REQ-0004-011's
// failure clause, and it replaces the old "silently fall back to a text input"
// test. That fallback is what a mistyped model name is made of — an index that
// never builds, a journal build that dies hours in.
//
// Every failure mode gets the same shape: a disabled <select> still showing
// the saved value, an explanation, and a retry.
func TestLLMTabDiscoveryFailureDisablesSelects(t *testing.T) {
	cases := []struct {
		name   string
		fc     *fakeLLMConfigurator
		banner string
	}{
		{
			// models nil → ListModels returns ErrModelsNotSupported.
			"listing unsupported",
			&fakeLLMConfigurator{cur: llm.Settings{BaseURL: "http://llm.test:4000/v1", EmbedModel: "test-embed", ChatModel: "test-chat"}},
			"This endpoint does not list its models.",
		},
		{
			"listing empty",
			&fakeLLMConfigurator{
				cur:    llm.Settings{BaseURL: "http://llm.test:4000/v1", EmbedModel: "test-embed", ChatModel: "test-chat"},
				models: []string{},
			},
			"The endpoint offers no models.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			srv.SetLLMConfig(tc.fc)

			body := llmTabBody(t, srv)
			assertNoModelTextInput(t, body)
			for _, id := range []string{"llm-embed-model", "llm-facts-model"} {
				if !contains(body, `<select id="`+id+`"`) {
					t.Errorf("%s must still be a <select>, never a text input", id)
				}
			}
			if n := strings.Count(body, `class="filter-input w-full font-mono" disabled`); n != 2 {
				t.Errorf("disabled model selects = %d, want 2 when discovery fails", n)
			}
			// The saved values survive on screen — the user must be able to
			// see what is configured even when it cannot be changed here —
			// and are NOT accused of being unoffered by an endpoint that
			// never answered.
			for _, m := range []string{"test-embed", "test-chat"} {
				if !contains(body, `<option value="`+m+`" selected>`+m+`</option>`) {
					t.Errorf("saved model %q must remain displayed, selected, and unannotated", m)
				}
			}
			if !contains(body, tc.banner) {
				t.Errorf("missing the explanatory banner %q", tc.banner)
			}
			// The retry the requirement asks for.
			if !contains(body, "Refresh models") {
				t.Error("missing the retry control")
			}
		})
	}
}

// TestLLMTabUnlistedModelSurvives: a model set by config file or
// MSGBROWSE_LLM_* that the endpoint does not list is still rendered, still
// selected, and marked — dropping it would make the browser submit whatever
// option happened to be first and silently rewrite the configuration.
func TestLLMTabUnlistedModelSurvives(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetLLMConfig(&fakeLLMConfigurator{
		cur:    llm.Settings{BaseURL: "http://llm.test:4000/v1", EmbedModel: "alpha", ChatModel: "env-only-model"},
		models: []string{"alpha", "beta"},
	})

	body := llmTabBody(t, srv)
	if !contains(body, `<option value="env-only-model" selected>env-only-model — not currently offered</option>`) {
		t.Error("an unlisted configured model must render, stay selected, and be marked as unlisted")
	}
	// It is NOT confused with the off state.
	if contains(body, `<option value="" selected>Off — AI features disabled</option>`) {
		t.Error("an unlisted model must not fall through to the off option")
	}
}

// llmTabBody GETs the LLM tab and returns the rendered body.
func llmTabBody(t *testing.T, srv *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/settings/llm", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	return rec.Body.String()
}

// assertNoModelTextInput is the prohibition itself: no rendering path, in any
// state, may put a model behind a text input.
//
// It scans every <input> tag and rejects any whose name is a model field,
// rather than matching attribute substrings. The substring form missed the
// ordering a developer is most likely to write by hand — `<input type="text"
// name="embed_model">` matches neither `<input id="llm-embed-model"` nor
// `name="embed_model" type="text"`, so the prohibition passed with a free-text
// model field rendered on the page. Any <input> carrying these names defeats
// the select, hidden ones included, so the name alone is the test.
//
// @joestump-agent 08/20/2026 - Made order-insensitive while reviewing #375.
var (
	inputTagRe  = regexp.MustCompile(`(?is)<input\b[^>]*>`)
	inputNameRe = regexp.MustCompile(`(?is)\bname\s*=\s*["']([^"']*)["']`)
)

func assertNoModelTextInput(t *testing.T, body string) {
	t.Helper()
	for _, tag := range inputTagRe.FindAllString(body, -1) {
		m := inputNameRe.FindStringSubmatch(tag)
		if m == nil {
			continue
		}
		switch strings.TrimSpace(m[1]) {
		case "embed_model", "facts_model":
			t.Errorf("model field rendered as an <input> — REQ-0004-011 requires a <select>.\ntag: %s", tag)
		}
	}
}

// TestLLMSaveRejectsUndiscoveredModel is REQ-0004-011's server-side clause: a
// <select> is a client-side hint, and the same POST is one curl away. A model
// the endpoint does not offer must be refused rather than written into the
// config file, where it would be discovered wrong at the next LLM call — for
// the journal, hours into a multi-thousand-call build.
func TestLLMSaveRejectsUndiscoveredModel(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{
		cur:    llm.Settings{BaseURL: "http://llm.test:4000/v1", EmbedModel: "alpha", ChatModel: "beta"},
		models: []string{"alpha", "beta"},
	}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := llmPOST(t, srv, selfOrigin, tok, map[string]string{
		"base_url":    "http://llm.test:4000/v1",
		"embed_model": "typo-embed-3-large",
		"facts_model": "beta",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(fc.applied) != 0 {
		t.Fatalf("applied %d settings, want 0 — an unoffered model must not reach the config file", len(fc.applied))
	}
	if !contains(rec.Body.String(), "That model is not offered by this endpoint.") {
		t.Error("missing the not-offered field error")
	}
}

// TestLLMSaveKeepsCurrentModelWhenAbsent is the disabled-select round trip: a
// disabled control submits no key at all, so an absent field means "keep what
// is saved". Reading it as an empty choice would let saving the API key
// silently turn semantic search off, on exactly the broken-endpoint page where
// the user could not see a listing to re-pick from.
func TestLLMSaveKeepsCurrentModelWhenAbsent(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{
		// models nil → discovery fails → the selects render disabled.
		cur: llm.Settings{BaseURL: "http://llm.test:4000/v1", EmbedModel: "kept-embed", ChatModel: "kept-chat"},
	}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	// No embed_model / facts_model keys at all — what a disabled select posts.
	rec := llmPOST(t, srv, selfOrigin, tok, map[string]string{
		"base_url": "http://llm.test:4000/v1",
		"api_key":  "sk-new-key",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(fc.applied) != 1 {
		t.Fatalf("applied %d, want 1", len(fc.applied))
	}
	if got := fc.applied[0]; got.EmbedModel != "kept-embed" || got.ChatModel != "kept-chat" {
		t.Errorf("applied = %+v, want both models kept", got)
	}
}

// TestLLMSaveUnlistedCurrentModelSurvivesSave is the second scenario the
// requirement spells out: an env-set model the endpoint does not list is
// submitted back unchanged and must be accepted, not rejected as unoffered.
func TestLLMSaveUnlistedCurrentModelSurvivesSave(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{
		cur:    llm.Settings{BaseURL: "http://llm.test:4000/v1", EmbedModel: "alpha", ChatModel: "env-only-model"},
		models: []string{"alpha", "beta"},
	}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := llmPOST(t, srv, selfOrigin, tok, map[string]string{
		"base_url":    "http://llm.test:4000/v1",
		"embed_model": "alpha",
		"facts_model": "env-only-model", // what the rendered form posts back
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(fc.applied) != 1 {
		t.Fatalf("applied %d, want 1 — the configured model must survive a save", len(fc.applied))
	}
	if got := fc.applied[0]; got.ChatModel != "env-only-model" {
		t.Errorf("applied chat model = %q, want the retained env-set model", got.ChatModel)
	}
	if !contains(rec.Body.String(), "not currently offered") {
		t.Error("the retained model should still be marked as unlisted after the save")
	}
}

// TestLLMSaveOffIsAlwaysAllowed: the explicit off option is a legitimate
// choice, not an undiscovered model.
func TestLLMSaveOffIsAlwaysAllowed(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{
		cur:    llm.Settings{BaseURL: "http://llm.test:4000/v1", EmbedModel: "alpha", ChatModel: "beta"},
		models: []string{"alpha", "beta"},
	}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := llmPOST(t, srv, selfOrigin, tok, map[string]string{
		"base_url":    "http://llm.test:4000/v1",
		"embed_model": "",
		"facts_model": "beta",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(fc.applied) != 1 {
		t.Fatalf("applied %d, want 1", len(fc.applied))
	}
	if got := fc.applied[0]; got.EmbedModel != "" {
		t.Errorf("applied embed model = %q, want off", got.EmbedModel)
	}
}
