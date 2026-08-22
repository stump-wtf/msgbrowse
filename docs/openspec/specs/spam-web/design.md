# Design: Unsolicited-contact evidence — web surface

## Context

[ADR-0029 §7](../../../adr/0029-unsolicited-contact-evidence.md) shipped the
evidence layer CLI-first and said why the web surface was deferred rather than
dropped: *"a dossier is a legal-adjacent artifact and its presentation deserves
its own design pass, not a table bolted onto the contacts page."*
[SPEC-0029](spec.md) is that pass; this document covers how it is built.

The implementation template is the Status page's semantic-index card
([SPEC-0025](../search/spec.md), `internal/web/semanticindex.go`) for the
asynchronous job and its polled progress fragment, and the LLM settings tab
(`internal/web/llmsettings.go`, `internal/config/save.go`) for a settings form
that writes back into the YAML configuration. Both patterns are reused rather
than reinvented; what is genuinely new here is that editing this tab's
configuration *invalidates data*, which neither precedent has to handle.

## Goals / Non-Goals

**Goals.** Present SPEC-0028's record without adding to it. Make the app
sufficient for operating the feature — scanning, re-deriving, diagnosing a
failed run, and editing the rules — without a terminal. Make the consequences of
a configuration change visible before it is committed. Keep the degraded-mode
and stale-findings states legible on every page. Keep counterparty identifiers
out of the request line.

**Non-Goals.** No classification logic in `internal/web`. No second rules
digest. No new dossier content. No write path that the CLI lacks.

## Decisions

### The scan control is the reason this surface exists

The release CLI is built `CGO_ENABLED=0` and links no address-book provider, so
it runs the degraded stranger predicate of REQ-0028-006. The desktop shell wires
`macoscontacts` through `SetContactResolver`. A scan started from the web surface
inside the desktop app therefore uses the *accurate* predicate on the platform
most archives come from, and is the best scan available to the user.

This inverts the usual reading of a web UI as a convenience layer over a CLI,
and it is why REQ-0028-013 (scan-environment provenance) blocks this spec: a
surface that can scan accurately, in a product whose CLI scans degraded, will
interleave the two generations in one table unless each row records which
predicate produced it.

It also sets the scope of this surface. The three things that keep the record
current — running the scan, finding out why a run failed, and editing the rules
— all live here, because the person who owns the record works in the app, and a
feature whose maintenance requires a terminal is a feature that stops being
maintained. The CLI keeps every one of those capabilities (REQ-0029-011); what
the UI removes is the requirement to use it.

### A different `scan_env` resumes; making the record uniform is an explicit act

REQ-0028-013 deliberately does *not* rescan a conversation whose cursor carries
a different `scan_env`, and that asymmetry with `ruleset_version` shapes two
controls here.

The uniqueness key is `(message_hash, ruleset_version)` and the writer upserts,
so re-scanning under a degraded predicate would overwrite the accurate rows the
desktop app wrote — one release-CLI run would silently downgrade the whole
layer. Resuming instead keeps each row correct for the predicate that produced
it, and the record simply spans both, which REQ-0028-011 requires it to say.

So the surface offers two distinct actions where a naive design would offer one:
an ordinary scan that resumes, and a confirmed rescan-from-the-top that
re-derives. And the mixed-predicate banner carries the second one, because
nothing else does: the condition is permanent until someone asks for it to be
resolved, and a banner reporting a state with no offered remedy is a dead end.

### Failures are data, not log lines

`spam.Run` returns an error and the run-time `Summary` dies with the process.
Left there, the UI can only ever show "no results", which is the same rendering
whether the archive is clean or the scan aborted on its first batch — and those
two mean opposite things about the record.

REQ-0028-014's `spam_runs` table fixes it the way `embed_runs` already does for
indexing: insert on start, heartbeat per batch, terminal write with the counters
or the error text. The heartbeat is what distinguishes a crashed run from a live
one across process boundaries, which matters because `msgbrowse spam` and
`msgbrowse serve` are separate processes over the same SQLite file — exactly the
situation `embed_runs` was built for.

### Sender routes key on the row id

`spam_senders.id`, not `(source, identifier)`. Identifiers are phone numbers and
email addresses; a path-embedded identifier lands in the access log that the Logs
page renders, putting counterparty PII into a surface meant to be read casually.
The integer id costs one indexed lookup.

This is a small decision with an asymmetric payoff: the cost is a `WHERE id = ?`,
and the failure it prevents is a class of privacy leak that would be discovered
long after the logs had been written.

### Configuration save is a two-phase interaction, not a form POST

`spam.NewRules` is pure and cheap, so the handler can build the candidate ruleset
from the submitted values and compute its version *before* touching the file.
When the version is unchanged — the common case, including a pure list re-order,
which REQ-0028-003 requires not to change it — the save proceeds silently.

