package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

// seedOnThisDay builds a small archive spanning several years on the SAME
// calendar month/day (07-25) plus decoys on adjacent days, which is the exact
// shape Home's "On this day" card queries (#239).
func seedOnThisDay(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	for _, c := range []struct {
		conv string
		msgs []signal.Message
	}{
		{"Harper", []signal.Message{
			// 2019: two candidates — the longer body must win, deterministically.
			msg("Harper", "2019-07-25 09:00:00", "Harper", "short one", nil, nil),
			msg("Harper", "2019-07-25 10:00:00", "Harper", "the cabin is booked, three weeks off the grid", nil, nil),
			// Adjacent-day decoys: must never be picked for 07-25.
			msg("Harper", "2019-07-24 23:59:59", "Harper", "a much much much longer message on the day before", nil, nil),
			msg("Harper", "2019-07-26 00:00:00", "Harper", "a much much much longer message on the day after", nil, nil),
		}},
		{"Wren", []signal.Message{
			msg("Wren", "2021-07-25 12:00:00", "Me", "lunch?", nil, nil),
		}},
		{"Secret", []signal.Message{
			// Same day as Wren but a longer body: only excluded-ness keeps it out.
			msg("Secret", "2021-07-25 13:00:00", "Secret", "this is a considerably longer body than lunch", nil, nil),
		}},
	} {
		id, err := st.UpsertConversation(ctx, source.Signal, c.conv)
		if err != nil {
			t.Fatalf("upsert %s: %v", c.conv, err)
		}
		if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal, c.msgs); err != nil {
			t.Fatalf("seed %s: %v", c.conv, err)
		}
	}
}

