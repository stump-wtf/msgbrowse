package web

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// Search modes. Keyword is the default FTS5 path; semantic embeds the query and
// ranks by cosine similarity over the built vector index; hybrid fuses both by
// reciprocal-rank so an exact-term hit and a meaning hit both surface. The
// semantic/hybrid modes are the human-facing counterpart to MCP's
// semantic_search — the whole point of building embeddings.
const (
	searchModeKeyword  = "keyword"
	searchModeSemantic = "semantic"
	searchModeHybrid   = "hybrid"
)

// normalizeMode coerces an arbitrary mode param to one of the three known
// values, defaulting to keyword (the always-available path).
func normalizeMode(m string) string {
	switch m {
	case searchModeSemantic, searchModeHybrid:
		return m
	default:
		return searchModeKeyword
	}
}

// searchForm holds the raw filter state so the form re-renders with the user's
// selections intact.
type searchForm struct {
	Q              string
	Mode           string // keyword | semantic | hybrid
	ConversationID int64
	Source         string
	Sender         string
	Start          string // YYYY-MM-DD
	End            string // YYYY-MM-DD
	HasAttachment  bool
	HasLink        bool
}

// searchResult is one rendered hit. It embeds store.SearchHit (so the template's
// existing field accesses keep working) and adds Score, the cosine similarity
// (semantic) or fused reciprocal-rank score (hybrid); Score is 0 on the keyword
// path and the template only shows it for the semantic/hybrid modes.
type searchResult struct {
	store.SearchHit
	Score float64
}

type searchData struct {
	baseData
	// FilterConversations feeds the conversation dropdown: the lightweight
	// id+name listing, NOT the sidebar summaries, so partial renders never need
	// the expensive listing (SPEC-0008 REQ-0008-006). Ordered alphabetically.
	FilterConversations []store.ConversationRef
	Form                searchForm
	Sources             []string
	Hits                []searchResult
	Ran                 bool // a query was actually executed
	Count               int
	// SemanticAvailable is true when an indexer is wired AND an embed model is
	// configured, so the semantic/hybrid modes can actually run. When false and
	// the user selected one, the results area explains how to enable it instead
	// of silently returning nothing.
	SemanticAvailable bool
	// Elapsed is the server-side query time, formatted (e.g. "0.04s"), shown in
	// the results meta line (the brief's "· 0.04s").
	Elapsed string
}

// searchContextWindow is how many messages on each side of a hit the
// jump-to-context view loads.
const searchContextWindow = 20

// handleSearch renders the full search page (filter form + initial results).
// It degrades without JavaScript: the form submits here via GET and the results
// render server-side.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var base baseData
	if isPartialRequest(r) {
		base = partialBase("Search · msgbrowse", 0)
	} else {
		var err error
		base, err = s.baseData(ctx, "Search · msgbrowse", 0)
		if err != nil {
			s.serverError(w, err)
			return
		}
	}
	// The dropdown uses the cheap id+name listing on BOTH paths so partial and
	// full renders show the identical (alphabetical) option order.
	refs, err := s.store.ConversationRefs(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	form, opts := parseSearchForm(r)
	data := searchData{
		baseData:            base,
		FilterConversations: refs,
		Form:                form,
		Sources:             source.All,
		SemanticAvailable:   s.semanticAvailable(),
	}
	if opts.Query != "" {
		if err := s.runSearch(ctx, form, opts, &data); err != nil {
			s.serverError(w, err)
			return
		}
	}
	s.render(w, r, "search", data)
}

// handleSearchResults renders just the results list for HTMX live search.
func (s *Server) handleSearchResults(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	form, opts := parseSearchForm(r)
	data := searchData{Form: form, SemanticAvailable: s.semanticAvailable()}
	if opts.Query != "" {
		if err := s.runSearch(ctx, form, opts, &data); err != nil {
			s.serverError(w, err)
			return
		}
	}
	s.render(w, r, "search_results", data)
}

