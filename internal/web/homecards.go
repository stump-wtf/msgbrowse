package web

import (
	"context"
	"fmt"
	"time"
)

// Home's two resurfacing cards (#239, SPEC-0006 REQ-0006-007): "On this day",
// which brings back a message from this calendar day in a previous year, and
// "Jump back in", the most recently active conversations.
//
// Both follow the overview.go convention of precomputing every display string
// here, so the template stays logic-free and this stays table-testable.

const (
	// onThisDayMaxYears bounds how many prior years are probed. Each year is one
	// indexed one-day range scan, so the cost is linear in this number; 20 covers
	// any realistic archive without unbounded work on a mis-seeded one.
	onThisDayMaxYears = 20
	// jumpBackLimit is how many recent conversations the second card lists.
	jumpBackLimit = 5
	// onThisDayPerDay / onThisDayFetch: the card shows ONE message, but a
	// candidate can be dropped after the fact (a body that previews to nothing —
	// see OnThisDayCandidates), so ask for a few alternates per day and cap the
	// total. The total cap is what stops the compound query eagerly evaluating
	// all 20 arms to build rows that are then discarded.
	onThisDayPerDay = 3
	onThisDayFetch  = 12
)

// onThisDayCard is the resurfaced message from the same calendar month/day in a
// previous year. Show is false whenever no prior year qualifies, and the
// template renders nothing at all then — never an empty frame, never a
// fabricated entry.
type onThisDayCard struct {
	Show             bool
	Day              string // "2019-07-25"
	DateLabel        string // "July 25, 2019"
	YearsLabel       string // "6 years ago"
	ConversationID   int64
	ConversationName string
	Source           string
	Sender           string // humanName'd, or "You" for the archive owner
	Initials         string
	AvatarColor      string
	Time             string // "14:32:07"
	Body             string // preview()-capped; escaped by html/template
	ConvURL          string // "/c/123"
	JournalURL       string // "/journal?day=2019-07-25"
}

// jumpBackItem is one row of the "Jump back in" card: avatar, name, relative
// stamp, link. Nothing more — the design brief's card is deliberately not a
// message preview, and a field the template never reads is work the store pays
// for on every Home render.
type jumpBackItem struct {
	ID          int64
	Name        string // humanName'd display name
	Initials    string
	AvatarColor string
	Rel         string // "2m" / "3h" / "Yesterday" / "4d" / "Oct 22, 2022"
	URL         string // "/c/123"
}

// overviewOnThisDay picks the resurfaced message for Home.
//
// Determinism is a requirement, not a nicety (#239): the only input taken from
// now is the CALENDAR DATE, and the store's per-day selection is longest-body
// then lowest-id. So the card is stable for a whole day and does not reshuffle
// on refresh. It walks years newest-first and takes the first year that has
// anything, which is also why the store must return arms in the requested order.
//
// The journal's exclude_conversations denylist is honoured here too — a
// conversation the user has hidden from the journal must not reappear on the
// landing page.
func (s *Server) overviewOnThisDay(ctx context.Context, now time.Time) (onThisDayCard, error) {
	_, mo, dd := now.Date()
	firstYear, _, err := s.store.MessageYearRange(ctx)
	if err != nil {
		return onThisDayCard{}, err
	}
	if firstYear == 0 {
		return onThisDayCard{}, nil // empty archive
	}
	oldest := now.Year() - onThisDayMaxYears
	if firstYear > oldest {
		oldest = firstYear
	}
	var days []string
	for y := now.Year() - 1; y >= oldest; y-- {
		days = append(days, fmt.Sprintf("%04d-%02d-%02d", y, int(mo), dd))
	}
	if len(days) == 0 {
		return onThisDayCard{}, nil
	}
	// Feb 29 needs no special case: a prior non-leap year simply yields an arm
	// whose one-day window matches nothing, and the card is omitted.
	rows, err := s.store.OnThisDayCandidates(ctx, days, onThisDayPerDay, onThisDayFetch, s.journalExclude)
	if err != nil {
		return onThisDayCard{}, err
	}
	if len(rows) == 0 {
		return onThisDayCard{}, nil
	}
	m := rows[0]

	year := 0
	if len(m.Day) >= 4 {
		if t, perr := time.Parse("2006-01-02", m.Day); perr == nil {
			year = t.Year()
		}
	}
	sender := humanName(m.Sender)
	if m.IsOwner {
		sender = "You"
	}
	return onThisDayCard{
		Show:             true,
		Day:              m.Day,
		DateLabel:        dateLabel(m.TS),
		YearsLabel:       yearsAgoLabel(now.Year() - year),
		ConversationID:   m.ConversationID,
		ConversationName: humanName(m.ConversationName),
		Source:           m.Source,
		Sender:           sender,
		Initials:         initials(m.ConversationName),
		AvatarColor:      avatarColor(m.ConversationName),
		Time:             clockTime(m.TS),
		Body:             m.Body, // store-side preview(): stripped, collapsed, rune-capped
		ConvURL:          fmt.Sprintf("/c/%d", m.ConversationID),
		JournalURL:       "/journal?day=" + m.Day,
	}, nil
}

// yearsAgoLabel renders the year gap the card's eyebrow shows. A zero or
// negative gap cannot occur (only prior years are probed) but degrades to the
// plural form rather than printing "0 years ago".
func yearsAgoLabel(n int) string {
	if n == 1 {
		return "1 year ago"
	}
	return fmt.Sprintf("%d years ago", n)
}

// overviewJumpBackIn lists the most recently active conversations with coarse
// relative stamps.
//
// It uses the dedicated LIMITed store query rather than baseData.Conversations:
// the boosted Home render deliberately skips the sidebar listing (REQ-0008-006),
// so a card fed from it would silently empty out on every boosted navigation.
func (s *Server) overviewJumpBackIn(ctx context.Context, n int) ([]jumpBackItem, error) {
	rows, err := s.store.RecentConversations(ctx, n)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	now := wallNow(time.Now())
	out := make([]jumpBackItem, 0, len(rows))
	for _, c := range rows {
		out = append(out, jumpBackItem{
			ID:          c.ID,
			Name:        humanName(c.Name),
			Initials:    initials(c.Name),
			AvatarColor: avatarColor(c.Name),
			Rel:         relTimeLabel(c.LastTSUnix, now),
			URL:         fmt.Sprintf("/c/%d", c.ID),
		})
	}
	return out, nil
}
