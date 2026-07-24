package store

// Journal persistence (ADR-0023): the two day-keyed tables added in schemaV11.
//
//   - journal_days   — the deterministic MECHANICAL rollup (counts, per-source
//     counts, top senders) for one calendar day. A cache/index over messages,
//     always rebuildable, so a stale row is harmless.
//   - journal_digests — one cached LLM digest per day, versioned by
//     (model, prompt_version) so a model swap or a DigestPrompt edit invalidates
//     it. This layer is what internal/journal writes after a Chat call.
//
// Day keys are 'YYYY-MM-DD' bucketed in UTC via date(ts_unix,'unixepoch').
// ts_unix is the wall-clock ts string parsed AS UTC, so no timezone conversion
// is applied anywhere here — a 'localtime' shift would misfile messages across
// day boundaries (ADR-0023's load-bearing constraint). Bucketing on ts_unix
// rather than substr(ts,1,10) is deliberate: a not-yet-re-ingested database can
// still hold a legacy iMessage ts string ("Nov 13, 2015 …", see render.go),
// whose substring is not a valid day — ts_unix is always canonical seconds.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
)

// topSendersPerDay caps how many senders a day's mechanical rollup records.
const topSendersPerDay = 5

// SenderCount is one participant's message count within a day's mechanical
// rollup, owner-excluded and ordered most-active first.
type SenderCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// JournalDay is the mechanical rollup for a single calendar day. SourceCounts
// maps a source id ("signal"/"imessage"/"whatsapp") to its message count that
// day; TopSenders is the day's most-active non-owner participants.
type JournalDay struct {
	Day               string
	MessageCount      int
	ConversationCount int
	SourceCounts      map[string]int
	TopSenders        []SenderCount
	UpdatedAt         string
}

// JournalDigest is one day's cached LLM digest plus the (model, prompt_version)
// that produced it — the cache key that makes a model or prompt change
// invalidate the row.
type JournalDigest struct {
	Day           string
	Model         string
	PromptVersion string
	Body          string // plain-text summary (fallback + empty-response guard)
	Structured    string // canonical JSON of the editorial digest ("" = prose-only)
	Mood          string // denormalized mood label ("" = none), for the calendar tint
	UpdatedAt     string
}

// JournalDayView is a mechanical day joined with its digest (if any), as the
// /journal page lists them. DigestBody is "" when the day has no cached digest,
// in which case the page renders the mechanical summary instead.
type JournalDayView struct {
	JournalDay
	DigestBody       string
	DigestModel      string
	DigestStructured string // canonical JSON ("" = prose-only / no digest); parsed on read in the web layer
	Mood             string // "" = no digest
}

// DayTranscriptLine is one message in a day's cross-conversation transcript,
// carrying the conversation name and source that the plain MessageView lacks so
// the digest prompt can disambiguate threads.
type DayTranscriptLine struct {
	MessageView
	ConversationName string
	Source           string
}

