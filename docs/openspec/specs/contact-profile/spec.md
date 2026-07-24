# SPEC-0017: Contact profile

- **Status:** Accepted
- **Date:** 2026-07-11
- **Capability:** contact-profile
- **Source packages:** `internal/store` (`contacts.go`, `facts.go`, `query.go`), `internal/web` (`contact.go`, `templates/contact.html`, `server.go`)
- **Related ADRs:** [ADR-0003 (dual-source archive)](../../../adr/0003-dual-source-archive.md), [ADR-0011 (contact facts extraction)](../../../adr/0011-contact-facts-extraction.md), [ADR-0010 (security & privacy posture)](../../../adr/0010-security-privacy-posture.md)

## Overview

msgbrowse gives every person a single profile page keyed by **contact** — the
merged-person grain, not the per-source conversation. The page gathers the
contact's merged identity, the AI-gathered facts about them ([SPEC-0005](../contact-facts/spec.md)),
a set of derivable profile statistics, a message-volume sparkline, and their top
reactions. Because there is no `contact_id` column on `messages`, `attachments`,
or `reactions`, every aggregate spans the contact's conversations through one
load-bearing predicate. The page reuses the existing schema — no new tables — and
holds to the archive's UTC and ordering discipline ([SPEC-0008](../web-performance/spec.md)).

## Requirements

### REQ-0017-001: Contact-keyed profile page reached from the transcript header

The profile MUST be served at `GET /contact/{id}`, keyed by `contacts(id)` (the
merged-person grain). It MUST be reachable from the transcript header, where the
conversation name links to `/contact/{ContactID}` **only** when the conversation
is linked to a contact; a group or an unlinked source thread (`ContactID == 0`)
MUST render the name as plain text with no link.

#### Scenario: Header links a linked conversation to its person
- **Given** a conversation whose `ContactID` is greater than 0
- **When** the transcript header renders
- **Then** the person's name is a boosted link to `/contact/{ContactID}`; a group or unlinked thread renders the name unlinked.

### REQ-0017-002: Contact-scoped aggregation via the conversation subquery

Every per-contact aggregate (stats, photos, message volume, most-active hour,
reactions, edge timestamps) MUST scope its rows with the predicate
`conversation_id IN (SELECT id FROM conversations WHERE contact_id = ?)`. The
feature MUST introduce no new tables and MUST read only from `contacts`,
`contact_identifiers`, `contact_facts`, `conversations`, `messages`,
`reactions`, and `attachments`.

#### Scenario: A merged contact's aggregates span every owned thread
- **Given** a contact that owns a Signal and an iMessage conversation
- **When** the profile computes message counts, photos, volume, and reactions
- **Then** each aggregate covers rows from both conversations via the subquery predicate, with no `contact_id` column on the message-grain tables.

### REQ-0017-003: Merged-identity header

The header MUST list the contact's **full** identifier set across sources with no
self-identity exclusion (unlike the conversation view, which hides the thread's
own handle). When the contact's identifiers span more than one source, the
subtitle MUST read "Same person across N sources · merged"; when
`SourceCount == 1` it MUST degrade to the single human source label.

#### Scenario: Merged subtitle vs. single source
- **Given** a contact whose identifiers span two sources
- **When** the header renders
- **Then** it shows every identifier and a "Same person across 2 sources · merged" subtitle; a single-source contact shows only that source's label instead.

### REQ-0017-004: AI facts grouped by declared category with own-conversation deep links

