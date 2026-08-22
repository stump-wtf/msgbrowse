// The Settings → LLM tab (issue #191): the user-facing surface for the AI
// endpoint — base URL, embed model, "Facts model" (llm.chat_model; the facts
// feature consumes it today, the journal digest later), and the API key. The
// key is editable here and persisted to the 0600 config file (Option A — a
// desktop user has no convenient env var; ADR-0010's loopback single-user
// trust). Two exceptions keep the secret honest: a key supplied via
// MSGBROWSE_LLM_API_KEY is used but NEVER written back to disk (it stays in the
// environment it was scoped to), and the field itself is a password input whose
// value is never echoed — a blank field means "keep the current key", and an
// explicit Clear checkbox wipes it. Saving applies LIVE — the wired
// LLMConfigurator persists the keys into the loaded config file and swaps the
// process's llm.Holder, so the MCP server's semantic search uses the new
// endpoint with no restart.
//
// The save POST is privileged (it changes the app's single network egress,
// ADR-0010) and is gated exactly like the Setup POSTs: same-origin +
// per-session token + MaxBytesReader, rejected 403 before any work
// (checkSetupPOST, SPEC-0013 §Security). Validation failures re-render the
// tab with fixed-enum field errors; the only request-derived strings in the
// render are the echoed form values, which html/template escapes like all
// message content.
package web

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/joestump/msgbrowse/internal/llm"
)

// llmBaseURLMaxLen / llmModelMaxLen bound the accepted field lengths — far
// beyond any real endpoint or model name, small enough to reject garbage.
const (
	llmBaseURLMaxLen = 2048
	llmModelMaxLen   = 128
	llmAPIKeyMaxLen  = 512
)

// LLMConfigurator is the live-settings seam behind the LLM tab (the
// SetDetector/SetEnabler pattern): serve and the desktop shell wire an
// llm.Applier over the process's shared llm.Holder; tests wire fakes. With no
// configurator wired the tab still renders (showing the boot config values)
// but a save reports itself unavailable rather than pretending.
type LLMConfigurator interface {
	// CurrentLLM returns the effective settings behind the live client
	// (config file + defaults merged, as loaded or last applied).
	CurrentLLM() llm.Settings
	// ApplyLLM persists s to the config file and swaps the live client.
	// Nothing is swapped when persistence fails.
	ApplyLLM(s llm.Settings) error
	// TestLLM probes the endpoint described by s with a cheap real call,
	// WITHOUT persisting or swapping the live client — the "Test connection"
	// affordance so a user can verify an endpoint before saving. Returns a
	// TestResult with per-model outcomes; the web layer maps Failure to a
	// fixed-enum banner.
	TestLLM(ctx context.Context, s llm.Settings) llm.TestResult
	// ListModels fetches the model ids from the currently configured
	// endpoint's /v1/models listing. Returns llm.ErrModelsNotSupported
	// when the endpoint returns 404 or a non-JSON body. Returns the
	// underlying error when the endpoint is unreachable.
	ListModels(ctx context.Context) ([]string, error)
}

// SetLLMConfig wires the live LLM settings source. Call it after NewServer
// and before serving begins — handlers read the field without locking, so
// late wiring would race.
func (s *Server) SetLLMConfig(c LLMConfigurator) { s.llmConfig = c }

