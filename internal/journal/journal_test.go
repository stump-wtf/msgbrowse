package journal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/llm"
	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// fakeClient is an llm.Client that returns a canned digest and records the user
// prompts it saw and how many times it was called.
type fakeClient struct {
	mu      sync.Mutex
	prompts []string
	resp    string
	chatErr error
	calls   int
}

func (f *fakeClient) Chat(_ context.Context, req llm.ChatRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	for _, m := range req.Messages {
		if m.Role == llm.RoleUser {
			f.prompts = append(f.prompts, m.Content)
		}
	}
	if f.chatErr != nil {
		return "", f.chatErr
	}
	return f.resp, nil
}

func (f *fakeClient) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("unused")
}
func (f *fakeClient) Transcribe(context.Context, []byte, string) (string, error) {
	return "", errors.New("unused")
}
func (f *fakeClient) Vision(context.Context, []byte, string, string) (string, error) {
	return "", errors.New("unused")
}

func (f *fakeClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeClient) sawText(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.prompts {
		if strings.Contains(p, s) {
			return true
		}
	}
	return false
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "journal.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func mk(conv, ts, sender, body string) signal.Message {
	parsed, _ := time.Parse(signal.TimestampLayout, ts)
	return signal.Message{Conversation: conv, Timestamp: parsed, TimestampRaw: ts, Sender: sender, Body: body}
}

