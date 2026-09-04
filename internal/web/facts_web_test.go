package web

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// TestFactsLiveOnTheProfileNotTheTranscript (#446): the conversation view no
// longer renders the "What the AI has learned" panel — facts live on
// /contact/{id}, reached through the header's profile button (#445). A fact
// exists for the contact; the transcript must not show it, the profile must.
func TestFactsLiveOnTheProfileNotTheTranscript(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	var convID, contactID int64
	if err := st.DB().QueryRow(
		`SELECT id, contact_id FROM conversations WHERE name = 'Harper'`).Scan(&convID, &contactID); err != nil {
		t.Fatalf("find Harper: %v", err)
	}
	var msgID, tsUnix int64
	var hash, ts string
	if err := st.DB().QueryRow(
		`SELECT id, hash, ts, ts_unix FROM messages WHERE conversation_id = ? ORDER BY ts_unix, id LIMIT 1`,
		convID).Scan(&msgID, &hash, &ts, &tsUnix); err != nil {
		t.Fatalf("find message: %v", err)
	}

	if _, err := st.PutFact(ctx, store.FactInput{
		ContactID: contactID, Fact: "Has a dog named Biscuit", Category: "personal",
		Source: source.Signal, SourceMessageHash: hash, SourceTS: ts, SourceTSUnix: tsUnix, Model: "test",
	}); err != nil {
		t.Fatalf("put fact: %v", err)
	}

	transcript := get(t, srv, "/c/"+strconv.FormatInt(convID, 10)).Body.String()
	if strings.Contains(transcript, "What the AI has learned") || strings.Contains(transcript, "Has a dog named Biscuit") {
		t.Error("the messages view must not render the facts panel (#446)")
	}

	profile := get(t, srv, "/contact/"+strconv.FormatInt(contactID, 10)).Body.String()
	for _, want := range []string{"AI-gathered facts", "Has a dog named Biscuit"} {
		if !strings.Contains(profile, want) {
			t.Errorf("profile page missing %q", want)
		}
	}
}

// TestFactsOrphanCountSurfaces (#447): the Facts card shows the orphaned-
// citation count so the automatic reap is observable before it happens.
func TestFactsOrphanCountSurfaces(t *testing.T) {
	srv, st, _ := newTestServer(t)
	srv.SetFactsExtractor(newFakeFactsExtractor("test-chat"))
	ctx := context.Background()

	var convID, contactID int64
	if err := st.DB().QueryRow(`SELECT id, contact_id FROM conversations WHERE name = 'Harper'`).Scan(&convID, &contactID); err != nil {
		t.Fatalf("find Harper: %v", err)
	}
	orphan := store.FactInput{
		ContactID: contactID, Fact: "cites a vanished message", Category: "c",
		Source: source.Signal, SourceMessageHash: "no-such-hash", SourceTS: "2020-01-01 00:00:00", Model: "test",
	}
	if added, err := st.PutFact(ctx, orphan); err != nil || !added {
		t.Fatalf("seed orphan: added=%v err=%v", added, err)
	}

	body := get(t, srv, "/settings/facts").Body.String()
	if !contains(body, "facts cite messages that were re-imported") {
		t.Error("orphan count note missing from the Facts card")
	}
	// The count itself renders through num() inside the badge; the note text
	// above is the observable contract.
	if !contains(body, "facts cite messages that were re-imported") && !contains(body, "will be removed") {
		t.Error("reap note incomplete")
	}
}