// llmSettingsData drives the LLM tab. Field errors and SaveResult are fixed
// enums mapped to prose by the template; BaseURL/EmbedModel/FactsModel echo
// the submitted values on a validation failure (html/template-escaped) so the
// user can correct instead of retyping.
type llmSettingsData struct {
	baseData
	BaseURL    string
	EmbedModel string
	FactsModel string
	// HasAPIKey reports whether a key is currently set, so the template can show
	// a "key is set" placeholder without ever rendering the secret. The key
	// value is NEVER echoed into the form (a blank field means "keep it").
	HasAPIKey bool
	// APIKeyFromEnv reports the current key came from MSGBROWSE_LLM_API_KEY, so
	// the template can label it "set via environment" and note that saving will
	// not write it to the config file. Only meaningful when HasAPIKey is true.
	APIKeyFromEnv bool
	// SetupToken is the per-session token the form submits through the same
	// checkSetupPOST gate the Setup POSTs use.
	SetupToken string
	// SaveResult is the post-save banner state: "" (no save attempted), "ok",
	// "unavailable" (no configurator wired), or "error" (persist/swap failed).
	SaveResult string
	// TestResult is the post-probe banner state for "Test connection": ""
	// (no probe attempted), "ok" (endpoint reachable + models valid),
	// "unavailable" (no configurator wired), or one of llm.TestFailure's
	// classified enums ("unauthorized", "model-not-found", "timeout",
	// "bad-response", "unreachable", "no-model") — no raw error is echoed.
	TestResult string
	// ModelsResult is the post-refresh banner state for the "Refresh models"
	// button: "" (no refresh attempted), "ok" (listing succeeded),
	// "unsupported" (endpoint returned 404/non-JSON), "unreachable"
	// (connection failed), or "unavailable" (no configurator wired).
	ModelsResult string
	// Models is the list of model ids returned by the last successful listing,
	// used to populate the embed and facts model dropdowns.
	Models []string
	// ModelsAvailable reports that discovery produced a usable listing, so the
	// model <select>s are live. False disables them (SPEC-0004 REQ-0004-011:
	// degrade to a disabled select, never to a text input).
	ModelsAvailable bool
	// ModelsUnavailable explains WHY the selects are disabled, as its own
	// fixed enum ("unsupported", "unreachable", "empty", "unavailable"),
	// rendered as a banner beside the fields. It is deliberately separate from
	// ModelsResult: that one reports the outcome of an explicit "Refresh
	// models" click and loses the top-banner slot to a save or test result,
	// while this one must stay on screen for as long as the controls are
	// disabled.
	ModelsUnavailable string
	// EmbedOptions / FactsOptions are the rendered <option> sets, built once in
	// renderLLMSettings so no response can reach the template with a model
	// field and no options behind it.
	EmbedOptions []llmModelOption
	FactsOptions []llmModelOption
	// modelsResolved records that a handler already ran discovery, so
	// renderLLMSettings does not probe the endpoint a second time on the same
	// response. Unexported: it is bookkeeping, not template data.
	modelsResolved bool
	// Per-field validation errors: "" (valid), "required", "scheme",
	// "invalid", "toolong", or — for the model fields — "notoffered" (a
	// submitted id that is neither in the discovered listing nor the value
	// already saved).
	ErrBaseURL    string
	ErrEmbedModel string
	ErrFactsModel string
	ErrAPIKey     string
	// EmbedModelChanged is true when the saved embed model differs from the
	// model that was last used by the index, signalling that the index needs
	// a rebuild.
	EmbedModelChanged bool
}

// llmModelOption is one <option> in a model select.
//
// Unlisted marks the awkward case the requirement is mostly about: a model set
// by config file or MSGBROWSE_LLM_* that the endpoint does not currently offer.
// It is still rendered, still selected, and still saveable — dropping it would
// silently rewrite the user's configuration on the next save — but it is
// labelled so the discrepancy is visible rather than mysterious.
type llmModelOption struct {
	Value    string
	Label    string
	Selected bool
	Unlisted bool
}

