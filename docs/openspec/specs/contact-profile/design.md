# SPEC-0017 Design: Contact profile

- **Capability:** contact-profile
- **Related ADRs:** [ADR-0003](../../../adr/0003-dual-source-archive.md), [ADR-0011](../../../adr/0011-contact-facts-extraction.md), [ADR-0010](../../../adr/0010-security-privacy-posture.md)
- **Related specs:** [SPEC-0005 (contact facts)](../contact-facts/spec.md), [SPEC-0008 (web performance)](../web-performance/spec.md)

## Architecture

The profile is a read-only page assembled by one handler from several
independent, cheap store reads. `handleContact` fetches identity, facts, stats,
volume, most-active hour, and reactions, computes the sparkline geometry in Go,
and renders `contact` (which boosts to `contact_content`).

```
internal/web/server.go        GET /contact/{id} ──▶ handleContact
                                                       │
internal/web/contact.go ───────┼─ GetContactByID        (identity only)
                               ├─ ContactFacts          (grouped by declared category)
                               ├─ ContactStats          (one scan: counts, photos, edges)
                               ├─ ContactMessageVolume  (per-month, UTC)
                               ├─ ContactMostActiveHour (UTC hour histogram)
                               ├─ ContactTopReactions   (emoji tally)
                               │    groupFacts → buildSparkline (Go geometry)
internal/web/templates/contact.html ─▶ contact_content (#main-content swap unit)
```

All per-contact reads live in `internal/store/contacts.go`; the fact reads and
the `ContactFact` provenance columns live in `internal/store/facts.go`; the
transcript header's `ContactID` link comes from `ConversationSummary` in
`internal/store/query.go`.

## Key design decisions

### The contact-scope predicate, and no index on `conversations.contact_id`

A contact is a person who may be merged across sources, so every aggregate must
span all of the contact's conversations. There is no `contact_id` column on
`messages`, `attachments`, or `reactions`, so the load-bearing predicate
throughout is:

```
conversation_id IN (SELECT id FROM conversations WHERE contact_id = ?)
```

`conversations` is a tiny table (one row per source thread), so the subquery is
negligible and needs no dedicated index on `conversations.contact_id` at current
scale. Keeping the predicate identical everywhere makes the page one mental model
instead of six, and avoids widening three high-cardinality tables with a
denormalized `contact_id` that the merge story (below) does not yet require.

### `GetContactByID` is identity-only; `ContactStats` owns the counts

`GetContactByID` deliberately does **not** compute any message counts or totals.
It returns display name, the full merged identifier set, the owned conversations,
and first/last **display** timestamps (via `contactEdgeTS` probes). Counts,
sent/received split, photos, and the epoch bounds are `ContactStats`' single
scan. Splitting them this way means the page issues exactly one aggregate scan of
the message table, not two — the identity query never re-scans for a `COUNT(*)`
it does not need. The `Contact.SourceCount()` helper derives the merged-vs-single
subtitle purely from the identifier set, so no extra query is spent on it.

### First/last edges via `ts_unix` probes, buckets in UTC

Edge timestamps use indexed `ts_unix` probes (`ORDER BY ts_unix, id LIMIT 1`),
never `MIN(ts)`/`MAX(ts)` string aggregation — iMessage's human-formatted `ts`
strings sort alphabetically wrong, the exact hazard [SPEC-0008 REQ-0008-002](../web-performance/spec.md)
codifies. Month and hour bucketing use `strftime('%Y-%m', ts_unix, 'unixepoch')`
and `strftime('%H', ts_unix, 'unixepoch')` so output is deterministic and
testable in UTC, matching the journal's timezone discipline. The messages/day
pace is derived in Go from the epoch bounds (`ContactStats`), clamped to a
one-day floor so a same-day burst does not divide by zero.

### Facts deep-link to their OWN conversation

`ContactFacts` resolves each fact's `source_message_hash` back to the current
`messages.id` **and** its owning `conversation_id` via a `LEFT JOIN` on the
globally-unique `messages.hash`. The profile spans multiple conversations, so a
fact's jump-to-context link must target the message's own conversation, not a
single "active" one — hence both `SourceMessageID` and `SourceConversationID`
are carried, and the link is `/c/{SourceConversationID}/at/{SourceMessageID}`.
A vanished message resolves both to 0 (via `COALESCE`), and the template renders
that fact undated and link-less rather than emitting a broken link. This differs
from `ContactFactsByConversation` (used by the conversation view), which leaves
`SourceConversationID` at 0 because its caller already knows the conversation.

