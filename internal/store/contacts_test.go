package store

import (
	"context"
	"testing"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

// seedMergedContact builds one contact that owns TWO conversations (a Signal
// thread and an iMessage thread relinked onto the same contact — the "merged
// person" case), returning the contact id and the two conversation ids. There
// is no merge store method yet (ADR-0003 / Slice 4.5), so the test relinks with
// a raw UPDATE, exactly as the design plan prescribes.
func seedMergedContact(t *testing.T, st *Store) (cid, sigConv, imConv int64) {
	t.Helper()
	ctx := context.Background()

	sig, err := st.UpsertConversation(ctx, source.Signal, "Chelsea")
	if err != nil {
		t.Fatal(err)
	}
	withReaction := msg("Chelsea", "2022-10-22 04:17:13", "Chelsea", "look at this",
		[]signal.Attachment{{Kind: signal.KindImage, RelPath: "media/a.jpg", OriginalName: "a.jpg"}}, nil)
	withReaction.Reactions = []signal.Reaction{{Emoji: "😂", Actor: "Me"}}
	if _, err := st.ReplaceConversationMessages(ctx, sig, source.Signal, []signal.Message{
		withReaction,
		msg("Chelsea", "2022-10-22 04:18:02", signal.OwnerSender, "lol ordering one", nil, nil),
		msg("Chelsea", "2023-06-28 09:14:00", "Chelsea", "cabin booked", nil, nil),
	}); err != nil {
		t.Fatal(err)
	}
	cid = contactID(t, st, sig)

	im, err := st.UpsertConversation(ctx, source.IMessage, "ChelseaIM")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, im, source.IMessage, []signal.Message{
		msg("ChelseaIM", "2024-02-02 11:10:00", "ChelseaIM", "hi from imessage", nil, nil),
		msg("ChelseaIM", "2024-02-02 23:41:00", signal.OwnerSender, "night", nil, nil),
	}); err != nil {
		t.Fatal(err)
	}
	// Relink the iMessage thread onto the Signal contact (the merge).
	if _, err := st.DB().ExecContext(ctx, `UPDATE conversations SET contact_id = ? WHERE id = ?`, cid, im); err != nil {
		t.Fatal(err)
	}
	// Give the contact its merged handle set (signal name + two iMessage handles).
	if _, err := st.DB().ExecContext(ctx, `
INSERT OR IGNORE INTO contact_identifiers(contact_id, source, identifier) VALUES
  (?, 'signal', 'Chelsea'), (?, 'imessage', '+14155550148'), (?, 'imessage', 'chelsea@stump.rocks')`,
		cid, cid, cid); err != nil {
		t.Fatal(err)
	}
	return cid, sig, im
}

func TestGetContactByIDMergedIdentity(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	cid, _, _ := seedMergedContact(t, st)

	c, err := st.GetContactByID(ctx, cid)
	if err != nil || c == nil {
		t.Fatalf("GetContactByID = %v, %v", c, err)
	}
	if c.DisplayName != "Chelsea" {
		t.Errorf("DisplayName = %q, want Chelsea", c.DisplayName)
	}
	if len(c.Conversations) != 2 {
		t.Errorf("Conversations = %d, want 2 (merged)", len(c.Conversations))
	}
	if c.FirstTS != "2022-10-22 04:17:13" || c.LastTS != "2024-02-02 23:41:00" {
		t.Errorf("span = %q..%q, want 2022-10-22 04:17:13..2024-02-02 23:41:00", c.FirstTS, c.LastTS)
	}
	if c.SourceCount() != 2 {
		t.Errorf("SourceCount = %d, want 2 (signal + imessage merged)", c.SourceCount())
	}

	// Missing contact → (nil, nil) → handler 404s.
	miss, err := st.GetContactByID(ctx, 999999)
	if err != nil || miss != nil {
		t.Errorf("GetContactByID(missing) = %v, %v; want nil, nil", miss, err)
	}
}

