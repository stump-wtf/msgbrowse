package store

// Calendar read-side for the journal redesign (Phase 2): the mood-tinted month
// grid, the year heatmap, the streak/peak stat tiles, and the single-day
// editorial view. All day bucketing is UTC via date(ts_unix,'unixepoch'),
// consistent with journal_days (ADR-0023). The month/heatmap reads scan only the
// tiny journal_days table (<=366 rows/year) using its cached message_count —
// never re-scanning the messages table; only JournalStats' weekday/peak-hour
// touch messages (one GROUP BY argmax each).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// unmarshalDayJSON hydrates a JournalDayView's source_counts + top_senders JSON
// blobs. Shared by ListJournalDays and GetJournalDay.
func unmarshalDayJSON(srcJSON, sendersJSON string, v *JournalDayView) error {
	if err := json.Unmarshal([]byte(srcJSON), &v.SourceCounts); err != nil {
		return fmt.Errorf("unmarshal source counts for %s: %w", v.Day, err)
	}
	if err := json.Unmarshal([]byte(sendersJSON), &v.TopSenders); err != nil {
		return fmt.Errorf("unmarshal top senders for %s: %w", v.Day, err)
	}
	return nil
}

// JournalMonthDay is one day cell in the mood-tinted month grid.
type JournalMonthDay struct {
	Day          string // "YYYY-MM-DD"
	MessageCount int
	Mood         string // "" when no digest — the cell renders count-only/neutral
	HasDigest    bool
	// Stale is true when this day carries a digest that was written before more
	// messages landed on it (#240). A zero captured count means UNKNOWN — a
	// pre-v15 digest — and deliberately does NOT read as stale.
	Stale bool
}

// JournalStats are the journal's headline numbers for a year (year 0 = all-time).
type JournalStats struct {
	LongestStreakDays  int
	MostActiveWeekday  time.Weekday
	MostActiveWeekdayN int
	PeakHour           int
	PeakHourN          int
	DaysWithEntries    int
	HasActivity        bool
}

// GetJournalDay returns one day's mechanical rollup joined with its digest
// (structured included), for the editorial day card. ok is false when the day
// has no journal_days row.
func (s *Store) GetJournalDay(ctx context.Context, day string) (JournalDayView, bool, error) {
	var v JournalDayView
	var srcJSON, sendersJSON string
	err := s.db.QueryRowContext(ctx, `
SELECT jd.day, jd.message_count, jd.conversation_count, jd.source_counts, jd.top_senders, jd.updated_at,
       COALESCE(dg.body,''), COALESCE(dg.model,''), COALESCE(dg.structured,''), COALESCE(dg.mood,''),
       COALESCE(dg.message_count, 0)
  FROM journal_days jd
  LEFT JOIN journal_digests dg ON dg.day = jd.day
 WHERE jd.day = ?`, day).
		Scan(&v.Day, &v.MessageCount, &v.ConversationCount, &srcJSON, &sendersJSON, &v.UpdatedAt,
			&v.DigestBody, &v.DigestModel, &v.DigestStructured, &v.Mood, &v.DigestMessageCount)
	if err == sql.ErrNoRows {
		return JournalDayView{}, false, nil
	}
	if err != nil {
		return JournalDayView{}, false, fmt.Errorf("get journal day: %w", err)
	}
	if err := unmarshalDayJSON(srcJSON, sendersJSON, &v); err != nil {
		return JournalDayView{}, false, err
	}
	return v, true, nil
}

// LatestJournalDay returns the newest day with a mechanical rollup ("" when the
// journal has never been built) — the default the /journal page opens on.
func (s *Store) LatestJournalDay(ctx context.Context) (string, error) {
	var day string
	err := s.db.QueryRowContext(ctx, `SELECT day FROM journal_days ORDER BY day DESC LIMIT 1`).Scan(&day)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("latest journal day: %w", err)
	}
	return day, nil
}

