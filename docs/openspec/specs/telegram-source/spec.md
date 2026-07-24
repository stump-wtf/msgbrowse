# SPEC-0015: Telegram source

- **Status:** Accepted
- **Date:** 2026-07-06
- **Capability:** telegram-source
- **Source packages:** `internal/telegram` (new — parser of exporter output), `internal/toolchain` (bundled `tdl`), `internal/source`, `internal/store` (allowed-source migration), `internal/archivepath`, `internal/ingest`, `internal/setup` (detection + authorization + guidance), `internal/onboard`/`internal/onboardsvc` (export/adopt/refresh), `internal/cli` (`export.go`, `import.go`, `doctor.go`, `sync.go`), `internal/config`, `internal/web` (Providers card, source styling), `internal/devsync` (folder provisioning), `.github/workflows/desktop.yml` (bundle + pin), `docs-site`
- **Related ADRs:** [ADR-0022 (Telegram via delegated exporter)](../../../adr/0022-telegram-source-delegated-exporter.md), [ADR-0020 (bundled exporters + guided setup)](../../../adr/0020-bundled-exporters-guided-setup.md), [ADR-0016 (WhatsApp source)](../../../adr/0016-whatsapp-source-exporter.md), [ADR-0003 (dual-source archive)](../../../adr/0003-dual-source-archive.md), [ADR-0010 (security & privacy posture)](../../../adr/0010-security-privacy-posture.md)
- **Related specs:** [SPEC-0013 (desktop guided setup)](../desktop-onboarding/spec.md), [SPEC-0001 (archive ingestion)](../ingestion/spec.md), [SPEC-0014 (Syncthing device sync)](../device-sync-syncthing/spec.md)

## Overview

Telegram becomes the fourth message source. Per ADR-0022, extraction is
delegated to `tdl` (iyear/tdl) — a dedicated, actively maintained Telegram
exporter bundled into the toolchain like the three existing exporters.
msgbrowse never implements provider extraction itself: it detects Telegram
Desktop, guides a one-time exporter authorization, runs the exporter on
Enable/Refresh (with genuinely incremental time-window exports), and ingests
the exporter's JSON output through the existing pipeline.

## Requirements

### Requirement: Exporter delegation (house invariant)

msgbrowse MUST NOT implement Telegram data extraction. All provider
extraction SHALL be performed by the bundled `tdl` exporter, invoked with
explicit argv (never a shell) through the ADR-0020 toolchain resolver, which
MUST NOT fall back to `$PATH`. The bundled `tdl` release MUST be version- and
sha256-pinned in CI. msgbrowse's own code SHALL be limited to invoking the
exporter and parsing the files it produces. This restates the project-wide
idiom: new sources are added by delegating to a provider-targeted exporter,
never by teaching msgbrowse a provider's raw formats or protocols.

#### Scenario: No extraction code in msgbrowse

- **WHEN** the telegram source is enabled and refreshed end to end
- **THEN** every byte of Telegram data entering the staging root was written
  by the bundled exporter subprocess, and msgbrowse only reads files from the
  staging/managed root.

#### Scenario: Exporter missing from bundle

- **WHEN** the bundled `tdl` binary cannot be resolved from the toolchain
- **THEN** Enable/Refresh fail with a sentinel-classified error naming the
  bundle problem (never a `$PATH` fallback), and doctor reports it with a
  hint.

### Requirement: Source registration

The system SHALL register `telegram` as a first-class source: a
`source.Telegram` constant with the persisted literal `telegram`, membership
in `source.All`, a human label `Telegram`, and acceptance by the store's
allowed-source validation (shipped as a versioned migration). The literal
MUST never be renamed once persisted.

#### Scenario: Unified store accepts the source

- **WHEN** a conversation, message, contact identifier, or ingest run is
  written with `source = "telegram"`
- **THEN** the store accepts it, and every source-enumerating surface (search
  filters, gallery source filter, Providers, doctor, MCP tools) includes
  Telegram alongside the existing three sources.

### Requirement: One-time exporter authorization

The exporter owns its Telegram session; msgbrowse MUST NOT read, store, or
log it. Authorization SHALL be a one-time guided step per the SPEC-0013
guidance grammar. When Telegram Desktop is detected, the primary path SHALL
be the exporter's non-interactive session import from the installed client;
where that fails (client passcode set, no desktop client), the card SHALL
present honest guided fallback instructions for the exporter's interactive
login. A non-destructive authorization probe SHALL gate the Enable
affordance: one-click Enable appears only when the exporter is authorized.

#### Scenario: Desktop session import

- **WHEN** Telegram Desktop is detected and the user clicks Authorize
- **THEN** msgbrowse runs the exporter's session-import login with explicit
  argv, the session lands in the exporter's own data directory, no session
  material appears in msgbrowse config, database, or logs, and the card
  advances to the enable-ready state.

#### Scenario: Authorization-shaped failure

