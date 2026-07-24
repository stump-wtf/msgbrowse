# SPEC-0016: AI-editorialized journal

- **Status:** Accepted
- **Date:** 2026-07-11 (Phase 2 revision: 2026-07-12)
- **Capability:** journal
- **Source packages:** `internal/journal` (`journal.go`), `internal/store` (`journal.go`, `journal_calendar.go`, `schema.go` v11), `internal/config` (`config.go` `DefaultDigestPrompt`), `internal/cli` (`journal.go`), `internal/web` (`journal.go`, `templates/journal.html`, `templates/home.html`)
- **Related ADRs:** [ADR-0023 (AI-editorialized journal)](../../../adr/0023-ai-editorialized-journal.md), [ADR-0011 (contact facts extraction)](../../../adr/0011-contact-facts-extraction.md), [ADR-0010 (security & privacy posture)](../../../adr/0010-security-privacy-posture.md)
- **Related specs:** [SPEC-0005 (contact facts)](../contact-facts/spec.md) — the sibling extraction feature

## Overview

msgbrowse builds a day-by-day journal over the local archive in two layers. The
**mechanical** layer is a deterministic per-day rollup (counts and top senders)
derived entirely from `messages` with no LLM and no network egress; it is always
built. The **digest** layer is one prose summary per day written by the
configured chat model, cached in SQLite and keyed by `(day, model,
prompt_version)`; it is the only network egress and is gated on
`journal.digest_enabled`. Both layers bucket messages by **UTC** calendar day,
honor `journal.exclude_conversations` before any content is assembled, and are
re-ingest-safe (day-keyed, no foreign key to `messages`).

**Phase 2 upgraded the digest from a single prose paragraph to a structured
editorial object** — the LLM now returns a JSON document (summary, people,
themes, mood, timed highlights, standout media, notable links) parsed tolerantly
and cached alongside a plain-text summary fallback, with `mood` denormalized into
its own column. The read surface changed to match: the flat, keyset-paginated day
list (REQ-0016-010) is superseded by a **mood-tinted month-calendar navigator**
with an editorial day card and headline stat tiles (REQ-0016-014). Phase 2
requirements (REQ-0016-011 through REQ-0016-016) add to the Phase 1 requirements
below; none of the two-layer, UTC-bucketing, cache-key, or privacy invariants
(REQ-0016-001 through REQ-0016-009) changed.

## Requirements

### REQ-0016-001: Mechanical day journal, deterministic and egress-free

The mechanical journal MUST be a per-day rollup — message count, conversation
count, per-source counts, and top senders — derived solely from the local
`messages` table, with NO LLM call and NO network egress. It MUST be built on
every `msgbrowse journal` run regardless of `journal.digest_enabled`. Rebuilding
it MUST be idempotent: a re-run over unchanged messages MUST produce the same
`journal_days` rows (an upsert keyed on `day`), never duplicates.

#### Scenario: Mechanical build never egresses
- **Given** an imported archive and `journal.digest_enabled = false`
- **When** the user runs `msgbrowse journal`
- **Then** per-day `journal_days` rows are written from `messages` alone, no call is made to `llm.base_url`, and re-running produces identical rows.

### REQ-0016-002: UTC day bucketing

A day MUST be derived as `substr(ts,1,10)` (equivalently
`date(ts_unix,'unixepoch')`). Because `messages.ts_unix` is the wall-clock string
parsed **as UTC**, day bucketing MUST NOT apply any timezone conversion; using
`'localtime'` (which would double-shift the already-UTC value and misfile
messages across midnight) is prohibited. The mechanical rollup and the transcript
assembled for a digest MUST use the same UTC day definition.

#### Scenario: A late-night message stays on its own UTC day
- **Given** a message with `ts = '2026-07-01 23:30:00'` (`ts_unix` = that instant as UTC)
- **When** the journal is built
- **Then** it is counted under day `2026-07-01`, and no `'localtime'` conversion shifts it into an adjacent day.

### REQ-0016-003: Opt-in digest as the single egress

The LLM digest MUST be produced only when `journal.digest_enabled` is true, and
each digest MUST be the only network egress — one `llm.Chat` call per day to
`llm.base_url` using `llm.chat_model`. Import and `serve` MUST NOT trigger digest
generation. A per-day digest call MUST use a longer per-call context timeout
(~180s) than the facts default, because a full day's transcript can be large.

#### Scenario: Digest is explicit and gated
- **Given** `journal.digest_enabled = true` and days with no cached digest
- **When** the user runs `msgbrowse journal`
- **Then** each eligible day is summarized via `llm.base_url`; running `signal-import` or `serve` alone never calls the LLM, and setting `digest_enabled = false` produces the mechanical journal with zero LLM calls.

