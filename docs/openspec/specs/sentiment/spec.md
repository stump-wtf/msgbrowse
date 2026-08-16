---
status: draft
date: 2026-07-29
implements: [ADR-0028]
extends: [SPEC-0016, SPEC-0017]
---

# SPEC-0027: IPIP-anchored sentiment & trait scoring

- **Capability:** sentiment
- **Target packages:** `internal/sentiment` (new), `internal/store`
  (`schema.go`, new `sentiment.go`), `internal/cli` (new `sentiment.go`),
  `internal/web` (`contact.go`, `journal.go`, `templates/contact.html`,
  `templates/journal.html`)
- **Related ADRs:** [ADR-0028](../../../adr/0028-ipip-sentiment-trait-scoring.md),
  [ADR-0011](../../../adr/0011-contact-facts-extraction.md),
  [ADR-0023](../../../adr/0023-ai-editorialized-journal.md),
  [ADR-0010](../../../adr/0010-security-privacy-posture.md),
  [ADR-0002](../../../adr/0002-vector-backend.md)
- **Related specs:** [SPEC-0017](../contact-profile/spec.md)
  — REQ-0017-010 reserved the slot this spec fills;
  [SPEC-0016](../journal/spec.md) — gains the per-day mood strip;
  [SPEC-0005](../contact-facts/spec.md) — the sibling
  extraction feature whose incremental design this mirrors.

## Overview

msgbrowse scores messages against a curated subset of the public-domain
[IPIP](https://ipip.ori.org) construct taxonomy — the Big Five trait domains
plus ~10 affect facets — using the configured chat model, with IPIP's marker
items as behavioral anchors in the prompt (ADR-0028). Scores land in a sparse,
hash-keyed, model- and lexicon-stamped `message_sentiment` table that survives
re-ingest, exactly as `embeddings` and `contact_facts` do. Three read-side
surfaces consume it: sentiment-over-time and a Big Five trait sketch on the
contact profile (resolving the deferral in SPEC-0017 REQ-0017-010), and a
per-day mood strip in the journal.

Scoring is a deliberate `msgbrowse sentiment` command — never a side effect of
import or serving — whose only network call is `llm.Chat` to `llm.base_url`.
All surfaces are labeled AI-generated with an "expressed in messages, not a
psychological assessment" framing, and a per-contact opt-out excludes a person
from scoring entirely.

## Scope

In scope: the embedded IPIP item table and curated lexicon, the schema
migration (`message_sentiment`, `sentiment_state`, per-contact opt-out), the
incremental scoring engine and CLI command, the two contact-profile surfaces,
the journal mood strip, and the opt-out control.

Non-goals: web-triggered scoring runs (CLI-only in v1), exposing scores over
MCP, translated (non-English) anchor sets, editing or curating scores in the
UI, alerting/notification on mood changes, and any change to SPEC-0017's
contact-scope predicate or SPEC-0016's digest pipeline.

## Requirements

### REQ-0027-001: Embedded IPIP item table and versioned lexicon

The normalized IPIP item-assignment table (columns `instrument, alpha, key,
text, label`) MUST ship in-repo as a `go:embed`-ed data file in
`internal/sentiment`, with an attribution header crediting the IPIP and the
Oregon Research Institute. A curated **lexicon** MUST select the scoring
constructs from that table: the five Big Five domains (Extraversion,
Agreeableness, Conscientiousness, Neuroticism, Intellect/Openness) and a
default affect tier of Anger, Anxiety, Depression, Cheerfulness,
Hope/Optimism, Calmness, Vulnerability, Self-consciousness,
Vitality/Enthusiasm/Zest, and Empathy. Each lexicon construct MUST carry
anchor items selected from the table with both positive and negative keying
represented. The lexicon MUST carry a `lexicon_version` string; any change to
construct membership or anchor selection MUST change the version.

#### Scenario: Lexicon builds from the embedded table
- **Given** the embedded item table and the v1 lexicon definition
- **When** the lexicon is built at startup of a scoring run
- **Then** every lexicon construct resolves to at least one positively keyed and one negatively keyed anchor item from the table, and the build fails loudly (not silently skips) if a construct's label no longer matches the table.

### REQ-0027-002: Schema — sparse, hash-keyed, FK-less score storage

A schema migration MUST add `message_sentiment(message_hash, model,
lexicon_version, construct, score, ts_unix, contact_id)` with
`UNIQUE(message_hash, model, lexicon_version, construct)` and **no** foreign
key to `messages`, and `sentiment_state(conversation_id, last_message_hash,
model, lexicon_version, updated_at)` keyed by conversation. `score` MUST be a
REAL in [−1.0, +1.0]. Writes MUST be idempotent upserts (`INSERT … ON CONFLICT
DO NOTHING`). Storage MUST be sparse: only constructs the model reports as
salient for a message are stored; a message with no salient constructs stores
no rows. Multi-step writes (scores + cursor advance for a batch) MUST occur in
a transaction, and all queries MUST use parameterized SQL.

#### Scenario: Re-ingest does not orphan or cascade
- **Given** scored messages whose conversation is re-imported (rows deleted and re-inserted with new rowids, stable hashes)
- **When** the profile or journal aggregates run afterwards
- **Then** existing scores still join by `message_hash` and no scores were deleted by the re-ingest.

#### Scenario: Rescoring is a no-op
- **Given** a conversation already scored under the current `(model, lexicon_version)`
- **When** `msgbrowse sentiment` runs again with no new messages
- **Then** no LLM calls are made for that conversation and the table is unchanged.

### REQ-0027-003: Anchored scoring engine with defensive parsing

The engine MUST prompt the configured chat model with the lexicon's constructs
and their keyed anchor items, and score message batches per conversation. The
model's response MUST be parsed tolerantly (code fences / surrounding prose
around the JSON MUST NOT fail the batch, mirroring facts). Defensive coercion
MUST apply: scores clamp to [−1.0, +1.0]; constructs not in the lexicon are
dropped; a malformed per-message entry is skipped without failing the batch.
The prompt MUST instruct the model to omit non-salient constructs, and MUST
frame the task as scoring what the *text expresses*, never as assessing the
person. Errors MUST be wrapped with conversation/batch context and reported
via structured logging; a failed batch MUST NOT silently advance the cursor.

