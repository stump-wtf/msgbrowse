---
status: draft
date: 2026-08-22
implements: [ADR-0029]
extends: [SPEC-0028, SPEC-0004]
---

# SPEC-0029: Unsolicited-contact evidence — web surface

- **Capability:** spam-web
- **Target packages:** `internal/web` (new `spam.go`, `spamsettings.go`, new
  `templates/spam.html`, `templates/spam_sender.html`,
  `templates/spam_settings.html`, `templates/spam_runs.html`),
  `internal/config` (`save.go`), `internal/store` (`spam.go`)
- **Related ADRs:** [ADR-0029](../../../adr/0029-unsolicited-contact-evidence.md)
  §7 (which defers this surface and says why),
  [ADR-0006](../../../adr/0006-web-stack-htmx.md),
  [ADR-0010](../../../adr/0010-security-privacy-posture.md),
  [ADR-0012](../../../adr/0012-slate-redesign-design-system.md),
  [ADR-0024](../../../adr/0024-contact-merging-and-address-book-abstraction.md)
- **Related specs:** [SPEC-0028](../spam-evidence/spec.md) — the evidence layer
  this presents, and the source of every invariant repeated here;
  [SPEC-0004](../web-ui/spec.md) — the web surface this extends;
  [SPEC-0006](../web-ui-redesign/spec.md) — the slate design system;
  [SPEC-0026](../backups/spec.md) — the closest precedent for a settings tab
  that writes files with restrictive modes.

## Overview

SPEC-0028 ships the unsolicited-contact evidence layer CLI-first.
[ADR-0029 §7](../../../adr/0029-unsolicited-contact-evidence.md) defers the web
surface deliberately: *"a dossier is a legal-adjacent artifact and its
presentation deserves its own design pass, not a table bolted onto the contacts
page."* This spec is that pass.

It covers four things: the read surface (senders, violations, a dossier
rendered in the browser), the write surface for human judgments (`sender-set`
and `event add` as forms), the operating surface (running and re-running the
scan, and reading why a run failed), and the `spam:` configuration tab — which
is the hard part, because editing configuration here changes `ruleset_version`
and therefore invalidates the evidence record.

**The app is where this feature is operated, not a viewer bolted on beside the
CLI.** Everything needed to keep the record current — start a scan, re-derive
it from the top, see that the last run aborted and why, fix the address-book
permission that degraded it, and edit the ruleset — MUST be reachable from the
UI without dropping to a terminal. The CLI remains complete and remains the
headless and automation path, and REQ-0029-011 requires the two surfaces to
agree; what changes is that neither is a prerequisite for the other.

**This surface is not a convenience skin over the CLI.** On macOS the desktop
shell wires a real address book through `SetContactResolver`
([ADR-0024](../../../adr/0024-contact-merging-and-address-book-abstraction.md));
the released CLI is built `CGO_ENABLED=0` and links no provider, so it runs the
degraded stranger predicate of REQ-0028-006. The web surface running inside the
desktop shell is therefore the *highest-fidelity* way to run a scan on the
platform most archives come from. That is also why this spec depends on
REQ-0028-013: without recorded provenance, a UI that can scan will silently
interleave accurate rows with the CLI's degraded ones.

## Scope

In scope: the `/spam` route tree and its templates; the senders and violations
lists; the in-browser dossier; judgment and event forms; dossier download and
export; the asynchronous scan job, its progress fragment, and the explicit
rescan-from-the-top control; the scan run history and its failure reporting; the
Settings → Spam tab; ruleset-version change preview; the stale-findings and
degraded-mode banners; and `config.SaveSpam`.

Non-goals: any change to the classification ruleset, the schema, or the dossier
*content* — this spec presents SPEC-0028's record and adds nothing to it.
MCP tools ([SPEC-0030](../spam-mcp/spec.md)), carrier lookup, group threads,
attachment evidence and LLM-assisted classification remain out of scope, as in
SPEC-0028.

## Requirements

### REQ-0029-001: Placement and navigation

