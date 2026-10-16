package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

// tieConversation writes six messages whose timestamps collide on purpose: one
// at 09:00, four sharing 09:01, one at 09:02. Any page size from 1 to 4 drops a
// page seam inside the tied run, so a cursor that compares only timestamps
// would drop or repeat a row there. Insertion order is rowid order, which is
// what the keyset's id tie-breaker resolves to.
func tieConversation(t *testing.T, st *Store, name string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.Signal, name)
	if err != nil {
		t.Fatal(err)
	}
	ts := []string{
		"2022-03-01 09:00:00",
		"2022-03-01 09:01:00",
		"2022-03-01 09:01:00",
		"2022-03-01 09:01:00",
		"2022-03-01 09:01:00",
		"2022-03-01 09:02:00",
	}
	msgs := make([]signal.Message, 0, len(ts))
	for i, raw := range ts {
		msgs = append(msgs, msg(name, raw, name, fmt.Sprintf("m%d", i), nil, nil))
	}
	if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal, msgs); err != nil {
		t.Fatal(err)
	}
	return id
}

// walkPages drains a conversation one page at a time and returns the bodies in
// visit order. It refuses to spin: a comparator that fails to advance the cursor
// fails the test instead of hanging it.
func walkPages(t *testing.T, st *Store, convID int64, limit int, desc bool, maxPages int) []string {
	t.Helper()
	ctx := context.Background()
	var bodies []string
	var cursorTS, cursorID int64
	for page := 0; ; page++ {
		if page > maxPages {
			t.Fatalf("desc=%v limit=%d: no progress after %d pages, %d rows so far (cursor %d/%d)",
				desc, limit, maxPages, len(bodies), cursorTS, cursorID)
		}
		got, err := st.GetMessages(ctx, convID, cursorTS, cursorID, limit, desc)
		if err != nil {
			t.Fatalf("GetMessages(desc=%v, limit=%d) page %d: %v", desc, limit, page, err)
		}
		if len(got.Messages) == 0 && got.HasMore {
			t.Fatalf("desc=%v limit=%d page %d: empty page with HasMore=true", desc, limit, page)
		}
		for _, m := range got.Messages {
			bodies = append(bodies, m.Body)
		}
		if !got.HasMore {
			return bodies
		}
		if got.NextTSUnix == 0 || got.NextID == 0 {
			t.Fatalf("desc=%v limit=%d page %d: HasMore=true but zero cursor", desc, limit, page)
		}
		cursorTS, cursorID = got.NextTSUnix, got.NextID
	}
}

// TestGetMessagesTimestampTiesAcrossSeam pins the contract the July 2026 quality
// review asked for (issue #58): with several messages sharing ts_unix, walking a
// whole conversation with the page seam landing inside the tied run yields every
// message exactly once, in (ts_unix, id) order, in both directions.
func TestGetMessagesTimestampTiesAcrossSeam(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	const name = "Tied Seam"
	id := tieConversation(t, st, name)

	ascending := []string{"m0", "m1", "m2", "m3", "m4", "m5"}

	// Ties resolve by rowid, which for a replaced conversation is ingestion
	// order. That is the comparator's tie-breaker, so state it explicitly
	// against the database rather than assuming it.
	rs, err := st.DB().QueryContext(ctx,
		`SELECT body FROM messages WHERE conversation_id = ? ORDER BY ts_unix, id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rs.Close() }()
	var dbOrder []string
	for rs.Next() {
		var body string
		if err := rs.Scan(&body); err != nil {
			t.Fatal(err)
		}
		dbOrder = append(dbOrder, body)
	}
	if err := rs.Err(); err != nil {
		t.Fatal(err)
	}
	if !slicesEqual(dbOrder, ascending) {
		t.Fatalf("(ts_unix, id) order = %v, want %v", dbOrder, ascending)
	}
	var ties int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM (SELECT ts_unix FROM messages WHERE conversation_id = ? GROUP BY ts_unix HAVING count(*) > 1)`, id).
		Scan(&ties); err != nil {
		t.Fatal(err)
	}
	if ties != 1 {
		t.Fatalf("fixture has %d tied timestamps, want 1", ties)
	}

	for _, limit := range []int{1, 2, 3, 4, 5} {
		if got := walkPages(t, st, id, limit, false, len(ascending)); !slicesEqual(got, ascending) {
			t.Errorf("ascending limit=%d walk = %v, want %v", limit, got, ascending)
		}
		want := reverseStrings(ascending)
		if got := walkPages(t, st, id, limit, true, len(ascending)); !slicesEqual(got, want) {
			t.Errorf("descending limit=%d walk = %v, want %v", limit, got, want)
		}
	}
}

