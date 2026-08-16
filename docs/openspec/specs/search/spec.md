---
status: approved
date: 2026-06-27
implements: [ADR-0002, ADR-0004]
---

# SPEC-0002: Search

- **Capability:** search
- **Source packages:** `internal/store` (`search.go`, `vector.go`), `internal/web` (`search.go`), `internal/embed`, `internal/mcp` (hybrid fusion)
- **Related ADRs:** [ADR-0002](../../../adr/0002-vector-backend.md), [ADR-0004](../../../adr/0004-mcp-sdk-and-rag.md)

## Overview

msgbrowse provides keyword (FTS5), semantic (vector), and hybrid search over the
unified message store. All search building blocks live in the store so the web UI
and MCP layer cannot diverge. Query construction MUST be injection-safe, snippet
highlighting MUST be escape-safe, and every result MUST carry enough provenance to
jump back to the message in context.

## Requirements

### REQ-0002-001: FTS5 keyword search with filters

`SearchMessages` MUST run a full-text search over message bodies via the FTS5
`messages_fts` index, ranked by bm25 (`ORDER BY rank`). It MUST support optional
filters: conversation id, source, exact sender, start/end unix-time bounds,
has-attachment, and has-link. It MUST cap results at a default of 50 and a maximum
of 200, and MUST return no hits (and no error) for an empty or all-whitespace
query.

#### Scenario: Filtered keyword search
- **Given** messages across conversations and sources
- **When** `SearchMessages` runs with a query plus a source filter and a date range
- **Then** only matching messages within that source and range are returned, ranked by bm25.

#### Scenario: Has-attachment / has-link filters
- **Given** a query and `HasAttachment` true
- **Then** only messages that have at least one attachment are returned.

#### Scenario: Empty query yields nothing
- **Given** an empty or whitespace-only query
- **When** `SearchMessages` runs
- **Then** it returns zero hits and no error.

### REQ-0002-002: Injection-safe query building

`buildFTSQuery` MUST convert free-form user input into a safe FTS5 MATCH
expression by quoting every whitespace-separated token as a prefix term
(`"token"*`), doubling embedded double quotes, and ANDing the tokens. The
resulting expression MUST never be a syntax error and MUST never let user input
alter the query structure (FTS5 operators/punctuation in input are neutralized).
Empty input MUST yield an empty expression.

#### Scenario: FTS5 operators in input are neutralized
- **Given** the input `foo OR bar"; --`
- **When** `buildFTSQuery` runs
- **Then** each token is emitted as a quoted prefix term with embedded quotes doubled, so no FTS5 operator takes effect and the query cannot error.

### REQ-0002-003: Snippet highlighting and escaping

`SearchMessages` MUST return a body excerpt with matched terms wrapped in control
characters (`SnippetMarkStart`=`\x02`, `SnippetMarkEnd`=`\x03`), keeping the store
layer presentation-free. The web layer MUST, in order: strip C0 control characters
except the two sentinels (and tab/newline), HTML-escape the text, then replace the
surviving sentinels with `<mark>`/`</mark>`. A literal sentinel byte in an
untrusted message body MUST NOT produce stray or unbalanced markup, and untrusted
text MUST NOT be able to inject HTML.

#### Scenario: Matched terms render as balanced, escaped marks
- **Given** a snippet whose matched term is wrapped in the sentinel bytes and whose surrounding text contains `<script>`
- **When** the web layer highlights it
- **Then** the term is wrapped in `<mark>...</mark>` and the `<script>` is HTML-escaped.

#### Scenario: A forged sentinel in the body is sanitized
- **Given** a message body that itself contains a literal sentinel byte
- **When** the snippet is highlighted
- **Then** the forged byte is stripped before escaping, so no unbalanced `<mark>` is emitted.

### REQ-0002-004: Semantic search over embeddings

`SemanticSearch` MUST rank messages by cosine similarity to a query embedding,
applying the same metadata filters as keyword search (conversation, source,
sender, date range) and capping at K (default 20, max 200). Embeddings MUST be
stored as little-endian float32 blobs keyed by `(message_hash, model)` so multiple
models coexist and re-ingest does not orphan unchanged embeddings. A candidate
whose stored dimension does not match the query MUST be skipped rather than scored.
An empty query vector or zero-norm query MUST return no results.

#### Scenario: Top-K by cosine similarity with filters
- **Given** a query embedding and embeddings stored for the same model
- **When** `SemanticSearch` runs with K=10 and a conversation filter
- **Then** the 10 most cosine-similar messages within that conversation are returned, sorted by descending score.

#### Scenario: Dimension mismatch is skipped, not scored
- **Given** a stored embedding whose dimension differs from the query vector
- **When** semantic search scores candidates
- **Then** that embedding is skipped rather than producing a garbage score.

### REQ-0002-005: Incremental, idempotent embedding generation

The embed pass MUST embed only messages that lack an embedding for the configured
model (keyed by stable hash), skipping system messages and empty bodies. The model
string MUST be normalized once and used identically for the "needs embedding"
query and for storage. `embed --prune` MUST be able to delete embeddings whose
message hash no longer exists. The pass MUST be resumable and MUST abort with a
diagnostic if no progress is made.

#### Scenario: Re-running embed after import embeds only new messages
- **Given** a corpus already embedded for a model and a fresh import adding messages
- **When** the embed pass runs
- **Then** only the new, non-empty, non-system messages are embedded.

