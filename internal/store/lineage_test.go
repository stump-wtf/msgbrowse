package store

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// This file covers issue #217: two lineages independently shipped a migration
// numbered v11 and both reached real databases. The reconciliation gave main's
// journal tables the v11 slot they already occupied, renumbered the other
// lineage's pair to v12/v13, and added v14 to re-assert the union so every
// database converges no matter which path it took.
//
// The tests below rebuild each historical state and prove convergence, because
// "renumbered correctly" is not the same claim as "every database in the wild
// ends up with the same schema".

// openRawDB opens a temp SQLite database with the same pragmas Open uses,
// returning a dedicated connection so the connection-scoped foreign_keys
// pragma is stable across seeding statements.
func openRawDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(NORMAL)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedPreLedger applies raw migration text in order and stamps user_version,
// WITHOUT creating the schema_migrations ledger. That is what a database
// written by any build before this change looks like, and it is the state the
// backfill path has to cope with.
func seedPreLedger(t *testing.T, db *sql.DB, stampVersion int, bodies ...string) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()
	// v2 rebuilds a referenced table; seeding needs the same FK regime the
	// real runner uses.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("fk off: %v", err)
	}
	for i, body := range bodies {
		if _, err := conn.ExecContext(ctx, body); err != nil {
			t.Fatalf("seed body %d: %v", i+1, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(stampVersion)); err != nil {
		t.Fatalf("stamp v%d: %v", stampVersion, err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("fk on: %v", err)
	}
}

// baseBodies returns migrations 1..10 — the common ancestor both lineages
// share, before either invented a v11.
func baseBodies() []string {
	return []string{
		schemaV1, schemaV2, schemaV3, schemaV4, schemaV5,
		schemaV6, schemaV7, schemaV8, schemaV9, schemaV10,
	}
}

func hasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		t.Fatalf("look up table %s: %v", name, err)
	}
	return n > 0
}

// convergedTables is the union every database must end up with once the
// reconciliation has run, regardless of lineage.
var convergedTables = []string{
	"journal_days", "journal_digests", // main lineage v11
	"embed_runs",                           // other lineage v11, now v12
	"contact_links", "contact_merge_rules", // other lineage v12, now v13
}

// TestLineageConvergence is the core #217 regression test: every historical
// database state must reach schemaVersion with the SAME set of tables.
//
// The two v11 rows are the whole point. Before the fix a fork-lineage database
// stamped 11 or 12 was already "past" v11 and would never have received the
// journal tables, while a main-lineage database stamped 11 would never have
// received embed_runs — both arriving at the latest version describing a schema
// they did not have.
func TestLineageConvergence(t *testing.T) {
	cases := []struct {
		name   string
		stamp  int
		bodies []string
	}{
		{
			name:   "fresh database",
			stamp:  0,
			bodies: nil,
		},
		{
			name:   "common ancestor at v10",
			stamp:  10,
			bodies: baseBodies(),
		},
		{
			// main shipped journal_days/journal_digests as v11 in #213.
			name:   "main lineage at v11 (journal)",
			stamp:  11,
			bodies: append(baseBodies(), schemaV11),
		},
		{
			// The contact-merge branch shipped embed_runs as ITS v11.
			// Same integer, different schema — now renumbered to v12.
			name:   "fork lineage at v11 (embed_runs)",
			stamp:  11,
			bodies: append(baseBodies(), schemaV12),
		},
		{
			// The state of the real 241 MB archive that surfaced this bug.
			name:   "fork lineage at v12 (embed_runs + contacts)",
			stamp:  12,
			bodies: append(baseBodies(), schemaV12, schemaV13),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := openRawDB(t, "lineage.sqlite")
			if len(tc.bodies) > 0 {
				seedPreLedger(t, db, tc.stamp, tc.bodies...)
			}

			s := &Store{db: db}
			if err := s.migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}

			v, err := readUserVersion(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			if v != schemaVersion {
				t.Errorf("user_version = %d, want %d", v, schemaVersion)
			}
			for _, table := range convergedTables {
				if !hasTable(t, db, table) {
					t.Errorf("table %q missing after migrate — lineages did not converge", table)
				}
			}
			// The v13 seed row must exist exactly once no matter how many
			// times its DDL was re-asserted (v13 itself, then v14).
			var rules int
			if err := db.QueryRow(`SELECT count(*) FROM contact_merge_rules`).Scan(&rules); err != nil {
				t.Fatal(err)
			}
			if rules != 1 {
				t.Errorf("contact_merge_rules rows = %d, want exactly 1", rules)
			}
		})
	}
}

