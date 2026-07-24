package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// FactConversation identifies a conversation eligible for fact extraction: one
// linked to a contact and holding at least one real (non-system, non-empty)
// message. Source and Name let the orchestrator honor the exclude list and
// label prompts.
type FactConversation struct {
	ID        int64
	Source    string
	Name      string
	ContactID int64
}

// FactInput is a single extracted fact to persist, with the provenance that ties
// it back to a specific source message.
type FactInput struct {
	ContactID         int64
	Fact              string
	Category          string
	Source            string
	SourceMessageHash string
	SourceTS          string
	SourceTSUnix      int64
	Model             string
}

// ContactFact is a stored fact as rendered for the UI. SourceMessageID is the
// current rowid of the supporting message (for a jump-to-context link), or 0 if
// that message no longer exists (e.g. removed by a re-ingest).
// SourceConversationID is the conversation that owns the supporting message
// (0 when the message is gone) — the contact page spans multiple conversations,
// so a fact's jump-to-context link must target the message's OWN conversation,
// not a single active one. It is left 0 by ContactFactsByConversation (whose
// caller already knows the conversation).
type ContactFact struct {
	Fact                 string
	Category             string
	Source               string
	SourceMessageHash    string
	SourceMessageID      int64
	SourceConversationID int64
	SourceTS             string
	SourceTSUnix         int64
	Model                string
}

// factHash is the stable dedup key for a fact: a digest of its normalized text.
// Two extractions that phrase the same fact identically collapse to one row;
// genuinely different wordings are kept (the extractor is instructed to be
// terse and consistent, which keeps near-duplicates rare).
func factHash(fact string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(fact))))
	return hex.EncodeToString(sum[:])
}

// FactConversations returns every conversation eligible for fact extraction —
// linked to a contact, holding real messages, and not on the exclude list.
// Excluded conversations are filtered by name (the same folder-name denylist the
// journal honors) so their content is never handed to the orchestrator, let
// alone the LLM.
func (s *Store) FactConversations(ctx context.Context, exclude []string) ([]FactConversation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT c.id, c.source, c.name, c.contact_id
  FROM conversations c
 WHERE c.contact_id IS NOT NULL
   AND EXISTS (
       SELECT 1 FROM messages m
        WHERE m.conversation_id = c.id AND m.is_system = 0 AND TRIM(m.body) <> ''
   )
 ORDER BY c.id`)
	if err != nil {
		return nil, fmt.Errorf("fact conversations: %w", err)
	}
	defer rows.Close()

	excluded := make(map[string]struct{}, len(exclude))
	for _, name := range exclude {
		excluded[name] = struct{}{}
	}

	var out []FactConversation
	for rows.Next() {
		var fc FactConversation
		if err := rows.Scan(&fc.ID, &fc.Source, &fc.Name, &fc.ContactID); err != nil {
			return nil, err
		}
		if _, skip := excluded[fc.Name]; skip {
			continue
		}
		out = append(out, fc)
	}
	return out, rows.Err()
}

// GetFactState returns the extraction cursor for a conversation: the hash of the
// last message handed to the extractor and the chat model that produced its
// facts. ok is false when the conversation has never been processed.
func (s *Store) GetFactState(ctx context.Context, convID int64) (lastHash, model string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT last_message_hash, model FROM fact_state WHERE conversation_id = ?`, convID).
		Scan(&lastHash, &model)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("get fact state: %w", err)
	}
	return lastHash, model, true, nil
}

// SetFactState advances a conversation's extraction cursor to lastHash for the
// given model and adds factsAdded to its running total. It upserts so the first
// call creates the row.
func (s *Store) SetFactState(ctx context.Context, convID int64, lastHash, model string, factsAdded int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO fact_state(conversation_id, last_message_hash, model, facts_added, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(conversation_id) DO UPDATE SET
    last_message_hash = excluded.last_message_hash,
    model             = excluded.model,
    facts_added       = fact_state.facts_added + excluded.facts_added,
    updated_at        = excluded.updated_at`,
		convID, lastHash, model, factsAdded, now)
	if err != nil {
		return fmt.Errorf("set fact state: %w", err)
	}
	return nil
}

// ResolveCursor maps a stored last-message hash back to its current keyset
// position (ts_unix, id) within the conversation. ok is false when the message
// no longer exists (deleted by re-ingest), in which case the caller restarts
// from the beginning — safe because PutFact is idempotent.
func (s *Store) ResolveCursor(ctx context.Context, convID int64, hash string) (tsUnix, id int64, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT ts_unix, id FROM messages WHERE conversation_id = ? AND hash = ?`, convID, hash).
		Scan(&tsUnix, &id)
	if err == sql.ErrNoRows {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("resolve cursor: %w", err)
	}
	return tsUnix, id, true, nil
}

// PutFact stores one extracted fact, deduplicated per contact by normalized
// text. It returns whether a new row was inserted (false means the fact already
// existed and was left untouched).
func (s *Store) PutFact(ctx context.Context, in FactInput) (bool, error) {
	if strings.TrimSpace(in.Fact) == "" {
		return false, fmt.Errorf("put fact: empty fact text")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
INSERT INTO contact_facts(
    contact_id, fact, category, fact_hash,
    source, source_message_hash, source_ts, source_ts_unix, model, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(contact_id, fact_hash) DO NOTHING`,
		in.ContactID, strings.TrimSpace(in.Fact), in.Category, factHash(in.Fact),
		in.Source, in.SourceMessageHash, in.SourceTS, in.SourceTSUnix, in.Model, now)
	if err != nil {
		return false, fmt.Errorf("put fact: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ContactFactsByConversation returns the facts known about the contact linked to
// the given conversation, ordered by category then chronology, with each fact's
// supporting message resolved to its current rowid (0 if gone). Returns nil for
// a conversation with no linked contact.
func (s *Store) ContactFactsByConversation(ctx context.Context, convID int64) ([]ContactFact, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT f.fact, f.category, f.source, f.source_message_hash,
       COALESCE(m.id, 0), f.source_ts, f.source_ts_unix, f.model
  FROM contact_facts f
  LEFT JOIN messages m ON m.hash = f.source_message_hash
 WHERE f.contact_id = (SELECT contact_id FROM conversations WHERE id = ?)
 ORDER BY f.category ASC, f.source_ts_unix ASC, f.id ASC`, convID)
	if err != nil {
		return nil, fmt.Errorf("contact facts: %w", err)
	}
	defer rows.Close()
	var out []ContactFact
	for rows.Next() {
		var f ContactFact
		if err := rows.Scan(&f.Fact, &f.Category, &f.Source, &f.SourceMessageHash,
			&f.SourceMessageID, &f.SourceTS, &f.SourceTSUnix, &f.Model); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// CountFacts returns the total number of stored contact facts (for progress and
// summaries).
func (s *Store) CountFacts(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_facts`).Scan(&n)
	return n, err
}

// ResetFacts clears all extracted facts and extraction cursors so the next run
// re-derives everything from scratch (e.g. after a prompt or model change).
func (s *Store) ResetFacts(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reset facts: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM contact_facts`); err != nil {
		return fmt.Errorf("reset facts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM fact_state`); err != nil {
		return fmt.Errorf("reset fact state: %w", err)
	}
	return tx.Commit()
}