// handleConversationAt renders a conversation transcript centered on a specific
// message (the jump-to-context target from a search result). The page anchor
// (#m<id>) scrolls to it; the message is visually marked.
func (s *Server) handleConversationAt(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	mid, ok := parseID(r.PathValue("mid"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	active, err := s.store.GetConversationByID(ctx, id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if active == nil {
		http.NotFound(w, r)
		return
	}
	// Verify the target message actually belongs to this conversation. Without
	// this, /c/{id}/at/{mid} would render conversation {id}'s header and sidebar
	// state while GetContext (which derives the conversation from the message
	// itself) shows a *different* conversation's transcript — an
	// information-disclosure / identity-confusion bug for a crafted or mistyped
	// link.
	ownerConv, found, err := s.store.MessageConversationID(ctx, mid)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found || ownerConv != id {
		http.NotFound(w, r)
		return
	}
	msgs, err := s.store.GetContext(ctx, mid, searchContextWindow)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if len(msgs) == 0 {
		http.NotFound(w, r)
		return
	}
	list := messageListData{
		ActiveID:    id,
		Source:      active.Source,
		ConvName:    active.Name,
		Sort:        sortAsc, // context reads chronologically regardless of the transcript default
		Messages:    msgs,
		HighlightID: mid,
	}
	// Continue infinite scroll downward from the newest message in the window.
	last := msgs[len(msgs)-1]
	list.HasMore = true
	list.NextTSUnix = last.TSUnix
	list.NextID = last.ID

	// Jump-to-context links are deliberately un-boosted (the #m{id} anchor needs
	// a full-page load), but the partial contract still holds if an HTMX request
	// arrives here (REQ-0008-006).
	var base baseData
	if isPartialRequest(r) {
		base = partialBase(active.Name+" · msgbrowse", id)
	} else {
		base, err = s.baseData(ctx, active.Name+" · msgbrowse", id)
		if err != nil {
			s.serverError(w, err)
			return
		}
	}
	// Toolbar contextual title is the conversation name on a transcript (#152);
	// jump-to-context is still a Messages surface (#190).
	base.NavTitle = humanName(active.Name)
	base.NavTab = navTabMessages
	s.render(w, r, "conversation", conversationData{
		baseData: base,
		Active:   active,
		List:     list,
	})
}

// parseSearchForm reads the search filters from the request query string into a
// re-renderable form and a store.SearchOptions.
func parseSearchForm(r *http.Request) (searchForm, store.SearchOptions) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	convID, _ := strconv.ParseInt(r.URL.Query().Get("conversation"), 10, 64)
	src := r.URL.Query().Get("source")
	if !source.IsKnown(src) {
		src = ""
	}
	sender := strings.TrimSpace(r.URL.Query().Get("sender"))
	start := r.URL.Query().Get("start")
	end := r.URL.Query().Get("end")
	hasAtt := r.URL.Query().Get("has_attachment") != ""
	hasLink := r.URL.Query().Get("has_link") != ""
	mode := normalizeMode(r.URL.Query().Get("mode"))

	form := searchForm{
		Q: q, Mode: mode, ConversationID: convID, Source: src, Sender: sender,
		Start: start, End: end, HasAttachment: hasAtt, HasLink: hasLink,
	}
	opts := store.SearchOptions{
		Query:          q,
		ConversationID: convID,
		Source:         src,
		Sender:         sender,
		StartUnix:      dayStartUnix(start),
		EndUnix:        dayEndUnix(end),
		HasAttachment:  hasAtt,
		HasLink:        hasLink,
	}
	return form, opts
}

// dayStartUnix parses a YYYY-MM-DD date as the start of that UTC day (00:00:00).
// Returns 0 for an empty or unparseable date (no lower bound).
func dayStartUnix(date string) int64 {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return 0
	}
	return t.UTC().Unix()
}

// dayEndUnix parses a YYYY-MM-DD date as the end of that UTC day (23:59:59).
// Returns 0 for an empty or unparseable date (no upper bound).
func dayEndUnix(date string) int64 {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return 0
	}
	return t.UTC().Add(24*time.Hour - time.Second).Unix()
}