- **WHEN** an export fails because the session is missing, expired, or
  revoked
- **THEN** the failure is sentinel-classified as authorization-shaped and the
  card re-enters the authorization guidance state (mirroring the
  permission-guidance pattern of #181), with raw exporter output available in
  Settings → Logs.

### Requirement: Export invocation and staging

Enable SHALL run the bundled exporter to enumerate chats and export each
chat's full history plus referenced media into the staging root, then
atomically adopt staging into the enum-gated managed root
(`<data_dir>/archives/telegram`). Refresh SHALL export incrementally using
the exporter's time-window filter anchored to the last successful import,
merging into the managed root before re-import. Both SHALL report per-chat
progress and capture bounded exporter output (the existing ring buffer) into
the job logs. Exporter invocations MUST use explicit argv with values from
the fixed enum, server-side probes, or msgbrowse-owned state — never from
HTTP input.

#### Scenario: First enable on a large account

- **WHEN** Enable runs against an account with hundreds of chats
- **THEN** chats export sequentially with progress (n of m chats), provider
  rate limiting (flood-wait) is absorbed by the exporter's backoff without
  failing the job, and a mid-run cancellation leaves the managed root
  unchanged (staging discarded).

#### Scenario: Incremental refresh

- **WHEN** Refresh runs after a prior successful import
- **THEN** the exporter is invoked with a time window from the last import
  anchor, only new activity is exported, and re-import stores only genuinely
  new messages.

### Requirement: Exporter-output parsing

The system SHALL parse the bundled exporter's JSON output from the managed
root. Parsing MUST be streaming (token-level decode) with bounded memory —
account-wide exports reach gigabytes. Message text SHALL be stored flattened
with links extracted; service/system events represented in the output SHALL
map to system messages; reactions present in the output SHALL map to
msgbrowse's reaction model (badges, not rows); reply/forward metadata SHALL
be preserved as metadata. Unknown JSON fields MUST be ignored; malformed
chats or messages MUST be logged and skipped, never fatal. The parser targets
the JSON schema of the pinned exporter version, captured in versioned
synthetic fixtures.

#### Scenario: Malformed entry

- **WHEN** one chat's export file contains an entry missing required fields
- **THEN** that entry is logged and skipped and the remaining messages import
  normally.

#### Scenario: Schema drift on exporter upgrade

- **WHEN** the pinned exporter version is bumped and a field is renamed
- **THEN** fixture tests against the new pin surface the drift at CI time
  (tolerant decode keeps old fields optional), never at user runtime as a
  crash.

### Requirement: Idempotent incremental re-import

Telegram imports SHALL flow through the existing ingest invariants: messages
keyed by a stable content hash with a sequence disambiguator, re-import of
unchanged exports a no-op, per-conversation atomic replacement in
transactions. Overlapping exports (a full export plus a later time-window
export covering some of the same span) MUST NOT produce duplicates.

#### Scenario: Overlapping windows

- **WHEN** a refresh window overlaps messages already imported
- **THEN** re-import stores only the genuinely new messages.

### Requirement: Media and attachments

Media the exporter downloads SHALL resolve through the shared
traversal-safe archive-path layer against the managed telegram root. Missing
media MUST render as placeholders, not errors. Image media SHALL participate
in the gallery and the existing transcode pipeline.

#### Scenario: Traversal attempt in a crafted export file

- **WHEN** a crafted export references `../../etc/passwd` as a media path
- **THEN** resolution is refused by the archive-path containment layer and
  the message imports without an attachment.

### Requirement: Detection and guided onboarding

Setup detection SHALL report Telegram as Detected when Telegram Desktop is
present for the current user (application bundle or its user data
directory), and SHALL report the exporter authorization state via the
non-destructive probe. The Providers card SHALL present: NotDetected (quiet),
Detected-unauthorized (Authorize affordance + guidance), Authorized
(one-click Enable), and Enabled (counts, Refresh, Disable) — following the
SPEC-0013 card grammar, with progress and logs identical to other sources.
Guidance copy SHALL be honest about limitations: secret chats are not
exported, and the one-time authorization uses the user's own Telegram
session via the exporter.

#### Scenario: Detected but unauthorized

- **WHEN** Telegram Desktop is installed but the exporter has no session
- **THEN** the Telegram card shows Authorize with the guided explanation and
  no export runs.

### Requirement: No filesystem paths over HTTP

Consistent with SPEC-0013, the web layer MUST NOT accept client-supplied
filesystem paths for any Telegram operation. All Telegram setup POSTs
(authorize, enable, refresh, recheck) MUST be gated by the shared
privileged-POST checks (same-origin + per-session token).

#### Scenario: Crafted POST with a path

- **WHEN** a POST to a Telegram setup endpoint includes a `path` form value
- **THEN** the value is ignored (or the request rejected) and the exporter
  argv is built solely from server-side state.

### Requirement: Doctor and CLI coverage

`msgbrowse doctor` SHALL validate the bundled exporter resolution, the
authorization state, and the managed root (presence, parseable export, media
heuristic) with actionable hints. `msgbrowse export` and the `sync` pipeline
SHALL run the Telegram export stage via the bundled exporter, consistent
with the other three sources (flag/config/env parity, per-source
`--skip-on-error` semantics).

#### Scenario: Doctor on an unauthorized exporter

- **WHEN** doctor runs while the exporter has no session
- **THEN** it reports the authorization finding with the exact guided step as
  the hint.

### Requirement: Device-sync participation

The managed telegram root SHALL participate in Syncthing device sync exactly
like the other enum sources (SPEC-0014): folder provisioning, importer/
replica roles, and re-ingest on arrival require no telegram-specific handling
beyond enum membership. Exporter session material MUST NOT live inside the
synced root.

#### Scenario: Replica receives telegram archive

- **WHEN** a paired replica receives the telegram folder from the importer
- **THEN** its watcher re-ingests it under `source = "telegram"`, and no
  session material was part of the transfer.

### Requirement: Error Handling Standards

All error-producing operations in exporter invocation, parsing, adoption,
and import MUST follow structured error handling: errors wrapped with
context at layer boundaries; sentinel errors for the failure modes callers
distinguish (exporter-missing, authorization-shaped, malformed-export,
export-interrupted); no silent swallowing; structured key-value logging. Log
lines MUST NOT contain message bodies, personal content, or session material
— counts, ids, exit codes, and paths only.

#### Scenario: Classified failure surfaces honestly

- **WHEN** an export exits non-zero with authorization-shaped output
- **THEN** the card shows the authorization guidance state (not a generic
  failure) and the bounded raw output is in Settings → Logs.

### Requirement: Database Operation Standards

Telegram imports MUST use the existing transactional ingest path:
per-conversation atomic replacement, parameterized queries, and the
allowed-source migration in its own transaction with `PRAGMA user_version`
recorded.

#### Scenario: Interrupted import

- **WHEN** an import is cancelled mid-conversation
- **THEN** the database contains either the conversation's prior state or its
  complete new state, never a partial mix.

## Security Requirements

This spec is web-facing (Providers/setup endpoints) and invokes a bundled
subprocess that performs network egress. House posture: loopback-only,
single-user (ADR-0010); the egress is the exporter talking to Telegram's API
for the user's own account data, only during user-initiated operations
(ADR-0022 amends the egress framing accordingly, as ADR-0021 did for LAN
sync).

- **Authentication:** all mutating endpoints inherit the shared
  privileged-POST gate (same-origin verification + 256-bit per-session
  token, constant-time compare).

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| POST | /setup/telegram/authorize | Required (setup gate) | Run the exporter's session import (explicit argv) |
| POST | /setup/telegram/enable | Required (setup gate) | Full export → stage → adopt → import |
| POST | /setup/telegram/refresh | Required (setup gate) | Time-window export → merge → import |
| POST | /setup/telegram/recheck | Required (setup gate) | Re-run detection + authorization probes |
| GET | /setup/telegram/status | Loopback read | Progress fragment (poll) |

- **Subprocess execution:** explicit argv only, never a shell; argv values
  come from the fixed enum, server-side probes, or msgbrowse-owned state.
  The exporter binary resolves only from the pinned bundle (no `$PATH`).
- **Session material:** owned by the exporter in its own data directory;
  never read, copied, logged, rendered, or synced by msgbrowse.
- **Rate limiting:** the per-source single-job guard applies (a second
  Enable/Refresh while one runs is a no-op); provider-side flood-wait is the
  exporter's backoff concern.
- **Security headers / CSRF / body limits / redirects:** inherited app-wide
  (strict CSP, nosniff, no-referrer, frame denial; Origin/Sec-Fetch-Site +
  token as CSRF defense; 4 KiB `MaxBytesReader` on setup POSTs; fixed
  same-origin redirect targets only). This spec introduces no inline
  styles/scripts.

## Accessibility Requirements

This spec involves user-facing UI (the Providers card, authorization and
guidance flows). The following are MANDATORY per WCAG 2.1 AA, consistent with
the existing Providers surfaces:

- **WCAG 2.1 AA Compliance:** the Telegram card, authorization/guidance
  modal, and progress states MUST meet WCAG 2.1 AA.
- **ARIA Landmarks:** the card renders inside the existing `role="main"`
  content region; the guidance modal is a labelled dialog.
- **Icon-Only Controls:** any icon-only control MUST carry `aria-label`.
- **Dynamic Content Regions:** export/import progress updates MUST use
  `aria-live="polite"`, matching the existing setup progress fragments.
- **Keyboard Navigation:** Authorize/Enable/Recheck/guidance controls MUST
  be keyboard-operable; Escape dismisses the guidance modal.
- **Focus Management:** the guidance modal MUST trap focus while open, focus
  its first actionable element on open, and return focus to the invoking
  control on close.
