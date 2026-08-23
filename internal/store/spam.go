package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Sender statuses. Promotion is one-way: a scan may lift 'seen' to 'watch', but
// it never overwrites a status a person set by hand.
const (
	// SpamStatusSeen is a stranger who has messaged you and tripped no rule.
	// Recorded rather than discarded so a sender who becomes a problem already
	// has history on file.
	SpamStatusSeen = "seen"
	// SpamStatusWatch is set automatically when a scan sees at least one rule fire.
	SpamStatusWatch = "watch"
	// SpamStatusTracked is set by hand. This is the set a dossier is for.
	SpamStatusTracked = "tracked"
	// SpamStatusIgnored is set by hand: you decided this is not spam.
	SpamStatusIgnored = "ignored"
)

// Consent states. The default is deliberately not "unknown": a prior business
// relationship is the usual defense, and the record has to say plainly that
// none is on file until someone says otherwise.
const (
	SpamConsentNone     = "no_consent_on_record"
	SpamConsentGiven    = "consent_given"
	SpamConsentRevoked  = "consent_revoked"
	SpamConsentDisputed = "disputed"
)

// Message directions recorded on a finding.
const (
	SpamInbound  = "inbound"
	SpamOutbound = "outbound"
)

// SpamConversation is a one-to-one thread a scan may examine. Unlike
// eligibleConversations (which fact extraction and sentiment share) this does
// NOT require contact_id to be set: the interesting threads here are precisely
// the ones nobody has reconciled to a person.
type SpamConversation struct {
	ID        int64
	Source    string
	Name      string
	ContactID sql.NullInt64
}

// SpamSender is one non-contact counterparty. The derived halves
// (first/last seen) are rewritten by every scan; status, suspected_entity,
// consent_* and notes are human judgments a scan never touches.
type SpamSender struct {
	ID               int64
	Source           string
	Identifier       string
	ConversationName string
	Status           string
	SuspectedEntity  string
	ConsentStatus    string
	ConsentNotes     string
	Notes            string
	FirstSeenUnix    int64
	LastSeenUnix     int64
	CreatedAt        string
	UpdatedAt        string
}

// SpamFinding is one message's scan result, stamped with the ruleset that
// produced it.
type SpamFinding struct {
	MessageHash   string
	Source        string
	Identifier    string
	Direction     string
	TSUnix        int64
	Reasons       []string
	URLs          []string
	Phones        []string
	Emails        []string
	Names         []string
	Entities      []string
	IsCandidate   bool
	IsAfterOptOut bool
}

// SpamEvent is something that happened to a sender, inside the message stream
// (an opt-out a scan detected, origin "scan") or outside it (a complaint you
// filed, origin "manual").
type SpamEvent struct {
	ID          int64
	Source      string
	Identifier  string
	EventType   string
	EventAt     string
	EventAtUnix int64
	Details     string
	Origin      string
	MessageHash string
}

// SpamDossierMessage is a finding joined back to the message it describes. Body
// is the importer's stored text, unaltered — this is the evidence, and every
// other field on the row is an index into it.
type SpamDossierMessage struct {
	SpamFinding
	Body     string
	Sender   string
	TS       string
	IsSystem bool
	// Present is false when the finding's message hash no longer resolves —
	// the conversation was re-exported and this message is gone. The dossier
	// renders it as a gap rather than dropping it silently.
	Present bool
}

// SpamSenderCounts are the aggregate tallies the senders/violations reports show.
type SpamSenderCounts struct {
	Inbound     int
	Outbound    int
	Candidates  int
	AfterOptOut int
}