### REQ-0016-004: Digest cache keyed by day, model, and prompt version

Each digest MUST be cached in `journal_digests` (`PRIMARY KEY day`) alongside the
`model` and `prompt_version` that produced it, where `prompt_version =
sha256hex(lower(trim(effective DigestPrompt)))` (the same normalization recipe as
`factHash`). A day whose cached `(model, prompt_version)` matches the current
effective values MUST be skipped with no LLM call. Editing `journal.digest_prompt`
or switching `llm.chat_model` MUST make previously-digested days eligible again on
the next run. There MUST be no foreign key from `journal_digests` (or
`journal_days`) to `messages`.

#### Scenario: Prompt change invalidates the cache
- **Given** a day already digested under prompt version `P1` and model `M`
- **When** `journal.digest_prompt` is edited (yielding version `P2`) and `msgbrowse journal` runs again
- **Then** that day is re-digested and its row is updated to `(M, P2)`; with the prompt and model unchanged instead, the day is skipped with no LLM call.

### REQ-0016-005: One digest per day

The digest MUST be a single cross-thread summary per calendar day (matching
`config.DefaultDigestPrompt`, "summarizing one day"), keyed on `day` alone.
Per-conversation digests are OUT OF SCOPE for this capability.

#### Scenario: Multi-thread day yields one digest
- **Given** a day with messages across several conversations
- **When** the digest for that day is generated
- **Then** exactly one `journal_digests` row is written for that day, summarizing the day as a whole.

### REQ-0016-006: Honor the exclude list before assembly

`journal.exclude_conversations` MUST be applied during day enumeration and
transcript assembly, **before** any message content is gathered, so an excluded
conversation's content never reaches the assembled transcript or the LLM. This
MUST NOT affect a day's inclusion when other, non-excluded conversations have
messages that day.

#### Scenario: Excluded thread is never sent
- **Given** a conversation named in `journal.exclude_conversations` with messages on a given day
- **When** that day's digest is generated
- **Then** the excluded conversation's content is absent from the transcript sent to `llm.base_url`, while non-excluded threads from the same day are summarized normally.

### REQ-0016-007: CLI flags and run semantics

`msgbrowse journal` MUST expose `--since YYYY-MM-DD`, `--backfill`,
`--regenerate`, and `--dry-run`. A default run MUST be **incremental**: it
digests every day whose cached digest is absent or stale by `(model,
prompt_version)`. `--backfill` MUST apply the same eligibility across all history
with no day cap. `--regenerate` MUST wipe all cached digests and rebuild them.
`--since` MUST set a day floor (process only days on or after the given date).

#### Scenario: Incremental default skips current days
- **Given** some days already digested under the current model and prompt and some not
- **When** `msgbrowse journal` runs with no flags
- **Then** only days lacking a current digest are sent to the LLM; already-current days are skipped.

#### Scenario: Regenerate rebuilds everything
- **Given** a fully-digested history
- **When** `msgbrowse journal --regenerate` runs
- **Then** all cached digests are wiped and re-generated for every eligible day.

### REQ-0016-008: Per-run day cap with remaining count

An incremental run MUST cap the number of days it digests at
`journal.max_days_per_run` (0 = unbounded) so a scheduled run has a bounded cost,
and MUST report how many eligible days remain after the cap so the operator can
schedule follow-up runs. `--backfill` MUST ignore the cap.

#### Scenario: Cap bounds a cron run and reports the remainder
- **Given** 50 days eligible for a digest and `journal.max_days_per_run = 20`
- **When** `msgbrowse journal` runs
- **Then** 20 days are digested and the run reports 30 days remaining; a subsequent run digests the next batch.

### REQ-0016-009: Dry-run makes zero LLM calls

`--dry-run` MUST make NO `llm.Chat` calls. It MUST enumerate the days that lack a
current digest and estimate input tokens with a local `len(runes)/4` heuristic.
It MUST NOT print a dollar/cost figure — the codebase has no tokenizer or price
table and does not decode the provider's `usage` object, so a real cost estimate
is explicitly future work.

#### Scenario: Dry-run reports days and a token estimate only
- **Given** several days lacking a current digest
- **When** `msgbrowse journal --dry-run` runs
- **Then** it prints the eligible-day count and an approximate input-token total (`runes/4`), makes no network call, and prints no dollar amount.

### REQ-0016-010: Journal web page and home quick-link

> **Phase 2 (superseded presentation):** the flat, keyset-paginated day list this
> requirement describes has been replaced by the mood-tinted month-calendar
> navigator specified in **REQ-0016-014**. The load-bearing invariants below carry
> forward unchanged — a day renders its cached digest when present and falls back
> to the mechanical rollup otherwise, an empty archive renders an empty state, the
> home page offers a fourth quick-link tile to `/journal`, and rendering the page
> issues **no** LLM call. Only the day-navigation surface changed (keyset list →
> calendar grid + editorial day card); the scenarios below record the Phase 1
> behavior.

