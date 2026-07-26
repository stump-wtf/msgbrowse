package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
)

// This file backs Home's two resurfacing cards (#239, SPEC-0006 REQ-0006-007):
// "On this day", which pulls one message from the same calendar month/day in a
// previous year, and "Jump back in", which lists the most recently active
// conversations.
//
// Both are on the boosted /#main-content path, so both are LIMITed and indexed.
// ListConversations is deliberately NOT reused for the second one: it carries
// aggregate counts over every conversation, has no LIMIT, and measures
// ~346-388ms on the reference archive — which is exactly why handleIndex skips
// it on partial renders (SPEC-0008 REQ-0008-006).

// OnThisDayMessage is one candidate for the "On this day" card: a single message
// from a prior year's same calendar day, with the conversation it belongs to.
type OnThisDayMessage struct {
	ID               int64
	ConversationID   int64
	ConversationName string
	Source           string
	Sender           string
	IsOwner          bool
	TS               string // "YYYY-MM-DD HH:MM:SS"
	TSUnix           int64
	Day              string // "YYYY-MM-DD" — the arm this row came from
	// Body is already display-ready: markdown/quote markers stripped, whitespace
	// collapsed, and capped on a rune boundary at onThisDayBodyChars. The card
	// renders it on the landing page, so it is bounded here rather than trusting
	// the template to clamp an arbitrarily long message.
	Body string
}

// onThisDayBodyChars caps the resurfaced excerpt. preview() strips markers and
// collapses whitespace before taking its runes, so the SQL prefix it is fed has
// to be comfortably larger than this (the same starvation reasoning as
// ListConversations' 1024-char prefix).
const onThisDayBodyChars = 240

// MessageYearRange returns the first and last calendar year present in the
// archive, or (0, 0) when it is empty. Both ends are MIN/MAX over the indexed
// ts_unix column, so this is a two-endpoint index probe rather than a scan.
//
// The years come back in the store's wall-clock-parsed-as-UTC space (see
// signal.TimestampLayout): ts_unix is an ordering key derived from a wall-clock
// string, not a true instant, so it is converted with .UTC() and never .Local().
func (s *Store) MessageYearRange(ctx context.Context) (first, last int, err error) {
	// Two scalar subqueries, NOT `SELECT MIN(x), MAX(x) FROM messages`. SQLite
	// only applies its min/max index optimization to an aggregate query with a
	// SINGLE MIN or MAX and nothing else; asking for both in one SELECT falls
	// back to a full scan of every message — which this runs on every Home
	// render, boosted included. As separate subqueries each one is the
	// two-endpoint index probe on idx_messages_ts_unix this is meant to be.
	const q = `SELECT COALESCE((SELECT MIN(ts_unix) FROM messages), 0),
                      COALESCE((SELECT MAX(ts_unix) FROM messages), 0)`
	var lo, hi int64
	if err := s.db.QueryRowContext(ctx, q).Scan(&lo, &hi); err != nil {
		return 0, 0, fmt.Errorf("message year range: %w", err)
	}
	if lo == 0 && hi == 0 {
		return 0, 0, nil
	}
	return time.Unix(lo, 0).UTC().Year(), time.Unix(hi, 0).UTC().Year(), nil
}

