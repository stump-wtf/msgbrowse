package web

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// tieSeamTotal is deliberately one page plus a remainder (pageSize is 50), so an
// ascending walk makes exactly one page boundary.
const tieSeamTotal = 60

// Two runs of messages share a timestamp each, placed so the single page
// boundary falls inside a run in whichever direction the scroll walks: rows
// 46-53 straddle the boundary of an ascending walk (page one ends at row 49,
// page two begins at row 50), and rows 8-15 straddle the same boundary seen from
// the newest message downwards, where page one ends at row 10 and page two
// begins at row 9. A cursor keyed on the timestamp alone would drop or repeat
// rows at those seams.
var tieSeamRuns = [][2]int{{8, 16}, {46, 54}}

var (
	messageAnchorRE = regexp.MustCompile(`id="m(\d+)"`)
	loadMoreRE      = regexp.MustCompile(`hx-get="([^"]+)"`)
)

// tieSeamConversation writes tieSeamTotal messages into a conversation of its
// own, so the walk never mixes with the shared fixture archive.
func tieSeamConversation(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.Signal, name)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2022, 3, 1, 0, 0, 0, 0, time.UTC)
	msgs := make([]signal.Message, 0, tieSeamTotal)
	for i := 0; i < tieSeamTotal; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		for _, run := range tieSeamRuns {
			if i >= run[0] && i < run[1] {
				at = base.Add(time.Duration(run[0]) * time.Minute)
			}
		}
		raw := at.Format(signal.TimestampLayout)
		msgs = append(msgs, signal.Message{
			Conversation: name, Timestamp: at, TimestampRaw: raw,
			Sender: name, Body: fmt.Sprintf("seam-%02d", i),
		})
	}
	if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal, msgs); err != nil {
		t.Fatal(err)
	}
	return id
}