// SpamConversations returns every non-group conversation holding at least one
// real message, minus the exclude list. The exclusion is applied here, before
// any caller reads a body, matching the posture of the LLM passes even though
// this one performs no egress.
//
// Group threads are skipped: spam to a group is rare and a group message cannot
// be attributed to a single counterparty, which is the unit the whole feature
// is keyed on.
func (s *Store) SpamConversations(ctx context.Context, exclude []string) ([]SpamConversation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT c.id, c.source, c.name, c.contact_id
  FROM conversations c
 WHERE c.is_group = 0
   AND EXISTS (
       SELECT 1 FROM messages m
        WHERE m.conversation_id = c.id AND m.is_system = 0 AND TRIM(m.body) <> ''
   )
 ORDER BY c.id`)
	if err != nil {
		return nil, fmt.Errorf("spam conversations: %w", err)
	}
	defer rows.Close()

	excluded := make(map[string]struct{}, len(exclude))
	for _, name := range exclude {
		excluded[name] = struct{}{}
	}
	var out []SpamConversation
	for rows.Next() {
		var c SpamConversation
		if err := rows.Scan(&c.ID, &c.Source, &c.Name, &c.ContactID); err != nil {
			return nil, err
		}
		if _, skip := excluded[c.Name]; skip {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetSpamState returns the scan cursor for a conversation: the hash of the last
// message examined and the ruleset version that examined it. A stored version
// different from the current one means the caller must rescan from the top,
// because those findings answer a different question.
func (s *Store) GetSpamState(ctx context.Context, convID int64) (lastHash, version string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT last_message_hash, ruleset_version FROM spam_state WHERE conversation_id = ?`, convID).
		Scan(&lastHash, &version)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("get spam state: %w", err)
	}
	return lastHash, version, true, nil
}

