package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// The transcript decides day separators and consecutive-sender grouping by
// walking its rows, so a page appended at an HTMX scroll seam used to forget
// the row above it (#38): the seam re-emitted a separator and re-showed a
// sender the reader had already seen. These tests walk real scroll pages —
// following each page's own sentinel, the way the browser does — and read the
// seam out of the rendered HTML.

// seamTotal is one page plus a remainder (pageSize is 50), so an ascending walk
// crosses exactly one page boundary, between rows 49 and 50.
const seamTotal = 60

const (
	seamAlice = "seamsender-alice"
	seamBob   = "seamsender-bob"
)

// seamConversation writes seamTotal messages a minute apart, with the second
// calendar day beginning at dayChangeRow and the sender changing at
// senderChangeRow, so a caller can place either boundary on the seam.
func seamConversation(t *testing.T, st *store.Store, name string, dayChangeRow, senderChangeRow int) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.Signal, name)
	if err != nil {
		t.Fatalf("upsert conversation: %v", err)
	}
	base := time.Date(2022, 6, 1, 9, 0, 0, 0, time.UTC)
	msgs := make([]signal.Message, 0, seamTotal)
	for i := 0; i < seamTotal; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		if i >= dayChangeRow {
			at = at.Add(24 * time.Hour)
		}
		sender := seamAlice
		if i >= senderChangeRow {
			sender = seamBob
		}
		msgs = append(msgs, signal.Message{
			Conversation: name, Timestamp: at, TimestampRaw: at.Format(signal.TimestampLayout),
			Sender: sender, Body: fmt.Sprintf("seam-%02d", i),
		})
	}
	if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal, msgs); err != nil {
		t.Fatalf("replace messages: %v", err)
	}
	return id
}

// scrollSeam walks the transcript from firstURL by following each page's own
// load-more sentinel, and returns the rendered pages. A cursor that never
// advances fails instead of spinning.
func scrollSeam(t *testing.T, srv *Server, firstURL string, maxPages int) []string {
	t.Helper()
	var pages []string
	next := firstURL
	for page := 0; next != ""; page++ {
		if page >= maxPages {
			t.Fatalf("scroll never ended within %d pages: %d fetched", maxPages, len(pages))
		}
		rec := getPartial(t, srv, next)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", next, rec.Code)
		}
		body := rec.Body.String()
		pages = append(pages, body)
		if m := loadMoreRE.FindStringSubmatch(body); m != nil {
			next = m[1]
		} else {
			next = ""
		}
	}
	return pages
}

func sentinelURL(t *testing.T, body string) string {
	t.Helper()
	m := loadMoreRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("page has no load-more sentinel")
	}
	return m[1]
}

// dropGroupingCursor strips the grouping half of a scroll cursor, leaving the
// keyset params: the request the transcript made before #38.
func dropGroupingCursor(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse sentinel %q: %v", rawURL, err)
	}
	q := u.Query()
	q.Del("prev_date")
	q.Del("prev_sender")
	u.RawQuery = q.Encode()
	return u.String()
}

// separatorOpensPage reports whether the page emits a day separator above its
// first message row.
func separatorOpensPage(body string) bool {
	sep := strings.Index(body, `class="day-sep"`)
	row := strings.Index(body, `<div class="msg-row`)
	return sep >= 0 && (row < 0 || sep < row)
}

// firstRowGrouped reports whether the page's first message row suppresses its
// sender heading.
func firstRowGrouped(body string) bool {
	row := strings.Index(body, `<div class="msg-row`)
	if row < 0 {
		return false
	}
	end := strings.Index(body[row:], ">")
	if end < 0 {
		return false
	}
	return strings.Contains(body[row:row+end], "msg-row-grouped")
}

// anchorOffsets returns where each message row's anchor starts in the page.
func anchorOffsets(body string) []int {
	var offsets []int
	for _, m := range messageAnchorRE.FindAllStringIndex(body, -1) {
		offsets = append(offsets, m[0])
	}
	return offsets
}

// TestSeamContinuesGroupingAscending is the bug as reported: the first row of an
// appended page shares the previous page's day and sender, so it must not open
// a separator or repeat the name, while the real day change further down the
// page still gets one.
func TestSeamContinuesGroupingAscending(t *testing.T) {
	srv, st, _ := newTestServer(t)
	const name = "SeamAscending"
	// Day two begins at row 56 (inside page two); the sender changes at row 54,
	// so one run of a single sender spans the seam at rows 49/50.
	id := seamConversation(t, st, name, 56, 54)

	pages := scrollSeam(t, srv, "/c/"+itoa(id)+"/messages?sort=asc", 4)
	if len(pages) != 2 {
		t.Fatalf("pages = %d, want 2", len(pages))
	}
	first := pages[0]
	if !separatorOpensPage(first) {
		t.Errorf("first page lost its opening day separator")
	}
	if !strings.Contains(first, "msg-row-grouped") {
		t.Errorf("first page shows no sender grouping at all, so the seam assertion would be vacuous")
	}

	second := pages[1]
	if separatorOpensPage(second) {
		t.Errorf("seam re-emitted a day separator for a row on the same day: %s", head(second))
	}
	if !firstRowGrouped(second) {
		t.Errorf("seam re-showed the sender name the previous page already showed")
	}
	// The genuine day change inside page two (row 56) still separates, and it
	// sits where it belongs: after row 55, before row 56.
	if n := strings.Count(second, `class="day-sep"`); n != 1 {
		t.Fatalf("page two day separators = %d, want 1", n)
	}
	offsets := anchorOffsets(second)
	if len(offsets) != seamTotal-pageSize {
		t.Fatalf("page two rows = %d, want %d", len(offsets), seamTotal-pageSize)
	}
	sep := strings.Index(second, `class="day-sep"`)
	if sep < offsets[5] || sep > offsets[6] {
		t.Errorf("day separator landed at %d, want it between row 55 (%d) and row 56 (%d)", sep, offsets[5], offsets[6])
	}
}