func TestMessageYearRange(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// An empty archive must report (0, 0) rather than 1970 — the caller uses
	// zero to mean "nothing to resurface" and skips the query entirely.
	if first, last, err := st.MessageYearRange(ctx); err != nil || first != 0 || last != 0 {
		t.Fatalf("empty archive: (%d, %d, %v), want (0, 0, nil)", first, last, err)
	}

	seedOnThisDay(t, st)
	first, last, err := st.MessageYearRange(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first != 2019 || last != 2021 {
		t.Errorf("year range = (%d, %d), want (2019, 2021)", first, last)
	}
}

func TestOnThisDayCandidatesSelectsLongestBodyPerDay(t *testing.T) {
	st := newTestStore(t)
	seedOnThisDay(t, st)
	ctx := context.Background()

	days := []string{"2021-07-25", "2019-07-25"}
	rows, err := st.OnThisDayCandidates(ctx, days, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want one per day", len(rows))
	}
	// Arms come back in the order requested, so the caller's newest-first day
	// list yields newest-first rows — the card takes rows[0].
	if rows[0].Day != "2021-07-25" || rows[1].Day != "2019-07-25" {
		t.Errorf("arm order not preserved: %q then %q", rows[0].Day, rows[1].Day)
	}
	// Longest body wins within a day.
	if rows[1].Body != "the cabin is booked, three weeks off the grid" {
		t.Errorf("2019 pick = %q, want the longer body", rows[1].Body)
	}
	// Adjacent days must not leak in: the 07-24/07-26 decoys are longer still.
	for _, r := range rows {
		if r.TS[:10] != r.Day {
			t.Errorf("row from %s leaked into the %s arm", r.TS[:10], r.Day)
		}
	}
	// The archive owner is flagged so the card can render "You" instead of "Me".
	// Reach the owner's message by excluding the longer-bodied Secret row.
	owner, err := st.OnThisDayCandidates(ctx, []string{"2021-07-25"}, 1, 0, []string{"Secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(owner) != 1 || !owner[0].IsOwner {
		t.Errorf("owner-sent pick = %+v, want IsOwner set", owner)
	}
}

// The card must not reshuffle between renders — #239 requires the selection to
// be deterministic for a given day.
func TestOnThisDayCandidatesDeterministic(t *testing.T) {
	st := newTestStore(t)
	seedOnThisDay(t, st)
	ctx := context.Background()

	days := []string{"2021-07-25", "2019-07-25"}
	first, err := st.OnThisDayCandidates(ctx, days, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		again, err := st.OnThisDayCandidates(ctx, days, 1, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != len(first) {
			t.Fatalf("run %d returned %d rows, want %d", i, len(again), len(first))
		}
		for j := range first {
			if again[j].ID != first[j].ID {
				t.Fatalf("run %d row %d picked id %d, want %d — selection is not deterministic",
					i, j, again[j].ID, first[j].ID)
			}
		}
	}
}

func TestOnThisDayCandidatesHonoursExclude(t *testing.T) {
	st := newTestStore(t)
	seedOnThisDay(t, st)
	ctx := context.Background()

	// Without the denylist the longer "Secret" body wins the 2021 arm.
	rows, err := st.OnThisDayCandidates(ctx, []string{"2021-07-25"}, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ConversationName != "Secret" {
		t.Fatalf("baseline pick = %+v, want the Secret conversation", rows)
	}
	// With it, the conversation is invisible to Home entirely — a conversation
	// hidden from the journal must not resurface on the landing page.
	rows, err = st.OnThisDayCandidates(ctx, []string{"2021-07-25"}, 1, 0, []string{"Secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ConversationName != "Wren" {
		t.Fatalf("excluded pick = %+v, want Wren", rows)
	}
}

func TestOnThisDayCandidatesSkipsSystemAndBlank(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.Signal, "Quiet")
	if err != nil {
		t.Fatal(err)
	}
	blank := msg("Quiet", "2020-07-25 08:00:00", "Quiet", "   ", nil, nil)
	sys := msg("Quiet", "2020-07-25 09:00:00", signal.SystemSender, "Quiet shared 2 photos", nil, nil)
	sys.IsSystem = true
	if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal,
		[]signal.Message{blank, sys}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.OnThisDayCandidates(ctx, []string{"2020-07-25"}, 5, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows, want none — system events and blank bodies must not resurface: %+v", len(rows), rows)
	}
}

// Feb 29 needs no special case: a prior non-leap year's one-day window simply
// matches nothing and the card is omitted rather than erroring or fabricating.
func TestOnThisDayCandidatesLeapDay(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.Signal, "Leap")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal,
		[]signal.Message{msg("Leap", "2020-02-29 10:00:00", "Leap", "leap day message", nil, nil)}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.OnThisDayCandidates(ctx, []string{"2023-02-29", "2021-02-29", "2020-02-29"}, 1, 0, nil)
	if err != nil {
		t.Fatalf("a non-leap Feb 29 must not error: %v", err)
	}
	if len(rows) != 1 || rows[0].Day != "2020-02-29" {
		t.Errorf("got %+v, want only the real 2020-02-29 row", rows)
	}
}

func TestOnThisDayCandidatesEmptyInputs(t *testing.T) {
	st := newTestStore(t)
	seedOnThisDay(t, st)
	ctx := context.Background()
	for name, call := range map[string]func() ([]OnThisDayMessage, error){
		"no days": func() ([]OnThisDayMessage, error) { return st.OnThisDayCandidates(ctx, nil, 1, 0, nil) },
		"zero perDay": func() ([]OnThisDayMessage, error) {
			return st.OnThisDayCandidates(ctx, []string{"2019-07-25"}, 0, 0, nil)
		},
		"malformed day": func() ([]OnThisDayMessage, error) {
			return st.OnThisDayCandidates(ctx, []string{"not-a-day"}, 1, 0, nil)
		},
	} {
		rows, err := call()
		if err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
		}
		if len(rows) != 0 {
			t.Errorf("%s: got %d rows, want none", name, len(rows))
		}
	}
}

func TestRecentConversations(t *testing.T) {
	st := newTestStore(t)
	seedOnThisDay(t, st)
	ctx := context.Background()

	// n <= 0 must not touch the database at all.
	if rows, err := st.RecentConversations(ctx, 0); err != nil || rows != nil {
		t.Fatalf("n=0 returned (%v, %v), want (nil, nil)", rows, err)
	}

	rows, err := st.RecentConversations(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want the LIMIT of 2", len(rows))
	}
	// Newest activity first: Secret (2021-07-25 13:00) then Wren (12:00).
	if rows[0].Name != "Secret" || rows[1].Name != "Wren" {
		t.Errorf("order = %q, %q; want Secret then Wren (most recent first)", rows[0].Name, rows[1].Name)
	}
	if rows[0].LastTSUnix == 0 {
		t.Error("recent rows should carry the ordering key the relative stamp is derived from")
	}
	if rows[0].LastTS == "" {
		t.Error("recent rows should carry the last-activity timestamp")
	}
}

// TestOnThisDayArgAlignment is the regression guard for the UNION ALL builder:
// each arm appends its own placeholders (day, two window bounds, one per exclude
// name, then the LIMIT), so a mismatch between the generated SQL and the arg
// slice desyncs every arm after the first. Multiple arms AND a multi-name
// exclude list at once is the only shape that surfaces it — a single arm, or an
// empty exclude list, would pass a broken builder.
func TestOnThisDayArgAlignment(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for _, c := range []string{"Keep", "DropA", "DropB"} {
		id, err := st.UpsertConversation(ctx, source.Signal, c)
		if err != nil {
			t.Fatal(err)
		}
		var msgs []signal.Message
		for _, y := range []string{"2019", "2020", "2021"} {
			msgs = append(msgs, msg(c, y+"-07-25 10:00:00", c, c+" said something in "+y, nil, nil))
		}
		if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal, msgs); err != nil {
			t.Fatal(err)
		}
	}
	days := []string{"2021-07-25", "2020-07-25", "2019-07-25"}
	rows, err := st.OnThisDayCandidates(ctx, days, 3, 0, []string{"DropA", "DropB"})
	if err != nil {
		t.Fatalf("multi-arm + multi-exclude query failed: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 (one Keep row per year)", len(rows))
	}
	for i, r := range rows {
		if r.ConversationName != "Keep" {
			t.Errorf("row %d came from %q — the exclude list did not apply to every arm", i, r.ConversationName)
		}
		if r.Day != days[i] {
			t.Errorf("row %d day = %s, want %s — arm order or args desynced", i, r.Day, days[i])
		}
	}
}

// TestOnThisDayCandidatesSkipsEmptyPreview is the regression for the bug this
// query's own shape invites: the ORDER BY ranks on the RAW body length while the
// card renders the PREVIEW. preview() strips quote markers, unwraps markdown
// links and collapses whitespace, so a long reply that is nothing but ">" quote
// lines both WINS the sort and renders as nothing — and SQLite's one-argument
// TRIM strips spaces only, so TRIM(body) <> ” does not catch a newline-only
// body either. The card would show a bare pair of quotation marks and suppress
// the day's real message.
func TestOnThisDayCandidatesSkipsEmptyPreview(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.Signal, "Quoty")
	if err != nil {
		t.Fatal(err)
	}
	quoteOnly := strings.Repeat("> \n", 200) // long, all quote markers
	newlineOnly := strings.Repeat("\n", 300) // long, survives TRIM()
	real := "a real memorable line from that day"
	if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal, []signal.Message{
		msg("Quoty", "2019-07-25 08:00:00", "Quoty", quoteOnly, nil, nil),
		msg("Quoty", "2019-07-25 09:00:00", "Quoty", newlineOnly, nil, nil),
		msg("Quoty", "2019-07-25 10:00:00", "Quoty", real, nil, nil),
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.OnThisDayCandidates(ctx, []string{"2019-07-25"}, 3, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Body == "" {
			t.Errorf("returned a candidate whose preview is empty (id %d) — the card would render a bare quotation", r.ID)
		}
	}
	if len(rows) != 1 || rows[0].Body != real {
		t.Errorf("got %+v, want only the message that actually renders", rows)
	}
}

// TestOnThisDayCandidatesLargeExcludeList: the denylist is bound once in a CTE
// rather than re-emitted per arm. Inlined, a long list across ~20 arms multiplies
// the bound-parameter count past SQLITE_MAX_VARIABLE_NUMBER and 500s the whole
// Home page instead of degrading the card.
func TestOnThisDayCandidatesLargeExcludeList(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.Signal, "Keep")
	if err != nil {
		t.Fatal(err)
	}
	var msgs []signal.Message
	var days []string
	for y := 2006; y <= 2025; y++ { // 20 arms, the web layer's cap
		msgs = append(msgs, msg("Keep", fmt.Sprintf("%d-07-25 10:00:00", y), "Keep",
			fmt.Sprintf("something worth remembering from %d", y), nil, nil))
		days = append(days, fmt.Sprintf("%d-07-25", y))
	}
	if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal, msgs); err != nil {
		t.Fatal(err)
	}
	exclude := make([]string, 60)
	for i := range exclude {
		exclude[i] = fmt.Sprintf("Excluded Conversation %d", i)
	}
	rows, err := st.OnThisDayCandidates(ctx, days, 1, 0, exclude)
	if err != nil {
		t.Fatalf("20 arms x a 60-name denylist must not exhaust the parameter limit: %v", err)
	}
	if len(rows) != len(days) {
		t.Errorf("got %d rows, want %d — the CTE denylist must still filter every arm", len(rows), len(days))
	}
}