// LatestJournalDayInYear returns the most recent day with a rollup in the given
// year; ok is false when that year has none. Backs the year-tab default so a
// year opens on its latest active month, not an empty January.
func (s *Store) LatestJournalDayInYear(ctx context.Context, year int) (string, bool, error) {
	var day string
	err := s.db.QueryRowContext(ctx,
		`SELECT day FROM journal_days WHERE day >= ? AND day < ? ORDER BY day DESC LIMIT 1`,
		fmt.Sprintf("%04d-01-01", year), fmt.Sprintf("%04d-01-01", year+1)).Scan(&day)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("latest journal day in year: %w", err)
	}
	return day, true, nil
}

// JournalYears returns the distinct years that have journal days, newest first —
// the calendar's year tabs.
func (s *Store) JournalYears(ctx context.Context) ([]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT substr(day,1,4) FROM journal_days ORDER BY 1 DESC`)
	if err != nil {
		return nil, fmt.Errorf("journal years: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var y int
		if err := rows.Scan(&y); err != nil {
			return nil, err
		}
		out = append(out, y)
	}
	return out, rows.Err()
}

// JournalMonth returns every day WITH content in the given month, joined with its
// mood — the mood-tinted month grid. Days without content are simply absent (the
// web layer lays them into a fixed grid). Cheap: <=31 rows off journal_days.
func (s *Store) JournalMonth(ctx context.Context, year int, month time.Month) ([]JournalMonthDay, error) {
	start := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	startStr := start.Format("2006-01-02")
	endStr := start.AddDate(0, 1, 0).Format("2006-01-02")
	rows, err := s.db.QueryContext(ctx, `
SELECT jd.day, jd.message_count, COALESCE(dg.mood,''), dg.day IS NOT NULL,
       COALESCE(dg.message_count, 0) > 0 AND dg.message_count <> jd.message_count
  FROM journal_days jd
  LEFT JOIN journal_digests dg ON dg.day = jd.day
 WHERE jd.day >= ? AND jd.day < ?
 ORDER BY jd.day`, startStr, endStr)
	if err != nil {
		return nil, fmt.Errorf("journal month: %w", err)
	}
	defer rows.Close()
	var out []JournalMonthDay
	for rows.Next() {
		var d JournalMonthDay
		if err := rows.Scan(&d.Day, &d.MessageCount, &d.Mood, &d.HasDigest, &d.Stale); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// JournalStats computes the headline numbers for a year (year 0 = all-time),
// honoring the exclude denylist exactly like BuildJournalDays. Longest streak +
// days-with-entries come from the journal_days key set (Go); most-active weekday
// and peak hour are argmax GROUP BYs over messages (UTC bucketing). A year is
// bounded by a sargable ts_unix range (uses idx_messages_ts_unix); the strftime
// bucketing still visits each filtered row via a temp b-tree, so year 0
// (all-time) is a full scan — but the web page always passes a concrete year.
func (s *Store) JournalStats(ctx context.Context, year int, exclude []string) (JournalStats, error) {
	var st JournalStats
	excl, err := s.excludedConversationIDs(ctx, exclude)
	if err != nil {
		return st, err
	}

	// Day set → streak + count (journal_days already applied the exclude at build
	// time via BuildJournalDays, so no exclude predicate needed here).
	dayArgs := []any{}
	dayQ := `SELECT day FROM journal_days`
	if year != 0 {
		dayQ += ` WHERE day >= ? AND day < ?`
		dayArgs = append(dayArgs, fmt.Sprintf("%04d-01-01", year), fmt.Sprintf("%04d-01-01", year+1))
	}
	dayQ += ` ORDER BY day`
	dayRows, err := s.db.QueryContext(ctx, dayQ, dayArgs...)
	if err != nil {
		return st, fmt.Errorf("journal stats days: %w", err)
	}
	var days []string
	for dayRows.Next() {
		var d string
		if err := dayRows.Scan(&d); err != nil {
			dayRows.Close()
			return st, err
		}
		days = append(days, d)
	}
	dayRows.Close()
	if err := dayRows.Err(); err != nil {
		return st, err
	}
	st.DaysWithEntries = len(days)
	st.LongestStreakDays = longestStreak(days)
	st.HasActivity = len(days) > 0

	// Most-active weekday (%w: 0=Sun..6=Sat → time.Weekday) and peak hour (%H),
	// both over messages with the standard journal filter + UTC bucketing.
	wd, wn, err := s.journalArgmax(ctx, "%w", year, excl)
	if err != nil {
		return st, err
	}
	st.MostActiveWeekday, st.MostActiveWeekdayN = time.Weekday(wd), wn
	hr, hn, err := s.journalArgmax(ctx, "%H", year, excl)
	if err != nil {
		return st, err
	}
	st.PeakHour, st.PeakHourN = hr, hn
	return st, nil
}

// journalArgmax returns the most-frequent UTC weekday (unit "%w") or hour
// (unit "%H") over the journal's real messages, and its count. Returns (0,0) on
// no rows.
func (s *Store) journalArgmax(ctx context.Context, unit string, year int, excl []int64) (int, int, error) {
	args := []any{}
	q := `SELECT CAST(strftime('` + unit + `', ts_unix, 'unixepoch') AS INTEGER) AS b, COUNT(*) n
	        FROM messages
	       WHERE is_system = 0 AND TRIM(body) <> ''`
	if year != 0 {
		// Sargable ts_unix range (uses idx_messages_ts_unix), NOT date(ts_unix)
		// which wraps the column and forces a full scan.
		start := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
		end := time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
		q += ` AND ts_unix >= ? AND ts_unix < ?`
		args = append(args, start, end)
	}
	q += notInClause("conversation_id", excl, &args)
	q += ` GROUP BY b ORDER BY n DESC, b LIMIT 1`
	var b, n int
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&b, &n)
	if err == sql.ErrNoRows {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("journal argmax %s: %w", unit, err)
	}
	return b, n, nil
}

// longestStreak returns the longest run of consecutive calendar days in a
// sorted-ascending 'YYYY-MM-DD' set. Adjacency is by date arithmetic (parse +
// AddDate), NOT string succession, so a month/year rollover (2026-01-31 →
// 2026-02-01) counts. O(n).
func longestStreak(days []string) int {
	best, run := 0, 0
	var prev time.Time
	havePrev := false
	for _, ds := range days {
		t, err := time.Parse("2006-01-02", ds)
		if err != nil {
			continue
		}
		if havePrev && t.Equal(prev.AddDate(0, 0, 1)) {
			run++
		} else {
			run = 1
		}
		if run > best {
			best = run
		}
		prev, havePrev = t, true
	}
	return best
}

// JournalCoverage is the journal's build footprint: how much of the mechanical
// day layer carries a digest, and how much of that has since gone out of date.
// It is the journal's answer to the semantic index's coverage figure.
type JournalCoverage struct {
	Days         int    // days with activity (rows in journal_days)
	Digested     int    // of those, how many carry a digest
	Stale        int    // of those, how many were written before more messages landed
	BuiltThrough string // MAX(journal_days.day); "" when the journal was never built
}

// JournalCoverage returns the build footprint in one pass over journal_days
// joined to its digests.
//
// This is a few thousand tiny rows even on a decade-long archive — unlike
// EmbeddingCoverage, which scans messages — so it is safe on every Journal
// render. It deliberately does NOT try to report "days with messages but no
// journal_days row": that needs a GROUP BY over messages, i.e. a full scan. The
// UI compares BuiltThrough against the newest message timestamp instead, which
// is already loaded.
func (s *Store) JournalCoverage(ctx context.Context) (JournalCoverage, error) {
	var c JournalCoverage
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(dg.day IS NOT NULL), 0),
       COALESCE(SUM(dg.day IS NOT NULL AND dg.message_count > 0
                    AND dg.message_count <> jd.message_count), 0),
       COALESCE(MAX(jd.day), '')
  FROM journal_days jd
  LEFT JOIN journal_digests dg ON dg.day = jd.day`).
		Scan(&c.Days, &c.Digested, &c.Stale, &c.BuiltThrough)
	if err != nil {
		return JournalCoverage{}, fmt.Errorf("journal coverage: %w", err)
	}
	return c, nil
}