// llmModelOptions builds the option set for one model field: an explicit "off"
// entry, every discovered model, and — when the saved value is absent from the
// listing — the saved value itself, marked unlisted.
//
// The "off" entry exists because both model fields are optional, and an
// optional <select> has no equivalent of an empty text box: without a value to
// select, "no embed model" would be indistinguishable from "the first model in
// the list", and the browser would submit that first model on save. So the
// absence is a choice the user can see and make (SPEC-0004 REQ-0004-011: the
// off state MUST be an explicit option rather than an empty field).
func llmModelOptions(models []string, current, offLabel string) []llmModelOption {
	opts := []llmModelOption{{Value: "", Label: offLabel, Selected: current == ""}}
	listed := false
	for _, m := range models {
		if m == current {
			listed = true
		}
		opts = append(opts, llmModelOption{Value: m, Label: m, Selected: m == current})
	}
	if current != "" && !listed {
		// Second position, not last: it is the selected value, and burying it
		// under a long listing makes the "why does this say something the
		// endpoint has never heard of" question harder to answer, not easier.
		//
		// Marked unlisted only when there WAS a listing to be absent from.
		// With discovery down the endpoint has said nothing either way, and
		// "not currently offered" would be an accusation the page cannot
		// support — the disabled control and its banner already explain why
		// nothing else is selectable.
		opts = append(opts[:1], append([]llmModelOption{{
			Value: current, Label: current, Selected: true, Unlisted: len(models) > 0,
		}}, opts[1:]...)...)
	}
	return opts
}

// llmModelAllowed reports whether a submitted model id may be written to the
// config file: the empty "off" choice, the value already saved, or one the
// endpoint actually offers.
//
// A <select> is a client-side hint and nothing more — the same POST is one
// curl away, and a disabled select submits nothing at all — so the handler
// re-decides rather than trusting the shape of the control it rendered
// (SPEC-0004 REQ-0004-011: client-side control choice is not validation).
func llmModelAllowed(submitted, current string, models []string) bool {
	if submitted == "" || submitted == current {
		return true
	}
	for _, m := range models {
		if m == submitted {
			return true
		}
	}
	return false
}

// llmModelFromForm reads one model field, falling back to the currently-saved
// value when the field is ABSENT from the submission.
//
// Absent is not the same as empty. A disabled <select> — what discovery
// failure degrades to — submits no key at all, so treating a missing key as
// "the user chose off" would let the act of saving the API key silently clear
// the model the user could not even see a listing for (SPEC-0004 REQ-0004-011:
// the saved value MUST NOT be cleared by saving the form). An explicitly
// submitted empty value still means off.
func llmModelFromForm(r *http.Request, field, current string) string {
	// Idempotent, and cheap once the gate has already parsed the body.
	_ = r.ParseForm()
	if _, ok := r.PostForm[field]; !ok {
		return current
	}
	return strings.TrimSpace(r.PostFormValue(field))
}

// HasErrors reports whether any field failed validation, for the template's
// summary banner.
func (d llmSettingsData) HasErrors() bool {
	return d.ErrBaseURL != "" || d.ErrEmbedModel != "" || d.ErrFactsModel != "" || d.ErrAPIKey != ""
}

// currentLLM resolves the effective settings for display: the live
// configurator when wired, else the boot-time config snapshot.
func (s *Server) currentLLM() llm.Settings {
	if s.llmConfig != nil {
		return s.llmConfig.CurrentLLM()
	}
	return s.llmBoot
}

// handleSettingsLLM renders the LLM tab (GET /settings/llm) with the current
// effective values. Safe GET: no mutation; the minted token arms the save
// form.
//
// Model discovery runs on every render, not just on an explicit "Refresh
// models" click (#271) — the model fields are dropdowns now, and a dropdown
// with nothing behind it is not a control (SPEC-0004 REQ-0004-011). It happens
// in renderLLMSettings so no response path can skip it.
func (s *Server) handleSettingsLLM(w http.ResponseWriter, r *http.Request) {
	cur := s.currentLLM()
	data := llmSettingsData{
		BaseURL:       cur.BaseURL,
		EmbedModel:    cur.EmbedModel,
		FactsModel:    cur.ChatModel,
		HasAPIKey:     cur.APIKey != "",
		APIKeyFromEnv: cur.APIKeyFromEnv,
	}
	data.EmbedModelChanged = s.embedModelChanged(r.Context(), cur.EmbedModel)
	s.renderLLMSettings(w, r, data)
}

