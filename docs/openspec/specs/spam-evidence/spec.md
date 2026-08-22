---
status: draft
date: 2026-08-22
implements: [ADR-0029]
extends: [SPEC-0018]
---

# SPEC-0028: Unsolicited-contact evidence

- **Capability:** spam-evidence
- **Target packages:** `internal/spam` (new), `internal/store`
  (`schema.go`, new `spam.go`), `internal/cli` (new `spam.go`,
  `contactresolver.go`, `contactresolver_macos.go`), `internal/config`
- **Related ADRs:** [ADR-0029](../../../adr/0029-unsolicited-contact-evidence.md),
  [ADR-0011](../../../adr/0011-contact-facts-extraction.md),
  [ADR-0024](../../../adr/0024-contact-merging-and-address-book-abstraction.md),
  [ADR-0010](../../../adr/0010-security-privacy-posture.md),
  [ADR-0005](../../../adr/0005-imessage-txt-parser.md)
- **Related specs:** [SPEC-0027](../sentiment/spec.md) — the sibling derivation
  whose generation-stamping and cursor design this mirrors;
  [SPEC-0018](../contact-merge/spec.md) — owns the address-book resolver this
  spec consumes.
- **Upstream:** [jonstump/spam-catcher](https://gitea.stump.rocks/jonstump/spam-catcher)

## Overview

msgbrowse derives an evidence record of unsolicited contact from the archive it
has already imported: which strangers messaged the owner, which configured rules
each message tripped, when the owner told them to stop, what arrived afterwards,
and how many contacts fall inside any twelve-month window. The record is
exportable per sender as a dossier in Markdown and JSON.

Scanning is a deliberate `msgbrowse spam scan` command. It is local,
deterministic and regex-based, and performs **no network egress at all** — no
LLM call, no carrier lookup. Findings are stored hash-keyed, FK-less, and
stamped with a ruleset version so a configuration change re-derives rather than
silently mixing generations.

It reads and reports. It never sends a message, never replies to a sender, and
never files anything with a carrier, the FCC or the FTC. `spam event add`
records that the owner did those by hand, with the confirmation number.

## Scope

In scope: the classification ruleset and its version stamp, the schema migration
(`spam_findings`, `spam_senders`, `spam_events`, `spam_state`, `spam_runs`),
the stranger predicate and its degraded mode, opt-out detection and wholesale
recomputation, the rolling twelve-month window math, the dossier and its
limitations section, the durable scan-run log, and the `msgbrowse spam` command
tree.

Non-goals for this spec: carrier / line-type lookup (declined in ADR-0029 §3 —
a follow-up spec may add it opt-in), a web UI surface
([SPEC-0029](../spam-web/spec.md)), MCP tools
([SPEC-0030](../spam-mcp/spec.md)), reading `chat.db` directly, group-thread
scanning, attachment evidence, and LLM-assisted classification.

## Requirements

### REQ-0028-001: Deterministic, local, zero-egress classification

Classification MUST be deterministic and local: regex and keyword matching over
message bodies and sender identifiers. The scan MUST perform no network I/O —
no call to `llm.base_url`, no carrier lookup, no telemetry. Re-running a scan
over an unchanged archive with an unchanged ruleset MUST produce identical
findings.

#### Scenario: A scan makes no network calls
- **Given** an archive and a configured ruleset
- **When** `msgbrowse spam scan` runs
- **Then** no HTTP request is issued by the scan, and the command completes with
  the LLM endpoint unreachable.

### REQ-0028-002: Rule set and the reasons a message trips

A message from a stranger MUST be evaluated against these rules, each firing an
identifiable reason string:

| Rule | Reason |
| --- | --- |
| Sender's NANP area code is on `spam.watch_area_codes` | `area_code:<code>` |
| Body uses one of `spam.name_variants`, word-boundary and case-insensitive | `name_variant:<names>` |
| Body links to a domain on the shortener denylist | `shortener:<domains>` |
| Body contains any URL and `spam.flag_any_url` is true, and no shortener fired | `url` |

A message MUST additionally have its URLs, callback phone numbers (normalized to
E.164), email addresses, matched name variants, and entity keyword/domain hits
extracted into structured fields. Extraction MUST be additive: the stored
message body is never modified, normalized, or trimmed.

A message that trips no rule MUST still be recorded, with `is_candidate = 0` —
the rolling-window counts are counts of contact, not counts of violation.

`entities_matched` MUST be presented everywhere as a lead to confirm by hand,
never as an attribution.

#### Scenario: A shortener supersedes the bare-URL rule
- **Given** `flag_any_url: true` and a body containing `https://bit.ly/x`
- **When** the message is classified
- **Then** the reasons are exactly `["shortener:bit.ly"]`, not `["url"]` as well.

#### Scenario: A name variant does not fire on a longer name
- **Given** `name_variants: ["Jon"]` and a body reading "Hi Jonathan"
- **When** the message is classified
- **Then** no `name_variant` reason fires.

### REQ-0028-003: Ruleset version and generation partitioning

Every finding MUST be stamped with a `ruleset_version`: a stable digest of the
effective `spam:` configuration (identity lists, area codes, name variants, the
URL rule, shortener domains, entity keywords, stop keywords, the canned notice
and its match ratio, and the conversation exclusion list). Re-ordering any list
MUST NOT change the version; changing any value MUST change it.

`spam.exclude_conversations` MUST participate in the digest. It decides which
conversations enter the record at all, so a change to it changes what a
generation means; because the cursor is per-conversation, leaving it out would
let an added exclusion strand already-written findings, and a removed one add
findings later, both under an unchanged version.

A conversation whose stored cursor carries a different ruleset version MUST be
rescanned from the top. Reads MUST filter to one generation; findings from one
generation MUST NOT be visible to a query for another.

#### Scenario: Widening the watch list re-derives
- **Given** a scanned archive and a config edit adding an area code
- **When** the scan is re-run
- **Then** the ruleset version differs, the previous generation's findings are
  not returned for the new version, and every conversation is re-examined.

### REQ-0028-004: Schema — hash-keyed, FK-less, re-ingest-safe

A schema migration MUST add `spam_findings`, `spam_senders`, `spam_events`,
`spam_state` and `spam_runs` as specified in ADR-0029 §2. `spam_findings`,
`spam_senders` and `spam_events` MUST have **no** foreign key to `messages`, so
a re-ingest that rewrites message rowids leaves the evidence record intact and
still joinable by content hash. `spam_state` MAY cascade from `conversations`.

The `scan_env` columns (REQ-0028-013) and `spam_runs` (REQ-0028-014) MUST land
in a **new** migration version, not by editing v18. v18 has shipped; migrations
are append-only and `scripts/check-migrations.sh` fails the build on an edit to
one (#217). An `ALTER TABLE ... ADD COLUMN` is what gives an existing install
`scan_env`, because the `CREATE TABLE IF NOT EXISTS` in v18 is inert against a
table that already exists.

Writes MUST be idempotent upserts. A batch's findings, detected events, sender
window and cursor advance MUST occur in one transaction, so a crash cannot leave
the cursor ahead of the evidence it covers. All queries MUST be parameterized.

#### Scenario: A re-ingest does not destroy findings
- **Given** a scanned conversation
- **When** the conversation is re-imported, rewriting its message rowids
- **Then** the findings still resolve to their messages by hash.

### REQ-0028-005: Incremental, resumable scanning

The scan MUST maintain a per-conversation cursor anchored on a message content
hash, resolved back to a `(ts_unix, id)` keyset position at run time. A cursor
whose message no longer exists MUST restart that conversation from the top
(safe, because writes are idempotent). The cursor MUST be anchored on the last
non-system message in a batch.

Re-running a scan with no new messages MUST examine zero messages and change
nothing.

#### Scenario: A second scan is a no-op
- **Given** a fully scanned archive
- **When** the scan is re-run under the same ruleset
- **Then** zero messages are examined and no finding is duplicated.

### REQ-0028-006: The stranger predicate, and an honest degraded mode

A conversation MUST be excluded from the evidence layer when its identifier is
on `spam.my_numbers`, is on `spam.allowlist`, or — when the `contacts.Resolver`
reports `Available` — resolves to a person in the address book. Group
conversations and conversations named on `spam.exclude_conversations` MUST be
excluded before any body is read.

When the resolver reports `Absent` or `NeedsPermission`, the scan MUST NOT treat
every counterparty as a stranger. It MUST instead narrow to conversations whose
name is a bare phone number or email address, MUST report `Degraded = true` and
the resolver's availability in the run summary, and the CLI MUST print a warning
naming both failure directions (a known person named by a bare number is
misfiled; a stranger named by a person's name is invisible).

The predicate MUST be recorded, not merely reported. Every finding and every
cursor MUST carry the resolver identity and degraded flag under which it was
written (REQ-0028-013), because the same binary on the same archive answers
"is this person a stranger?" differently depending on whether an address book
was readable, and the run summary is gone once the run ends.

#### Scenario: A known contact is not enrolled
- **Given** a readable address book containing the sender
- **When** the scan runs
- **Then** no `spam_senders` row exists for that identifier and the run reports
  it as skipped-in-contacts.

#### Scenario: Degraded mode narrows rather than falling open
- **Given** no readable address book, a thread named `+15551110001` and a thread
  named `Mom`
- **When** the scan runs
- **Then** only the phone-shaped thread is examined, and the summary reports
  degraded with the count of threads skipped for not being handle-shaped.

### REQ-0028-007: Sender ladder, consent, and the human/derived boundary

`spam_senders.status` MUST be one of `seen`, `watch`, `tracked`, `ignored`. A
scan MAY promote `seen` → `watch` when a rule fires; it MUST NOT overwrite any
status set by a person, and MUST NOT write `suspected_entity`,
`consent_status`, `consent_notes` or `notes`.

`consent_status` MUST default to `no_consent_on_record` and be one of
`no_consent_on_record`, `consent_given`, `consent_revoked`, `disputed`.

`spam scan --reset` MUST clear findings, cursors and scan-detected events, and
MUST preserve every human judgment and every manually recorded event.

#### Scenario: A reset keeps what cannot be re-derived
- **Given** a sender marked `ignored` with notes, and a manually filed FCC event
- **When** `spam scan --reset` runs
- **Then** the status, notes and filed event survive, and the findings are gone.

### REQ-0028-008: Opt-out detection and wholesale recomputation

An outbound message MUST be recorded as `stop_sent` when its normalized body
equals a configured stop keyword **in full**, and as `notice_sent` when it
contains the configured canned notice's normalized prefix at the configured
ratio. A body that merely contains the word "stop" MUST NOT register as an
opt-out.

`is_after_optout` MUST be recomputed across the entire generation after every
scan and after every manually recorded opt-out — never incrementally. The
threshold MUST be the sender's **earliest** opt-out. Only inbound messages MUST
ever be flagged.

#### Scenario: A backfilled opt-out re-flags older messages
- **Given** a scanned sender with messages in January and June and no opt-out
- **When** a `notice_sent` event dated March is recorded
- **Then** only the June message is flagged as after the opt-out.

### REQ-0028-009: Rolling twelve-month window

Reports MUST provide both the trailing-twelve-month inbound count and the
largest inbound count inside **any** twelve-month window, with that window's
start and end dates. The two MUST be reported as distinct numbers, because the
DNC private right of action turns on the latter.

#### Scenario: An old burst is still the worst window
- **Given** four inbound messages in early 2022 and one in 2025
- **When** the stats are computed as of late 2025
- **Then** the trailing-twelve-month count is 1 and the worst window is 4.

### REQ-0028-010: Manual event record

`spam event add` MUST record an event of type `stop_sent`, `notice_sent`,
`reported_7726`, `reported_apple_junk`, `fcc_complaint_filed`,
`ftc_dnc_complaint_filed` or `lawyer_referral`, with free-text details for the
confirmation number and an `--at` that defaults to now — an event is dated when
it *happened*, not when it was typed. Duplicate inserts MUST be ignored.
Events MUST record whether they were detected by a scan or entered by hand.

msgbrowse MUST NOT send any message, reply to any sender, or submit anything to
a carrier, the FCC or the FTC.

### REQ-0028-011: Dossier — one struct, two formats, and its own limitations

`spam evidence` MUST export a per-sender dossier containing: the sender and the
judgments recorded about them; the summary stats of REQ-0028-009; every recorded
event; and every message verbatim with its timestamp, direction, the rules it
tripped, the extracted fields, and a SHA-256 of the body text.

Markdown and JSON MUST render from the same value so they cannot disagree. A
finding whose message no longer resolves MUST be rendered as an explicit gap,
not dropped.

Every dossier MUST embed a limitations section stating at minimum: that
timestamps carry **no recorded UTC offset**; that the text comes from an
exporter rather than `chat.db`; that carrier and line type are **not
established**; that Apple's GUID/ROWID are not preserved; that entity matching
is a lead and not attribution; that `suspected_entity` and `consent_status` are
human judgments; that group threads and attachments are excluded; and that
nothing in it is legal advice.

When any finding in the dossier was written under a degraded stranger predicate,
the limitations section MUST say so and name both failure directions. When the
dossier draws on findings written under more than one predicate, it MUST state
that too — a record assembled half from an accurate scan and half from a
degraded one is not uniform, and presenting it as uniform is the precise failure
this section exists to prevent.

Dossiers MUST be written 0600 into a 0700 directory (`spam.export_dir`, default
`<data_dir>/spam-exports`).

#### Scenario: The timezone gap is in the artifact, not only the docs
- **Given** any exported dossier
- **When** the Markdown is rendered
- **Then** it states that the timestamps carry no recorded UTC offset, and that
  carrier and line type are not established.

### REQ-0028-012: CLI surface

The binary MUST expose `msgbrowse spam` with subcommands `scan`, `senders`,
`violations`, `evidence`, `sender-set`, and `event` (`add` / `list`).
`senders` MUST default to the `watch` and `tracked` statuses and offer `--json`.
`scan` MUST offer `--reset`, `--conversation` and `--batch-size`. A
`--conversation` naming an ineligible id MUST error rather than report a clean
zero-result run.

Help text MUST state that the command reads and reports only, that it performs
no network egress, and that nothing in its output is legal advice.

### REQ-0028-013: Scan-environment provenance

`spam_findings` and `spam_state` MUST each carry a `scan_env` value recording
the address-book resolver identity and the degraded flag in force when the row
was written. Reads that assemble a dossier MUST surface it (REQ-0028-011).

`scan_env` MUST NOT participate in `ruleset_version`. A degraded scan runs the
same rules over a weaker input; folding resolver availability into the version
would re-derive the whole layer whenever the user switched between the desktop
shell and the release CLI, which is a routine act and not a policy change.

A cursor whose `scan_env` differs from the current one MUST NOT, on that basis
alone, force a rescan. The conversation MUST resume from its cursor; rows
written by the resumed scan carry the current `scan_env`, and the cursor's
`scan_env` becomes that of the run that last extended it. A record may
therefore span predicates, which is why REQ-0028-011 requires that it be
disclosed.

This is the one place `scan_env` and `ruleset_version` behave differently, and
deliberately. A changed ruleset makes existing rows *mean* something different,
so they must be re-derived. A changed environment makes them *differently
sourced* — a fact about them, not a defect in them.

Re-deriving on an environment change would also destroy data. The uniqueness
key is `(message_hash, ruleset_version)` and the writer upserts, so a rescan
under a degraded predicate overwrites the accurate row the desktop app wrote,
in place and unrecoverably. One run of the release CLI would silently downgrade
the whole layer and leave nothing recording that it had.

Making the record uniform MUST remain available as an explicit act — `--reset`,
or the rescan control of REQ-0029-007 — and MUST NOT happen as a side effect of
which binary was run.

#### Scenario: A desktop scan and a CLI scan are distinguishable
- **Given** an archive scanned by the desktop shell with Contacts readable, and
  then by the release CLI with no address-book provider
- **When** a dossier is exported
- **Then** the findings record which predicate produced each row, and the
  limitations section states that the record spans both.

#### Scenario: A degraded run does not overwrite an accurate one
- **Given** a conversation whose findings were written under an accurate
  predicate
- **When** the scan is re-run with no readable address book
- **Then** the conversation resumes from its cursor, the existing rows keep
  their `scan_env`, and only newly examined messages are stamped degraded.

#### Scenario: The environment does not re-derive the ruleset
- **Given** two scans differing only in address-book availability
- **When** their findings are compared
- **Then** both carry the same `ruleset_version` and differing `scan_env`.

### REQ-0028-014: Every scan run is durably logged, including its failures

Each scan MUST write a `spam_runs` row: inserted when the run starts, heartbeat
its counters while it works, and stamped on termination with the duration and
final totals, or with the error text when it aborted. The row MUST record the
`ruleset_version`, the `scan_env`, the address-book availability and the
degraded flag under which the run executed.

An unfinished row whose heartbeat has gone stale MUST be reported as a crashed
run, not as one still in progress — the same treatment `embed_runs` gives an
indexing process that died before its terminal write.

The run-time `Summary` evaporates with the process that produced it. Without a
durable log, a scan that aborted halfway is indistinguishable from one that was
never started, the partial record it left behind has no explanation attached,
and the only account of the failure is a line in a terminal the user did not
keep. This requirement is what allows REQ-0029-012 to answer "what went wrong"
on-screen rather than in a log file.

#### Scenario: A failed scan is legible afterwards
- **Given** a scan that aborts because the address book cannot be read
- **When** the run log is read after the process exits
- **Then** the row is terminal, carries the error text, and records the counts
  completed before the abort.

#### Scenario: A crashed scan is not reported as running
- **Given** an unfinished run row whose heartbeat is stale
- **When** the run history is read
- **Then** the run is reported as crashed rather than in progress.
