package web

// Tests for the conversation header's profile affordance (#445): the title is
// plain text (no hover-underline link), the route to the profile is an icon
// button rendered exactly once for a linked thread and never for an unlinked
// one, and the contact page mirrors it with "Open messages".
//
// @joestump-agent 09/04/2026 - Added with #445.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

func TestConversationHeaderProfileButton(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv := fixtureConversation(t, st, "Harper")

	body := get(t, srv, "/c/"+itoa(conv.ID)).Body.String()
	if strings.Contains(body, `class="link link-hover"`) && strings.Contains(body, "conv-title") {
		// Cheap structural check: the title anchor must not carry the hover
		// underline classes. (The contact-page sidebar may use link classes
		// elsewhere, so scope this to the h1 region.)
		h1 := body[strings.Index(body, `<h1 class="conv-title"`):]
		h1 = h1[:strings.Index(h1, "</h1>")]
		if strings.Contains(h1, "link link-hover") {
			t.Error("the conversation title must be plain text, no hover underline")
		}
	}
	// Exactly one profile affordance in the header for a linked thread.
	if got := strings.Count(body, `title="Profile — what the AI has learned"`); got != 1 {
		t.Fatalf("profile button count = %d, want exactly 1", got)
	}
	if !contains(body, `href="/contact/`) {
		t.Error("profile button missing its /contact/ href")
	}
}

func TestConversationHeaderNoProfileButtonUnlinked(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	// A conversation that never links to a contact: unlink it directly, the
	// same state a group or an unlinked thread renders in.
	id, err := st.UpsertConversation(ctx, source.Signal, "Unlinked Thread")
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := time.Parse(signal.TimestampLayout, "2023-05-01 10:00:00")
	if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal, []signal.Message{{
		Conversation: "Unlinked Thread", Timestamp: ts, TimestampRaw: ts.Format(signal.TimestampLayout),
		Sender: "Unlinked Thread", Body: "unlinked body",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE conversations SET contact_id = NULL WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	body := get(t, srv, "/c/"+itoa(id)).Body.String()
	if strings.Contains(body, `title="Profile — what the AI has learned"`) {
		t.Error("unlinked thread must render no profile button")
	}
}

func TestContactPageOpenMessagesMirror(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv := fixtureConversation(t, st, "Harper")

	var contactID int64
	if err := st.DB().QueryRow(`SELECT contact_id FROM conversations WHERE id = ?`, conv.ID).Scan(&contactID); err != nil {
		t.Fatal(err)
	}
	body := get(t, srv, "/contact/"+itoa(contactID)).Body.String()
	if !contains(body, "Open messages") || !contains(body, `href="/c/`+itoa(conv.ID)+`"`) {
		t.Error("contact page missing the Open-messages mirror link to the primary conversation")
	}
}
