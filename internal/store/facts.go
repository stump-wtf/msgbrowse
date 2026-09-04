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
// not a single active one. It is left 0 by the by-conversation fact reader (whose
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
	return s.eligibleConversations(ctx, "fact conversations", exclude)
}

// eligibleConversations answers the question both LLM-backed enrichment passes
// ask: which conversations may be read at all? Linked to a contact, holding at
// least one real (non-system, non-empty) message, and not on the exclude list.
// The exclusion is applied here rather than by the caller so an excluded
// conversation's content is never loaded in the first place. what names the
// operation in the wrapped error.
func (s *Store) eligibleConversations(ctx context.Context, what string, exclude []string) ([]FactConversation, error) {
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
		return nil, fmt.Errorf("%s: %w", what, err)
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
// a conversation with no linked contact. No web surface calls it since #446
// moved facts to the profile (which reads by contact), but it remains the
// provenance-resolving read the facts tests pin their dedup/re-ingest
// guarantees on.
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

// Fact-Run Bookkeeping — The Durable Record Of An Extraction Pass
//
// Everything below backs the Settings → Facts tab's fact-extraction card (#366).
// Until that card existed, fact extraction was CLI-only and left no trace of
// having run at all: a live archive showed 0 contact_facts and 0 fact_state
// rows, which is indistinguishable from "ran and found nothing" unless someone
// remembers whether they ever typed `msgbrowse facts`.
//
// fact_runs (schema v21) is the facts analogue of journal_runs, and exists for
// the same second reason: `msgbrowse facts` and `msgbrowse serve` are separate
// processes sharing one SQLite file, so the table is their only communication
// channel. It is what lets the web layer refuse to start a second extraction
// rather than race one already running elsewhere — and extraction is billable
// outbound LLM work over every eligible conversation, so that refusal is a
// cost control, not a tidiness preference.
//
// Governing: SPEC-0005 (contact-facts) REQ-0005-001 (extraction is a
// deliberate, opt-in pass that performs the only egress), REQ-0005-004
// (incremental cursor — FactCoverage reports it rather than re-deriving it);
// ADR-0011.
//
// @joestump-agent 08/23/2026 - Added with the in-app extraction controls (#366).

// Fixed scope tokens recorded on a fact_runs row. They are TOKENS, not display
// strings: the web layer maps them to prose, so the rendered run history can
// never carry request- or model-derived text. An unrecognized value reads as
// the default whole-archive scope rather than being printed verbatim.
const (
	// FactScopeArchive is the incremental whole-archive pass (stored as '').
	FactScopeArchive = ""
	// FactScopeReset is a wipe-and-re-extract pass.
	FactScopeReset = "reset"
	// FactScopeConversation is a single-conversation run (the CLI's
	// --conversation flag; the web layer never starts one).
	FactScopeConversation = "conversation"
)

// FactRun is one fact-extraction pass. FinishedAt is the zero time while the
// run is still in flight (or died before its terminal write); UpdatedAt is the
// per-conversation heartbeat readers use to tell a live run from a crashed one.
// Conversations/Messages/FactsAdded/Batches are live counters during a run and
// the final totals after it. Error carries the abort reason ("" on success).
type FactRun struct {
	ID    int64
	Model string
	// Scope is one of the FactScope* tokens above.
	Scope         string
	StartedAt     time.Time
	UpdatedAt     time.Time
	FinishedAt    time.Time
	DurationMS    int64
	Conversations int
	Messages      int
	FactsAdded    int
	Batches       int
	Error         string
}

// InFlight reports whether the run has not recorded its terminal write.
func (r FactRun) InFlight() bool { return r.FinishedAt.IsZero() }

const factRunCols = `id, model, scope, started_at, updated_at, finished_at,
                     duration_ms, conversations, messages, facts_added, batches, error`

// BeginFactRun records the start of an extraction pass and returns the row id
// the run's later heartbeat/finish writes target. The heartbeat (updated_at)
// starts equal to startedAt. scope is one of the FactScope* tokens.
func (s *Store) BeginFactRun(ctx context.Context, model, scope string, startedAt time.Time) (int64, error) {
	ts := startedAt.UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO fact_runs (model, scope, started_at, updated_at) VALUES (?, ?, ?, ?)`,
		model, scope, ts, ts)
	if err != nil {
		return 0, fmt.Errorf("begin fact run: %w", err)
	}
	return res.LastInsertId()
}

// UpdateFactRunProgress refreshes a run's live counters and heartbeat after a
// conversation finishes. Readers treat an unfinished row with a fresh heartbeat
// as "extracting"; one whose heartbeat has gone cold reads as interrupted.
func (s *Store) UpdateFactRunProgress(ctx context.Context, id int64, conversations, factsAdded int, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE fact_runs SET conversations = ?, facts_added = ?, updated_at = ? WHERE id = ?`,
		conversations, factsAdded, at.UTC().Format(time.RFC3339), id); err != nil {
		return fmt.Errorf("update fact run progress: %w", err)
	}
	return nil
}

