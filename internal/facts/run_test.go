package facts

import (
	"context"
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

// fakeClient is an llm.Client that returns a canned facts response and records
// the prompts it was asked to complete.
type fakeClient struct {
	mu      sync.Mutex
	prompts []string
	resp    string
	chatErr error // when set, Chat returns this (simulates a transport error)
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
func (f *fakeClient) ListModels(context.Context) ([]string, error) { return nil, nil }

func (f *fakeClient) sawContact(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.prompts {
		if strings.Contains(p, "Contact: "+name) {
			return true
		}
	}
	return false
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "facts.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seed(t *testing.T, st *store.Store, src, name string) {
	t.Helper()
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, src, name)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(ts, sender, body string) signal.Message {
		parsed, _ := time.Parse(signal.TimestampLayout, ts)
		return signal.Message{Conversation: name, Timestamp: parsed, TimestampRaw: ts, Sender: sender, Body: body}
	}
	if _, err := st.ReplaceConversationMessages(ctx, id, src, []signal.Message{
		mk("2023-05-01 10:00:00", name, "hello from "+name),
		mk("2023-05-01 10:01:00", signal.OwnerSender, "hi back"),
	}); err != nil {
		t.Fatal(err)
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRunExtractsHonorsExcludeAndIsIncremental(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seed(t, st, source.Signal, "Alex")
	seed(t, st, source.Signal, "Blair")
	seed(t, st, source.Signal, "Secret")

	client := &fakeClient{resp: `[{"fact":"Really loves hiking in the mountains","category":"preferences","evidence":1}]`}
	opts := Options{Model: "test-model", Exclude: []string{"Secret"}, Logger: quietLogger()}

	sum, err := Run(ctx, st, client, opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Conversations != 2 {
		t.Errorf("Conversations = %d, want 2 (Secret excluded)", sum.Conversations)
	}
	if sum.FactsAdded != 2 {
		t.Errorf("FactsAdded = %d, want 2 (one per contact)", sum.FactsAdded)
	}
	if client.sawContact("Secret") {
		t.Error("excluded conversation Secret was sent to the LLM")
	}

	total, err := st.CountFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("stored facts = %d, want 2", total)
	}

	// Second run: the cursor has consumed every message, so nothing new is sent
	// and no facts are added.
	callsBefore := client.calls
	sum2, err := Run(ctx, st, client, opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum2.FactsAdded != 0 {
		t.Errorf("re-run FactsAdded = %d, want 0 (incremental)", sum2.FactsAdded)
	}
	if client.calls != callsBefore {
		t.Errorf("re-run made %d new LLM calls, want 0 (cursor exhausted)", client.calls-callsBefore)
	}
}

func TestRunSkipsUnparseableBatchWithoutAborting(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seed(t, st, source.Signal, "Alex")
	seed(t, st, source.Signal, "Blair")

	// A response with no JSON array is unparseable. It must be skipped (logged),
	// not abort the run, and the cursor must still advance so it isn't retried.
	client := &fakeClient{resp: "Sorry, I can't help with that."}
	sum, err := Run(ctx, st, client, Options{Model: "m", Logger: quietLogger()})
	if err != nil {
		t.Fatalf("Run aborted on an unparseable batch: %v", err)
	}
	if sum.FactsAdded != 0 {
		t.Errorf("FactsAdded = %d, want 0", sum.FactsAdded)
	}
	if sum.Conversations != 2 {
		t.Errorf("Conversations = %d, want 2 (both still processed)", sum.Conversations)
	}
	if n, _ := st.CountFacts(ctx); n != 0 {
		t.Errorf("stored facts = %d, want 0", n)
	}
	// Cursor advanced despite the skip: a re-run makes no new LLM calls.
	before := client.calls
	if _, err := Run(ctx, st, client, Options{Model: "m", Logger: quietLogger()}); err != nil {
		t.Fatal(err)
	}
	if client.calls != before {
		t.Errorf("re-run made %d new calls, want 0 (cursor advanced past skipped batch)", client.calls-before)
	}
}

func TestRunAbortsOnTransportError(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seed(t, st, source.Signal, "Alex")

	client := &fakeClient{chatErr: errors.New("connection refused")}
	_, err := Run(ctx, st, client, Options{Model: "m", Concurrency: 1, Logger: quietLogger()})
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("Run err = %v, want a transport error to abort the run", err)
	}
	// A transport error must NOT advance the cursor (so the next run resumes).
	var rows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM fact_state`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("fact_state rows = %d, want 0 (cursor not advanced on abort)", rows)
	}
}

func TestRunOnlyConversationAndReset(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seed(t, st, source.Signal, "Alex")
	seed(t, st, source.Signal, "Blair")

	// Find Alex's conversation id.
	var alexID int64
	if err := st.DB().QueryRow(`SELECT id FROM conversations WHERE name = 'Alex'`).Scan(&alexID); err != nil {
		t.Fatal(err)
	}

	client := &fakeClient{resp: `[{"fact":"Really loves hiking in the mountains","category":"preferences","evidence":1}]`}
	sum, err := Run(ctx, st, client, Options{Model: "m", OnlyConversationID: alexID, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Conversations != 1 || sum.FactsAdded != 1 {
		t.Errorf("scoped run = %+v, want 1 conversation / 1 fact", sum)
	}
	if client.sawContact("Blair") {
		t.Error("OnlyConversationID did not scope the run; Blair was processed")
	}

	// Reset wipes facts + cursors so a full run re-derives everything.
	sumReset, err := Run(ctx, st, client, Options{Model: "m", Reset: true, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if sumReset.FactsAdded != 2 {
		t.Errorf("reset run FactsAdded = %d, want 2 (re-derived for both)", sumReset.FactsAdded)
	}
}

// TestRunRecordsRunLog covers the #366 bookkeeping: every pass writes a
// fact_runs row, and it writes the TERMINAL row on failure too. The terminal
// write is what the web layer's cross-process guard depends on — a run that
// dies without one leaves the card reading "extracting…" for the whole
// staleness window, and that window is also a window in which no new run may
// start.
func TestRunRecordsRunLog(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	seed(t, st, source.Signal, "Harper")
	client := &fakeClient{resp: `[{"fact":"Has a dog named Biscuit","category":"personal","evidence":1}]`}

	sum, err := Run(ctx, st, client, Options{Model: "test-chat", Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.LatestFactRun(ctx)
	if err != nil || run == nil {
		t.Fatalf("LatestFactRun = %v, %v; want a recorded run", run, err)
	}
	if run.InFlight() {
		t.Error("a completed pass left its run row in flight")
	}
	if run.Model != "test-chat@p2" || run.Scope != store.FactScopeArchive {
		t.Errorf("run = %+v, want model test-chat and the archive scope", run)
	}
	if run.FactsAdded != sum.FactsAdded || run.Conversations != sum.Conversations {
		t.Errorf("run totals %d/%d disagree with the summary %d/%d",
			run.Conversations, run.FactsAdded, sum.Conversations, sum.FactsAdded)
	}

	// A reset pass records the reset scope, so the run history can say which
	// runs were the expensive ones.
	if _, err := Run(ctx, st, client, Options{Model: "test-chat", Reset: true, Logger: quietLogger()}); err != nil {
		t.Fatal(err)
	}
	if run, _ = st.LatestFactRun(ctx); run.Scope != store.FactScopeReset {
		t.Errorf("reset run scope = %q, want %q", run.Scope, store.FactScopeReset)
	}

	// A transport failure still lands a terminal row carrying the abort reason.
	boom := &fakeClient{chatErr: errors.New("endpoint refused")}
	if _, err := Run(ctx, st, boom, Options{Model: "test-chat", Reset: true, Logger: quietLogger()}); err == nil {
		t.Fatal("expected the transport error to abort the run")
	}
	run, err = st.LatestFactRun(ctx)
	if err != nil || run == nil {
		t.Fatalf("LatestFactRun after a failure = %v, %v", run, err)
	}
	if run.InFlight() {
		t.Error("a failed pass left its run row in flight; the guard would block every later run")
	}
	if !strings.Contains(run.Error, "endpoint refused") {
		t.Errorf("run error = %q, want the abort reason", run.Error)
	}
}

// TestRunReapsOrphanFactsFirst (issue #447): a run deletes facts citing
// messages that a re-import invalidated before extracting, reports the count,
// and folds it into the run record's message.
func TestRunReapsOrphanFactsFirst(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seed(t, st, source.Signal, "Alex")

	var contactID int64
	if err := st.DB().QueryRow(`SELECT contact_id FROM conversations WHERE name = 'Alex'`).Scan(&contactID); err != nil {
		t.Fatal(err)
	}

	// An orphan: cites a hash no message carries.
	orphan := store.FactInput{ContactID: contactID, Fact: "stale orphan", Category: "c",
		Source: source.Signal, SourceMessageHash: "no-such-hash", SourceTS: "2020-01-01 00:00:00", Model: "test"}
	if added, err := st.PutFact(ctx, orphan); err != nil || !added {
		t.Fatalf("seed orphan: added=%v err=%v", added, err)
	}

	client := &fakeClient{resp: `[{"fact":"Really loves hiking in the mountains","category":"preferences","evidence":1}]`}
	sum, err := Run(ctx, st, client, Options{Model: "test-model", Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if sum.FactsReaped != 1 {
		t.Fatalf("FactsReaped = %d, want 1", sum.FactsReaped)
	}
	if n, _ := st.CountOrphanFacts(ctx); n != 0 {
		t.Fatalf("orphan survived the reap: %d", n)
	}

	// The run record carries the reap count in its message.
	runs, err := st.RecentFactRuns(ctx, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("recent runs: %v (n=%d)", err, len(runs))
	}
	if !strings.Contains(runs[0].Error, "1 orphaned facts reaped") {
		t.Errorf("run record = %q, want the reap count", runs[0].Error)
	}
}

// TestRunReportsNearDupCollapses (#449): a paraphrase of an existing fact is
// not stored and shows up in the run summary's collapse count.
func TestRunReportsNearDupCollapses(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seed(t, st, source.Signal, "Alex")

	var contactID int64
	if err := st.DB().QueryRow(`SELECT contact_id FROM conversations WHERE name = 'Alex'`).Scan(&contactID); err != nil {
		t.Fatal(err)
	}
	// Pre-existing fact; the canned response paraphrases it.
	var seedHash string
	if err := st.DB().QueryRow(`SELECT hash FROM messages WHERE conversation_id = (SELECT id FROM conversations WHERE name = 'Alex') LIMIT 1`).Scan(&seedHash); err != nil {
		t.Fatal(err)
	}
	pre := store.FactInput{ContactID: contactID, Fact: "Really loves hiking in the mountains", Category: "preferences",
		Source: source.Signal, SourceMessageHash: seedHash, SourceTS: "2023-05-01 09:00:00", Model: "test"}
	if added, err := st.PutFact(ctx, pre); err != nil || !added {
		t.Fatalf("seed fact: added=%v err=%v", added, err)
	}

	client := &fakeClient{resp: `[{"fact":"Loves hiking in the mountains often","category":"preferences","evidence":1}]`}
	sum, err := Run(ctx, st, client, Options{Model: "test-model", Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if sum.NearDupsCollapsed != 1 {
		var fact, cat string
		_ = st.DB().QueryRow(`SELECT fact, category FROM contact_facts LIMIT 1`).Scan(&fact, &cat)
		t.Fatalf("NearDupsCollapsed = %d; stored fact %q cat %q; summary %+v", sum.NearDupsCollapsed, fact, cat, sum)
	}
	total, err := st.CountFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("stored facts = %d, want 1 (paraphrase collapsed)", total)
	}
}