// llmModelsTimeout bounds the discovery probe. It is short on purpose: the
// listing decorates a settings page, and a slow endpoint must not hold the
// page hostage — a timed-out probe degrades to a disabled select with a
// banner, which is a usable page.
const llmModelsTimeout = 3 * time.Second

// resolveModels runs model discovery and records the outcome on data: the
// listing itself, whether the selects are live, and — when they are not — the
// fixed-enum reason the banner explains.
//
// An EMPTY listing counts as a failure, not as success with nothing in it. An
// endpoint that answers /v1/models with zero models cannot furnish a choice,
// and a select holding only "off" would quietly offer to turn the user's
// features off (SPEC-0004 REQ-0004-011 names the empty listing alongside
// unreachable and unsupported).
func (s *Server) resolveModels(ctx context.Context, data *llmSettingsData) {
	data.modelsResolved = true
	if s.llmConfig == nil {
		data.ModelsUnavailable = "unavailable"
		return
	}
	ctx, cancel := context.WithTimeout(ctx, llmModelsTimeout)
	defer cancel()
	models, err := s.listLLMModels(ctx)
	switch {
	case err == llm.ErrModelsNotSupported:
		s.log.Debug("LLM model listing unsupported", "base_url", data.BaseURL)
		data.ModelsUnavailable = "unsupported"
	case err != nil:
		s.log.Debug("LLM model listing failed", "base_url", data.BaseURL, "error", err)
		data.ModelsUnavailable = "unreachable"
	case len(models) == 0:
		s.log.Debug("LLM model listing empty", "base_url", data.BaseURL)
		data.ModelsUnavailable = "empty"
	default:
		data.Models = models
		data.ModelsAvailable = true
	}
}

// listLLMModels fetches, filters, and sorts the endpoint's /v1/models listing
// via the live configurator. Shared between handleSettingsLLM (auto-load) and
// handleSettingsLLMModels (explicit refresh). The caller is responsible for
// the timeout and for mapping the error to a user-facing banner when one is
// wanted — the helper returns the raw error so each caller can decide.
func (s *Server) listLLMModels(ctx context.Context) ([]string, error) {
	models, err := s.llmConfig.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	// Filter: reject models that fail validation (non-printable, too long).
	filtered := make([]string, 0, len(models))
	for _, m := range models {
		if validateLLMModel(m) == "" {
			filtered = append(filtered, m)
		}
	}
	// Sort for stable display.
	sort.Strings(filtered)
	return filtered, nil
}

