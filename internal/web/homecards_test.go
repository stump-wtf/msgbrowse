package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// seedDay writes one conversation's messages at the given wall-clock stamps.
func seedDay(t *testing.T, st *store.Store, conv string, msgs ...signal.Message) {
	t.Helper()
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.Signal, conv)
	if err != nil {
		t.Fatalf("upsert %s: %v", conv, err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal, msgs); err != nil {
		t.Fatalf("seed %s: %v", conv, err)
	}
}

func webMsg(conv, ts, sender, body string) signal.Message {
	parsed, _ := time.Parse(signal.TimestampLayout, ts)
	return signal.Message{
		Conversation: conv, Timestamp: parsed, TimestampRaw: ts,
		Sender: sender, Body: body,
	}
}

// newEmptyServer wires a Server to a store with NO ingested archive.
//
// These tests must not use the shared newTestServer: it ingests the committed
// fixture, whose messages span several years, so an "On this day" assertion
// would silently depend on what calendar date the suite runs on — a fixture
// message on today's month/day in a prior year would win the arm and break an
// unrelated expectation. Seeding every case explicitly keeps them hermetic.
func newEmptyServer(t *testing.T) *Server {
	t.Helper()
	srv, _ := newEmptyServerStore(t)
	return srv
}

func newEmptyServerStore(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "empty.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv, err := NewServer(st, &config.Config{DataDir: t.TempDir()},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, st
}

// priorYearSameDay is the seed date the "On this day" card actually probes:
// the SAME month/day, one year earlier. It must NOT be built with
// now.AddDate(-1, 0, 0), which Go normalizes — on 29 February that yields
// 1 March of the prior year, while overviewOnThisDay probes "%04d-02-29", so the
// seeded message lands in no arm at all and the test fails once every four
// years. Formatting the components directly keeps the two in lockstep; on a
// leap day the prior year has no 29 Feb, so the caller skips instead.
func priorYearSameDay(t *testing.T, now time.Time) (string, bool) {
	t.Helper()
	y, mo, d := now.Date()
	if mo == time.February && d == 29 {
		return "", false
	}
	return fmt.Sprintf("%04d-%02d-%02d", y-1, int(mo), d), true
}

// TestRelTimeLabel pins the coarse buckets Home's "Jump back in" stamps use.
// now is in wall-clock space (what wallNow produces), matching ts_unix.
func TestRelTimeLabel(t *testing.T) {
	now, err := time.Parse(signal.TimestampLayout, "2026-07-25 14:00:00")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ ts, want, why string }{
		{"2026-07-25 13:59:30", "just now", "under a minute"},
		{"2026-07-25 13:58:00", "2m", "minutes"},
		{"2026-07-25 11:00:00", "3h", "same calendar day"},
		{"2026-07-25 00:30:00", "13h", "still the same calendar day"},
		{"2026-07-24 23:59:00", "Yesterday", "previous calendar day, even though only ~14h ago"},
		{"2026-07-21 09:00:00", "4d", "within the week"},
		{"2022-10-22 04:17:00", "Oct 22, 2022", "older than a week falls back to an absolute date"},
		// Clock skew / an archive imported from a machine running ahead must not
		// render a negative age.
		{"2026-07-25 14:05:00", "just now", "a future stamp degrades to just now"},
	} {
		ts, perr := time.Parse(signal.TimestampLayout, tc.ts)
		if perr != nil {
			t.Fatalf("parse %s: %v", tc.ts, perr)
		}
		if got := relTimeLabel(ts.Unix(), now); got != tc.want {
			t.Errorf("relTimeLabel(%s) = %q, want %q (%s)", tc.ts, got, tc.want, tc.why)
		}
	}
}

// TestWallNowCancelsZoneOffset is the regression this helper exists for: ts_unix
// is a wall-clock string parsed AS UTC, not a true instant. Subtracting a real
// time.Now().Unix() would report the machine's UTC offset as phantom age — five
// hours in New York — so a message written seconds ago would read "5h".
func TestWallNowCancelsZoneOffset(t *testing.T) {
	zone := time.FixedZone("UTC-5", -5*60*60)
	local := time.Date(2026, 7, 25, 14, 0, 30, 0, zone)

	// The store would have written this message's ts_unix from the same wall
	// clock ("2026-07-25 14:00:00"), parsed as UTC.
	ts, err := time.Parse(signal.TimestampLayout, "2026-07-25 14:00:00")
	if err != nil {
		t.Fatal(err)
	}
	if got := relTimeLabel(ts.Unix(), wallNow(local)); got != "just now" {
		t.Errorf("relTimeLabel with a non-UTC local clock = %q, want %q", got, "just now")
	}
	// And the naive version is what would have been wrong.
	if naive := relTimeLabel(ts.Unix(), local.UTC()); naive == "just now" {
		t.Log("note: naive comparison happened to agree here; wallNow is still required for non-zero offsets")
	}
}