When it differs, the handler renders a confirmation naming both versions and the
counts derived under the old one, and offers save and save-then-rescan as
distinct actions. It never scans on its own.

The alternative, saving immediately and showing a stale banner afterwards, was
rejected. It is technically equivalent and experientially wrong: the user learns
they invalidated an evidence generation *after* doing it, and the dossier they
exported last week now belongs to a generation the UI no longer shows.

### Two banners, computed on every page

- **Stale** — any `spam_state.ruleset_version` differs from the current digest.
- **Degraded / mixed** — the current environment's predicate is degraded, or the
  stored findings span more than one predicate.

Both are cheap aggregate queries and both are rendered by the shared layout
rather than per-handler, because the failure mode is a page that omits one.
Without the stale banner, a configuration change makes the sender list render
zero rows, which reads as data loss.

### Two new store queries

- `SpamVersions(ctx) ([]{version, scan_env, findings, senders, last_scan})` —
  powers the settings preview and both banners. One row per
  `(ruleset_version, scan_env)` pair, so "the findings span more than one
  predicate" is a row count, not a scan.
- `SpamRuns(ctx, limit)` over REQ-0028-014's `spam_runs`, with the same
  stale-heartbeat interpretation `embedRunHistory` applies to `embed_runs`. It
  serves the run-history page and the last-run line on `/spam`.

Everything else on this surface is served by the queries SPEC-0028 already
added.

### `SaveSpam` factors the merge out of `SaveLLM`

`config.SaveLLM` hardcodes its four keys around a surgical YAML merge and an
atomic 0600 write. `SaveSpam` needs the identical machinery for a different
block, so the merge and write are factored into a block-agnostic helper and
`SaveLLM` is reimplemented on it. Two independent copies of a routine that
rewrites the user's configuration file is how one of them acquires a truncation
bug.

## Data flow

```
GET  /spam                 → ListSpamSenders + SpamCounts + SpamVersions → list + banners
GET  /spam/s/{id}          → GetSpamSender → BuildDossier → HTML (limitations required)
POST /spam/s/{id}/judgment → SetSpamSenderFields (only submitted fields)
POST /spam/s/{id}/event    → AddSpamEvent (origin = manual)
POST /spam/s/{id}/export   → BuildDossier → 0600 file in 0700 spam.export_dir
POST /spam/scan            → coalesce-or-start background spam.Run (resume)
POST /spam/scan/reset      → confirmed re-derivation (spam.Run with Reset)
GET  /spam/scan/progress   → polled fragment (job state + degraded flag)
GET  /spam/runs            → SpamRuns → history incl. error text; stale
                             heartbeat renders as crashed, never as running
GET  /settings/spam        → render config.SpamConfig
POST /settings/spam        → NewRules(candidate).Version() ?= current
                             ├─ same    → SaveSpam
                             └─ differs → confirmation (nothing written yet)
```

## Risks / Trade-offs

**A scan started from the browser can run for a long time.** Mitigated by the
existing job pattern: the request returns immediately, progress is polled, and a
concurrent request coalesces rather than starting a second writer.

**`export_dir` is a filesystem path chosen through a web form.** It is validated
absolute and created 0700, and the export route ignores any path supplied by the
request. This is the sharpest edge on the tab, because the files it governs
contain verbatim message bodies.

**The HTML dossier could drift from the exported one.** Both render from the same
`spam.Dossier` value, and REQ-0029-011 requires the surfaces to agree. The
limitations block is separately test-asserted in HTML, mirroring the Markdown
test, because a browser is where a reader is most likely to form a conclusion
quickly and least likely to scroll for caveats.

## Testing

- The nav label is not "Spam" (REQ-0029-001).
- A dossier page request does not put the identifier in a log line
  (REQ-0029-002).
- The rendered HTML contains the limitations section, not collapsed
  (REQ-0029-004) — the counterpart of the existing Markdown assertion.
- A judgment form submitting one field leaves the others intact
  (REQ-0029-005).
- An export request carrying a path parameter writes under `spam.export_dir`
  anyway (REQ-0029-006).
- A second scan request during a running scan does not start a second writer
  (REQ-0029-007).
- An ordinary scan over a record whose cursors carry a different `scan_env`
  resumes and retains the existing rows; only the reset control re-derives
  (REQ-0029-007).
- The mixed-predicate banner renders the rescan control, and the
  `needs-permission` degraded banner names granting access rather than the build
  (REQ-0029-008).
- An aborted run renders its error text on both `/spam` and `/spam/runs`, and an
  unfinished run with a stale heartbeat renders as crashed (REQ-0029-012).
- A configuration change that bumps the version renders the confirmation and
  writes nothing; a list re-order saves silently (REQ-0029-009).
- `SaveSpam` round-trips unrelated blocks and lands 0600 (REQ-0029-010).
