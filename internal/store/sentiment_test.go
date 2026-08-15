package store

import (
	"context"
	"testing"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

var genV1 = SentimentGeneration{Model: "test-model", LexiconVersion: "v1"}

func TestPutSentimentBatchIsIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.Signal, "Harper")
	cid := contactID(t, st, conv)
	hash, _, _, tsUnix := firstMessage(t, st, conv)

	scores := []SentimentScore{
		{MessageHash: hash, Construct: "Cheerfulness", Score: 0.6, TSUnix: tsUnix, ContactID: cid},
		{MessageHash: hash, Construct: "Anxiety", Score: -0.4, TSUnix: tsUnix, ContactID: cid},
	}
	for range 3 {
		if err := st.PutSentimentBatch(ctx, conv, genV1, hash, scores); err != nil {
			t.Fatalf("PutSentimentBatch: %v", err)
		}
	}

	n, err := st.CountSentimentScores(ctx, genV1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("score rows = %d after three identical batches, want 2", n)
	}
}

// TestSentimentScoresSurviveReingest is the REQ's lineage scenario: a re-ingest
// deletes and re-inserts message rows with new rowids but stable hashes, and the
// scores — keyed by hash, with no FK to messages — must still be there and still
// join.
func TestSentimentScoresSurviveReingest(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.Signal, "Harper")
	cid := contactID(t, st, conv)
	hash, _, msgIDBefore, tsUnix := firstMessage(t, st, conv)

	if err := st.PutSentimentBatch(ctx, conv, genV1, hash, []SentimentScore{
		{MessageHash: hash, Construct: "Empathy", Score: 0.5, TSUnix: tsUnix, ContactID: cid},
	}); err != nil {
		t.Fatal(err)
	}

	// Re-ingest the same conversation: rows are replaced, rowids move.
	if _, err := st.ReplaceConversationMessages(ctx, conv, source.Signal, []signal.Message{
		msg("Harper", "2023-05-01 10:00:00", "Harper", "i just adopted a dog named Biscuit", nil, nil),
		msg("Harper", "2023-05-01 10:01:00", signal.OwnerSender, "aww congrats", nil, nil),
	}); err != nil {
		t.Fatal(err)
	}

	hashAfter, _, msgIDAfter, _ := firstMessage(t, st, conv)
	if hashAfter != hash {
		t.Fatalf("message hash changed across re-ingest: %s -> %s", hash, hashAfter)
	}
	if msgIDAfter == msgIDBefore {
		t.Logf("note: rowid was stable across re-ingest (%d); the FK-less join is still what is under test", msgIDAfter)
	}

	n, err := st.CountSentimentScores(ctx, genV1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("score rows = %d after re-ingest, want 1 — re-ingest deleted derived scores", n)
	}

	// And the score still joins back to the current message row by hash.
	var joined int
	if err := st.DB().QueryRow(`
SELECT COUNT(*) FROM message_sentiment ms JOIN messages m ON m.hash = ms.message_hash
 WHERE m.conversation_id = ?`, conv).Scan(&joined); err != nil {
		t.Fatal(err)
	}
	if joined != 1 {
		t.Errorf("scores joinable to messages = %d, want 1", joined)
	}
}

func TestSentimentCursorRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.Signal, "Harper")
	hash, _, _, _ := firstMessage(t, st, conv)

	if _, _, ok, err := st.GetSentimentState(ctx, conv); err != nil || ok {
		t.Fatalf("GetSentimentState on a fresh conversation = ok %v, err %v; want ok false", ok, err)
	}

	if err := st.PutSentimentBatch(ctx, conv, genV1, hash, nil); err != nil {
		t.Fatal(err)
	}
	gotHash, gotGen, ok, err := st.GetSentimentState(ctx, conv)
	if err != nil || !ok {
		t.Fatalf("GetSentimentState = ok %v, err %v; want ok true", ok, err)
	}
	if gotHash != hash {
		t.Errorf("cursor hash = %q, want %q", gotHash, hash)
	}
	if gotGen != genV1 {
		t.Errorf("cursor generation = %+v, want %+v", gotGen, genV1)
	}
}

// TestResolveCursorRestartsWhenHashIsGone covers the "a hash that no longer
// resolves restarts the conversation from the top" half of the cursor contract.
func TestResolveCursorRestartsWhenHashIsGone(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.Signal, "Harper")
	hash, _, _, _ := firstMessage(t, st, conv)

	if _, _, ok, err := st.ResolveCursor(ctx, conv, hash); err != nil || !ok {
		t.Fatalf("ResolveCursor on a live hash = ok %v, err %v; want ok true", ok, err)
	}
	_, _, ok, err := st.ResolveCursor(ctx, conv, "0000000000000000000000000000000000000000000000000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("ResolveCursor resolved a hash that is not in the conversation; the engine would resume from nowhere")
	}
}