// handleSettingsLLMSave is POST /settings/llm — the privileged save. Gate
// first (checkSetupPOST: same-origin + per-session token + body cap, 403
// before any work), validate second, and only then apply: persist the three
// keys and swap the live client. Success re-renders the tab with the saved
// banner and the now-effective values (the boosted-partial pattern — htmx
// swaps #main-content; a plain form POST gets the full document).
func (s *Server) handleSettingsLLMSave(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return // 403 already written; nothing was validated or applied
	}

	// The API key is a password field we never echo, so a BLANK field means
	// "keep the current key" — only a non-blank value changes it (the standard
	// secret-field UX; it also means the form can't accidentally wipe a key on
	// an unrelated save). The "Clear saved key" checkbox is the explicit wipe.
	// The effective key (and its provenance) is resolved against the live value.
	cur := s.currentLLM()
	clearKey := r.PostFormValue("clear_api_key") != ""
	typedKey := strings.TrimSpace(r.PostFormValue("api_key"))
	apiKey := typedKey
	fromEnv := false
	keptKey := false
	switch {
	case clearKey:
		apiKey = "" // explicit wipe wins over any typed/kept value
	case typedKey == "":
		// Blank and not clearing: keep the current key, preserving its
		// provenance so an env-provided key still isn't persisted to disk.
		apiKey = cur.APIKey
		fromEnv = cur.APIKeyFromEnv
		keptKey = true
	}

	data := llmSettingsData{
		BaseURL:       strings.TrimSpace(r.PostFormValue("base_url")),
		EmbedModel:    llmModelFromForm(r, "embed_model", cur.EmbedModel),
		FactsModel:    llmModelFromForm(r, "facts_model", cur.ChatModel),
		HasAPIKey:     apiKey != "",
		APIKeyFromEnv: fromEnv,
	}
	// Discovery before validation: the listing IS the accept-list, so the save
	// cannot decide whether a model is legitimate without it. Resolving here
	// also means the re-render below reuses this probe instead of making a
	// second call to the same endpoint.
	s.resolveModels(r.Context(), &data)
	data.ErrBaseURL = validateLLMBaseURL(data.BaseURL)
	data.ErrEmbedModel = validateLLMModel(data.EmbedModel)
	data.ErrFactsModel = validateLLMModel(data.FactsModel)
	data.ErrAPIKey = validateLLMAPIKey(apiKey)
	// Only when a configurator is wired. Without one there is no endpoint that
	// could have offered anything and no config file to write into, so "that
	// model is not offered" would be a verdict on a question never asked — the
	// honest answer is the unavailable banner a few lines down.
	if s.llmConfig != nil {
		if data.ErrEmbedModel == "" && !llmModelAllowed(data.EmbedModel, cur.EmbedModel, data.Models) {
			data.ErrEmbedModel = "notoffered"
		}
		if data.ErrFactsModel == "" && !llmModelAllowed(data.FactsModel, cur.ChatModel, data.Models) {
			data.ErrFactsModel = "notoffered"
		}
	}
	if data.HasErrors() {
		s.renderLLMSettings(w, r, data)
		return
	}

	if s.llmConfig == nil {
		data.SaveResult = "unavailable"
		s.renderLLMSettings(w, r, data)
		return
	}
	if err := s.llmConfig.ApplyLLM(llm.Settings{
		BaseURL:       data.BaseURL,
		EmbedModel:    data.EmbedModel,
		ChatModel:     data.FactsModel,
		APIKey:        apiKey,
		APIKeyFromEnv: fromEnv,
	}); err != nil {
		s.log.Error("LLM settings save failed", "error", err)
		data.SaveResult = "error"
		s.renderLLMSettings(w, r, data)
		return
	}
	// Endpoint/model names are configuration, never message content — safe to
	// log. The API key value is NEVER logged; only whether one is set and
	// whether this save changed it.
	s.log.Info("LLM settings saved and applied live",
		"base_url", data.BaseURL, "embed_model", data.EmbedModel, "chat_model", data.FactsModel,
		"api_key_set", apiKey != "", "api_key_changed", !keptKey)

	applied := s.llmConfig.CurrentLLM()
	next := llmSettingsData{
		BaseURL:           applied.BaseURL,
		EmbedModel:        applied.EmbedModel,
		FactsModel:        applied.ChatModel,
		HasAPIKey:         applied.APIKey != "",
		APIKeyFromEnv:     applied.APIKeyFromEnv,
		SaveResult:        "ok",
		EmbedModelChanged: s.embedModelChanged(r.Context(), applied.EmbedModel),
	}
	// Carry this response's discovery forward ONLY when it still describes the
	// endpoint that is now live. Discovery ran before ApplyLLM, against the
	// PREVIOUS base URL and key; if the save changed either, those models came
	// from a different server. Reusing them would render the old endpoint's
	// listing as an enabled, selectable set while the client points somewhere
	// else — and omit the models the new endpoint actually offers. Re-probing
	// costs one bounded call on the save that changed the endpoint, and none
	// on the far more common save that did not.
	if applied.BaseURL == cur.BaseURL && keptKey {
		next.Models = data.Models
		next.ModelsAvailable = data.ModelsAvailable
		next.ModelsUnavailable = data.ModelsUnavailable
		next.modelsResolved = true
	}
	s.renderLLMSettings(w, r, next)
}