`GET /journal` MUST list days newest-first, showing each day's digest when one is
cached and falling back to the mechanical rollup when it is not, with keyset
pagination over days and an empty state when no days exist. The home page MUST
offer a fourth quick-link icon tile linking to `/journal`. Rendering the page
MUST NOT trigger any LLM call.

#### Scenario: Days render newest-first with fallback
- **Given** a day with a cached digest and an earlier day with only a mechanical rollup
- **When** the user opens `/journal`
- **Then** the digested day appears first showing its prose, the earlier day shows its counts and top senders, older days page in via keyset pagination, and no LLM call is made.

#### Scenario: Empty state
- **Given** an archive with no imported messages
- **When** the user opens `/journal`
- **Then** the page renders an empty state rather than an error or a blank list.

### REQ-0016-011: Structured editorial digest with tolerant parsing

The digest MUST be produced as a single JSON object with the keys `summary`
(string), `people` (string array), `themes` (string array), `mood` (enum string),
`highlights` (array of `{text, time}` objects), `standout_media` (string array),
and `notable_links` (string array), matching `config.DefaultDigestPrompt`. The
response MUST be parsed **tolerantly**: the object is extracted from the first
`{` to the last `}` (tolerating markdown fences or surrounding prose), and every
field is coerced rather than rejected — each list item is trimmed and empty items
are dropped, an unknown `mood` is coerced to `"neutral"`, and a malformed `HH:MM`
highlight `time` is blanked (`""`) while its `text` is kept. A response that
carries **no JSON object** or whose `summary` is empty MUST be treated exactly
like an empty body: the day is skipped-and-logged (counted as skipped, no row
written) so a re-run retries it, and it MUST NOT wedge the resumable run. The
validated digest MUST be re-canonicalized to JSON before it is stored.

#### Scenario: Fenced, partly-malformed response is coerced, not rejected
- **Given** an LLM response wrapping the JSON object in markdown fences with a blank `people` entry, an unknown `mood`, and one highlight whose `time` is not `HH:MM`
- **When** the digest is parsed
- **Then** the object is extracted from the fences, the empty `people` item is dropped, `mood` becomes `"neutral"`, the bad highlight time is blanked while its text is retained, and the canonicalized digest is stored.

#### Scenario: No-JSON or empty-summary response is skipped, not persisted
- **Given** an LLM response containing no JSON object (or a JSON object with an empty `summary`)
- **When** that day is digested
- **Then** no `journal_digests` row is written, the day is logged and counted as skipped, and a subsequent run treats the day as still eligible.

### REQ-0016-012: Mood is a fixed enum, coerced and denormalized

`mood` MUST be one of a fixed allowlist — `journal.Moods` = `upbeat`, `neutral`,
`quiet`, `tense` — kept in sync with `config.DefaultDigestPrompt`. Any value
outside the allowlist (including an empty or unrecognized string) MUST be coerced
to `"neutral"` at parse time. The coerced mood MUST be denormalized into the
`journal_digests.mood` column so the calendar and stat reads can tint day cells
without unmarshaling the structured blob per day.

#### Scenario: Unknown mood degrades to neutral
- **Given** a model that returns `"mood": "chaotic"`
- **When** the digest is parsed and stored
- **Then** `journal_digests.mood` is written as `"neutral"`, and the calendar reads the column directly without parsing `structured`.

### REQ-0016-013: Structured columns via in-place schema amendment and prompt-version re-derivation

`journal_digests` MUST carry a `structured` column (the canonical JSON of the
validated digest) and a `mood` column, both defaulting to `''` so a prose-only or
legacy row reads cleanly; `body` MUST remain the plain-text summary used as the
fallback and empty-response guard. Because these columns were added by amending
`schemaV11` **in place** (safe only while v11 is unmerged/unshipped), and because
`journal_digests` is a derived cache with no source-of-truth data, no schema
version bump and no `--regenerate` are required to populate them: editing
`config.DefaultDigestPrompt` changes `prompt_version`, which makes every existing
prose-era digest eligible again and re-derives it in structured form on the next
run — a free data migration. A database already at `user_version = 11` from before
the amendment MUST be understood NOT to retroactively gain the new columns (the
`CREATE TABLE IF NOT EXISTS` is a no-op on an existing table); the documented
remedy is to recreate the derived-cache database (it is rebuildable from
`messages`), not to hand-alter the table.