// TestGenerationChangeIsVisibleToCaller is the model/lexicon rescan trigger: the
// store records the generation, and a caller comparing it against the current
// one is what forces the rescan.
func TestGenerationChangeIsVisibleToCaller(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.Signal, "Harper")
	hash, _, _, _ := firstMessage(t, st, conv)

	if err := st.PutSentimentBatch(ctx, conv, genV1, hash, nil); err != nil {
		t.Fatal(err)
	}
	_, gotGen, _, err := st.GetSentimentState(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	genV2 := SentimentGeneration{Model: "test-model", LexiconVersion: "v2"}
	if gotGen == genV2 {
		t.Fatal("stored generation should differ from v2")
	}

	// Advancing under v2 replaces the stamp, so the conversation is now current.
	if err := st.PutSentimentBatch(ctx, conv, genV2, hash, nil); err != nil {
		t.Fatal(err)
	}
	if _, gotGen, _, err = st.GetSentimentState(ctx, conv); err != nil {
		t.Fatal(err)
	}
	if gotGen != genV2 {
		t.Errorf("generation after rescan = %+v, want %+v", gotGen, genV2)
	}
}

// TestScoresAreSegregatedByGeneration guards the read-side contract: two
// generations coexist and are counted separately, so a model swap cannot
// silently average incomparable scores together.
func TestScoresAreSegregatedByGeneration(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.Signal, "Harper")
	cid := contactID(t, st, conv)
	hash, _, _, tsUnix := firstMessage(t, st, conv)

	genOther := SentimentGeneration{Model: "other-model", LexiconVersion: "v1"}
	score := []SentimentScore{{MessageHash: hash, Construct: "Anger", Score: 0.9, TSUnix: tsUnix, ContactID: cid}}
	if err := st.PutSentimentBatch(ctx, conv, genV1, hash, score); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSentimentBatch(ctx, conv, genOther, hash, score); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		gen  SentimentGeneration
		want int
	}{{genV1, 1}, {genOther, 1}} {
		n, err := st.CountSentimentScores(ctx, tc.gen)
		if err != nil {
			t.Fatal(err)
		}
		if n != tc.want {
			t.Errorf("scores for %+v = %d, want %d", tc.gen, n, tc.want)
		}
	}
}