// FinishFactRun records a run's terminal state: finished_at (which flips the
// row out of "in flight"), the final totals, and the abort error when the run
// failed. r.ID selects the row; r.UpdatedAt is stamped to r.FinishedAt.
//
// The caller MUST make this a deferred write. An extraction pass can abort on a
// transport error mid-batch or a cancelled context, and a run that never
// records its terminal state leaves the card reading "extracting…" until the
// heartbeat goes stale — a much worse failure than reporting the error, because
// the stale window is also a window in which no new run may start.
func (s *Store) FinishFactRun(ctx context.Context, r FactRun) error {
	ts := r.FinishedAt.UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE fact_runs
		    SET finished_at = ?, updated_at = ?, duration_ms = ?, conversations = ?,
		        messages = ?, facts_added = ?, batches = ?, error = ?
		  WHERE id = ?`,
		ts, ts, r.DurationMS, r.Conversations, r.Messages, r.FactsAdded, r.Batches, r.Error, r.ID); err != nil {
		return fmt.Errorf("finish fact run: %w", err)
	}
	return nil
}

func scanFactRun(sc interface{ Scan(...any) error }) (FactRun, error) {
	var (
		r                          FactRun
		started, updated, finished string
	)
	if err := sc.Scan(&r.ID, &r.Model, &r.Scope, &started, &updated, &finished,
		&r.DurationMS, &r.Conversations, &r.Messages, &r.FactsAdded, &r.Batches, &r.Error); err != nil {
		return FactRun{}, err
	}
	r.StartedAt = parseRFC3339(started)
	r.UpdatedAt = parseRFC3339(updated)
	if finished != "" {
		r.FinishedAt = parseRFC3339(finished)
	}
	return r, nil
}

// LatestFactRun returns the run whose state the card should report: any
// still-in-flight run in preference to a newer finished one, else the newest
// row. nil when none has ever been recorded. The caller decides what an
// unfinished row means by its heartbeat age (live vs crashed).
func (s *Store) LatestFactRun(ctx context.Context) (*FactRun, error) {
	// In-flight rows sort FIRST, then newest. Plain `ORDER BY id DESC` would
	// hide a run that is still going behind any newer finished row — and this
	// query is the cross-process guard, so hiding a live run means letting a
	// second (billable) one start alongside it.
	row := s.db.QueryRowContext(ctx,
		`SELECT `+factRunCols+`
		   FROM fact_runs
		  ORDER BY (finished_at = '') DESC, id DESC
		  LIMIT 1`)
	r, err := scanFactRun(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest fact run: %w", err)
	}
	return &r, nil
}

// RecentFactRuns returns the most recently started passes, newest first, capped
// at n (n <= 0 yields nothing without touching the database). LatestFactRun
// answers "what is the current state"; this answers "what has extraction done
// lately".
func (s *Store) RecentFactRuns(ctx context.Context, n int) ([]FactRun, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+factRunCols+` FROM fact_runs ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, fmt.Errorf("recent fact runs: %w", err)
	}
	defer rows.Close()

	var out []FactRun
	for rows.Next() {
		r, err := scanFactRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FactCoverage is how much of the archive extraction has looked at, and what it
// produced. Processed counts conversations the extractor has EXAMINED (they
// carry a fact_state row), which is deliberately not the same as conversations
// that yielded facts: a thread of logistics chatter is legitimately processed
// and factless, and conflating the two is exactly the ambiguity #366 exists to
// remove.
type FactCoverage struct {
	// Conversations is the eligible population: linked to a contact, holding at
	// least one real message, not on the exclude list.
	Conversations int
	// Processed is how many of those carry an extraction cursor.
	Processed int
	// Productive is how many of those produced at least one fact.
	Productive int
	// Facts is the total number of stored contact_facts rows.
	Facts int
	// Contacts is how many distinct contacts have at least one fact.
	Contacts int
}

// Remaining is the eligible conversations extraction has never looked at — the
// scope (and therefore the cost) of the next incremental run. It is clamped at
// zero so a stale cursor row for a conversation that has since become
// ineligible can never render a negative count.
func (c FactCoverage) Remaining() int {
	if n := c.Conversations - c.Processed; n > 0 {
		return n
	}
	return 0
}

// FactCoverage assembles the coverage snapshot for the extraction card.
//
// The eligible population is derived through eligibleConversations — the SAME
// query the extractor itself feeds from — so the card's denominator can never
// disagree with what a run would actually process. In particular the exclude
// list is applied here too: without it the card would report "1,200 of 2,438"
// forever on an archive whose remaining threads are all excluded, and the user
// would keep paying for runs chasing a number that cannot move.
func (s *Store) FactCoverage(ctx context.Context, exclude []string) (FactCoverage, error) {
	var c FactCoverage
	convs, err := s.eligibleConversations(ctx, "fact coverage", exclude)
	if err != nil {
		return c, err
	}
	c.Conversations = len(convs)

	// fact_state is one small row per EXAMINED conversation; reading it whole and
	// intersecting in Go avoids building an IN-list over a few thousand ids.
	rows, err := s.db.QueryContext(ctx, `SELECT conversation_id, facts_added FROM fact_state`)
	if err != nil {
		return c, fmt.Errorf("fact coverage: %w", err)
	}
	defer rows.Close()
	state := make(map[int64]int)
	for rows.Next() {
		var id int64
		var added int
		if err := rows.Scan(&id, &added); err != nil {
			return c, err
		}
		state[id] = added
	}
	if err := rows.Err(); err != nil {
		return c, err
	}
	for _, fc := range convs {
		added, ok := state[fc.ID]
		if !ok {
			continue
		}
		c.Processed++
		if added > 0 {
			c.Productive++
		}
	}

	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COUNT(DISTINCT contact_id) FROM contact_facts`).
		Scan(&c.Facts, &c.Contacts); err != nil {
		return c, fmt.Errorf("fact coverage totals: %w", err)
	}
	return c, nil
}

// ContactFactScan reports how much of ONE contact's archive the extractor has
// examined: Scanned is the number of that contact's conversations carrying a
// fact_state cursor, Total is how many they have.
//
// This is what lets the contact page tell "extraction has never run here" from
// "extraction ran and this person simply has no durable facts". Both rendered
// as an identical blank panel before #366, which made a configuration problem
// (no chat model, extraction never started) look like a considered verdict
// about a person.
type ContactFactScan struct {
	Scanned int
	Total   int
}

// Extracted reports whether the extractor has examined ANY of this contact's
// conversations. False means the empty fact list is an absence of work, not a
// finding.
func (s ContactFactScan) Extracted() bool { return s.Scanned > 0 }

// ContactFactScan returns the per-contact extraction-cursor coverage above.
func (s *Store) ContactFactScan(ctx context.Context, contactID int64) (ContactFactScan, error) {
	var out ContactFactScan
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(SUM(CASE WHEN fs.conversation_id IS NOT NULL THEN 1 ELSE 0 END), 0)
  FROM conversations c
  LEFT JOIN fact_state fs ON fs.conversation_id = c.id
 WHERE c.contact_id = ?`, contactID).Scan(&out.Total, &out.Scanned)
	if err != nil {
		return out, fmt.Errorf("contact fact scan: %w", err)
	}
	return out, nil
}