// handleSettingsLLMTest is POST /settings/llm/test — the "Test connection"
// probe. Gated exactly like the save (checkSetupPOST: same-origin +
// per-session token + body cap, 403 before any work), it reads the SAME
// currently-entered (unsaved) form values, validates them, then probes the
// endpoint WITHOUT persisting or swapping the live client. The result is a
// fixed-enum banner ("ok" / "unavailable" / a classified llm.TestFailure);
// the raw probe error is logged server-side but never echoed into the page
// (it can carry the endpoint URL). The form and effective/entered values
// re-render unchanged so the user can adjust and retry.
func (s *Server) handleSettingsLLMTest(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return // 403 already written; nothing was validated or probed
	}

	// Same secret-field resolution as save: a blank api_key means "use the
	// current key", so the probe verifies the endpoint the user would save.
	cur := s.currentLLM()
	apiKey := strings.TrimSpace(r.PostFormValue("api_key"))
	if apiKey == "" {
		apiKey = cur.APIKey
	}

	data := llmSettingsData{
		BaseURL:    strings.TrimSpace(r.PostFormValue("base_url")),
		EmbedModel: llmModelFromForm(r, "embed_model", cur.EmbedModel),
		FactsModel: llmModelFromForm(r, "facts_model", cur.ChatModel),
		HasAPIKey:  apiKey != "",
	}
	data.ErrBaseURL = validateLLMBaseURL(data.BaseURL)
	data.ErrEmbedModel = validateLLMModel(data.EmbedModel)
	data.ErrFactsModel = validateLLMModel(data.FactsModel)
	data.ErrAPIKey = validateLLMAPIKey(apiKey)
	if data.HasErrors() {
		s.renderLLMSettings(w, r, data)
		return
	}

	if s.llmConfig == nil {
		data.TestResult = "unavailable"
		s.renderLLMSettings(w, r, data)
		return
	}
	result := s.llmConfig.TestLLM(r.Context(), llm.Settings{
		BaseURL:    data.BaseURL,
		EmbedModel: data.EmbedModel,
		ChatModel:  data.FactsModel,
		APIKey:     apiKey,
	})
	if result.Failure != "" {
		s.log.Warn("LLM test connection failed",
			"base_url", data.BaseURL, "embed_model", data.EmbedModel, "chat_model", data.FactsModel,
			"failure", result.Failure, "embed_ok", result.EmbedOK, "chat_ok", result.ChatOK)
		data.TestResult = string(result.Failure)
		s.renderLLMSettings(w, r, data)
		return
	}
	s.log.Info("LLM test connection succeeded",
		"base_url", data.BaseURL, "embed_model", data.EmbedModel, "chat_model", data.FactsModel)
	data.TestResult = "ok"
	s.renderLLMSettings(w, r, data)
}

// renderLLMSettings finishes any LLM-tab response: model discovery (unless a
// handler already ran it), the two option sets, shell (full or boosted
// partial), fresh per-session token, render.
//
// Discovery and option-building live HERE rather than in each handler because
// every response — first paint, a save, a failed validation, a test probe —
// renders the same two model fields, and a handler that forgot to fill them
// would ship a select with nothing in it. One choke point makes that
// unrepresentable.
func (s *Server) renderLLMSettings(w http.ResponseWriter, r *http.Request, data llmSettingsData) {
	const title = "LLM · msgbrowse"
	if !data.modelsResolved {
		s.resolveModels(r.Context(), &data)
	}
	data.EmbedOptions = llmModelOptions(data.Models, data.EmbedModel, "Off — semantic search disabled")
	data.FactsOptions = llmModelOptions(data.Models, data.FactsModel, "Off — AI features disabled")
	if isPartialRequest(r) {
		data.baseData = partialBase(title, 0)
	} else {
		base, err := s.baseData(r.Context(), title, 0)
		if err != nil {
			s.serverError(w, err)
			return
		}
		data.baseData = base
	}
	tok, err := s.setupTokens.mint()
	if err != nil {
		s.serverError(w, err)
		return
	}
	data.SetupToken = tok
	s.render(w, r, "llmsettings", data)
}