func TestYearsAgoLabel(t *testing.T) {
	for n, want := range map[int]string{1: "1 year ago", 2: "2 years ago", 6: "6 years ago"} {
		if got := yearsAgoLabel(n); got != want {
			t.Errorf("yearsAgoLabel(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestHomeOnThisDayCard: a message on this calendar day in a previous year
// resurfaces, with the year gap, the quote, who said it, and a link onward.
func TestHomeOnThisDayCard(t *testing.T) {
	srv, st := newEmptyServerStore(t)
	now := time.Now()
	lastYear, ok := priorYearSameDay(t, now)
	if !ok {
		t.Skip("29 February has no counterpart in the prior year; the card is correctly omitted")
	}
	seedDay(t, st, "Arne",
		webMsg("Arne", lastYear+" 14:32:07", "Arne", "the cabin is booked, three weeks off the grid"))

	body := get(t, srv, "/").Body.String()
	for _, want := range []string{
		"On this day",
		"1 year ago",
		"the cabin is booked, three weeks off the grid",
		`href="/journal?day=` + lastYear + `"`,
		"Open that day",
	} {
		if !contains(body, want) {
			t.Errorf("home missing %q from the On this day card", want)
		}
	}
}

// TestHomeOnThisDayOmittedWhenNoPriorYear: with activity only in the current
// year there is nothing to resurface, and the card must be absent rather than an
// empty bordered frame.
func TestHomeOnThisDayOmittedWhenNoPriorYear(t *testing.T) {
	srv, st := newEmptyServerStore(t)
	today := time.Now().Format("2006-01-02")
	seedDay(t, st, "Today", webMsg("Today", today+" 09:00:00", "Today", "only this year"))

	body := get(t, srv, "/").Body.String()
	if contains(body, "On this day") {
		t.Error("On this day card rendered with no qualifying prior year")
	}
	if contains(body, "home-card-eyebrow") {
		t.Error("the card's eyebrow rendered without a card")
	}
}

// TestHomeJumpBackIn: the recent-conversation card lists rows with relative
// stamps and links into the transcript.
func TestHomeJumpBackIn(t *testing.T) {
	srv, st := newEmptyServerStore(t)
	now := time.Now()
	seedDay(t, st, "Recent",
		webMsg("Recent", now.Add(-90*time.Minute).Format(signal.TimestampLayout), "Recent", "still around"))

	body := get(t, srv, "/").Body.String()
	if !contains(body, "Jump back in") {
		t.Fatal("home missing the Jump back in card")
	}
	if !contains(body, "jumpback-row") {
		t.Error("Jump back in rendered no conversation rows")
	}
	conv, err := st.GetConversation(context.Background(), "Recent")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	if !contains(body, `href="/c/`+itoa(conv.ID)+`"`) {
		t.Error("Jump back in rows should link into the transcript")
	}
}

// TestHomeCardsSurviveBoostedRender is the trap this design exists to avoid:
// handleIndex's partial branch deliberately skips the sidebar listing, so a card
// fed from base.Conversations would silently empty out on every boosted
// navigation to Home. Both cards must render identically on the boosted path.
func TestHomeCardsSurviveBoostedRender(t *testing.T) {
	srv, st := newEmptyServerStore(t)
	now := time.Now()
	lastYear, ok := priorYearSameDay(t, now)
	if !ok {
		t.Skip("29 February has no counterpart in the prior year; the card is correctly omitted")
	}
	seedDay(t, st, "Arne", webMsg("Arne", lastYear+" 14:32:07", "Arne", "the cabin is booked"))
	seedDay(t, st, "Recent",
		webMsg("Recent", now.Add(-90*time.Minute).Format(signal.TimestampLayout), "Recent", "still around"))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()

	if strings.Contains(strings.ToLower(body), "<!doctype") {
		t.Error("boosted render should be a fragment, not a full document")
	}
	for _, want := range []string{"On this day", "the cabin is booked", "Jump back in", "jumpback-row"} {
		if !contains(body, want) {
			t.Errorf("boosted home missing %q — the cards must not depend on the sidebar listing", want)
		}
	}
}

// TestHomeOnThisDayEscapesBody: the resurfaced excerpt is raw message content
// rendered onto the landing page — the highest-value injection sink on Home.
func TestHomeOnThisDayEscapesBody(t *testing.T) {
	srv, st := newEmptyServerStore(t)
	lastYear, ok := priorYearSameDay(t, time.Now())
	if !ok {
		t.Skip("29 February has no counterpart in the prior year; the card is correctly omitted")
	}
	seedDay(t, st, "Evil",
		webMsg("Evil", lastYear+" 10:00:00", "Evil", `<script>alert(1)</script> and "quoted" & bare`))

	body := get(t, srv, "/").Body.String()
	if contains(body, "<script>alert(1)</script>") {
		t.Error("message body reached the page unescaped")
	}
	if !contains(body, "&lt;script&gt;") {
		t.Error("expected the body to be html/template-escaped into the card")
	}
}

// TestHomeCardsEmptyArchive: on a store with no messages at all the assemblers
// must not error and must not fabricate — which is what keeps the pre-ingest
// empty state on Home free of empty bordered frames.
func TestHomeCardsEmptyArchive(t *testing.T) {
	srv := newEmptyServer(t)
	card, err := srv.overviewOnThisDay(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("empty archive should not error: %v", err)
	}
	if card.Show {
		t.Errorf("empty archive produced a card: %+v", card)
	}
	rows, err := srv.overviewJumpBackIn(context.Background(), jumpBackLimit)
	if err != nil {
		t.Fatalf("empty archive should not error: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("empty archive produced %d recent rows", len(rows))
	}
}
