package web

// The digest gap as a NUMBER, on the surface that can close it (#370).
//
// Coverage was legible only as "91%", and a percentage is a score rather than a
// work queue — it invites "good enough" where "329 days have never been
// digested" invites a Build. The count belongs on the Settings → Journal build
// card, beside the control, and NOT on /journal, which REQ-0016-017 forbids from
// carrying any pipeline status at all.

import (
	"strings"
	"testing"
)

// TestJournalBuildCardStatesTheUndigestedCount: the build card names how many
// days have messages and no digest, and points at what the calendar shows for
// them.
func TestJournalBuildCardStatesTheUndigestedCount(t *testing.T) {
	srv, st := newPipelineServer(t)
	seedJournalDays(t, st, "Harper", []string{
		"2026-01-01", "2026-01-02", "2026-01-03", "2026-01-04",
	})
	putDigest(t, st, "2026-01-01", "Digested.", "quiet", "")

	body := get(t, srv, "/settings/journal").Body.String()
	if !strings.Contains(body, "1 of 4 days (25%)") {
		t.Error("build card missing the digest-coverage figure")
	}
	if !strings.Contains(body, "3 days have messages but no digest") {
		t.Error("build card should state the undigested-day count, not only the percentage")
	}
	if !strings.Contains(body, "not analysed yet") {
		t.Error("the count should name what those days look like on the calendar")
	}
}

// TestJournalBuildCardHidesTheGapWhenClosed: at full coverage the line is absent
// rather than reading "0 days have messages but no digest", which would be a
// standing reminder of a problem that no longer exists.
func TestJournalBuildCardHidesTheGapWhenClosed(t *testing.T) {
	srv, st := newPipelineServer(t)
	seedJournalDays(t, st, "Harper", []string{"2026-01-01", "2026-01-02"})
	putDigest(t, st, "2026-01-01", "Digested.", "quiet", "")
	putDigest(t, st, "2026-01-02", "Digested.", "upbeat", "")

	body := get(t, srv, "/settings/journal").Body.String()
	if strings.Contains(body, "have messages but no digest") {
		t.Error("the gap line should not render when every day is digested")
	}
}

// TestJournalPageStillCarriesNoCoverageFigure re-pins REQ-0016-017 against this
// change specifically. Surfacing the gap is exactly the kind of useful number
// that has repeatedly been added to /journal; it goes on Settings, and the
// calendar communicates the same fact by marking the days instead.
func TestJournalPageStillCarriesNoCoverageFigure(t *testing.T) {
	srv, st := newPipelineServer(t)
	seedJournalDays(t, st, "Harper", []string{"2026-01-01", "2026-01-02", "2026-01-03"})
	putDigest(t, st, "2026-01-01", "Digested.", "quiet", "")

	body := get(t, srv, "/journal?year=2026&month=1").Body.String()
	for _, forbidden := range []string{
		"have messages but no digest", "Digest coverage", "Journal build", "Build journal",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("/journal renders pipeline status %q — REQ-0016-017 forbids it", forbidden)
		}
	}
	// The same fact, said the way a reading surface is allowed to say it.
	if !strings.Contains(body, "cal-day--unanalyzed") {
		t.Error("/journal should mark the undigested days rather than counting them")
	}
}
