# ADR-0003: Dual-source archive with unified contacts

- **Status:** Accepted
- **Date:** 2026-06-27
- **Supersedes:** the implicit Signal-only model in [ADR-0001](0001-sqlite-driver-mattn-cgo.md) (schema scope)

## Context

The original brief framed msgbrowse (then `sigbrowse`) as a browser and MCP
server over a single `signal-export` archive. After Slices 1 + 2 landed we
pivoted: msgbrowse is becoming a **local, AI-powered backup browser** over two
upstream CLI exporters — `signal-export` (Signal Desktop) and
[`imessage-exporter`](https://github.com/ReagentX/imessage-exporter) (Apple
iMessage) — with an **editorialized journal** as the headline feature.

The journal interleaves "all chats" by day. Leaving iMessage out cuts that
value in half. The downstream layers (SQLite, FTS5, sqlite-vec, MCP, web UI,
journal) are source-agnostic; only the parser and Cowork setup prompt differ
per source. Adding a second source is therefore a small surface-area change
*if* the schema is designed for it from the start.

## Decision

**Hold one unified schema; tag every row with its source; introduce a contacts
layer that maps source-side identities to canonical people.**

### Source tag

A `source TEXT NOT NULL` column on `conversations`, `messages`, and
`ingest_runs`, plus on `contact_identifiers`. Canonical values live in
`internal/source` (`Signal`, `IMessage`) — the literals are persisted to disk
and must never be renamed. Migration v2 stamps every pre-existing row
`source='signal'`.

### Conversation uniqueness

The v1 `UNIQUE(name)` on conversations becomes `UNIQUE(source, name)`. Two
conversations called "MJ" — one on Signal, one on iMessage — must coexist as
distinct rows. The v2 migration rebuilds the conversations table to swap the
constraint, copying the existing rows verbatim (already-tagged
`source='signal'`).

### Contacts layer

- `contacts(id, display_name, notes)` — the canonical person.
- `contact_identifiers(id, contact_id, source, identifier, UNIQUE(source, identifier))` — the source-side handle (e.g. `signal:MJ`, `imessage:+15551234567`).
- `conversations.contact_id` (nullable) — the conversation's owner, when 1:1. NULL for groups (`is_group=1`).

### Auto-creation on import

When an import sees a new `(source, identifier)` it auto-creates a contact with
`display_name = identifier` and links it. No silent cross-source merging. The
contacts page (Slice 4.5) is where you merge `signal:MJ + imessage:+1555…` into
one canonical contact — and it MUST be a manual confirmation, never a heuristic.

## Why these specific choices

### Why a unified schema, not separate Signal/iMessage tables

The journal asks "what happened on this day, across everyone you talk to"; the
MCP RAG asks "what did MJ say about the lease, anywhere." Both queries are
trivial against one table and gnarly across two parallel ones (UNION ALL +
schema drift risk). The cost of `source TEXT NOT NULL` on every row is one
indexed column.

### Why manual contact merging, not auto-merge on phone collisions

A wrong merge corrupts the journal's per-person history irrecoverably (the
LLM-written digests propagate it forward). The macOS Contacts vCard is the
right *suggestion* source but the wrong *decision* source — shared phone
numbers across family plans, renamed contacts, and stale Address Book entries
are real. The Slice 4.5 page presents Contacts suggestions; the user confirms.

### Why now (Slice 1.5), before iMessage parser code exists

Schema changes are cheap when the database is empty and there is one importer;
they are expensive when there are many users with live data. Doing the v2
migration **now** ensures the iMessage importer in Slice 2.5 plugs into a
schema that already accommodates it, and the editorialized-journal work in
Slice 6 sees a populated contacts layer from day one.

### Why keep `internal/signal/` named after its source instead of generalizing it

Each parser is source-specific in detail (timestamp format quirks, attachment
markup, reaction handling). Generalization across sources would force a
lowest-common-denominator parser and lose detail. Two sibling packages —
`internal/signal` and `internal/imessage` — are the cleanest factoring; they
share the same `signal.Message` data shape because the message model after
parsing is genuinely the same across sources.

## Consequences

### Positive

- The store, FTS5, web UI, MCP, and journal layers are all written once and
  serve both sources from day one.
- Adding a third source (WhatsApp, Telegram, …) is "write a parser package +
  add a source constant + add an import subcommand," no schema migration.
- The contacts page becomes a high-leverage feature: one place to reconcile
  cross-source identity.
- The mechanical and editorial journal layers don't need a "which source?"
  switch — they just read the unified store.

### Negative

- `UpsertConversation` now performs a transactional find-or-create across
  three tables (conversations, contacts, contact_identifiers). More moving
  parts than the v1 `INSERT … ON CONFLICT DO NOTHING`.
- Migration v2 rebuilds the conversations table. SQLite's table-rebuild
  pattern requires `PRAGMA foreign_keys = OFF` around the apply — the
  migration runner handles this on a dedicated connection so concurrent
  readers in the connection pool aren't affected, but the pattern is a
  footgun if future migrations forget it.
- A future "merge MJ across sources" UI must repoint every
  `contact_identifiers.contact_id` AND `conversations.contact_id` for the
  losing contact and then delete the losing contact. This is the contacts
  page's main job; it is not free.

### Operational

- The Cowork setup prompt count in the README doubles (one per source).
- SECURITY.md now describes a slightly larger egress surface: any source can
  end up in the journal digest pass, so the `journal.exclude_conversations`
  denylist is the right per-thread privacy control regardless of source.

## Alternatives considered

- **Two separate databases, one per source.** Rejected: the journal and MCP
  queries are fundamentally cross-source, and the schema is the same anyway.
- **Auto-merge contacts using macOS Contacts vCards on import.** Rejected: a
  wrong merge corrupts the journal forever; manual confirmation is the only
  safe default.
- **Defer the schema change until iMessage is actually wired up.** Rejected:
  schema migrations are cheap now and expensive later; the brief is locked
  enough that there's no realistic future in which msgbrowse stays
  Signal-only.

## References

- [feature/2 in the project plan: editorialized journal](../../README.md)
- [ADR-0001: SQLite driver — mattn + cgo](0001-sqlite-driver-mattn-cgo.md)
- [ADR-0002: vector backend — sqlite-vec extension](0002-vector-backend.md)
- [imessage-exporter (ReagentX)](https://github.com/ReagentX/imessage-exporter)
- [signal-export (carderne)](https://github.com/carderne/signal-export)
