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
       COALESCE(dg.body,''), COALESCE(dg.model,''), COALESCE(dg.structured,''), COALESCE(dg.mood,'')
  FROM journal_days jd
  LEFT JOIN journal_digests dg ON dg.day = jd.day
 WHERE jd.day = ?`, day).
		Scan(&v.Day, &v.MessageCount, &v.ConversationCount, &srcJSON, &sendersJSON, &v.UpdatedAt,
			&v.DigestBody, &v.DigestModel, &v.DigestStructured, &v.Mood)
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
SELECT jd.day, jd.message_count, COALESCE(dg.mood,''), dg.day IS NOT NULL
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
		if err := rows.Scan(&d.Day, &d.MessageCount, &d.Mood, &d.HasDigest); err != nil {
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
