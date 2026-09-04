package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// OwnerContactID attributes a score to the archive owner, who has no row in
// contacts (they are identified by sender name, not as a contact). Real contact
// ids are SQLite rowids and therefore start at 1, so 0 is unambiguous.
//
// Owner rows are stored rather than dropped because the journal's mood strip
// aggregates everyone in a day, including you; the contact-profile surfaces
// filter to a specific contact id and so never pick them up.
const OwnerContactID int64 = 0

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

// The Sentiment Run Log, Coverage, And Read-Side Aggregates (#367)
//
// Everything above this line is the WRITE side that shipped with the scoring
// engine (#310/#311). Everything below is what #367 needed and found missing:
// a durable record that a run happened, a coverage snapshot that can tell
// "never scored" from "scored, found nothing", and the generation-pinned
// aggregates the three consumer surfaces (#313) read.
//
// The context is worth stating once, because the issue that prompted this got
// it backwards. Sentiment scoring is NOT a free local lexicon pass:
// sentiment.Run refuses without a chat model and makes one llm.Chat call per
// batch, the IPIP lexicon being the anchor set rendered into the system prompt
// (internal/sentiment/prompt.go). ADR-0028 rejected a classical local lexicon
// outright and calls corpus scoring "the most expensive extraction". So the run
// log below is not bookkeeping decoration — it is the cross-process guard that
// stops a second billable pass starting alongside one already in flight.
//
// Two invariants run through every read here, and neither is optional:
//
//   - GENERATION PINNING. Scores are only comparable within one
//     (model, lexicon_version) pair, so every aggregate takes a generation and
//     an unset one returns nothing rather than averaging incomparable rows
//     together (SPEC-0027, "sparse, generation-stamped storage").
//   - OPT-OUT AT READ TIME. contact_sentiment_optout is enforced with a
//     NOT EXISTS INSIDE each query, exactly as PutSentimentBatch guards its
//     writes, and never as a caller-supplied id list. SPEC-0027 makes opt-out
//     DELETION rather than suppression, so in a settled database those rows are
//     already gone — but a contact who opts out while a long run is in flight
//     can have scores written back seconds later, and their affect must not
//     reach a rendered surface even for the minutes before the next cleanup.
//     Making it a parameter would leave the privacy guarantee to whether each
//     future caller remembered to pass it, which is not a guarantee.
//
// The aggregates are deliberately shaped as per-(bucket, construct) rows rather
// than as finished moods or trait sketches: which constructs belong to which
// tier, and which read as pleasant or unpleasant, is taxonomy that lives with
// internal/sentiment's lexicon, and internal/store cannot import it without
// closing a cycle. The web layer folds these rows using
// internal/sentiment.AffectValence. At most a few hundred rows per surface, so
// folding in Go costs nothing.
//
// Governing: SPEC-0027 (sentiment), ADR-0028 (IPIP-anchored scoring and its
// cost), ADR-0023 (the UTC day rule the day bucket obeys — date(ts_unix,
// 'unixepoch') with NO 'localtime'), ADR-0002 (message_sentiment is a derived
// cache keyed by content hash).
//
// @joestump-agent 08/23/2026 - Added with the in-app scoring controls (#367).

// Fixed scope tokens recorded on a sentiment_runs row. They are TOKENS, not
// display strings: the web layer maps them to prose, so nothing stored here can
// reach the rendered run-history table verbatim.
const (
	// SentimentScopeArchive is the incremental whole-archive pass (stored as '').
	SentimentScopeArchive = ""
	// SentimentScopeReset is a wipe-and-rescore pass.
	SentimentScopeReset = "reset"
	// SentimentScopeDayPrefix marks a per-day re-score run (#441); the stored
	// token is the prefix plus the UTC day (day:YYYY-MM-DD).
	SentimentScopeDayPrefix = "day:"
	// SentimentScopeConversation is a single-conversation run (the CLI's
	// --conversation flag; the web controls never produce one).
	SentimentScopeConversation = "conversation"
)