#### Scenario: Prune reclaims orphaned embeddings
- **Given** embeddings whose messages were removed by a re-ingest
- **When** `embed --prune` runs
- **Then** those orphan embeddings are deleted.

### REQ-0002-006: Hybrid search via RRF in the MCP layer

The MCP `search_messages` tool MUST run the keyword half and (best-effort) the
vector half, then fuse them with reciprocal-rank fusion (`score = sum of 1/(60 + rank)`)
because the native bm25 and cosine scores are not comparable. If the LLM endpoint
or embeddings are unavailable, it MUST degrade to keyword-only results rather than
failing, and MUST log the degradation. The fused list MUST be sorted by descending
score with ties broken deterministically by message id.

#### Scenario: Both halves available
- **Given** working embeddings and keyword index
- **When** `search_messages` runs
- **Then** keyword and vector hits are fused by RRF and returned in descending fused-score order.

#### Scenario: Embeddings unavailable degrades gracefully
- **Given** the LLM/embedding endpoint is unavailable
- **When** `search_messages` runs
- **Then** keyword-only results are returned, the degradation is logged, and no error is surfaced to the caller.

### REQ-0002-007: Jump-to-context with conversation-ownership check

The web jump-to-context view (`/c/{id}/at/{mid}`) MUST verify that the target
message actually belongs to the conversation in the URL before rendering. If the
message does not exist or belongs to a different conversation, it MUST return 404.
On success it MUST render a window of messages on each side of the target,
highlight the target, and continue infinite scroll from the window's newest
message.

#### Scenario: Mismatched message id is rejected
- **Given** a request `/c/{id}/at/{mid}` where message `mid` belongs to a different conversation
- **When** the handler runs
- **Then** it returns 404 and renders no transcript (no cross-conversation disclosure).

#### Scenario: Valid jump renders highlighted context
- **Given** a request where `mid` belongs to conversation `id`
- **When** the handler runs
- **Then** a context window around `mid` is rendered with `mid` highlighted and infinite scroll set to continue.

### REQ-0002-008: Semantic and hybrid search in the web Search UI

The web Search page MUST expose the three search modes to a human — keyword
(FTS5, always available), semantic (vector), and hybrid — via a mode selector,
so the built embedding index is queryable from the UI and not only from MCP.
The semantic path MUST embed the query with the live LLM client and rank via
`SemanticSearch`; hybrid MUST fuse keyword and semantic with the same
reciprocal-rank scheme as REQ-0002-006. When no embedding model is configured or
no indexer is wired (browser mode), selecting semantic/hybrid MUST render an
explainer pointing to the LLM settings + Status build — never a silent empty
result — while keyword search keeps working. A query that fails to embed MUST
degrade to no semantic hits (hybrid falls back to keyword-only) rather than
erroring. Semantic and hybrid results MUST render through the same result cards
as keyword, carrying a relevance-score affordance; the results meta MUST include
elapsed query time.

#### Scenario: Semantic mode unavailable is explained, not silent
- **Given** no embedding model configured (or no indexer wired)
- **When** a user selects Semantic or Hybrid and searches
- **Then** the page explains how to enable semantic search and keyword search still works, with no error.

#### Scenario: Semantic results render with a score and no FTS mark
- **Given** an embed model, a wired indexer, and a stored embedding for a message
- **When** a user runs a semantic query whose vector is similar to that message
- **Then** the message is returned through the result card with a relevance-score chip and without an FTS `<mark>` highlight.

#### Scenario: Failed query embedding degrades gracefully
- **Given** a wired indexer whose query embedding fails (endpoint down)
- **When** a user runs a semantic query
- **Then** the page shows the empty state (no hits) with no 500 and no invented results.

### REQ-0002-009: Semantic index observability + in-app management on Status

The Status page MUST surface the semantic index's live state and track record and
let the user drive it in-app: a coverage progress bar (embedded of embeddable,
%), the current run's in-progress / interrupted state (from the `embed_runs`
heartbeat), a recent-run history table (newest first, bounded) with each run's
start, outcome (completed / running / interrupted / failed), embedded count,
duration, and model, and Build / Reset-&-rebuild controls that run the same
embedding pass `msgbrowse embed` does. Each embedding pass (CLI or in-app) MUST
record a durable row in `embed_runs` (begin → per-batch heartbeat → terminal
write, even on abort). While a run is in flight the card MUST refresh itself live
via a same-origin poll of a fragment endpoint (`GET /status/index/progress`) with
no inline script (CSP-clean), and MUST stop polling once the run is no longer in
progress. The Build / Reset controls MUST be privileged same-origin POSTs gated
by the per-session token, single-flight (a second Build coalesces), and report a
fixed-enum banner; with no indexer wired they render an "unavailable" note.

#### Scenario: Live progress while indexing
- **Given** an index run in flight
- **When** the Status page renders
- **Then** the semantic-index card shows a coverage progress bar and polls `GET /status/index/progress` until the run finishes, then stops polling.

#### Scenario: Run history reflects recorded runs
- **Given** one or more recorded `embed_runs`
- **When** the Status page renders
- **Then** a recent-runs table lists them newest-first with a status badge, embedded count, duration, and model.

#### Scenario: Build coalesces under single-flight
- **Given** an index job already in flight
- **When** a second Build POST arrives
- **Then** it returns the "in progress" banner and starts no second writer.
