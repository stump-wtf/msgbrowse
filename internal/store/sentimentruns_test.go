// The sentiment run log, coverage snapshot, and read-time opt-out guard (#367)
//
// The most important test here is TestSentimentAggregatesEnforceOptOutAtReadTime.
// SPEC-0027 makes opting out a DELETION rather than a suppression, so in a
// settled database the rows are already gone — which makes it very easy to ship
// a read path that only works because of that. The test writes the marker
// directly, leaving the score rows in place, reproducing the real race (a
// contact opting out while a long run is in flight has scores written back
// seconds later) and proving every aggregate refuses them on its own.
//
// @joestump-agent 08/23/2026 - Added with the in-app scoring controls (#367).
package store

import (
	"context"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/source"
)

// TestSentimentRunLifecycle: begin → heartbeat → finish, and the in-flight
// classification each stage produces.
func TestSentimentRunLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	start := time.Now().Add(-time.Minute)
	id, err := st.BeginSentimentRun(ctx, "test-model", "v1", SentimentScopeArchive, start)
	if err != nil {
		t.Fatalf("BeginSentimentRun: %v", err)
	}

	run, err := st.LatestSentimentRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || !run.InFlight() {
		t.Fatal("a freshly begun run does not read as in flight")
	}
	if run.Model != "test-model" || run.LexiconVersion != "v1" {
		t.Errorf("generation not recorded: %q / %q", run.Model, run.LexiconVersion)
	}

	if err := st.UpdateSentimentRunProgress(ctx, id, 3, 17, time.Now()); err != nil {
		t.Fatal(err)
	}
	run, _ = st.LatestSentimentRun(ctx)
	if run.Conversations != 3 || run.ScoresWritten != 17 {
		t.Errorf("heartbeat counters = %d/%d, want 3/17", run.Conversations, run.ScoresWritten)
	}

	if err := st.FinishSentimentRun(ctx, SentimentRun{
		ID: id, FinishedAt: time.Now(), DurationMS: 1200,
		Conversations: 4, Messages: 40, ScoresWritten: 20, Batches: 2,
	}); err != nil {
		t.Fatal(err)
	}
	run, _ = st.LatestSentimentRun(ctx)
	if run.InFlight() {
		t.Error("a finished run still reads as in flight")
	}
	if run.ScoresWritten != 20 || run.DurationMS != 1200 {
		t.Errorf("terminal totals not recorded: %+v", run)
	}
}

