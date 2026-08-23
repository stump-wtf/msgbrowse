package web

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/joestump/msgbrowse/internal/facts"
	"github.com/joestump/msgbrowse/internal/store"
)

// contactData drives the /contact/{id} profile page.
type contactData struct {
	baseData
	C             *store.Contact
	PrimarySource string // first thread's source ("" when none) — the header presence dot; empty-safe, so the template never indexes an empty slice
	FactGroups    []factGroup
	FactCount     int
	// FactsExtracted, FactScanned and FactConvTotal separate the two empty
	// states the facts section used to render identically (#366). fact_state
	// carries one row per conversation the extractor LOOKED AT, including the
	// ones that produced nothing, so "we have never analyzed this person" and
	// "we analyzed them and they have no durable facts" are distinguishable —
	// and must be distinguished, because the first is a configuration problem
	// with a fix and the second is a finding. Before this they were the same
	// blank panel, which read as a verdict about the person.
	FactsExtracted bool
	FactScanned    int
	FactConvTotal  int
	Stats          store.ContactStats
	// VolumeHUD and PaceHUD are the profile's two derived-stat strips
	// (message volume, then pace/photos/active-hour), rendered through the
	// shared "hud" define (#395) instead of hand-rolled markup.
	VolumeHUD hudData
	PaceHUD   hudData
	Bars      []sparkBar // message-volume sparkline (year-rolled), Go-normalized
	SparkW    int
	SparkH    int
	Reactions []store.EmojiCount
}

// factGroup is a contact's facts under one category, in declared-category order.
type factGroup struct {
	Category string
	Facts    []store.ContactFact
}

// contactVolumeHUD builds the profile's first stat strip (Messages / You
// sent / Received) through the shared "hud" define (#395), replacing what
// used to be a hand-rolled copy of Home/Status's stat-strip markup.
func contactVolumeHUD(stats store.ContactStats) hudData {
	return hudData{
		Class: "mb-3",
		Cells: []hudCell{
			{Label: "Messages", Value: commaInt(int64(stats.TotalMessages))},
			{Label: "You sent", Value: commaInt(int64(stats.SentMessages))},
			{Label: "Received", Value: commaInt(int64(stats.ReceivedMessages))},
		},
	}
}

// contactPaceHUD builds the profile's second stat strip (Messages/day /
// Photos shared / Most active hour) through the shared "hud" define (#395).
func contactPaceHUD(pace, photos int, activeHourLabel string) hudData {
	hour := "—"
	if activeHourLabel != "" {
		hour = activeHourLabel
	}
	return hudData{
		Class: "mb-8",
		Cells: []hudCell{
			{Label: "Messages / day", Value: commaInt(int64(pace))},
			{Label: "Photos shared", Value: commaInt(int64(photos))},
			{Label: "Most active hour", Value: hour, Small: true},
		},
	}
}

// sparkBar is one pre-computed SVG bar for the message-volume sparkline. All
// geometry is computed in Go so the template emits presentational ATTRIBUTES
// only (the strict CSP forbids inline style=).
type sparkBar struct {
	X, Y, W, H     int
	LabelX, LabelY int
	Opacity        string
	Label          string
	Count          int
}

