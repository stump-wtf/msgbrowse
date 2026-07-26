# SPEC-0026 Design: msgbrowse-owned snapshots

- **Capability:** backups
- **Related ADRs:** [ADR-0026](../../../adr/0026-msgbrowse-owned-snapshots.md),
  [ADR-0010](../../../adr/0010-security-privacy-posture.md),
  [ADR-0013](../../../adr/0013-pure-go-sqlite-driver.md)

## Architecture

The snapshot surface lives in a new pure-Go package `internal/backup`, with
the web handlers and CLI as thin consumers — mirroring how `internal/embed`
backs the Status page's Build control and the `msgbrowse embed` CLI.

```
internal/backup
  ├── manager.go     Create / Prune / Restore / List (filesystem source of truth)
  └── manager_test.go
internal/store
  ├── store.go       ReplaceSnapshots unchanged (external inventory only)
  └── query.go       ListSnapshots unchanged (external inventory only)
internal/web
  ├── backups.go     GET /backups + POST /backups/create | prune | restore
  └── templates/backups.html   owned table (actionable) + external table (read-only)
internal/cli
  └── backups.go    msgbrowse backups {create,prune,restore,list}
```

The `snapshots` table in the DB continues to hold the **external** inventory
only; the `ReplaceSnapshots` ingest path is unchanged. msgbrowse-owned
snapshots are **listed from the filesystem** at render time, so ingest cannot
clobber the owned inventory (REQ-0026-006).

## Key design decisions

### 1. `VACUUM INTO` for consistency

A snapshot of `msgbrowse.db` is produced by `VACUUM INTO <target>`, SQLite's
online backup primitive. `VACUUM INTO` writes a transactionally consistent
copy of the database into a new file without taking an exclusive lock and
without coordinating with writers — a concurrent ingest or a read request
continues uninterrupted and the snapshot sees the committed state as of the
call.

The alternative — a filesystem copy of the live DB file — is **not a
backup**: the file may be mid-write, the WAL may be uncheckpointed, and a
restore from such a copy can be corrupt. "Copying a live SQLite file is not a
backup" is the load-bearing reason `VACUUM INTO` is the only mechanism the
manager uses for the DB.

The embeddings file, when it is a separate SQLite file (the sqlite-vec
brute-force backend), is snapshotted with the same primitive. If a future
backend stores embeddings outside SQLite, it is snapshotted with a streaming
copy **after** the DB snapshot commits, so the pair is consistent as of the
same logical instant.

### 2. Snapshot format — a directory, not a tar

A snapshot is a **directory** named `snapshot-YYYYMMDD-HHMMSS/` under
`backups.dir`, containing:

```
backups.dir/
  snapshot-20260726-143012/
    msgbrowse.db            # VACUUM INTO output
    embeddings.*            # the vector index, if a separate file
    config.yaml             # the runtime config
    manifest.json           # {version, taken_at, schema_version, file checksums}
```

A directory (not a tar) is chosen so `VACUUM INTO` can write the DB directly
to its final path without a second copy, and so restore can hard-link or
stream-copy individual files without extracting an archive first. The
`manifest.json` carries the schema version and per-file SHA-256 checksums so
restore can verify integrity before swapping the live DB.

### 3. File permissions — `0600` / `0700`

Snapshot files are created with mode `0600` and the snapshot directory with
`0700` — they contain the entire plaintext corpus (ADR-0013: no SQLCipher
in-process). The `backups.dir` parent is created `0700` on first use. The
default location (`<data_dir>/backups`) inherits `data_dir`'s already-
restrictive permissions; a custom `backups.dir` is created `0700` if it does
not exist.

### 4. Single-flight in-progress guard

Create and Prune are disk-expensive and MUST be serialized. The manager holds
an `atomic.Bool` (or `sync.Once`-shaped guard) for "a snapshot operation is in
flight": a second Create POST returns a "snapshot already in progress"
result without touching the filesystem. This mirrors the Status page's Build
control, where the button is `disabled` while a run is in progress so a
second click cannot race the guard.

### 5. Restore — two-step confirm, pre-restore snapshot, atomic swap

Restore is the destructive operation. The flow is:

1. **Step 1 (POST `/backups/restore` without `confirm=1`):** nothing is
   mutated. The row re-renders in a confirm state (the two-step unpair
   pattern from `settings.html:250-266`), with a confirm button and a
   cancel link.
