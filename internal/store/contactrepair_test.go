package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/joestump/msgbrowse/internal/contacts"
	"github.com/joestump/msgbrowse/internal/source"
)

func repairTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "repair.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func identifiersOf(t *testing.T, st *Store, contactID int64) []string {
	t.Helper()
	rows, err := st.db.Query(`SELECT identifier FROM contact_identifiers WHERE contact_id = ? ORDER BY identifier`, contactID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// TestWhatsAppJIDBecomesTheIdentifier: the importer's parsed handle wins over
// the display name. This is the fix that lets a WhatsApp contact meet the same
// person on another source — before #363 the identifier was "Chelsea Stump".
func TestWhatsAppJIDBecomesTheIdentifier(t *testing.T) {
	st := repairTestStore(t)
	ctx := context.Background()

	convID, err := st.UpsertConversationIdentity(ctx, source.WhatsApp, "Chelsea Stump",
		contacts.SourceIdentity{Identifier: "15551234567"})
	if err != nil {
		t.Fatal(err)
	}
	var contactID int64
	if err := st.db.QueryRow(`SELECT contact_id FROM conversations WHERE id = ?`, convID).Scan(&contactID); err != nil {
		t.Fatal(err)
	}
	if got := identifiersOf(t, st, contactID); len(got) != 1 || got[0] != "15551234567" {
		t.Errorf("identifiers = %v, want [15551234567]", got)
	}
	var name string
	if err := st.db.QueryRow(`SELECT display_name FROM contacts WHERE id = ?`, contactID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "Chelsea Stump" {
		t.Errorf("display_name = %q, want the profile name", name)
	}
}

// TestGroupThreadMintsNoContact: a multi-recipient thread name is a list of
// people, so no contact is invented for it. The reported archive had a contact
// whose identifier was "me@…, chelsea…@gmail.com".
func TestGroupThreadMintsNoContact(t *testing.T) {
	st := repairTestStore(t)
	ctx := context.Background()

	convID, err := st.UpsertConversation(ctx, source.IMessage, "me@example.com, chelsea@example.com")
	if err != nil {
		t.Fatal(err)
	}
	var isGroup int
	var contactID *int64
	if err := st.db.QueryRow(`SELECT is_group, contact_id FROM conversations WHERE id = ?`, convID).
		Scan(&isGroup, &contactID); err != nil {
		t.Fatal(err)
	}
	if isGroup != 1 {
		t.Error("comma-joined conversation not marked is_group")
	}
	if contactID != nil {
		t.Errorf("group thread synthesized contact %d", *contactID)
	}
	var contactCount int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM contacts`).Scan(&contactCount); err != nil {
		t.Fatal(err)
	}
	if contactCount != 0 {
		t.Errorf("contacts = %d, want 0 for a group-only archive", contactCount)
	}
}

// TestCrossSourceAutoMergeOnSharedPhone is the issue's headline acceptance:
// with real handles stored, a Signal-side and an iMessage-side contact for the
// same number auto-merge, and the decision is recorded with origin='auto'. On
// the reported archive contact_links held 9 rows, every one of them manual.
func TestCrossSourceAutoMergeOnSharedPhone(t *testing.T) {
	st := repairTestStore(t)
	ctx := context.Background()

	if err := st.SetMergeRules(ctx, MergeRules{
		AutoMerge: true, MatchPhone: true, MatchEmail: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Two sources that BOTH carry the number, in different shapes: an
	// international form on one side and a national one on the other.
	if _, err := st.UpsertConversationIdentity(ctx, source.WhatsApp, "Chelsea Stump",
		contacts.SourceIdentity{Identifier: "+15551234567"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertConversation(ctx, source.IMessage, "15551234567"); err != nil {
		t.Fatal(err)
	}
	if err := st.ReconcileContacts(ctx, nil); err != nil {
		t.Fatal(err)
	}

	var autoLinks int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM contact_links WHERE kind = 'merge' AND origin = 'auto'`).Scan(&autoLinks); err != nil {
		t.Fatal(err)
	}
	if autoLinks == 0 {
		t.Fatal("no auto merge recorded — cross-source matching still cannot fire")
	}
	var contactCount int
	if err := st.db.QueryRow(`SELECT COUNT(DISTINCT contact_id) FROM conversations WHERE contact_id IS NOT NULL`).
		Scan(&contactCount); err != nil {
		t.Fatal(err)
	}
	if contactCount != 1 {
		t.Errorf("conversations resolve to %d contacts, want 1 merged person", contactCount)
	}
}

// TestDisplayNameNeverAutoMerges is the safety counterpart: the same two people
// matched ONLY by name stay apart, no matter how permissive the rules are. This
// mirrors the address-book guarantee in ADR-0024.
func TestDisplayNameNeverAutoMerges(t *testing.T) {
	st := repairTestStore(t)
	ctx := context.Background()

	if err := st.SetMergeRules(ctx, MergeRules{
		AutoMerge: true, MatchPhone: true, MatchEmail: true, MatchDisplayName: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Signal has only a profile name; iMessage has an email. Nothing but the
	// name connects them.
	if _, err := st.UpsertConversation(ctx, source.Signal, "ChelseaStump"); err != nil {
		t.Fatal(err)
	}
	iid, err := st.UpsertConversation(ctx, source.IMessage, "chelsea@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// Give the iMessage contact the matching display name.
	var iContact int64
	if err := st.db.QueryRow(`SELECT contact_id FROM conversations WHERE id = ?`, iid).Scan(&iContact); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE contacts SET display_name = ? WHERE id = ?`, "Chelsea Stump", iContact); err != nil {
		t.Fatal(err)
	}

	if err := st.ReconcileContacts(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var autoLinks int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM contact_links WHERE kind = 'merge' AND origin = 'auto'`).Scan(&autoLinks); err != nil {
		t.Fatal(err)
	}
	if autoLinks != 0 {
		t.Fatalf("a display-name match auto-merged %d pair(s) — it must only ever suggest", autoLinks)
	}

	// It must still SUGGEST, or the Signal contact is unreachable forever.
	cands, err := st.MergeCandidates(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range cands {
		if c.Reason == string(contacts.ReasonDisplayName) {
			found = true
		}
	}
	if !found {
		t.Errorf("no display-name suggestion offered; candidates = %+v", cands)
	}
}

// TestRepairHealsLegacyRowsAndIsIdempotent: an archive written under the old
// rule fixes itself on the next import, and a second pass writes nothing.
func TestRepairHealsLegacyRowsAndIsIdempotent(t *testing.T) {
	st := repairTestStore(t)
	ctx := context.Background()

	// Simulate the legacy shape directly: a group thread that minted a contact,
	// with the thread name stored as that contact's identifier.
	if _, err := st.db.Exec(
		`INSERT INTO contacts(id, display_name) VALUES (1, 'me@example.com, chelsea@example.com')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(
		`INSERT INTO contact_identifiers(contact_id, source, identifier)
		 VALUES (1, 'imessage', 'me@example.com, chelsea@example.com')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(
		`INSERT INTO conversations(id, source, name, contact_id, is_group)
		 VALUES (1, 'imessage', 'me@example.com, chelsea@example.com', 1, 0)`); err != nil {
		t.Fatal(err)
	}

	rep, err := st.RepairContactIdentities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.GroupsMarked != 1 {
		t.Errorf("GroupsMarked = %d, want 1", rep.GroupsMarked)
	}
	if rep.ContactsOrphaned != 1 {
		t.Errorf("ContactsOrphaned = %d, want 1 (the invented person)", rep.ContactsOrphaned)
	}
	var contactCount, isGroup int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM contacts`).Scan(&contactCount); err != nil {
		t.Fatal(err)
	}
	if contactCount != 0 {
		t.Errorf("contacts = %d, want 0 — the group's contact should be gone", contactCount)
	}
	if err := st.db.QueryRow(`SELECT is_group FROM conversations WHERE id = 1`).Scan(&isGroup); err != nil {
		t.Fatal(err)
	}
	if isGroup != 1 {
		t.Error("conversation not marked is_group by the repair")
	}

	// Idempotence: the second pass must be a no-op, or the repair would churn
	// the database on every import forever.
	again, err := st.RepairContactIdentities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed() {
		t.Errorf("second repair pass wrote changes: %+v", again)
	}
}

// TestContactDiagnosticCounts: the counters that make "merging is silently
// doing nothing" visible instead of inferred.
func TestContactDiagnosticCounts(t *testing.T) {
	st := repairTestStore(t)
	ctx := context.Background()

	if _, err := st.UpsertConversationIdentity(ctx, source.WhatsApp, "Chelsea Stump",
		contacts.SourceIdentity{Identifier: "+15551234567"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertConversation(ctx, source.Signal, "ChelseaStump"); err != nil {
		t.Fatal(err)
	}

	d, err := st.ContactDiagnosticCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.Contacts != 2 {
		t.Errorf("Contacts = %d, want 2", d.Contacts)
	}
	if d.RealHandles != 1 {
		t.Errorf("RealHandles = %d, want 1 — only WhatsApp supplied a phone", d.RealHandles)
	}
	if d.MultiSource != 0 {
		t.Errorf("MultiSource = %d, want 0 before any merge", d.MultiSource)
	}
	if d.AutoMerged != 0 || d.ManualMerged != 0 {
		t.Errorf("merge counts = auto %d / manual %d, want 0/0", d.AutoMerged, d.ManualMerged)
	}
}

// TestAutoMergeableReasonRefusesWeakEvidence pins the auto-merge allow-list
// directly, because the caller cannot currently exercise it:
// ReconcileContacts builds candidates with contacts.Candidates(stored, nil, …),
// and without names ReasonDisplayName is never produced — so deleting the guard
// from the reconcile loop passes every other test in this package.
//
// The safety property today is therefore structural (reconcile does not compute
// weak reasons), and this guard is the backstop for the day that changes. An
// untested backstop reads as protection while providing none, so it gets a test
// of its own.
//
// @joestump-agent 08/20/2026 - Added while reviewing #378.
func TestAutoMergeableReasonRefusesWeakEvidence(t *testing.T) {
	for _, tc := range []struct {
		reason contacts.ReasonKind
		want   bool
		why    string
	}{
		{contacts.ReasonPhone, true, "a shared phone number is exact evidence"},
		{contacts.ReasonEmail, true, "a shared email is exact evidence"},
		{contacts.ReasonAddressBook, false, "an address-book grouping is a hint a human confirms (ADR-0024)"},
		{contacts.ReasonDisplayName, false, "two people can share a name; merging blends two archives irreversibly"},
	} {
		if got := autoMergeableReason(tc.reason); got != tc.want {
			t.Errorf("autoMergeableReason(%q) = %v, want %v — %s", tc.reason, got, tc.want, tc.why)
		}
	}
}

// TestReconcilePathNeverComputesDisplayNameCandidates pins the structural half:
// the auto-merge path asks for candidates without names, and the matcher must
// not invent a display-name reason from identifiers alone.
func TestReconcilePathNeverComputesDisplayNameCandidates(t *testing.T) {
	stored := []contacts.StoredIdentifier{
		{ContactID: 1, Source: "signal", Raw: "ChelseaStump"},
		{ContactID: 2, Source: "imessage", Raw: "chelsea stump"},
	}
	rules := contacts.MatchRules{MatchPhone: true, MatchEmail: true, MatchDisplayName: true}

	for _, c := range contacts.Candidates(stored, nil, rules) {
		if c.Reason == contacts.ReasonDisplayName {
			t.Fatalf("the names-less Candidates call produced a display-name candidate (%+v); "+
				"the auto-merge path relies on it not doing so", c)
		}
	}
}

// TestRepairRewritesHandleShapedNamesButNotWhatsApp pins what the repair pass
// can and cannot heal, because the difference decides whether a user has to
// re-import.
//
// The pass re-derives from the conversation NAME, which is all the
// conversations table stores. So an iMessage row named by a handle in
// non-canonical form is normalized in place — no re-import needed. A WhatsApp
// row named by a display name is NOT: its real handle is the JID local part,
// the JID is never persisted, and no amount of re-deriving from "Chelsea"
// produces a phone number. Those archives heal on their next import, when the
// importer supplies the JID again.
//
// Without this test the distinction lives only in a comment, and the comment
// was wrong before this test existed — it claimed the pass handled the
// WhatsApp case.
//
// @joestump 08/22/2026 - Added while reviewing #378.
func TestRepairRewritesHandleShapedNamesButNotWhatsApp(t *testing.T) {
	st := repairTestStore(t)
	ctx := context.Background()

	// WhatsApp, named by display name: unhealable from the stored row alone.
	mustExec(t, st, `INSERT INTO contacts(id, display_name) VALUES (1, 'Chelsea')`)
	mustExec(t, st, `INSERT INTO contact_identifiers(contact_id, source, identifier)
	                 VALUES (1, 'whatsapp', 'Chelsea')`)
	mustExec(t, st, `INSERT INTO conversations(id, source, name, contact_id, is_group)
	                 VALUES (1, 'whatsapp', 'Chelsea', 1, 0)`)

	// iMessage, named by a handle in non-canonical form: healable in place.
	mustExec(t, st, `INSERT INTO contacts(id, display_name) VALUES (2, '(555) 123-4567')`)
	mustExec(t, st, `INSERT INTO contact_identifiers(contact_id, source, identifier)
	                 VALUES (2, 'imessage', '(555) 123-4567')`)
	mustExec(t, st, `INSERT INTO conversations(id, source, name, contact_id, is_group)
	                 VALUES (2, 'imessage', '(555) 123-4567', 2, 0)`)

	rep, err := st.RepairContactIdentities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.IdentifiersRewritten != 1 {
		t.Errorf("IdentifiersRewritten = %d, want 1 (the iMessage row only)", rep.IdentifiersRewritten)
	}

	if got := identifierOf(t, st, 1); got != "Chelsea" {
		t.Errorf("whatsapp identifier = %q, want %q unchanged — the JID is not persisted, "+
			"so this archive can only be healed by re-importing", got, "Chelsea")
	}
	if got := identifierOf(t, st, 2); got == "(555) 123-4567" {
		t.Errorf("imessage identifier = %q — a handle-shaped name should normalize in place", got)
	}

	// Still idempotent with a rewrite in the mix.
	again, err := st.RepairContactIdentities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed() {
		t.Errorf("second repair pass wrote changes: %+v", again)
	}
}

func mustExec(t *testing.T, st *Store, q string) {
	t.Helper()
	if _, err := st.db.Exec(q); err != nil {
		t.Fatalf("exec %s: %v", q, err)
	}
}

func identifierOf(t *testing.T, st *Store, contactID int64) string {
	t.Helper()
	var id string
	if err := st.db.QueryRow(
		`SELECT identifier FROM contact_identifiers WHERE contact_id = ?`, contactID).Scan(&id); err != nil {
		t.Fatalf("read identifier for contact %d: %v", contactID, err)
	}
	return id
}
