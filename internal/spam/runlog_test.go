package spam

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/contacts"
	"github.com/joestump/msgbrowse/internal/store"
)

// REQ-0028-014: every scan is durably logged — a spam_runs row inserted when
// the run starts, heartbeated while it works, stamped on termination with
// duration and final totals or the error text when it aborted.
func TestRunWritesTerminalRowOnSuccess(t *testing.T) {
	st := newStore(t)
	seed(t, st, "+15551110001",
		[3]string{"2025-01-05 09:00:00", "+15551110001", "cut your solar bill"},
	)

	run := runScan(t, st, Options{
		AddressBook: fakeBook{avail: contacts.Available, nums: []string{"+14045559999"}},
	})

	row, err := st.LatestSpamRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		t.Fatal("no spam_runs row written")
	}
	if row.InFlight() {
		t.Error("row still in flight after a completed run")
	}
	if row.FinishedAt.Before(row.StartedAt) {
		t.Errorf("finished_at %v before started_at %v", row.FinishedAt, row.StartedAt)
	}
	if row.DurationMS < 0 {
		t.Errorf("duration_ms = %d", row.DurationMS)
	}
	if row.Error != "" {
		t.Errorf("error = %q, want empty on success", row.Error)
	}
	if row.RulesetVersion != run.RulesetVersion {
		t.Errorf("ruleset_version = %q, want %q", row.RulesetVersion, run.RulesetVersion)
	}
	// The environment stamp reuses the #385 scanEnv exactly (REQ-0028-013);
	// fakeBook does not name a provider, so it records "unknown".
	if want := "unknown/" + contacts.Available.String(); row.ScanEnv != want {
		t.Errorf("scan_env = %q, want %q", row.ScanEnv, want)
	}
	if row.AddressBook != run.AddressBook || row.Degraded != run.Degraded {
		t.Errorf("address_book=%q degraded=%v, want %q/%v",
			row.AddressBook, row.Degraded, run.AddressBook, run.Degraded)
	}
	if row.Conversations != run.Conversations || row.Senders != run.Senders ||
		row.MessagesScanned != run.MessagesScanned ||
		row.Findings != run.Findings || row.Candidates != run.Candidates ||
		row.OptOutsDetected != run.OptOutsDetected {
		t.Errorf("counters = %+v, want those of Summary %+v", row, run)
	}
}

// A failed scan is legible afterwards (REQ-0028-014 scenario): terminal row,
// carries the error text. The rows it did write before the abort stay — here
// there are none, because the failure is the targeted-id typo guard.
func TestRunLogsAbortedScanWithError(t *testing.T) {
	st := newStore(t)
	seed(t, st, "+15551110001",
		[3]string{"2025-01-05 09:00:00", "+15551110001", "cut your solar bill"},
	)

	_, err := Run(context.Background(), st, Options{
		Rules:              testRules(t, nil),
		AddressBook:        fakeBook{avail: contacts.Available},
		OnlyConversationID: 9999,
	})
	if err == nil {
		t.Fatal("expected error for unknown conversation id")
	}

	row, err2 := st.LatestSpamRun(context.Background())
	if err2 != nil {
		t.Fatal(err2)
	}
	if row == nil {
		t.Fatal("aborted run left no spam_runs row")
	}
	if row.InFlight() {
		t.Error("aborted run's row still reads as in flight")
	}
	if row.Error == "" {
		t.Error("aborted run's row carries no error text")
	}
	if !strings.Contains(row.Error, "9999") {
		t.Errorf("error text does not identify the failing input: %q", row.Error)
	}
}

// Begin → heartbeat → finish, and what readers see at each stage. Heartbeat
// staleness semantics live with the caller (same contract as embed_runs); the
// store just keeps updated_at fresh.
func TestSpamRunProgressAndHistory(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	start := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	id, err := st.BeginSpamRun(ctx, store.SpamRun{
		RulesetVersion: "abc123",
		ScanEnv:        "macoscontacts/available",
		AddressBook:    "available",
		Degraded:       false,
	}, start)
	if err != nil {
		t.Fatal(err)
	}

	live, err := st.LatestSpamRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if live == nil || !live.InFlight() {
		t.Fatalf("freshly begun run should be in flight, got %+v", live)
	}
	if live.ScanEnv != "macoscontacts/available" {
		t.Errorf("scan_env = %q — an early-crashed run must still answer which environment ran", live.ScanEnv)
	}

	if err := st.UpdateSpamRunProgress(ctx, id, 1, 40, 3, 1, 0, 1,
		start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	live, _ = st.LatestSpamRun(ctx)
	if live.Conversations != 1 || live.MessagesScanned != 40 || live.Findings != 3 {
		t.Errorf("heartbeat lost counters: %+v", live)
	}

	if err := st.FinishSpamRun(ctx, store.SpamRun{
		ID:              id,
		StartedAt:       start,
		FinishedAt:      start.Add(2 * time.Minute),
		DurationMS:      120000,
		RulesetVersion:  "abc123",
		ScanEnv:         "macoscontacts/available",
		AddressBook:     "available",
		Conversations:   2,
		MessagesScanned: 99,
		Findings:        7,
		Candidates:      4,
		Senders:         2,
		Error:           "",
	}); err != nil {
		t.Fatal(err)
	}

	done, err := st.LatestSpamRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if done.InFlight() || done.MessagesScanned != 99 || done.Error != "" {
		t.Errorf("terminal row wrong: %+v", done)
	}

	rows, err := st.RecentSpamRuns(ctx, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("recent runs = %d, %v", len(rows), err)
	}

	if _, err := st.BeginSpamRun(ctx, store.SpamRun{Degraded: true}, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	dg, err := st.RecentSpamRuns(ctx, 10)
	if err != nil || len(dg) != 2 {
		t.Fatalf("recent runs after second begin = %d, %v", len(dg), err)
	}
	if !dg[0].InFlight() || !dg[0].Degraded {
		t.Errorf("second row should be in-flight degraded, got %+v", dg[0])
	}
}

// A scan cancelled mid-flight (Ctrl-C, a shutdown deadline) is the abort case
// REQ-0028-014 exists for, and it is the one the deferred terminal write can
// silently lose: the write rides the same context the scan aborted on, so a
// cancelled ctx fails the UPDATE and the row reads as in flight forever. The
// run's own outcome is unchanged — only its legibility afterwards.
func TestRunLogsCancelledScanWithError(t *testing.T) {
	st := newStore(t)
	seed(t, st, "+15551110001",
		[3]string{"2025-01-05 09:00:00", "+15551110001", "cut your solar bill"},
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the scan loop reaches its ctx.Err() check

	if _, err := Run(ctx, st, Options{
		Rules:       testRules(t, nil),
		AddressBook: fakeBook{avail: contacts.Available},
	}); err == nil {
		t.Fatal("expected the cancelled scan to return an error")
	}

	row, err := st.LatestSpamRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		t.Fatal("cancelled run left no spam_runs row")
	}
	if row.InFlight() {
		t.Error("cancelled run's row still reads as in flight — terminal write was lost")
	}
	if row.Error == "" {
		t.Error("cancelled run's row carries no error text")
	}
}
