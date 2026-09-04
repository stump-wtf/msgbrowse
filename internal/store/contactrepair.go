// Contact Identity Repair — Healing Archives Written By The Old Rule
//
// Before issue #363 every importer handed the store the conversation NAME and
// the store wrote it into contact_identifiers as though it were a handle. On a
// real archive that produced 2,429 contacts whose identifiers were mostly
// profile and display names, plus a scattering of contacts minted from
// multi-recipient thread names like "me@example.com, chelsea@example.com" —
// people who do not exist.
//
// Those rows are already on disk, and a 320k-message archive is not something
// anyone should have to re-import to fix. RepairContactIdentities re-derives
// each conversation's identity with the same DeriveIdentity the importers now
// use and rewrites what changed. It runs from the import path beside
// ReconcileContacts, so an existing archive heals on its next sync.
//
// It is idempotent by construction: it only writes where the derived value
// differs from what is stored, so a repaired archive produces zero writes on
// every subsequent run.
//
// @joestump-agent 08/20/2026 - Added for issue #363.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/joestump/msgbrowse/internal/contacts"
	"github.com/joestump/msgbrowse/internal/signal"
)

// RepairReport counts what a repair pass changed, so the caller can log it and
// a test can assert the pass did something (and then nothing).
type RepairReport struct {
	// GroupsMarked is conversations newly recognised as multi-recipient
	// threads. Their synthesized contact is detached.
	GroupsMarked int
	// IdentifiersRewritten is contact_identifiers rows whose value changed to
	// the derived or persisted handle: a conversation named by a handle in
	// non-canonical form ("(555) 123-4567") re-derives to the normalized
	// value, and — since schemaV24 (#396) — a conversation whose importer knew
	// the real handle all along (WhatsApp's JID local part) heals from its
	// persisted `conversations.handle` on the first repair run after an import
	// stamps it, instead of waiting for a full re-import.
	IdentifiersRewritten int
	// ContactsOrphaned is contacts left with no identifiers and no
	// conversations once group threads were detached; these are the invented
	// people, and they are deleted.
	ContactsOrphaned int
}

// Changed reports whether the pass wrote anything.
func (r RepairReport) Changed() bool {
	return r.GroupsMarked > 0 || r.IdentifiersRewritten > 0 || r.ContactsOrphaned > 0
}

