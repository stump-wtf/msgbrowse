package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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
//
// A day with messages always lands in exactly one of three DEFINED mood states
// (#370) — see journalmood.go for the precedence and why it exists:
//
//	digest    → MoodClass set, Inferred false      (full tint)
//	sentiment → MoodClass set, Inferred true       (faint tint + dashed edge)
//	neither   → MoodClass "",  Unanalyzed true     (explicit "not analysed yet")
//
// The three are mutually exclusive and jointly exhaustive over HasContent cells,
// which is what stops a day from rendering as a bare `cal-day` again.
type calCell struct {
	InMonth   bool
	DayNum    int
	Count     int
	MoodClass string // "cal-day--upbeat" etc; "" only when the day has no mood at all
	// Inferred marks a tint that came from sentiment scores rather than a
	// digest. It renders fainter and dashed so the calendar never implies an
	// editorial reading of a day the digest pass has not reached.
	Inferred bool
	// Unanalyzed marks a day WITH messages that has neither a digest mood nor
	// enough sentiment to infer one. It is a real state with its own class and
	// its own legend entry, not an absence.
	Unanalyzed bool
	HasContent bool // true → the cell links to ?day=
	Stale      bool // digest predates later messages on this day (#240)
	Selected   bool
	URL        string
	// Reactions is the day's top emojis (issue #299), rendered as small
	// emoji × count chips on the cell. nil/empty renders no chips.
	Reactions []store.EmojiCount
}

// dayCard is the editorial reading card for one selected day.
type dayCard struct {
	Day               string
	DateLabel         string // "Saturday, June 28, 2026"
	MessageCount      int
	ConversationCount int
	Mood              string
	MoodClass         string
	// MoodInferred says the mood on this card came from sentiment scores rather
	// than a digest (#370). The chip is labelled and styled differently so a
	// reader is never told a day was editorialized when it was not.
	MoodInferred bool
	Digest       *journal.Digest     // parsed structured digest (nil when none)
	Body         string              // prose fallback (older/parse-failed digests)
	TopSenders   []store.SenderCount // mechanical fallback when no digest at all
	// Stale marks a digest written BEFORE later messages landed on this day
	// (#240). NewMessages is how many arrived since. Both stay zero-valued when
	// the captured count is unknown (a pre-v15 digest), so an old digest is
	// never accused of being out of date on no evidence.
	Stale       bool
	NewMessages int
}

// handleJournal renders the journal as a mood-tinted month calendar with an
// editorial day card. Boosted navigations swap only #main-content.
func (s *Server) handleJournal(w http.ResponseWriter, r *http.Request) {
	s.renderJournalPage(w, r)
}