// keysetOrder returns the conversation's message ids in (ts_unix, id) order —
// the order the endpoint promises to walk.
func keysetOrder(t *testing.T, st *store.Store, convID int64) []int64 {
	t.Helper()
	rs, err := st.DB().QueryContext(context.Background(),
		`SELECT id FROM messages WHERE conversation_id = ? ORDER BY ts_unix, id`, convID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()
	var ids []int64
	for rs.Next() {
		var id int64
		if err := rs.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// tiedIDs returns the ids whose timestamp is shared with another message in the
// conversation — the rows where the keyset must fall back to its id tie-breaker.
func tiedIDs(t *testing.T, st *store.Store, convID int64) map[int64]bool {
	t.Helper()
	rs, err := st.DB().QueryContext(context.Background(),
		`SELECT id FROM messages WHERE conversation_id = ?
		   AND ts_unix IN (SELECT ts_unix FROM messages WHERE conversation_id = ?
		                   GROUP BY ts_unix HAVING count(*) > 1)`, convID, convID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()
	tied := map[int64]bool{}
	for rs.Next() {
		var id int64
		if err := rs.Scan(&id); err != nil {
			t.Fatal(err)
		}
		tied[id] = true
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	return tied
}

// scrollMessages follows the load-more sentinel from firstURL until the pages
// stop, returning the ids rendered per page. It refuses to spin: a cursor that
// never advances fails the test instead of looping it.
func scrollMessages(t *testing.T, srv *Server, firstURL string, maxPages int) [][]int64 {
	t.Helper()
	var pages [][]int64
	url := firstURL
	for page := 0; url != ""; page++ {
		if page >= maxPages {
			t.Fatalf("no last page within %d requests: %v", maxPages, pages)
		}
		rec := getPartial(t, srv, url)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", url, rec.Code)
		}
		body := rec.Body.String()
		var ids []int64
		for _, m := range messageAnchorRE.FindAllStringSubmatch(body, -1) {
			id, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil {
				t.Fatalf("%s: bad message anchor %q: %v", url, m[1], err)
			}
			ids = append(ids, id)
		}
		pages = append(pages, ids)
		if next := loadMoreRE.FindStringSubmatch(body); next != nil {
			url = next[1]
		} else {
			url = ""
		}
	}
	return pages
}

func flatten(pages [][]int64) []int64 {
	var out []int64
	for _, p := range pages {
		out = append(out, p...)
	}
	return out
}

func equalIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func reverseIDs(in []int64) []int64 {
	out := make([]int64, len(in))
	for i, id := range in {
		out[len(in)-1-i] = id
	}
	return out
}

// TestMessagesEndpointTiedTimestampsAcrossSeam is the endpoint half of issue #58:
// scrolling a conversation whose tied timestamps straddle the page boundary must
// render every message exactly once, in keyset order, in both directions, with
// the sentinel carrying the direction-cursor params.
func TestMessagesEndpointTiedTimestampsAcrossSeam(t *testing.T) {
	srv, st, _ := newTestServer(t)
	const name = "Seam Scroll"
	id := tieSeamConversation(t, st, name)
	base := fmt.Sprintf("/c/%d/messages", id)

	want := keysetOrder(t, st, id)
	if len(want) != tieSeamTotal {
		t.Fatalf("fixture has %d messages, want %d", len(want), tieSeamTotal)
	}

	tied := tiedIDs(t, st, id)
	if len(tied) != 16 {
		t.Fatalf("fixture has %d messages sharing a timestamp, want 16", len(tied))
	}

	for _, tc := range []struct {
		sort         string
		wantParams   []string
		unwantParams []string
		wantOrder    []int64
	}{
		{"asc", []string{"after_ts=", "after_id="}, []string{"before_ts=", "before_id="}, want},
		{"desc", []string{"before_ts=", "before_id="}, []string{"after_ts=", "after_id="}, reverseIDs(want)},
	} {
		pages := scrollMessages(t, srv, base+"?sort="+tc.sort, 8)
		if got := flatten(pages); !equalIDs(got, tc.wantOrder) {
			t.Errorf("%s scroll rendered %d ids, want the %d messages in keyset order (got %v)",
				tc.sort, len(got), len(tc.wantOrder), got)
		}
		if len(pages) != 2 || len(pages[0]) != 50 || len(pages[1]) != 10 {
			t.Errorf("%s scroll page sizes = %v, want [50 10]", tc.sort, pageSizes(pages))
		}

		// The seam must land inside a run of tied timestamps, otherwise this
		// test would pass without ever exercising the tie-breaker.
		if len(pages) == 2 {
			before, after := pages[0][len(pages[0])-1], pages[1][0]
			if !tied[before] || !tied[after] {
				t.Errorf("%s scroll seam is not inside a tied-timestamp run (rows %d → %d)", tc.sort, before, after)
			}
			if before == after {
				t.Errorf("%s scroll repeated the cursor row %d at the seam", tc.sort, after)
			}
		}

		// The sentinel followed for this direction carries that direction's
		// cursor params, never the opposite direction's.
		rec := getPartial(t, srv, base+"?sort="+tc.sort)
		next := loadMoreRE.FindStringSubmatch(rec.Body.String())
		if next == nil {
			t.Fatalf("%s first page rendered no load-more sentinel", tc.sort)
		}
		for _, param := range tc.wantParams {
			if !contains(next[1], param) {
				t.Errorf("%s sentinel URL %q missing %s", tc.sort, next[1], param)
			}
		}
		for _, param := range tc.unwantParams {
			if contains(next[1], param) {
				t.Errorf("%s sentinel URL %q carries %s, which continues the other direction", tc.sort, next[1], param)
			}
		}
	}
}

// TestMessagesEndpointEmptyPage covers the scroll reaching the end of a
// conversation: a cursor past the newest row (ascending) or before the oldest
// (descending) renders no rows and no sentinel, so htmx stops asking.
func TestMessagesEndpointEmptyPage(t *testing.T) {
	srv, st, _ := newTestServer(t)
	id := tieSeamConversation(t, st, "Seam Empty")
	ctx := context.Background()

	var newestTS, newestID, oldestTS, oldestID int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT ts_unix, id FROM messages WHERE conversation_id = ? ORDER BY ts_unix DESC, id DESC LIMIT 1`, id).
		Scan(&newestTS, &newestID); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT ts_unix, id FROM messages WHERE conversation_id = ? ORDER BY ts_unix ASC, id ASC LIMIT 1`, id).
		Scan(&oldestTS, &oldestID); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ name, path string }{
		{"ascending past the newest row", fmt.Sprintf("/c/%d/messages?sort=asc&after_ts=%d&after_id=%d", id, newestTS, newestID)},
		{"descending before the oldest row", fmt.Sprintf("/c/%d/messages?sort=desc&before_ts=%d&before_id=%d", id, oldestTS, oldestID)},
	}
	for _, tc := range cases {
		rec := getPartial(t, srv, tc.path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", tc.name, rec.Code)
		}
		body := rec.Body.String()
		if n := len(messageAnchorRE.FindAllStringSubmatch(body, -1)); n != 0 {
			t.Errorf("%s: rendered %d message rows, want 0", tc.name, n)
		}
		if loadMoreRE.MatchString(body) {
			t.Errorf("%s: rendered a load-more sentinel on an empty page; the scroll would never stop", tc.name)
		}
	}
}

// TestMessagesEndpointLastPageEndsTheScroll pins the last page itself: a short
// final page still renders its rows, reports no further pages, and the first
// page's sentinel carries a cursor that lands exactly on it.
func TestMessagesEndpointLastPageEndsTheScroll(t *testing.T) {
	srv, st, _ := newTestServer(t)
	id := tieSeamConversation(t, st, "Seam Last")
	all := keysetOrder(t, st, id)

	first := getPartial(t, srv, fmt.Sprintf("/c/%d/messages?sort=asc", id))
	if first.Code != http.StatusOK {
		t.Fatalf("first page status = %d", first.Code)
	}
	firstIDs := anchors(t, first.Body.String())
	if len(firstIDs) != 50 {
		t.Fatalf("first page rows = %d, want 50", len(firstIDs))
	}
	next := loadMoreRE.FindStringSubmatch(first.Body.String())
	if next == nil {
		t.Fatal("first page rendered no load-more sentinel")
	}

	last := getPartial(t, srv, next[1])
	if last.Code != http.StatusOK {
		t.Fatalf("last page status = %d", last.Code)
	}
	lastIDs := anchors(t, last.Body.String())
	if len(lastIDs) != tieSeamTotal-50 {
		t.Errorf("last page rows = %d, want %d", len(lastIDs), tieSeamTotal-50)
	}
	if !equalIDs(append(append([]int64{}, firstIDs...), lastIDs...), all) {
		t.Errorf("two ascending pages are not the whole conversation in keyset order")
	}
	if loadMoreRE.MatchString(last.Body.String()) {
		t.Errorf("last page rendered a load-more sentinel; HasMore should be false")
	}
}

func anchors(t *testing.T, body string) []int64 {
	t.Helper()
	var ids []int64
	for _, m := range messageAnchorRE.FindAllStringSubmatch(body, -1) {
		id, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			t.Fatalf("bad message anchor %q: %v", m[1], err)
		}
		ids = append(ids, id)
	}
	return ids
}

func pageSizes(pages [][]int64) []int {
	out := make([]int, len(pages))
	for i, p := range pages {
		out[i] = len(p)
	}
	return out
}