// SentimentRun is one scoring pass. FinishedAt is the zero time while the run is
// still in flight (or died before its terminal write); UpdatedAt is the
// per-conversation heartbeat readers use to tell a live run from a crashed one.
// Error carries the abort reason ("" on success).
type SentimentRun struct {
	ID    int64
	Model string
	// LexiconVersion is the curation the run scored under. Together with Model
	// it is the generation stamped on every row the run wrote.
	LexiconVersion string
	// Scope is one of the SentimentScope* tokens above.
	Scope         string
	StartedAt     time.Time
	UpdatedAt     time.Time
	FinishedAt    time.Time
	DurationMS    int64
	Conversations int
	Messages      int
	ScoresWritten int
	Batches       int
	Error         string
}

// InFlight reports whether the run has not recorded its terminal write.
func (r SentimentRun) InFlight() bool { return r.FinishedAt.IsZero() }

const sentimentRunCols = `id, model, lexicon_version, scope, started_at, updated_at, finished_at,
                          duration_ms, conversations, messages, scores_written, batches, error`

// BeginSentimentRun records the start of a scoring pass and returns the row id
// the run's later heartbeat/finish writes target. The heartbeat (updated_at)
// starts equal to startedAt. scope is one of the SentimentScope* tokens.
func (s *Store) BeginSentimentRun(ctx context.Context, model, lexiconVersion, scope string, startedAt time.Time) (int64, error) {
	ts := startedAt.UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO sentiment_runs (model, lexicon_version, scope, started_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		model, lexiconVersion, scope, ts, ts)
	if err != nil {
		return 0, fmt.Errorf("begin sentiment run: %w", err)
	}
	return res.LastInsertId()
}

// UpdateSentimentRunProgress refreshes a run's live counters and heartbeat after
// a conversation finishes. Readers treat an unfinished row with a fresh
// heartbeat as "scoring"; one whose heartbeat has gone cold reads as
// interrupted.
func (s *Store) UpdateSentimentRunProgress(ctx context.Context, id int64, conversations, scoresWritten int, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sentiment_runs SET conversations = ?, scores_written = ?, updated_at = ? WHERE id = ?`,
		conversations, scoresWritten, at.UTC().Format(time.RFC3339), id); err != nil {
		return fmt.Errorf("update sentiment run progress: %w", err)
	}
	return nil
}

// FinishSentimentRun records a run's terminal state: finished_at (which flips
// the row out of "in flight"), the final totals, and the abort error when the
// run failed. r.ID selects the row; r.UpdatedAt is stamped to r.FinishedAt.
//
// The caller MUST make this a deferred write. A scoring pass can abort on a
// transport error mid-batch or a cancelled context, and a run that never records
// its terminal state leaves the card reading "scoring…" until the heartbeat goes
// stale — a much worse failure than reporting the error, because the stale
// window is also a window in which no new run may start.
func (s *Store) FinishSentimentRun(ctx context.Context, r SentimentRun) error {
	ts := r.FinishedAt.UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sentiment_runs
		    SET finished_at = ?, updated_at = ?, duration_ms = ?, conversations = ?,
		        messages = ?, scores_written = ?, batches = ?, error = ?
		  WHERE id = ?`,
		ts, ts, r.DurationMS, r.Conversations, r.Messages, r.ScoresWritten, r.Batches, r.Error, r.ID); err != nil {
		return fmt.Errorf("finish sentiment run: %w", err)
	}
	return nil
}

func scanSentimentRun(sc interface{ Scan(...any) error }) (SentimentRun, error) {
	var (
		r                          SentimentRun
		started, updated, finished string
	)
	if err := sc.Scan(&r.ID, &r.Model, &r.LexiconVersion, &r.Scope, &started, &updated, &finished,
		&r.DurationMS, &r.Conversations, &r.Messages, &r.ScoresWritten, &r.Batches, &r.Error); err != nil {
		return SentimentRun{}, err
	}
	r.StartedAt = parseRFC3339(started)
	r.UpdatedAt = parseRFC3339(updated)
	if finished != "" {
		r.FinishedAt = parseRFC3339(finished)
	}
	return r, nil
}

