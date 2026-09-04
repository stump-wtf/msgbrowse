package store

// Tests for orphaned-fact reaping (issue #447): a re-import invalidates fact
// citations (hashes change), and 51% of the live archive's facts pointed at
// messages that no longer exist. ReapOrphanFacts deletes exactly those.
//
// @joestump-agent 09/04/2026 - Added with #447.

import (
	"context"
	"testing"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

func TestReapOrphanFacts(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sig, err := st.UpsertConversation(ctx, source.Signal, "Alex")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, sig, source.Signal, []signal.Message{
		msg("Alex", "2023-05-01 10:00:00", "Alex", "first", nil, nil),
		msg("Alex", "2023-05-01 10:01:00", signal.OwnerSender, "second", nil, nil),
	}); err != nil {
		t.Fatal(err)
	}
	contactID := contactID(t, st, sig)

	var liveHash, goneHash string
	if err := st.DB().QueryRow(`SELECT hash FROM messages WHERE conversation_id = ? ORDER BY ts_unix ASC LIMIT 1`, sig).Scan(&goneHash); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT hash FROM messages WHERE conversation_id = ? ORDER BY ts_unix DESC LIMIT 1`, sig).Scan(&liveHash); err != nil {
		t.Fatal(err)
	}

	// One fact citing a LIVE message, one citing a message about to vanish.
	in := FactInput{ContactID: contactID, Fact: "survives", Category: "c", Source: source.Signal, SourceMessageHash: liveHash, SourceTS: "2023-05-01 10:00:00", Model: "test"}
	if added, err := st.PutFact(ctx, in); err != nil || !added {
		t.Fatalf("put live-cited fact: added=%v err=%v", added, err)
	}
	in2 := FactInput{ContactID: contactID, Fact: "orphan", Category: "c", Source: source.Signal, SourceMessageHash: goneHash, SourceTS: "2023-05-01 10:01:00", Model: "test"}
	if added, err := st.PutFact(ctx, in2); err != nil || !added {
		t.Fatalf("put orphan fact: added=%v err=%v", added, err)
	}

	// Delete the one message: its fact is now orphaned.
	if _, err := st.DB().Exec(`DELETE FROM messages WHERE hash = ?`, goneHash); err != nil {
		t.Fatal(err)
	}

	n, err := st.CountOrphanFacts(ctx)
	if err != nil || n != 1 {
		t.Fatalf("CountOrphanFacts = %d err %v, want 1", n, err)
	}

	reaped, err := st.ReapOrphanFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 1 {
		t.Fatalf("reaped %d, want 1", reaped)
	}

	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM contact_facts WHERE fact = 'orphan'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("orphaned fact survived the reap")
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM contact_facts WHERE fact = 'survives'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("live-cited fact was reaped")
	}

	// Idempotent: a second reap reports zero.
	if reaped, err = st.ReapOrphanFacts(ctx); err != nil || reaped != 0 {
		t.Fatalf("second reap = %d err %v, want 0 nil", reaped, err)
	}
}
