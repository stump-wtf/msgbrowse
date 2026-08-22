# Design: Unsolicited-contact evidence

## Context

[ADR-0029](../../../adr/0029-unsolicited-contact-evidence.md) decided to port
[jonstump/spam-catcher](https://gitea.stump.rocks/jonstump/spam-catcher)'s
*analysis* layer into msgbrowse as a derivation over the already-imported
archive, and to leave its `chat.db` acquisition layer and its carrier lookup
behind. This design covers how the ruleset is built and versioned, how the scan
walks the archive, how the human/derived boundary is enforced, and how the
dossier is assembled.

The implementation template is `internal/facts`
([ADR-0011](../../../adr/0011-contact-facts-extraction.md)) as reinterpreted by
`internal/sentiment` ([ADR-0028](../../../adr/0028-ipip-sentiment-trait-scoring.md)):
same hash cursor, same generation stamp, same idempotent-upsert contract, same
exclude-before-read posture — minus the LLM, because this derivation has none.

## Goals / Non-Goals

### Goals
- An evidence record that survives re-ingest and can be regenerated to the same
  conclusions, or say clearly that the rules changed.
- A record that is honest about what it cannot establish, in the artifact itself.
- Zero egress, so it is usable on an archive the owner will not send anywhere.
- Human judgments that no automated pass can silently overwrite or destroy.

### Non-Goals
- Evidentiary parity with a `chat.db` reader. UTC offsets, Apple GUIDs and the
  SMS/iMessage distinction are lost at the exporter boundary
  ([ADR-0005](../../../adr/0005-imessage-txt-parser.md)) and this design does not
  recover them; it documents them.
- Carrier lookup, a web surface, MCP tools, group threads, LLM classification
  (all deferred; see spec Scope).

## Decisions

### The ruleset is a value, and its digest is the generation

**Choice**: `spam.NewRules` takes the raw config, fills defaults, normalizes and
de-duplicates every list, and computes `version` — a 12-byte SHA-256 prefix over
a canonical JSON projection with every list sorted. The struct is immutable
afterwards.

**Why**: the version has to move on a rule change and stay still on a re-order,
or a YAML tidy-up triggers a full rescan and a real widening might not. Sorting
inside the digest, not inside the ruleset, keeps first-seen ordering in the
`reasons` output while making the stamp order-insensitive. The recipe is the one
the journal already uses for `prompt_version`.

**Rejected**: a hand-maintained `ruleset_version` constant — it would drift the
first time someone edits a default without bumping it, and the failure is silent.

### Identity: canonical for storage, a looser key for matching

**Choice**: the stored `identifier` is `contacts.Normalize(conversation.name).Value`.
Matching against `my_numbers`, `allowlist` and the address book goes through
`spam.MatchKey`, which reduces a phone to its trailing ten digits, folds an email
to lowercase, and passes handles through — with a digits-only fallback so SMS
short codes (below `NormalizePhone`'s seven-digit floor, so they arrive as
handles) still compare.

**Why**: `contacts.NormalizePhone` deliberately never guesses a country code, so
`+15551234567` and `5551234567` are different canonical values for the same
line. `internal/contacts` already documents trailing-digit comparison as the one
sanctioned widening for phones; this reuses that rule rather than inventing a
second normalization.

### The stranger predicate, and why degraded mode narrows

**Choice**: exclusion order is owner → allowlist → address book (when
`Available`) → shape heuristic (when not). Degraded mode examines only
phone/email-shaped thread names and sets `Summary.Degraded`.

**Why**: spam-catcher's "no Contacts ⇒ everyone is a stranger" rule is safe only
because that tool stores nothing about contacts. msgbrowse already holds the
whole archive, so the same rule would enroll the owner's family. Narrowing fails
in the opposite direction — under-reporting — which is the correct direction for
a record headed to a filing, provided the run says so. Hence the loud summary
line rather than a debug log.

**Consequence worth naming**: the release CLI is `CGO_ENABLED=0` and links no
provider, so degraded is the default. `internal/cli/contactresolver.go` /
`contactresolver_macos.go` is the build-tag seam that lets
`-tags macoscontacts` (and the desktop shell, which already wires it) get the
accurate path.

### `spam_findings` is dense, `message_sentiment` is sparse

**Choice**: every examined message from a stranger gets a row, including
outbound ones and including inbound ones that trip nothing.

**Why**: the twelve-month window math counts *contacts*, not violations, so the
non-matching messages are load-bearing data rather than noise. Outbound rows are
what prove the owner sent STOP. Sentiment could be sparse because "expresses
nothing" is genuinely nothing; "messaged you and tripped nothing" is not.

### `is_after_optout` is owned by one wholesale UPDATE

**Choice**: the batch upsert deliberately does **not** write `is_after_optout`.
`RecomputeSpamAfterOptOut` rewrites it for the whole generation in one statement,
after every scan and after every `spam event add`.

**Why**: an opt-out entered today changes the flag on messages scanned months
ago. If the batch write also touched the column, its value would depend on scan
order — the exact class of bug that produces a quietly wrong number in a filing.
One writer, one moment, whole generation.

The SQL uses `COALESCE(MIN(event_at_unix), 9223372036854775807)` so a sender with
no opt-out compares against a sentinel far future rather than needing a second
query, and only `direction = 'inbound'` is ever set to 1.

### Human columns are protected by the shape of the upsert

**Choice**: `upsertSpamSenderTx` writes only `conversation_name`, the
first/last-seen window, and a `seen → watch` promotion expressed as a `CASE` in
the `DO UPDATE`. `SetSpamSenderFields` writes only the columns whose flags the
user actually passed (`cmd.Flags().Changed`). `ResetSpam` deletes findings,
cursors, and `origin = 'scan'` events only.

**Why**: three independent guards for one invariant — a scan can never destroy a
judgment — because that invariant is the difference between a record someone
trusts and one they re-enter by hand every time.

**Rejected**: splitting the human columns into a separate table. Cleaner in
theory, but it doubles the read joins for a single-writer guarantee that a
`CASE` expression already gives.

### Opt-out matching: exact for keywords, fuzzy prefix for the notice

**Choice**: a stop keyword must equal the *whole* normalized body; the canned
notice matches as a normalized prefix at a configurable ratio (default 0.6),
cut on a rune boundary.

**Why**: "please stop texting me about solar" is a complaint. Treating it as a
formal opt-out would date the violation window from the wrong message, which
over-claims. The notice gets the looser rule because autocorrect rewrites it and
long notices are sent truncated; a prefix that must appear *somewhere* in the
body survives a greeting in front of it. Cutting on runes matters because
`NormalizeForMatch` keeps non-ASCII letters, and a byte slice can land
mid-codepoint and never match anything.

### The dossier renders from one struct

**Choice**: `Dossier` is a plain struct with JSON tags; `.JSON()` marshals it and
`.Markdown()` walks the same value.

**Why**: this is spam-catcher's property and it is worth keeping — two renderers
over two data paths drift, and the drift shows up as a JSON export and a Markdown
export that disagree about a count in front of a lawyer.

`Limitations()` is a package function returning a fixed list, embedded in every
export and asserted by tests. It is maintained as the pipeline changes; a stale
limitations list is worse than none, because it is a claim rather than an
omission.

## Data flow

```
config spam:  ──► spam.NewRules ──► ruleset_version
                                        │
conversations (is_group = 0)            │
  ├─ exclude list ─────────┐            │
  ├─ my_numbers ───────────┤            │
  ├─ allowlist ────────────┼──► stranger predicate
  └─ contacts.Resolver ────┘       │
                                   ▼
        store.GetMessages(cursor) ──► classify / match opt-out
                                   │
                    PutSpamBatch (one transaction)
                      ├─ spam_senders  (derived columns only)
                      ├─ spam_findings (upsert, per generation)
                      ├─ spam_events   (origin = 'scan')
                      └─ spam_state    (hash cursor)
                                   │
              RecomputeSpamAfterOptOut(version)  ◄── spam event add
                                   │
        senders · violations · evidence (Markdown + JSON)
```

## Risks

- **Degraded mode is the default.** Mitigated by a warning that names both
  failure directions, a `Degraded` field on the summary, and the build-tag seam.
  Not mitigated by anything that makes the release binary accurate — that needs
  cgo.
- **The limitations list decays.** Mitigated by tests asserting its key phrases
  and by the ADR naming maintenance as an ongoing obligation.
- **A full rescan on every rule edit.** Acceptable: it is CPU-only, single-pass,
  and bounded by archive size, and the alternative (mixed generations) is a
  correctness bug.
- **Someone treats a dossier as more than it is.** The strongest available
  mitigation is the artifact stating its own limits; the honest framing is that
  this organizes facts and a lawyer decides what they mean.
