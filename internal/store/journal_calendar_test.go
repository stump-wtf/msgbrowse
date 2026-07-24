package store

import (
	"context"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

func TestLongestStreak(t *testing.T) {
	cases := []struct {
		name string
		days []string
		want int
	}{
		{"empty", nil, 0},
		{"single", []string{"2023-05-01"}, 1},
		{"consecutive", []string{"2023-05-01", "2023-05-02", "2023-05-03"}, 3},
		{"gap picks longest", []string{"2023-05-01", "2023-05-02", "2023-05-05", "2023-05-06", "2023-05-07"}, 3},
		{"month rollover", []string{"2026-01-30", "2026-01-31", "2026-02-01"}, 3},
		{"year rollover", []string{"2025-12-31", "2026-01-01"}, 2},
	}
	for _, c := range cases {
		if got := longestStreak(c.days); got != c.want {
			t.Errorf("%s: longestStreak = %d, want %d", c.name, got, c.want)
		}
	}
}

// buildJournalFor rebuilds the mechanical journal_days from the seeded messages.
func buildJournalFor(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	days, err := st.BuildJournalDays(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range days {
		if err := st.PutJournalDay(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
}

func TestJournalMonthYearAndDay(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, conv, source.Signal, []signal.Message{
		msg("Harper", "2023-05-01 09:00:00", "Harper", "a", nil, nil),
		msg("Harper", "2023-05-01 10:00:00", "Harper", "b", nil, nil),
		msg("Harper", "2023-05-03 09:00:00", "Harper", "c", nil, nil),
		msg("Harper", "2023-08-15 09:00:00", "Harper", "d", nil, nil), // other month
	}); err != nil {
		t.Fatal(err)
	}
	buildJournalFor(t, st)
	if err := st.PutDayDigest(ctx, JournalDigest{
		Day: "2023-05-01", Model: "m", PromptVersion: "pv", Body: "sum",
		Structured: `{"summary":"sum","mood":"upbeat"}`, Mood: "upbeat",
	}); err != nil {
		t.Fatal(err)
	}

	// Month grid: only May days, with mood on the digested one.
	month, err := st.JournalMonth(ctx, 2023, time.May)
	if err != nil {
		t.Fatal(err)
	}
	if len(month) != 2 {
		t.Fatalf("May grid = %d days, want 2 (Aug excluded)", len(month))
	}
	if month[0].Day != "2023-05-01" || month[0].MessageCount != 2 || month[0].Mood != "upbeat" || !month[0].HasDigest {
		t.Errorf("day[0] = %+v, want 2023-05-01 / 2 msgs / upbeat / digested", month[0])
	}
	if month[1].Day != "2023-05-03" || month[1].Mood != "" || month[1].HasDigest {
		t.Errorf("day[1] = %+v, want 2023-05-03 / no mood / no digest", month[1])
	}

	years, err := st.JournalYears(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(years) != 1 || years[0] != 2023 {
		t.Errorf("years = %v, want [2023]", years)
	}
	if latest, _ := st.LatestJournalDay(ctx); latest != "2023-08-15" {
		t.Errorf("latest day = %q, want 2023-08-15", latest)
	}
	if d, ok, _ := st.LatestJournalDayInYear(ctx, 2023); !ok || d != "2023-08-15" {
		t.Errorf("LatestJournalDayInYear(2023) = %q,%v, want 2023-08-15,true", d, ok)
	}
	if _, ok, _ := st.LatestJournalDayInYear(ctx, 2019); ok {
		t.Error("LatestJournalDayInYear(2019) ok should be false (no days)")
	}

	// Single day view carries the structured digest.
	v, ok, err := st.GetJournalDay(ctx, "2023-05-01")
	if err != nil || !ok {
		t.Fatalf("GetJournalDay ok=%v err=%v", ok, err)
	}
	if v.MessageCount != 2 || v.Mood != "upbeat" || v.DigestStructured == "" {
		t.Errorf("day view = %+v, want 2 msgs / upbeat / structured set", v)
	}
	if _, ok, _ := st.GetJournalDay(ctx, "2020-01-01"); ok {
		t.Error("GetJournalDay for a day with no rollup should be ok=false")
	}
}

func TestJournalStats(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	// 2023-05-01 is a Monday; seed 3 messages that day + one each on 05-02/05-03,
	// all at 09:00 UTC → peak hour 9, most-active weekday Monday, streak 3.
	if _, err := st.ReplaceConversationMessages(ctx, conv, source.Signal, []signal.Message{
		msg("Harper", "2023-05-01 09:00:00", "Harper", "a", nil, nil),
		msg("Harper", "2023-05-01 09:05:00", "Harper", "b", nil, nil),
		msg("Harper", "2023-05-01 09:10:00", "Harper", "c", nil, nil),
		msg("Harper", "2023-05-02 09:00:00", "Harper", "d", nil, nil),
		msg("Harper", "2023-05-03 09:00:00", "Harper", "e", nil, nil),
	}); err != nil {
		t.Fatal(err)
	}
	buildJournalFor(t, st)

	stats, err := st.JournalStats(ctx, 2023, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !stats.HasActivity {
		t.Fatal("HasActivity = false")
	}
	if stats.DaysWithEntries != 3 || stats.LongestStreakDays != 3 {
		t.Errorf("days/streak = %d/%d, want 3/3", stats.DaysWithEntries, stats.LongestStreakDays)
	}
	if stats.MostActiveWeekday != time.Monday {
		t.Errorf("most-active weekday = %v, want Monday", stats.MostActiveWeekday)
	}
	if stats.PeakHour != 9 {
		t.Errorf("peak hour = %d, want 9", stats.PeakHour)
	}
	// All-time (year 0) agrees here (single year of data).
	if all, _ := st.JournalStats(ctx, 0, nil); all.DaysWithEntries != 3 {
		t.Errorf("all-time days = %d, want 3", all.DaysWithEntries)
	}
}