// Sentiment as a SECOND mood source for the calendar
//
// The calendar's only mood source used to be journal_digests.mood, so a day the
// LLM digest pass had not reached yet had no mood at all and rendered untinted —
// indistinguishable from a day the pass HAD read and found unremarkable (#370).
// The read below adds the sentiment layer (SPEC-0027 REQ-0027-009, ADR-0028) as
// a weaker second source: a day with affect-tier scores can be tinted even
// before it is digested.
//
// This is NOT free, local, or egress-free. ADR-0028 explicitly rejected a
// classical local lexicon (VADER/NRC-style); the IPIP lexicon is an anchor set
// fed to a chat model, and internal/sentiment.Run makes a billable Chat call per
// batch. So the sentiment layer is only ever a REFINEMENT on the digest layer's
// gap, never a promise that every day can be tinted for nothing.
//
// Deliberately shaped as a per-(day, construct) aggregate rather than a finished
// mood: which constructs read as pleasant or unpleasant is taxonomy that belongs
// with internal/sentiment's lexicon, and internal/store cannot import it without
// closing a cycle. The web layer owns that mapping and folds these rows into an
// enum (see internal/web/journalmood.go). At most 31 days x ~15 constructs, so
// folding in Go costs nothing.

// SentimentDayConstruct is one (UTC day, construct) aggregate of the sentiment
// scores in a month: the summed signed score and how many message-level rows it
// came from. Sum/N is the construct's mean for that day.
//
// Governing: SPEC-0027 REQ-0027-009 (per-day mood, UTC-bucketed exactly as
// ADR-0023 mandates), SPEC-0016 REQ-0016-015 (calendar reads are UTC-bucketed
// and honor the journal denylist).
type SentimentDayConstruct struct {
	Day       string // "YYYY-MM-DD", UTC, same frame as journal_days
	Construct string
	Sum       float64
	N         int
}

