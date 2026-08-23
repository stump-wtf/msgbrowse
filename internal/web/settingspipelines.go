// Settings Tabs For The Derived-Data Pipelines
//
// One Settings tab per pipeline: /settings/search-index owns the semantic-search
// embeddings, /settings/journal owns the journal digests, /settings/facts owns
// contact-fact extraction. Each renders its pipeline's coverage, model, run
// history and build controls, and nothing else.
//
// SPEC-0004 REQ-0004-010 requires this shape. The two cards previously shared
// the Status tab, whose unrelated coverage figures, models, run histories and
// costs read as one undifferentiated machine-room dump; before that the
// semantic card was rendered on both Home and Status, and the two copies
// drifted. So the rule is one tab per pipeline, rendered on exactly one
// surface, with the reading surfaces (Home, /journal) carrying at most a
// pointer.
//
// The POST routes are unchanged — /status/index, /status/index/reset,
// /journal/build, /journal/rebuild, /journal/rebuild/day keep their paths and
// their checkSetupPOST gates. Only the surface they re-render moved, so no
// bookmark, form action or guard changed meaning. The facts pipeline follows
// that same shape: its POSTs live at /facts/run and /facts/reset, and only the
// tab they re-render is new.
//
// @joestump-agent 08/20/2026 - Split out of the Status tab (#368).
//
// @joestump-agent 08/23/2026 - Added the Facts tab (#366). Fact extraction had
// been CLI-only since it shipped: no route, no control, no Settings surface at
// all, so on a real archive the contact page rendered an empty fact set forever
// with no way to fill it. It joins as its own tab rather than sharing one,
// because that is what REQ-0004-010 requires and because the shared Status tab
// this rule replaced is exactly the shape being avoided.
package web

import (
	"context"
	"net/http"
)

// searchIndexData drives /settings/search-index. The field names match what the
// semantic_index_card define reads, so the card is unchanged by the move.
type searchIndexData struct {
	baseData
	// Embedding is the coverage + latest-run half of the card, assembled by
	// overviewEmbedding; History is the recent-runs track record beside it.
	Embedding embedStatusData
	History   []pipelineRunView
	// IndexAvailable reports whether an Indexer is wired: false (browser / no-op
	// mode) hides the Build controls and shows the unavailable note.
	IndexAvailable bool
	// IndexRunning is the web layer's in-memory single-flight flag: true from the
	// instant a Build / Reset starts, BEFORE the detached goroutine writes the
	// first embed_runs row. The template ORs it with Embedding.InProgress to
	// start the live poll (and disable the buttons) immediately after a click,
	// bridging the gap until the heartbeat row exists. Embedding.InProgress still
	// catches a run started by a separate `msgbrowse embed` process.
	IndexRunning bool
	// IndexResult is the post-POST banner state after a Build / Reset-&-rebuild:
	// "" (no action), "started", "reset", "inprogress", "nomodel",
	// "unavailable", or "error" — a fixed enum mapped to prose by the template.
	IndexResult string
	// SetupToken arms the Build / Reset forms with the same per-session token
	// gate the other privileged POSTs use; "" when no Indexer is wired (the
	// forms are not rendered then).
	SetupToken string
}

// journalSettingsData drives /settings/journal. Field names match what the
// journal_build_card define reads.
type journalSettingsData struct {
	baseData
	Build journalBuildData
	// JournalResult is the fixed-enum banner from a just-completed Build /
	// Rebuild POST, "" on a plain GET.
	JournalResult string
	SetupToken    string
}

// factsSettingsData drives /settings/facts. Field names match what the
// facts_build_card define reads, so the card is shared verbatim with the
// live-progress fragment (factsCardData).
type factsSettingsData struct {
	baseData
	Build factsBuildData
	// FactsResult is the fixed-enum banner from a just-completed Extract /
	// Re-extract POST, "" on a plain GET.
	FactsResult string
	SetupToken  string
}