// TestLatestSentimentRunPrefersInFlight: an in-flight row must sort ahead of a
// newer finished one. This query IS the cross-process guard — hiding a live run
// behind a newer finished row would let a second (billable) pass start beside
// it.
func TestLatestSentimentRunPrefersInFlight(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	live, err := st.BeginSentimentRun(ctx, "m", "v1", SentimentScopeArchive, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	newer, err := st.BeginSentimentRun(ctx, "m", "v1", SentimentScopeReset, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishSentimentRun(ctx, SentimentRun{ID: newer, FinishedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	got, err := st.LatestSentimentRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != live {
		t.Fatalf("LatestSentimentRun returned %v, want the still-in-flight run %d", got, live)
	}
}

// TestRecentSentimentRunsNewestFirst also pins the n <= 0 shortcut.
func TestRecentSentimentRunsNewestFirst(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for range 3 {
		if _, err := st.BeginSentimentRun(ctx, "m", "v1", SentimentScopeArchive, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := st.RecentSentimentRuns(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	if runs[0].ID < runs[1].ID {
		t.Error("runs are not newest-first")
	}
	if got, _ := st.RecentSentimentRuns(ctx, 0); got != nil {
		t.Error("n <= 0 should yield nothing")
	}
}

// TestSentimentCoverageSeparatesProcessedFromProductive: storage is sparse by
// design, so a conversation the engine READ and found nothing salient in is a
// normal outcome — not a failure, and not the same as one it never read.
func TestSentimentCoverageSeparatesProcessedFromProductive(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	scored := seedConversation(t, st, source.Signal, "Harper")
	quiet := seedConversation(t, st, source.Signal, "Quinn")
	seedConversation(t, st, source.Signal, "Rowan") // never read at all

	cid := contactID(t, st, scored)
	hash, _, _, tsUnix := firstMessage(t, st, scored)
	if err := st.PutSentimentBatch(ctx, scored, genV1, hash, []SentimentScore{
		{MessageHash: hash, Construct: "Cheerfulness", Score: 0.5, TSUnix: tsUnix, ContactID: cid},
	}); err != nil {
		t.Fatal(err)
	}
	// A cursor with no scores: read, found nothing.
	quietHash, _, _, _ := firstMessage(t, st, quiet)
	if err := st.PutSentimentBatch(ctx, quiet, genV1, quietHash, nil); err != nil {
		t.Fatal(err)
	}

	cov, err := st.SentimentCoverage(ctx, genV1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Conversations != 3 {
		t.Errorf("eligible conversations = %d, want 3", cov.Conversations)
	}
	if cov.Processed != 2 {
		t.Errorf("processed = %d, want 2 (both cursors, including the one that found nothing)", cov.Processed)
	}
	if cov.Productive != 1 {
		t.Errorf("productive = %d, want 1", cov.Productive)
	}
	if cov.Remaining() != 1 {
		t.Errorf("remaining = %d, want 1 — the cost of the next run", cov.Remaining())
	}
	if cov.Scores != 1 || cov.Messages != 1 {
		t.Errorf("totals = %d scores / %d messages, want 1/1", cov.Scores, cov.Messages)
	}
}

// TestSentimentCoverageIgnoresOtherGenerations: a cursor stamped with an older
// model is work the next run will redo, so it must not count as processed.
func TestSentimentCoverageIgnoresOtherGenerations(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.Signal, "Harper")
	hash, _, _, _ := firstMessage(t, st, conv)
	if err := st.PutSentimentBatch(ctx, conv, genV1, hash, nil); err != nil {
		t.Fatal(err)
	}

	cov, err := st.SentimentCoverage(ctx,
		SentimentGeneration{Model: "another-model", LexiconVersion: "v1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Processed != 0 {
		t.Errorf("processed = %d under a different generation, want 0", cov.Processed)
	}
}

// TestSentimentAggregatesEnforceOptOutAtReadTime is the privacy guarantee.
//
// The marker is written DIRECTLY rather than through SetSentimentOptOut, so the
// score rows survive — reproducing a contact who opts out while a run is in
// flight and has scores written back moments later. Every read must refuse them
// on its own rather than relying on the delete having already happened.
func TestSentimentAggregatesEnforceOptOutAtReadTime(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.Signal, "Harper")
	cid := contactID(t, st, conv)
	hash, _, _, tsUnix := firstMessage(t, st, conv)

	if err := st.PutSentimentBatch(ctx, conv, genV1, hash, []SentimentScore{
		{MessageHash: hash, Construct: "Cheerfulness", Score: 0.9, TSUnix: tsUnix, ContactID: cid},
		{MessageHash: hash, Construct: "Extraversion", Score: 0.7, TSUnix: tsUnix, ContactID: cid},
	}); err != nil {
		t.Fatal(err)
	}
	// Sanity: the rows are readable BEFORE the opt-out, so a false pass below
	// cannot come from an empty fixture.
	if rows, err := st.ContactSentimentConstructs(ctx, cid, genV1); err != nil || len(rows) == 0 {
		t.Fatalf("fixture produced no readable scores (err=%v, rows=%d)", err, len(rows))
	}

	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO contact_sentiment_optout(contact_id, created_at) VALUES(?, ?)`,
		cid, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	// The rows are still physically present — that is the point of this test.
	var remaining int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM message_sentiment WHERE contact_id = ?`, cid).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining == 0 {
		t.Fatal("the fixture deleted the scores; this test must exercise the READ guard")
	}

	day := time.Unix(tsUnix, 0).UTC().Format("2006-01-02")
	for _, tc := range []struct {
		name string
		read func() (int, error)
	}{
		{"ContactSentimentMonths", func() (int, error) {
			r, err := st.ContactSentimentMonths(ctx, cid, genV1)
			return len(r), err
		}},
		{"ContactSentimentConstructs", func() (int, error) {
			r, err := st.ContactSentimentConstructs(ctx, cid, genV1)
			return len(r), err
		}},
		{"DaySentiment", func() (int, error) {
			r, err := st.DaySentiment(ctx, day, genV1, nil)
			return len(r), err
		}},
		{"ContactScoredMessages", func() (int, error) {
			return st.ContactScoredMessages(ctx, cid, genV1)
		}},
	} {
		got, err := tc.read()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != 0 {
			t.Errorf("%s returned %d rows for an opted-out contact; the guard must live "+
				"inside the query, not in the caller", tc.name, got)
		}
	}
}

// TestSentimentAggregatesPinGeneration: an unset generation reads nothing rather
// than averaging every generation in the table together.
func TestSentimentAggregatesPinGeneration(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.Signal, "Harper")
	cid := contactID(t, st, conv)
	hash, _, _, tsUnix := firstMessage(t, st, conv)
	if err := st.PutSentimentBatch(ctx, conv, genV1, hash, []SentimentScore{
		{MessageHash: hash, Construct: "Cheerfulness", Score: 0.9, TSUnix: tsUnix, ContactID: cid},
	}); err != nil {
		t.Fatal(err)
	}

	if rows, err := st.ContactSentimentMonths(ctx, cid, SentimentGeneration{}); err != nil || rows != nil {
		t.Errorf("an unset generation read %d rows (err=%v); it must read none", len(rows), err)
	}
	other := SentimentGeneration{Model: "test-model", LexiconVersion: "v2"}
	if rows, err := st.ContactSentimentMonths(ctx, cid, other); err != nil || len(rows) != 0 {
		t.Errorf("a different lexicon version read %d rows; scores are not comparable across generations", len(rows))
	}
}

// TestDaySentimentBucketsInUTC: the ADR-0023 frame. A message at 23:30 UTC on
// the 1st belongs to the 1st and nothing else — a 'localtime' conversion
// anywhere in this path would shift it into a neighbouring day.
func TestDaySentimentBucketsInUTC(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.Signal, "Harper")
	cid := contactID(t, st, conv)
	hash, _, _, _ := firstMessage(t, st, conv)

	// 2023-05-01 23:30:00 UTC.
	late := time.Date(2023, 5, 1, 23, 30, 0, 0, time.UTC).Unix()
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE messages SET ts_unix = ? WHERE hash = ?`, late, hash); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSentimentBatch(ctx, conv, genV1, hash, []SentimentScore{
		{MessageHash: hash, Construct: "Cheerfulness", Score: 0.5, TSUnix: late, ContactID: cid},
	}); err != nil {
		t.Fatal(err)
	}

	on, err := st.DaySentiment(ctx, "2023-05-01", genV1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(on) != 1 || on[0].Bucket != "2023-05-01" {
		t.Errorf("a 23:30 UTC message did not bucket into its own UTC day: %+v", on)
	}
	next, err := st.DaySentiment(ctx, "2023-05-02", genV1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 0 {
		t.Errorf("the message leaked into the next UTC day: %+v", next)
	}
	// A malformed day reads nothing rather than silently widening the window.
	if rows, err := st.DaySentiment(ctx, "not-a-day", genV1, nil); err != nil || rows != nil {
		t.Errorf("a malformed day read %d rows (err=%v)", len(rows), err)
	}
}
