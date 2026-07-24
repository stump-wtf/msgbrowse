package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

// ledgerDDL creates the migration ledger. It lives OUTSIDE the versioned
// `migrations` chain on purpose: every database needs it, including ones that
// are already at the latest version and would therefore never run another
// migration.
//
// The ledger exists because `PRAGMA user_version` is a single integer, and
// issue #217 proved an integer is not an identity. Two lineages both defined a
// migration numbered v11 (journal tables on main, embed_runs on the contact
// -merge branch) and both reached real databases. Nothing in the schema could
// distinguish them: two databases reporting user_version = 11 had different
// tables, and the runner had no way to know.
//
// One row per applied version, with a checksum of the exact SQL that ran.
// checksum is empty for versions applied before the ledger existed — we
// genuinely cannot know which v11 a legacy database ran, and recording a guess
// would be worse than recording nothing. applied_at is likewise empty there.
const ledgerDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    checksum   TEXT    NOT NULL DEFAULT '',
    applied_at TEXT    NOT NULL DEFAULT ''
);`

// migrationChecksum is the hex sha256 of a migration's exact SQL text. Recorded
// when the migration is applied and re-verified on every subsequent Open, so
// editing a shipped migration is detected on the databases that already ran the
// old text rather than silently diverging from them.
func migrationChecksum(sqlText string) string {
	sum := sha256.Sum256([]byte(sqlText))
	return hex.EncodeToString(sum[:])
}

// migrate brings the database forward from whatever schema version it is on
// today to schemaVersion, applying each pending migration in order.
//
// Each migration runs inside its own transaction; foreign keys are toggled OFF
// for the duration of the migration (SQLite's recommended pattern when an
// ALTER may rebuild a referenced table) and back ON afterward, before commit
// recreates the regular constraint regime. Setting `PRAGMA user_version` is
// part of the same transaction, so a crashed apply leaves the version
// unchanged and the next Open retries from the same point.
//
// For a fresh database (version 0), every migration runs in order — there is
// no "current full schema" shortcut, so reasoning about how any database
// reached its current state is uniform.
func (s *Store) migrate(ctx context.Context) error {
	var current int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if current > schemaVersion {
		return fmt.Errorf("database is at schema version %d, newer than this binary supports (%d) — this build is older than the archive it was opened against; update msgbrowse", current, schemaVersion)
	}
	if err := s.ensureLedger(ctx, current); err != nil {
		return err
	}
	if err := s.verifyLedger(ctx, current); err != nil {
		return err
	}
	for v := current + 1; v <= schemaVersion; v++ {
		// Guard the slice access so a forgotten registration (bumped
		// schemaVersion without appending to migrations) yields the intended
		// clean error instead of an index-out-of-range panic.
		if v >= len(migrations) || migrations[v] == "" {
			return fmt.Errorf("no migration registered for version %d", v)
		}
		if err := s.applyMigration(ctx, v, migrations[v]); err != nil {
			return fmt.Errorf("migration v%d: %w", v, err)
		}
	}
	return nil
}

// applyMigration runs one version's SQL in a transaction with foreign-key
// enforcement disabled (SQLite's recommended pattern for ALTERs that rebuild a
// referenced table). The named return lets the deferred FK-restore surface its
// own failure so a connection can never be returned to the pool with
// enforcement silently left off.
func (s *Store) applyMigration(ctx context.Context, version int, sqlText string) (err error) {
	// Foreign keys are a connection-scoped pragma and cannot be toggled inside
	// a transaction. Toggle outside the tx, on the same connection — we use a
	// dedicated Conn so concurrent readers in the pool aren't affected.
	conn, cerr := s.db.Conn(ctx)
	if cerr != nil {
		return cerr
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys: %w", err)
	}
	// Verify the pragma actually took effect BEFORE running any destructive
	// DDL. A table rebuild (DROP + RENAME of a referenced table) that runs with
	// enforcement still ON would cascade-delete every child row and commit
	// silently. If it is not off, refuse to proceed.
	var fkEnabled int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fkEnabled); err != nil {
		return fmt.Errorf("read foreign_keys pragma: %w", err)
	}
	if fkEnabled != 0 {
		return fmt.Errorf("refusing to run migration v%d: foreign_keys did not disable", version)
	}
	// Always attempt to restore enforcement; surface a restore failure (when
	// the migration itself otherwise succeeded) so the caller aborts Open
	// rather than handing back a FK-disabled connection.
	defer func() {
		if _, rerr := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); rerr != nil && err == nil {
			err = fmt.Errorf("restore foreign_keys: %w", rerr)
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx, sqlText); err != nil {
		return fmt.Errorf("apply migration body: %w", err)
	}
	// Belt-and-suspenders: with enforcement off, a buggy migration could leave
	// dangling references. foreign_key_check reports one row per violation (none
	// when clean); treat any as fatal so we never commit a corrupt graph.
	if err := checkNoForeignKeyViolations(ctx, tx, version); err != nil {
		return err
	}
	// Record the exact text that ran, in the same transaction as the DDL and
	// the user_version bump — so the ledger can never disagree with the schema
	// it describes, even if the process dies mid-apply.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, checksum, applied_at) VALUES (?, ?, ?)
		 ON CONFLICT(version) DO UPDATE SET checksum = excluded.checksum, applied_at = excluded.applied_at`,
		version, migrationChecksum(sqlText), time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("record migration in ledger: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	rollback = false
	return nil
}

