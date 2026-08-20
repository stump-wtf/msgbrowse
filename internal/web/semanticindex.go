// In-app semantic-index management (issue #191): the Status page's "Build" and
// "Reset & rebuild" controls that kick off (or clear-then-run) the embedding
// pass without dropping to the `msgbrowse embed` CLI. Before this, serve and
// desktop could show coverage ("0 of N") but offered no way to act on it.
//
// The web layer owns the concurrency guard and the DETACHED goroutine: a job
// runs under a background context so it outlives the HTTP request that started
// it, and a single-flight flag makes a second "Build" while one is running a
// no-op rather than a duplicate SQLite writer against the same file (the
// onboard Runner's per-source guard idea, but here one global job). The wired
// Indexer (internal/embed.Indexer) supplies the live store + client and the
// reset primitive; browser / no-op mode wires none and the controls render an
// "unavailable" state instead of 500ing.
//
// The Build/Reset POSTs carry the SAME privileged-POST gate as every other
// mutating endpoint — same-origin + per-session setup token + MaxBytesReader
// via checkSetupPOST — and re-render the Status page with a fixed-enum result
// banner (the LLM-save pattern), never request-derived prose.
package web

import (
	"context"
	"net/http"
	"time"
)

// Indexer is the live seam behind the semantic-index controls (the
// SetLLMConfig / SetEnabler pattern). serve and the desktop shell wire an
// internal/embed.Indexer over the process's shared store + llm.Holder; tests
// wire a fake. With none wired the controls report themselves unavailable
// rather than pretending.
type Indexer interface {
	// EmbedModel returns the currently configured embedding model, "" when
	// unset (semantic search off). The web layer refuses to start a run then.
	EmbedModel() string
	// RunEmbed runs one embedding pass over the live store + client, clearing
	// the index first when reset is true. It blocks until the pass finishes;
	// the web layer calls it in a detached background goroutine. ctx is NOT the
	// request context — the job outlives the HTTP request.
	RunEmbed(ctx context.Context, reset bool) error
	// EmbedQuery embeds a single search query with the live client + embed
	// model so the Search page can run semantic / hybrid queries against the
	// index (the same live-holder read as RunEmbed). Returns nil (not an error)
	// when embedding is unavailable or fails, so the caller degrades to
	// keyword-only rather than 500ing — mirroring MCP's embedQuery.
	EmbedQuery(ctx context.Context, query string) []float32
}

// SetIndexer wires the semantic-index job runner. Call it after NewServer and
// before serving begins — handlers read s.indexer without locking, so late
// wiring would race (the guard's own mutex protects only the in-flight flag).
func (s *Server) SetIndexer(ix Indexer) { s.indexer = ix }

// embedRunHistoryLimit caps the Status page's index run-history table. Recent
// runs tell the track record without turning the card into an unbounded log.
const embedRunHistoryLimit = 8