// ReapOrphanFacts deletes facts whose source message no longer exists — a
// re-import can shift timestamps and hashes (DST / export timezone), which
// invalidates citations and left 51% of the live archive's facts orphaned
// (issue #447). Runs at the start of every extraction pass; idempotent.
// Returns how many rows were removed.
//
// @joestump-agent 09/04/2026 - Added with #447.
func (s *Store) ReapOrphanFacts(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
DELETE FROM contact_facts
 WHERE source_message_hash <> ''
   AND source_message_hash NOT IN (SELECT hash FROM messages)`)
	if err != nil {
		return 0, fmt.Errorf("reap orphan facts: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CountOrphanFacts reports how many facts cite a source message that no longer
// exists — the number Settings → Facts shows beside the reap note (issue #447).
func (s *Store) CountOrphanFacts(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM contact_facts
 WHERE source_message_hash <> ''
   AND source_message_hash NOT IN (SELECT hash FROM messages)`).Scan(&n)
	return n, err
}

// NearDuplicate — Similarity threshold and helpers for the insert-time
// paraphrase collapse (issue #449): "Owns a Google Pixel 3 smartphone" and
// "Owns a Pixel 3 phone" are one fact. Pure Go, no model call; the comparison
// set is one contact's facts in one category, which is small.
const (
	nearDupJaccard     = 0.6 // token-set overlap floor
	nearDupContainment = 0.8 // one side nearly contained in the other
	nearDupMinTokens   = 2   // shorter than this, exact-hash dedup is enough
)

