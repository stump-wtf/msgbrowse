package store

// Tests for the handle-named contact repair (issue #444): the sidebar must
// show what the transcript already shows.
//
// @joestump-agent 09/04/2026 - Added with #444.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

// repairNamesFixture seeds a conversation whose contact was minted from the
// handle "+14252413911" and whose messages carry the real sender name
// "Graham Blache" (owner messages included, mirroring the live DB).
func repairNamesFixture(t *testing.T, st *Store) (convID, contactID int64) {
	t.Helper()
	ctx := context.Background()
	var err error
	convID, err = st.UpsertConversation(ctx, source.IMessage, "+14252413911")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := time.Parse(signal.TimestampLayout, "2023-05-01 10:00:00")
	msgs := make([]signal.Message, 0, 3)
	for i, sender := range []string{"Graham Blache", signal.OwnerSender, "Graham Blache"} {
		ts := base.Add(time.Duration(i) * time.Minute)
		msgs = append(msgs, signal.Message{
			Conversation: "+14252413911", Timestamp: ts,
			TimestampRaw: ts.Format(signal.TimestampLayout),
			Sender:       sender, Body: "repair fixture body",
		})
	}
	if _, err := st.ReplaceConversationMessages(ctx, convID, source.IMessage, msgs); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT contact_id FROM conversations WHERE id = ?`, convID).Scan(&contactID); err != nil {
		t.Fatal(err)
	}
	if contactID == 0 {
		t.Fatal("fixture contact was not linked")
	}
	// Force the handle-named state: display_name byte-equals an identifier.
	if _, err := st.DB().Exec(`UPDATE contacts SET display_name = '+14252413911' WHERE id = ?`, contactID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE contact_identifiers SET identifier = '+14252413911' WHERE contact_id = ?`, contactID); err != nil {
		t.Fatal(err)
	}
	return convID, contactID
}

func TestNameRepairRenamesHandleNamedContact(t *testing.T) {
	st := repairTestStore(t)
	ctx := context.Background()
	_, contactID := repairNamesFixture(t, st)

	report, err := st.RepairHandleNamedContacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Renamed != 1 || report.Scanned < 1 {
		t.Fatalf("report = %+v, want exactly one rename", report)
	}
	var name string
	if err := st.DB().QueryRow(`SELECT display_name FROM contacts WHERE id = ?`, contactID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Graham Blache" {
		t.Fatalf("display_name = %q, want Graham Blache", name)
	}
	if len(report.Renames) != 1 || !strings.Contains(report.Renames[0], "Graham Blache") {
		t.Errorf("renames = %v", report.Renames)
	}
}

func TestNameRepairIsIdempotentAndKeepsHumanNames(t *testing.T) {
	st := repairTestStore(t)
	ctx := context.Background()
	_, contactID := repairNamesFixture(t, st)

	if _, err := st.RepairHandleNamedContacts(ctx); err != nil {
		t.Fatal(err)
	}
	// A human-set name (anything that is not the handle) must be untouched —
	// simulate Joe editing the contact after the first pass.
	if _, err := st.DB().Exec(`UPDATE contacts SET display_name = 'Graham B.' WHERE id = ?`, contactID); err != nil {
		t.Fatal(err)
	}
	report, err := st.RepairHandleNamedContacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	if err := st.DB().QueryRow(`SELECT display_name FROM contacts WHERE id = ?`, contactID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Graham B." {
		t.Errorf("human-set name overwritten: %q", name)
	}
	if report.Renamed != 0 {
		t.Errorf("second pass renamed %d contacts; want 0 (idempotent)", report.Renamed)
	}
}

func TestNameRepairIgnoresHandleShapedSenders(t *testing.T) {
	st := repairTestStore(t)
	ctx := context.Background()
	convID, contactID := repairNamesFixture(t, st)

	// Replace the messages' senders with another handle: nothing usable to
	// name the contact from, so no rename may happen.
	if _, err := st.DB().Exec(
		`UPDATE messages SET sender = '+15558675309' WHERE conversation_id = ?`, convID); err != nil {
		t.Fatal(err)
	}
	report, err := st.RepairHandleNamedContacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	if err := st.DB().QueryRow(`SELECT display_name FROM contacts WHERE id = ?`, contactID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "+14252413911" {
		t.Errorf("handle-shaped sender caused a rename: %q", name)
	}
	if report.Renamed != 0 {
		t.Errorf("renamed %d contacts from handle-shaped senders", report.Renamed)
	}
}

func TestListConversationsPrefersContactDisplayName(t *testing.T) {
	st := repairTestStore(t)
	ctx := context.Background()
	convID, _ := repairNamesFixture(t, st)

	if _, err := st.RepairHandleNamedContacts(ctx); err != nil {
		t.Fatal(err)
	}
	sums, err := st.ListConversations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, cs := range sums {
		if cs.ID == convID {
			if cs.Name != "Graham Blache" {
				t.Fatalf("sidebar name = %q, want the contact display name", cs.Name)
			}
			return
		}
	}
	t.Fatal("conversation missing from the sidebar listing")
}

func TestGetConversationByIDPrefersContactDisplayName(t *testing.T) {
	st := repairTestStore(t)
	ctx := context.Background()
	convID, _ := repairNamesFixture(t, st)

	if _, err := st.RepairHandleNamedContacts(ctx); err != nil {
		t.Fatal(err)
	}
	cs, err := st.GetConversationByID(ctx, convID)
	if err != nil || cs == nil {
		t.Fatalf("GetConversationByID: %v", err)
	}
	if cs.Name != "Graham Blache" {
		t.Fatalf("header name = %q, want the contact display name", cs.Name)
	}
}
