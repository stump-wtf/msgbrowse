// In-app journal building (#240): the Build / Rebuild controls on the
// Settings → Status tab (consolidated there out of the Journal page — the
// journal calendar is a reading surface, not a pipeline console), which run
// the mechanical day layer and the LLM digests without dropping to the
// `msgbrowse journal` CLI.
//
// This is deliberately the same machinery as the semantic-index controls (#191,
// semanticindex.go): the web layer owns the concurrency guard and the DETACHED
// goroutine, the POSTs carry the same privileged-POST gate (same-origin +
// per-session setup token + MaxBytesReader via checkSetupPOST), and the page
// re-renders with a fixed-enum result banner rather than request-derived prose.
//
// One deliberate DIVERGENCE from startReindex: an unset chat model does NOT
// refuse the job. The mechanical day layer is real, useful, egress-free work
// (REQ-0016-001) and building it is exactly what an unconfigured user needs. So
// a missing model is reported as an explanatory outcome while the build still
// runs, where a model-less index reset would have been destructive.
package web

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// JournalBuilder is the live seam behind the journal build controls. serve and
// the desktop shell wire an internal/journal.Builder over the process's shared
// store + llm.Holder; tests wire a fake. With none wired the controls report
// themselves unavailable rather than pretending.
//
// Its parameters are primitives only: internal/web already imports
// internal/journal (for journal.Digest / journal.Moods), so a shared options
// struct would close an import cycle.
type JournalBuilder interface {
	// ChatModel returns the currently configured chat model, "" when unset. The
	// build still runs then — only the digests are skipped.
	ChatModel() string
	// DigestEnabled reports whether the LLM digest pass is configured on.
	DigestEnabled() bool
	// RunJournal executes one pass. day == "" builds the mechanical layer plus
	// every missing digest; day != "" rebuilds exactly that day. regenerate
	// clears cached digests in scope first. It blocks until the pass finishes;
	// the web layer calls it in a detached background goroutine. ctx is NOT the
	// request context — the job outlives the HTTP request.
	RunJournal(ctx context.Context, day string, regenerate bool) error
	// RescoreDay re-derives exactly one UTC day's sentiment scores (issue
	// #441) — the affect half of the day card's Refresh, which the digest
	// rebuild alone did not cover. Same detached-context contract as
	// RunJournal. Optional capability: implementors without a sentiment
	// engine return an error and the chain (#453) surfaces it.
	RescoreDay(ctx context.Context, day string) error
}

// SetJournalBuilder wires the journal job runner. Call it after NewServer and
// before serving begins — handlers read s.journalBuilder without locking, so
// late wiring would race.
func (s *Server) SetJournalBuilder(b JournalBuilder) { s.journalBuilder = b }

// Fixed-enum outcome of a Build / Rebuild request, mapped to a banner by
// journal.html. Never request-derived, so no user input can reach the banner
// selection or a class attribute.
const (
	journalResultStarted     = "started"     // a build was started
	journalResultRebuilt     = "rebuilt"     // a whole-archive regenerate was started
	journalResultDay         = "day"         // a single-day rebuild was started
	journalResultInProgress  = "inprogress"  // a run is already going; the click coalesced
	journalResultNoModel     = "nomodel"     // no chat model — day layer only
	journalResultDigestOff   = "digestoff"   // digests disabled in config — day layer only
	journalResultUnavailable = "unavailable" // no builder wired (browser / no-op mode)
	journalResultBadDay      = "badday"      // day failed validation or is not in journal_days
)

const (
	// journalRunHistoryLimit caps the run-history table. Recent runs tell the
	// track record without turning the card into an unbounded log.
	journalRunHistoryLimit = 8
	// journalRunStaleAfter is how long an unfinished run's heartbeat may go cold
	// before it reads as interrupted rather than live — same reasoning as
	// embedRunStaleAfter.
	journalRunStaleAfter = 30 * time.Minute
)

// journalJobRunning reports whether a web-initiated journal job is in flight in
// THIS process (the single-flight flag, distinct from the journal_runs heartbeat
// which also catches a concurrent CLI `msgbrowse journal`).
func (s *Server) journalJobRunning() bool {
	s.journalMu.Lock()
	defer s.journalMu.Unlock()
	return s.journalRunning
}

