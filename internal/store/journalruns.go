// Journal-run bookkeeping (#240): the store-level queries behind the Journal
// page's build card. internal/journal records a row per pass here (begin →
// per-day heartbeat → terminal write), and the web layer reads the latest row to
// show the last run, a live in-progress marker, and an interrupted run that can
// be resumed.
//
// This is the journal's analogue of embed_runs, and exists for the same reason:
// `msgbrowse journal` and `msgbrowse serve` are separate processes sharing one
// SQLite file, so the table is their only communication channel. It is what lets
// the page refuse to start a second build rather than race one already running
// in another process.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// JournalRun is one journal pass. FinishedAt is the zero time while the run is
// still in flight (or died before its terminal write); UpdatedAt is the per-day
// heartbeat readers use to tell a live run from a crashed one. Digested/Cached/
// Skipped are live counters during a run and the final totals after it. Error
// carries the abort reason for a failed run ("" on success).
type JournalRun struct {
	ID    int64
	Model string
	// Scope is "" for a whole-archive pass, else the single day a per-day
	// Rebuild targeted ("YYYY-MM-DD").
	Scope      string
	StartedAt  time.Time
	UpdatedAt  time.Time
	FinishedAt time.Time
	DurationMS int64
	Days       int
	Digested   int
	Cached     int
	Skipped    int
	Error      string
}

// InFlight reports whether the run has not recorded its terminal write.
func (r JournalRun) InFlight() bool { return r.FinishedAt.IsZero() }

const journalRunCols = `id, model, scope, started_at, updated_at, finished_at,
                        duration_ms, days, digested, cached, skipped, error`

// BeginJournalRun records the start of a journal pass and returns the row id the
// run's later heartbeat/finish writes target. The heartbeat (updated_at) starts
// equal to startedAt. scope is "" for a whole-archive pass.
func (s *Store) BeginJournalRun(ctx context.Context, model, scope string, startedAt time.Time) (int64, error) {
	ts := startedAt.UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO journal_runs (model, scope, started_at, updated_at) VALUES (?, ?, ?, ?)`,
		model, scope, ts, ts)
	if err != nil {
		return 0, fmt.Errorf("begin journal run: %w", err)
	}
	return res.LastInsertId()
}

// UpdateJournalRunProgress refreshes a run's live counters and heartbeat after a
// day is digested. Readers treat an unfinished row with a fresh heartbeat as
// "building in progress"; one whose heartbeat has gone cold reads as interrupted.
func (s *Store) UpdateJournalRunProgress(ctx context.Context, id int64, days, digested int, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE journal_runs SET days = ?, digested = ?, updated_at = ? WHERE id = ?`,
		days, digested, at.UTC().Format(time.RFC3339), id); err != nil {
		return fmt.Errorf("update journal run progress: %w", err)
	}
	return nil
}

// FinishJournalRun records a run's terminal state: finished_at (which flips the
// row out of "in flight"), the final totals, and the abort error when the run
// failed. r.ID selects the row; r.UpdatedAt is stamped to r.FinishedAt.
//
// The caller MUST make this a deferred write. A journal pass can abort on a
// transport error mid-digest or a cancelled context, and a run that never
// records its terminal state leaves the page reading "building…" until the
// heartbeat goes stale — a much worse failure than reporting the error.
func (s *Store) FinishJournalRun(ctx context.Context, r JournalRun) error {
	ts := r.FinishedAt.UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE journal_runs
		    SET finished_at = ?, updated_at = ?, duration_ms = ?, days = ?,
		        digested = ?, cached = ?, skipped = ?, error = ?
		  WHERE id = ?`,
		ts, ts, r.DurationMS, r.Days, r.Digested, r.Cached, r.Skipped, r.Error, r.ID); err != nil {
		return fmt.Errorf("finish journal run: %w", err)
	}
	return nil
}

func scanJournalRun(sc interface{ Scan(...any) error }) (JournalRun, error) {
	var (
		r                          JournalRun
		started, updated, finished string
	)
	if err := sc.Scan(&r.ID, &r.Model, &r.Scope, &started, &updated, &finished,
		&r.DurationMS, &r.Days, &r.Digested, &r.Cached, &r.Skipped, &r.Error); err != nil {
		return JournalRun{}, err
	}
	r.StartedAt = parseRFC3339(started)
	r.UpdatedAt = parseRFC3339(updated)
	if finished != "" {
		r.FinishedAt = parseRFC3339(finished)
	}
	return r, nil
}

// LatestJournalRun returns the run whose state the page should report: any
// still-in-flight run in preference to a newer finished one, else the newest
// row. nil when none has ever been recorded. The caller decides what an
// unfinished row means by its heartbeat age (live vs crashed).
func (s *Store) LatestJournalRun(ctx context.Context) (*JournalRun, error) {
	// In-flight rows sort FIRST, then newest. Plain `ORDER BY id DESC` would hide
	// a run that is still going behind any newer finished row — and this query is
	// the cross-process guard, so hiding a live run means letting a second
	// (billable) one start alongside it.
	row := s.db.QueryRowContext(ctx,
		`SELECT `+journalRunCols+`
		   FROM journal_runs
		  ORDER BY (finished_at = '') DESC, id DESC
		  LIMIT 1`)
	r, err := scanJournalRun(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest journal run: %w", err)
	}
	return &r, nil
}

// RecentJournalRuns returns the most recently started passes, newest first,
// capped at n (n <= 0 yields nothing without touching the database). It backs
// the Journal page's run-history table: LatestJournalRun answers "what is the
// current state", this answers "what has the journal done lately".
func (s *Store) RecentJournalRuns(ctx context.Context, n int) ([]JournalRun, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+journalRunCols+` FROM journal_runs ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, fmt.Errorf("recent journal runs: %w", err)
	}
	defer rows.Close()

	var out []JournalRun
	for rows.Next() {
		r, err := scanJournalRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
