package store

// Per-CONTACT reads backing the /contact/{id} profile page (Phase 1 redesign).
// A contact is a person, potentially merged across sources (Signal + iMessage),
// so every aggregate spans ALL of the contact's conversations. There is no
// contact_id column on messages/attachments/reactions, so the load-bearing
// predicate throughout is:
//
//	conversation_id IN (SELECT id FROM conversations WHERE contact_id = ?)
//
// conversations is a tiny table, so the subquery is negligible and needs no
// index at current scale. Day/hour bucketing is UTC (strftime(...,'unixepoch'))
// for deterministic, testable output — matching the journal's UTC discipline.

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/joestump/msgbrowse/internal/signal"
)

// Contact is the identity view of a person for the profile page: display name,
// every merged source handle, the conversations they own, and message span.
// Message counts/totals come from ContactStats (one scan), not here — the page
// fetches both, so GetContactByID stays identity-only and avoids a second scan.
type Contact struct {
	ID            int64
	DisplayName   string
	Notes         string
	Identifiers   []ContactIdentifier
	Conversations []ContactConversation
	FirstTS       string // display string ("" if none), via indexed ts_unix probe
	LastTS        string
}

// SourceCount is how many distinct sources the contact's identifiers span (1 =
// single-source, >1 = genuinely merged). Drives the "merged" subtitle.
func (c *Contact) SourceCount() int {
	seen := map[string]struct{}{}
	for _, id := range c.Identifiers {
		seen[id.Source] = struct{}{}
	}
	return len(seen)
}

// ContactConversation is one thread a contact owns (id/source/name only).
type ContactConversation struct {
	ID     int64
	Source string
	Name   string
}

// ContactStats are the cheap scalar tiles for the profile, computed in one scan
// over the contact's messages (sent/received split by the owner sender).
type ContactStats struct {
	TotalMessages    int
	SentMessages     int
	ReceivedMessages int
	Photos           int
	FirstTSUnix      int64
	LastTSUnix       int64
	MessagesPerDay   float64 // derived in Go
}

// MonthBucket is one "YYYY-MM" message-volume point for the sparkline. Months
// with no traffic are ABSENT (the caller gap-fills for a dense axis).
type MonthBucket struct {
	Month string
	Count int
}

// EmojiCount is one reaction tally for the top-reactions row.
type EmojiCount struct {
	Emoji string
	Count int
}

// GetContactByID assembles a contact's identity: name, every merged identifier,
// the conversations they own, total message count, and first/last timestamps.
// Returns (nil, nil) when no such contact exists (handler 404s).
func (s *Store) GetContactByID(ctx context.Context, contactID int64) (*Contact, error) {
	c := Contact{ID: contactID}
	err := s.db.QueryRowContext(ctx,
		`SELECT display_name, notes FROM contacts WHERE id = ?`, contactID).
		Scan(&c.DisplayName, &c.Notes)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get contact: %w", err)
	}

	if c.Identifiers, err = s.ContactIdentifiers(ctx, contactID); err != nil {
		return nil, err
	}
	if c.Conversations, err = s.ContactConversations(ctx, contactID); err != nil {
		return nil, err
	}

	// First/last DISPLAY strings via indexed ts_unix probes (each returns "" when
	// the contact has no messages) — never MIN/MAX(ts), whose iMessage month-name
	// strings sort alphabetically wrong (SPEC-0008 REQ-0008-002). Counts/totals
	// are ContactStats' job, so no aggregate scan happens here.
	if c.FirstTS, err = s.contactEdgeTS(ctx, contactID, true); err != nil {
		return nil, err
	}
	if c.LastTS, err = s.contactEdgeTS(ctx, contactID, false); err != nil {
		return nil, err
	}
	return &c, nil
}

// contactEdgeTS returns the display ts of the contact's earliest (asc) or latest
// message via an indexed ts_unix probe.
func (s *Store) contactEdgeTS(ctx context.Context, contactID int64, asc bool) (string, error) {
	order := "DESC"
	if asc {
		order = "ASC"
	}
	var ts string
	err := s.db.QueryRowContext(ctx, `
SELECT ts FROM messages
 WHERE conversation_id IN (SELECT id FROM conversations WHERE contact_id = ?)
   AND is_system = 0
 ORDER BY ts_unix `+order+`, id `+order+` LIMIT 1`, contactID).Scan(&ts)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("contact edge ts: %w", err)
	}
	return ts, nil
}

// ContactIdentifiers returns the contact's full handle set across sources — with
// NO self-identity exclusion (the profile header wants every merged handle,
// unlike ConversationIdentifiers which hides the thread's own identity).
func (s *Store) ContactIdentifiers(ctx context.Context, contactID int64) ([]ContactIdentifier, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT source, identifier FROM contact_identifiers
		  WHERE contact_id = ? ORDER BY source, identifier`, contactID)
	if err != nil {
		return nil, fmt.Errorf("contact identifiers: %w", err)
	}
	defer rows.Close()
	var out []ContactIdentifier
	for rows.Next() {
		var ci ContactIdentifier
		if err := rows.Scan(&ci.Source, &ci.Identifier); err != nil {
			return nil, err
		}
		out = append(out, ci)
	}
	return out, rows.Err()
}

// ContactConversations lists the threads a contact owns (id/source/name).
func (s *Store) ContactConversations(ctx context.Context, contactID int64) ([]ContactConversation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, source, name FROM conversations WHERE contact_id = ? ORDER BY source, name`, contactID)
	if err != nil {
		return nil, fmt.Errorf("contact conversations: %w", err)
	}
	defer rows.Close()
	var out []ContactConversation
	for rows.Next() {
		var cc ContactConversation
		if err := rows.Scan(&cc.ID, &cc.Source, &cc.Name); err != nil {
			return nil, err
		}
		out = append(out, cc)
	}
	return out, rows.Err()
}