func TestContactStatsAndVolume(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	cid, _, _ := seedMergedContact(t, st)

	stt, err := st.ContactStats(ctx, cid)
	if err != nil {
		t.Fatal(err)
	}
	if stt.TotalMessages != 5 {
		t.Errorf("TotalMessages = %d, want 5", stt.TotalMessages)
	}
	if stt.SentMessages != 2 || stt.ReceivedMessages != 3 {
		t.Errorf("sent/received = %d/%d, want 2/3 (Me vs others)", stt.SentMessages, stt.ReceivedMessages)
	}
	if stt.Photos != 1 {
		t.Errorf("Photos = %d, want 1", stt.Photos)
	}
	if stt.MessagesPerDay <= 0 {
		t.Errorf("MessagesPerDay = %v, want > 0", stt.MessagesPerDay)
	}

	vol, err := st.ContactMessageVolume(ctx, cid)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, b := range vol {
		got[b.Month] = b.Count
	}
	if got["2022-10"] != 2 || got["2023-06"] != 1 || got["2024-02"] != 2 {
		t.Errorf("volume = %v, want 2022-10:2 2023-06:1 2024-02:2", got)
	}

	hour, count, ok, err := st.ContactMostActiveHour(ctx, cid)
	if err != nil || !ok {
		t.Fatalf("ContactMostActiveHour ok=%v err=%v", ok, err)
	}
	if hour != 4 || count != 2 {
		t.Errorf("most active hour = %d (×%d), want 04 (×2)", hour, count)
	}

	rx, err := st.ContactTopReactions(ctx, cid, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(rx) != 1 || rx[0].Emoji != "😂" || rx[0].Count != 1 {
		t.Errorf("top reactions = %+v, want [😂 ×1]", rx)
	}
}

func TestContactFactsResolveOwnConversation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	cid, sigConv, _ := seedMergedContact(t, st)

	// A fact whose supporting message still exists → resolves to its rowid AND
	// owning conversation (for a correct cross-thread deep-link).
	hash, ts, msgID, tsUnix := firstMessage(t, st, sigConv)
	if _, err := st.PutFact(ctx, FactInput{
		ContactID: cid, Fact: "Has a brother named Sean", Category: "family",
		Source: source.Signal, SourceMessageHash: hash, SourceTS: ts, SourceTSUnix: tsUnix, Model: "m",
	}); err != nil {
		t.Fatal(err)
	}
	// A fact whose message is gone → id and conversation both 0 (render undated link-less).
	if _, err := st.PutFact(ctx, FactInput{
		ContactID: cid, Fact: "Lives in the Bay Area", Category: "location",
		Source: source.Signal, SourceMessageHash: "no-such-hash", SourceTS: "2021-09-03 00:00:00", SourceTSUnix: 1, Model: "m",
	}); err != nil {
		t.Fatal(err)
	}

	facts, err := st.ContactFacts(ctx, cid)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 2 {
		t.Fatalf("facts = %d, want 2", len(facts))
	}
	var resolved, missing *ContactFact
	for i := range facts {
		if facts[i].SourceMessageHash == hash {
			resolved = &facts[i]
		} else {
			missing = &facts[i]
		}
	}
	if resolved == nil || resolved.SourceMessageID != msgID || resolved.SourceConversationID != sigConv {
		t.Errorf("resolved fact = %+v, want msgID %d / convID %d", resolved, msgID, sigConv)
	}
	if missing == nil || missing.SourceMessageID != 0 || missing.SourceConversationID != 0 {
		t.Errorf("missing-message fact = %+v, want id 0 / conv 0", missing)
	}
}

func TestGetConversationByIDExposesContactID(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	cid, sigConv, _ := seedMergedContact(t, st)

	cs, err := st.GetConversationByID(ctx, sigConv)
	if err != nil || cs == nil {
		t.Fatalf("GetConversationByID = %v, %v", cs, err)
	}
	if cs.ContactID != cid {
		t.Errorf("ConversationSummary.ContactID = %d, want %d (drives the header profile link)", cs.ContactID, cid)
	}
}