The surface MUST be reachable from top-level navigation, not nested under
Contacts or Settings. The nav label MUST NOT be "Spam": the record is
legal-adjacent and the label is the first thing that frames what the reader is
looking at. It MUST read **Unsolicited** (or an equally neutral noun phrase).

Route paths MUST use the `/spam` prefix, matching the CLI verb, so that
documentation and the command tree stay searchable against each other.

When the archive has never been scanned, the surface MUST render an explanatory
empty state whose primary action is the in-page scan control, naming
`msgbrowse spam scan` as the headless equivalent rather than as the instruction.
It MUST NOT render an empty table.

#### Scenario: The label does not prejudge the record
- **Given** the application shell
- **When** the navigation is rendered
- **Then** the entry for this surface is not labelled "Spam".

### REQ-0029-002: Senders are addressed by row id, never by identifier

Every route addressing one sender MUST use the `spam_senders` integer primary
key. Routes MUST NOT embed `source` and `identifier` in the path.

Identifiers are phone numbers and email addresses. A path-embedded identifier is
written into the request line of the access log that the Logs page renders
(SPEC-0004), which would put counterparty PII into a surface whose whole purpose
is to be read casually. The integer id costs one lookup and keeps it out.

A request for an unknown or non-numeric id MUST render 404, not an error page
echoing the requested value.

#### Scenario: An identifier never reaches the log surface
- **Given** a sender whose identifier is `+15551110001`
- **When** their dossier page is requested and the Logs page is then rendered
- **Then** no log line contains the identifier.

### REQ-0029-003: Read surface

The following MUST be available:

| Route | Renders |
| --- | --- |
| `GET /spam` | the sender list of REQ-0028-007, filterable by status |
| `GET /spam/violations` | senders with contact after an opt-out (REQ-0028-008) |
| `GET /spam/s/{id}` | one sender's dossier (REQ-0028-011) rendered in-browser |

The sender list MUST default to the `watch` and `tracked` statuses, matching the
CLI default, and MUST offer the remaining statuses as filters rather than hiding
them.

The dossier page MUST render every message verbatim with its timestamp,
direction, tripped rules, extracted fields and body hash, and MUST render a
finding whose message no longer resolves as an explicit gap rather than omitting
it — the same requirement the exported artifact carries.

#### Scenario: An unresolvable message is visible as a gap
- **Given** a finding whose message is absent from the archive
- **When** the dossier page renders
- **Then** the gap is shown explicitly and is not silently skipped.

### REQ-0029-004: The limitations section is not optional in HTML

The in-browser dossier MUST embed the same limitations section the exported
artifact carries (REQ-0028-011), including the degraded-mode and mixed-predicate
statements of REQ-0028-013. It MUST NOT be collapsed behind a disclosure control
that is closed by default.

A test MUST assert its presence in the rendered HTML, mirroring the test that
guards the Markdown artifact. A dossier read in a browser without its
limitations is the same failure mode as a file without them, and the browser is
the surface where a reader is *most* likely to form a conclusion quickly.

#### Scenario: The rendered page states what it cannot establish
- **Given** any sender with findings
- **When** the dossier page is rendered
- **Then** the response body states that timestamps carry no recorded UTC
  offset and that carrier and line type are not established.

### REQ-0029-005: Write surface for human judgments

`POST /spam/s/{id}/judgment` MUST record `status`, `suspected_entity`,
`consent_status`, `consent_notes` and `notes`, and MUST write only the fields
actually submitted — the `sender-set` guarantee of REQ-0028-007, which exists so
a partial form submission cannot blank a judgment the owner entered earlier.

`POST /spam/s/{id}/event` MUST record a manual event (REQ-0028-010) with
`origin = manual`, so it survives `--reset`.

Both MUST reject an unknown `status`, `consent_status` or event type with a
validation error naming the permitted values, rather than silently storing an
unrecognized string.

#### Scenario: A partial submission does not blank a judgment
- **Given** a sender with `notes` set
- **When** a judgment form submits only `status`
- **Then** `notes` is unchanged.

### REQ-0029-006: Export and download

`GET /spam/s/{id}/dossier.md` and `.json` MUST stream the dossier as a download
without writing to disk. `POST /spam/s/{id}/export` MUST write it into
`spam.export_dir` under the mode rules of REQ-0028-011 (0600 files in a 0700
directory) and report the path written.

