# SPEC-0016 Design: AI-editorialized journal

- **Capability:** journal
- **Related ADRs:** [ADR-0023](../../../adr/0023-ai-editorialized-journal.md), [ADR-0011](../../../adr/0011-contact-facts-extraction.md), [ADR-0010](../../../adr/0010-security-privacy-posture.md)
- **Related specs:** [SPEC-0005 (contact facts)](../contact-facts/spec.md)

## Architecture

The journal mirrors the facts pipeline, split into two layers. A dedicated
command drives an orchestrator that (1) rebuilds the mechanical rollup from the
store with no network, then (2) — only when digests are enabled — assembles each
eligible day's transcript, calls the LLM once per day, and caches the prose.
Orchestration lives in `internal/journal`; persistence and the day-keyed caches
live in `internal/store`. The web layer reads both caches and never calls the LLM.

```
internal/cli/journal.go ──▶ journal.Run(store, llm.Client, Options)
                              │
internal/journal/journal.go ──┼─ BuildMechanical (no LLM, no egress):
                              │    per UTC day: counts, per-source counts, top senders
                              │    → store.PutJournalDay (upsert, keyed on day)
                              ├─ if digest_enabled and not --dry-run:
                              │    EnumerateEligibleDays (exclude list applied here)
                              │      → days missing a current (model, prompt_version)
                              │      → capped by max_days_per_run (unless --backfill)
                              │    per day (bounded): AssembleTranscript (exclude list)
                              │      → buildPrompt → llm.Chat (~180s timeout) → body
                              │      → store.PutJournalDigest (day, model, prompt_version)
                              └─ Summary (days built, digested, remaining, tokens est.)
internal/web/journal.go ─────▶ store.JournalDaysPage (keyset) + JournalDigest fallback
```

`prompt_version = sha256hex(lower(trim(effective DigestPrompt)))`, computed once
per run and threaded through enumeration and persistence so eligibility and the
stored key can never drift.

## Key design decisions

### Two layers, one command, independent gating

The mechanical rollup and the digest are separate passes of `journal.Run`. The
mechanical pass reads only `messages` and always runs, so a machine with digests
disabled still gets a complete, deterministic journal at zero egress cost. The
digest pass runs only when `journal.digest_enabled` is true and is the sole
`llm.Chat` egress — exactly the posture `facts` established ([ADR-0011](../../../adr/0011-contact-facts-extraction.md),
[ADR-0010](../../../adr/0010-security-privacy-posture.md)). Splitting them keeps
the always-on layer honest (no accidental network) and the opt-in layer auditable
(one call per day, one endpoint).

### UTC day bucketing is the load-bearing correctness rule

