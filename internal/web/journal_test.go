package web

import (
	"context"
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

// newJournalServer wires a Server over an EMPTY store (no fixture archive) so
// journal-day counts are exactly what the test seeds.
func newJournalServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "journal-web.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{DataDir: t.TempDir()}
	srv, err := NewServer(st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, st
}

// seedJournalDays inserts one message per date and builds the mechanical journal.
func seedJournalDays(t *testing.T, st *store.Store, conv string, dates []string) {
	t.Helper()
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.Signal, conv)
	if err != nil {
		t.Fatal(err)
	}
	msgs := make([]signal.Message, 0, len(dates))
	for _, d := range dates {
		ts := d + " 09:00:00"
		parsed, _ := time.Parse(signal.TimestampLayout, ts)
		msgs = append(msgs, signal.Message{
			Conversation: conv, Timestamp: parsed, TimestampRaw: ts, Sender: conv, Body: "note on " + d,
		})
	}
	if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal, msgs); err != nil {
		t.Fatal(err)
	}
	days, err := st.BuildJournalDays(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range days {
		if err := st.PutJournalDay(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
}

func putDigest(t *testing.T, st *store.Store, day, summary, mood, structured string) {
	t.Helper()
	if err := st.PutDayDigest(context.Background(), store.JournalDigest{
		Day: day, Model: "m", PromptVersion: "pv", Body: summary, Structured: structured, Mood: mood,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestJournalEmptyState(t *testing.T) {
	srv, _ := newJournalServer(t)
	rec := get(t, srv, "/journal")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No journal yet") || !strings.Contains(body, "msgbrowse journal") {
		t.Error("empty state should point at `msgbrowse journal`")
	}
}

func TestJournalCalendarRendersMonthAndStats(t *testing.T) {
	srv, st := newJournalServer(t)
	seedJournalDays(t, st, "Harper", []string{"2023-05-01", "2023-05-02", "2023-05-03"})

	body := get(t, srv, "/journal?year=2023&month=5").Body.String()
	for _, want := range []string{
		`<main id="main-content"`, "May 2023", "days with entries", "longest streak",
		"/journal?day=2023-05-01", ">2023<", "cal-day",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("calendar missing %q", want)
		}
	}
}

func TestJournalDayCardStructured(t *testing.T) {
	srv, st := newJournalServer(t)
	seedJournalDays(t, st, "Harper", []string{"2023-05-01"})
	putDigest(t, st, "2023-05-01", "A calm day with Harper.", "upbeat",
		`{"summary":"A calm day with Harper.","people":["Harper"],"themes":["travel"],"mood":"upbeat",`+
			`"highlights":[{"text":"Booked the cabin","time":"09:14"}],"standout_media":[],"notable_links":["https://ex.com/trip"]}`)

	body := get(t, srv, "/journal?day=2023-05-01").Body.String()
	for _, want := range []string{
		"This day, editorialized", "A calm day with Harper.", "Booked the cabin", "09:14",
		"Harper", "travel", "upbeat", "https://ex.com/trip",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("day card missing %q", want)
		}
	}
}

func TestJournalDefaultsToLatestDay(t *testing.T) {
	srv, st := newJournalServer(t)
	seedJournalDays(t, st, "Harper", []string{"2023-04-15", "2023-05-20"})
	putDigest(t, st, "2023-05-20", "The latest editorialized day.", "quiet",
		`{"summary":"The latest editorialized day.","people":[],"themes":[],"mood":"quiet","highlights":[],"standout_media":[],"notable_links":[]}`)

	// No params → defaults to the newest day (2023-05-20) and its month.
	body := get(t, srv, "/journal").Body.String()
	if !strings.Contains(body, "May 2023") || !strings.Contains(body, "The latest editorialized day.") {
		t.Error("no-param journal should open on the latest day's card")
	}
}

func TestJournalYearTabOpensLatestMonth(t *testing.T) {
	srv, st := newJournalServer(t)
	// 2023 activity starts in June — a year tab must open on the latest active
	// month, not an empty January.
	seedJournalDays(t, st, "Harper", []string{"2023-06-10", "2023-09-20"})
	body := get(t, srv, "/journal?year=2023").Body.String()
	if !strings.Contains(body, "September 2023") {
		t.Error("year tab should open on the year's latest active month")
	}
	if strings.Contains(body, "January 2023") {
		t.Error("year tab must not open on an empty January")
	}
}

func TestJournalPartialOmitsShell(t *testing.T) {
	srv, st := newJournalServer(t)
	seedJournalDays(t, st, "Harper", []string{"2023-05-01"})

	full := get(t, srv, "/journal").Body.String()
	if !strings.Contains(full, "<!doctype html>") {
		t.Error("full-page render should carry the document shell")
	}
	req := httptest.NewRequest(http.MethodGet, "/journal", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	partial := rec.Body.String()
	if strings.Contains(partial, "<!doctype html>") || strings.Contains(partial, "<html") {
		t.Error("boosted partial must not carry the document shell")
	}
	if !strings.Contains(partial, `<main id="main-content"`) {
		t.Error("partial should carry #main-content")
	}
}

func TestJournalDigestFieldsEscaped(t *testing.T) {
	srv, st := newJournalServer(t)
	seedJournalDays(t, st, "Harper", []string{"2023-05-01"})
	// Untrusted model output with markup in the summary + a person.
	putDigest(t, st, "2023-05-01", "<script>alert(1)</script> ok", "neutral",
		`{"summary":"<script>alert(1)</script> ok","people":["<b>x</b>"],"themes":[],"mood":"neutral","highlights":[],"standout_media":[],"notable_links":[]}`)

	body := get(t, srv, "/journal?day=2023-05-01").Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") || strings.Contains(body, "<b>x</b>") {
		t.Error("structured digest markup rendered live — escaping bypassed")
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("digest summary should appear HTML-escaped")
	}
}
