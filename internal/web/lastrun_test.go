package web

// Tests for the "Last run" tile fix (issue #443): the tile shows the most
// recent FINISHED run, independent of whether a newer run is in flight or
// stalled — the tile and the history table beneath it must never contradict
// each other.
//
// @joestump-agent 09/04/2026 - Added with #443.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/store"
)

// TestJournalTileShowsFinishedRunBesideStalledRun: a stalled run (cold
// heartbeat, no terminal write) must not blank the tile to "never" while the
// history table lists earlier finished runs. The stalled banner and the last
// finished run both render.
func TestJournalTileShowsFinishedRunBesideStalledRun(t *testing.T) {
	srv, st, _ := newTestServer(t)
	srv.SetJournalBuilder(newFakeJournalBuilder("test-chat", true))
	ctx := context.Background()
	seedJournalDay(t, st, "2026-06-01", 12, -1)

	// An older, properly finished run.
	fin := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	jid, err := st.BeginJournalRun(ctx, "test-chat", "", fin.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishJournalRun(ctx, store.JournalRun{
		ID: jid, FinishedAt: fin, DurationMS: 900, Digested: 7, Error: "boom 502",
	}); err != nil {
		t.Fatal(err)
	}

	// A newer stalled run: heartbeat cold past the stale threshold.
	sid, err := st.BeginJournalRun(ctx, "test-chat", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateJournalRunProgress(ctx, sid, 1, 1,
		time.Now().Add(-journalRunStaleAfter-time.Minute)); err != nil {
		t.Fatal(err)
	}

	body := get(t, srv, "/settings/journal").Body.String()
	if !contains(body, "Interrupted") {
		t.Error("stalled banner missing — the interruption itself must still surface")
	}
	if !contains(body, "boom 502") {
		t.Errorf("tile must show the last FINISHED run's error, got:\n%s", truncFor(body))
	}
	if contains(body, ">never<") {
		t.Errorf("tile reads \"never\" while a finished run exists:\n%s", truncFor(body))
	}
}

// TestJournalTileShowsFinishedRunDuringLiveRun: a live heartbeat is not a
// blank slate either — the tile keeps reporting the prior finished run while
// the new one runs.
func TestJournalTileShowsFinishedRunDuringLiveRun(t *testing.T) {
	srv, st, _ := newTestServer(t)
	srv.SetJournalBuilder(newFakeJournalBuilder("test-chat", true))
	ctx := context.Background()
	seedJournalDay(t, st, "2026-06-01", 12, -1)

	fin := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	jid, err := st.BeginJournalRun(ctx, "test-chat", "", fin.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishJournalRun(ctx, store.JournalRun{
		ID: jid, FinishedAt: fin, DurationMS: 900, Digested: 7,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginJournalRun(ctx, "test-chat", "", time.Now()); err != nil {
		t.Fatal(err)
	}

	rec := get(t, srv, "/settings/journal")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if contains(body, ">never<") {
		t.Errorf("tile reads \"never\" during a live run with a finished predecessor:\n%s", truncFor(body))
	}
	if !contains(body, fin.In(time.Local).Format(overviewTimeFormat)) {
		t.Errorf("tile missing the finished run's stamp:\n%s", truncFor(body))
	}
}

// truncFor caps a body for failure output.
func truncFor(s string) string {
	if len(s) > 400 {
		return s[:400]
	}
	return s
}