// startJournal launches the journal job under the single-flight guard and
// returns the fixed-enum outcome.
//
// The guard is two-layered because there are two ways a run can already be
// going. s.journalRunning catches a second click in THIS process. The
// journal_runs heartbeat catches a run started elsewhere — a `msgbrowse journal`
// CLI against the same SQLite file. The template disables the buttons on that
// heartbeat, but that is client-side only: a page rendered before the CLI
// started, or a direct POST, would otherwise sail past the in-memory flag and
// start a second writer that pays for the same digests twice. A run whose
// heartbeat has gone stale reads as crashed, not live, so a Build can still
// resume after a killed CLI run.
//
// This matters more here than for the index: digests are BILLABLE outbound LLM
// calls, so a raced double-start costs real money, not just duplicated work.
func (s *Server) startJournal(ctx context.Context, day string, regenerate bool) string {
	b := s.journalBuilder
	if b == nil {
		return journalResultUnavailable
	}
	// A read error here is not a reason to refuse: fall through to the in-memory
	// guard rather than blocking a build on a transient store hiccup.
	if run, err := s.store.LatestJournalRun(ctx); err == nil && run != nil &&
		run.InFlight() && time.Since(run.UpdatedAt) <= journalRunStaleAfter {
		return journalResultInProgress
	}

	s.journalMu.Lock()
	if s.journalRunning {
		s.journalMu.Unlock()
		return journalResultInProgress
	}
	s.journalRunning = true
	s.journalMu.Unlock()

	go func() {
		// Detached: NOT the request context, which dies with the response and
		// would cancel every build mid-flight. journal.Run writes its terminal
		// journal_runs row even on abort, so the heartbeat never sticks.
		defer func() {
			s.journalMu.Lock()
			s.journalRunning = false
			s.journalMu.Unlock()
		}()
		if err := b.RunJournal(context.Background(), day, regenerate); err != nil {
			s.log.Error("journal job failed", "error", err, "day", day, "regenerate", regenerate)
		}
	}()

	// The job IS running by now; these outcomes describe what it will do, and
	// the two "only the day layer" cases explain why no digests will appear.
	switch {
	case b.ChatModel() == "":
		return journalResultNoModel
	case !b.DigestEnabled():
		return journalResultDigestOff
	case day != "":
		return journalResultDay
	case regenerate:
		return journalResultRebuilt
	default:
		return journalResultStarted
	}
}

// startJournalDay validates a per-day Rebuild target BEFORE starting anything.
// The day must parse as a date AND already exist in journal_days, so a typo or
// a hand-crafted POST can never spawn a job over an arbitrary or unbounded
// range. Returns journalResultBadDay without starting a job otherwise.
func (s *Server) startJournalDay(ctx context.Context, day string) string {
	if !isValidDay(day) {
		return journalResultBadDay
	}
	if _, ok, err := s.store.GetJournalDay(ctx, day); err != nil || !ok {
		return journalResultBadDay
	}
	return s.startJournal(ctx, day, true)
}

// journalCardData drives the journal build card fragment on the Settings →
// Journal tab: the build status snapshot plus the token its forms submit.
type journalCardData struct {
	Build      journalBuildData
	SetupToken string
}

// handleJournalBuildProgress is GET /journal/build/progress — the live-refresh
// endpoint behind the build card. It re-renders JUST the card, so the card's
// hx-get swaps itself every couple of seconds WHILE a run is in flight and stops
// once the fresh HTML no longer carries the poll trigger. Without it the
// "Building… N days digested so far" line the card renders would be frozen at
// page-load time — it would claim to report progress and never move.
//
// It is a read-only GET (no token needed to observe) and mints a token for the
// embedded forms ONLY when they render enabled — i.e. no run is in flight. A 2s
// poll that minted every tick would push ~1800 tokens/hour through a set capped
// at setupTokenCap (1024), evicting the still-valid tokens armed on other open
// pages and 403ing their next save mid-run. While a run is in flight the buttons
// are disabled, so the token would go unused anyway; the poll that observes the
// run finish renders enabled buttons and mints one then.
func (s *Server) handleJournalBuildProgress(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	build, err := s.journalBuildStatus(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	data := journalCardData{Build: build}
	if s.journalBuilder != nil && !build.Busy() {
		tok, terr := s.setupTokens.mint()
		if terr != nil {
			s.serverError(w, terr)
			return
		}
		data.SetupToken = tok
	}
	s.renderFragment(w, "journal_build_card", data)
}

// handleJournalBuild is POST /journal/build — fill the mechanical day layer and
// every missing digest. The build controls live on the Settings → Journal tab
// (#368: one tab per pipeline), so the POST re-renders THAT page with the
// fixed-enum banner. The route itself is unchanged.
func (s *Server) handleJournalBuild(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return // 403 already written; no job started, no LLM call made
	}
	s.renderJournalSettings(w, r, s.startJournal(r.Context(), "", false))
}

// handleJournalRebuildAll is POST /journal/rebuild — clear every cached digest
// and re-derive. Billable, which is why the control states the count first.
func (s *Server) handleJournalRebuildAll(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return
	}
	s.renderJournalSettings(w, r, s.startJournal(r.Context(), "", true))
}

// handleJournalRebuildDay is POST /journal/rebuild/day — regenerate exactly one
// day's digest. The day travels as a FORM FIELD, not a path wildcard, so it is
// validated uniformly and never lands in a URL htmx pushes or a proxy logs.
func (s *Server) handleJournalRebuildDay(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return
	}
	result := s.startJournalDay(r.Context(), strings.TrimSpace(r.PostFormValue("day")))
	// The day card's Refresh button (#440) posts with from=card and expects
	// the JOURNAL page back — banner on the day it acted on, not the settings
	// tab. The settings form omits the field and gets its own tab re-rendered.
	if r.PostFormValue("from") == "card" {
		s.renderJournalPage(w, r, result)
		return
	}
	s.renderJournalSettings(w, r, result)
}