// OnThisDayCandidates returns up to perDay candidate messages for each of the
// given days ("YYYY-MM-DD"), with the arms kept in the caller's day order — so a
// caller passing days newest-first gets the newest qualifying year first.
//
// Selection within a day is deterministic: longest body, then lowest id. That
// matters as much as the query plan — Home must not reshuffle on every refresh
// (#239), so there is no RANDOM() and no clock input anywhere in this path.
//
// Each day is its own sargable arm bounded by dayUnixWindow, i.e. a one-day
// range scan on idx_messages_ts_unix. It is NOT expressed as
// strftime('%m-%d', ts_unix, 'unixepoch') = ?, which wraps the indexed column
// and degrades to a full scan of messages — the same trap called out in
// journal_calendar.go. System messages and blank bodies are excluded so the card
// never resurfaces "— X shared 2 photos —", and exclude drops conversations the
// journal denylist already hides.
//
// limit caps the TOTAL rows across all arms, so the compound stops as soon as
// the caller has enough. Without it the query eagerly evaluates all ~20 arms to
// build rows the caller then throws away; the caller only wants the first
// non-empty candidate. limit <= 0 means no cap.
func (s *Store) OnThisDayCandidates(ctx context.Context, days []string, perDay, limit int, exclude []string) ([]OnThisDayMessage, error) {
	if len(days) == 0 || perDay <= 0 {
		return nil, nil
	}

	// Build one UNION ALL arm per day. The arms are parameterised, never
	// formatted — only the arm COUNT and the placeholder count vary with input.
	var (
		arms []string
		args []any
	)
	// The exclude list is bound ONCE in a CTE, not re-emitted per arm. Inlining
	// it into every arm multiplied the bound-parameter count by the arm count —
	// a 50-name denylist across 20 arms is 1000+ parameters, which trips
	// SQLITE_MAX_VARIABLE_NUMBER and 500s the whole Home page rather than
	// degrading the card. As a CTE the cost is len(exclude), flat.
	prefix, excludeClause := "", ""
	if len(exclude) > 0 {
		rowsSQL := make([]string, len(exclude))
		for i := range exclude {
			rowsSQL[i] = "(?)"
		}
		prefix = "WITH ex(name) AS (VALUES " + strings.Join(rowsSQL, ",") + ")\n"
		excludeClause = " AND c.name NOT IN (SELECT name FROM ex)"
		for _, e := range exclude {
			args = append(args, e)
		}
	}
	for _, day := range days {
		start, end, err := dayUnixWindow(day)
		if err != nil {
			// A malformed day is a programming error upstream, not user input
			// (the web layer builds these), but skip rather than poison the union.
			continue
		}
		// Each arm is wrapped in a subquery: SQLite rejects a bare ORDER BY /
		// LIMIT inside a compound SELECT ("ORDER BY clause should come after
		// UNION ALL not before"), and the per-arm ordering is precisely what
		// this query is for, so it has to be pushed down a level.
		arms = append(arms, `
SELECT * FROM (
SELECT m.id, m.conversation_id, c.name, c.source, m.sender, m.ts, m.ts_unix, ? AS day,
       substr(m.body, 1, 1024) AS body
  FROM messages m
  JOIN conversations c ON c.id = m.conversation_id
 WHERE m.ts_unix >= ? AND m.ts_unix < ?
   AND m.is_system = 0
   AND TRIM(m.body) <> ''`+excludeClause+`
 ORDER BY length(m.body) DESC, m.id ASC
 LIMIT ?)`)
		args = append(args, day, start, end, perDay)
	}
	if len(arms) == 0 {
		return nil, nil
	}
	// No global ORDER BY: the per-arm ordering IS the selection rule, and the
	// caller relies on arms arriving in the order they were requested. A global
	// LIMIT is safe alongside that — it truncates the concatenation, it does not
	// reorder it.
	q := prefix + strings.Join(arms, "\nUNION ALL\n")
	if limit > 0 {
		q += "\nLIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("on this day candidates: %w", err)
	}
	defer rows.Close()

	var out []OnThisDayMessage
	for rows.Next() {
		var (
			m    OnThisDayMessage
			body string
		)
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.ConversationName, &m.Source,
			&m.Sender, &m.TS, &m.TSUnix, &m.Day, &body); err != nil {
			return nil, err
		}
		m.IsOwner = m.Sender == signal.OwnerSender
		m.Body = preview(body, onThisDayBodyChars)
		// Drop candidates that preview to nothing. The ORDER BY ranks on the RAW
		// body length while the card renders the PREVIEW, and preview() strips
		// quote markers, unwraps markdown links and collapses whitespace — so a
		// long reply that is nothing but ">" quote lines, or bare newlines, or a
		// single anchorless link, both wins the sort and renders empty. The SQL
		// filter cannot catch these: SQLite's one-argument TRIM strips spaces
		// only, so a newline- or tab-only body sails past TRIM(body) <> ''.
		// Without this the card renders a bare “” on the landing page and
		// suppresses the day's real message.
		if m.Body == "" {
			continue
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RecentConversation is one row of Home's "Jump back in" card: who, and how long
// ago. Deliberately NOT a last-message preview — the design brief's card is a
// name and a relative stamp, so fetching a body prefix and running preview() on
// it would be work whose output nothing renders.
type RecentConversation struct {
	ID         int64
	Name       string
	Source     string
	LastTS     string
	LastTSUnix int64
}

// RecentConversations returns the n most recently active conversations, newest
// first. Unlike ListConversations it carries no per-conversation aggregates and
// is LIMITed, so it is cheap enough for every Home render including the boosted
// one. n <= 0 returns nothing without touching the database.
func (s *Store) RecentConversations(ctx context.Context, n int) ([]RecentConversation, error) {
	if n <= 0 {
		return nil, nil
	}
	// The correlated subquery is served by idx_messages_conv_ts. No message body
	// is fetched: the card shows a name and a relative stamp, so a body prefix
	// would be bytes across the driver for output nothing renders.
	const q = `
SELECT c.id, c.name, c.source, lm.ts, lm.ts_unix
  FROM conversations c
  JOIN messages lm ON lm.id = (SELECT m2.id FROM messages m2
                                WHERE m2.conversation_id = c.id
                                ORDER BY m2.ts_unix DESC, m2.id DESC LIMIT 1)
 ORDER BY lm.ts_unix DESC, c.name ASC
 LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, n)
	if err != nil {
		return nil, fmt.Errorf("recent conversations: %w", err)
	}
	defer rows.Close()

	var out []RecentConversation
	for rows.Next() {
		var rc RecentConversation
		if err := rows.Scan(&rc.ID, &rc.Name, &rc.Source, &rc.LastTS, &rc.LastTSUnix); err != nil {
			return nil, err
		}
		out = append(out, rc)
	}
	return out, rows.Err()
}
