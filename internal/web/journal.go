package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/joestump/msgbrowse/internal/journal"
	"github.com/joestump/msgbrowse/internal/store"
)

// journalData drives the /journal mood-calendar + editorial-day view (redesign
// Phase 2). Navigation is by query params (?year&month&day), all boosted, so
// there is no client JS state.
type journalData struct {
	baseData
	Empty         bool        // no journal built yet
	Years         []int       // year tabs
	ActiveYear    int         // year tab + stats + month context
	MonthLabel    string      // "June 2026"
	PrevURL       string      // previous month (?year&month), "" not used
	NextURL       string      // next month
	Grid          [][]calCell // 6x7 month grid, laid out in Go
	Stats         store.JournalStats
	WeekdayLabel  string   // "Saturdays" ("" when no activity)
	PeakHourLabel string   // "11 PM" ("" when no activity)
	Moods         []string // legend order (journal.Moods)
	Selected      *dayCard // the selected day's editorial card (nil = none)
}

// calCell is one day cell in the month grid. A zero-value cell (InMonth false)
// is a leading/trailing blank.
type calCell struct {
	InMonth    bool
	DayNum     int
	Count      int
	MoodClass  string // "cal-day--upbeat" etc; "" when no digest
	HasContent bool   // true → the cell links to ?day=
	Selected   bool
	URL        string
}

// dayCard is the editorial reading card for one selected day.
type dayCard struct {
	Day               string
	DateLabel         string // "Saturday, June 28, 2026"
	MessageCount      int
	ConversationCount int
	Mood              string
	MoodClass         string
	Digest            *journal.Digest     // parsed structured digest (nil when none)
	Body              string              // prose fallback (older/parse-failed digests)
	TopSenders        []store.SenderCount // mechanical fallback when no digest at all
}

