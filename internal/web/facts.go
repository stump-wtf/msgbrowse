// In-app fact extraction (#366): the Extract / Re-extract controls on the
// Settings → Facts tab, which run internal/facts over the live store and LLM
// client without dropping to the `msgbrowse facts` CLI.
//
// Until this landed, extraction had NO in-app entry point at all — no route, no
// control, nothing. `facts.Run` was reachable only from `msgbrowse facts` and
// the sync command, so on a real archive contact_facts and fact_state were both
// empty while the contact page rendered its facts section, grouped by
// facts.Categories, over an empty set forever. The feature was shipped and
// unreachable.
//
// This is deliberately the same machinery as the journal-build and
// semantic-index controls (journalbuild.go, semanticindex.go): the web layer
// owns the concurrency guard and the DETACHED goroutine, the POSTs carry the
// same privileged-POST gate (same-origin + per-session setup token +
// MaxBytesReader via checkSetupPOST), and the page re-renders with a
// fixed-enum result banner rather than request-derived prose.
//
// One deliberate DIVERGENCE from startJournal, and it goes the other way: a
// missing chat model REFUSES the job here. The journal can run without one
// because its mechanical day layer is real, useful, egress-free work
// (REQ-0016-001), so it builds that and explains what was skipped. Fact
// extraction has NO model-free layer — every fact is the model's output, and a
// run without one would do nothing at all while writing a fact_runs row saying
// it ran, which is worse than not starting: the next reader could not tell an
// unconfigured archive from a genuinely factless one. So this follows
// startReindex instead: refuse, and say why.
//
// Governing: SPEC-0005 (contact-facts) REQ-0005-001 (extraction stays a
// deliberate, opt-in pass performing the only egress — a click through a
// privileged POST is as deliberate as a command, and neither import nor a plain
// page render ever triggers one), REQ-0005-004 (a normal run is incremental off
// fact_state, so Reset is passed ONLY by the reset control), REQ-0005-005 (the
// exclude list is applied by internal/facts before any content is read);
// ADR-0011; SPEC-0013 §Security (the reused privileged-POST gate).
//
// @joestump-agent 08/23/2026 - Added (#366).
package web

import (
	"context"
	"net/http"
	"time"
)

// FactsExtractor is the live seam behind the fact-extraction controls. serve
// and the desktop shell wire an internal/facts.Extractor over the process's
// shared store + llm.Holder; tests wire a fake. With none wired the controls
// report themselves unavailable rather than pretending.
//
// Its parameters are primitives only, mirroring JournalBuilder: internal/web
// already imports internal/facts (for facts.Categories on the contact page), so
// a shared options struct would close an import cycle.
type FactsExtractor interface {
	// ChatModel returns the currently configured chat model, "" when unset.
	// Unlike the journal's, an empty value means the run is REFUSED — there is
	// no useful model-free layer to fall back on.
	ChatModel() string
	// RunFacts executes one extraction pass, wiping stored facts and cursors
	// first when reset is true. It blocks until the pass finishes; the web layer
	// calls it in a detached background goroutine. ctx is NOT the request
	// context — the job outlives the HTTP request.
	RunFacts(ctx context.Context, reset bool) error
}

// SetFactsExtractor wires the fact-extraction job runner. Call it after
// NewServer and before serving begins — handlers read s.factsExtractor without
// locking, so late wiring would race.
func (s *Server) SetFactsExtractor(e FactsExtractor) { s.factsExtractor = e }

// Fixed-enum outcome of an Extract / Re-extract request, mapped to a banner by
// facts_settings.html. Never request-derived, so no user input can reach the
// banner selection or a class attribute.
const (
	factsResultStarted     = "started"     // an incremental extraction was started
	factsResultReset       = "reset"       // a wipe-and-re-extract was started
	factsResultInProgress  = "inprogress"  // a run is already going; the click coalesced
	factsResultNoModel     = "nomodel"     // no chat model — NOTHING was started
	factsResultUnavailable = "unavailable" // no extractor wired (browser / no-op mode)
)

// factsJobRunning reports whether a web-initiated extraction is in flight in
// THIS process (the single-flight flag, distinct from the fact_runs heartbeat
// which also catches a concurrent CLI `msgbrowse facts`).
func (s *Server) factsJobRunning() bool {
	s.factsMu.Lock()
	defer s.factsMu.Unlock()
	return s.factsRunning
}

