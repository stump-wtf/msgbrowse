# ADR-0026: msgbrowse owns its own snapshots — create, prune, restore

- **Status:** Accepted
- **Date:** 2026-07-26
- **Deciders:** Joe Stump
- **Supersedes:** the "msgbrowse never creates or prunes snapshots" posture
  documented in `internal/ingest/snapshots.go` and in
  [ADR-0010 §5](0010-security-privacy-posture.md).
- **Related:**
  - [ADR-0010](0010-security-privacy-posture.md) —
    the snapshot is a *second full plaintext copy of the corpus*; this ADR
    broadens the on-disk asset list and the SECURITY.md copy, it does not
    weaken the loopback-only / one-egress posture.
  - [ADR-0013](0013-pure-go-sqlite-driver.md) —
    msgbrowse's database is **not** SQLCipher-encrypted; a snapshot is therefore
    a plaintext copy, which is why file mode and storage location matter here.
  - [SPEC-0026](../openspec/specs/backups/spec.md)
    — the requirements and WHEN/THEN scenarios that pin this decision.

## Context and Problem Statement

The Backups tab was, until this ADR, a **read-only inventory** of tars that an
**external** backup job was expected to write into
`{archive_root}/.snapshots`. On a real machine with no such job — which is
every desktop-onboarded machine, since the desktop app does not install a
backup scheduler — the tab rendered a single line:

> No snapshot pipeline on this machine.

There was no way to create a backup from msgbrowse, and `internal/ingest/snapshots.go`
documents the posture outright:

> msgbrowse **never creates or prunes snapshots** (the upstream backup job does).

That posture also had a **mismatch** worth naming. The external `.snapshots`
tars are described as SQLCipher-encrypted backups of the *source* application
databases (Signal's `signal.db`, iMessage's `chat.db`) — things msgbrowse
"lists but never opens or decrypts." But the artifact that is actually
expensive to lose is msgbrowse's **own** `data_dir` — `msgbrowse.db` plus the
embeddings — which represents hours of ingest and LLM spend and is not
backed up by anything. A user who lost `data_dir` would lose every embedding
and every contact-merge decision, and face a full re-ingest plus a full
re-embed to get back to where they were.

Reversing the posture means msgbrowse starts **writing files, deleting files,
and overwriting a live database**. Each of those is a new failure mode and a
new privacy surface, and each is decided here rather than in a commit message.

## Decision

msgbrowse **owns its own snapshots**: a timestamped archive of `data_dir`
(`msgbrowse.db` + the embeddings) and the config file, that msgbrowse
**creates, lists, prunes by GFS tier, and can restore**. Source export trees
stay out — they are large and the exporters can regenerate them.

### 1. Snapshot contents

A snapshot contains **`data_dir` and the config file only**:

- `msgbrowse.db` (the SQLite database — messages, contacts, contact-merge
  decisions, journal digests, ingest state, the FTS5 index).
- The embeddings (the vector index that backs semantic search; hours of LLM
  spend to rebuild).
- `config.yaml` (the runtime config, which pins `archive_root`, `data_dir`,
  the LLM route, and every source — a restored snapshot without the config
  that points at the archive is half a backup).

**Excluded:** the archive roots (Signal / iMessage / WhatsApp export trees).
They are large, read-only, and regenerable by re-running the exporters; backing
them up would duplicate the corpus many times over and bloat the snapshot
target.

A restored snapshot is **browsable without re-running any exporter or
re-embedding** — that is the load-bearing property and the reason embeddings
are in the archive.

### 2. Storage location — configurable, never inside the read-only archive

`{archive_root}/.snapshots` is the *external* job's directory and the archive
is documented **read-only** ("msgbrowse only ever reads this tree" —
`config.example.yaml`; ADR-0010 §4). msgbrowse's own snapshots need their own
**configurable path**.

A new top-level config key, `backups.dir`, selects the snapshot directory. When
unset, the default is **`<data_dir>/backups`** — outside both `archive_root`
(the read-only tree) and the live database file (so a restore can swap the live
DB without tripping over its own snapshot target).

If the configured `backups.dir` resolves **inside `archive_root`**, startup
emits a warning and refuses the path: writing into the read-only archive
violates ADR-0010 §4 and would surprise an operator whose archive is mounted
`:ro`.

### 3. Encryption posture — plaintext, with restrictive file mode

The external tars are SQLCipher-encrypted; msgbrowse's own database is **not**
(ADR-0013, pure-Go driver — no SQLCipher available in-process). A snapshot of
it is therefore a **plaintext copy of the entire message corpus**.

The decision is **plaintext at rest**, with the mitigations:

- Snapshot files are created with mode **`0600`** and the snapshot directory
  with **`0700`** — they contain the corpus.
