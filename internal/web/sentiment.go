// In-app IPIP sentiment scoring (#367): the Score / Rescore controls on the
// Settings → Sentiment tab, which run internal/sentiment over the live store and
// LLM client without dropping to the `msgbrowse sentiment` CLI.
//
// Until this landed the pipeline had NO in-app entry point and NO consumer at
// all — no route, no control, and `grep -r IPIP internal/web` returned nothing.
// On the live archive that showed up exactly as you would expect: 0 rows in
// message_sentiment, 0 rows in sentiment_state, across 2,438 conversations. The
// engine, the 300-item IPIP lexicon, the schema, ADR-0028 and SPEC-0027 were all
// shipped; scoring had simply never run once, and nothing would have displayed
// it if it had.
//
// This is deliberately the same machinery as the journal-build, semantic-index
// and fact-extraction controls (journalbuild.go, semanticindex.go, facts.go):
// the web layer owns the concurrency guard and the DETACHED goroutine, the POSTs
// carry the same privileged-POST gate (same-origin + per-session setup token +
// MaxBytesReader via checkSetupPOST), and the page re-renders with a fixed-enum
// result banner rather than request-derived prose.
//
// THE COST STATEMENT IS NOT OPTIONAL HERE, and the reason is worth writing down
// because the issue this implements asserted the opposite. #367 described the
// lexicon pass as "deterministic and local", said it "has no LLM cost and can
// run freely", and listed "Egress: none" on its security checklist. None of that
// is true of the shipped implementation. sentiment.Run REFUSES without a chat
// model and makes an llm.Chat call per batch; the IPIP lexicon is the anchor set
// rendered into the system prompt (internal/sentiment/prompt.go), not a local
// scorer. ADR-0028 lists "classical local lexicon (VADER/NRC-style) — no egress
// at all" under REJECTED alternatives and states plainly that corpus scoring is
// "the most expensive extraction". So this control gets the same
// state-the-price-before-the-click posture as the journal's "Rebuild all N
// digests" and the facts tab's re-extract, not a run-freely affordance.
//
// A missing chat model REFUSES the job, following startFacts rather than
// startJournal. The journal can run without one because its mechanical day layer
// is real, useful, egress-free work; sentiment has no model-free layer at all —
// every score is the model's output. A run without one would do nothing while
// writing a sentiment_runs row saying it ran, which is worse than not starting.
//
// Governing: SPEC-0027 (sentiment) — scoring is a deliberate, opt-in pass and a
// click through a privileged POST is as deliberate as a command; the
// per-conversation cursor makes a normal run incremental, so Reset is passed
// ONLY by the reset control; ADR-0028 (the single llm.Chat egress to
// llm.base_url — this adds no new outbound path, it makes the existing one
// reachable); SPEC-0004 REQ-0004-010 (one settings tab per pipeline);
// SPEC-0013 §Security (the reused privileged-POST gate).
//
// @joestump-agent 08/23/2026 - Added (#367).
package web

import (
	"context"
	"net/http"
	"time"
)

// SentimentScorer is the live seam behind the sentiment controls. serve and the
// desktop shell wire an internal/sentiment.Scorer over the process's shared
// store + llm.Holder; tests wire a fake. With none wired the controls report
// themselves unavailable rather than pretending.
//
// Its parameters are primitives only, mirroring JournalBuilder and
// FactsExtractor: internal/web imports internal/sentiment (for the affect
// taxonomy the consumer surfaces fold with), so a shared options struct would
// close an import cycle.
type SentimentScorer interface {
	// ChatModel returns the currently configured chat model, "" when unset.
	// Unlike the journal's, an empty value means the run is REFUSED — there is
	// no useful model-free layer to fall back on.
	ChatModel() string
	// LexiconVersion returns the curation this build scores under. Together with
	// ChatModel it names the generation every read-side aggregate filters on.
	LexiconVersion() string
	// RunSentiment executes one scoring pass, wiping stored scores and cursors
	// first when reset is true (never opt-outs). It blocks until the pass
	// finishes; the web layer calls it in a detached background goroutine. ctx is
	// NOT the request context — the job outlives the HTTP request.
	RunSentiment(ctx context.Context, reset bool) error
}

// SetSentimentScorer wires the scoring job runner. Call it after NewServer and
// before serving begins — handlers read s.sentimentScorer without locking, so
// late wiring would race.
func (s *Server) SetSentimentScorer(sc SentimentScorer) { s.sentimentScorer = sc }

// Fixed-enum outcome of a Score / Rescore request, mapped to a banner by
// sentiment_settings.html. Never request-derived, so no user input can reach the
// banner selection or a class attribute.
const (
	sentimentResultStarted     = "started"     // an incremental scoring pass was started
	sentimentResultReset       = "reset"       // a wipe-and-rescore was started
	sentimentResultInProgress  = "inprogress"  // a run is already going; the click coalesced
	sentimentResultNoModel     = "nomodel"     // no chat model — NOTHING was started
	sentimentResultUnavailable = "unavailable" // no scorer wired (browser / no-op mode)
)

// sentimentJobRunning reports whether a web-initiated scoring pass is in flight
// in THIS process (the single-flight flag, distinct from the sentiment_runs
// heartbeat which also catches a concurrent CLI `msgbrowse sentiment`).
func (s *Server) sentimentJobRunning() bool {
	s.sentimentMu.Lock()
	defer s.sentimentMu.Unlock()
	return s.sentimentRunning
}