// handleSettingsSearchIndex is GET /settings/search-index.
func (s *Server) handleSettingsSearchIndex(w http.ResponseWriter, r *http.Request) {
	s.renderSearchIndex(w, r, "")
}

// handleSettingsJournal is GET /settings/journal.
func (s *Server) handleSettingsJournal(w http.ResponseWriter, r *http.Request) {
	s.renderJournalSettings(w, r, "")
}

// renderSearchIndex assembles and renders the Search index tab (full document
// or boosted #main-content partial). indexResult is the fixed-enum banner from
// a just-completed Build / Reset POST, "" on a plain GET.
func (s *Server) renderSearchIndex(w http.ResponseWriter, r *http.Request, indexResult string) {
	ctx := r.Context()
	base, err := s.settingsBase(ctx, r, "Search index · msgbrowse")
	if err != nil {
		s.serverError(w, err)
		return
	}
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
		baseData:       base,
		Embedding:      embedding,
		History:        history,
		IndexAvailable: s.indexer != nil,
		IndexRunning:   s.indexJobRunning(),
		IndexResult:    indexResult,
	}
	// The Build / Reset forms are privileged POSTs: arm them with a live token,
	// but only when there is an Indexer to drive (browser mode renders the
	// unavailable note and no forms).
	if s.indexer != nil {
		tok, err := s.setupTokens.mint()
		if err != nil {
			s.serverError(w, err)
			return
		}
		data.SetupToken = tok
	}
	s.render(w, r, "search_index", data)
}

// renderJournalSettings assembles and renders the Journal tab. journalResult is
// the fixed-enum banner from a just-completed Build / Rebuild POST.
func (s *Server) renderJournalSettings(w http.ResponseWriter, r *http.Request, journalResult string) {
	ctx := r.Context()
	base, err := s.settingsBase(ctx, r, "Journal · msgbrowse")
	if err != nil {
		s.serverError(w, err)
		return
	}
	build, err := s.journalBuildStatus(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	data := journalSettingsData{
		baseData:      base,
		Build:         build,
		JournalResult: journalResult,
	}
	if s.journalBuilder != nil {
		tok, err := s.setupTokens.mint()
		if err != nil {
			s.serverError(w, err)
			return
		}
		data.SetupToken = tok
	}
	s.render(w, r, "journal_settings", data)
}

// handleSettingsFacts is GET /settings/facts.
func (s *Server) handleSettingsFacts(w http.ResponseWriter, r *http.Request) {
	s.renderFactsSettings(w, r, "")
}

// renderFactsSettings assembles and renders the Facts tab. factsResult is the
// fixed-enum banner from a just-completed Extract / Re-extract POST, "" on a
// plain GET.
func (s *Server) renderFactsSettings(w http.ResponseWriter, r *http.Request, factsResult string) {
	ctx := r.Context()
	base, err := s.settingsBase(ctx, r, "Facts · msgbrowse")
	if err != nil {
		s.serverError(w, err)
		return
	}
	build, err := s.factsBuildStatus(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	data := factsSettingsData{
		baseData:    base,
		Build:       build,
		FactsResult: factsResult,
	}
	// The Extract / Re-extract forms are privileged POSTs: arm them with a live
	// token, but only when there is an extractor to drive (browser mode renders
	// the unavailable note and no forms at all).
	if s.factsExtractor != nil {
		tok, err := s.setupTokens.mint()
		if err != nil {
			s.serverError(w, err)
			return
		}
		data.SetupToken = tok
	}
	s.render(w, r, "facts_settings", data)
}

// settingsBase builds the page shell for a Settings tab. A boosted navigation
// swaps only #main-content, so it never needs the sidebar conversation listing
// (SPEC-0008 REQ-0008-006) — these tabs carry no stat strip either, so unlike
// Status they need no archive counts on the partial path.
func (s *Server) settingsBase(ctx context.Context, r *http.Request, title string) (baseData, error) {
	if isPartialRequest(r) {
		return partialBase(title, 0), nil
	}
	return s.baseData(ctx, title, 0)
}