// TestOptOutDeletesRetroactively is the REQ scenario: opting a contact out
// deletes their stored scores and persists the marker, both in one transaction.
func TestOptOutDeletesRetroactively(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.Signal, "Harper")
	other := seedConversation(t, st, source.Signal, "Wren")
	cid := contactID(t, st, conv)
	otherCID := contactID(t, st, other)
	hash, _, _, tsUnix := firstMessage(t, st, conv)
	otherHash, _, _, otherTS := firstMessage(t, st, other)

	if err := st.PutSentimentBatch(ctx, conv, genV1, hash, []SentimentScore{
		{MessageHash: hash, Construct: "Anger", Score: 0.7, TSUnix: tsUnix, ContactID: cid},
		{MessageHash: hash, Construct: "Anxiety", Score: 0.3, TSUnix: tsUnix, ContactID: cid},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSentimentBatch(ctx, other, genV1, otherHash, []SentimentScore{
		{MessageHash: otherHash, Construct: "Empathy", Score: 0.8, TSUnix: otherTS, ContactID: otherCID},
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.SetSentimentOptOut(ctx, cid, true); err != nil {
		t.Fatalf("SetSentimentOptOut: %v", err)
	}

	out, err := st.IsSentimentOptedOut(ctx, cid)
	if err != nil || !out {
		t.Errorf("IsSentimentOptedOut = %v, err %v; want true", out, err)
	}
	var remaining int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM message_sentiment WHERE contact_id = ?`, cid).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Errorf("opted-out contact still has %d score rows, want 0", remaining)
	}

	// The other contact is untouched.
	var kept int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM message_sentiment WHERE contact_id = ?`, otherCID).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Errorf("bystander contact has %d score rows, want 1", kept)
	}

	optedOut, err := st.SentimentOptedOut(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := optedOut[cid]; !ok {
		t.Error("SentimentOptedOut does not include the opted-out contact")
	}
	if _, ok := optedOut[otherCID]; ok {
		t.Error("SentimentOptedOut includes a contact who never opted out")
	}
}

// TestPutSentimentBatchRefusesOptedOutContacts covers the race the caller-side
// filter cannot: a run reads the opt-out set once at the top, so a contact who
// opts out while that run is in flight would otherwise get scores written back
// moments after SetSentimentOptOut deleted them — marked opted out and scored
// anyway. The guard lives on the write, inside the same transaction.
func TestPutSentimentBatchRefusesOptedOutContacts(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.Signal, "Harper")
	other := seedConversation(t, st, source.Signal, "Wren")
	cid := contactID(t, st, conv)
	otherCID := contactID(t, st, other)
	hash, _, _, tsUnix := firstMessage(t, st, conv)

	if err := st.SetSentimentOptOut(ctx, cid, true); err != nil {
		t.Fatal(err)
	}

	// A batch computed before the opt-out lands, carrying both contacts.
	if err := st.PutSentimentBatch(ctx, conv, genV1, hash, []SentimentScore{
		{MessageHash: hash, Construct: "Anger", Score: 0.7, TSUnix: tsUnix, ContactID: cid},
		{MessageHash: hash, Construct: "Empathy", Score: 0.4, TSUnix: tsUnix, ContactID: otherCID},
	}); err != nil {
		t.Fatalf("PutSentimentBatch: %v", err)
	}

	var reinstated int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM message_sentiment WHERE contact_id = ?`, cid).Scan(&reinstated); err != nil {
		t.Fatal(err)
	}
	if reinstated != 0 {
		t.Errorf("an in-flight batch wrote %d score rows for an opted-out contact, want 0", reinstated)
	}

	// The bystander in the same batch is still written, and the cursor still
	// advanced — an opt-out must not stall the run.
	var kept int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM message_sentiment WHERE contact_id = ?`, otherCID).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Errorf("bystander score rows = %d, want 1", kept)
	}
	if gotHash, _, ok, err := st.GetSentimentState(ctx, conv); err != nil || !ok || gotHash != hash {
		t.Errorf("cursor = %q (ok %v, err %v), want it advanced to %q", gotHash, ok, err, hash)
	}
}

func TestOptOutIsReversibleAndIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.Signal, "Harper")
	cid := contactID(t, st, conv)

	for range 2 {
		if err := st.SetSentimentOptOut(ctx, cid, true); err != nil {
			t.Fatalf("opting out twice: %v", err)
		}
	}
	if out, _ := st.IsSentimentOptedOut(ctx, cid); !out {
		t.Fatal("contact is not opted out after two opt-outs")
	}
	if err := st.SetSentimentOptOut(ctx, cid, false); err != nil {
		t.Fatalf("opting back in: %v", err)
	}
	if out, _ := st.IsSentimentOptedOut(ctx, cid); out {
		t.Error("contact is still opted out after opting back in")
	}
}

// TestResetSentimentKeepsOptOuts pins the deliberate asymmetry: --reset wipes
// derived data but must never silently re-enable scoring for someone who opted
// out.
func TestResetSentimentKeepsOptOuts(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.Signal, "Harper")
	cid := contactID(t, st, conv)
	hash, _, _, tsUnix := firstMessage(t, st, conv)

	if err := st.PutSentimentBatch(ctx, conv, genV1, hash, []SentimentScore{
		{MessageHash: hash, Construct: "Calmness", Score: 0.2, TSUnix: tsUnix, ContactID: cid},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSentimentOptOut(ctx, cid, true); err != nil {
		t.Fatal(err)
	}
	if err := st.ResetSentiment(ctx); err != nil {
		t.Fatalf("ResetSentiment: %v", err)
	}

	n, err := st.CountSentimentScores(ctx, genV1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("scores after reset = %d, want 0", n)
	}
	if _, _, ok, err := st.GetSentimentState(ctx, conv); err != nil || ok {
		t.Errorf("cursor survived reset (ok=%v, err=%v)", ok, err)
	}
	if out, _ := st.IsSentimentOptedOut(ctx, cid); !out {
		t.Error("reset cleared an opt-out; scoring would silently resume for a contact who asked to be excluded")
	}
}

func TestSentimentConversationsHonorsExclude(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedConversation(t, st, source.Signal, "Harper")
	seedConversation(t, st, source.Signal, "Wren")

	all, err := st.SentimentConversations(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("conversations = %d, want 2", len(all))
	}

	filtered, err := st.SentimentConversations(ctx, []string{"Wren"})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Name != "Harper" {
		t.Errorf("excluding Wren left %+v, want only Harper", filtered)
	}
}