The export destination MUST come from configuration. A request MUST NOT be able
to name an output path.

#### Scenario: The browser cannot choose a destination
- **Given** an export request carrying a path parameter
- **When** it is handled
- **Then** the parameter is ignored and the file lands under `spam.export_dir`.

### REQ-0029-007: The scan is asynchronous, coalesced, and never implicit

`POST /spam/scan` MUST start a scan in the background and return immediately.
`GET /spam/scan/progress` MUST return a fragment suitable for polling, following
the established job pattern in `internal/web/semanticindex.go`.

A scan request arriving while one is running MUST coalesce into the running job
rather than starting a second writer.

A scan MUST NOT be started as a side effect of any other action — in particular
not by saving configuration (REQ-0029-009). The deliberateness of the scan is
load-bearing: it is what makes a generation correspond to a moment the owner
chose.

The running scan's degraded state MUST be visible while it runs, not only in the
completed summary.

**The incremental scan and the re-derivation MUST be distinct controls.**
`POST /spam/scan` resumes every conversation from its cursor. Re-deriving the
record from the top — `--reset`, the explicit act REQ-0028-013 requires for
making a mixed record uniform — MUST be offered as a separate control, MUST be
confirmed before it runs, and the confirmation MUST name the findings and sender
counts that will be re-derived and state that manually recorded judgments and
events survive it.

Nothing rescans implicitly. A conversation is re-derived from the top only when
the ruleset version changed (REQ-0028-003) or when the owner asked for it here,
never because a different binary ran the previous scan.

#### Scenario: A double submission does not start two writers
- **Given** a running scan
- **When** a second scan is requested
- **Then** the request reports the running job and no second scan begins.

#### Scenario: Re-derivation is asked for, not stumbled into
- **Given** a record whose findings span two scan environments
- **When** the owner starts an ordinary scan
- **Then** existing findings are retained and only new messages are examined;
  the record is re-derived only if the rescan control is used and confirmed.

### REQ-0029-008: Stale and degraded banners

Every page under `/spam` MUST show a banner when any `spam_state` row carries a
`ruleset_version` differing from the digest of the current configuration,
stating that the findings predate a rules change and naming the scan control.

Without it, a configuration edit makes the sender list render zero rows, which
reads as data loss rather than as a pending re-derivation.

Every page under `/spam` MUST likewise show a banner when the current
environment's stranger predicate is degraded (REQ-0028-006), or when the stored
findings span more than one predicate (REQ-0028-013), naming both failure
directions in each case.

The mixed-predicate banner MUST offer the rescan-from-the-top control of
REQ-0029-007. Nothing makes such a record uniform on its own, by design, so the
banner that reports the condition is the only place the remedy is offered; a
banner that states a problem and names no action is a dead end.

The degraded banner MUST distinguish the two causes and state the corresponding
remedy, because they are not the same problem: an address book that reports
`needs-permission` is fixable here and now by granting access and re-running the
scan, while `absent` means this build links no provider at all and no action in
the UI will change it.

#### Scenario: A rules change is legible, not silent
- **Given** a scanned archive and a subsequent configuration change
- **When** the sender list is rendered
- **Then** it states that the findings were derived under a previous ruleset.

#### Scenario: A fixable permission is presented as fixable
- **Given** an address book reporting `needs-permission`
- **When** any page under `/spam` is rendered
- **Then** the degraded banner names granting access and re-running the scan as
  the remedy, and does not present the degradation as inherent to the build.

### REQ-0029-009: Settings → Spam previews the consequence before saving

`GET /settings/spam` MUST render every `config.SpamConfig` field. `POST` MUST
persist them through `config.SaveSpam`.

Before persisting, the handler MUST construct the candidate ruleset from the
submitted values and compare its version against the current one. When they
differ, it MUST NOT save directly. It MUST render a confirmation naming the old
and new version, and the count of findings and senders derived under the old
one, and offer saving and saving-then-rescanning as distinct actions.