// startSentiment launches the scoring job under the single-flight guard and
// returns the fixed-enum outcome. It refuses (without starting anything) when no
// scorer is wired or no chat model is configured, and coalesces a start that
// races a running job into "in progress" rather than a duplicate writer.
//
// The guard is two-layered because there are two ways a run can already be
// going. s.sentimentRunning catches a second click in THIS process. The
// sentiment_runs heartbeat catches a run started elsewhere — a `msgbrowse
// sentiment` CLI against the same SQLite file. The card disables its buttons on
// that heartbeat, but that is client-side only: a page rendered before the CLI
// started, or a direct POST, would otherwise sail past the in-memory flag and
// start a second writer that pays for the same conversations twice. A run whose
// heartbeat has gone stale reads as crashed, not live, so a Score can still
// resume after a killed CLI run.
//
// reset is a fixed BOOLEAN chosen by the route, not a parameter: there is no
// user-supplied scope on this endpoint at all, so a hand-crafted POST cannot
// widen the job beyond what the two buttons offer.
func (s *Server) startSentiment(ctx context.Context, reset bool) string {
	sc := s.sentimentScorer
	if sc == nil {
		return sentimentResultUnavailable
	}
	if sc.ChatModel() == "" {
		// Refuse rather than start. See the divergence note in the file header:
		// with no model there is no work to do at all, and a reset that cleared
		// every score before discovering that would be actively destructive.
		return sentimentResultNoModel
	}
	// A read error here is not a reason to refuse: fall through to the in-memory
	// guard rather than blocking scoring on a transient store hiccup.
	if run, err := s.store.LatestSentimentRun(ctx); err == nil && run != nil &&
		run.InFlight() && time.Since(run.UpdatedAt) <= sentimentRunStaleAfter {
		return sentimentResultInProgress
	}

	s.sentimentMu.Lock()
	if s.sentimentRunning {
		s.sentimentMu.Unlock()
		return sentimentResultInProgress
	}
	s.sentimentRunning = true
	s.sentimentMu.Unlock()

	go func() {
		// Detached: NOT the request context, which dies with the response and
		// would cancel every scoring pass mid-flight. sentiment.Run writes its
		// terminal sentiment_runs row even on abort, so the heartbeat never
		// sticks.
		defer func() {
			s.sentimentMu.Lock()
			s.sentimentRunning = false
			s.sentimentMu.Unlock()
		}()
		if err := sc.RunSentiment(context.Background(), reset); err != nil {
			s.log.Error("sentiment scoring job failed", "error", err, "reset", reset)
		}
	}()

	if reset {
		return sentimentResultReset
	}
	return sentimentResultStarted
}

// sentimentCardData drives the scoring card fragment on the Settings →
// Sentiment tab: the card's status snapshot plus the token its forms submit. It
// mirrors factsCardData, so the fragment and the full page feed the card
// identically.
type sentimentCardData struct {
	Build      sentimentBuildData
	SetupToken string
}

// handleSentimentProgress is GET /sentiment/run/progress — the live-refresh
// endpoint behind the scoring card. It re-renders JUST the card, so the card's
// hx-get swaps itself every couple of seconds WHILE a run is in flight and stops
// once the fresh HTML no longer carries the poll trigger. Without it the
// "N conversations scored so far" line would be frozen at page-load time — it
// would claim to report progress and never move.
//
// It is a read-only GET (no token needed to observe) and mints a token for the
// embedded forms ONLY when they render enabled — i.e. no run is in flight. A 2s
// poll that minted every tick would push ~1800 tokens/hour through a set capped
// at setupTokenCap (1024), evicting the still-valid tokens armed on other open
// pages and 403ing their next save mid-run. While a run is in flight the buttons
// are disabled, so the token would go unused anyway; the poll that observes the
// run finish renders enabled buttons and mints one then.
func (s *Server) handleSentimentProgress(w http.ResponseWriter, r *http.Request) {
	build, err := s.sentimentBuildStatus(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	data := sentimentCardData{Build: build}
	if s.sentimentScorer != nil && !build.Busy() {
		tok, terr := s.setupTokens.mint()
		if terr != nil {
			s.serverError(w, terr)
			return
		}
		data.SetupToken = tok
	}
	s.renderFragment(w, "sentiment_build_card", data)
}

// handleSentimentRun is POST /sentiment/run — score everything the stored
// cursors have not covered yet. Incremental by construction: Reset is false, so
// sentiment.Run resumes from each conversation's sentiment_state cursor and
// re-reads nothing already paid for.
func (s *Server) handleSentimentRun(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return // 403 already written; no job started, no LLM call made
	}
	s.renderSentimentSettings(w, r, s.startSentiment(r.Context(), false))
}

// handleSentimentReset is POST /sentiment/reset — clear every stored score and
// cursor, then rescore from scratch. Billable over the whole archive, which is
// why the control states the conversation count and the score count it is about
// to discard before the click. Opt-outs are deliberately NOT cleared by a reset
// (see store.ResetSentiment).
func (s *Server) handleSentimentReset(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return
	}
	s.renderSentimentSettings(w, r, s.startSentiment(r.Context(), true))
}
