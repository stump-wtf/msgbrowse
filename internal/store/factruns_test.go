// Fact-run bookkeeping and coverage (#366)
//
// The run log is the cross-process guard between `msgbrowse facts` and a
// running `msgbrowse serve`, so the properties tested here are the ones the
// guard leans on: an in-flight row is found in preference to a newer finished
// one, a terminal write flips the row out of flight, and coverage counts
// EXAMINED conversations separately from productive ones.
//
// @joestump-agent 08/23/2026 - Added with the in-app extraction controls (#366).
package store

import (
	"context"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/source"
)

func TestFactRunLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if run, err := st.LatestFactRun(ctx); err != nil || run != nil {
		t.Fatalf("LatestFactRun on a fresh store = %v, %v; want nil, nil", run, err)
	}
	if runs, err := st.RecentFactRuns(ctx, 5); err != nil || len(runs) != 0 {
		t.Fatalf("RecentFactRuns on a fresh store = %d rows, %v; want 0, nil", len(runs), err)
	}

	start := time.Now().Add(-2 * time.Minute)
	id, err := st.BeginFactRun(ctx, "test-chat", FactScopeArchive, start)
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.LatestFactRun(ctx)
	if err != nil || run == nil {
		t.Fatalf("LatestFactRun after begin = %v, %v", run, err)
	}
	if !run.InFlight() {
		t.Error("a run with no terminal write must read as in flight")
	}
	if run.Model != "test-chat" || run.Scope != FactScopeArchive {
		t.Errorf("run = %+v, want model test-chat and the archive scope", run)
	}

	// The heartbeat carries live counters and moves updated_at.
	beat := time.Now().Add(-time.Minute)
	if err := st.UpdateFactRunProgress(ctx, id, 7, 12, beat); err != nil {
		t.Fatal(err)
	}
	run, _ = st.LatestFactRun(ctx)
	if run.Conversations != 7 || run.FactsAdded != 12 {
		t.Errorf("heartbeat counters = %d/%d, want 7/12", run.Conversations, run.FactsAdded)
	}
	if !run.UpdatedAt.After(run.StartedAt) {
		t.Error("the heartbeat must move updated_at past started_at")
	}

	// An in-flight run outranks a NEWER finished one — the guard depends on it,
	// because hiding a live run means letting a second billable one start.
	newer, err := st.BeginFactRun(ctx, "test-chat", FactScopeReset, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishFactRun(ctx, FactRun{ID: newer, FinishedAt: time.Now(), DurationMS: 10}); err != nil {
		t.Fatal(err)
	}
	run, _ = st.LatestFactRun(ctx)
	if run.ID != id {
		t.Errorf("LatestFactRun = row %d, want the still-in-flight row %d", run.ID, id)
	}

	// The terminal write flips it out of flight and records the totals.
	fin := time.Now()
	if err := st.FinishFactRun(ctx, FactRun{
		ID: id, FinishedAt: fin, DurationMS: 4200,
		Conversations: 9, Messages: 340, FactsAdded: 15, Batches: 6, Error: "endpoint refused",
	}); err != nil {
		t.Fatal(err)
	}
	// With nothing in flight the latest row is simply the newest, so read this
	// run back out of the history rather than assuming it wins that race.
	runs, err := st.RecentFactRuns(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	var finished *FactRun
	for i := range runs {
		if runs[i].ID == id {
			finished = &runs[i]
		}
	}
	if finished == nil {
		t.Fatalf("run %d missing from the history", id)
	}
	if finished.InFlight() {
		t.Error("a finished run still reads as in flight")
	}
	if finished.DurationMS != 4200 || finished.Messages != 340 || finished.Batches != 6 ||
		finished.Error != "endpoint refused" {
		t.Errorf("terminal totals not recorded: %+v", finished)
	}
	if latest, lerr := st.LatestFactRun(ctx); lerr != nil || latest == nil || latest.InFlight() {
		t.Errorf("with nothing in flight LatestFactRun = %v, %v; want a finished row", latest, lerr)
	}
	if len(runs) != 2 {
		t.Fatalf("RecentFactRuns = %d rows, want 2", len(runs))
	}
	if runs[0].ID < runs[1].ID {
		t.Error("RecentFactRuns must be newest first")
	}
	if runs, err := st.RecentFactRuns(ctx, 0); err != nil || runs != nil {
		t.Errorf("RecentFactRuns(0) = %v, %v; want nil, nil without touching the database", runs, err)
	}
}

func TestFactCoverageCountsExaminedAndProductive(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	harper := seedConversation(t, st, source.Signal, "Harper")
	quinn := seedConversation(t, st, source.Signal, "Quinn")
	seedConversation(t, st, source.Signal, "Note to Self")

	cov, err := st.FactCoverage(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Conversations != 3 {
		t.Fatalf("eligible conversations = %d, want 3", cov.Conversations)
	}
	if cov.Processed != 0 || cov.Facts != 0 {
		t.Errorf("a never-extracted archive reports processed=%d facts=%d, want 0/0", cov.Processed, cov.Facts)
	}
	if cov.Remaining() != 3 {
		t.Errorf("Remaining = %d, want 3", cov.Remaining())
	}

	// Harper was examined AND produced a fact; Quinn was examined and produced
	// nothing. Both are "processed"; only Harper is "productive". Collapsing the
	// two is the ambiguity #366 exists to remove.
	hHash, hTS, _, hTSUnix := firstMessage(t, st, harper)
	if err := st.SetFactState(ctx, harper, hHash, "test-chat", 1); err != nil {
		t.Fatal(err)
	}
	qHash, _, _, _ := firstMessage(t, st, quinn)
	if err := st.SetFactState(ctx, quinn, qHash, "test-chat", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutFact(ctx, FactInput{
		ContactID: contactID(t, st, harper), Fact: "Has a dog named Biscuit", Category: "personal",
		Source: source.Signal, SourceMessageHash: hHash, SourceTS: hTS, SourceTSUnix: hTSUnix,
		Model: "test-chat",
	}); err != nil {
		t.Fatal(err)
	}

	cov, err = st.FactCoverage(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Processed != 2 || cov.Productive != 1 {
		t.Errorf("processed/productive = %d/%d, want 2/1", cov.Processed, cov.Productive)
	}
	if cov.Facts != 1 || cov.Contacts != 1 {
		t.Errorf("facts/contacts = %d/%d, want 1/1", cov.Facts, cov.Contacts)
	}
	if cov.Remaining() != 1 {
		t.Errorf("Remaining = %d, want 1 (only the unexamined conversation)", cov.Remaining())
	}

	// The exclude list must bound the DENOMINATOR too (REQ-0005-005): an
	// excluded thread is never read, so counting it would leave coverage stuck
	// below 100% forever while the user paid for runs chasing it.
	cov, err = st.FactCoverage(ctx, []string{"Note to Self"})
	if err != nil {
		t.Fatal(err)
	}
	if cov.Conversations != 2 {
		t.Errorf("excluded conversation still counted: %d eligible, want 2", cov.Conversations)
	}
	if cov.Remaining() != 0 {
		t.Errorf("Remaining = %d, want 0 once the only unexamined thread is excluded", cov.Remaining())
	}
}

func TestContactFactScanSeparatesNeverScannedFromEmpty(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.Signal, "Harper")
	cid := contactID(t, st, conv)

	scan, err := st.ContactFactScan(ctx, cid)
	if err != nil {
		t.Fatal(err)
	}
	if scan.Total != 1 || scan.Scanned != 0 || scan.Extracted() {
		t.Fatalf("never-scanned contact = %+v, extracted=%v; want 1/0/false", scan, scan.Extracted())
	}

	hash, _, _, _ := firstMessage(t, st, conv)
	if err := st.SetFactState(ctx, conv, hash, "test-chat", 0); err != nil {
		t.Fatal(err)
	}
	scan, err = st.ContactFactScan(ctx, cid)
	if err != nil {
		t.Fatal(err)
	}
	if scan.Scanned != 1 || !scan.Extracted() {
		t.Errorf("scanned contact = %+v, extracted=%v; want scanned=1/true", scan, scan.Extracted())
	}
}