Saving MUST NOT trigger a scan on its own (REQ-0029-007).

A ruleset change is a re-derivation of a legal-adjacent record. A user who
widens a watch list to catch one more sender is entitled to know, before
committing, that they are invalidating the generation a previously exported
dossier was drawn from.

#### Scenario: Widening the watch list warns before it invalidates
- **Given** 1,204 findings across 37 senders under ruleset `a1b2c3d4e5f6`
- **When** a configuration change adding an area code is submitted
- **Then** the response names both versions and both counts, and no
  configuration has yet been written.

#### Scenario: A no-op save does not warn
- **Given** a submission that re-orders a list without changing any value
- **When** it is submitted
- **Then** the version is unchanged, no confirmation is shown, and the save
  proceeds.

### REQ-0029-010: Configuration write-back is a surgical merge

`config.SaveSpam` MUST merge the `spam:` block into the YAML configuration file,
round-tripping every unrelated key verbatim, and MUST land the file atomically
at mode 0600 — the contract `SaveLLM` already meets.

The merge and atomic-write machinery SHOULD be factored into a block-agnostic
helper that `SaveLLM` and `SaveBackups` are reimplemented on, rather than
copied a third time. The duplication has already happened: `SaveLLM` and
`SaveBackups` (`internal/config/save.go`) carry character-identical
read/unmarshal/nil-guard blocks today, so `SaveSpam` would be the third copy —
and three independent copies of a surgical-merge routine is how one of them
acquires a bug that truncates a user's configuration.

`spam.export_dir` MUST be validated before it is written: absolute, creatable,
and created 0700. It is the one field on this tab where a web form chooses a
filesystem destination for a file containing verbatim message bodies.

#### Scenario: An unrelated key survives the save
- **Given** a configuration file containing `llm:` and `journal:` blocks
- **When** the Spam tab is saved
- **Then** both blocks round-trip unchanged.

### REQ-0029-011: The surface adds nothing to the record

Handlers MUST read and present SPEC-0028's record. They MUST NOT introduce a
classification path, a second rules digest, or a derived field that the CLI
cannot also produce.

Anything a person can see in the browser MUST be obtainable from the CLI, and
anything they can record MUST be recordable by `sender-set` or `event add`. A
divergence between the two surfaces means the exported artifact and the screen
disagree, which for this record is the one defect worth engineering against.

This cuts both ways now that the app is the operating surface: the run history
of REQ-0029-012 MUST also be readable from the CLI, and the rescan control of
REQ-0029-007 is `spam scan --reset`. The UI adds affordances, not capabilities.

#### Scenario: Both surfaces agree
- **Given** a sender with findings and recorded judgments
- **When** the dossier is exported from the CLI and rendered in the browser
- **Then** the stats, events and messages are the same in both.

### REQ-0029-012: Run history and failures are readable on-screen

`GET /spam/runs` MUST render the `spam_runs` history of REQ-0028-014: for each
run its start time, duration, ruleset version, scan environment, address-book
availability, degraded flag, the counters it completed, and — when it aborted —
its error text. The most recent run's outcome MUST also appear on `GET /spam`
itself, not only on the history page.

A run whose heartbeat has gone stale MUST be rendered as crashed, with the
counters it reached, and MUST NOT be rendered as still in progress. A stuck
progress spinner is the failure this requirement exists to prevent: it reports
the one state that is certainly not true.

A failed scan MUST NOT be reported only as "no results". The distinction between
*we examined the archive and found nothing* and *the scan did not complete* is
the difference between an empty record and an unknown one, and only the first is
safe to read as an answer.

The error text MUST be rendered as text, never interpolated as markup, and MUST
NOT be truncated to a status word — the reason a scan aborted is the actionable
part.

#### Scenario: A failed run is not indistinguishable from an empty archive
- **Given** a scan that aborted before completing
- **When** the sender list is rendered
- **Then** it reports that the last run failed and names the reason, rather than
  presenting the empty list as the result.

#### Scenario: A crashed run does not spin forever
- **Given** an unfinished run row whose heartbeat is stale
- **When** the run history is rendered
- **Then** the run is shown as crashed with the counters it reached.