// LatestSentimentGeneration returns the (model, lexicon_version) the scoring
// engine most recently ran under, read off the per-conversation cursor table.
// ok is false when nothing has ever been scored.
//
// Every read of message_sentiment MUST pin a generation: scores from different
// models or lexicon curations are not comparable and must never be averaged
// together (SPEC-0027 REQ "sparse, generation-stamped storage"). sentiment_state
// is the right source for "current" because it records when the engine last
// advanced a cursor — message_sentiment.ts_unix is the MESSAGE's timestamp, not
// a write time, so ordering by it would name the generation that happened to
// touch the newest message rather than the one that ran last.
func (s *Store) LatestSentimentGeneration(ctx context.Context) (SentimentGeneration, bool, error) {
	var gen SentimentGeneration
	err := s.db.QueryRowContext(ctx,
		`SELECT model, lexicon_version FROM sentiment_state ORDER BY updated_at DESC, conversation_id DESC LIMIT 1`).
		Scan(&gen.Model, &gen.LexiconVersion)
	if err == sql.ErrNoRows {
		return SentimentGeneration{}, false, nil
	}
	if err != nil {
		return SentimentGeneration{}, false, fmt.Errorf("latest sentiment generation: %w", err)
	}
	return gen, true, nil
}

// MonthSentiment returns the month's sentiment scores aggregated per (UTC day,
// construct) for one generation, ordered day then construct so the fold is
// deterministic.
//
// Three filters, and every one of them is load-bearing:
//
//   - The generation pin, for the comparability reason above. An unset
//     generation returns nothing rather than averaging every generation in the
//     table together.
//   - The contact_sentiment_optout guard, applied as a NOT EXISTS INSIDE this
//     query rather than as a caller-supplied id list. SPEC-0027 makes opt-out
//     DELETION rather than suppression, so in a settled database these rows are
//     already gone — but a contact who opts out while a scoring run is in flight
//     can have scores written back moments later, and their affect must not
//     reach a day tint even for the minutes before the next run cleans up.
//     PutSentimentBatch guards its writes the same way and for the same reason;
//     making it a parameter here would leave the privacy guarantee to whether
//     each future caller remembered to pass it, which is not a guarantee.
//   - exclude, the journal.exclude_conversations denylist, resolved to ids and
//     applied through the same notInClause the rest of this file uses. A
//     denylisted thread must not colour the calendar any more than it may
//     inflate the stat tiles (REQ-0016-015).
//
// Bounded by a sargable ts_unix range on idx_message_sentiment_ts. The join to
// messages cannot fan out: messages.hash is UNIQUE.
func (s *Store) MonthSentiment(ctx context.Context, year int, month time.Month, gen SentimentGeneration, exclude []string) ([]SentimentDayConstruct, error) {
	if gen.Model == "" || gen.LexiconVersion == "" {
		return nil, nil // nothing has been scored under a known generation
	}
	excl, err := s.excludedConversationIDs(ctx, exclude)
	if err != nil {
		return nil, err
	}
	start := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)

	args := []any{gen.Model, gen.LexiconVersion, start.Unix(), start.AddDate(0, 1, 0).Unix()}
	q := `
SELECT date(ms.ts_unix,'unixepoch') d, ms.construct, SUM(ms.score), COUNT(*)
  FROM message_sentiment ms
  JOIN messages m ON m.hash = ms.message_hash
 WHERE ms.model = ? AND ms.lexicon_version = ?
   AND ms.ts_unix >= ? AND ms.ts_unix < ?
   AND m.is_system = 0 AND TRIM(m.body) <> ''
   AND NOT EXISTS (SELECT 1 FROM contact_sentiment_optout o WHERE o.contact_id = ms.contact_id)`
	q += notInClause("m.conversation_id", excl, &args)
	q += ` GROUP BY d, ms.construct ORDER BY d, ms.construct`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("month sentiment: %w", err)
	}
	defer rows.Close()
	var out []SentimentDayConstruct
	for rows.Next() {
		var a SentimentDayConstruct
		if err := rows.Scan(&a.Day, &a.Construct, &a.Sum, &a.N); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MonthTopReactions returns the top reaction emojis per day for the given
// month (issue #299), for the calendar day cells' emoji chips. Days map to
// "YYYY-MM-DD" keys; each value lists the day's most-used emojis, ties
// broken by emoji so the chips are deterministic for the same data. The
// journal exclude denylist is honored exactly like JournalStats — an
// excluded conversation's reactions must not surface on the calendar.
//
// One GROUP BY over reactions joined to messages bounded by a sargable
// ts_unix range; per-day top-N selection happens in Go (<=31 * few rows).
func (s *Store) MonthTopReactions(ctx context.Context, year int, month time.Month, exclude []string, perDay int) (map[string][]EmojiCount, error) {
	if perDay <= 0 {
		return nil, nil
	}
	excl, err := s.excludedConversationIDs(ctx, exclude)
	if err != nil {
		return nil, err
	}
	start := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	startUnix := start.Unix()
	endUnix := start.AddDate(0, 1, 0).Unix()

	q := `
SELECT date(m.ts_unix,'unixepoch') d, r.emoji, COUNT(*) n
  FROM reactions r
  JOIN messages m ON m.hash = r.message_hash
 WHERE m.ts_unix >= ? AND m.ts_unix < ?`
	var args []any
	args = append(args, startUnix, endUnix)
	q += notInClause("m.conversation_id", excl, &args)
	q += ` GROUP BY d, r.emoji ORDER BY d, n DESC, r.emoji`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("month top reactions: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]EmojiCount)
	for rows.Next() {
		var day, emoji string
		var n int
		if err := rows.Scan(&day, &emoji, &n); err != nil {
			return nil, err
		}
		if len(out[day]) < perDay {
			out[day] = append(out[day], EmojiCount{Emoji: emoji, Count: n})
		}
	}
	return out, rows.Err()
}
