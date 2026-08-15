package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SentimentConversation identifies a conversation eligible for sentiment
// scoring. Eligibility is the same question fact extraction asks — linked to a
// contact, holding at least one real message — so it is the same type, resolved
// by the same query (see eligibleConversations). Keeping it an alias rather than
// a parallel struct means the two orchestrators cannot drift apart silently.
type SentimentConversation = FactConversation

// SentimentScore is one construct's score for one message, as written by the
// scoring engine. ContactID is the resolved *sender* at scoring time, which is
// what both consumer surfaces aggregate on; TSUnix is copied from the message
// so aggregates never have to join back through messages.
type SentimentScore struct {
	MessageHash string
	Construct   string
	Score       float64
	TSUnix      int64
	ContactID   int64
}

// SentimentGeneration is the (model, lexicon_version) pair every score is
// stamped with. Scores from different generations are not comparable, so both
// writes and reads carry it explicitly rather than defaulting it.
type SentimentGeneration struct {
	Model          string
	LexiconVersion string
}

// SentimentConversations returns every conversation eligible for scoring —
// linked to a contact, holding real messages, and not on the exclude list.
// Excluded conversations are filtered by name here, before any caller reads
// message content, so their text never reaches the engine let alone the LLM.
func (s *Store) SentimentConversations(ctx context.Context, exclude []string) ([]SentimentConversation, error) {
	return s.eligibleConversations(ctx, "sentiment conversations", exclude)
}

// GetSentimentState returns the scoring cursor for a conversation: the hash of
// the last message scored, and the generation that scored it. ok is false when
// the conversation has never been scored.
//
// The caller compares the returned generation against the current one: a
// difference means the conversation must be rescanned from the top, because its
// stored scores belong to a generation the read side no longer looks at.
func (s *Store) GetSentimentState(ctx context.Context, convID int64) (lastHash string, gen SentimentGeneration, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT last_message_hash, model, lexicon_version FROM sentiment_state WHERE conversation_id = ?`, convID).
		Scan(&lastHash, &gen.Model, &gen.LexiconVersion)
	if err == sql.ErrNoRows {
		return "", SentimentGeneration{}, false, nil
	}
	if err != nil {
		return "", SentimentGeneration{}, false, fmt.Errorf("get sentiment state: %w", err)
	}
	return lastHash, gen, true, nil
}

// PutSentimentBatch writes one batch's scores and advances the conversation's
// cursor in a single transaction, so a crash can never leave the cursor ahead
// of the scores it claims to cover (SPEC-0027: "multi-step writes MUST occur in
// a transaction").
//
// Writes are idempotent upserts: rescanning a conversation, or resuming after a
// failure that lost the cursor, re-inserts nothing and overwrites nothing.
// Passing an empty scores slice is valid and still advances the cursor — a
// batch where the model found nothing salient is progress, not a no-op.
//
// Every insert is guarded on contact_sentiment_optout inside the write
// transaction. Callers filter opted-out contacts before scoring, but that filter
// is read once at the start of a run: a contact who opts out while a long run is
// in flight would otherwise have scores written back seconds after
// SetSentimentOptOut deleted them, leaving them marked opted out and scored
// anyway. SPEC-0027 makes opt-out deletion rather than suppression, so the
// invariant belongs on the write itself, not only on the caller.
func (s *Store) PutSentimentBatch(ctx context.Context, convID int64, gen SentimentGeneration, lastHash string, scores []SentimentScore) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sentiment batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if len(scores) > 0 {
		stmt, err := tx.PrepareContext(ctx, `
INSERT INTO message_sentiment(message_hash, model, lexicon_version, construct, score, ts_unix, contact_id)
SELECT ?, ?, ?, ?, ?, ?, ?
 WHERE NOT EXISTS (SELECT 1 FROM contact_sentiment_optout o WHERE o.contact_id = ?)
ON CONFLICT(message_hash, model, lexicon_version, construct) DO NOTHING`)
		if err != nil {
			return fmt.Errorf("prepare sentiment insert: %w", err)
		}
		defer stmt.Close()

		for _, sc := range scores {
			if _, err := stmt.ExecContext(ctx, sc.MessageHash, gen.Model, gen.LexiconVersion, sc.Construct, sc.Score, sc.TSUnix, sc.ContactID, sc.ContactID); err != nil {
				return fmt.Errorf("insert sentiment score (%s/%s): %w", sc.MessageHash, sc.Construct, err)
			}
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `
INSERT INTO sentiment_state(conversation_id, last_message_hash, model, lexicon_version, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(conversation_id) DO UPDATE SET
    last_message_hash = excluded.last_message_hash,
    model             = excluded.model,
    lexicon_version   = excluded.lexicon_version,
    updated_at        = excluded.updated_at`,
		convID, lastHash, gen.Model, gen.LexiconVersion, now); err != nil {
		return fmt.Errorf("advance sentiment cursor: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sentiment batch: %w", err)
	}
	return nil
}

// CountSentimentScores returns how many score rows exist for a generation.
func (s *Store) CountSentimentScores(ctx context.Context, gen SentimentGeneration) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM message_sentiment WHERE model = ? AND lexicon_version = ?`,
		gen.Model, gen.LexiconVersion).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count sentiment scores: %w", err)
	}
	return n, nil
}

// ResetSentiment drops every score and cursor, so the next run rescans from the
// top under the current generation. Opt-outs are deliberately NOT cleared: a
// reset is a "rebuild the derived data" lever, and silently re-enabling scoring
// for a contact who asked to be excluded would be the worst possible reading of
// it.
func (s *Store) ResetSentiment(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sentiment reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, q := range []string{`DELETE FROM message_sentiment`, `DELETE FROM sentiment_state`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("reset sentiment: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sentiment reset: %w", err)
	}
	return nil
}

// SentimentOptedOut returns the set of contact ids excluded from scoring.
func (s *Store) SentimentOptedOut(ctx context.Context) (map[int64]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT contact_id FROM contact_sentiment_optout`)
	if err != nil {
		return nil, fmt.Errorf("sentiment opt-outs: %w", err)
	}
	defer rows.Close()

	out := map[int64]struct{}{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

// IsSentimentOptedOut reports whether a contact is excluded from scoring.
func (s *Store) IsSentimentOptedOut(ctx context.Context, contactID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM contact_sentiment_optout WHERE contact_id = ?`, contactID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("is sentiment opted out: %w", err)
	}
	return true, nil
}

// SetSentimentOptOut opts a contact out of scoring and deletes every score
// already attributed to them, in one transaction — opting out is retroactive by
// design (SPEC-0027: deletion, not suppression), and a partial application that
// left the rows behind would be a privacy failure rather than a bug.
//
// Opting back in clears the marker but does not rebuild history; cursors are
// per-conversation, so the engine only re-derives the deleted rows on --reset.
func (s *Store) SetSentimentOptOut(ctx context.Context, contactID int64, optOut bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sentiment opt-out: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if optOut {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM message_sentiment WHERE contact_id = ?`, contactID); err != nil {
			return fmt.Errorf("delete scores on opt-out: %w", err)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO contact_sentiment_optout(contact_id, created_at) VALUES (?, ?)
             ON CONFLICT(contact_id) DO NOTHING`, contactID, now); err != nil {
			return fmt.Errorf("record sentiment opt-out: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx,
		`DELETE FROM contact_sentiment_optout WHERE contact_id = ?`, contactID); err != nil {
		return fmt.Errorf("clear sentiment opt-out: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sentiment opt-out: %w", err)
	}
	return nil
}
