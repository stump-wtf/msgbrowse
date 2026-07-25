# ADR-0025: Semantic search in the web UI + in-app index management

- **Status:** Accepted
- **Date:** 2026-07-19
- **Deciders:** Joe Stump
- **Related:**
  - [ADR-0002 (vector backend)](0002-vector-backend.md) — the brute-force
    `SemanticSearch` this ADR finally surfaces to a human; no new backend.
  - [ADR-0004 (MCP SDK and RAG)](0004-mcp-sdk-and-rag.md) — hybrid RRF fusion
    and the "degrade to keyword-only, log it" contract, reused in the web layer.
  - [ADR-0010 (security/privacy posture)](0010-security-privacy-posture.md) —
    the single egress (query embedding + the Build pass are the only
    `llm.base_url` calls) and the privileged-POST gate the Build/Reset controls
    keep.
  - [SPEC-0002 (search)](../openspec/specs/search/spec.md) — REQ-0002-008
    (web semantic/hybrid) and REQ-0002-009 (index observability + management).

## Context and Problem Statement

The embedding *storage* and *search* primitives existed — `internal/embed.Run`,
`store.SemanticSearch`, `PutEmbedding`, `PruneOrphanEmbeddings` — but the index
was neither **observable** nor **usable from the UI**:

1. **Not usable.** The only caller of `SemanticSearch` was the MCP server. The
   human-facing web Search page ran FTS5 only (`SearchMessages`). A user who
   built the index could not semantic-search from the app.
2. **Not observable / not manageable.** There was no run log, no coverage query,
   no Status surface, and no in-app way to build or rebuild the index — only the
   `msgbrowse embed` CLI. So "how much of my archive is indexed / is a run
   happening / build it now" had no answer in the app.

## Considered Options

- **Reuse the MCP embed/fuse code via a shared package** vs. **a thin web-layer
  seam.**
- **Run bookkeeping:** a durable `embed_runs` table (begin → heartbeat →
  finish) vs. an in-memory status vs. no tracking (CLI-only).
- **Live progress:** htmx self-poll of a fragment vs. manual refresh vs.
  WebSocket/SSE.

## Decision Outcome

**Usable:** add an `EmbedQuery(ctx, query) []float32` method to the `embed.Indexer`
seam (which already holds the shared store + live `llm.Holder`) and a search-mode
selector (`keyword | semantic | hybrid`) on the web Search page. Semantic embeds
the query and ranks via `store.SemanticSearch`; hybrid fuses keyword + semantic
with the same reciprocal-rank scheme (`1/(60+rank)`) MCP uses. Semantic results
convert to the existing `store.SearchHit` shape (body → snippet, no FTS mark) so
they render through the identical result cards, with a similarity-score chip as
the semantic affordance. Unavailable (no model / no indexer) and failed-embed
cases degrade — an explainer or the empty state, never a 500, never silent
nothing.

**Observable + manageable:** introduce durable run bookkeeping —
`embed_runs` (migration v12) plus `EmbeddingCoverage`, `LatestEmbedRun`,
`RecentEmbedRuns`, and `ResetEmbeddings` — and wire `embed.Run` to record every
pass (begin, per-batch heartbeat, terminal write even on abort via
`WithoutCancel`). The web layer adds an `Indexer` seam (Build / Reset as a
single-flight detached goroutine) and a Status "Semantic search index" card:
coverage `<progress>` bar (CSP-clean — no inline style, banned by the template
test), a recent-runs table classified live/stalled/finished by the heartbeat,
and **live self-refresh via htmx polling** — the card is a `semantic_index_card`
define that a fragment endpoint (`GET /status/index/progress`) re-renders; while a
run is `InProgress` the card carries `hx-trigger="every 2s"` targeting itself,
and once the refreshed HTML is no longer in progress it carries no trigger, so
polling stops on its own.

Rejected the shared-package refactor: the web fuse is a dozen lines over a web
view type, and the MCP `messageHit` type is not the web `SearchHit`. Rejected
WebSocket/SSE: far more machinery than a 2-second poll over a personal-scale
index that runs for seconds to minutes. Rejected in-memory status: the CLI and
`serve` are separate processes sharing one SQLite file, so a durable table is the
only channel between them.

### Consequences

- The embedding index is finally a product feature (usable in Search) and an
  operable one (visible + buildable on Status), not just an MCP capability;
  keyword search is untouched and remains the default.
- One new seam method (`EmbedQuery`), one new migration (`embed_runs` v12), and a
  handful of store queries; `embed.Run` gains best-effort run recording that
  never aborts the embedding work.
- The single-egress and privileged-POST contracts are preserved: query embedding
  and the Build pass are the network calls, Build/Reset keep the per-session
  token gate, and the poll endpoint is a read-only GET that mints a fresh token
  only for the forms it re-renders (the token set is bounded).
- The poll runs only while a job is in flight and stops itself; an idle Status
  page makes no background requests.
- Deferred: a home-page index card (this ADR scopes the surface to Status),
  per-hit source/attachment glyphs on the semantic path (the vector scan does not
  join them), and a keyword↔semantic score normalization beyond RRF.