// PutSpamBatch writes one batch of findings, any opt-out events detected in it,
// the sender's first/last-seen window, and the conversation cursor — all in a
// single transaction, so a crash can never leave the cursor ahead of the
// evidence it claims to cover.
//
// Every write is an idempotent upsert, so resuming after a lost cursor
// re-derives the same rows rather than duplicating them. The sender row is
// created if absent and promoted seen→watch when a candidate appears, but its
// human-set columns are never touched here.
//
// env is the scan environment (SPEC-0028 REQ-0028-013): "provider/availability"
// as built by spam.scanEnv. It is stamped onto every finding and onto the
// cursor so a row records the stranger predicate that produced it, which
// ruleset_version alone cannot express — see schemaV20. It deliberately does
// NOT participate in the cursor's rescan decision: a changed address book is
// not a changed ruleset, and forcing a full re-derive on every switch between
// the desktop app and the CLI is exactly the cost ADR-0029 §2 avoids.
func (s *Store) PutSpamBatch(ctx context.Context, convID int64, version, env string, lastHash string, sender SpamSender, findings []SpamFinding, events []SpamEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin spam batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339)

	if sender.Identifier != "" {
		if err := upsertSpamSenderTx(ctx, tx, sender, findingsHaveCandidate(findings), now); err != nil {
			return err
		}
	}

	if len(findings) > 0 {
		stmt, err := tx.PrepareContext(ctx, `
INSERT INTO spam_findings(message_hash, ruleset_version, source, identifier, direction, ts_unix,
                          reasons, urls, phones, emails, names_matched, entities, is_candidate,
                          scan_env)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(message_hash, ruleset_version) DO UPDATE SET
    source        = excluded.source,
    identifier    = excluded.identifier,
    direction     = excluded.direction,
    ts_unix       = excluded.ts_unix,
    reasons       = excluded.reasons,
    urls          = excluded.urls,
    phones        = excluded.phones,
    emails        = excluded.emails,
    names_matched = excluded.names_matched,
    entities      = excluded.entities,
    is_candidate  = excluded.is_candidate,
    scan_env  = excluded.scan_env`)
		// is_after_optout is deliberately NOT written here. It is owned by
		// RecomputeSpamAfterOptOut, which rewrites the whole generation after
		// every scan and after every manually recorded opt-out; letting a batch
		// upsert clear it would make the column depend on scan order.
		if err != nil {
			return fmt.Errorf("prepare spam finding insert: %w", err)
		}
		defer stmt.Close()
		for _, f := range findings {
			if _, err := stmt.ExecContext(ctx, f.MessageHash, version, f.Source, f.Identifier, f.Direction, f.TSUnix,
				jsonList(f.Reasons), jsonList(f.URLs), jsonList(f.Phones), jsonList(f.Emails),
				jsonList(f.Names), jsonList(f.Entities), boolInt(f.IsCandidate), env); err != nil {
				return fmt.Errorf("insert spam finding %s: %w", f.MessageHash, err)
			}
		}
	}

	for _, e := range events {
		if err := insertSpamEventTx(ctx, tx, e, now); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO spam_state(conversation_id, last_message_hash, ruleset_version, scan_env, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(conversation_id) DO UPDATE SET
    last_message_hash = excluded.last_message_hash,
    ruleset_version   = excluded.ruleset_version,
    scan_env      = excluded.scan_env,
    updated_at        = excluded.updated_at`,
		convID, lastHash, version, env, now); err != nil {
		return fmt.Errorf("advance spam cursor: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit spam batch: %w", err)
	}
	return nil
}

// upsertSpamSenderTx creates or widens a sender row. It writes ONLY the derived
// columns: the first/last-seen window widens monotonically, and status is
// promoted seen→watch when a candidate appears. suspected_entity,
// consent_status, consent_notes and notes are human judgments and are never
// written here, which is what makes a rescan safe to run at any time.
//
// A zero in the incoming window means "this write carries no message", not
// "the sender was first seen at the epoch", and is ignored on both columns. Two
// callers depend on that: a scan batch made entirely of system lines has no
// timestamps to contribute, and AddSpamEvent creates a row for a sender it has
// never scanned. Letting either through would move a window that is supposed to
// record only when the sender CONTACTED you — and because the first-seen guard
// takes any smaller value, a zero did not merely fail to widen it, it erased it.
func upsertSpamSenderTx(ctx context.Context, tx *sql.Tx, sender SpamSender, sawCandidate bool, now string) error {
	status := SpamStatusSeen
	if sawCandidate {
		status = SpamStatusWatch
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO spam_senders(source, identifier, conversation_name, status,
                         first_seen_unix, last_seen_unix, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(source, identifier) DO UPDATE SET
    conversation_name = excluded.conversation_name,
    first_seen_unix   = CASE WHEN excluded.first_seen_unix = 0 THEN spam_senders.first_seen_unix
                             WHEN spam_senders.first_seen_unix = 0 OR excluded.first_seen_unix < spam_senders.first_seen_unix
                             THEN excluded.first_seen_unix ELSE spam_senders.first_seen_unix END,
    last_seen_unix    = MAX(spam_senders.last_seen_unix, excluded.last_seen_unix),
    status            = CASE WHEN spam_senders.status = 'seen' AND excluded.status = 'watch'
                             THEN 'watch' ELSE spam_senders.status END,
    updated_at        = excluded.updated_at`,
		sender.Source, sender.Identifier, sender.ConversationName, status,
		sender.FirstSeenUnix, sender.LastSeenUnix, now, now)
	if err != nil {
		return fmt.Errorf("upsert spam sender %s: %w", sender.Identifier, err)
	}
	return nil
}

