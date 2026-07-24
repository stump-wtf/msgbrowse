# ADR-0023: AI-editorialized journal — mechanical day rollups + cached LLM digests

- **Status:** Accepted
- **Date:** 2026-07-11
- **Relates to:** [ADR-0011](0011-contact-facts-extraction.md) (contact facts — the sibling extraction feature this mirrors: single egress, exclude list, model-versioned cache), [ADR-0010](0010-security-privacy-posture.md) (single egress, exclude list), [ADR-0002](0002-vector-backend.md) (FK-less derived cache precedent), [ADR-0003](0003-dual-source-archive.md) (dual-source archive), [SECURITY.md](../../SECURITY.md) (egress posture)

## Context

msgbrowse should answer "what happened on this day?" the way [contact facts](0011-contact-facts-extraction.md)
answers "who is this person?" — an editorialized layer over the raw transcript.
We want a day-by-day journal: a factual, always-available rollup of each day's
activity, plus an optional short prose digest the LLM writes about the day.

The design splits cleanly into **two layers** with very different costs and trust
properties:

- A **mechanical day journal** — a deterministic per-day rollup (message count,
  conversation count, per-source counts, top senders) derived entirely from the
  local `messages` table. No LLM, no network, always available.
- An **LLM digest** — one prose paragraph per day, written by the configured
  chat model. This is the only part that leaves the machine.

Constraints from the rest of the system:

- **One egress, opt-in cost.** LLM calls go only to `llm.base_url` and must be a
  deliberate step, never a side effect of import or serving ([ADR-0010](0010-security-privacy-posture.md),
  [SECURITY.md](../../SECURITY.md)). The digest is exactly the egress posture the
  `facts` feature already established.
- **Re-ingest churn.** `ReplaceConversationMessages` deletes and re-inserts a
  conversation's rows on every import (new rowids, stable content), exactly as it
  does for embeddings ([ADR-0002](0002-vector-backend.md)) and facts
  ([ADR-0011](0011-contact-facts-extraction.md)).
- **Day boundaries are a correctness hazard.** `messages.ts` is a wall-clock
  string `'YYYY-MM-DD HH:MM:SS'` and `messages.ts_unix` is that string parsed
  **as UTC** (`internal/signal/message.go`: local time with no zone, parsed as
  UTC purely for a stable ordering key). A day bucket must be derived in the same
  frame or messages land in the wrong day.
- **Privacy.** `journal.exclude_conversations` names threads that must never be
  sent to any LLM.

## Decision

A real `msgbrowse journal` command (replacing the current `errNotImplemented`
stub) builds both layers, modeled directly on the `facts` command's incremental,
single-egress, model-versioned design.

1. **Two layers, independently gated.** The mechanical day journal is **always**
   built from `messages` regardless of `journal.digest_enabled`, with no LLM and
   no network. The LLM digest is built **only** when `journal.digest_enabled` is
   true (the default) and is the only new egress — one `llm.Chat` call per day to
   `llm.base_url`, mirroring facts.

2. **Day bucketing is UTC.** A day is `substr(ts,1,10)`, equivalently
   `date(ts_unix,'unixepoch')`. Because `ts_unix` is the wall-clock string parsed
   *as UTC*, this is a pure slice with **no timezone conversion**. Using
   `'localtime'` would double-shift the already-UTC value and misfile messages
   across midnight — this is the single highest-consequence correctness
   constraint of the feature.

3. **Schema v11: two day-keyed, FK-less tables.** `journal_days` (`PRIMARY KEY
   day`) caches the mechanical rollup; `journal_digests` (`PRIMARY KEY day`, plus
   `model`, `prompt_version`, `body`, `updated_at`) caches the digest. **Neither
   has a foreign key to `messages`:** re-ingest deletes and re-inserts message
   rowids, so a CASCADE would wipe every derived journal row on each import —
   the same reasoning that keeps embeddings (v3) and contact_facts (v4) FK-less.
   Both are day-keyed, so the migration's `foreign_key_check` passes trivially.

4. **Digest cache keyed by (day, model, prompt_version).** `prompt_version =
   sha256hex(lower(trim(effective DigestPrompt)))` — the exact hashing recipe as
   `factHash` in `internal/store/facts.go`. Editing `journal.digest_prompt` or
   switching `llm.chat_model` changes the key, so the cached digest no longer
   matches and that day becomes eligible again on the next run. A day whose cached
   `(model, prompt_version)` still matches is skipped with zero LLM calls.

5. **One digest per day.** The digest is a single cross-thread daily summary,
   matching `config.DefaultDigestPrompt` ("summarizing one day"). Per-conversation
   digests are an explicit future extension, not in scope — the schema keys on
   `day` alone.

6. **Honors the exclude list before assembly.** `journal.exclude_conversations`
   is applied during day enumeration and transcript assembly, **before** any
   content is gathered, so excluded threads never reach the transcript, let alone
   the endpoint — the same boundary `FactConversations` enforces for facts. The
   sole egress is `llm.Chat` to `llm.base_url`.

7. **CLI semantics and an honest `--dry-run`.** A default run is **incremental**:
   it digests every day whose cached digest is absent or stale by
   `(model, prompt_version)`, bounded by `journal.max_days_per_run` for cron use
   and reporting the count of days left. `--backfill` applies the same
   eligibility across all history (unbounded), `--regenerate` wipes all digests
   then rebuilds, and `--since YYYY-MM-DD` sets a day floor. `--dry-run` makes
   **zero** `Chat` calls: it enumerates the days lacking a current digest and
   estimates input tokens with a local `len(runes)/4` heuristic. It deliberately
   prints **no dollar figure** — the codebase has no tokenizer and no price table,
   and the OpenAI-compatible response's `usage` object is not decoded today, so a
   fabricated cost would be dishonest. A real cost estimate is future work.

## Consequences

- The mechanical journal is free, deterministic, and always present: it needs no
  LLM, never egresses, and is a rebuildable cache, so a stale or missing
  `journal_days` row is repaired by a cheap re-derive from `messages`.
- The digest inherits facts' privacy and cost posture exactly — one auditable
  egress, gated behind an explicit command, defaulting to a local endpoint, with
  the exclude list applied before any content is assembled ([ADR-0010](0010-security-privacy-posture.md),
  [SECURITY.md](../../SECURITY.md)).
- Cache invalidation is automatic and cheap: a prompt edit or model switch
  re-derives digests without a `--reset`-style wipe, because the key carries both
  identifiers; `--regenerate` remains for a forced rebuild.
- The UTC bucketing rule is load-bearing and must be preserved everywhere a day
  is derived (SQL and Go). A single `'localtime'` slip silently misfiles messages
  around midnight; it is called out in the schema comment and the spec.
- `--dry-run` is honest about what it cannot know: it reports day count and a
  coarse token estimate, not money. A precise estimate is deferred and would need
  a price-config knob plus decoding the provider's `usage` object.
- Deferred (stated non-goals): per-conversation digests, image-caption and
  audio-transcript enrichment ([SECURITY.md](../../SECURITY.md) Slice 6), and a
  real per-run cost/price estimate. The schema (day-keyed rows, not a blob)
  leaves room to layer these on later.