- The default location (`<data_dir>/backups`) inherits `data_dir`'s
  already-restrictive permissions.
- **SECURITY.md is updated** to say a second full copy of the corpus now exists
  on disk (see §Consequences), and the UI names the path on the Backups tab so
  the operator can see where the plaintext lives.

**Key management is out of scope for this ADR.** A snapshot-encryption layer
would need a key the operator must not lose ("lost the key = lost every
backup") and a key the app cannot bake into the image. That is a real feature
with its own ADR; the **plaintext-with-restrictive-mode** posture is honest
about what is shipped today, and adding encryption later is additive (wrap the
tar, do not change the format).

### 4. Consistency mechanism — `VACUUM INTO`, not a filesystem copy

Copying a live SQLite file is **not a backup**: the file may be mid-write, the
WAL may be uncheckpointed, and a restore from such a copy can be corrupt.

The consistency mechanism is **`VACUUM INTO <target>`** — SQLite's online
backup primitive that produces a transactionally consistent snapshot of the
database into a new file, without taking an exclusive lock or requiring the
caller to coordinate with writers. `VACUUM INTO` runs on a connection that
sees the committed state; concurrent ingests and reads continue uninterrupted.

The embeddings file (when it is a separate file from the DB, as in the
sqlite-vec brute-force backend) is snapshotted with the same primitive if it
is a SQLite file, or with a streaming copy **only after** the DB snapshot
commits, so the pair is consistent as of the same logical instant.

### 5. Retention semantics — enforced GFS policy, never empties the set

The GFS tiers currently *displayed* (`daily ≤14d`, `monthly ≤~13mo`,
`quarterly ≤~3y`, else `yearly`) are a **classification of someone else's
filenames**. Once msgbrowse prunes, those become **enforced policy**.

A new config block `backups.retention` carries the tier counts. Defaults keep
the existing boundaries:

| Tier    | Keep ≤ age         | Default count |
|---------|--------------------|---------------|
| daily   | 14 days            | 14            |
| monthly | ~13 months (395d)  | 12            |
| quarterly | ~3 years (1095d) | 4             |
| yearly  | older than quarterly | 2          |

**Prune is bounded and safe:** it never deletes the only snapshot. If the
policy would empty the set, prune keeps the newest one and reports that it
held back. Prune **previews** what it would delete (names + count) before
deleting, not after.

### 6. The external `.snapshots` inventory — kept, visually distinguished

The existing external-`.snapshots` inventory is **preserved as a separate
read-only listing**, not silently dropped. A machine that still runs an external
backup job continues to see those tars on the Backups tab, clearly marked as
**read-only / external** and never pruneable or restorable from msgbrowse —
the ADR-0010 §5 "never opens or decrypts" contract still holds for them.

The two listings are visually distinct: msgbrowse-owned snapshots are the
actionable ones (Create / Restore / Prune), external snapshots are a static
table. A user can tell at a glance which msgbrowse can prune and restore.

### 7. Restore — guarded, two-step, pre-restore snapshot first

Restore replaces a live database. It is **not a single unconfirmed click**:

1. **Step 1 (click Restore):** nothing is mutated. The row re-renders in a
   confirm state, following the two-step unpair pattern in
   `settings.html:250-266`.
2. **Step 2 (confirm):** the **current database is itself snapshotted first**
   (a "pre-restore" snapshot), then the selected snapshot is restored over the
   live DB. A mid-failure is recoverable: the pre-restore snapshot is the
   rollback point, and the UI says so.

Restore succeeds by swapping the live DB file atomically (rename) after the
snapshot is extracted to a temp file and verified to open. The app makes clear
**whether a restart is required** (it is, for the desktop shell; `serve` can
re-open the connection), and does not serve a half-swapped database.

## Considered Options

### Snapshot contents

1. **`data_dir` (DB + embeddings) + config file (CHOSEN).** Restorable without
   re-running any exporter or re-embedding; the expensive artifacts are
   preserved.
2. `data_dir` only, no config. **Rejected:** a restored DB without the config
   that points at `archive_root` and pins the LLM route is half a backup; the
   operator has to re-derive the config to browse.
3. Include the archive trees. **Rejected:** they are large, read-only, and
   regenerable; backing them up duplicates the corpus many times and bloats the
   target. The exclusion is justified by **size and regenerability**.

### Storage location

1. **Configurable `backups.dir`, default `<data_dir>/backups`, refuse
   `archive_root` (CHOSEN).** Outside the read-only archive and outside the
   live DB file; inherits `data_dir`'s permissions.
2. Reuse `{archive_root}/.snapshots`. **Rejected:** violates ADR-0010 §4
   (read-only archive) and collides with the external job's directory.
3. A system-default path (e.g. `~/.cache/msgbrowse/backups`). **Rejected:**
   puts the corpus in a location the operator does not associate with msgbrowse
   and is harder to reason about for backups-of-backups.

### Encryption

1. **Plaintext at rest with `0600` files / `0700` directory (CHOSEN).** Honest
   about what is shipped today; no key to lose; restrictive mode contains the
   blast radius; SECURITY.md names the new copy.
2. Symmetric encryption with a user-supplied passphrase. **Rejected for this
   ADR:** key management is a real feature — "lost the key = lost every
   backup" — and the pure-Go driver cannot use SQLCipher. A later ADR can add
   a wrapping layer additively (encrypt the tar, do not change the format).
3. Rely on FileVault / full-disk encryption. **Insufficient:** FileVault is
   assumed (ADR-0010) but does not protect against other software running as
   the same user; `0600` is the defense that does.

### Consistency

1. **`VACUUM INTO` (CHOSEN).** SQLite's online backup primitive; produces a
   transactionally consistent snapshot without an exclusive lock; available in
   the pure-Go driver.
2. Filesystem copy of the DB file. **Rejected:** a live SQLite file may be
   mid-write; the copy can be corrupt. "Copying a live SQLite file is not a
   backup" is the load-bearing reason this option is rejected.
3. `sqlite3_backup` API step loop. **Equivalent to `VACUUM INTO` in result but
   more code; `VACUUM INTO` is a single statement.** Rejected on simplicity.

### Retention

1. **Configured GFS counts, never empty the set (CHOSEN).** Reuses the tier
   boundaries already displayed; makes them policy; safe by construction.
2. Time-only pruning (delete anything older than N days). **Rejected:** loses
   the GFS shape that keeps a long history at low cost.
3. No pruning. **Rejected:** the snapshot target would grow without bound and
   the operator would have to prune by hand, which is the failure mode this ADR
   exists to fix.

### External inventory

1. **Keep as a separate read-only listing, visually distinguished (CHOSEN).**
   A machine with an external job keeps seeing its tars; ADR-0010 §5 holds.
2. Drop the external inventory. **Rejected:** silently regresses a machine
   that relied on the listing; the ADR must be explicit, not silent.
3. Merge the two listings into one table. **Rejected:** a user could not tell
   which msgbrowse can prune/restore vs. which is read-only; the distinction
   is load-bearing for safe operation.

## Consequences

### Positive

- A user with no external backup job can **create a restorable snapshot from
  the Backups tab** — the original epic acceptance criterion.
- The retention tiers on display become **enforced policy**, not a
  classification of someone else's filenames.
- Restore is **guarded** (two-step confirm) and **recoverable** (pre-restore
  snapshot), so a misclick or a mid-failure does not lose the live DB.
- `data_dir` — the expensive artifact (ingest + embeddings) — is finally
  backed up.

### Negative

- **A second full plaintext copy of the corpus now exists on disk.** This is
  the real cost of the decision. SECURITY.md is updated to name it; the file
  mode (`0600` / `0700`) and the default location (under `data_dir`, already
  restrictive) contain the blast radius; encryption-at-rest is deferred to a
  later ADR with an honest "lost the key = lost every backup" tradeoff.
- **msgbrowse now writes, deletes, and overwrites files.** Each is a new
  failure mode: a full disk aborts Create; a prune bug could delete snapshots;
  a restore bug could swap a bad DB. The mitigations are: Create refuses
  cleanly on a full disk (no partial file in the listing), prune never empties
  the set and previews before deleting, restore takes a pre-restore snapshot
  first and is two-step confirmed.
- **The snapshot identifier is a path-traversal surface.** A restore/delete
  target MUST be validated against the stored inventory, never joined from a
  request string; the SPEC requires rejecting anything containing separators
  or `..`.
- **The external inventory is still listed.** A machine with an external job
  now shows two tables; the visual distinction must be clear or a user will
  try to restore an external tar msgbrowse cannot open.

### Neutral

- The `snapshots` table in the DB continues to hold the **external** inventory
  (the `ReplaceSnapshots` ingest path is unchanged). msgbrowse-owned snapshots
  are listed from the filesystem at render time, not from the DB, so ingest
  cannot clobber the owned inventory.
- `internal/ingest/snapshots.go`'s "never creates or prunes" comment is
  corrected to point at this ADR; the classification logic stays (it now
  serves the owned listing too).
- The GFS tier boundaries stay the same numbers; they become policy.

## Requirements

Normative requirements live in
[SPEC-0026](../openspec/specs/backups/spec.md)
with design rationale in its paired
[design.md](../openspec/specs/backups/design.md). Implementation is tracked by
epic #232 (children: #233 this ADR + spec, #234 the create/prune/restore
surface).