`groupFacts` buckets facts into the declared `facts.Categories` order rather than
the SQL `ORDER BY f.category` (alphabetical) order, so the display order is the
curated category order, not an accident of collation. Any fact with an unexpected
category (extraction coerces to the known set, so this should not happen) is
appended last so none are ever silently dropped. Fact text is model-derived and
therefore untrusted; it is printed through `html/template` auto-escaping.

### Sparkline: year roll-up, gap-fill, and the label band

`ContactMessageVolume` returns sparse per-month buckets (zero-traffic months are
absent). `buildSparkline` rolls those months up to per-year totals and then
walks the **full** `minYear..maxYear` span, emitting a bar for every year
including silent ones — a gap-filled axis, so a quiet year is a labeled empty
slot rather than a collapsed gap that would misrepresent the timeline. Geometry
is computed entirely in Go: a fixed bar width and gap, a `sparkLabelH` band below
the baseline reserved for year labels so they never overlap the bars, and a
per-bar `fill-opacity` scaled to the year's share of the max. A present-but-tiny
year is floored to a 2px stub so it stays visible; a truly silent year stays
zero-height. Emitting pre-computed integers lets the template use presentational
SVG attributes only (`x`/`y`/`width`/`height`/`fill-opacity`) with no inline
`style=`, which the strict CSP forbids.

### Merged-identity reality: mostly one conversation per contact today

The page is built for the merged-person grain, but the merge story is only
partly realized. `UpsertConversation` creates one contact per `(source, name)`
pair, and there is no store method that merges two contacts yet — that is
[ADR-0003](../../../adr/0003-dual-source-archive.md)'s Slice 4.5. So in practice
most contacts own a single conversation today, and `SourceCount()` is usually 1
(the header degrades to a single source label). The design is deliberately
forward-compatible: because every aggregate already scopes by the
`contact_id → conversations` subquery, the day a merge method lands, a contact's
profile transparently spans both threads with no query changes — only more rows
matched.

### Zero-conversation resilience

A contact can outlive its conversations (a cross-source identifier surviving a
source deletion). The handler and template tolerate this: `GetContactByID`
returns the identity with empty `Conversations`, the edge probes and every
aggregate return empty/zero, `PrimarySource` stays `""` (no presence dot),
`buildSparkline` short-circuits on empty input, and the facts section shows its
empty state. The page renders without a template crash — the explicit contract in
REQ-0017-008.

### Boosted-partial contract

`contact` composes `page_start` + `contact_content` + `page_end`; a boosted
request renders `contact_content` alone — `<title>` plus `<main
id="main-content">` — so the partial carries no shell and swaps cleanly into
`#main-content`, per [SPEC-0008 REQ-0008-006](../web-performance/spec.md). The
handler uses `partialBase` vs. `s.baseData` on that branch so the sidebar listing
is not recomputed on a partial. A bad, unknown, or group contact id 404s: an
unparseable id, or a `GetContactByID` that returns `(nil, nil)`, both call
`http.NotFound`.

### Deferred derived insights → a future model-stamped NLP table

Sentiment-over-time, shared vocabulary, an activity-rhythm heatmap, median reply
time, and per-day mood are out of scope (REQ-0017-010). They are all
derived-insight surfaces that need a persisted, per-message NLP artifact rather
than a live SQL aggregate. The intended plug-in point is a hash-keyed,
model-stamped table — e.g. `message_sentiment(message_hash, model, …)` — mirroring
how `embeddings` and `contact_facts` carry provenance and stay idempotent across
re-ingest. Landing that table later feeds new tiles/sections without disturbing
this page's contact-scope predicate.

## Testing

- `internal/store/contacts_test.go`: contact-scope aggregation across a merged
  contact's threads, sent/received split by the owner sender, photo counts, UTC
  month/hour bucketing, `ts_unix`-probe edges, top-reaction tally, and the
  zero-conversation contact returning empty (not error).
- `internal/store/facts_test.go`: `ContactFacts` resolving `SourceMessageID` +
  `SourceConversationID` to the owning conversation, and a gone message resolving
  both to 0.
- `internal/web/contact_test.go` (`contact.go`): declared-category grouping order,
  sparkline gap-fill + label-band geometry, presentational-attributes-only SVG,
  the boosted `contact_content` swap unit, and the unknown-contact 404.