// ContactFacts returns every AI-extracted fact about a contact, ordered by
// category then chronology, with each fact's supporting message resolved to its
// current rowid AND owning conversation (both 0 if the message is gone) so the
// page can deep-link into the message's OWN conversation. Because messages.hash
// is globally unique, the hash→row resolution is correct across all the
// contact's threads. Callers group by category in declared order (SQL's
// alphabetical category order is not the display order).
func (s *Store) ContactFacts(ctx context.Context, contactID int64) ([]ContactFact, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT f.fact, f.category, f.source, f.source_message_hash,
       COALESCE(m.id, 0), COALESCE(m.conversation_id, 0), f.source_ts, f.source_ts_unix, f.model
  FROM contact_facts f
  LEFT JOIN messages m ON m.hash = f.source_message_hash
 WHERE f.contact_id = ?
 ORDER BY f.category ASC, f.source_ts_unix ASC, f.id ASC`, contactID)
	if err != nil {
		return nil, fmt.Errorf("contact facts: %w", err)
	}
	defer rows.Close()
	var out []ContactFact
	for rows.Next() {
		var f ContactFact
		if err := rows.Scan(&f.Fact, &f.Category, &f.Source, &f.SourceMessageHash,
			&f.SourceMessageID, &f.SourceConversationID, &f.SourceTS, &f.SourceTSUnix, &f.Model); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ContactStats returns the cheap scalar tiles in one scan: total, sent/received
// (split by the owner sender), the epoch bounds, and photos shared.
// MessagesPerDay is derived in Go.
func (s *Store) ContactStats(ctx context.Context, contactID int64) (ContactStats, error) {
	var st ContactStats
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(sender =  ?), 0),
       COALESCE(SUM(sender <> ?), 0),
       COALESCE(MIN(ts_unix), 0),
       COALESCE(MAX(ts_unix), 0),
       (SELECT COUNT(*) FROM attachments
         WHERE conversation_id IN (SELECT id FROM conversations WHERE contact_id = ?)
           AND kind = 'image')
  FROM messages
 WHERE conversation_id IN (SELECT id FROM conversations WHERE contact_id = ?)
   AND is_system = 0`,
		signal.OwnerSender, signal.OwnerSender, contactID, contactID).
		Scan(&st.TotalMessages, &st.SentMessages, &st.ReceivedMessages, &st.FirstTSUnix, &st.LastTSUnix, &st.Photos)
	if err != nil {
		return ContactStats{}, fmt.Errorf("contact stats: %w", err)
	}
	if st.TotalMessages > 0 && st.LastTSUnix > st.FirstTSUnix {
		days := float64(st.LastTSUnix-st.FirstTSUnix) / 86400
		if days < 1 {
			days = 1
		}
		st.MessagesPerDay = float64(st.TotalMessages) / days
	} else if st.TotalMessages > 0 {
		st.MessagesPerDay = float64(st.TotalMessages)
	}
	return st, nil
}

// ContactMessageVolume returns per-month message counts (UTC) for the sparkline,
// oldest month first. Zero-traffic months are absent — the caller gap-fills.
func (s *Store) ContactMessageVolume(ctx context.Context, contactID int64) ([]MonthBucket, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT strftime('%Y-%m', ts_unix, 'unixepoch') AS ym, COUNT(*)
  FROM messages
 WHERE conversation_id IN (SELECT id FROM conversations WHERE contact_id = ?)
   AND is_system = 0
 GROUP BY ym ORDER BY ym`, contactID)
	if err != nil {
		return nil, fmt.Errorf("contact message volume: %w", err)
	}
	defer rows.Close()
	var out []MonthBucket
	for rows.Next() {
		var b MonthBucket
		if err := rows.Scan(&b.Month, &b.Count); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ContactMostActiveHour returns the UTC hour (0–23) the contact is most active
// and its message count. ok is false when the contact has no real messages.
func (s *Store) ContactMostActiveHour(ctx context.Context, contactID int64) (hour, count int, ok bool, err error) {
	e := s.db.QueryRowContext(ctx, `
SELECT CAST(strftime('%H', ts_unix, 'unixepoch') AS INTEGER) AS hr, COUNT(*) n
  FROM messages
 WHERE conversation_id IN (SELECT id FROM conversations WHERE contact_id = ?)
   AND is_system = 0
 GROUP BY hr ORDER BY n DESC, hr LIMIT 1`, contactID).Scan(&hour, &count)
	if e == sql.ErrNoRows {
		return 0, 0, false, nil
	}
	if e != nil {
		return 0, 0, false, fmt.Errorf("contact most active hour: %w", e)
	}
	return hour, count, true, nil
}

// ContactTopReactions returns the contact's most-used reaction emoji across all
// their threads, most-frequent first, capped at limit.
func (s *Store) ContactTopReactions(ctx context.Context, contactID int64, limit int) ([]EmojiCount, error) {
	if limit <= 0 || limit > 50 {
		limit = 8
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT emoji, COUNT(*) n FROM reactions
 WHERE conversation_id IN (SELECT id FROM conversations WHERE contact_id = ?)
 GROUP BY emoji ORDER BY n DESC, emoji LIMIT ?`, contactID, limit)
	if err != nil {
		return nil, fmt.Errorf("contact top reactions: %w", err)
	}
	defer rows.Close()
	var out []EmojiCount
	for rows.Next() {
		var e EmojiCount
		if err := rows.Scan(&e.Emoji, &e.Count); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
