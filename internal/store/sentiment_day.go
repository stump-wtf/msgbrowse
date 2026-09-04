package store

// Per-day sentiment rescoring support (issue #441): the day card's refresh
// (#440/#453) needs to redo ONE UTC day's scores, and the incremental pass's
// per-conversation cursor machinery cannot express "rescore 2026-07-11" — the
// day's messages are already behind every cursor. These helpers read a day's
// messages and replace one day's score rows without touching sentiment_state.
//
// The UTC bucket rule is the same one journal_days uses (ADR-0023):
// date(ts_unix,'unixepoch'), never 'localtime', so a message near midnight
// lands in exactly one bucket everywhere in the app.
//
// @joestump-agent 09/04/2026 - Added with #441.

import (
	"context"
	"fmt"
)

// DayMessage is one message read for per-day rescoring: the scoring view plus
// the conversation it belongs to, so the orchestrator can attribute the
// sender's contact and keep batches within one conversation.
type DayMessage struct {
	MessageView
	ConversationID int64
}

// validDay checks the YYYY-MM-DD shape the day queries expect. The value only
// ever reaches a bound parameter, so this is a correctness guard (a malformed
// day would silently match nothing), not an injection defense.
func validDayShape(day string) bool {
	if len(day) != 10 || day[4] != '-' || day[7] != '-' {
		return false
	}
	for i, r := range day {
		if i == 4 || i == 7 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// MessagesForDay returns every scorable message bucketed into the given UTC
// day, across all ELIGIBLE conversations: linked to a contact, not on the
// exclude list (filtered here, before any caller reads message content), and
// not belonging to a contact who opted out of sentiment scoring. System
// messages and empty bodies are excluded — the same real-message rule the
// incremental pass applies before scoring.
func (s *Store) MessagesForDay(ctx context.Context, day string, exclude []string) ([]DayMessage, error) {
	if !validDayShape(day) {
		return nil, fmt.Errorf("messages for day: malformed day %q (want YYYY-MM-DD)", day)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT m.id, m.hash, m.sender, m.is_system, m.ts, m.ts_unix, m.body, m.conversation_id, c.name
  FROM messages m
  JOIN conversations c ON c.id = m.conversation_id
 WHERE date(m.ts_unix,'unixepoch') = ?
   AND c.contact_id IS NOT NULL
   AND m.is_system = 0
   AND TRIM(m.body) <> ''
   AND NOT EXISTS (SELECT 1 FROM contact_sentiment_optout o WHERE o.contact_id = c.contact_id)
 ORDER BY m.conversation_id, m.ts_unix, m.id`, day)
	if err != nil {
		return nil, fmt.Errorf("messages for day: %w", err)
	}
	defer rows.Close()

	excluded := make(map[string]struct{}, len(exclude))
	for _, name := range exclude {
		excluded[name] = struct{}{}
	}

	var out []DayMessage
	for rows.Next() {
		var dm DayMessage
		var convName string
		if err := rows.Scan(&dm.ID, &dm.Hash, &dm.Sender, &dm.IsSystem, &dm.TS, &dm.TSUnix, &dm.Body, &dm.ConversationID, &convName); err != nil {
			return nil, fmt.Errorf("messages for day: scan: %w", err)
		}
		// Excluded conversations are dropped here, after the read but before
		// the caller ever sees a row — their text still transited this process,
		// but never reaches the engine let alone the LLM (the same posture
		// eligibleConversations takes; per-day reads cannot pre-filter by
		// conversation because the day is the selection axis).
		if _, skip := excluded[convName]; skip {
			continue
		}
		out = append(out, dm)
	}
	return out, rows.Err()
}

// DeleteSentimentForDay removes every score row of the given generation whose
// message falls in the UTC day, returning how many rows went away. RunDay
// calls this before rescoring so a re-run cannot leave stale rows behind for
// constructs the new pass no longer produced (the batch inserts are
// conflict-nothing upserts).
func (s *Store) DeleteSentimentForDay(ctx context.Context, day string, gen SentimentGeneration) (int64, error) {
	if !validDayShape(day) {
		return 0, fmt.Errorf("delete sentiment for day: malformed day %q (want YYYY-MM-DD)", day)
	}
	res, err := s.db.ExecContext(ctx, `
DELETE FROM message_sentiment
 WHERE model = ? AND lexicon_version = ?
   AND date(ts_unix,'unixepoch') = ?`,
		gen.Model, gen.LexiconVersion, day)
	if err != nil {
		return 0, fmt.Errorf("delete sentiment for day: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PutSentimentScores writes one batch of scores WITHOUT touching the
// conversation's sentiment_state cursor (issue #441: a per-day re-score must
// not advance cursors past messages the incremental pass has not reached
// yet). The opt-out guard on the write matches PutSentimentBatch: a contact
// who opts out mid-run must not have scores written back anyway.
func (s *Store) PutSentimentScores(ctx context.Context, gen SentimentGeneration, scores []SentimentScore) error {
	if len(scores) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sentiment scores: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sentiment scores: %w", err)
	}
	return nil
}
