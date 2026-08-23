package store

// Coverage for the calendar's second mood source (#370): MonthSentiment and the
// generation pin it reads through. The filters here are the ones a reader cannot
// see failing — an opted-out contact still colouring a day, or two generations
// averaged into one tint, both look like a perfectly ordinary calendar.

import (
	"context"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

// insertRawScore writes a message_sentiment row DIRECTLY, bypassing
// PutSentimentBatch's opt-out guard.
//
// That bypass is the whole point. SPEC-0027 makes opt-out a deletion, and both
// the write guard and SetSentimentOptOut conspire to keep opted-out rows out of
// the table — so the only way to prove the READ also filters them is to plant a
// row the writer would have refused. That state is reachable in production: a
// contact who opts out while a scoring run is in flight can have a batch land
// against the pre-run opt-out snapshot moments later.
func insertRawScore(t *testing.T, st *Store, gen SentimentGeneration, hash, construct string, score float64, tsUnix, contactID int64) {
	t.Helper()
	if _, err := st.DB().Exec(`
INSERT INTO message_sentiment(message_hash, model, lexicon_version, construct, score, ts_unix, contact_id)
VALUES (?, ?, ?, ?, ?, ?, ?)`, hash, gen.Model, gen.LexiconVersion, construct, score, tsUnix, contactID); err != nil {
		t.Fatalf("insert raw score: %v", err)
	}
}

// TestMonthSentimentExcludesOptedOutContact is the privacy invariant: an
// opted-out contact's affect must not reach a day tint even when a score row for
// them exists.
func TestMonthSentimentExcludesOptedOutContact(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.Signal, "Harper")
	cid := contactID(t, st, conv)
	hash, _, _, tsUnix := firstMessage(t, st, conv)

	insertRawScore(t, st, genV1, hash, "Cheerfulness", 0.9, tsUnix, cid)

	// Without the marker the row is visible.
	rows, err := st.MonthSentiment(ctx, 2023, time.May, genV1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("aggregates = %d before opt-out, want 1", len(rows))
	}

	// Plant the marker without going through SetSentimentOptOut, which would
	// delete the score rows as part of the same transaction — the point here is a
	// database where the marker and the rows coexist, which is what a mid-run
	// opt-out produces.
	if _, err := st.DB().Exec(
		`INSERT INTO contact_sentiment_optout(contact_id, created_at) VALUES (?, '2023-05-02T00:00:00Z')`, cid); err != nil {
		t.Fatalf("insert opt-out marker: %v", err)
	}

	// The read drops the row even though it is still in the table, and no caller
	// had to ask for that: MonthSentiment enforces the opt-out itself.
	rows, err = st.MonthSentiment(ctx, 2023, time.May, genV1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("opted-out contact contributed %d aggregates to the day tint, want 0", len(rows))
	}
}

// TestMonthSentimentPinsGeneration: scores written under a different model or
// lexicon version are not comparable and must never be folded into the same
// tint.
func TestMonthSentimentPinsGeneration(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.Signal, "Harper")
	cid := contactID(t, st, conv)
	hash, _, _, tsUnix := firstMessage(t, st, conv)

	other := SentimentGeneration{Model: "other-model", LexiconVersion: "v1"}
	insertRawScore(t, st, genV1, hash, "Cheerfulness", 0.9, tsUnix, cid)
	insertRawScore(t, st, other, hash, "Anger", 0.9, tsUnix, cid)

	rows, err := st.MonthSentiment(ctx, 2023, time.May, genV1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Construct != "Cheerfulness" {
		t.Fatalf("aggregates = %+v, want only the pinned generation's Cheerfulness row", rows)
	}
	if rows[0].N != 1 || rows[0].Sum != 0.9 {
		t.Errorf("aggregate = {N:%d Sum:%v}, want {N:1 Sum:0.9}", rows[0].N, rows[0].Sum)
	}
}

