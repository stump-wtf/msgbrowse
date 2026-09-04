package sentiment

// Tests for the per-day rescoring entry point (issue #441): RunDay replaces
// exactly that UTC day's rows and nothing else, leaves sentiment_state cursors
// untouched (the day's messages are already behind them), keeps opted-out
// contacts unscored, and records a day:YYYY-MM-DD run the history can show.
//
// @joestump-agent 09/04/2026 - Added with #441.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// seedDayN creates a conversation with n real messages on the given UTC day
// (10:00 onward), mirroring run_test.go's seedN but with a selectable day.
func seedDayN(t *testing.T, st *store.Store, name, day string, n int) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.Signal, name)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := time.Parse(signal.TimestampLayout, day+" 10:00:00")
	msgs := make([]signal.Message, 0, n)
	for i := range n {
		ts := base.Add(time.Duration(i) * time.Minute)
		raw := ts.Format(signal.TimestampLayout)
		sender := name
		if i%2 == 1 {
			sender = signal.OwnerSender
		}
		msgs = append(msgs, signal.Message{
			Conversation: name, Timestamp: ts, TimestampRaw: raw,
			Sender: sender, Body: "day seed message",
		})
	}
	if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal, msgs); err != nil {
		t.Fatal(err)
	}
	return id
}

func dayHashes(t *testing.T, st *store.Store, day string) []string {
	t.Helper()
	rows, err := st.DB().Query(`SELECT hash FROM messages WHERE date(ts_unix,'unixepoch') = ?`, day)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
	return out
}

func dayScoreCount(t *testing.T, st *store.Store, day, construct string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM message_sentiment WHERE date(ts_unix,'unixepoch') = ? AND construct = ?`,
		day, construct).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// seedStaleRow plants one score row carrying a construct the scoring pass
// never produces, stamped with the message's REAL ts_unix so the day bucket
// matches (the row must be deleted by RunDay on its own day only).
func seedStaleRow(t *testing.T, st *store.Store, gen store.SentimentGeneration, hash, construct string) {
	t.Helper()
	var tsUnix int64
	if err := st.DB().QueryRow(`SELECT ts_unix FROM messages WHERE hash = ?`, hash).Scan(&tsUnix); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSentimentScores(t.Context(), gen, []store.SentimentScore{
		{MessageHash: hash, Construct: construct, Score: 0.5, TSUnix: tsUnix, ContactID: store.OwnerContactID},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRunDayReplacesOnlyThatDay(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seedDayN(t, st, "Alex", "2023-05-01", 4)
	seedDayN(t, st, "Sam", "2023-05-02", 4)

	lex, lexErr := BuildLexicon()
	if lexErr != nil {
		t.Fatal(lexErr)
	}
	gen := store.SentimentGeneration{Model: "test-model", LexiconVersion: lex.Version}
	// Stale rows on both days: the day-1 one must be replaced, the day-2 one
	// must survive untouched.
	h1 := dayHashes(t, st, "2023-05-01")[0]
	h2 := dayHashes(t, st, "2023-05-02")[0]
	seedStaleRow(t, st, gen, h1, "Stale")
	seedStaleRow(t, st, gen, h2, "Stale")

	client := &fakeClient{respFn: func(p string) (string, error) { return scoreEveryMessage(p) }}
	if _, err := RunDay(ctx, st, client, baseOpts(), "2023-05-01"); err != nil {
		t.Fatalf("RunDay: %v", err)
	}

	if got := dayScoreCount(t, st, "2023-05-01", "Stale"); got != 0 {
		t.Errorf("day-1 stale rows survived: %d", got)
	}
	if got := dayScoreCount(t, st, "2023-05-01", "Cheerfulness"); got != 4 {
		t.Errorf("day-1 Cheerfulness rows = %d, want one per message (4)", got)
	}
	if got := dayScoreCount(t, st, "2023-05-02", "Stale"); got != 1 {
		t.Errorf("day-2 rows must be untouched: Stale = %d, want 1", got)
	}
	if got := dayScoreCount(t, st, "2023-05-02", "Cheerfulness"); got != 0 {
		t.Errorf("day-2 must not be rescored: Cheerfulness = %d, want 0", got)
	}
}

func TestRunDayLeavesCursorsUntouched(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	conv := seedDayN(t, st, "Alex", "2023-05-01", 4)

	if _, err := RunDay(ctx, st, &fakeClient{respFn: func(p string) (string, error) { return scoreEveryMessage(p) }}, baseOpts(), "2023-05-01"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := st.GetSentimentState(ctx, conv); err != nil || ok {
		t.Fatalf("RunDay must not write a sentiment_state cursor: ok=%v err=%v", ok, err)
	}
}

func TestRunDayHonorsOptOut(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seedDayN(t, st, "Alex", "2023-05-01", 4)

	var contactID int64
	if err := st.DB().QueryRow(`SELECT contact_id FROM conversations WHERE name = 'Alex'`).Scan(&contactID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSentimentOptOut(ctx, contactID, true); err != nil {
		t.Fatal(err)
	}

	if _, err := RunDay(ctx, st, &fakeClient{respFn: func(p string) (string, error) { return scoreEveryMessage(p) }}, baseOpts(), "2023-05-01"); err != nil {
		t.Fatal(err)
	}
	if got := dayScoreCount(t, st, "2023-05-01", "Cheerfulness"); got != 0 {
		t.Errorf("opted-out contact scored anyway: %d rows", got)
	}
}

func TestRunDayRecordsDayScopeAndContinuesPastFailure(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seedDayN(t, st, "Dead", "2023-05-01", 4)
	seedDayN(t, st, "Alive", "2023-05-01", 4)

	client := &fakeClient{respFn: func(p string) (string, error) {
		if strings.Contains(p, "Dead") {
			return "", errors.New("simulated transport failure")
		}
		return scoreEveryMessage(p)
	}}
	if _, err := RunDay(ctx, st, client, baseOpts(), "2023-05-01"); err != nil {
		t.Fatalf("one dead conversation must not fail the day: %v", err)
	}

	runs, err := st.RecentSentimentRuns(ctx, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("recent runs: %v (n=%d)", err, len(runs))
	}
	if runs[0].Scope != "day:2023-05-01" {
		t.Errorf("scope = %q, want day:2023-05-01", runs[0].Scope)
	}
	if runs[0].FinishedAt.IsZero() {
		t.Error("day run must record its terminal write")
	}
	if got := dayScoreCount(t, st, "2023-05-01", "Cheerfulness"); got != 4 {
		t.Errorf("surviving conversation rows = %d, want 4", got)
	}
}