func seedConv(t *testing.T, st *store.Store, src, name string, msgs []signal.Message) {
	t.Helper()
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, src, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, id, src, msgs); err != nil {
		t.Fatal(err)
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const testPrompt = "Summarize the day."

// jsonDigest is a minimal valid structured-digest response with the given
// summary — the shape the model must now return (parseDigest rejects prose).
func jsonDigest(summary string) string {
	return `{"summary":"` + summary + `","people":[],"themes":[],"mood":"upbeat","highlights":[],"standout_media":[],"notable_links":[]}`
}

func baseOpts() Options {
	return Options{Model: "test-model", DigestEnabled: true, DigestPrompt: testPrompt, Logger: quietLogger()}
}

func TestRunBuildsMechanicalAndDigests(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	seedConv(t, st, source.Signal, "Harper", []signal.Message{
		mk("Harper", "2023-05-01 09:00:00", "Harper", "hello there"),
		mk("Harper", "2023-05-02 09:00:00", "Harper", "world today"),
	})

	client := &fakeClient{resp: jsonDigest("A short digest.")}
	sum, err := Run(ctx, st, client, baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Days != 2 || sum.Digested != 2 || sum.Cached != 0 {
		t.Errorf("summary = %+v, want Days:2 Digested:2 Cached:0", sum)
	}
	if client.callCount() != 2 {
		t.Errorf("LLM calls = %d, want 2", client.callCount())
	}
	if !client.sawText("hello there") {
		t.Error("transcript for the first day was not sent to the LLM")
	}
	// Digests persisted and readable.
	for _, day := range []string{"2023-05-01", "2023-05-02"} {
		body, model, _, ok, err := st.GetDayDigest(ctx, day)
		if err != nil || !ok || body != "A short digest." || model != "test-model" {
			t.Errorf("digest %s = (%q,%q,%v,%v), want stored", day, body, model, ok, err)
		}
	}
}

func TestRunCacheHitSkipsSecondRun(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	seedConv(t, st, source.Signal, "Harper", []signal.Message{
		mk("Harper", "2023-05-01 09:00:00", "Harper", "hello"),
		mk("Harper", "2023-05-02 09:00:00", "Harper", "world"),
	})

	if _, err := Run(ctx, st, &fakeClient{resp: jsonDigest("d")}, baseOpts()); err != nil {
		t.Fatal(err)
	}
	second := &fakeClient{resp: jsonDigest("d")}
	sum, err := Run(ctx, st, second, baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Digested != 0 || sum.Cached != 2 {
		t.Errorf("second run = %+v, want Digested:0 Cached:2", sum)
	}
	if second.callCount() != 0 {
		t.Errorf("second run made %d LLM calls, want 0", second.callCount())
	}
}

func TestRunStalePromptReDigests(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	seedConv(t, st, source.Signal, "Harper", []signal.Message{
		mk("Harper", "2023-05-01 09:00:00", "Harper", "hello"),
	})
	if _, err := Run(ctx, st, &fakeClient{resp: jsonDigest("d")}, baseOpts()); err != nil {
		t.Fatal(err)
	}
	// A changed prompt bumps prompt_version → the day is stale and re-digested.
	changed := baseOpts()
	changed.DigestPrompt = "A completely different instruction."
	client := &fakeClient{resp: jsonDigest("d2")}
	sum, err := Run(ctx, st, client, changed)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Digested != 1 || sum.Cached != 0 {
		t.Errorf("stale-prompt run = %+v, want Digested:1 Cached:0", sum)
	}
	if client.callCount() != 1 {
		t.Errorf("stale-prompt LLM calls = %d, want 1", client.callCount())
	}
}

func TestRunRegenerateWipesCache(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	seedConv(t, st, source.Signal, "Harper", []signal.Message{
		mk("Harper", "2023-05-01 09:00:00", "Harper", "hello"),
		mk("Harper", "2023-05-02 09:00:00", "Harper", "world"),
	})
	if _, err := Run(ctx, st, &fakeClient{resp: jsonDigest("d")}, baseOpts()); err != nil {
		t.Fatal(err)
	}
	regen := baseOpts()
	regen.Regenerate = true
	client := &fakeClient{resp: jsonDigest("d")}
	sum, err := Run(ctx, st, client, regen)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Digested != 2 || sum.Cached != 0 {
		t.Errorf("regenerate run = %+v, want Digested:2 Cached:0", sum)
	}
}

func TestRunDryRunMakesNoCallsOrWrites(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	seedConv(t, st, source.Signal, "Harper", []signal.Message{
		mk("Harper", "2023-05-01 09:00:00", "Harper", "hello"),
		mk("Harper", "2023-05-02 09:00:00", "Harper", "world"),
	})
	dry := baseOpts()
	dry.DryRun = true
	client := &fakeClient{resp: jsonDigest("d")}
	sum, err := Run(ctx, st, client, dry)
	if err != nil {
		t.Fatal(err)
	}
	if client.callCount() != 0 {
		t.Errorf("dry run made %d LLM calls, want 0", client.callCount())
	}
	if sum.Eligible != 2 || sum.EstimatedTokens <= 0 {
		t.Errorf("dry run = %+v, want Eligible:2 and a positive token estimate", sum)
	}
	// Nothing persisted: no mechanical rows, no digests.
	list, err := st.ListJournalDays(ctx, "", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("dry run persisted %d journal_days rows, want 0", len(list))
	}
	if _, _, _, ok, _ := st.GetDayDigest(ctx, "2023-05-01"); ok {
		t.Error("dry run persisted a digest")
	}
}

func TestRunMaxDaysPerRunCapsAndResumes(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	seedConv(t, st, source.Signal, "Harper", []signal.Message{
		mk("Harper", "2023-05-01 09:00:00", "Harper", "one"),
		mk("Harper", "2023-05-02 09:00:00", "Harper", "two"),
		mk("Harper", "2023-05-03 09:00:00", "Harper", "three"),
	})
	opts := baseOpts()
	opts.MaxDaysPerRun = 2

	first := &fakeClient{resp: jsonDigest("d")}
	sum, err := Run(ctx, st, first, opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Eligible != 3 || sum.Digested != 2 || sum.Remaining != 1 {
		t.Errorf("first capped run = %+v, want Eligible:3 Digested:2 Remaining:1", sum)
	}
	// Default order is newest-first: the oldest day is the one left over.
	if _, _, _, ok, _ := st.GetDayDigest(ctx, "2023-05-01"); ok {
		t.Error("oldest day should be the one deferred by the cap")
	}

	second := &fakeClient{resp: jsonDigest("d")}
	sum, err = Run(ctx, st, second, opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Digested != 1 || sum.Remaining != 0 {
		t.Errorf("second capped run = %+v, want Digested:1 Remaining:0", sum)
	}
}

func TestRunExcludeNeverReachesLLM(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	seedConv(t, st, source.Signal, "Harper", []signal.Message{
		mk("Harper", "2023-05-01 09:00:00", "Harper", "ordinary chatter"),
	})
	seedConv(t, st, source.Signal, "Secret", []signal.Message{
		mk("Secret", "2023-05-01 10:00:00", "Secret", "TOPSECRETPAYLOAD"),
	})
	opts := baseOpts()
	opts.Exclude = []string{"Secret"}
	client := &fakeClient{resp: jsonDigest("d")}
	if _, err := Run(ctx, st, client, opts); err != nil {
		t.Fatal(err)
	}
	if client.sawText("TOPSECRETPAYLOAD") {
		t.Error("excluded conversation content was sent to the LLM")
	}
	// The mechanical rollup also excludes it: the day counts only Harper's message.
	list, err := st.ListJournalDays(ctx, "", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].MessageCount != 1 {
		t.Errorf("day rollup = %+v, want a single day with 1 message", list)
	}
}

func TestRunDigestDisabledBuildsMechanicalOnly(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	seedConv(t, st, source.Signal, "Harper", []signal.Message{
		mk("Harper", "2023-05-01 09:00:00", "Harper", "hello"),
	})
	opts := baseOpts()
	opts.DigestEnabled = false
	client := &fakeClient{resp: jsonDigest("d")}
	sum, err := Run(ctx, st, client, opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Days != 1 || sum.Digested != 0 {
		t.Errorf("digest-disabled run = %+v, want Days:1 Digested:0", sum)
	}
	if client.callCount() != 0 {
		t.Errorf("digest-disabled made %d LLM calls, want 0", client.callCount())
	}
	// Mechanical layer was still persisted.
	if list, _ := st.ListJournalDays(ctx, "", 30); len(list) != 1 {
		t.Errorf("mechanical rows = %d, want 1", len(list))
	}
}

func TestRunNoModelSkipsDigestsWithoutError(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	seedConv(t, st, source.Signal, "Harper", []signal.Message{
		mk("Harper", "2023-05-01 09:00:00", "Harper", "hello"),
	})
	opts := baseOpts()
	opts.Model = "" // digest enabled but no chat model configured
	client := &fakeClient{resp: jsonDigest("d")}
	sum, err := Run(ctx, st, client, opts)
	if err != nil {
		t.Fatalf("want no error when model unset, got %v", err)
	}
	if sum.Days != 1 || sum.Digested != 0 || client.callCount() != 0 {
		t.Errorf("no-model run = %+v (calls %d), want mechanical-only", sum, client.callCount())
	}
}

func TestRunTransportErrorAborts(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	seedConv(t, st, source.Signal, "Harper", []signal.Message{
		mk("Harper", "2023-05-01 09:00:00", "Harper", "hello"),
	})
	client := &fakeClient{chatErr: errors.New("boom")}
	sum, err := Run(ctx, st, client, baseOpts())
	if err == nil {
		t.Fatal("want a transport error to abort the run")
	}
	if sum.Digested != 0 {
		t.Errorf("Digested = %d on error, want 0", sum.Digested)
	}
	// The mechanical layer still committed before the digest phase failed.
	if list, _ := st.ListJournalDays(ctx, "", 30); len(list) != 1 {
		t.Errorf("mechanical rows = %d, want 1 (built before the digest error)", len(list))
	}
}

func TestParseDigest(t *testing.T) {
	// Valid JSON wrapped in fences + prose, with an empty person and an
	// empty-text/bad-time highlight → tolerated + cleaned.
	raw := "here you go:\n```json\n" +
		`{"summary":"A calm day.","mood":"quiet","people":["Harper"," "],"themes":["travel"],` +
		`"highlights":[{"text":"Booked the trip","time":"09:14"},{"text":"","time":"nope"}],` +
		`"standout_media":[],"notable_links":["https://ex.com"]}` + "\n```"
	pd, err := parseDigest(raw)
	if err != nil {
		t.Fatalf("parseDigest(valid) err = %v", err)
	}
	if pd.Summary != "A calm day." || pd.Mood != "quiet" {
		t.Errorf("summary/mood = %q/%q", pd.Summary, pd.Mood)
	}
	var d Digest
	if err := json.Unmarshal([]byte(pd.Canonical), &d); err != nil {
		t.Fatalf("canonical not valid JSON: %v", err)
	}
	if len(d.People) != 1 || d.People[0] != "Harper" {
		t.Errorf("people = %v, want the empty one dropped", d.People)
	}
	if len(d.Highlights) != 1 || d.Highlights[0].Time != "09:14" {
		t.Errorf("highlights = %v, want the empty-text one dropped", d.Highlights)
	}

	// Unknown mood → coerced to neutral (never fails).
	if pd, err := parseDigest(`{"summary":"x","mood":"ecstatic"}`); err != nil || pd.Mood != "neutral" {
		t.Errorf("unknown mood → %q (err %v), want neutral", pd.Mood, err)
	}

	// No JSON, empty/blank summary, or broken JSON → errBadDigest.
	for _, bad := range []string{"just prose", "", `{"summary":"   ","mood":"upbeat"}`, `{not json`} {
		if _, err := parseDigest(bad); !errors.Is(err, errBadDigest) {
			t.Errorf("parseDigest(%q) err = %v, want errBadDigest", bad, err)
		}
	}
}

func TestRunSkipsMalformedDigest(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	seedConv(t, st, source.Signal, "Harper", []signal.Message{
		mk("Harper", "2023-05-01 09:00:00", "Harper", "hello"),
	})
	// A response with no JSON must skip the day, not wedge the run.
	sum, err := Run(ctx, st, &fakeClient{resp: "sorry, I can't do that"}, baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Digested != 0 || sum.Skipped != 1 {
		t.Errorf("malformed-digest run = %+v, want Digested:0 Skipped:1", sum)
	}
}