// TestMigrationRegistryIsComplete is the local half of the #217 guard —
// scripts/check-migrations.sh enforces immutability against the base branch
// (it needs git), while this catches the registration mistakes that need no
// history to detect. Both run under `make check`.
func TestMigrationRegistryIsComplete(t *testing.T) {
	if got, want := len(migrations), schemaVersion+1; got != want {
		t.Errorf("len(migrations) = %d, want %d (schemaVersion+1) — a version was "+
			"registered without bumping schemaVersion, or vice versa", got, want)
	}
	for v := 1; v <= schemaVersion; v++ {
		if v >= len(migrations) || migrations[v] == "" {
			t.Errorf("no migration registered for v%d", v)
		}
	}
	if migrations[0] != "" {
		t.Error("migrations[0] must stay empty; versions are 1-based")
	}
}

// TestRepairMigrationIsIdempotent runs migrate twice. The second call is a
// no-op, but v14's DDL re-asserting tables that already exist is exactly the
// situation the repair relies on, so prove it does not error or duplicate.
func TestRepairMigrationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := openRawDB(t, "idempotent.sqlite")
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	// Re-apply v14 directly against the finished schema — the repair path a
	// converging database takes.
	//
	// Pinned to 14, not schemaVersion. v14 is THE repair migration: it
	// deliberately re-asserts the union of the contested v11/v12/v13 DDL, so a
	// database that took any lineage converges, and being re-applied to an
	// already-complete schema is its whole job. That is a property of v14
	// specifically, not of "the newest migration" — the `migrations` invariant
	// only requires idempotency WITHIN a version transition, and an ordinary
	// additive migration (v15's ALTER TABLE ADD COLUMN; SQLite has no IF NOT
	// EXISTS for that) is correctly non-repeatable. Reading schemaVersion here
	// made this test fail the moment any normal migration was appended, which
	// says nothing about the repair path.
	const repairVersion = 14
	if err := s.applyMigration(ctx, repairVersion, migrations[repairVersion]); err != nil {
		t.Fatalf("re-apply v%d: %v", repairVersion, err)
	}
	var rules int
	if err := db.QueryRow(`SELECT count(*) FROM contact_merge_rules`).Scan(&rules); err != nil {
		t.Fatal(err)
	}
	if rules != 1 {
		t.Errorf("contact_merge_rules rows = %d after re-apply, want 1", rules)
	}
}

// TestLedgerBackfillIsHonest asserts a pre-ledger database gets a row per
// claimed version with an EMPTY checksum. Recording a real checksum there would
// assert we know which v11 it ran, which is exactly what #217 proved we cannot
// know.
func TestLedgerBackfillIsHonest(t *testing.T) {
	ctx := context.Background()
	db := openRawDB(t, "backfill.sqlite")
	seedPreLedger(t, db, 12, append(baseBodies(), schemaV12, schemaV13)...)

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	rows, err := db.Query(`SELECT version, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[int]string{}
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			t.Fatal(err)
		}
		got[v] = sum
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// 1..12 predate the ledger: recorded, unverifiable.
	for v := 1; v <= 12; v++ {
		sum, ok := got[v]
		if !ok {
			t.Errorf("v%d missing from ledger", v)
			continue
		}
		if sum != "" {
			t.Errorf("v%d checksum = %q, want empty (pre-ledger backfill must not guess)", v, sum)
		}
	}
	// 13..14 were applied by this binary and must be verifiable.
	for v := 13; v <= schemaVersion; v++ {
		sum, ok := got[v]
		if !ok {
			t.Fatalf("v%d missing from ledger", v)
		}
		if want := migrationChecksum(migrations[v]); sum != want {
			t.Errorf("v%d checksum = %q, want %q", v, sum, want)
		}
	}
}

// TestLedgerDetectsEditedMigration proves the ledger catches the mutation that
// made #217 possible in the first place: changing a migration's SQL after
// databases have already applied it. Simulated by corrupting a recorded
// checksum, which is indistinguishable from the constant having been edited.
func TestLedgerDetectsEditedMigration(t *testing.T) {
	ctx := context.Background()
	db := openRawDB(t, "edited.sqlite")
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE schema_migrations SET checksum = 'deadbeef' WHERE version = 13`); err != nil {
		t.Fatal(err)
	}
	err := s.migrate(ctx)
	if err == nil {
		t.Fatal("migrate accepted a database whose recorded migration text differs from this build")
	}
	if !strings.Contains(err.Error(), "has been edited since this database applied it") {
		t.Errorf("error = %q, want the immutability violation message", err)
	}
}

// TestForwardVersionIsRefused keeps the guard that produced the original
// user-visible symptom — an older build must refuse a newer database rather
// than migrating it backwards or running against a schema it cannot describe.
func TestForwardVersionIsRefused(t *testing.T) {
	ctx := context.Background()
	db := openRawDB(t, "forward.sqlite")
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(schemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	err := s.migrate(ctx)
	if err == nil {
		t.Fatal("migrate accepted a database newer than the binary")
	}
	if !strings.Contains(err.Error(), "newer than this binary supports") {
		t.Errorf("error = %q, want the forward-version refusal", err)
	}
}