Stored facts MUST be grouped by category in the DECLARED `facts.Categories`
order (not SQL's alphabetical order), skipping empty categories. Each fact MUST
deep-link to `/c/{SourceConversationID}/at/{SourceMessageID}` when its supporting
message still resolves (both ids > 0), targeting the message's OWN owning
conversation. A fact whose source message is gone (resolved id 0) MUST render as
undated/link-less text, never a broken link. Fact text is model-derived and
untrusted and MUST be emitted through `html/template` auto-escaping.

#### Scenario: Cited fact deep-links to its own conversation
- **Given** a contact with a stored fact whose source message resolves to conversation C and message M
- **When** the profile renders the facts section
- **Then** the fact links to `/c/C/at/M`; a fact whose source message no longer resolves renders as plain, link-less text.

### REQ-0017-005: Derivable profile statistics and top reactions

The profile MUST show derivable stat tiles computed from the contact's messages:
total messages, messages you sent vs. received (split by the owner sender),
photos shared, a messages/day pace, the most-active hour, and the first-message
date. It MUST also show the contact's top reactions grouped by emoji, most
frequent first. Counts MUST come from a single stats scan; `GetContactByID` MUST
NOT also scan for counts.

#### Scenario: Stat tiles and reactions from one scan
- **Given** a contact with messages and reactions
- **When** the profile renders
- **Then** the tiles show total, sent/received, photos, pace, most-active hour, and first-message date, plus the top reactions grouped by emoji — with counts sourced from `ContactStats`, not a second identity-query scan.

### REQ-0017-006: Gap-filled message-volume sparkline as CSP-safe inline SVG

The profile MUST render a message-volume sparkline. Per-month store buckets MUST
be rolled up to years in the view and GAP-FILLED across the full year span — a
silent year MUST appear as an empty slot with its label, never a collapsed axis.
The sparkline MUST be inline SVG whose geometry is pre-computed in Go and emitted
using presentational ATTRIBUTES only (`x`, `y`, `width`, `height`,
`fill-opacity`); it MUST NOT use inline `style=`, which the strict CSP forbids.

#### Scenario: A silent year keeps its slot
- **Given** message volume with traffic in 2021 and 2023 but none in 2022
- **When** the sparkline builds
- **Then** 2022 renders as a labeled zero-height slot between the two, and every bar is an SVG element with presentational attributes and no inline `style=`.

### REQ-0017-007: UTC bucketing and probe-based timestamp ordering

Hour-of-day and month bucketing MUST use UTC via `strftime(..., 'unixepoch')`
for deterministic, testable output. Earliest/latest timestamp selection MUST use
indexed `ts_unix` probes and MUST NOT use `MIN(ts)`/`MAX(ts)` string aggregation,
whose iMessage month-name strings sort wrong ([SPEC-0008 REQ-0008-002](../web-performance/spec.md)).

#### Scenario: Deterministic buckets and correct edges
- **Given** a contact whose messages span multiple months and time zones
- **When** volume, most-active hour, and first/last dates are computed
- **Then** buckets use UTC `strftime(..., 'unixepoch')` and the first/last dates come from `ts_unix` probes, never string `MIN/MAX(ts)`.

### REQ-0017-008: Zero-conversation contacts still render

A contact with zero conversations (e.g. a cross-source identifier that survived a
source deletion) MUST still render the profile without a template crash. Empty
aggregates MUST degrade gracefully: no sparkline, zeroed tiles, an unknown
most-active hour, and no source presence dot.

#### Scenario: Orphaned contact renders empty
- **Given** a contact with identifiers but no conversations or messages
- **When** `/contact/{id}` is requested
- **Then** the page renders with zeroed stats, no sparkline, an em-dash most-active hour, and no crash.

### REQ-0017-009: Boosted-partial swap unit and 404 for unknown contacts

`contact_content` (its `<title>` plus `<main id="main-content">`) MUST be the
`#main-content` swap unit for boosted navigation, carrying no shell on a partial
request ([SPEC-0008 REQ-0008-006](../web-performance/spec.md)). A bad, unknown, or
group contact id MUST return 404.

#### Scenario: Partial swap and unknown-contact 404
- **Given** a boosted request to `/contact/{id}` for an existing contact
- **When** the handler renders
- **Then** it returns only `contact_content` with no shell; a request for a non-existent contact id returns 404.

### REQ-0017-010: Deferred derived-insight surfaces are non-goals

Sentiment-over-time, shared-vocabulary, an activity-rhythm heatmap, median reply
time, and per-day mood are explicit NON-GOALS of this spec. They MUST NOT be
implemented here. Their future home is a hash-keyed, model-stamped NLP table
(e.g. `message_sentiment`) mirroring the provenance and idempotency pattern of
`embeddings` and `contact_facts`, so they can be layered in without changing this
page's contact-scope predicate.

#### Scenario: Deferred surfaces are absent by design
- **Given** the current profile
- **When** it renders
- **Then** no sentiment, vocabulary, heatmap, reply-time, or mood surface appears, and the deferred work is documented as a future model-stamped NLP table rather than a stub.