// excludedConversationIDs resolves the conversation-name denylist
// (journal.exclude_conversations) to the current conversation ids, across every
// source. The journal excludes by NAME (like facts' FactConversations), so a
// denylisted thread's content never reaches an aggregate — let alone the LLM.
// Returns nil for an empty denylist.
func (s *Store) excludedConversationIDs(ctx context.Context, exclude []string) ([]int64, error) {
	if len(exclude) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(exclude)), ",")
	args := make([]any, len(exclude))
	for i, name := range exclude {
		args[i] = name
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM conversations WHERE name IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("excluded conversation ids: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// notInClause renders an " AND col NOT IN (?,?,…)" fragment and appends the ids
// to args. It returns the empty string (args untouched) when ids is empty.
func notInClause(col string, ids []int64, args *[]any) string {
	if len(ids) == 0 {
		return ""
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	for _, id := range ids {
		*args = append(*args, id)
	}
	return " AND " + col + " NOT IN (" + placeholders + ")"
}

// BuildJournalDays computes the mechanical rollup for every day with real
// (non-system, non-empty) content on or after sinceDay (” = all history),
// newest day first. It is the deterministic, egress-free layer of a journal run
// and is built from two GROUP BY passes over messages — the whole mechanical
// journal in two round trips, not one query per day. Excluded conversations
// contribute to nothing.
func (s *Store) BuildJournalDays(ctx context.Context, sinceDay string, exclude []string) ([]JournalDay, error) {
	excl, err := s.excludedConversationIDs(ctx, exclude)
	if err != nil {
		return nil, err
	}

	// Pass 1: per (day, source) message and distinct-conversation counts. A
	// conversation belongs to exactly one source, so summing distinct-conversation
	// counts across sources yields the day's true distinct-conversation count.
	days := make(map[string]*JournalDay)
	order := make([]string, 0)
	{
		args := []any{}
		q := `SELECT date(ts_unix,'unixepoch') AS day, source, COUNT(*) AS msgs, COUNT(DISTINCT conversation_id) AS convs
		        FROM messages
		       WHERE is_system = 0 AND TRIM(body) <> ''`
		if sinceDay != "" {
			q += ` AND date(ts_unix,'unixepoch') >= ?`
			args = append(args, sinceDay)
		}
		q += notInClause("conversation_id", excl, &args)
		q += ` GROUP BY day, source`
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("journal day counts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var day, src string
			var msgs, convs int
			if err := rows.Scan(&day, &src, &msgs, &convs); err != nil {
				return nil, err
			}
			d := days[day]
			if d == nil {
				d = &JournalDay{Day: day, SourceCounts: map[string]int{}}
				days[day] = d
				order = append(order, day)
			}
			d.MessageCount += msgs
			d.ConversationCount += convs
			d.SourceCounts[src] += msgs
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("journal day counts rows: %w", err)
		}
	}

	// Pass 2: per (day, sender) counts, owner excluded, folded into each day's
	// top-senders list. A day with only owner messages simply has no top senders.
	{
		args := []any{signal.OwnerSender}
		q := `SELECT date(ts_unix,'unixepoch') AS day, sender, COUNT(*) AS c
		        FROM messages
		       WHERE is_system = 0 AND TRIM(body) <> '' AND sender <> ?`
		if sinceDay != "" {
			q += ` AND date(ts_unix,'unixepoch') >= ?`
			args = append(args, sinceDay)
		}
		q += notInClause("conversation_id", excl, &args)
		q += ` GROUP BY day, sender`
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("journal day senders: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var day, sender string
			var c int
			if err := rows.Scan(&day, &sender, &c); err != nil {
				return nil, err
			}
			if d := days[day]; d != nil {
				d.TopSenders = append(d.TopSenders, SenderCount{Name: sender, Count: c})
			}
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("journal day senders rows: %w", err)
		}
	}

	// Newest day first (matches the /journal listing and the digest run order),
	// and each day's senders most-active first, truncated to the top N.
	sort.Sort(sort.Reverse(sort.StringSlice(order)))
	out := make([]JournalDay, 0, len(order))
	for _, day := range order {
		d := days[day]
		sort.SliceStable(d.TopSenders, func(i, j int) bool { return d.TopSenders[i].Count > d.TopSenders[j].Count })
		if len(d.TopSenders) > topSendersPerDay {
			d.TopSenders = d.TopSenders[:topSendersPerDay]
		}
		out = append(out, *d)
	}
	return out, nil
}

// PutJournalDay upserts one day's mechanical rollup. It DO UPDATEs so a rebuild
// refreshes counts in place (the rollup is deterministic, so re-running is
// idempotent).
func (s *Store) PutJournalDay(ctx context.Context, d JournalDay) error {
	srcJSON, err := json.Marshal(d.SourceCounts)
	if err != nil {
		return fmt.Errorf("marshal source counts: %w", err)
	}
	sendersJSON, err := json.Marshal(d.TopSenders)
	if err != nil {
		return fmt.Errorf("marshal top senders: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.ExecContext(ctx, `
INSERT INTO journal_days(day, message_count, conversation_count, source_counts, top_senders, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(day) DO UPDATE SET
    message_count      = excluded.message_count,
    conversation_count = excluded.conversation_count,
    source_counts      = excluded.source_counts,
    top_senders        = excluded.top_senders,
    updated_at         = excluded.updated_at`,
		d.Day, d.MessageCount, d.ConversationCount, string(srcJSON), string(sendersJSON), now)
	if err != nil {
		return fmt.Errorf("put journal day: %w", err)
	}
	return nil
}

// DayTranscript returns one day's real messages across every conversation, in
// per-thread chronological order, enriched with attachments and links — the
// content the digest prompt summarizes. The day is bounded by its UTC ts_unix
// window (indexed by idx_messages_ts_unix). Excluded conversations never appear,
// so their content is never assembled, let alone sent to the LLM.
func (s *Store) DayTranscript(ctx context.Context, day string, exclude []string) ([]DayTranscriptLine, error) {
	startUnix, endUnix, err := dayUnixWindow(day)
	if err != nil {
		return nil, err
	}
	excl, err := s.excludedConversationIDs(ctx, exclude)
	if err != nil {
		return nil, err
	}
	args := []any{startUnix, endUnix}
	q := `SELECT m.id, m.hash, m.sender, m.is_system, m.ts, m.ts_unix, m.body, c.name, c.source
	        FROM messages m
	        JOIN conversations c ON c.id = m.conversation_id
	       WHERE m.is_system = 0 AND TRIM(m.body) <> ''
	         AND m.ts_unix >= ? AND m.ts_unix < ?`
	q += notInClause("m.conversation_id", excl, &args)
	q += ` ORDER BY m.conversation_id, m.ts_unix, m.id`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("day transcript: %w", err)
	}
	defer rows.Close()

	var (
		msgs  []MessageView
		names []string
		srcs  []string
	)
	for rows.Next() {
		var m MessageView
		var isSystem int
		var name, src string
		if err := rows.Scan(&m.ID, &m.Hash, &m.Sender, &isSystem, &m.TS, &m.TSUnix, &m.Body, &name, &src); err != nil {
			return nil, err
		}
		m.IsSystem = isSystem == 1
		m.IsOwner = m.Sender == signal.OwnerSender
		msgs = append(msgs, m)
		names = append(names, name)
		srcs = append(srcs, src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("day transcript rows: %w", err)
	}
	// Enrich attachments/links/reactions in place, then zip the per-line
	// conversation identity back on top.
	if err := s.attachChildren(ctx, msgs); err != nil {
		return nil, err
	}
	out := make([]DayTranscriptLine, len(msgs))
	for i := range msgs {
		out[i] = DayTranscriptLine{MessageView: msgs[i], ConversationName: names[i], Source: srcs[i]}
	}
	return out, nil
}

// GetDayDigest returns the cached digest for a day and the (model,
// prompt_version) that produced it. ok is false when the day has never been
// digested; the caller treats a stale (model, prompt_version) the same as
// missing and re-runs.
func (s *Store) GetDayDigest(ctx context.Context, day string) (body, model, promptVersion string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT body, model, prompt_version FROM journal_digests WHERE day = ?`, day).
		Scan(&body, &model, &promptVersion)
	if err == sql.ErrNoRows {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, fmt.Errorf("get day digest: %w", err)
	}
	return body, model, promptVersion, true, nil
}

// PutDayDigest upserts a day's digest. It DO UPDATEs (not DO NOTHING) so a
// changed model or prompt_version overwrites the prior digest — the run persists
// each day immediately after its Chat call, so a mid-run cancel resumes cleanly
// at the next uncached day.
func (s *Store) PutDayDigest(ctx context.Context, d JournalDigest) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO journal_digests(day, model, prompt_version, body, structured, mood, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(day) DO UPDATE SET
    model          = excluded.model,
    prompt_version = excluded.prompt_version,
    body           = excluded.body,
    structured     = excluded.structured,
    mood           = excluded.mood,
    updated_at     = excluded.updated_at`,
		d.Day, d.Model, d.PromptVersion, d.Body, d.Structured, d.Mood, now)
	if err != nil {
		return fmt.Errorf("put day digest: %w", err)
	}
	return nil
}

// ResetDigests clears every cached digest so the next run re-derives them (the
// --regenerate path). The mechanical journal_days rows are left in place — they
// are rebuilt (upserted) on every run regardless.
func (s *Store) ResetDigests(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM journal_digests`); err != nil {
		return fmt.Errorf("reset digests: %w", err)
	}
	return nil
}

// ListJournalDays returns one page of mechanical days joined with their digests,
// newest first, using a keyset cursor on the day key: beforeDay "" starts at the
// newest day, otherwise the page begins strictly before that day. The caller
// fetches limit+1 to detect a further page.
func (s *Store) ListJournalDays(ctx context.Context, beforeDay string, limit int) ([]JournalDayView, error) {
	if limit <= 0 || limit > 365 {
		limit = 30
	}
	args := []any{}
	q := `SELECT jd.day, jd.message_count, jd.conversation_count, jd.source_counts, jd.top_senders, jd.updated_at,
	             COALESCE(dg.body, ''), COALESCE(dg.model, ''), COALESCE(dg.structured, ''), COALESCE(dg.mood, '')
	        FROM journal_days jd
	        LEFT JOIN journal_digests dg ON dg.day = jd.day`
	if beforeDay != "" {
		q += ` WHERE jd.day < ?`
		args = append(args, beforeDay)
	}
	q += ` ORDER BY jd.day DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list journal days: %w", err)
	}
	defer rows.Close()
	var out []JournalDayView
	for rows.Next() {
		var v JournalDayView
		var srcJSON, sendersJSON string
		if err := rows.Scan(&v.Day, &v.MessageCount, &v.ConversationCount, &srcJSON, &sendersJSON, &v.UpdatedAt,
			&v.DigestBody, &v.DigestModel, &v.DigestStructured, &v.Mood); err != nil {
			return nil, err
		}
		if err := unmarshalDayJSON(srcJSON, sendersJSON, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// dayUnixWindow converts a 'YYYY-MM-DD' day key to its half-open ts_unix window
// [start, end) in UTC. time.Parse with a zone-less layout interprets the string
// as UTC, matching how ts_unix was computed from the wall-clock ts string and
// how BuildJournalDays derived the key via date(ts_unix,'unixepoch') (ADR-0023).
func dayUnixWindow(day string) (start, end int64, err error) {
	t, perr := time.Parse(signal.TimestampLayout, day+" 00:00:00")
	if perr != nil {
		return 0, 0, fmt.Errorf("parse day %q: %w", day, perr)
	}
	return t.Unix(), t.AddDate(0, 0, 1).Unix(), nil
}