A day is `substr(ts,1,10)`, equivalently `date(ts_unix,'unixepoch')`.
`messages.ts` is a wall-clock string `'YYYY-MM-DD HH:MM:SS'` and `messages.ts_unix`
is that string parsed **as UTC** (`internal/signal/message.go`: "local time with
no zone, parsed as UTC purely for a stable ordering key"). Because both encode the
same already-UTC instant, day derivation is a pure slice with **no** timezone
conversion. Introducing `'localtime'` anywhere would double-shift the value and
push messages near midnight into the wrong day — the single highest-consequence
bug this feature can have. The mechanical rollup and the per-day transcript query
use the identical UTC day expression so counts and digests always agree.

### Day-keyed, FK-less caches (schema v11)

`journal_days(day PRIMARY KEY, message_count, conversation_count, source_counts,
top_senders, updated_at)` caches the mechanical rollup; `source_counts` and
`top_senders` are small JSON blobs read whole. `journal_digests(day PRIMARY KEY,
model, prompt_version, body, structured, mood, updated_at)` caches the digest —
`body` is the plain-text summary (fallback + empty-response guard), `structured`
is the canonical JSON of the editorial object, and `mood` is denormalized out of
that object (both added in Phase 2; see "Structured digest" and "In-place schema
amendment" below). **Neither table has a
foreign key to `messages`**: `ReplaceConversationMessages` deletes and re-inserts a
conversation's message rows on every re-ingest (rowids change, content does not),
so a CASCADE would wipe every derived journal row on each import — the same reason
embeddings (v3) and `contact_facts` (v4) omit the FK. Both tables are day-keyed,
so the migration's `foreign_key_check` passes trivially, and a stale/missing
`journal_days` row is simply re-derived (it is a cache, not a source of truth).

### Digest cache key and invalidation

`journal_digests` stores the `model` and `prompt_version` that produced each
day's `body`. `prompt_version` is `sha256hex(lower(trim(effective DigestPrompt)))`
— the same normalization `factHash` applies in `internal/store/facts.go`, reused
so the two features hash prompt-shaped text identically. Eligibility is "no row,
or a row whose `(model, prompt_version)` differs from the current effective
values." This means editing `journal.digest_prompt` or switching `llm.chat_model`
invalidates cached digests **without** a wipe: the key no longer matches and the
day re-digests on the next run, updating the row in place (`PutJournalDigest`
upserts on `day`). `--regenerate` remains the explicit "wipe and rebuild all"
escape hatch; a day whose key still matches costs zero LLM calls.

### One digest per day

The digest is a single cross-thread daily summary keyed on `day`, matching
`config.DefaultDigestPrompt` ("summarizing one day"). Keying on `day` alone (not
`(day, conversation)`) is deliberate: per-conversation digests are a future
extension, and the day-keyed schema leaves room to add a companion table later
without disturbing this one.

### Privacy boundary

`journal.exclude_conversations` is applied while enumerating days and assembling
each day's transcript, **before** any message body is gathered, so an excluded
thread's content never reaches the transcript or the LLM — the same
before-any-read boundary `FactConversations` enforces ([ADR-0010](../../../adr/0010-security-privacy-posture.md)).
A day remains eligible when non-excluded conversations have messages that day; the
excluded thread simply contributes nothing. The sole egress is `llm.Chat` to
`llm.base_url`.

### CLI semantics, per-run cap, and an honest dry-run

A default run is incremental: rebuild the mechanical rollup, then digest every day
whose cached digest is absent or stale by `(model, prompt_version)`, capped at
`journal.max_days_per_run` (0 = unbounded) for cron and reporting the remaining
eligible-day count. `--since` sets a day floor; `--backfill` runs the same
eligibility across all history ignoring the cap; `--regenerate` wipes digests then
rebuilds. A longer per-day context timeout (~180s vs. the tight facts default)
absorbs large daily transcripts.

`--dry-run` makes **zero** `Chat` calls: it enumerates the eligible days and
estimates input tokens with a local `len(runes)/4` heuristic. It intentionally
prints **no** dollar figure — there is no tokenizer or price table in the codebase
today, and the OpenAI-compatible response's `usage` object is not decoded, so any
cost number would be fabricated. A real estimate is stated future work (it would
need a price-config knob plus `usage` decoding), and the dry-run output says so
rather than implying a precision it does not have.

### Web surface (Phase 1 — superseded by the calendar navigator)

Phase 1 shipped a flat list: `GET /journal` listed days newest-first via
`store.JournalDaysPage` (keyset pagination over `day`), rendering each day's
cached digest when present and falling back to the mechanical rollup (counts +
top senders) otherwise, with an empty state when no days exist. The page read
only the two caches and never triggered an LLM call, and the home page gained a
fourth quick-link icon tile pointing at `/journal`. **Phase 2 replaced the flat
list with the mood-calendar navigator** (see "Mood-calendar navigator" below); the
digest-or-mechanical fallback, empty state, home quick-link tile, and
no-LLM-on-render invariants carry forward unchanged.

## Phase 2 design decisions

### Structured digest with tolerant parsing

Phase 2 turns the digest from a prose paragraph into a JSON object — `summary`,
`people`, `themes`, `mood`, `highlights[{text,time}]`, `standout_media`,
`notable_links` — so the web layer can render an editorial card (timed
highlights, chips, media, links) instead of a wall of text. `config.DefaultDigestPrompt`
demands exactly that object and forbids fences/prose. Parsing is deliberately the
**mirror of `internal/facts`, not stricter**: `extractJSONObject` slices from the
first `{` to the last `}` (tolerating fences or a chatty preamble — the object
twin of facts' `extractJSONArray`), then `parseDigest` **coerces every field
rather than failing on any one** — each list item trimmed with empties dropped,
an unknown `mood` folded to `"neutral"`, a malformed `HH:MM` highlight time
blanked while its text survives. Only a total absence of a JSON object or an empty
`summary` is an error, and that error is handled like facts' `errBadResponse`: the
day is skipped-and-logged (`sum.Skipped++`, no row written), so one
deterministically-bad response cannot wedge the resumable run — the next run
retries the day. The validated object is re-marshaled to a **canonical** JSON
string before storage so the stored `structured` is our shape, not the model's raw
bytes. `Temperature` defaults low (0.2) and `MaxTokens` is raised to 2048 because
a structured object with highlights/media/links is larger than a prose blurb.

### Mood as a fixed enum, coerced and denormalized

`mood` is constrained to `journal.Moods` = {`upbeat`, `neutral`, `quiet`,
`tense`}, kept in sync with the prompt's mood list. `parseDigest` lower-cases and
allowlist-checks it, coercing anything else to `"neutral"`, so prompt/model drift
degrades gracefully instead of failing a day or admitting an arbitrary value. The
coerced mood is **denormalized into its own `journal_digests.mood` column** rather
than left only inside `structured`: the calendar tints up to ~366 day cells per
year from one bounded range query reading a single column, never unmarshaling 366
JSON blobs — the same denormalization rationale as the schemaV7/V8 counts. A fixed
enum is also a security property: it is the only thing the day-cell CSS class is
keyed on (see "Untrusted fields" below), so a cell class can never be
attacker-controlled.

### In-place schema amendment and prompt-version as a free migration

The `structured` and `mood` columns were added by **amending `schemaV11` in
place** rather than introducing a v12 migration. This is safe **only because v11
is still unmerged/unshipped** — no released database is at `user_version = 11`
yet, so there is no deployed table to alter. Because `journal_digests` is a
derived cache (rebuildable from `messages` + the LLM) with no source-of-truth
data, populating the new columns needs neither a schema bump nor `--regenerate`:
editing `DefaultDigestPrompt` to demand the JSON object changes `prompt_version`,
which makes every prose-era row stale by the existing `(model, prompt_version)`
eligibility rule, so those days re-derive in structured form on the next run and
`PutDayDigest` updates the row in place — a **free data migration** riding the
Phase 1 invalidation path. Both columns default `''` so a prose-only row and any
legacy read remain valid. The load-bearing caveat: a database that *already*
reached `user_version = 11` before the amendment will **not** retroactively gain
the columns — `CREATE TABLE IF NOT EXISTS` is a no-op on an existing table, and
there is no `ALTER`. The remedy is to **recreate the derived-cache database** (it
is fully rebuildable), not to hand-`ALTER` the table; once v11 ships this
amend-in-place shortcut is spent and any further column change needs a real v12.

### Mood-calendar navigator

`GET /journal` (Phase 2) is a mood-tinted month-calendar navigator plus an
editorial day card and a row of stat tiles, all driven by **boosted query params**
(`?year`, `?month`, `?day`) so there is no client-side state — every year tab,
month chevron, and day cell is an `<a>` that swaps `#main-content`. Two default
rules keep the landing sensible: the bare `/journal` opens on `LatestJournalDay`
(the newest day's card), and a year tab (`?year` with no month) opens on
`LatestJournalDayInYear` — that year's most recent active month — instead of a
January that would render an empty grid for a year whose activity starts later.
The month grid is laid out in Go (`buildMonthGrid`): a fixed Sun-first 6×7 grid,
present days keyed by day-of-month into tinted, count-subscripted, `?day=`-linked
cells and absent days rendered as inert blanks. The day card parses `structured`
when present, then falls back to the prose `body`, then to the mechanical
top-senders — the same graceful-degradation ladder as Phase 1, now per selected
day rather than per list row.

### Calendar read surface

The navigator is served by narrow store methods that keep the hot paths off the
`messages` table. `JournalMonth` returns ≤31 rows off `journal_days` left-joined
to `journal_digests.mood` (never re-scanning messages — it uses the cached
`message_count`); `GetJournalDay` joins a day's rollup with its digest (`structured`
+ `mood` included); `LatestJournalDay`/`LatestJournalDayInYear` and `JournalYears`
back the defaults and year tabs. `JournalStats` splits its work by cost:
days-with-entries and longest streak come from the `journal_days` **key set in
Go** — `longestStreak` walks the sorted days with adjacency by date arithmetic
(`time.Parse` + `AddDate`), so a month/year rollover (`2026-01-31 → 2026-02-01`)
counts, which naive string succession would miss — while most-active weekday and
peak hour are argmax `GROUP BY` reads over `messages` (`journalArgmax`, `%w`/`%H`
UTC buckets). A year is bounded by a **sargable `ts_unix` BETWEEN range** that can
use `idx_messages_ts_unix`, deliberately *not* `date(ts_unix,'unixepoch')` which
would wrap the column and force a full scan; year `0` (all-time) is an accepted
full scan, but the web layer always passes a concrete year. Crucially,
`JournalStats` resolves and applies `journal.exclude_conversations` (via
`excludedConversationIDs`, the same name-denylist `BuildJournalDays` used), so the
stat tiles reflect exactly the journal the day rollups were built from — an
excluded high-volume thread never inflates the weekday/peak-hour argmax or the day
and streak counts. All bucketing is UTC, consistent with the rest of the feature.

### Untrusted structured fields — escaping and no attacker-controlled markup

Every field of the digest is model output and is treated as untrusted. The whole
card renders through `html/template` auto-escaping, so a `summary` containing
`<script>` is inert text. Notable links render as **plain text, never a raw
`href`** — the archive owner sees the URL but the page never emits a clickable
(or `javascript:`) link built from model output. Mood tints are applied as
`cal-day--<mood>` CSS classes keyed on the fixed `journal.Moods` enum, so the
class is drawn from a closed set and can never be attacker-supplied. This also
respects the site CSP (`style-src 'self'`, no `'unsafe-inline'`, enforced by
`csp_templates_test.go`): the calendar carries no `style=` attribute, tinting is
class-based only, and there is nothing for a malicious digest field to inject.

## Testing

- `internal/store/journal_test.go`: mechanical rollup correctness and UTC day
  bucketing (incl. a near-midnight message staying on its UTC day, and a
  `'localtime'` regression guard), `PutJournalDay` upsert idempotency, digest
  upsert + `(model, prompt_version)` round-trip, eligibility after prompt/model
  change, keyset day paging, re-ingest leaves derived rows intact (no FK cascade).
- `internal/journal/journal_test.go`: end-to-end run with a fake `llm.Client` —
  mechanical build with digests disabled makes no calls; incremental digest skips
  current days (`TestRunCacheHitSkipsSecondRun`); a prompt edit re-digests
  (`TestRunStalePromptReDigests`); `--regenerate` rebuilds; `max_days_per_run`
  caps and reports the remainder; the exclude list keeps a thread's content out of
  the assembled transcript; `--dry-run` makes zero calls and writes nothing. Phase
  2: `TestParseDigest` covers tolerant parsing (fences, empty-item drops, unknown
  mood → neutral, blanked highlight time, no-JSON/empty-summary → error), and
  `TestRunSkipsMalformedDigest` proves a bad response is skipped (no row, day still
  eligible) without wedging the run.
- `internal/store/journal_calendar_test.go` (Phase 2): `TestLongestStreak`
  (including a month/year rollover), `TestJournalMonthYearAndDay` (month grid rows,
  `JournalYears`, `GetJournalDay` with structured/mood, `LatestJournalDay(InYear)`),
  and `TestJournalStats` (streak, days, weekday/peak-hour argmax, exclude denylist
  honored, UTC bucketing).
- `internal/web/journal_test.go`: `/journal` shows the empty state and issues no
  LLM call; Phase 2 — `TestJournalCalendarRendersMonthAndStats` (month grid + stat
  tiles), `TestJournalDayCardStructured` (structured card render with
  digest-or-mechanical fallback), `TestJournalYearTabOpensLatestMonth` (year tab
  opens the latest active month, not January), and `TestJournalDigestFieldsEscaped`
  (untrusted fields HTML-escaped, links as text, no injected markup). The home page
  renders the `/journal` quick-link tile.