// renderJournalPage assembles and renders the Journal page (full document or
// boosted #main-content partial). The journal surface is deliberately free of
// build/indexing machinery — those controls live on the Settings → Status tab
// (issue #300-era consolidation of the LLM/semantic surfaces into Settings).
func (s *Server) renderJournalPage(w http.ResponseWriter, r *http.Request) {
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
	// The header's Journal tab reads active on every /journal render (#238),
	// set once for both branches exactly as handleGallery does for Media. Only
	// the full-page branch actually renders the shell that reads it — a boosted
	// partial emits no header, and an htmx history-restore takes the full-page
	// branch because isPartialRequest excludes HX-History-Restore-Request. The
	// single assignment is for consistency, not because the partial needs it;
	// after a boosted swap shell.js re-derives the active tab from location.
	base.NavTab = navTabJournal

	latest, err := s.store.LatestJournalDay(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if latest == "" {
		// The empty state points at the build surface on Settings → Status —
		// the journal page itself carries no build machinery.
		s.render(w, r, "journal", journalData{
			baseData: base, Empty: true,
		})
		return
	}

	// Day/month context comes from the query on a GET, and from the FORM on the
	// build POSTs — their URLs (/journal/build etc.) carry no query at all, so
	// reading only r.URL.Query() would silently bounce the user from the day they
	// acted on to the newest day in the archive.
	q := r.URL.Query()
	pick := func(name string) string {
		if v := q.Get(name); v != "" {
			return v
		}
		return strings.TrimSpace(r.PostFormValue(name))
	}
	day := pick("day")
	if !isValidDay(day) {
		day = ""
	}
	yearQ, monthQ := pick("year"), pick("month")
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
	topReactions, err := s.store.MonthTopReactions(ctx, year, month, s.journalExclude, 2)
	if err != nil {
		s.serverError(w, err)
		return
	}
	stats, err := s.store.JournalStats(ctx, year, s.journalExclude)
	if err != nil {
		s.serverError(w, err)
		return
	}
	// The second, weaker mood source (#370). One read for the whole month, reused
	// by both the grid and the selected day's card — the selected day is always
	// inside the rendered month, because journalContext derives the month FROM it.
	inferred, err := s.monthSentimentMoods(ctx, year, month)
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
		Grid:          buildMonthGrid(year, month, monthDays, day, topReactions, inferred),
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
			data.Selected = buildDayCard(view, inferred[day])
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
// reactions maps "YYYY-MM-DD" → the day's top emojis for the cells' chips
// (issue #299); a nil map renders no chips. inferred maps "YYYY-MM-DD" → the
// sentiment-derived mood for days the digest pass has not reached (#370); a nil
// map simply leaves those days in the UNANALYSED state.
func buildMonthGrid(year int, month time.Month, days []store.JournalMonthDay, selected string, reactions map[string][]store.EmojiCount, inferred map[string]string) [][]calCell {
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
			cell.Reactions = reactions[dayStr]
			// The three-state precedence (#370), resolved here so the template
			// stays logic-free. A digest mood wins outright; otherwise the
			// sentiment fold gets a turn at a fainter tint; otherwise the day is
			// explicitly UNANALYSED. Both classes come from moodClass, so a mood
			// outside the fixed enum yields no class and lands the day in state 3
			// rather than emitting a model-derived one (REQ-0016-016).
			switch cell.MoodClass = moodClass(md.Mood); {
			case cell.MoodClass != "":
				// state 1: digested.
			case moodClass(inferred[dayStr]) != "":
				cell.MoodClass, cell.Inferred = moodClass(inferred[dayStr]), true
			default:
				cell.Unanalyzed = true
			}
		}
		if md, ok := byDOM[dom]; ok && md.Stale {
			cell.Stale = true
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
//
// inferredMood is the day's sentiment-derived mood ("" when it has none), used
// only when the day carries no digest mood — the same precedence the grid
// applies, so the card and the cell the reader just clicked can never disagree
// about what tinted it (#370).
func buildDayCard(v store.JournalDayView, inferredMood string) *dayCard {
	c := &dayCard{
		Day:               v.Day,
		MessageCount:      v.MessageCount,
		ConversationCount: v.ConversationCount,
		Body:              v.DigestBody,
		TopSenders:        v.TopSenders,
	}
	if t, err := time.Parse("2006-01-02", v.Day); err == nil {
		c.DateLabel = t.Format("Monday, January 2, 2006")
	} else {
		c.DateLabel = v.Day
	}
	// Mood and MoodClass are set together from a VALIDATED enum value, digest
	// first then sentiment. Mood is printed as the chip's label, so an
	// out-of-allowlist model string must not reach it either — a day whose digest
	// carries a mood this build does not know renders no chip at all rather than
	// an unstyled one (REQ-0016-016).
	if c.MoodClass = moodClass(v.Mood); c.MoodClass != "" {
		c.Mood = v.Mood
	} else if c.MoodClass = moodClass(inferredMood); c.MoodClass != "" {
		c.Mood, c.MoodInferred = inferredMood, true
	}
	if v.DigestStructured != "" {
		var d journal.Digest
		if err := json.Unmarshal([]byte(v.DigestStructured), &d); err == nil {
			c.Digest = &d
		}
	}
	if v.DigestStale() {
		c.Stale = true
		// Only a GROWN day has "more messages". A day can also shrink — a
		// re-ingest that dropped rows, or a conversation added to the journal
		// denylist — and that digest is equally out of date, but the count would
		// be negative, so the template falls back to a countless wording.
		if n := v.MessageCount - v.DigestMessageCount; n > 0 {
			c.NewMessages = n
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