// handleContact renders the per-person Contact + AI Facts + Profile page. It is
// keyed by contact id (the merged-person grain), reached from the transcript
// header. A bad/unknown/group contact 404s.
func (s *Server) handleContact(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	c, err := s.store.GetContactByID(ctx, id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}

	facts, err := s.store.ContactFacts(ctx, id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	stats, err := s.store.ContactStats(ctx, id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	vol, err := s.store.ContactMessageVolume(ctx, id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	hour, _, hasHour, err := s.store.ContactMostActiveHour(ctx, id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	reactions, err := s.store.ContactTopReactions(ctx, id, 8)
	if err != nil {
		s.serverError(w, err)
		return
	}
	// Governing: SPEC-0005 (contact-facts) REQ-0005-004 — the incremental cursor
	// is the only record of what extraction has examined, so it is also the only
	// honest source for the empty state below.
	scan, err := s.store.ContactFactScan(ctx, id)
	if err != nil {
		s.serverError(w, err)
		return
	}

	title := humanName(c.DisplayName) + " · msgbrowse"
	var base baseData
	if isPartialRequest(r) {
		base = partialBase(title, 0)
	} else {
		base, err = s.baseData(ctx, title, 0)
		if err != nil {
			s.serverError(w, err)
			return
		}
	}
	base.NavTitle = humanName(c.DisplayName)
	// A contact is a global surface — neither the Messages nor Media header tab.

	activeHourLabel := ""
	if hasHour {
		activeHourLabel = hourLabel(hour)
	}
	primarySource := ""
	if len(c.Conversations) > 0 {
		primarySource = c.Conversations[0].Source
	}
	bars, sw, sh := buildSparkline(vol)
	pace := int(stats.MessagesPerDay + 0.5)
	s.render(w, r, "contact", contactData{
		baseData:       base,
		C:              c,
		PrimarySource:  primarySource,
		FactGroups:     groupFacts(facts),
		FactCount:      len(facts),
		FactsExtracted: scan.Extracted(),
		FactScanned:    scan.Scanned,
		FactConvTotal:  scan.Total,
		Stats:          stats,
		VolumeHUD:      contactVolumeHUD(stats),
		PaceHUD:        contactPaceHUD(pace, stats.Photos, activeHourLabel),
		Bars:           bars,
		SparkW:         sw,
		SparkH:         sh,
		Reactions:      reactions,
	})
}

// hourLabel renders a 0–23 hour as a 12-hour clock label ("11 PM", "12 AM").
func hourLabel(h int) string {
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

// groupFacts buckets a contact's facts by category in the DECLARED
// facts.Categories order (not SQL's alphabetical order), skipping empty
// categories. Facts with an unexpected category (shouldn't happen — extraction
// coerces to the known set) are appended last so none are ever dropped.
func groupFacts(all []store.ContactFact) []factGroup {
	var groups []factGroup
	known := make(map[string]bool, len(facts.Categories))
	for _, cat := range facts.Categories {
		known[cat] = true
		var fs []store.ContactFact
		for _, f := range all {
			if f.Category == cat {
				fs = append(fs, f)
			}
		}
		if len(fs) > 0 {
			groups = append(groups, factGroup{Category: cat, Facts: fs})
		}
	}
	extra := map[string][]store.ContactFact{}
	var order []string
	for _, f := range all {
		if known[f.Category] {
			continue
		}
		if _, ok := extra[f.Category]; !ok {
			order = append(order, f.Category)
		}
		extra[f.Category] = append(extra[f.Category], f)
	}
	for _, cat := range order {
		groups = append(groups, factGroup{Category: cat, Facts: extra[cat]})
	}
	return groups
}

// Sparkline geometry (SVG user units). Bars are rolled up to YEARS to match the
// design's year sparkline; a fixed bar width + gap keeps the SVG compact. A
// label band sits BELOW the bars so year labels don't overlap them.
const (
	sparkChartH = 46
	sparkBarW   = 24
	sparkGap    = 10
	sparkPadTop = 6
	sparkLabelH = 12 // band below the bars for the year labels
)

// buildSparkline rolls per-month volume up to per-year bars, GAP-FILLS every
// year in the span (a silent year renders as an empty slot with its label, not a
// collapsed axis), and pre-computes each bar's SVG geometry + opacity. Returns
// the bars and the viewBox W/H.
func buildSparkline(months []store.MonthBucket) ([]sparkBar, int, int) {
	if len(months) == 0 {
		return nil, 0, 0
	}
	years := map[int]int{}
	minY, maxY := 0, 0
	first := true
	for _, b := range months {
		if len(b.Month) < 4 {
			continue
		}
		y, err := strconv.Atoi(b.Month[:4])
		if err != nil {
			continue
		}
		years[y] += b.Count
		if first || y < minY {
			minY = y
		}
		if first || y > maxY {
			maxY = y
		}
		first = false
	}
	if first { // no parseable year
		return nil, 0, 0
	}
	maxCount := 1
	for _, c := range years {
		if c > maxCount {
			maxCount = c
		}
	}
	baseline := sparkPadTop + sparkChartH
	viewH := baseline + sparkLabelH
	var bars []sparkBar
	for i, y := 0, minY; y <= maxY; i, y = i+1, y+1 { // gap-fill: every year in the span
		cnt := years[y]
		h := cnt * sparkChartH / maxCount
		if cnt > 0 && h < 2 { // a present-but-tiny year still shows a stub; a silent year stays 0-height
			h = 2
		}
		x := i * (sparkBarW + sparkGap)
		op := 0.35 + 0.6*float64(cnt)/float64(maxCount)
		bars = append(bars, sparkBar{
			X: x, Y: baseline - h, W: sparkBarW, H: h,
			LabelX: x + sparkBarW/2, LabelY: viewH - 3,
			Opacity: fmt.Sprintf("%.2f", op), Label: strconv.Itoa(y), Count: cnt,
		})
	}
	viewW := (maxY-minY+1)*(sparkBarW+sparkGap) - sparkGap
	return bars, viewW, viewH
}
