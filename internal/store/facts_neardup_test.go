package store

// Tests for the insert-time paraphrase collapse (issue #449): "Owns a Google
// Pixel 3 smartphone" and "Owns a Pixel 3 phone" are one fact; "Has a dog
// named Biscuit" and "Has a cat named Biscuit" are not; the boundary is
// per contact AND per category.
//
// @joestump-agent 09/04/2026 - Added with #449.

import (
	"context"
	"testing"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

func neardupFixture(t *testing.T, st *Store) (convID, contactID int64) {
	t.Helper()
	ctx := context.Background()
	sig, err := st.UpsertConversation(ctx, source.Signal, "Alex")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, sig, source.Signal, []signal.Message{
		msg("Alex", "2023-05-01 10:00:00", "Alex", "i got a pixel 3", nil, nil),
	}); err != nil {
		t.Fatal(err)
	}
	return sig, contactIDValue(t, st, sig)
}

func TestPutFactNearDupCollapsesParaphrase(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	_, contactID := neardupFixture(t, st)

	in := FactInput{ContactID: contactID, Fact: "Owns a Google Pixel 3 smartphone", Category: "possessions",
		Source: source.Signal, SourceMessageHash: "h1", SourceTS: "2023-05-01 10:00:00", SourceTSUnix: 1, Model: "test"}
	if added, err := st.PutFactNearDupAware(ctx, in); err != nil || !added {
		t.Fatalf("first put: added=%v err=%v", added, err)
	}

	// A paraphrase in the same category: collapsed, not stored.
	para := in
	para.Fact = "Owns a Pixel 3 phone"
	if added, err := st.PutFactNearDupAware(ctx, para); err != nil || added {
		t.Fatalf("paraphrase stored: added=%v err=%v", added, err)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM contact_facts WHERE contact_id = ?`, contactID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("stored %d facts, want 1 after collapse", n)
	}
}

func TestPutFactNearDupKeepsDifferentSubjects(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	_, contactID := neardupFixture(t, st)

	base := FactInput{ContactID: contactID, Category: "personal",
		Source: source.Signal, SourceMessageHash: "h1", SourceTS: "2023-05-01 10:00:00", SourceTSUnix: 1, Model: "test"}
	if added, err := st.PutFactNearDupAware(ctx, withFact(base, "Has a dog named Biscuit")); err != nil || !added {
		t.Fatalf("first put: added=%v err=%v", added, err)
	}
	// "cat named Biscuit" shares most tokens but is a different fact.
	if added, err := st.PutFactNearDupAware(ctx, withFact(base, "Has a cat named Biscuit")); err != nil {
		t.Fatal(err)
	} else if !added {
		t.Fatal("different-animal fact wrongly collapsed")
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM contact_facts WHERE contact_id = ?`, contactID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("stored %d facts, want 2", n)
	}
}

func TestPutFactNearDupCategoryBoundary(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	_, contactID := neardupFixture(t, st)

	in := FactInput{ContactID: contactID, Fact: "Owns a Google Pixel 3 smartphone", Category: "possessions",
		Source: source.Signal, SourceMessageHash: "h1", SourceTS: "2023-05-01 10:00:00", SourceTSUnix: 1, Model: "test"}
	if added, err := st.PutFactNearDupAware(ctx, in); err != nil || !added {
		t.Fatal(err)
	}
	// The compare set is per category: a near-duplicate in another category
	// does not block insertion here. (Exact same text is still suppressed by
	// PutFact's fact_hash dedup — pre-existing contract.)
	other := in
	other.Fact = "Owns a Pixel 3 phone"
	other.Category = "technology"
	if added, err := st.PutFactNearDupAware(ctx, other); err != nil || !added {
		t.Fatalf("near-dup in another category must not block: added=%v err=%v", added, err)
	}
}

func TestPutFactNearDupRefreshesCitation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	_, contactID := neardupFixture(t, st)

	first := FactInput{ContactID: contactID, Fact: "Owns a Google Pixel 3 smartphone", Category: "possessions",
		Source: source.Signal, SourceMessageHash: "h1", SourceTS: "2023-05-01 10:00:00", SourceTSUnix: 1000, Model: "test"}
	if added, err := st.PutFactNearDupAware(ctx, first); err != nil || !added {
		t.Fatal(err)
	}
	// Newer evidence for the same subject: the existing row's citation moves
	// forward instead of a second row appearing.
	newer := first
	newer.Fact = "Owns a Pixel 3 phone"
	newer.SourceMessageHash = "h2"
	newer.SourceTS = "2023-06-01 10:00:00"
	newer.SourceTSUnix = 2000
	if added, err := st.PutFactNearDupAware(ctx, newer); err != nil || added {
		t.Fatalf("refresh put: added=%v err=%v", added, err)
	}
	var hash string
	if err := st.DB().QueryRow(
		`SELECT source_message_hash FROM contact_facts WHERE contact_id = ?`, contactID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash != "h2" {
		t.Errorf("citation not refreshed: %q, want h2", hash)
	}
}

func withFact(in FactInput, fact string) FactInput {
	in.Fact = fact
	return in
}

// contactIDValue reads the contact linked to a conversation.
func contactIDValue(t *testing.T, st *Store, convID int64) int64 {
	t.Helper()
	var id int64
	if err := st.DB().QueryRow(`SELECT contact_id FROM conversations WHERE id = ?`, convID).Scan(&id); err != nil || id == 0 {
		t.Fatalf("contact for conv %d: %v", convID, err)
	}
	return id
}