// handleJournal renders the journal as a mood-tinted month calendar with an
// editorial day card. Boosted navigations swap only #main-content.
func (s *Server) handleJournal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var base baseData
	if isPartialRequest(r) {
		base = partialBase("Journal · msgbrowse", 0)
	} else {
		var err error
		base, err = s.baseData(ctx, "Journal · msgbrowse", 0)
		if err != nil {
			s.serverError(w, err)
			return
		}
	}

	latest, err := s.store.LatestJournalDay(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if latest == "" {
		s.render(w, r, "journal", journalData{baseData: base, Empty: true})
		return
	}

	q := r.URL.Query()
	day := q.Get("day")
	if !isValidDay(day) {
		day = ""
	}
	yearQ, monthQ := q.Get("year"), q.Get("month")
	if day == "" {
		switch {
		case yearQ == "":
			// The bare /journal landing opens on the newest day's editorial card.
			day = latest
		case monthQ == "":
			// A year tab (?year with no month) opens on that year's MOST RECENT
			// day — not January, which would render an empty grid for a year
			// whose activity starts later.
			if y, err := strconv.Atoi(yearQ); err == nil {
				d, ok, derr := s.store.LatestJournalDayInYear(ctx, y)
				if derr != nil {
					s.serverError(w, derr)
					return
				}
				if ok {
					day = d
				}
			}
		}
		// Otherwise (?year&month) show that month's calendar with no day
		// pre-selected until the user clicks one.
	}
	year, month := journalContext(day, yearQ, monthQ, latest)

	years, err := s.store.JournalYears(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	monthDays, err := s.store.JournalMonth(ctx, year, month)
	if err != nil {
		s.serverError(w, err)
		return
	}
	stats, err := s.store.JournalStats(ctx, year, s.journalExclude)
	if err != nil {
		s.serverError(w, err)
		return
	}

	data := journalData{
		baseData:      base,
		Years:         years,
		ActiveYear:    year,
		MonthLabel:    time.Date(year, month, 1, 0, 0, 0, 0, time.UTC).Format("January 2006"),
		PrevURL:       monthNavURL(year, month, -1),
		NextURL:       monthNavURL(year, month, +1),
		Grid:          buildMonthGrid(year, month, monthDays, day),
		Stats:         stats,
		Moods:         journal.Moods,
		WeekdayLabel:  weekdayLabel(stats),
		PeakHourLabel: peakHourLabel(stats),
	}
	if day != "" {
		if view, ok, err := s.store.GetJournalDay(ctx, day); err != nil {
			s.serverError(w, err)
			return
		} else if ok {
			data.Selected = buildDayCard(view)
		}
	}
	s.render(w, r, "journal", data)
}

// journalContext resolves the (year, month) to show from the selected day, the
// explicit ?year&month params, or (default) the newest journal day.
func journalContext(day, yearQ, monthQ, latest string) (int, time.Month) {
	if day != "" {
		if t, err := time.Parse("2006-01-02", day); err == nil {
			return t.Year(), t.Month()
		}
	}
	if y, err := strconv.Atoi(yearQ); err == nil && y >= 1970 && y <= 9999 {
		month := time.January
		if m, err := strconv.Atoi(monthQ); err == nil && m >= 1 && m <= 12 {
			month = time.Month(m)
		}
		return y, month
	}
	t, _ := time.Parse("2006-01-02", latest)
	return t.Year(), t.Month()
}

// buildMonthGrid lays the month's present days into a fixed 6x7 (Sun-first) grid;
// absent days are blank cells. Present days link to their editorial card.
func buildMonthGrid(year int, month time.Month, days []store.JournalMonthDay, selected string) [][]calCell {
	byDOM := make(map[int]store.JournalMonthDay, len(days))
	for _, d := range days {
		if len(d.Day) == 10 {
			if dom, err := strconv.Atoi(d.Day[8:10]); err == nil {
				byDOM[dom] = d
			}
		}
	}
	first := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	leading := int(first.Weekday()) // 0=Sun..6=Sat
	daysInMonth := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()

	var grid [][]calCell
	row := make([]calCell, 0, 7)
	for i := 0; i < leading; i++ {
		row = append(row, calCell{})
	}
	for dom := 1; dom <= daysInMonth; dom++ {
		dayStr := fmt.Sprintf("%04d-%02d-%02d", year, month, dom)
		cell := calCell{InMonth: true, DayNum: dom}
		if md, ok := byDOM[dom]; ok {
			cell.Count = md.MessageCount
			cell.HasContent = true
			cell.URL = "/journal?day=" + dayStr
			if md.Mood != "" {
				cell.MoodClass = "cal-day--" + md.Mood
			}
		}
		cell.Selected = dayStr == selected
		row = append(row, cell)
		if len(row) == 7 {
			grid = append(grid, row)
			row = make([]calCell, 0, 7)
		}
	}
	if len(row) > 0 {
		for len(row) < 7 {
			row = append(row, calCell{})
		}
		grid = append(grid, row)
	}
	return grid
}

// buildDayCard assembles the editorial card, parsing the structured digest when
// present and falling back to prose then the mechanical top-senders.
func buildDayCard(v store.JournalDayView) *dayCard {
	c := &dayCard{
		Day:               v.Day,
		MessageCount:      v.MessageCount,
		ConversationCount: v.ConversationCount,
		Mood:              v.Mood,
		Body:              v.DigestBody,
		TopSenders:        v.TopSenders,
	}
	if t, err := time.Parse("2006-01-02", v.Day); err == nil {
		c.DateLabel = t.Format("Monday, January 2, 2006")
	} else {
		c.DateLabel = v.Day
	}
	if v.Mood != "" {
		c.MoodClass = "cal-day--" + v.Mood
	}
	if v.DigestStructured != "" {
		var d journal.Digest
		if err := json.Unmarshal([]byte(v.DigestStructured), &d); err == nil {
			c.Digest = &d
		}
	}
	return c
}

// monthNavURL builds a /journal?year&month link delta months away from (year, month).
func monthNavURL(year int, month time.Month, delta int) string {
	t := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC).AddDate(0, delta, 0)
	return fmt.Sprintf("/journal?year=%d&month=%d", t.Year(), int(t.Month()))
}

// weekdayLabel renders the most-active weekday as a plural ("Saturdays"), or ""
// when there's no activity.
func weekdayLabel(st store.JournalStats) string {
	if !st.HasActivity || st.MostActiveWeekdayN == 0 {
		return ""
	}
	return st.MostActiveWeekday.String() + "s"
}

// peakHourLabel renders the peak hour as a 12-hour label ("11 PM"), or "".
func peakHourLabel(st store.JournalStats) string {
	if !st.HasActivity || st.PeakHourN == 0 {
		return ""
	}
	h := st.PeakHour
	switch {
	case h <= 0:
		return "12 AM"
	case h < 12:
		return fmt.Sprintf("%d AM", h)
	case h == 12:
		return "12 PM"
	default:
		return fmt.Sprintf("%d PM", h-12)
	}
}

// isValidDay reports whether s is a well-formed YYYY-MM-DD date.
func isValidDay(s string) bool {
	if len(s) != 10 {
		return false
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}
