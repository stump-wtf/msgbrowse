---
status: accepted
date: 2026-07-26
implements: [ADR-0026]
requires: [SPEC-0008]
---

# SPEC-0026: msgbrowse-owned snapshots

- **Capability:** backups
- **Target packages:** `internal/backup` (new), `internal/store`
  (`store.go`, `query.go`), `internal/web` (`backups.go`, `server.go`,
  `templates/backups.html`), `internal/cli` (`backups.go`, new)
- **Related ADRs:** [ADR-0026 (msgbrowse owns its own snapshots)](../../../adr/0026-msgbrowse-owned-snapshots.md),
  [ADR-0010 (security/privacy posture)](../../../adr/0010-security-privacy-posture.md),
  [ADR-0013 (pure-Go SQLite driver)](../../../adr/0013-pure-go-sqlite-driver.md)
- **Tracking:** epic #232 (children: #233 ADR + spec, #234 create/prune/restore)

## Overview

msgbrowse turns the Backups tab from a read-only inventory of external
`.snapshots` tars into a working surface: a user can **create** a snapshot of
`data_dir` (the SQLite database + embeddings) and the config file, **prune**
snapshots by enforced GFS retention policy, and **restore** a snapshot over the
live database — guarded by a two-step confirm and a pre-restore snapshot.

A snapshot is a timestamped archive under a configurable `backups.dir` (default
`<data_dir>/backups`), containing a `VACUUM INTO`-produced consistent copy of
`msgbrowse.db`, the embeddings, and `config.yaml`. The existing external
`.snapshots` inventory is preserved as a separate, visually-distinct read-only
listing; the ADR-0010 §5 "never opens or decrypts" contract still holds for
those tars.

Snapshots contain the **entire plaintext message corpus** (ADR-0013: no
SQLCipher in-process), so snapshot files are created mode `0600` and the
directory mode `0700`, and SECURITY.md is updated to name the second on-disk
copy of the corpus.

## Scope

In scope: Create / Prune / Restore from the Backups tab and a `msgbrowse
backups` CLI; the `backups.dir` and `backups.retention` config keys; the
two-step restore confirm; the pre-restore snapshot; the visual distinction
between owned and external snapshots; the empty state that invites creation.

Out of scope: snapshot encryption at rest (deferred to a later ADR — key
management is a feature, not a footnote); backing up archive roots (large,
read-only, regenerable); a scheduler (Create is manual or scripted via the
CLI, not auto-cronned by msgbrowse).

## Requirements

### REQ-0026-001: Create backup

- WHEN Create backup is clicked THEN a snapshot is written under
  `backups.dir` and appears in the listing on the next render.
- WHEN a snapshot is already being written THEN the control is disabled and a
  second POST is refused rather than racing (a single-flight in-progress
  guard, mirroring the Status page's Build control).
- WHEN the target directory is unwritable or the disk is full THEN Create
  fails with an actionable banner and leaves **no partial file** in the
  listing.
- WHEN a snapshot is taken while a request or an ingest is in flight THEN the
  archived database is consistent, produced by `VACUUM INTO` rather than a
  filesystem copy of the live DB file.
- WHEN a snapshot is created THEN its files are mode `0600` and its directory
  is mode `0700` (they contain the entire corpus).

### REQ-0026-002: Retention (prune)

- WHEN prune runs THEN snapshots outside the configured GFS policy are
  removed and the count and footprint update on the next render.
- WHEN only one snapshot exists THEN prune refuses to delete it and reports
  that it held back (prune never empties the set).
- WHEN prune would delete anything THEN it names what and how many **before**
  deleting, not after.
- WHEN retention is configured THEN the daily/monthly/quarterly/yearly counts
  are honored; defaults are 14 / 12 / 4 / 2.
- WHEN no `backups.retention` block is present THEN the defaults apply.

### REQ-0026-003: Restore (two-step, guarded)

- WHEN Restore is first clicked THEN nothing is mutated and the row
  re-renders in a confirm state, following the two-step unpair pattern in
  `settings.html:250-266`.
- WHEN the confirming POST arrives THEN the current database is itself
  snapshotted first (a "pre-restore" snapshot), then the selected snapshot is
  restored over the live DB.
- WHEN restore fails partway THEN the pre-restore state is recoverable and
  the failure banner says so.
- WHEN restore succeeds THEN the app makes clear whether a restart is
  required, and does not serve a half-swapped database.
- WHEN the snapshot identifier supplied to restore is not in the stored
  inventory THEN the request is rejected as a path-traversal attempt; the
  identifier is never joined from a request string and anything containing a
  separator or `..` is refused.

### REQ-0026-004: Listing — owned vs. external, distinguishable

- WHEN both msgbrowse-owned and external snapshots exist THEN a user can tell
  which msgbrowse can prune and restore (owned) and which are read-only
  (external).
- WHEN an external `.snapshots` tar is present THEN it is listed in a
  visually distinct read-only section, never pruneable or restorable from
  msgbrowse, and the ADR-0010 §5 "never opens or decrypts" contract holds.
- WHEN the listing renders THEN filenames stay `html/template`-escaped and
  unrecognised names are excluded rather than displayed.

### REQ-0026-005: Empty state

- WHEN no snapshot exists THEN the tab explains what a backup contains, where
  it will be written, and offers a Create backup button — it does not render
  a bare "No snapshot pipeline on this machine." line.

### REQ-0026-006: Ingest does not clobber the owned inventory

- WHEN ingest runs THEN the `ReplaceSnapshots` path that rewrites the
  `snapshots` table does not clobber msgbrowse-owned snapshots. Owned
  snapshots are listed from the filesystem at render time, not from the DB.

### REQ-0026-007: CLI equivalent

- WHEN `msgbrowse backups create` runs THEN a snapshot is written to
  `backups.dir`, scriptable and testable without the web layer, consistent with
  `msgbrowse embed` / `msgbrowse journal`.
- WHEN `msgbrowse backups prune` runs THEN the configured policy is applied.
- WHEN `msgbrowse backups restore <id>` runs THEN the two-step confirm is
  bypassed only by an explicit `--confirm` flag (the CLI is scriptable but
  still guarded against a bare destructive run).

### REQ-0026-008: Config

- WHEN `backups.dir` is unset THEN the default `<data_dir>/backups` is used.
- WHEN `backups.dir` resolves inside `archive_root` THEN startup warns and
  refuses the path (it violates the read-only archive contract, ADR-0010 §4).
- WHEN `backups.retention` is configured THEN the tier counts are honored.

## Security Checklist

- Authentication middleware applied — every mutating POST (`create`, `prune`,
  `restore`) behind `checkSetupPOST`: same-origin + per-session token +
  `MaxBytesReader`, 403 before any filesystem work.
- Input validation — the snapshot identifier is a path-traversal surface; a
  restore/delete target MUST be validated against the stored inventory, never
  joined from a request string; reject anything containing separators or `..`.
- Output encoding — filenames render into the table; they stay
  `html/template`-escaped; unrecognised names are excluded from the listing.
- Rate limiting — Create and Prune are disk-expensive; they MUST be serialized
  behind an in-progress guard so held-down clicks cannot fill the disk.
- Request body size limits — `MaxBytesReader` on all three POSTs.
- File permissions — snapshot files `0600`, directory `0700` (they contain the
  entire corpus).