func insertSpamEventTx(ctx context.Context, tx *sql.Tx, e SpamEvent, now string) error {
	origin := e.Origin
	if origin == "" {
		origin = "manual"
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO spam_events(source, identifier, event_type, event_at, event_at_unix, details, origin, message_hash, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(source, identifier, event_type, event_at_unix, message_hash) DO NOTHING`,
		e.Source, e.Identifier, e.EventType, e.EventAt, e.EventAtUnix, e.Details, origin, e.MessageHash, now)
	if err != nil {
		return fmt.Errorf("insert spam event %s/%s: %w", e.Identifier, e.EventType, err)
	}
	return nil
}

// AddSpamEvent records an event by hand — a 7726 forward, an FCC complaint and
// its ticket number, a lawyer referral. Duplicate inserts are ignored, so
// re-running a backfill script is safe.
func (s *Store) AddSpamEvent(ctx context.Context, e SpamEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin spam event: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339)
	if err := insertSpamEventTx(ctx, tx, e, now); err != nil {
		return err
	}
	// An event for a sender nobody has scanned yet still needs a row to hang
	// off, or `spam senders` would not list the person you just filed against.
	//
	// The window is left at zero rather than stamped with the event time. An
	// FCC complaint filed today is not the sender contacting you today, and the
	// dossier prints first/last seen as the contact window — stamping it there
	// would report a complaint as a contact in the evidence record itself.
	if err := upsertSpamSenderTx(ctx, tx, SpamSender{
		Source: e.Source, Identifier: e.Identifier,
	}, false, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ListSpamEvents returns a sender's events oldest first, or every event when
// identifier is empty.
func (s *Store) ListSpamEvents(ctx context.Context, source, identifier string) ([]SpamEvent, error) {
	q := `SELECT id, source, identifier, event_type, event_at, event_at_unix, details, origin, message_hash
	        FROM spam_events`
	var args []any
	if identifier != "" {
		q += ` WHERE source = ? AND identifier = ?`
		args = append(args, source, identifier)
	}
	q += ` ORDER BY event_at_unix, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list spam events: %w", err)
	}
	defer rows.Close()
	var out []SpamEvent
	for rows.Next() {
		var e SpamEvent
		if err := rows.Scan(&e.ID, &e.Source, &e.Identifier, &e.EventType, &e.EventAt, &e.EventAtUnix,
			&e.Details, &e.Origin, &e.MessageHash); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetSpamSenderFields updates the human-judgment columns. Only non-nil fields
// are written, so `--notes` alone does not blank the suspected entity.
func (s *Store) SetSpamSenderFields(ctx context.Context, source, identifier string, status, entity, consent, consentNotes, notes *string) error {
	var sets []string
	var args []any
	add := func(col string, v *string) {
		if v != nil {
			sets = append(sets, col+" = ?")
			args = append(args, *v)
		}
	}
	add("status", status)
	add("suspected_entity", entity)
	add("consent_status", consent)
	add("consent_notes", consentNotes)
	add("notes", notes)
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().UTC().Format(time.RFC3339), source, identifier)

	res, err := s.db.ExecContext(ctx,
		`UPDATE spam_senders SET `+strings.Join(sets, ", ")+` WHERE source = ? AND identifier = ?`, args...)
	if err != nil {
		return fmt.Errorf("update spam sender %s: %w", identifier, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no spam sender %s on source %s — run `msgbrowse spam scan` first, or check the identifier", identifier, source)
	}
	return nil
}

// GetSpamSender returns one sender by canonical identifier.
func (s *Store) GetSpamSender(ctx context.Context, source, identifier string) (SpamSender, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, source, identifier, conversation_name, status, suspected_entity,
       consent_status, consent_notes, notes, first_seen_unix, last_seen_unix, created_at, updated_at
  FROM spam_senders WHERE source = ? AND identifier = ?`, source, identifier)
	return scanSpamSender(row)
}

// FindSpamSenders resolves a user-typed identifier that may omit the source or
// be spelled differently from the stored canonical form. It matches on the
// canonical identifier first, then on the conversation name, so both
// "+15551110001" and the archive's own thread name find the same row.
func (s *Store) FindSpamSenders(ctx context.Context, identifier string) ([]SpamSender, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, source, identifier, conversation_name, status, suspected_entity,
       consent_status, consent_notes, notes, first_seen_unix, last_seen_unix, created_at, updated_at
  FROM spam_senders
 WHERE identifier = ? OR conversation_name = ?
 ORDER BY source, identifier`, identifier, identifier)
	if err != nil {
		return nil, fmt.Errorf("find spam sender: %w", err)
	}
	defer rows.Close()
	var out []SpamSender
	for rows.Next() {
		sender, err := scanSpamSender(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sender)
	}
	return out, rows.Err()
}

// ListSpamSenders returns senders in the given statuses (all when empty),
// oldest-first-seen first.
func (s *Store) ListSpamSenders(ctx context.Context, statuses []string) ([]SpamSender, error) {
	q := `SELECT id, source, identifier, conversation_name, status, suspected_entity,
	             consent_status, consent_notes, notes, first_seen_unix, last_seen_unix, created_at, updated_at
	        FROM spam_senders`
	var args []any
	if len(statuses) > 0 {
		q += ` WHERE status IN (` + placeholders(len(statuses)) + `)`
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	q += ` ORDER BY first_seen_unix, identifier`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list spam senders: %w", err)
	}
	defer rows.Close()
	var out []SpamSender
	for rows.Next() {
		sender, err := scanSpamSender(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sender)
	}
	return out, rows.Err()
}

// SpamMessages returns a sender's findings joined back to the message rows they
// describe, oldest first. A finding whose message hash no longer resolves is
// returned with Present=false rather than dropped: a message that vanished
// between exports is a gap the dossier must show, not hide.
func (s *Store) SpamMessages(ctx context.Context, source, identifier, version string) ([]SpamDossierMessage, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT f.message_hash, f.source, f.identifier, f.direction, f.ts_unix,
       f.reasons, f.urls, f.phones, f.emails, f.names_matched, f.entities,
       f.is_candidate, f.is_after_optout,
       COALESCE(m.body, ''), COALESCE(m.sender, ''), COALESCE(m.ts, ''),
       COALESCE(m.is_system, 0), (m.id IS NOT NULL)
  FROM spam_findings f
  LEFT JOIN messages m ON m.hash = f.message_hash
 WHERE f.source = ? AND f.identifier = ? AND f.ruleset_version = ?
 ORDER BY f.ts_unix, f.message_hash`, source, identifier, version)
	if err != nil {
		return nil, fmt.Errorf("spam messages: %w", err)
	}
	defer rows.Close()

	var out []SpamDossierMessage
	for rows.Next() {
		var m SpamDossierMessage
		var reasons, urls, phones, emails, names, entities string
		var candidate, after, system, present int
		if err := rows.Scan(&m.MessageHash, &m.Source, &m.Identifier, &m.Direction, &m.TSUnix,
			&reasons, &urls, &phones, &emails, &names, &entities,
			&candidate, &after, &m.Body, &m.Sender, &m.TS, &system, &present); err != nil {
			return nil, err
		}
		m.Reasons = parseList(reasons)
		m.URLs = parseList(urls)
		m.Phones = parseList(phones)
		m.Emails = parseList(emails)
		m.Names = parseList(names)
		m.Entities = parseList(entities)
		m.IsCandidate = candidate != 0
		m.IsAfterOptOut = after != 0
		m.IsSystem = system != 0
		m.Present = present != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// SpamCounts returns the per-sender tallies for one ruleset generation.
// SpamProvenance returns the distinct scan environments that produced a
// sender's findings in one ruleset generation, sorted for stable output.
//
// A record assembled from rows written under more than one stranger predicate
// is a mixed-provenance record, and the dossier has to say so: half its
// messages may have been selected because the sender was absent from an address
// book, and the other half because the thread name merely looked like a bare
// handle. An empty string in the result means rows that predate schemaV20,
// whose environment was never recorded — reported as unknown, never guessed.
func (s *Store) SpamProvenance(ctx context.Context, source, identifier, version string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT scan_env
  FROM spam_findings
 WHERE source = ? AND identifier = ? AND ruleset_version = ?
 ORDER BY scan_env`, source, identifier, version)
	if err != nil {
		return nil, fmt.Errorf("spam provenance: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var env string
		if err := rows.Scan(&env); err != nil {
			return nil, fmt.Errorf("scan spam provenance: %w", err)
		}
		out = append(out, env)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("spam provenance rows: %w", err)
	}
	return out, nil
}

func (s *Store) SpamCounts(ctx context.Context, version string) (map[string]SpamSenderCounts, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT source, identifier,
       SUM(CASE WHEN direction = 'inbound'  THEN 1 ELSE 0 END),
       SUM(CASE WHEN direction = 'outbound' THEN 1 ELSE 0 END),
       SUM(is_candidate),
       SUM(is_after_optout)
  FROM spam_findings
 WHERE ruleset_version = ?
 GROUP BY source, identifier`, version)
	if err != nil {
		return nil, fmt.Errorf("spam counts: %w", err)
	}
	defer rows.Close()
	out := map[string]SpamSenderCounts{}
	for rows.Next() {
		var src, ident string
		var c SpamSenderCounts
		if err := rows.Scan(&src, &ident, &c.Inbound, &c.Outbound, &c.Candidates, &c.AfterOptOut); err != nil {
			return nil, err
		}
		out[src+"\x00"+ident] = c
	}
	return out, rows.Err()
}

// SpamCountsKey builds the map key SpamCounts returns.
func SpamCountsKey(source, identifier string) string { return source + "\x00" + identifier }

// RecomputeSpamAfterOptOut rewrites is_after_optout across the WHOLE
// generation, never incrementally.
//
// This is deliberate and it is the reason the column is trustworthy. An
// opt-out you record today changes the flag on messages scanned months ago, so
// an incremental update would leave the column quietly, invisibly wrong on
// exactly the rows a filing depends on. Only inbound messages are ever flagged:
// your own later messages are not violations.
//
// The threshold is the sender's EARLIEST opt-out. A later STOP does not reset
// the clock — the first one is when consent was withdrawn.
func (s *Store) RecomputeSpamAfterOptOut(ctx context.Context, version string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE spam_findings
   SET is_after_optout = CASE
       WHEN direction = 'inbound' AND ts_unix > COALESCE((
            SELECT MIN(e.event_at_unix) FROM spam_events e
             WHERE e.source = spam_findings.source
               AND e.identifier = spam_findings.identifier
               AND e.event_type IN ('stop_sent', 'notice_sent')
       ), 9223372036854775807)
       THEN 1 ELSE 0 END
 WHERE ruleset_version = ?`, version)
	if err != nil {
		return fmt.Errorf("recompute spam after-optout: %w", err)
	}
	return nil
}

// SpamOptOutAt returns the unix time of a sender's earliest opt-out, if any.
func (s *Store) SpamOptOutAt(ctx context.Context, source, identifier string) (int64, bool, error) {
	var at sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
SELECT MIN(event_at_unix) FROM spam_events
 WHERE source = ? AND identifier = ? AND event_type IN ('stop_sent', 'notice_sent')`,
		source, identifier).Scan(&at)
	if err != nil {
		return 0, false, fmt.Errorf("spam opt-out at: %w", err)
	}
	if !at.Valid {
		return 0, false, nil
	}
	return at.Int64, true, nil
}

// ResetSpam clears the DERIVED halves of the evidence layer — findings,
// cursors, and the opt-out events a scan detected — and leaves everything a
// person entered by hand intact: sender statuses, suspected entities, consent
// records, notes, and every manually filed event with its confirmation number.
//
// Those cannot be re-derived from the archive, so a reset must never take them.
func (s *Store) ResetSpam(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin spam reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`DELETE FROM spam_findings`,
		`DELETE FROM spam_state`,
		`DELETE FROM spam_events WHERE origin = 'scan'`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("spam reset: %w", err)
		}
	}
	return tx.Commit()
}

func scanSpamSender(sc interface{ Scan(...any) error }) (SpamSender, error) {
	var s SpamSender
	err := sc.Scan(&s.ID, &s.Source, &s.Identifier, &s.ConversationName, &s.Status, &s.SuspectedEntity,
		&s.ConsentStatus, &s.ConsentNotes, &s.Notes, &s.FirstSeenUnix, &s.LastSeenUnix, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return SpamSender{}, err
	}
	if err != nil {
		return SpamSender{}, fmt.Errorf("scan spam sender: %w", err)
	}
	return s, nil
}

func findingsHaveCandidate(fs []SpamFinding) bool {
	for _, f := range fs {
		if f.IsCandidate {
			return true
		}
	}
	return false
}

// jsonList stores a string slice as a JSON array. Empty and nil both become
// "[]" so the column is never NULL and readers never branch.
func jsonList(in []string) string {
	if len(in) == 0 {
		return "[]"
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func parseList(raw string) []string {
	if raw == "" || raw == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