// RepairContactIdentities re-derives every conversation's counterparty identity
// and repairs the rows written under the old name-as-identifier rule.
//
// It repairs only what the stored row can prove. Re-derivation runs from the
// conversation NAME, because that is all the conversations table keeps — so it
// heals group threads and normalizes handle-shaped names, but it cannot invent
// a handle a source never wrote down. WhatsApp is the case that matters:
// its real handle lives in the JID, which the importer parses and does not
// persist, so a WhatsApp archive is healed by re-importing rather than by this
// pass. Re-import is cheap for WhatsApp (it re-reads one export) and is not
// what issue #363 was trying to avoid — that was the 320k-message iMessage and
// Signal history.
//
// It deliberately does NOT rewrite an identifier that is already a real handle
// into something else, and it never merges anything: merging stays the
// reconcile pass's job, gated by the user's rules. All this does is make the
// stored data say what the source actually knows, so matching has something
// true to work with.
func (s *Store) RepairContactIdentities(ctx context.Context) (RepairReport, error) {
	var rep RepairReport

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return rep, err
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()

	type convRow struct {
		id        int64
		source    string
		name      string
		isGroup   bool
		handle    string
		contactID sql.NullInt64
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id, source, name, is_group, handle, contact_id FROM conversations ORDER BY id`)
	if err != nil {
		return rep, fmt.Errorf("repair: load conversations: %w", err)
	}
	var convs []convRow
	for rows.Next() {
		var c convRow
		var grp int
		if err := rows.Scan(&c.id, &c.source, &c.name, &grp, &c.handle, &c.contactID); err != nil {
			rows.Close()
			return rep, err
		}
		c.isGroup = grp != 0
		convs = append(convs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}

	touchedContacts := map[int64]bool{}
	for _, c := range convs {
		identity := contacts.DeriveIdentity(c.name, contacts.SourceIdentity{IsGroup: c.isGroup})

		// A multi-recipient thread is not a person. Mark it and detach whatever
		// contact was minted for it; the contact is cleaned up below if nothing
		// else refers to it.
		if identity.IsGroup && (!c.isGroup || c.contactID.Valid) {
			if _, err := tx.ExecContext(ctx,
				`UPDATE conversations SET is_group = 1, contact_id = NULL WHERE id = ?`, c.id); err != nil {
				return rep, fmt.Errorf("repair: mark group %d: %w", c.id, err)
			}
			if c.contactID.Valid {
				touchedContacts[c.contactID.Int64] = true
				// Drop the identifier that was minted from the thread name; it
				// names a list of people, so it can never be anyone's handle.
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM contact_identifiers WHERE contact_id = ? AND source = ? AND identifier = ?`,
					c.contactID.Int64, c.source, c.name); err != nil {
					return rep, fmt.Errorf("repair: drop group identifier: %w", err)
				}
			}
			rep.GroupsMarked++
			continue
		}
		if identity.IsGroup || !c.contactID.Valid || identity.Identifier == "" {
			continue
		}

		// Rewrite an identifier that the old rule stored as the conversation
		// name when the source could have supplied a real handle. Only the row
		// that still holds the NAME is touched, and only when the derived value
		// actually differs — so a hand-edited or already-repaired identifier is
		// left alone and the pass stays idempotent.
		// #396: when the importer persisted a real handle (WhatsApp's JID
		// local part), that fact outranks anything derivable from the name,
		// and it may replace a display-name identifier even when the name is
		// itself not a handle — the case re-derivation alone cannot reach.
		rewriteTo, rewriteFrom := identity.Identifier, c.name
		if id := contacts.Normalize(c.handle); id.Kind == contacts.KindPhone || id.Kind == contacts.KindEmail {
			rewriteTo = id.Value
		}
		if rewriteTo == "" || rewriteTo == rewriteFrom {
			continue
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE OR IGNORE contact_identifiers SET identifier = ?
			   WHERE contact_id = ? AND source = ? AND identifier = ?`,
			rewriteTo, c.contactID.Int64, c.source, rewriteFrom)
		if err != nil {
			return rep, fmt.Errorf("repair: rewrite identifier for conversation %d: %w", c.id, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			rep.IdentifiersRewritten += int(n)
		}
	}

	// Delete contacts that the group repair left with nothing behind them: no
	// identifiers and no conversations. These are the invented people — a
	// contact whose only reason to exist was a thread name listing two others.
	for id := range touchedContacts {
		var identifiers, convCount int
		if err := tx.QueryRowContext(ctx,
			`SELECT (SELECT COUNT(*) FROM contact_identifiers WHERE contact_id = ?),
			        (SELECT COUNT(*) FROM conversations       WHERE contact_id = ?)`,
			id, id).Scan(&identifiers, &convCount); err != nil {
			return rep, fmt.Errorf("repair: count contact %d: %w", id, err)
		}
		if identifiers > 0 || convCount > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM contacts WHERE id = ?`, id); err != nil {
			return rep, fmt.Errorf("repair: delete orphaned contact %d: %w", id, err)
		}
		rep.ContactsOrphaned++
	}

	if err := tx.Commit(); err != nil {
		return rep, err
	}
	rollback = false
	return rep, nil
}

// ContactDiagnostics are the counters the Contacts settings tab shows so
// "merging is silently doing nothing" is visible rather than inferred. The
// archive that prompted issue #363 had 2,429 contacts, 9 of them multi-source,
// and 9 merge links — every one of them manual. Nothing in the UI said so.
type ContactDiagnostics struct {
	// Contacts is every contact row.
	Contacts int
	// MultiSource is contacts carrying identifiers from more than one source —
	// i.e. people the merge engine has actually joined across sources.
	MultiSource int
	// AutoMerged / ManualMerged split the recorded merge decisions by origin. A
	// zero AutoMerged next to a non-zero rule set is the symptom that matching
	// cannot fire at all.
	AutoMerged   int
	ManualMerged int
	// RealHandles is contacts with at least one phone- or email-shaped
	// identifier: the ones strong matching can ever apply to. A low number
	// against a large Contacts count explains an empty candidate queue.
	RealHandles int
}