#### Scenario: Sloppy model output degrades gracefully
- **Given** a response wrapping the JSON in a code fence, containing one score of 3.7 and one unknown construct "Sassiness"
- **When** the batch is parsed
- **Then** the fence is stripped, 3.7 clamps to 1.0, "Sassiness" is dropped, and the remaining scores are stored.

### REQ-0027-004: Incremental, resumable, model- and lexicon-sensitive runs

Scoring MUST be incremental per conversation via the `sentiment_state` hash
cursor: the stored `last_message_hash` resolves to a `(ts_unix, id)` keyset
position; a hash that no longer resolves restarts the conversation from the
top (safe — writes are idempotent). A stored `model` or `lexicon_version`
differing from the current configuration MUST rescan that conversation from
the start. Conversations MUST be processed by a bounded worker pool
(`--concurrency`, default 4) with context propagation for cancellation; the
cursor MUST be persisted after every batch so a mid-run failure resumes where
it stopped. Concurrent paths MUST be exercised with the race detector in CI.

#### Scenario: Mid-run failure resumes
- **Given** a run that fails at batch N of a long conversation
- **When** `msgbrowse sentiment` runs again
- **Then** scoring resumes from the last persisted cursor, not from the top, and previously stored scores are not duplicated.

#### Scenario: Lexicon bump rescans
- **Given** conversations scored under lexicon v1
- **When** the binary ships lexicon v2 and a run starts
- **Then** every conversation rescans from the top and new rows are stamped `lexicon_version = v2`.

### REQ-0027-005: Privacy gates — exclusion, opt-out, single egress

`journal.exclude_conversations` MUST filter conversations by name before any
message content is read, so excluded threads never reach the engine. A
**per-contact opt-out** MUST exclude all of a contact's messages from scoring;
setting it MUST also delete that contact's existing `message_sentiment` rows
in the same transaction. Scoring MUST run only via the deliberate CLI command
— never as a side effect of import, sync, or serving — and the engine's only
network call MUST be `llm.Chat` to `llm.base_url` (ADR-0010).

#### Scenario: Opt-out is retroactive
- **Given** a contact with 500 stored scores
- **When** the user opts the contact out
- **Then** the 500 rows are deleted, the opt-out persists, and subsequent runs skip every conversation message attributed to that contact.

#### Scenario: Excluded thread never leaves the machine
- **Given** a conversation named in `journal.exclude_conversations`
- **When** a scoring run executes
- **Then** no message content from that conversation is read by the engine and no part of it appears in any LLM request.

### REQ-0027-006: CLI command

`msgbrowse sentiment` MUST mirror the `facts` command surface: `--concurrency`
(default 4), `--reset` (wipe scores and cursors, then rescan), and per-run
progress output (conversations processed, messages scored, rows written). Exit
MUST be non-zero on a run aborted by LLM failure, with the resume behavior of
REQ-0027-004.

#### Scenario: Reset rebuilds from scratch
- **Given** a populated `message_sentiment` table
- **When** `msgbrowse sentiment --reset` completes
- **Then** old rows and cursors are gone and the table reflects a full rescan under the current model and lexicon.

### REQ-0027-007: Contact profile — sentiment over time