// TestGetMessagesSeamCursorExcludesTiedRow checks the seam itself: the cursor is
// the last row of the previous page, so the next page must start strictly after
// it even when that row sits inside a run of tied timestamps.
func TestGetMessagesSeamCursorExcludesTiedRow(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id := tieConversation(t, st, "Seam Strict")

	first, err := st.GetMessages(ctx, id, 0, 0, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bodiesOfView(first.Messages), []string{"m0", "m1"}; !slicesEqual(got, want) {
		t.Fatalf("first page = %v, want %v", got, want)
	}
	if !first.HasMore {
		t.Fatal("first page HasMore = false, want true")
	}

	second, err := st.GetMessages(ctx, id, first.NextTSUnix, first.NextID, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bodiesOfView(second.Messages), []string{"m2", "m3"}; !slicesEqual(got, want) {
		t.Errorf("page after a tied cursor = %v, want %v (the cursor row must not repeat)", got, want)
	}

	// The same seam walked backwards: the newest two rows, then the two older
	// than them, which lands inside the tied run again.
	firstDesc, err := st.GetMessages(ctx, id, 0, 0, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bodiesOfView(firstDesc.Messages), []string{"m5", "m4"}; !slicesEqual(got, want) {
		t.Fatalf("first descending page = %v, want %v", got, want)
	}
	if !firstDesc.HasMore {
		t.Fatal("first descending page HasMore = false, want true")
	}
	secondDesc, err := st.GetMessages(ctx, id, firstDesc.NextTSUnix, firstDesc.NextID, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bodiesOfView(secondDesc.Messages), []string{"m3", "m2"}; !slicesEqual(got, want) {
		t.Errorf("descending page after a tied cursor = %v, want %v (the cursor row must not repeat)", got, want)
	}

	// A cursor is positional: handed the last row of an ascending page, the
	// descending walk continues strictly before that row rather than restarting
	// at the newest message (what a sort toggle mid-scroll does).
	mixed, err := st.GetMessages(ctx, id, first.NextTSUnix, first.NextID, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := bodiesOfView(mixed.Messages), []string{"m0"}; !slicesEqual(got, want) {
		t.Errorf("descending from an ascending cursor = %v, want %v", got, want)
	}
	if mixed.HasMore {
		t.Errorf("descending from an ascending cursor: HasMore = true, want false (m0 is the oldest row)")
	}
}

// TestGetMessagesEmptyPages covers the ends of a walk: a cursor past the last
// row, a cursor before the first, and a conversation with no messages at all
// must all answer empty with HasMore=false, so an infinite-scroll caller stops
// instead of restarting from the top of the walk.
func TestGetMessagesEmptyPages(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id := tieConversation(t, st, "Empty Ends")

	newestTS, newestID := edgeRow(t, st, id, "DESC")
	oldestTS, oldestID := edgeRow(t, st, id, "ASC")

	for _, tc := range []struct {
		name               string
		cursorTS, cursorID int64
		desc               bool
	}{
		{"ascending past the newest row", newestTS, newestID, false},
		{"descending before the oldest row", oldestTS, oldestID, true},
	} {
		page, err := st.GetMessages(ctx, id, tc.cursorTS, tc.cursorID, 10, tc.desc)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(page.Messages) != 0 {
			t.Errorf("%s: %d messages, want 0", tc.name, len(page.Messages))
		}
		if page.HasMore {
			t.Errorf("%s: HasMore = true on an empty page, want false", tc.name)
		}
		if page.NextTSUnix != 0 || page.NextID != 0 {
			t.Errorf("%s: cursor = %d/%d on an empty page, want 0/0", tc.name, page.NextTSUnix, page.NextID)
		}
	}

	empty, err := st.UpsertConversation(ctx, source.Signal, "No Messages")
	if err != nil {
		t.Fatal(err)
	}
	for _, desc := range []bool{false, true} {
		page, err := st.GetMessages(ctx, empty, 0, 0, 10, desc)
		if err != nil {
			t.Fatalf("empty conversation desc=%v: %v", desc, err)
		}
		if len(page.Messages) != 0 || page.HasMore {
			t.Errorf("empty conversation desc=%v: %d messages, HasMore=%v; want none", desc, len(page.Messages), page.HasMore)
		}
	}
}

func bodiesOfView(msgs []MessageView) []string {
	bodies := make([]string, len(msgs))
	for i, m := range msgs {
		bodies[i] = m.Body
	}
	return bodies
}

func reverseStrings(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[len(in)-1-i] = s
	}
	return out
}

// edgeRow returns the (ts_unix, id) of the first row in the given SQL order:
// "DESC" is the newest row, "ASC" the oldest.
func edgeRow(t *testing.T, st *Store, convID int64, order string) (int64, int64) {
	t.Helper()
	var tsUnix, id int64
	err := st.DB().QueryRowContext(context.Background(),
		`SELECT ts_unix, id FROM messages WHERE conversation_id = ? ORDER BY ts_unix `+order+`, id `+order+` LIMIT 1`, convID).
		Scan(&tsUnix, &id)
	if err != nil {
		t.Fatalf("edge row (%s): %v", order, err)
	}
	return tsUnix, id
}