// searchLimit caps semantic/hybrid retrieval. Keyword search is already bounded
// by the store's own limit; the semantic scan is brute-force, so cap it to keep
// a single query bounded and to match the top-K feel of the result cards.
const searchLimit = 200

// semanticAvailable reports whether the semantic / hybrid modes can actually
// run: an indexer must be wired (serve / desktop, not browser mode) AND an
// embed model configured. The Search page reads this to decide between running
// a semantic query and showing the "not configured" explainer.
func (s *Server) semanticAvailable() bool {
	return s.indexer != nil && s.indexer.EmbedModel() != ""
}

// runSearch executes the query in the form's mode, filling data.Hits / Count /
// Ran / Elapsed. Semantic and hybrid degrade to keyword-only when the index is
// unavailable or the query fails to embed (never an error to the user). It is
// shared by the full-page and HTMX-partial handlers so both paths behave
// identically.
func (s *Server) runSearch(ctx context.Context, form searchForm, opts store.SearchOptions, data *searchData) error {
	start := time.Now()
	defer func() { data.Elapsed = formatElapsed(time.Since(start)) }()
	data.Ran = true

	switch form.Mode {
	case searchModeSemantic:
		if !data.SemanticAvailable {
			return nil // template shows the "enable semantic search" explainer
		}
		hits, err := s.semanticHits(ctx, form.Q, opts)
		if err != nil {
			return err
		}
		data.Hits = hits
	case searchModeHybrid:
		kw, err := s.store.SearchMessages(ctx, withLimit(opts, searchLimit))
		if err != nil {
			return err
		}
		if !data.SemanticAvailable {
			data.Hits = keywordResults(kw) // degrade to keyword-only
			break
		}
		sem, err := s.scoredFor(ctx, form.Q, opts)
		if err != nil {
			return err
		}
		data.Hits = fuseSearchResults(kw, sem, searchLimit)
	default: // keyword
		hits, err := s.store.SearchMessages(ctx, opts)
		if err != nil {
			return err
		}
		data.Hits = keywordResults(hits)
	}
	data.Count = len(data.Hits)
	return nil
}

// withLimit returns opts with Limit set (leaving the caller's value when it
// already set one). Hybrid pulls a wider keyword slice than the default so the
// fusion has candidates to rank.
func withLimit(opts store.SearchOptions, limit int) store.SearchOptions {
	if opts.Limit == 0 {
		opts.Limit = limit
	}
	return opts
}

// scoredFor embeds the query and runs the vector scan, returning the raw scored
// messages (used by the hybrid fuse). Returns nil (not an error) when the query
// can't be embedded, so hybrid falls back to keyword-only.
func (s *Server) scoredFor(ctx context.Context, query string, opts store.SearchOptions) ([]store.ScoredMessage, error) {
	vec := s.indexer.EmbedQuery(ctx, query)
	if vec == nil {
		return nil, nil
	}
	return s.store.SemanticSearch(ctx, vec, s.indexer.EmbedModel(), store.SemanticOptions{
		ConversationID: opts.ConversationID,
		Source:         opts.Source,
		Sender:         opts.Sender,
		StartUnix:      opts.StartUnix,
		EndUnix:        opts.EndUnix,
		K:              searchLimit,
	})
}

// semanticHits runs a pure-semantic query and returns rendered results ordered
// by descending similarity.
func (s *Server) semanticHits(ctx context.Context, query string, opts store.SearchOptions) ([]searchResult, error) {
	scored, err := s.scoredFor(ctx, query, opts)
	if err != nil {
		return nil, err
	}
	out := make([]searchResult, 0, len(scored))
	for _, m := range scored {
		out = append(out, scoredResult(m))
	}
	return out, nil
}