// ContactDiagnosticCounts assembles the Contacts tab's diagnostic counters.
func (s *Store) ContactDiagnosticCounts(ctx context.Context) (ContactDiagnostics, error) {
	var d ContactDiagnostics
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM contacts`).Scan(&d.Contacts); err != nil {
		return d, fmt.Errorf("contact diagnostics: count contacts: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM (
    SELECT contact_id FROM contact_identifiers
     GROUP BY contact_id HAVING COUNT(DISTINCT source) > 1
)`).Scan(&d.MultiSource); err != nil {
		return d, fmt.Errorf("contact diagnostics: count multi-source: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `
SELECT
  COALESCE(SUM(CASE WHEN origin = 'auto'   THEN 1 ELSE 0 END), 0),
  COALESCE(SUM(CASE WHEN origin = 'manual' THEN 1 ELSE 0 END), 0)
  FROM contact_links WHERE kind = 'merge'`).Scan(&d.AutoMerged, &d.ManualMerged); err != nil {
		return d, fmt.Errorf("contact diagnostics: count links: %w", err)
	}

	// Handle shape is decided by the same Normalize the matcher uses rather
	// than by a SQL LIKE, so this counter can never disagree with what matching
	// actually does.
	rows, err := s.db.QueryContext(ctx, `SELECT contact_id, identifier FROM contact_identifiers`)
	if err != nil {
		return d, fmt.Errorf("contact diagnostics: load identifiers: %w", err)
	}
	defer rows.Close()
	withHandle := map[int64]bool{}
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return d, err
		}
		switch contacts.Normalize(raw).Kind {
		case contacts.KindPhone, contacts.KindEmail:
			withHandle[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		return d, err
	}
	d.RealHandles = len(withHandle)
	return d, nil
}

// Handle-Named Contacts — Naming What The Transcript Already Knows
//
// A 1:1 conversation whose contact was minted from the handle shows a raw
// "+14252413911" in the sidebar while the transcript prints the real name:
// messages.sender carries the address-book resolution the export made, but
// nothing ever gave the contact row a name (the address-book hint is off on
// machines without the macOS Contacts provider, #235/#236). 427 conversations
// on the live DB carry a usable sender label; this pass copies it onto the
// contact.
//
// It can never overwrite a human-set name: it only ever touches contacts
// whose display_name IS one of their own identifiers. Handle-shaped senders
// are ignored — a raw handle is not a name — and the new name must clear the
// old one's length, so a truncated handle never replaces a longer label with
// something less informative.
//
// @joestump-agent 09/04/2026 - Added for issue #444.

// NameRepairReport counts what one RepairHandleNamedContacts pass did.
type NameRepairReport struct {
	// Scanned is how many handle-named contacts had a candidate sender label.
	Scanned int
	// Renamed is how many contacts received a real name.
	Renamed int
	// Renames records each rename as "old → new".
	Renames []string
}

// handleShaped reports whether s looks like a raw identifier (phone or email)
// rather than a human name. Such strings are never used as display names.
func handleShaped(s string) bool {
	id := contacts.Normalize(s)
	return id.Kind == contacts.KindPhone || id.Kind == contacts.KindEmail
}

// RepairHandleNamedContacts renames handle-named contacts from their dominant
// non-owner sender label. Idempotent: a renamed contact no longer matches the
// handle-named shape, so a second pass finds nothing to do. Run it after
// imports/syncs; it is a pure UPDATE pass and never merges or deletes.
func (s *Store) RepairHandleNamedContacts(ctx context.Context) (NameRepairReport, error) {
	var report NameRepairReport

	// Candidates: contacts whose display_name byte-equals one of their own
	// identifiers (minted from the handle, never named), with their dominant
	// non-owner, non-system sender label across all their conversations.
	rows, err := s.db.QueryContext(ctx, `
WITH candidates AS (
    SELECT ct.id, ct.display_name,
           (SELECT m.sender
              FROM messages m
              JOIN conversations cv ON cv.id = m.conversation_id
             WHERE cv.contact_id = ct.id
               AND m.sender <> ?
               AND m.is_system = 0
               AND TRIM(m.body) <> ''
             GROUP BY m.sender
             ORDER BY COUNT(*) DESC, m.sender ASC
             LIMIT 1) AS dominant
      FROM contacts ct
     WHERE EXISTS (
           SELECT 1 FROM contact_identifiers ci
            WHERE ci.contact_id = ct.id AND ci.identifier = ct.display_name)
)
SELECT id, display_name, dominant FROM candidates
 WHERE dominant IS NOT NULL AND TRIM(dominant) <> ''`, signal.OwnerSender)
	if err != nil {
		return report, fmt.Errorf("contact name repair: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		id       int64
		old      string
		dominant string
	}
	var updates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.old, &c.dominant); err != nil {
			return report, fmt.Errorf("contact name repair: scan: %w", err)
		}
		report.Scanned++
		dominant := strings.TrimSpace(c.dominant)
		if handleShaped(dominant) {
			continue // a raw handle is not a name
		}
		if len(dominant) <= len(c.old) {
			continue // must clear the existing name's floor
		}
		updates = append(updates, candidate{c.id, c.old, dominant})
	}
	if err := rows.Err(); err != nil {
		return report, fmt.Errorf("contact name repair: %w", err)
	}

	for _, u := range updates {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE contacts SET display_name = ? WHERE id = ?`, u.dominant, u.id); err != nil {
			return report, fmt.Errorf("contact name repair: rename %d: %w", u.id, err)
		}
		report.Renamed++
		report.Renames = append(report.Renames, u.old+" → "+u.dominant)
	}
	return report, nil
}
