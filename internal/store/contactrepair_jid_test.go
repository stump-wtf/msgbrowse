package store

import (
	"context"
	"testing"

	"github.com/joestump/msgbrowse/internal/contacts"
	"github.com/joestump/msgbrowse/internal/source"
)

// #396, shape 2: WhatsApp archives written under the old name-as-identifier
// rule kept display-name identifiers because the JID never reached the
// database. schemaV24 persists the importer's handle on the conversation row,
// which turns healing into ordinary RepairContactIdentities work.
func TestRepairHealsWhatsAppIdentifierFromPersistedHandle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// An OLD import: no real handle supplied — display name became identifier.
	if _, err := st.UpsertConversationIdentity(ctx, source.WhatsApp, "Chelsea Stump", contacts.SourceIdentity{}); err != nil {
		t.Fatal(err)
	}
	var cid int64
	var ident string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT c.contact_id FROM conversations c WHERE c.source = ? AND c.name = ?`,
		source.WhatsApp, "Chelsea Stump").Scan(&cid); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT identifier FROM contact_identifiers WHERE contact_id = ? AND source = ?`, cid, source.WhatsApp).
		Scan(&ident); err != nil {
		t.Fatal(err)
	}
	if ident == "" || ident == "17347090567" {
		t.Fatalf("expected old-rule display-name identifier, got %q", ident)
	}

	// Before any handle is persisted the repair pass is still a no-op here.
	rep1, err := st.RepairContactIdentities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep1.Changed() {
		t.Errorf("repair changed %+v with no handle persisted", rep1)
	}

	// The NEW import stamps the JID local part as the conversation's handle.
	if _, err := st.UpsertConversationIdentity(ctx, source.WhatsApp, "Chelsea Stump",
		contacts.SourceIdentity{Identifier: "17347090567", IdentifierKind: contacts.KindPhone}); err != nil {
		t.Fatal(err)
	}
	var handle string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT handle FROM conversations WHERE id = ?`, cid).Scan(&handle); err != nil {
		t.Fatal(err)
	}
	if handle == "" {
		t.Fatal("import did not persist the JID handle")
	}

	rep2, err := st.RepairContactIdentities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.IdentifiersRewritten != 1 {
		t.Fatalf("IdentifiersRewritten = %d, want 1: %+v", rep2.IdentifiersRewritten, rep2)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT identifier FROM contact_identifiers WHERE contact_id = ? AND source = ?`, cid, source.WhatsApp).
		Scan(&ident); err != nil {
		t.Fatal(err)
	}
	if ident != "17347090567" {
		t.Errorf("identifier = %q, want the normalized JID local part", ident)
	}

	// Idempotent by construction: a second pass writes nothing.
	rep3, err := st.RepairContactIdentities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep3.Changed() {
		t.Errorf("second repair wrote again: %+v", rep3)
	}
}

// A group thread's JID must never be stamped as a person's handle.
func TestUpsertHandleSkipsGroups(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, err := st.UpsertConversationIdentity(ctx, source.WhatsApp, "Book Club",
		contacts.SourceIdentity{IsGroup: true, Identifier: "12025550001@g.us", IdentifierKind: contacts.KindPhone})
	if err != nil {
		t.Fatal(err)
	}
	var (
		handle    string
		contactID interface{}
	)
	if err := st.DB().QueryRowContext(ctx,
		`SELECT handle, contact_id FROM conversations WHERE id = ?`, id).Scan(&handle, &contactID); err != nil {
		t.Fatal(err)
	}
	if handle != "" || contactID != nil {
		t.Errorf("group got handle=%q contact=%v", handle, contactID)
	}
}
