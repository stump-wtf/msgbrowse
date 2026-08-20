// Calendar Navigation Scroll Behaviour
//
// Paging the calendar used to throw the reader back to the top of the page. The
// month chevrons are boosted, URL-pushing links, so htmx applied its default for
// a page navigation — scroll the swapped target to the top — which is correct
// for "followed a link" and wrong for "paged the calendar", where the arrow the
// user just clicked jumps out from under the cursor.
//
// These tests pin the explicit modifiers, because the failure mode is a DEFAULT
// reasserting itself: someone tidies `hx-swap="outerHTML show:none"` back to
// `hx-swap="outerHTML"`, the markup still looks right, and the bug returns
// silently. Asserting the modifier is present is the only thing that catches it.
//
// @joestump-agent 08/20/2026 - Added for issue #369.
package web

import (
	"strings"
	"testing"
)

// TestCalendarNavPreservesScroll: month chevrons and year tabs must not move the
// viewport, and must carry a stable id so focus survives the swap.
func TestCalendarNavPreservesScroll(t *testing.T) {
	srv, st := newJournalServer(t)
	seedJournalDays(t, st, "Alice", []string{"2026-01-05", "2026-02-06", "2026-03-07"})

	body := get(t, srv, "/journal").Body.String()

	for _, id := range []string{`id="journal-cal-prev"`, `id="journal-cal-next"`} {
		if !contains(body, id) {
			t.Errorf("month chevron missing %s — without a stable id htmx cannot restore "+
				"keyboard focus after the swap and a keyboard user lands at the top of the document", id)
		}
	}

	// Each nav control's own tag must carry the modifier; searching the whole
	// page would pass on any one control having it.
	for _, marker := range []string{`id="journal-cal-prev"`, `id="journal-cal-next"`, `id="journal-year-`} {
		tag := tagContaining(t, body, marker)
		if !strings.Contains(tag, "show:none") {
			t.Errorf("nav control %s lacks an explicit show:none scroll modifier; "+
				"htmx's boosted-navigation default scrolls to the top, which is the bug.\ntag: %s", marker, tag)
		}
		if !strings.Contains(tag, "focus-scroll:false") {
			t.Errorf("nav control %s lacks focus-scroll:false, so restoring focus scrolls the page.\ntag: %s", marker, tag)
		}
	}
}

// TestDaySelectionScrollsToTheDayCard: selecting a day SHOULD scroll, because
// the new content is below the calendar — but to the card, not the document top.
func TestDaySelectionScrollsToTheDayCard(t *testing.T) {
	srv, st := newJournalServer(t)
	seedJournalDays(t, st, "Alice", []string{"2026-01-05"})

	// Ask for a specific day so the editorial card — the scroll target —
	// actually renders; with no day selected there is nothing to scroll to.
	body := get(t, srv, "/journal?day=2026-01-05").Body.String()
	tag := tagContaining(t, body, `<a class="cal-day`)
	if !strings.Contains(tag, "show:#journal-day-card:top") {
		t.Errorf("day cell should scroll to the day card it loads, not the page top.\ntag: %s", tag)
	}
	// The scroll target has to exist, or the modifier silently does nothing.
	if !contains(body, `id="journal-day-card"`) {
		t.Error(`no element with id="journal-day-card" — the day cells' scroll target does not exist`)
	}
}

// tagContaining returns the full <a …> tag that contains marker, so an
// assertion is anchored to one control instead of the whole document.
func tagContaining(t *testing.T, body, marker string) string {
	t.Helper()
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("marker %q not found in the rendered page", marker)
	}
	start := strings.LastIndex(body[:i+len(marker)], "<a")
	if start < 0 {
		t.Fatalf("no opening <a tag before %q", marker)
	}
	end := strings.Index(body[start:], ">")
	if end < 0 {
		t.Fatalf("unterminated tag around %q", marker)
	}
	return body[start : start+end+1]
}