// TestSeamCursorCarriesGroupingState pins the mechanism the fix depends on: the
// sentinel hands the next page the day key and sender it ended on, and without
// them that page still resets its grouping (the pre-fix request shape).
func TestSeamCursorCarriesGroupingState(t *testing.T) {
	srv, st, _ := newTestServer(t)
	const name = "SeamCursor"
	id := seamConversation(t, st, name, 56, 54)
	dayOne := time.Date(2022, 6, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")

	pages := scrollSeam(t, srv, "/c/"+itoa(id)+"/messages?sort=asc", 3)
	if len(pages) != 2 {
		t.Fatalf("pages = %d, want 2", len(pages))
	}
	next, err := url.Parse(sentinelURL(t, pages[0]))
	if err != nil {
		t.Fatalf("parse sentinel: %v", err)
	}
	if got := next.Query().Get("prev_date"); got != dayOne {
		t.Errorf("sentinel prev_date = %q, want %q", got, dayOne)
	}
	if got := next.Query().Get("prev_sender"); got != seamAlice {
		t.Errorf("sentinel prev_sender = %q, want %q", got, seamAlice)
	}

	rec := getPartial(t, srv, dropGroupingCursor(t, next.String()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !separatorOpensPage(rec.Body.String()) {
		t.Errorf("a cursor with no grouping state opened no separator: the fix is doing nothing")
	}
	if firstRowGrouped(rec.Body.String()) {
		t.Errorf("a cursor with no grouping state grouped its first row: prev_* params are ignored")
	}
}

// TestSeamDayChangeAtSeamStillSeparates covers the other side of the seam: when
// the appended page's first row really is a new day, threading the previous
// page's state must not swallow the separator.
func TestSeamDayChangeAtSeamStillSeparates(t *testing.T) {
	srv, st, _ := newTestServer(t)
	const name = "SeamDayChange"
	// The new day begins exactly at the seam, with the sender unchanged.
	id := seamConversation(t, st, name, 50, seamTotal)

	pages := scrollSeam(t, srv, "/c/"+itoa(id)+"/messages?sort=asc", 3)
	if len(pages) != 2 {
		t.Fatalf("pages = %d, want 2", len(pages))
	}
	if n := strings.Count(pages[0], `class="day-sep"`); n != 1 {
		t.Errorf("first page day separators = %d, want 1", n)
	}
	if !separatorOpensPage(pages[1]) {
		t.Errorf("day change at the seam lost its separator")
	}
	if firstRowGrouped(pages[1]) {
		t.Errorf("a new day opened with its sender grouped away; a new day always re-labels its sender")
	}
}

// TestSeamContinuesGroupingDescending covers the newest-first default, where
// scrolling walks older: the seam sits between two same-day rows, so page two
// opens no separator at all and continues the sender run it started mid-run.
func TestSeamContinuesGroupingDescending(t *testing.T) {
	srv, st, _ := newTestServer(t)
	const name = "SeamDescending"
	// Day two holds only the newest rows, so it is entirely on page one.
	id := seamConversation(t, st, name, 56, 54)

	pages := scrollSeam(t, srv, "/c/"+itoa(id)+"/messages?sort=desc", 4)
	if len(pages) != 2 {
		t.Fatalf("pages = %d, want 2", len(pages))
	}
	if n := strings.Count(pages[0], `class="day-sep"`); n != 2 {
		t.Errorf("first page day separators = %d, want 2 (day two opens the walk)", n)
	}
	second := pages[1]
	if strings.Contains(second, `class="day-sep"`) {
		t.Errorf("descending seam emitted a separator inside a single day: %s", head(second))
	}
	if !firstRowGrouped(second) {
		t.Errorf("descending seam re-showed a sender already shown on the previous page")
	}
}

// TestPrevCursorParamParsing covers the cursor's two halves as request input:
// anything but a day key reads as "no previous day", and a sender that round
// trips through the URL stays bounded and single-line.
func TestPrevCursorParamParsing(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"2026-10-16", "2026-10-16"},
		{"", ""},
		{"2026-13-99", "2026-13-99"},
		{"2026/10/16", ""},
		{"2026-10-1", ""},
		{"2026-10-16<script>", ""},
		{"<script>alert(1)</script>", ""},
	} {
		if got := parsePrevDate(tc.in); got != tc.want {
			t.Errorf("parsePrevDate(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	if got := parsePrevSender("alice@example.com"); got != "alice@example.com" {
		t.Errorf("parsePrevSender plain = %q", got)
	}
	if got := parsePrevSender("bob\nSet-Cookie: x"); got != "bobSet-Cookie: x" {
		t.Errorf("parsePrevSender control chars = %q, want them stripped", got)
	}
	if got := parsePrevSender(strings.Repeat("é", prevSenderMaxRunes*3)); len([]rune(got)) != prevSenderMaxRunes {
		t.Errorf("parsePrevSender did not cap a runaway sender: %d runes", len([]rune(got)))
	}
	if got := parsePrevSender(strings.Repeat("\xff", 8)); got != strings.Repeat("\uFFFD", 8) {
		t.Errorf("parsePrevSender invalid utf8 = %q", got)
	}
}

func head(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
