package store

import (
	"context"
	"testing"

	"github.com/joestump/msgbrowse/internal/source"
)

func seedSpamThread(t *testing.T, st *Store, name string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.IMessage, name)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSpamConversationsSkipsGroupsAndExcluded(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	one := seedConversation(t, st, source.IMessage, "+15551110001")
	two := seedConversation(t, st, source.IMessage, "+15551110002")
	if _, err := st.DB().ExecContext(ctx, `UPDATE conversations SET is_group = 1 WHERE id = ?`, two); err != nil {
		t.Fatal(err)
	}
	// A conversation with no real messages is not eligible either.
	seedSpamThread(t, st, "+15551110003")

	got, err := st.SpamConversations(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != one {
		t.Fatalf("conversations = %+v, want just %d", got, one)
	}

	got, err = st.SpamConversations(ctx, []string{"+15551110001"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("exclude list ignored: %+v", got)
	}
}

func TestPutSpamBatchIsIdempotentAndAdvancesTheCursor(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.IMessage, "+15551110001")

	sender := SpamSender{Source: source.IMessage, Identifier: "+15551110001",
		ConversationName: "+15551110001", FirstSeenUnix: 100, LastSeenUnix: 200}
	findings := []SpamFinding{{
		MessageHash: "hash-a", Source: source.IMessage, Identifier: "+15551110001",
		Direction: SpamInbound, TSUnix: 100, Reasons: []string{"url"}, IsCandidate: true,
	}}

	for range 2 {
		if err := st.PutSpamBatch(ctx, conv, "v1", "hash-a", sender, findings, nil); err != nil {
			t.Fatal(err)
		}
	}

	msgs, err := st.SpamMessages(ctx, source.IMessage, "+15551110001", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("findings duplicated: %d", len(msgs))
	}
	// The message hash is invented, so nothing joins — the row must still come
	// back, marked absent, rather than vanishing.
	if msgs[0].Present {
		t.Error("a finding with no matching message reported Present")
	}
	if len(msgs[0].Reasons) != 1 || msgs[0].Reasons[0] != "url" {
		t.Errorf("reasons = %q", msgs[0].Reasons)
	}

	lastHash, version, ok, err := st.GetSpamState(ctx, conv)
	if err != nil || !ok {
		t.Fatalf("cursor missing: ok=%v err=%v", ok, err)
	}
	if lastHash != "hash-a" || version != "v1" {
		t.Errorf("cursor = %q/%q", lastHash, version)
	}
}

// A scan may promote seen → watch. It must never overwrite a status, entity,
// consent record, or note a person set by hand.
func TestSpamSenderUpsertNeverClobbersHumanColumns(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.IMessage, "+15551110001")
	sender := SpamSender{Source: source.IMessage, Identifier: "+15551110001", FirstSeenUnix: 200, LastSeenUnix: 200}

	if err := st.PutSpamBatch(ctx, conv, "v1", "h1", sender, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetSpamSender(ctx, source.IMessage, "+15551110001")
	if got.Status != SpamStatusSeen {
		t.Fatalf("initial status = %q", got.Status)
	}

	ignored, notes := SpamStatusIgnored, "this is my dentist"
	if err := st.SetSpamSenderFields(ctx, source.IMessage, "+15551110001", &ignored, nil, nil, nil, &notes); err != nil {
		t.Fatal(err)
	}

	// A later scan sees a candidate. It must NOT lift 'ignored' back to 'watch'.
	candidate := []SpamFinding{{MessageHash: "h2", Source: source.IMessage,
		Identifier: "+15551110001", Direction: SpamInbound, TSUnix: 100, IsCandidate: true}}
	if err := st.PutSpamBatch(ctx, conv, "v1", "h2", sender, candidate, nil); err != nil {
		t.Fatal(err)
	}

	got, _ = st.GetSpamSender(ctx, source.IMessage, "+15551110001")
	if got.Status != SpamStatusIgnored {
		t.Errorf("scan overwrote a hand-set status: %q", got.Status)
	}
	if got.Notes != notes {
		t.Errorf("scan overwrote notes: %q", got.Notes)
	}
	// The seen window widens in both directions across batches.
	if got.FirstSeenUnix != 200 || got.LastSeenUnix != 200 {
		t.Errorf("seen window = %d..%d", got.FirstSeenUnix, got.LastSeenUnix)
	}
}

func TestRecomputeSpamAfterOptOutUsesTheEarliestOptOut(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.IMessage, "+15551110001")
	sender := SpamSender{Source: source.IMessage, Identifier: "+15551110001"}

	findings := []SpamFinding{
		{MessageHash: "before", Source: source.IMessage, Identifier: "+15551110001", Direction: SpamInbound, TSUnix: 100},
		{MessageHash: "after", Source: source.IMessage, Identifier: "+15551110001", Direction: SpamInbound, TSUnix: 300},
		{MessageHash: "mine", Source: source.IMessage, Identifier: "+15551110001", Direction: SpamOutbound, TSUnix: 400},
	}
	if err := st.PutSpamBatch(ctx, conv, "v1", "mine", sender, findings, nil); err != nil {
		t.Fatal(err)
	}
	for _, at := range []int64{200, 250} {
		if err := st.AddSpamEvent(ctx, SpamEvent{Source: source.IMessage, Identifier: "+15551110001",
			EventType: "stop_sent", EventAt: "x", EventAtUnix: at, Origin: "manual"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RecomputeSpamAfterOptOut(ctx, "v1"); err != nil {
		t.Fatal(err)
	}

	flags := map[string]bool{}
	msgs, _ := st.SpamMessages(ctx, source.IMessage, "+15551110001", "v1")
	for _, m := range msgs {
		flags[m.MessageHash] = m.IsAfterOptOut
	}
	if flags["before"] {
		t.Error("a message before the opt-out was flagged")
	}
	if !flags["after"] {
		t.Error("a message after the earliest opt-out was not flagged")
	}
	if flags["mine"] {
		t.Error("an outbound message was flagged")
	}

	at, ok, err := st.SpamOptOutAt(ctx, source.IMessage, "+15551110001")
	if err != nil || !ok || at != 200 {
		t.Errorf("SpamOptOutAt = %d, %v, %v — want the EARLIEST (200)", at, ok, err)
	}
}

func TestSpamCountsGroupPerSenderAndGeneration(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.IMessage, "+15551110001")
	sender := SpamSender{Source: source.IMessage, Identifier: "+15551110001"}
	if err := st.PutSpamBatch(ctx, conv, "v1", "b", sender, []SpamFinding{
		{MessageHash: "a", Source: source.IMessage, Identifier: "+15551110001", Direction: SpamInbound, TSUnix: 1, IsCandidate: true},
		{MessageHash: "b", Source: source.IMessage, Identifier: "+15551110001", Direction: SpamOutbound, TSUnix: 2},
	}, nil); err != nil {
		t.Fatal(err)
	}

	counts, err := st.SpamCounts(ctx, "v1")
	if err != nil {
		t.Fatal(err)
	}
	c := counts[SpamCountsKey(source.IMessage, "+15551110001")]
	if c.Inbound != 1 || c.Outbound != 1 || c.Candidates != 1 {
		t.Errorf("counts = %+v", c)
	}
	if other, err := st.SpamCounts(ctx, "v2"); err != nil || len(other) != 0 {
		t.Errorf("a different generation saw these rows: %+v (%v)", other, err)
	}
}

func TestSetSpamSenderFieldsOnUnknownSenderErrors(t *testing.T) {
	st := newTestStore(t)
	status := SpamStatusTracked
	err := st.SetSpamSenderFields(context.Background(), source.IMessage, "+19995550000", &status, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected an error naming the missing sender")
	}
}

// A sender's first/last-seen window is the record of when they CONTACTED you,
// and two writes that carry no contact information must not narrow or move it.
//
// Both paths reach the same upsert with a zero window. A scan batch made
// entirely of system lines yields first = last = 0, and AddSpamEvent passes the
// event's own timestamp for a sender it may never have scanned. Neither is a
// message, so neither may edit the window.
func TestSpamSenderWindowSurvivesWritesThatCarryNoMessages(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv := seedConversation(t, st, source.IMessage, "+15551110001")

	const (
		firstContact = 1_600_000_000 // 2020-09-13
		lastContact  = 1_600_086_400 // a day later
	)
	sender := SpamSender{Source: source.IMessage, Identifier: "+15551110001",
		ConversationName: "+15551110001", FirstSeenUnix: firstContact, LastSeenUnix: lastContact}
	findings := []SpamFinding{{
		MessageHash: "hash-a", Source: source.IMessage, Identifier: "+15551110001",
		Direction: SpamInbound, TSUnix: firstContact, IsCandidate: true,
	}}
	if err := st.PutSpamBatch(ctx, conv, "v1", "hash-a", sender, findings, nil); err != nil {
		t.Fatal(err)
	}

	// A later batch holding only system lines: real thread, no scannable
	// messages, so the scan has nothing to say about the window.
	empty := SpamSender{Source: source.IMessage, Identifier: "+15551110001",
		ConversationName: "+15551110001", FirstSeenUnix: 0, LastSeenUnix: 0}
	if err := st.PutSpamBatch(ctx, conv, "v1", "hash-a", empty, nil, nil); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetSpamSender(ctx, source.IMessage, "+15551110001")
	if err != nil {
		t.Fatal(err)
	}
	if got.FirstSeenUnix != firstContact {
		t.Errorf("a message-less batch moved first_seen_unix: got %d, want %d", got.FirstSeenUnix, firstContact)
	}
	if got.LastSeenUnix != lastContact {
		t.Errorf("a message-less batch moved last_seen_unix: got %d, want %d", got.LastSeenUnix, lastContact)
	}

	// Filing a complaint years later is not the sender contacting you, so it
	// must not become their "last seen" date in a dossier.
	const complaintAt = 1_750_000_000 // 2025-06-15
	if err := st.AddSpamEvent(ctx, SpamEvent{
		Source: source.IMessage, Identifier: "+15551110001",
		EventType: "fcc_complaint", EventAt: "2025-06-15T12:00:00Z",
		EventAtUnix: complaintAt, Details: "ticket 12345", Origin: "manual",
	}); err != nil {
		t.Fatal(err)
	}

	got, err = st.GetSpamSender(ctx, source.IMessage, "+15551110001")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastSeenUnix != lastContact {
		t.Errorf("filing an event moved last_seen_unix to the event date: got %d, want %d", got.LastSeenUnix, lastContact)
	}
	if got.FirstSeenUnix != firstContact {
		t.Errorf("filing an event moved first_seen_unix: got %d, want %d", got.FirstSeenUnix, firstContact)
	}
}