// handleStatusIndexProgress is GET /status/index/progress — the live-refresh
// endpoint behind the semantic-index card's progress bar. It re-renders JUST
// the card (coverage, in-progress line, run history, controls) as a fragment,
// so the card's hx-poll swaps itself every couple seconds WHILE a run is in
// flight and stops once the fresh HTML no longer carries the poll trigger.
//
// It is a read-only GET (no token needed to observe) and mints a token for the
// embedded Build / Reset forms ONLY when those forms render enabled — i.e. no
// run is in flight. A 2s poll that minted every tick would push ~1800
// tokens/hour through a set capped at setupTokenCap (1024), evicting the
// still-valid tokens armed on other open pages (Providers, Settings → LLM) and
// 403ing their next save mid-run. While a run is in flight the buttons are
// disabled, so the token would go unused anyway; the poll that observes the run
// finish renders enabled buttons and mints one then.
func (s *Server) handleStatusIndexProgress(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	embedding, err := s.overviewEmbedding(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	history, err := s.embedRunHistory(ctx, embedRunHistoryLimit)
	if err != nil {
		s.serverError(w, err)
		return
	}
	data := searchIndexData{
		Embedding:      embedding,
		History:        history,
		IndexAvailable: s.indexer != nil,
		IndexRunning:   s.indexJobRunning(),
	}
	if s.indexer != nil && !data.Embedding.InProgress && !data.IndexRunning {
		tok, err := s.setupTokens.mint()
		if err != nil {
			s.serverError(w, err)
			return
		}
		data.SetupToken = tok
	}
	s.renderFragment(w, "semantic_index_card", data)
}

// Fixed-enum result of a Build / Reset-&-rebuild request, mapped to a banner by
// search_index.html. Never request-derived.
const (
	indexResultStarted     = "started"     // a build job was started
	indexResultReset       = "reset"       // a reset-and-rebuild job was started
	indexResultInProgress  = "inprogress"  // a job is already running; the click coalesced
	indexResultNoModel     = "nomodel"     // no embed model configured — nothing started
	indexResultUnavailable = "unavailable" // no Indexer wired (browser / no-op mode)
	indexResultError       = "error"       // a start-time error (currently unused; reserved)
)

// indexJobRunning reports whether a web-initiated index job is in flight right
// now (the single-flight flag, distinct from the embed_runs heartbeat which
// also catches a concurrent CLI `msgbrowse embed`). The Status card disables
// its buttons on the heartbeat signal; this backs the guard.
func (s *Server) indexJobRunning() bool {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	return s.indexing
}

// startReindex launches the embedding job under the single-flight guard and
// returns the fixed-enum outcome. It refuses (without starting anything) when
// no Indexer is wired or no embed model is configured, and coalesces a start
// that races a running job into "in progress" rather than a duplicate writer.
//
// The guard is two-layered because there are two ways a run can already be
// going. s.indexing catches a second click in THIS process. The embed_runs
// heartbeat catches a run started elsewhere — a `msgbrowse embed` CLI against
// the same SQLite file. The template disables the buttons on that heartbeat,
// but that is client-side only: a page rendered before the CLI started, or a
// direct POST, would otherwise sail past the in-memory flag and start a second
// concurrent writer that embeds the same pending messages twice and interleaves
// two overlapping embed_runs rows. A run whose heartbeat has gone stale
// (embedRunStaleAfter) reads as crashed, not live, so a Build can still resume
// after a killed CLI run — the same classification overviewEmbedding applies.
//
// The job runs in a DETACHED goroutine under context.Background() so it
// survives the request that started it; embed.Run records its own begin/finish
// in embed_runs, so progress and completion are observable on the next Status
// render (or the Overview card) with no in-request wait.
func (s *Server) startReindex(ctx context.Context, reset bool) string {
	ix := s.indexer
	if ix == nil {
		return indexResultUnavailable
	}
	if ix.EmbedModel() == "" {
		return indexResultNoModel
	}
	// A read error here is not a reason to refuse: fall through to the
	// in-memory guard rather than blocking a Build on a transient store hiccup.
	if run, err := s.store.LatestEmbedRun(ctx); err == nil && run != nil &&
		run.InFlight() && time.Since(run.UpdatedAt) <= embedRunStaleAfter {
		return indexResultInProgress
	}

	s.indexMu.Lock()
	if s.indexing {
		s.indexMu.Unlock()
		return indexResultInProgress
	}
	s.indexing = true
	s.indexMu.Unlock()

	go func() {
		// Detached: NOT the request context. embed.Run writes its terminal
		// embed_runs row even on abort, so the heartbeat never sticks on
		// "Indexing…".
		defer func() {
			s.indexMu.Lock()
			s.indexing = false
			s.indexMu.Unlock()
		}()
		if err := ix.RunEmbed(context.Background(), reset); err != nil {
			s.log.Error("semantic index job failed", "error", err, "reset", reset)
		}
	}()

	if reset {
		return indexResultReset
	}
	return indexResultStarted
}