// validateLLMBaseURL checks the endpoint URL: required, bounded length, no
// control characters, and it must parse with an http/https scheme and a host.
// Returns "" or a fixed error enum ("required" / "toolong" / "invalid" /
// "scheme").
func validateLLMBaseURL(raw string) string {
	if raw == "" {
		return "required"
	}
	if len(raw) > llmBaseURLMaxLen {
		return "toolong"
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return "invalid"
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "scheme"
	}
	if u.Host == "" {
		return "invalid"
	}
	return ""
}

// validateLLMAPIKey checks the API key: bounded length and printable runes
// only (keys are opaque ASCII/base64-ish tokens). Empty is VALID — many local
// proxies need none, and an empty submission means "keep the current key"
// upstream. Returns "" or a fixed error enum ("toolong" / "invalid").
func validateLLMAPIKey(k string) string {
	if len(k) > llmAPIKeyMaxLen {
		return "toolong"
	}
	for _, r := range k {
		if !unicode.IsPrint(r) {
			return "invalid"
		}
	}
	return ""
}

// validateLLMModel checks a model name: bounded length, printable runes only.
// Empty is VALID — an empty embed model means semantic search off, an empty
// facts model means the facts/journal features are unconfigured. Returns ""
// or a fixed error enum ("toolong" / "invalid").
func validateLLMModel(m string) string {
	if len(m) > llmModelMaxLen {
		return "toolong"
	}
	for _, r := range m {
		if !unicode.IsPrint(r) {
			return "invalid"
		}
	}
	return ""
}

// handleSettingsLLMModels is POST /settings/llm/models — the "Refresh models"
// trigger. Gated exactly like the save (checkSetupPOST: same-origin +
// per-session token + body cap, 403 before any work), it reads the currently
// entered (unsaved) base URL and API key, probes the endpoint's /v1/models
// listing, and re-renders the tab with the result. The form fields are echoed
// back unchanged so the user can adjust the URL and retry.
func (s *Server) handleSettingsLLMModels(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return
	}

	cur := s.currentLLM()
	apiKey := strings.TrimSpace(r.PostFormValue("api_key"))
	if apiKey == "" {
		apiKey = cur.APIKey
	}

	data := llmSettingsData{
		BaseURL:    strings.TrimSpace(r.PostFormValue("base_url")),
		EmbedModel: llmModelFromForm(r, "embed_model", cur.EmbedModel),
		FactsModel: llmModelFromForm(r, "facts_model", cur.ChatModel),
		HasAPIKey:  apiKey != "",
	}
	data.ErrBaseURL = validateLLMBaseURL(data.BaseURL)
	data.ErrEmbedModel = validateLLMModel(data.EmbedModel)
	data.ErrFactsModel = validateLLMModel(data.FactsModel)
	data.ErrAPIKey = validateLLMAPIKey(apiKey)
	if data.HasErrors() {
		// Validation errors are not a models-list concern — re-render cleanly
		// and the user's next save/test will surface them.
		s.renderLLMSettings(w, r, data)
		return
	}

	// One discovery path for every surface: resolveModels decides live-vs-
	// disabled and why. The button's own banner is the same verdict said out
	// loud, because this render is the one the user asked for.
	s.resolveModels(r.Context(), &data)
	if data.ModelsAvailable {
		s.log.Info("LLM model listing succeeded",
			"base_url", data.BaseURL, "models", len(data.Models))
		data.ModelsResult = "ok"
	} else {
		s.log.Warn("LLM model listing failed",
			"base_url", data.BaseURL, "reason", data.ModelsUnavailable)
		data.ModelsResult = data.ModelsUnavailable
	}
	s.renderLLMSettings(w, r, data)
}

// embedModelChanged reports whether the given embed model differs from the
// model that was last used by the semantic index. When true, the template
// shows a warning that the index needs a rebuild.
func (s *Server) embedModelChanged(ctx context.Context, model string) bool {
	if model == "" {
		return false
	}
	run, err := s.store.LatestEmbedRun(ctx)
	if err != nil || run == nil {
		return false
	}
	return run.Model != "" && run.Model != model
}