#### Scenario: Prompt-version bump re-derives prose rows as structured
- **Given** days digested before Phase 2 (prose `body`, empty `structured`/`mood`) and a `DefaultDigestPrompt` edited to demand the JSON object
- **When** `msgbrowse journal` runs against a database that has the amended `journal_digests` shape
- **Then** each affected day is re-digested under the new `prompt_version`, and its row is updated in place with canonical `structured` JSON and a denormalized `mood`, with no schema bump and no `--regenerate`.

### REQ-0016-014: Mood-tinted month-calendar navigator with editorial day card and stat tiles

`GET /journal` MUST render a mood-tinted **month calendar** navigator: year tabs
(newest first), a fixed month grid whose present days are tinted by that day's
mood and annotated with a message-count subscript, previous/next-month controls,
and a mood legend. It MUST render an **editorial day card** for the selected day
(summary, timed highlights, people, themes, standout media, notable links) and a
row of **stat tiles** (days-with-entries, longest streak, most-active weekday,
peak hour). Navigation MUST be by boosted query parameters (`?year`, `?month`,
`?day`) with no client-side state. The bare `/journal` MUST open on the newest
day's card; a year tab (`?year` with no month) MUST open on that year's latest
active month, never an empty January. When the selected day has no structured
digest, the card MUST fall back to the prose `body`, then to the mechanical
top-senders. Rendering MUST issue no LLM call, and an archive with no journal MUST
render the empty state.

#### Scenario: Bare landing opens the newest day
- **Given** a built journal whose newest day is `2026-07-11`
- **When** the user opens `/journal` with no query parameters
- **Then** the calendar shows July 2026, `2026-07-11` is selected and its editorial card is rendered, and no LLM call is made.

#### Scenario: Year tab opens the year's latest active month
- **Given** a year whose activity begins in March
- **When** the user clicks that `?year` tab (no month specified)
- **Then** the calendar opens on that year's most recent active month, not January, and no empty grid is shown.

#### Scenario: Day cell tinted by mood with a count subscript
- **Given** a day with a cached digest whose mood is `tense` and 42 messages
- **When** the month grid renders
- **Then** that day's cell carries the `tense` mood tint and a `42` count subscript and links to `?day=` for that date; days without content are inert blanks.

### REQ-0016-015: Calendar read surface, UTC-bucketed and exclude-honoring

The calendar/stat reads MUST be served by dedicated store methods:
`JournalMonth` (≤31 rows off `journal_days` joined to `journal_digests.mood`),
`JournalStats`, `GetJournalDay` (rollup joined with its digest, `structured` and
`mood` included), `LatestJournalDay` and `LatestJournalDayInYear`, and
`JournalYears`. `JournalStats` MUST derive days-with-entries and longest streak
from the `journal_days` key set in Go (`longestStreak`, adjacency by date
arithmetic so month/year rollovers count), and most-active weekday and peak hour
as argmax `GROUP BY` reads over `messages` (`journalArgmax`), a year bounded by a
**sargable `ts_unix` range** (not a `date(ts_unix)` wrap). All day bucketing MUST
be UTC. Year `0` means all-time (a full scan), but the web layer MUST always pass
a concrete year. `JournalStats` MUST honor `journal.exclude_conversations` — the
same denylist `journal_days` was built with — so excluded threads never inflate
the stat tiles.

#### Scenario: Streak counts a month rollover
- **Given** journal days `2026-01-30`, `2026-01-31`, `2026-02-01`
- **When** `JournalStats` computes the longest streak
- **Then** the streak is 3, because adjacency is date arithmetic, not string succession.

#### Scenario: Stats honor the exclude denylist
- **Given** `journal.exclude_conversations` naming a high-volume thread
- **When** the stat tiles are computed for a year
- **Then** the excluded thread's messages are absent from the weekday/peak-hour argmax and from the day/streak counts, matching the journal the day rollups were built with.

### REQ-0016-016: Untrusted structured fields are escaped; no attacker-controlled markup

Every structured-digest field is model-derived and MUST be treated as untrusted
output. All rendered fields (summary, highlights, people, themes, standout media,
notable links) MUST be emitted through `html/template` auto-escaping. Notable
links MUST render as **text only**, never as a raw `href`. Mood tints MUST be
applied as CSS classes keyed by the fixed mood enum (`cal-day--<mood>`), never an
attacker-supplied class or inline style. The page MUST remain compatible with the
site CSP (`style-src 'self'`, no `'unsafe-inline'`): no `style=` attribute may
carry model-derived values.

#### Scenario: A malicious digest field cannot inject markup or a live link
- **Given** a digest whose `summary` contains `<script>` and whose `notable_links` contains a `javascript:` URL
- **When** the day card renders
- **Then** the `<script>` is HTML-escaped as inert text, the link is shown as escaped text with no clickable `href`, no inline `style=` is emitted, and the mood tint is a fixed `cal-day--<mood>` class.