// LatestSentimentRun returns the run whose state the card should report: any
// still-in-flight run in preference to a newer finished one, else the newest
// row. nil when none has ever been recorded. The caller decides what an
// unfinished row means by its heartbeat age (live vs crashed).
//
// In-flight rows sort FIRST, then newest. Plain `ORDER BY id DESC` would hide a
// run that is still going behind any newer finished row — and this query is the
// cross-process guard, so hiding a live run means letting a second (billable)
// one start alongside it.
func (s *Store) LatestSentimentRun(ctx context.Context) (*SentimentRun, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sentimentRunCols+`
		   FROM sentiment_runs
		  ORDER BY (finished_at = '') DESC, id DESC
		  LIMIT 1`)
	r, err := scanSentimentRun(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest sentiment run: %w", err)
	}
	return &r, nil
}

// RecentSentimentRuns returns the most recently started passes, newest first,
// capped at n (n <= 0 yields nothing without touching the database).
// LatestSentimentRun answers "what is the current state"; this answers "what has
// scoring done lately".
func (s *Store) RecentSentimentRuns(ctx context.Context, n int) ([]SentimentRun, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sentimentRunCols+` FROM sentiment_runs ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, fmt.Errorf("recent sentiment runs: %w", err)
	}
	defer rows.Close()

	var out []SentimentRun
	for rows.Next() {
		r, err := scanSentimentRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SentimentCoverage is how much of the archive scoring has looked at, and what
// it produced.
//
// Processed counts conversations the engine has EXAMINED (they carry a
// sentiment_state cursor), which is deliberately not the same as conversations
// that produced scores: storage is sparse by design — a thread of "ok, see you
// at 6" is legitimately scored and rowless — and conflating the two would make a
// working pipeline look broken.
type SentimentCoverage struct {
	// Conversations is the eligible population: linked to a contact, holding at
	// least one real message, not on the exclude list.
	Conversations int
	// Processed is how many of those carry a scoring cursor.
	Processed int
	// Productive is how many of those produced at least one score row under the
	// current generation.
	Productive int
	// Scores is the number of stored score rows for the generation, Messages the
	// distinct messages they cover, and Contacts the distinct people they are
	// attributed to (the archive owner included; see OwnerContactID).
	Scores   int
	Messages int
	Contacts int
	// OptedOut is how many contacts have excluded themselves from scoring. The
	// card states it so a shrinking denominator is explained rather than
	// mysterious.
	OptedOut int
}

// Remaining is the eligible conversations scoring has never looked at — the
// scope, and therefore the COST, of the next incremental run. Clamped at zero so
// a stale cursor row for a conversation that has since become ineligible can
// never render a negative count.
func (c SentimentCoverage) Remaining() int {
	if n := c.Conversations - c.Processed; n > 0 {
		return n
	}
	return 0
}

// SentimentCoverage assembles the coverage snapshot for the scoring card.
//
// The eligible population is derived through eligibleConversations — the SAME
// query the engine itself feeds from — so the card's denominator can never
// disagree with what a run would actually process. The exclude list is applied
// there for the same reason it is in FactCoverage: without it the card would
// report a percentage that can never move on an archive whose remaining threads
// are all denylisted, and the user would keep paying for runs chasing it.
//
// Opted-out contacts are NOT subtracted from Conversations. The engine skips
// them per run and reports the count separately, and quietly shrinking the
// denominator would make the coverage figure jump for a privacy reason the card
// never explained.
func (s *Store) SentimentCoverage(ctx context.Context, gen SentimentGeneration, exclude []string) (SentimentCoverage, error) {
	var c SentimentCoverage
	convs, err := s.eligibleConversations(ctx, "sentiment coverage", exclude)
	if err != nil {
		return c, err
	}
	c.Conversations = len(convs)

	// idSet reads a single-column id query into a set. Each read is fully
	// drained and CLOSED before the next starts: holding two open cursors over
	// one SQLite file while a scoring run is writing is a needless way to meet
	// SQLITE_BUSY.
	idSet := func(what, q string, args ...any) (map[int64]struct{}, error) {
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		defer rows.Close()
		out := make(map[int64]struct{})
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			out[id] = struct{}{}
		}
		return out, rows.Err()
	}

	// sentiment_state is one small row per EXAMINED conversation; reading it
	// whole and intersecting in Go avoids building an IN-list over a few
	// thousand ids. Only cursors stamped with the CURRENT generation count as
	// processed — a cursor from an older model is work the next run will redo.
	state, err := idSet("sentiment coverage cursors",
		`SELECT conversation_id FROM sentiment_state WHERE model = ? AND lexicon_version = ?`,
		gen.Model, gen.LexiconVersion)
	if err != nil {
		return c, err
	}

	// Which conversations actually produced rows, resolved by joining scores
	// back to their messages. message_sentiment has no conversation column (it
	// is keyed by content hash so it survives a re-ingest — ADR-0002), and
	// messages.hash is UNIQUE so the join cannot fan out.
	productive, err := idSet("sentiment coverage productive", `
SELECT DISTINCT m.conversation_id
  FROM message_sentiment ms
  JOIN messages m ON m.hash = ms.message_hash
 WHERE ms.model = ? AND ms.lexicon_version = ?`, gen.Model, gen.LexiconVersion)
	if err != nil {
		return c, err
	}

	for _, sc := range convs {
		if _, ok := state[sc.ID]; ok {
			c.Processed++
		}
		if _, ok := productive[sc.ID]; ok {
			c.Productive++
		}
	}

	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*), COUNT(DISTINCT message_hash), COUNT(DISTINCT contact_id)
  FROM message_sentiment WHERE model = ? AND lexicon_version = ?`,
		gen.Model, gen.LexiconVersion).Scan(&c.Scores, &c.Messages, &c.Contacts); err != nil {
		return c, fmt.Errorf("sentiment coverage totals: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM contact_sentiment_optout`).Scan(&c.OptedOut); err != nil {
		return c, fmt.Errorf("sentiment opt-out count: %w", err)
	}
	return c, nil
}