// TestMonthSentimentHonorsExcludeDenylist: a thread named in
// journal.exclude_conversations may not colour the calendar, exactly as it may
// not inflate the stat tiles (REQ-0016-015).
func TestMonthSentimentHonorsExcludeDenylist(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	loud := seedConversation(t, st, source.Signal, "Loud Group")
	cid := contactID(t, st, loud)
	hash, _, _, tsUnix := firstMessage(t, st, loud)
	insertRawScore(t, st, genV1, hash, "Anger", 0.8, tsUnix, cid)

	rows, err := st.MonthSentiment(ctx, 2023, time.May, genV1, []string{"Loud Group"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("denylisted conversation contributed %d aggregates, want 0", len(rows))
	}
}

// TestMonthSentimentBucketsByUTCDay pins REQ-0027-009's bucketing scenario:
// scores straddling midnight UTC land in the same day buckets their messages'
// journal rollups do — no 'localtime' shift anywhere.
func TestMonthSentimentBucketsByUTCDay(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, conv, source.Signal, []signal.Message{
		msg("Harper", "2023-05-01 23:50:00", "Harper", "late", nil, nil),
		msg("Harper", "2023-05-02 00:10:00", "Harper", "early", nil, nil),
	}); err != nil {
		t.Fatal(err)
	}
	cid := contactID(t, st, conv)

	rows, err := st.DB().Query(`SELECT hash, ts_unix FROM messages WHERE conversation_id = ? ORDER BY ts_unix`, conv)
	if err != nil {
		t.Fatal(err)
	}
	type m struct {
		hash string
		ts   int64
	}
	var msgs []m
	for rows.Next() {
		var one m
		if err := rows.Scan(&one.hash, &one.ts); err != nil {
			t.Fatal(err)
		}
		msgs = append(msgs, one)
	}
	rows.Close()
	if len(msgs) != 2 {
		t.Fatalf("seeded %d messages, want 2", len(msgs))
	}
	for _, one := range msgs {
		insertRawScore(t, st, genV1, one.hash, "Cheerfulness", 0.5, one.ts, cid)
	}

	aggs, err := st.MonthSentiment(ctx, 2023, time.May, genV1, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, a := range aggs {
		got[a.Day] += a.N
	}
	if got["2023-05-01"] != 1 || got["2023-05-02"] != 1 {
		t.Errorf("UTC day buckets = %v, want one score on each of 2023-05-01 and 2023-05-02", got)
	}
}

// TestLatestSentimentGenerationTracksTheCursor: the "current" generation is the
// one the engine last advanced a cursor under, not whichever touched the newest
// message.
func TestLatestSentimentGenerationTracksTheCursor(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if _, ok, err := st.LatestSentimentGeneration(ctx); err != nil || ok {
		t.Fatalf("LatestSentimentGeneration on a virgin store = (ok %v, err %v), want (false, nil)", ok, err)
	}

	conv := seedConversation(t, st, source.Signal, "Harper")
	cid := contactID(t, st, conv)
	hash, _, _, tsUnix := firstMessage(t, st, conv)
	if err := st.PutSentimentBatch(ctx, conv, genV1, hash, []SentimentScore{
		{MessageHash: hash, Construct: "Empathy", Score: 0.4, TSUnix: tsUnix, ContactID: cid},
	}); err != nil {
		t.Fatal(err)
	}

	gen, ok, err := st.LatestSentimentGeneration(ctx)
	if err != nil || !ok {
		t.Fatalf("LatestSentimentGeneration = (ok %v, err %v), want (true, nil)", ok, err)
	}
	if gen != genV1 {
		t.Errorf("generation = %+v, want %+v", gen, genV1)
	}
}

// TestMonthSentimentIgnoresUnsetGeneration: an unpinned read returns nothing
// rather than averaging every generation in the table together.
func TestMonthSentimentIgnoresUnsetGeneration(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.Signal, "Harper")
	cid := contactID(t, st, conv)
	hash, _, _, tsUnix := firstMessage(t, st, conv)
	insertRawScore(t, st, genV1, hash, "Cheerfulness", 0.9, tsUnix, cid)

	rows, err := st.MonthSentiment(ctx, 2023, time.May, SentimentGeneration{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("unpinned read returned %d aggregates, want 0", len(rows))
	}
}
