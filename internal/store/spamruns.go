// Spam-scan run bookkeeping (issue #402): the store-level queries behind
// REQ-0028-014's durable log of unsolicited-contact scans. internal/spam
// records a row per scan here (begin → per-conversation heartbeat → finish,
// terminal write even on abort), the exact analogue of embed_runs for
// indexing and fact_runs for extraction. `msgbrowse spam-scan` and a future
// web-driven scan are separate processes sharing one SQLite file, so this
// table is their only communication channel — and the only account of a scan
// that died before its Summary could be printed.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SpamRun is one unsolicited-contact scan. FinishedAt is the zero time while
// the run is still in flight (or crashed before its terminal write);
// UpdatedAt is the per-conversation heartbeat readers use to tell a live run
// from a dead one. The counters are live during a run and final totals after
// it; Error carries the abort reason ("" on success). RulesetVersion,
// ScanEnv, AddressBook and Degraded record what ran and under which
// conditions (REQ-0028-014); ScanEnv reuses the REQ-0028-013 stamp (#385).
type SpamRun struct {
	ID         int64
	StartedAt  time.Time
	UpdatedAt  time.Time
	FinishedAt time.Time
	DurationMS int64

	RulesetVersion  string
	ScanEnv         string
	AddressBook     string
	Degraded        bool
	Conversations   int
	MessagesScanned int
	Findings        int
	Candidates      int
	OptOutsDetected int
	Senders         int
	Error           string
}

// InFlight reports whether the run has not recorded its terminal write.
func (r SpamRun) InFlight() bool { return r.FinishedAt.IsZero() }

const spamRunCols = `id, started_at, updated_at, finished_at, duration_ms,
	ruleset_version, scan_env, address_book, degraded,
	conversations, messages_scanned, findings, candidates, optouts_detected,
	senders, error`

// BeginSpamRun records the start of a scan and returns the row id the run's
// later heartbeat/finish writes target. The environment columns are stamped
// up front, before any conversation is examined, so a scan that aborts in its
// first batch still answers "what ran, under which conditions". The heartbeat
// (updated_at) starts equal to startedAt.
func (s *Store) BeginSpamRun(ctx context.Context, r SpamRun, startedAt time.Time) (int64, error) {
	ts := startedAt.UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO spam_runs
			(started_at, updated_at, ruleset_version, scan_env, address_book, degraded)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		ts, ts, r.RulesetVersion, r.ScanEnv, r.AddressBook, boolInt(r.Degraded))
	if err != nil {
		return 0, fmt.Errorf("begin spam run: %w", err)
	}
	return res.LastInsertId()
}

// UpdateSpamRunProgress refreshes a run's live counters and heartbeat after a
// conversation's batches land. Readers treat an unfinished row with a fresh
// heartbeat as "scan in progress".
func (s *Store) UpdateSpamRunProgress(ctx context.Context, id int64, conversations, messagesScanned, findings, candidates, optoutsDetected, senders int, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE spam_runs SET updated_at = ?, conversations = ?,
		        messages_scanned = ?, findings = ?, candidates = ?,
		        optouts_detected = ?, senders = ?
		  WHERE id = ?`,
		at.UTC().Format(time.RFC3339), conversations, messagesScanned,
		findings, candidates, optoutsDetected, senders, id); err != nil {
		return fmt.Errorf("update spam run progress: %w", err)
	}
	return nil
}

// FinishSpamRun records a run's terminal state: finished_at (which flips the
// row out of "in flight"), duration, the final totals, and the abort error
// when the run failed. r.ID selects the row; r.UpdatedAt is stamped to
// r.FinishedAt. A failed run keeps whatever counts it completed first —
// REQ-0028-014's scenario has an aborted run legible afterwards.
func (s *Store) FinishSpamRun(ctx context.Context, r SpamRun) error {
	ts := r.FinishedAt.UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE spam_runs
		    SET finished_at = ?, updated_at = ?, duration_ms = ?,
		        ruleset_version = ?, scan_env = ?, address_book = ?, degraded = ?,
		        conversations = ?, messages_scanned = ?, findings = ?, candidates = ?,
		        optouts_detected = ?, senders = ?, error = ?
		  WHERE id = ?`,
		ts, ts, r.DurationMS,
		r.RulesetVersion, r.ScanEnv, r.AddressBook, boolInt(r.Degraded),
		r.Conversations, r.MessagesScanned, r.Findings, r.Candidates,
		r.OptOutsDetected, r.Senders, r.Error, r.ID); err != nil {
		return fmt.Errorf("finish spam run: %w", err)
	}
	return nil
}

// scanSpamRun reads one spam_runs row from sc into a SpamRun with its
// RFC3339 string columns converted to times.
func scanSpamRun(sc func(dest ...any) error) (SpamRun, error) {
	var (
		r                          SpamRun
		started, updated, finished string
		degraded                   int
	)
	err := sc(&r.ID, &started, &updated, &finished, &r.DurationMS,
		&r.RulesetVersion, &r.ScanEnv, &r.AddressBook, &degraded,
		&r.Conversations, &r.MessagesScanned, &r.Findings, &r.Candidates,
		&r.OptOutsDetected, &r.Senders, &r.Error)
	if err != nil {
		return r, err
	}
	r.Degraded = degraded == 1
	r.StartedAt = parseRFC3339(started)
	r.UpdatedAt = parseRFC3339(updated)
	if finished != "" {
		r.FinishedAt = parseRFC3339(finished)
	}
	return r, nil
}

// LatestSpamRun returns the most recently started spam scan, or nil when none
// has ever been recorded. The caller decides what an unfinished row means by
// its heartbeat age (live vs crashed), exactly as it does for embed_runs.
func (s *Store) LatestSpamRun(ctx context.Context) (*SpamRun, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+spamRunCols+` FROM spam_runs ORDER BY id DESC LIMIT 1`)
	r, err := scanSpamRun(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest spam run: %w", err)
	}
	return &r, nil
}

// RecentSpamRuns returns the most recently started spam scans, newest first,
// capped at n (n <= 0 yields an empty slice). It backs the run-history table:
// LatestSpamRun answers "what is the current state", this answers "what has
// the scanner done lately".
func (s *Store) RecentSpamRuns(ctx context.Context, n int) ([]SpamRun, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+spamRunCols+` FROM spam_runs ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, fmt.Errorf("recent spam runs: %w", err)
	}
	defer rows.Close()

	var out []SpamRun
	for rows.Next() {
		r, err := scanSpamRun(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("recent spam runs: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recent spam runs: %w", err)
	}
	return out, nil
}