// keywordResults wraps FTS hits as searchResults (Score 0 — the template hides
// the score chip on the keyword path).
func keywordResults(hits []store.SearchHit) []searchResult {
	out := make([]searchResult, 0, len(hits))
	for _, h := range hits {
		out = append(out, searchResult{SearchHit: h})
	}
	return out
}

// scoredResult converts a semantic ScoredMessage into a rendered searchResult.
// The body becomes the snippet (no FTS marks on the semantic path); attachment/
// link glyphs are omitted (the vector scan doesn't join them) — the score chip
// is the semantic affordance instead. The body is stripped of the FTS highlight
// sentinels (\x02/\x03) it never legitimately carries here, so a message that
// literally contained one can't render a stray, unbalanced <mark>.
func scoredResult(m store.ScoredMessage) searchResult {
	return searchResult{
		SearchHit: store.SearchHit{
			MessageID:        m.MessageID,
			Hash:             m.Hash,
			ConversationID:   m.ConversationID,
			ConversationName: m.ConversationName,
			Source:           m.Source,
			Sender:           m.Sender,
			IsOwner:          m.IsOwner,
			IsSystem:         m.IsSystem,
			TS:               m.TS,
			TSUnix:           m.TSUnix,
			Snippet:          stripSnippetMarks(m.Body),
		},
		Score: round4(m.Score),
	}
}

// fuseSearchResults blends keyword and semantic hits by reciprocal-rank fusion
// (the same k0=60 scheme MCP's fuseResults uses), deduping by message id so a
// hit found by both halves ranks higher. The result is the union, best-first,
// capped at limit — an exact-term match and a meaning match both surface.
func fuseSearchResults(kw []store.SearchHit, sem []store.ScoredMessage, limit int) []searchResult {
	const k0 = 60.0
	type agg struct {
		res   searchResult
		score float64
	}
	byID := map[int64]*agg{}
	order := []int64{}
	add := func(id int64, res searchResult, rank int) {
		a := byID[id]
		if a == nil {
			a = &agg{res: res}
			byID[id] = a
			order = append(order, id)
		}
		a.score += 1.0 / (k0 + float64(rank+1))
	}
	for rank, h := range kw {
		add(h.MessageID, searchResult{SearchHit: h}, rank)
	}
	for rank, m := range sem {
		add(m.MessageID, scoredResult(m), rank)
	}
	// Rank on the RAW fused score, then round only for display — rounding
	// before the sort would collapse near-equal RRF scores (which live around
	// 1/61 ≈ 0.016) into ties that fall back to insertion order.
	aggs := make([]*agg, 0, len(byID))
	for _, id := range order {
		aggs = append(aggs, byID[id])
	}
	sort.SliceStable(aggs, func(i, j int) bool { return aggs[i].score > aggs[j].score })
	out := make([]searchResult, 0, len(aggs))
	for _, a := range aggs {
		a.res.Score = round4(a.score)
		out = append(out, a.res)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// round4 rounds a score to 4 decimals for the display chip. Four (not two)
// keeps hybrid RRF scores (~0.016–0.03) distinguishable while still reading as
// a compact relevance number for cosine similarities (0–1).
func round4(f float64) float64 { return math.Round(f*10000) / 10000 }

// stripSnippetMarks removes the FTS highlight sentinels from a raw body used as
// a semantic-result snippet. FTS5 emits them as balanced pairs; a raw message
// body has no marks, so any sentinel it literally contains is spurious and would
// render an unbalanced <mark> via highlightSnippet. Cheap no-op for the common
// case (bodies contain no C0 sentinels).
func stripSnippetMarks(body string) string {
	if !strings.ContainsAny(body, store.SnippetMarkStart+store.SnippetMarkEnd) {
		return body
	}
	body = strings.ReplaceAll(body, store.SnippetMarkStart, "")
	return strings.ReplaceAll(body, store.SnippetMarkEnd, "")
}

// formatElapsed renders a query duration like the brief's "0.04s".
func formatElapsed(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 2, 64) + "s"
}