// SentimentBucketConstruct is one (time bucket, construct) aggregate: the summed
// signed score and how many message-level rows it came from. Sum/N is the
// construct's mean for that bucket.
//
// Bucket is whatever grain the query that produced it uses — "YYYY-MM" for the
// contact profile's month series, "YYYY-MM-DD" for the journal's per-day strip,
// and "" for a whole-archive roll-up. Keeping one row type for all three means
// the web layer folds them with one function rather than three that can drift.
type SentimentBucketConstruct struct {
	Bucket    string
	Construct string
	Sum       float64
	N         int
}

// sentimentAggregate is the shared body of the read aggregates: one generation,
// one optional contact, one optional time window, grouped by a caller-supplied
// SQLite date expression over ts_unix.
//
// bucketExpr is NEVER caller data — every call site passes one of the three
// literals defined below it. It is a parameter only because SQLite cannot
// parameterise a GROUP BY expression.
//
// The three filters that are always applied, and why, are in the file header:
// the generation pin, the contact_sentiment_optout NOT EXISTS, and the system /
// empty-body exclusion that keeps the denominator the same set of messages the
// engine scored (realMessages in internal/sentiment).
func (s *Store) sentimentAggregate(ctx context.Context, bucketExpr string, gen SentimentGeneration,
	contactID *int64, fromUnix, toUnix *int64, exclude []string) ([]SentimentBucketConstruct, error) {
	if gen.Model == "" || gen.LexiconVersion == "" {
		return nil, nil // nothing has been scored under a known generation
	}
	excl, err := s.excludedConversationIDs(ctx, exclude)
	if err != nil {
		return nil, err
	}

	args := []any{gen.Model, gen.LexiconVersion}
	q := `
SELECT ` + bucketExpr + ` b, ms.construct, SUM(ms.score), COUNT(*)
  FROM message_sentiment ms
  JOIN messages m ON m.hash = ms.message_hash
 WHERE ms.model = ? AND ms.lexicon_version = ?
   AND m.is_system = 0 AND TRIM(m.body) <> ''
   AND NOT EXISTS (SELECT 1 FROM contact_sentiment_optout o WHERE o.contact_id = ms.contact_id)`
	if contactID != nil {
		q += ` AND ms.contact_id = ?`
		args = append(args, *contactID)
	}
	if fromUnix != nil {
		q += ` AND ms.ts_unix >= ?`
		args = append(args, *fromUnix)
	}
	if toUnix != nil {
		q += ` AND ms.ts_unix < ?`
		args = append(args, *toUnix)
	}
	q += notInClause("m.conversation_id", excl, &args)
	q += ` GROUP BY b, ms.construct ORDER BY b, ms.construct`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("sentiment aggregate: %w", err)
	}
	defer rows.Close()
	var out []SentimentBucketConstruct
	for rows.Next() {
		var a SentimentBucketConstruct
		if err := rows.Scan(&a.Bucket, &a.Construct, &a.Sum, &a.N); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// The three bucket expressions. UTC in every case: ADR-0023 fixes the journal's
// day frame as date(ts_unix,'unixepoch') with NO 'localtime' conversion, and the
// month series uses the same frame so a month boundary cannot disagree with the
// days inside it.
const (
	sentimentBucketMonth = `strftime('%Y-%m', ms.ts_unix, 'unixepoch')`
	sentimentBucketDay   = `date(ms.ts_unix,'unixepoch')`
	sentimentBucketNone  = `''`
)

// ContactSentimentMonths returns one contact's scores aggregated per (UTC month,
// construct) for a generation, oldest month first — the series behind the
// profile's sentiment-over-time surface (SPEC-0027, #313).
func (s *Store) ContactSentimentMonths(ctx context.Context, contactID int64, gen SentimentGeneration) ([]SentimentBucketConstruct, error) {
	return s.sentimentAggregate(ctx, sentimentBucketMonth, gen, &contactID, nil, nil, nil)
}

// ContactSentimentConstructs returns one contact's scores aggregated per
// construct across their whole history — the input to the Big Five trait sketch,
// whose domain means are Sum/N per domain construct.
func (s *Store) ContactSentimentConstructs(ctx context.Context, contactID int64, gen SentimentGeneration) ([]SentimentBucketConstruct, error) {
	return s.sentimentAggregate(ctx, sentimentBucketNone, gen, &contactID, nil, nil, nil)
}

// DaySentiment returns one UTC day's scores aggregated per construct — the
// journal day view's mood strip (SPEC-0027, ADR-0023).
//
// The window is a sargable ts_unix range on idx_message_sentiment_ts rather than
// a date() comparison, and it is anchored on the SAME UTC midnight the journal's
// mechanical rollup uses, so a message that straddles local midnight lands in
// the same bucket on both surfaces. day is "YYYY-MM-DD"; an unparseable day
// returns nothing rather than silently widening the window.
func (s *Store) DaySentiment(ctx context.Context, day string, gen SentimentGeneration, exclude []string) ([]SentimentBucketConstruct, error) {
	t, err := time.ParseInLocation("2006-01-02", day, time.UTC)
	if err != nil {
		return nil, nil
	}
	from, to := t.Unix(), t.AddDate(0, 0, 1).Unix()
	return s.sentimentAggregate(ctx, sentimentBucketDay, gen, nil, &from, &to, exclude)
}

// ContactScoredMessages counts the DISTINCT messages of one contact that carry
// at least one score under a generation — the number the trait sketch's minimum
// threshold is applied to (SPEC-0027: render only at >= 50, to avoid false
// precision).
//
// It counts messages, not rows: storage is sparse and one expressive message can
// produce a dozen construct rows, so counting rows would clear a
// message-count threshold on a handful of messages.
//
// The opt-out guard is here too, so an opted-out contact reports zero scored
// messages and every threshold test fails closed.
func (s *Store) ContactScoredMessages(ctx context.Context, contactID int64, gen SentimentGeneration) (int, error) {
	if gen.Model == "" || gen.LexiconVersion == "" {
		return 0, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(DISTINCT ms.message_hash)
  FROM message_sentiment ms
 WHERE ms.model = ? AND ms.lexicon_version = ? AND ms.contact_id = ?
   AND NOT EXISTS (SELECT 1 FROM contact_sentiment_optout o WHERE o.contact_id = ms.contact_id)`,
		gen.Model, gen.LexiconVersion, contactID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("contact scored messages: %w", err)
	}
	return n, nil
}