2. **Step 2 (POST `/backups/restore` with `confirm=1`):**
   a. The manager takes a **pre-restore snapshot** of the current database
      (a normal Create, named `pre-restore-<timestamp>`).
   b. The selected snapshot's `msgbrowse.db` is verified via `manifest.json`
      checksums and opened in a read-only connection to confirm it is a valid
      SQLite database at the expected schema version.
   c. The live DB connection is closed, the live DB file is renamed aside
      (to `msgbrowse.db.pre-restore-<timestamp>`), and the snapshot DB is
      copied/renamed into place.
   d. The DB is re-opened. If re-open fails, the pre-restore DB is renamed
      back and the failure banner says "restore failed, rolled back to
      pre-restore state".
   e. On success, the UI reports that a restart is required (the desktop
      shell must re-open its connection; `serve` re-opens in-process).

A mid-failure is recoverable because the pre-restore snapshot and the
renamed-aside live DB are both on disk. The worst case is a crash during the
file rename, which leaves either the old or the new DB in place — never a
half-swapped file, because `rename(2)` is atomic on the same filesystem.

### 6. Path-traversal containment

The snapshot identifier in a restore/delete request is a path-traversal
surface. The manager **never** joins the request string into a path. It lists
`backups.dir` for directories matching `snapshot-\d{8}-\d{6}`, validates the
supplied identifier against that list, and refuses anything containing a path
separator or `..`. Unrecognised names are excluded from the listing rather
than displayed.

This mirrors `containWithin` (`internal/web/media.go`): the request supplies
a name, the manager resolves it against the stored inventory, and a mismatch
is a 403/400, not a fallback to a joined path.

### 7. Prune — preview, never empty the set

Prune applies the configured GFS counts. For each tier, it keeps the newest N
snapshots by timestamp and marks the rest for deletion. The delete is a
preview-first flow: the manager returns the list of snapshots it would delete
(names + count), and the POST deletes only after the preview. If the policy
would empty the set, prune keeps the newest one and reports that it held back.

### 8. Web layer — the Status-page POST shape

The three mutating POSTs (`/backups/create`, `/backups/prune`,
`/backups/restore`) follow the Status page's `POST /status/index` precedent
exactly:

- **Gate first:** `checkSetupPOST` (same-origin + per-session token +
  `MaxBytesReader`), 403 before any filesystem work.
- **Fixed-enum result banner:** `createResult` / `pruneResult` /
  `restoreResult` is a server-side enum (e.g. `created`, `inprogress`,
  `unwritable`, `diskfull`, `invalid`, `restored`, `restored-needs-restart`,
  `rolled-back`), never a request-derived string. The banner variant is
  selected from this enum, not from user input.
- **`hx-boost`ed swap:** the POST re-renders `#main-content`, so the page
  updates without a full reload.
- **Disabled while in progress:** the Create and Prune buttons are
  `disabled` while a snapshot operation is in flight, so a held-down click
  cannot fill the disk.

### 9. CLI — `msgbrowse backups {create,prune,restore,list}`

The CLI mirrors `msgbrowse embed` / `msgbrowse journal` so the action is
scriptable and testable without the web layer:

- `msgbrowse backups create` — writes a snapshot to `backups.dir`.
- `msgbrowse backups prune` — applies the configured policy (preview by
  default; `--apply` to delete).
- `msgbrowse backups restore <id>` — restores a snapshot; `--confirm` is
  required for the destructive step (the two-step confirm is bypassed only by
  the explicit flag, so a bare `msgbrowse backups restore <id>` previews and
  refuses).
- `msgbrowse backups list` — lists snapshots (owned and external).

### 10. Config keys

```yaml
backups:
  # Directory for msgbrowse-owned snapshots. Default: <data_dir>/backups.
  # MUST NOT be inside archive_root (the read-only archive; ADR-0010 §4).
  dir: ""
  retention:
    daily: 14      # keep ≤ 14 daily snapshots
    monthly: 12    # keep ≤ 12 monthly snapshots
    quarterly: 4   # keep ≤ 4 quarterly snapshots
    yearly: 2      # keep ≤ 2 yearly snapshots
```

When `backups.dir` resolves inside `archive_root`, startup warns and refuses
the path (REQ-0026-008).

## Out of scope

- **Snapshot encryption at rest.** A later ADR can add a wrapping layer
  (encrypt the tar, do not change the format). The load-bearing tradeoff is
  key management: "lost the key = lost every backup." This ADR ships plaintext
  with `0600`/`0700` and an honest SECURITY.md update.
- **A scheduler.** Create is manual or scripted via the CLI; msgbrowse does
  not install a cron. A scheduler would re-introduce a background timer and a
  new failure mode ("snapshot at 3am failed silently"), and the CLI + `launchd`
  / `systemd` is the honest answer for now.
- **Backing up archive roots.** They are large, read-only, and regenerable.
  The exclusion is justified by size and regenerability, not left implicit.