// ensureLedger creates schema_migrations and, for a database that predates the
// ledger, backfills one row per version it claims to have run.
//
// The backfill records versions WITHOUT checksums. That is the honest answer:
// a database stamped user_version = 11 in July 2026 might have run either
// lineage's v11 (#217), and there is no evidence left in the schema to say
// which. An empty checksum means "applied, unverifiable" and verifyLedger skips
// it; guessing would manufacture false confidence, and the v14 repair migration
// makes the distinction moot going forward by re-asserting both.
func (s *Store) ensureLedger(ctx context.Context, current int) error {
	if _, err := s.db.ExecContext(ctx, ledgerDDL); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	if current == 0 {
		// Fresh database: every version records itself as it runs.
		return nil
	}
	var recorded int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&recorded); err != nil {
		return fmt.Errorf("count schema_migrations: %w", err)
	}
	if recorded > 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for v := 1; v <= current; v++ {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations (version) VALUES (?)`, v); err != nil {
			return fmt.Errorf("backfill ledger v%d: %w", v, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ledger backfill: %w", err)
	}
	return nil
}

// verifyLedger re-checks every migration this database recorded a checksum for
// against the text this binary would run for that version. A mismatch means a
// shipped migration was edited after it had already been applied somewhere —
// the mutation that makes version numbers meaningless (#217). Fail loudly here
// rather than layering new migrations onto a schema we cannot describe.
//
// Rows with an empty checksum are pre-ledger backfills and are skipped; see
// ensureLedger.
func (s *Store) verifyLedger(ctx context.Context, current int) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT version, checksum FROM schema_migrations WHERE checksum != '' AND version <= ? ORDER BY version`, current)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var version int
		var recorded string
		if err := rows.Scan(&version, &recorded); err != nil {
			return fmt.Errorf("scan schema_migrations: %w", err)
		}
		if version >= len(migrations) || migrations[version] == "" {
			// This binary has no text for a version the database ran. Only
			// reachable if migrations were deleted rather than appended.
			return fmt.Errorf("database recorded migration v%d, which this binary does not define", version)
		}
		if want := migrationChecksum(migrations[version]); recorded != want {
			return fmt.Errorf("migration v%d has been edited since this database applied it "+
				"(database recorded %s, this build ships %s); shipped migrations are immutable — "+
				"append a new version instead", version, recorded, want)
		}
	}
	return rows.Err()
}

// checkNoForeignKeyViolations runs PRAGMA foreign_key_check inside tx and
// returns an error if any dangling reference exists. It fully drains and closes
// the result set before returning so the caller can keep using tx.
func checkNoForeignKeyViolations(ctx context.Context, tx *sql.Tx, version int) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("migration v%d left dangling foreign key references", version)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("foreign_key_check rows: %w", err)
	}
	return nil
}

// readUserVersion returns the database's current schema version. Useful for
// tests that need to assert the migration ran.
func readUserVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}