// startFacts launches the extraction job under the single-flight guard and
// returns the fixed-enum outcome. It refuses (without starting anything) when
// no extractor is wired or no chat model is configured, and coalesces a start
// that races a running job into "in progress" rather than a duplicate writer.
//
// The guard is two-layered because there are two ways a run can already be
// going. s.factsRunning catches a second click in THIS process. The fact_runs
// heartbeat catches a run started elsewhere — a `msgbrowse facts` CLI against
// the same SQLite file. The card disables its buttons on that heartbeat, but
// that is client-side only: a page rendered before the CLI started, or a direct
// POST, would otherwise sail past the in-memory flag and start a second writer
// that pays for the same conversations twice. A run whose heartbeat has gone
// stale reads as crashed, not live, so an Extract can still resume after a
// killed CLI run.
//
// reset is a fixed BOOLEAN chosen by the route, not a parameter: there is no
// user-supplied scope on this endpoint at all, so a hand-crafted POST cannot
// widen the job beyond what the two buttons offer.
func (s *Server) startFacts(ctx context.Context, reset bool) string {
	e := s.factsExtractor
	if e == nil {
		return factsResultUnavailable
	}
	if e.ChatModel() == "" {
		// Refuse rather than start. See the divergence note in the file header:
		// with no model there is no useful work to do, and a reset that cleared
		// every fact before discovering that would be actively destructive.
		return factsResultNoModel
	}
	// A read error here is not a reason to refuse: fall through to the in-memory
	// guard rather than blocking extraction on a transient store hiccup.
	if run, err := s.store.LatestFactRun(ctx); err == nil && run != nil &&
		run.InFlight() && time.Since(run.UpdatedAt) <= factsRunStaleAfter {
		return factsResultInProgress
	}

	s.factsMu.Lock()
	if s.factsRunning {
		s.factsMu.Unlock()
		return factsResultInProgress
	}
	s.factsRunning = true
	s.factsMu.Unlock()

	go func() {
		// Detached: NOT the request context, which dies with the response and
		// would cancel every extraction mid-flight. facts.Run writes its terminal
		// fact_runs row even on abort, so the heartbeat never sticks.
		defer func() {
			s.factsMu.Lock()
			s.factsRunning = false
			s.factsMu.Unlock()
		}()
		if err := e.RunFacts(context.Background(), reset); err != nil {
			s.log.Error("fact extraction job failed", "error", err, "reset", reset)
		}
	}()

	if reset {
		return factsResultReset
	}
	return factsResultStarted
}

// factsCardData drives the extraction card fragment on the Settings → Facts
// tab: the card's status snapshot plus the token its forms submit. It mirrors
// journalCardData, so the fragment and the full page feed the card identically.
type factsCardData struct {
	Build      factsBuildData
	SetupToken string
}

// handleFactsProgress is GET /facts/run/progress — the live-refresh
// endpoint behind the extraction card. It re-renders JUST the card, so the
// card's hx-get swaps itself every couple of seconds WHILE a run is in flight
// and stops once the fresh HTML no longer carries the poll trigger. Without it
// the "N conversations processed so far" line would be frozen at page-load
// time — it would claim to report progress and never move.
//
// It is a read-only GET (no token needed to observe) and mints a token for the
// embedded forms ONLY when they render enabled — i.e. no run is in flight. A 2s
// poll that minted every tick would push ~1800 tokens/hour through a set capped
// at setupTokenCap (1024), evicting the still-valid tokens armed on other open
// pages and 403ing their next save mid-run. While a run is in flight the
// buttons are disabled, so the token would go unused anyway; the poll that
// observes the run finish renders enabled buttons and mints one then.
func (s *Server) handleFactsProgress(w http.ResponseWriter, r *http.Request) {
	build, err := s.factsBuildStatus(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	data := factsCardData{Build: build}
	if s.factsExtractor != nil && !build.Busy() {
		tok, terr := s.setupTokens.mint()
		if terr != nil {
			s.serverError(w, terr)
			return
		}
		data.SetupToken = tok
	}
	s.renderFragment(w, "facts_build_card", data)
}

// handleFactsRun is POST /facts/run — extract from everything the
// stored cursors have not covered yet. Incremental by construction: Reset is
// false, so facts.Run resumes from each conversation's fact_state cursor and
// re-reads nothing already paid for (REQ-0005-004).
func (s *Server) handleFactsRun(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return // 403 already written; no job started, no LLM call made
	}
	s.renderFactsSettings(w, r, s.startFacts(r.Context(), false))
}

// handleFactsReset is POST /facts/reset — clear every stored fact
// and cursor, then re-extract from scratch. Billable over the whole archive,
// which is why the control states the conversation count and the fact count it
// is about to discard before the click.
func (s *Server) handleFactsReset(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return
	}
	s.renderFactsSettings(w, r, s.startFacts(r.Context(), true))
}