// stopwords strips the filler that carries no identity, so "owns a pixel 3
// phone" and "pixel 3 phone" share a token set.
var stopwords = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "and": {}, "or": {}, "of": {}, "in": {},
	"on": {}, "at": {}, "to": {}, "for": {}, "with": {}, "is": {}, "was": {},
	"has": {}, "have": {}, "had": {}, "his": {}, "her": {}, "their": {},
	"he": {}, "she": {}, "they": {}, "it": {}, "its": {}, "uses": {},
	"very": {}, "really": {}, "into": {},
}

// factTokens returns the normalised comparison token set for a fact.
func factTokens(fact string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, tok := range strings.Fields(strings.ToLower(fact)) {
		tok = strings.Trim(tok, ".,!?'\";:")
		if tok == "" {
			continue
		}
		if _, stop := stopwords[tok]; stop {
			continue
		}
		out[tok] = struct{}{}
	}
	return out
}

// nearDuplicate reports whether a and b are paraphrases of the same subject:
// Jaccard similarity of the token sets ≥ 0.6, or containment ≥ 0.8 (the
// shorter set is nearly inside the longer). Category equality is checked by
// the caller — the boundary is per contact AND per category.
// tokenIntersect counts sa's tokens that also appear in sb, tolerating
// morphological variants by substring: "smartphone" contains "phone", so the
// Pixel-3 pair is one subject while "dog" vs "cat" never match.
func tokenIntersect(sa, sb map[string]struct{}) int {
	inter := 0
	for t := range sa {
		if _, hit := sb[t]; hit {
			inter++
			continue
		}
		if len(t) < 5 {
			continue
		}
		for u := range sb {
			if len(u) >= 5 && (strings.Contains(t, u) || strings.Contains(u, t)) {
				inter++
				break
			}
		}
	}
	return inter
}

func nearDuplicate(a, b string) bool {
	if strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)) {
		return true
	}
	sa, sb := factTokens(a), factTokens(b)
	if len(sa) < nearDupMinTokens || len(sb) < nearDupMinTokens {
		return false
	}
	inter := tokenIntersect(sa, sb)
	union := len(sa) + len(sb) - inter
	if union == 0 {
		return false
	}
	if float64(inter)/float64(union) >= nearDupJaccard {
		return true
	}
	minLen := len(sa)
	if len(sb) < minLen {
		minLen = len(sb)
	}
	if minLen == 0 {
		return false
	}
	return float64(inter)/float64(minLen) >= nearDupContainment
}

// PutFactNearDupAware is PutFact with the paraphrase collapse (#449): before
// inserting, the new fact is compared against the contact's existing facts in
// the SAME category. On a near-duplicate hit the existing row wins (kept, its
// source updated only if the new citation is newer) and inserted=false comes
// back. The exact-hash dedup inside PutFact still runs first — it is free.
func (s *Store) PutFactNearDupAware(ctx context.Context, in FactInput) (bool, error) {
	existing, err := s.factsForContactCategory(ctx, in.ContactID, in.Category)
	if err != nil {
		return false, err
	}
	for _, f := range existing {
		if nearDuplicate(f.Fact, in.Fact) {
			// Keep the existing row; refresh its citation when the new one is
			// newer, so provenance tracks the most recent evidence.
			if in.SourceTSUnix > f.SourceTSUnix {
				if _, err := s.db.ExecContext(ctx,
					`UPDATE contact_facts SET source_ts = ?, source_ts_unix = ?, source_message_hash = ?
					  WHERE contact_id = ? AND fact_hash = ?`,
					in.SourceTS, in.SourceTSUnix, in.SourceMessageHash, in.ContactID, factHash(f.Fact)); err != nil {
					return false, fmt.Errorf("put fact: refresh near-dup source: %w", err)
				}
			}
			return false, nil
		}
	}
	return s.PutFact(ctx, in)
}

// factsForContactCategory loads the contact's existing facts in one category
// for the near-duplicate comparison.
func (s *Store) factsForContactCategory(ctx context.Context, contactID int64, category string) ([]ContactFact, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT fact, source_ts_unix FROM contact_facts
 WHERE contact_id = ? AND category = ?`, contactID, category)
	if err != nil {
		return nil, fmt.Errorf("put fact: load existing: %w", err)
	}
	defer rows.Close()
	var out []ContactFact
	for rows.Next() {
		var f ContactFact
		if err := rows.Scan(&f.Fact, &f.SourceTSUnix); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