The contact profile MUST render a sentiment-over-time surface aggregating the
**affect tier** of the contact's stored scores into month buckets, reading
only rows matching the currently configured `(model, lexicon_version)`. The
surface MUST degrade gracefully: with no scored messages it renders an empty
state (no crash, no fabricated neutral line), preserving SPEC-0017
REQ-0017-008's resilience contract. It MUST be labeled AI-generated with an
"expressed in messages, not a psychological assessment" disclaimer. The
boosted-partial contract of REQ-0017-009 MUST be unchanged: the new surface
renders inside `contact_content` with no new shell.

#### Scenario: Unscored contact renders empty
- **Given** a contact with messages but no `message_sentiment` rows
- **When** `/contact/{id}` renders
- **Then** the sentiment surface shows its empty state ("run msgbrowse sentiment" hint) and the rest of the page is unaffected.

#### Scenario: Affect trend renders from scores
- **Given** a contact with affect-facet scores spanning six months
- **When** `/contact/{id}` renders
- **Then** the surface shows month-bucketed affect intensities with the AI-generated disclaimer visible.

### REQ-0027-008: Contact profile — Big Five trait sketch

The contact profile MUST render a trait sketch from the **domain tier**: the
mean signed score per Big Five domain over the contact's scored messages. To
avoid false precision it MUST render only when the contact has at least a
minimum number of scored messages (default 50); below the threshold it MUST
show the "not enough scored messages" empty state instead of a sketch. The
same AI-generated disclaimer requirement as REQ-0027-007 applies.

#### Scenario: Thin evidence suppresses the sketch
- **Given** a contact with 12 scored messages
- **When** the profile renders
- **Then** no trait sketch appears; the surface explains more scored history is needed.

### REQ-0027-009: Journal — per-day mood

The journal day view MUST render a mood strip for days that have affect-tier
scores, bucketing by **UTC day** exactly as ADR-0023 mandates
(`date(ts_unix,'unixepoch')`, no `'localtime'` conversion). Days without
scores MUST render the journal unchanged — the mood strip is additive and MUST
NOT alter SPEC-0016's mechanical-rollup or digest behavior.

#### Scenario: Day bucketing matches the journal's frame
- **Given** scores for messages straddling midnight UTC
- **When** the journal renders both days
- **Then** each score lands in the same day bucket as the message's journal rollup — no double-shifted bucketing.

### REQ-0027-010: Opt-out control on the contact profile

The contact profile MUST expose the per-contact opt-out (REQ-0027-005) as a
gated control. The mutating POST MUST be protected exactly like the Setup and
contact-settings POSTs: same-origin + per-session token + `MaxBytesReader`,
rejected 403 before any work (`checkSetupPOST`), re-rendering with a
fixed-enum result banner — never request-derived prose.

#### Scenario: Forged opt-out is rejected
- **Given** a POST to the opt-out endpoint without the per-session token
- **When** the handler runs
- **Then** it returns 403 before touching the store and no rows change.

## Security Checklist

- Authentication middleware applied — the only mutating POST (opt-out) is
  behind `checkSetupPOST`: same-origin + per-session token +
  `MaxBytesReader`, 403 before any store work (REQ-0027-010).
- Input validation — the opt-out target is a contact id parsed as an integer
  and resolved via the store; unknown ids 404. Construct names from the LLM
  are validated against the lexicon allowlist before storage (REQ-0027-003).
- Output encoding — construct labels and disclaimers are static template
  strings; nothing request- or model-derived renders unescaped. Surfaces stay
  inside `html/template` with the existing CSP (no inline JS).
- Rate limiting — scoring runs only via the CLI; no web endpoint can trigger
  LLM egress, so there is no scoring endpoint to abuse.
- Request body size limits — `MaxBytesReader` on the opt-out POST.
- Egress — the engine's only network call is `llm.Chat` to `llm.base_url`;
  excluded and opted-out content is filtered before read (REQ-0027-005).

## Accessibility Requirements

The new surfaces are server-rendered additions to existing pages and MUST meet
WCAG 2.1 AA like the rest of the slate UI:

- The sentiment sparkline/trend and mood strip are SVG with presentational
  attributes only (the SPEC-0017 sparkline pattern) and MUST carry a text
  alternative (`role="img"` + `aria-label` summarizing the trend) so the
  surface is not visual-only.
- The trait sketch MUST NOT encode meaning in color alone; each domain shows
  its name and value as text.
- The opt-out control is a native form control with a visible label,
  keyboard-operable, and its result banner MUST be announced via the existing
  `aria-live="polite"` banner region pattern.
- Empty states and disclaimers are plain text within the page's landmark
  structure; no new landmark roles are needed and existing tab order is
  preserved.
