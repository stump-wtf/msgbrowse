package sentiment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/joestump/msgbrowse/internal/llm"
	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// fakeClient is an llm.Client returning a canned scoring response, recording the
// prompts it saw. The mutex is not decoration: Run drives it from a worker pool
// and the race detector runs in CI.
type fakeClient struct {
	mu       sync.Mutex
	systems  []string
	prompts  []string
	resp     string
	respFn   func(prompt string) (string, error)
	chatErr  error
	calls    int
	failFrom int // when > 0, Chat fails from this call onward
}

func (f *fakeClient) Chat(_ context.Context, req llm.ChatRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	var user string
	for _, m := range req.Messages {
		switch m.Role {
		case llm.RoleUser:
			user = m.Content
			f.prompts = append(f.prompts, m.Content)
		case llm.RoleSystem:
			f.systems = append(f.systems, m.Content)
		}
	}
	if f.failFrom > 0 && f.calls >= f.failFrom {
		return "", errors.New("simulated transport failure")
	}
	if f.chatErr != nil {
		return "", f.chatErr
	}
	if f.respFn != nil {
		return f.respFn(user)
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

func (f *fakeClient) sawConversation(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.prompts {
		if strings.Contains(p, "Conversation with: "+name) {
			return true
		}
	}
	return false
}

func (f *fakeClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sentiment.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedN creates a conversation with n real messages, alternating contact/owner.
func seedN(t *testing.T, st *store.Store, src, name string, n int) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, src, name)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := time.Parse(signal.TimestampLayout, "2023-05-01 10:00:00")
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
			Sender: sender, Body: fmt.Sprintf("message %d from %s", i+1, sender),
		})
	}
	if _, err := st.ReplaceConversationMessages(ctx, id, src, msgs); err != nil {
		t.Fatal(err)
	}
	return id
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// scoreEveryMessage builds a response scoring every message in the prompt, so a
// batch's row count is predictable regardless of batch size.
func scoreEveryMessage(prompt string) (string, error) {
	var n int
	for _, line := range strings.Split(prompt, "\n") {
		if len(line) > 0 && line[0] >= '1' && line[0] <= '9' && strings.Contains(line, ". [") {
			n++
		}
	}
	var b strings.Builder
	b.WriteString("[")
	for i := 1; i <= n; i++ {
		if i > 1 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"message":%d,"scores":{"Cheerfulness":0.8}}`, i)
	}
	b.WriteString("]")
	return b.String(), nil
}

func baseOpts() Options {
	return Options{Model: "test-model", Logger: quietLogger()}
}

func TestRunScoresHonorsExcludeAndIsIncremental(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seedN(t, st, source.Signal, "Alex", 2)
	seedN(t, st, source.Signal, "Blair", 2)
	seedN(t, st, source.Signal, "Secret", 2)

	client := &fakeClient{respFn: func(p string) (string, error) { return scoreEveryMessage(p) }}
	opts := baseOpts()
	opts.Exclude = []string{"Secret"}

	sum, err := Run(ctx, st, client, opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Conversations != 2 {
		t.Errorf("Conversations = %d, want 2 (Secret excluded)", sum.Conversations)
	}
	if sum.RowsWritten != 4 {
		t.Errorf("RowsWritten = %d, want 4 (2 messages x 2 conversations)", sum.RowsWritten)
	}
	if client.sawConversation("Secret") {
		t.Error("excluded conversation was sent to the LLM")
	}

	// Re-run: the cursor has consumed everything, so no LLM calls at all — the
	// REQ's "no new messages means no LLM calls and an unchanged table".
	before := client.callCount()
	rowsBefore := countRows(t, st)
	sum2, err := Run(ctx, st, client, opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum2.RowsWritten != 0 {
		t.Errorf("re-run RowsWritten = %d, want 0", sum2.RowsWritten)
	}
	if got := client.callCount(); got != before {
		t.Errorf("re-run made %d LLM calls, want 0", got-before)
	}
	if got := countRows(t, st); got != rowsBefore {
		t.Errorf("re-run changed the table: %d rows, want %d", got, rowsBefore)
	}
}

// TestRunRescansOnLexiconChange is the REQ scenario: conversations scored under
// one lexicon are rescanned when the binary ships another, and the new rows
// carry the new stamp.
func TestRunRescansOnGenerationChange(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	conv := seedN(t, st, source.Signal, "Alex", 2)

	client := &fakeClient{respFn: func(p string) (string, error) { return scoreEveryMessage(p) }}
	if _, err := Run(ctx, st, client, baseOpts()); err != nil {
		t.Fatal(err)
	}
	callsAfterFirst := client.callCount()

	// A different model is a different generation: the conversation must rescan.
	other := baseOpts()
	other.Model = "other-model"
	sum, err := Run(ctx, st, client, other)
	if err != nil {
		t.Fatal(err)
	}
	if sum.RowsWritten == 0 {
		t.Error("changing the model did not rescan the conversation")
	}
	if client.callCount() == callsAfterFirst {
		t.Error("changing the model made no new LLM calls")
	}

	_, gen, ok, err := st.GetSentimentState(ctx, conv)
	if err != nil || !ok {
		t.Fatalf("GetSentimentState: ok %v err %v", ok, err)
	}
	if gen.Model != "other-model" {
		t.Errorf("cursor model = %q, want other-model", gen.Model)
	}

	// Both generations coexist; neither is averaged into the other.
	lex := testLexicon(t)
	for _, tc := range []struct{ model string }{{"test-model"}, {"other-model"}} {
		n, err := st.CountSentimentScores(ctx, store.SentimentGeneration{Model: tc.model, LexiconVersion: lex.Version})
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Errorf("generation %q has no rows", tc.model)
		}
	}
}

// TestRunResumesFromCursorAfterFailure is the REQ scenario: a failure at batch N
// resumes from the last persisted cursor rather than the top, and does not
// duplicate what was already stored.
func TestRunResumesFromCursorAfterFailure(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seedN(t, st, source.Signal, "Alex", 12)

	opts := baseOpts()
	opts.BatchSize = 4
	opts.Concurrency = 1

	// Succeed on the first batch, fail on the second.
	client := &fakeClient{respFn: func(p string) (string, error) { return scoreEveryMessage(p) }, failFrom: 2}
	if _, err := Run(ctx, st, client, opts); err == nil {
		t.Fatal("Run succeeded despite a transport failure, want error")
	}
	afterFailure := countRows(t, st)
	if afterFailure == 0 {
		t.Fatal("no rows survived the failed run; the first batch's scores were lost")
	}

	// Resume cleanly.
	good := &fakeClient{respFn: func(p string) (string, error) { return scoreEveryMessage(p) }}
	if _, err := Run(ctx, st, good, opts); err != nil {
		t.Fatalf("resume run: %v", err)
	}

	// Every message is scored exactly once: 12 messages, one construct each.
	if got := countRows(t, st); got != 12 {
		t.Errorf("rows after resume = %d, want 12 (no gaps, no duplicates)", got)
	}

	// And the resume did not re-send the already-scored first batch.
	good.mu.Lock()
	firstResumePrompt := ""
	if len(good.prompts) > 0 {
		firstResumePrompt = good.prompts[0]
	}
	good.mu.Unlock()
	if strings.Contains(firstResumePrompt, "message 1 from Alex") {
		t.Error("resume re-sent the first batch instead of continuing from the cursor")
	}
}

// TestRunDoesNotAdvanceCursorOnTransportFailure pins the half of the contract
// that a passing resume test can hide: the failed batch must be retried, not
// skipped.
func TestRunDoesNotAdvanceCursorOnTransportFailure(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	conv := seedN(t, st, source.Signal, "Alex", 8)

	opts := baseOpts()
	opts.BatchSize = 4
	opts.Concurrency = 1

	client := &fakeClient{respFn: func(p string) (string, error) { return scoreEveryMessage(p) }, failFrom: 2}
	if _, err := Run(ctx, st, client, opts); err == nil {
		t.Fatal("want error")
	}
	lastHash, _, ok, err := st.GetSentimentState(ctx, conv)
	if err != nil || !ok {
		t.Fatalf("GetSentimentState: ok %v err %v", ok, err)
	}
	// The cursor must sit at the end of batch 1 (message 4), not batch 2.
	var body string
	if err := st.DB().QueryRow(`SELECT body FROM messages WHERE hash = ?`, lastHash).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "message 4") {
		t.Errorf("cursor is at %q, want the end of the last SUCCESSFUL batch (message 4)", body)
	}
}

// TestRunSkipsMalformedBatchWithoutAborting: a deterministically-bad response
// is logged and skipped, and the run completes.
func TestRunSkipsMalformedBatchWithoutAborting(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	seedN(t, st, source.Signal, "Alex", 4)

	client := &fakeClient{resp: "I'm afraid I can't score that."}
	sum, err := Run(ctx, st, client, baseOpts())
	if err != nil {
		t.Fatalf("a malformed response aborted the run: %v", err)
	}
	if sum.RowsWritten != 0 {
		t.Errorf("RowsWritten = %d, want 0", sum.RowsWritten)
	}
	// The cursor still advanced, so a re-run does not retry the bad batch forever.
	if sum2, err := Run(ctx, st, client, baseOpts()); err != nil || sum2.Batches != 0 {
		t.Errorf("re-run after a skipped batch: batches %d, err %v; want 0 batches", sum2.Batches, err)
	}
}

// TestRunSkipsOptedOutContactsBeforeReadingContent covers the privacy gate from
// the engine's side: an opted-out contact's messages are never sent.
func TestRunSkipsOptedOutContactsBeforeReadingContent(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	convA := seedN(t, st, source.Signal, "Alex", 2)
	seedN(t, st, source.Signal, "Blair", 2)

	var contactA int64
	if err := st.DB().QueryRow(`SELECT contact_id FROM conversations WHERE id = ?`, convA).Scan(&contactA); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSentimentOptOut(ctx, contactA, true); err != nil {
		t.Fatal(err)
	}

	client := &fakeClient{respFn: func(p string) (string, error) { return scoreEveryMessage(p) }}
	sum, err := Run(ctx, st, client, baseOpts())
	if err != nil {
		t.Fatal(err)
	}
	if client.sawConversation("Alex") {
		t.Error("an opted-out contact's messages were sent to the LLM")
	}
	if !client.sawConversation("Blair") {
		t.Error("a contact who did not opt out was skipped")
	}
	if sum.SkippedOptedOut != 1 {
		t.Errorf("SkippedOptedOut = %d, want 1", sum.SkippedOptedOut)
	}
}

// TestRunRejectsAnIneligibleTargetedConversation pins that --conversation with
// an id the run cannot score is an error. Succeeding with "0 scores written"
// makes a typo'd id look identical to a conversation that is already up to date.
func TestRunRejectsAnIneligibleTargetedConversation(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	conv := seedN(t, st, source.Signal, "Alex", 2)

	client := &fakeClient{respFn: func(p string) (string, error) { return scoreEveryMessage(p) }}

	opts := baseOpts()
	opts.OnlyConversationID = conv + 9999
	if _, err := Run(ctx, st, client, opts); err == nil {
		t.Error("Run accepted an unknown conversation id and reported success")
	}

	// On the exclude list is equally ineligible, and the message must say so
	// rather than looking like "nothing new to score".
	opts = baseOpts()
	opts.OnlyConversationID = conv
	opts.Exclude = []string{"Alex"}
	if _, err := Run(ctx, st, client, opts); err == nil {
		t.Error("Run accepted a targeted conversation that is on the exclude list")
	}

	// Opted out is its own case, reported distinctly.
	var contactA int64
	if err := st.DB().QueryRow(`SELECT contact_id FROM conversations WHERE id = ?`, conv).Scan(&contactA); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSentimentOptOut(ctx, contactA, true); err != nil {
		t.Fatal(err)
	}
	opts = baseOpts()
	opts.OnlyConversationID = conv
	_, err := Run(ctx, st, client, opts)
	if err == nil {
		t.Fatal("Run accepted a targeted conversation whose contact opted out")
	}
	if !strings.Contains(err.Error(), "opted out") {
		t.Errorf("error %q does not say the contact opted out", err)
	}
	if client.callCount() != 0 {
		t.Errorf("the LLM was called %d times for conversations that were never eligible", client.callCount())
	}
}

// TestRunTargetedConversationStillScores is the other half: a legitimate
// --conversation target must not be caught by the eligibility error above.
func TestRunTargetedConversationStillScores(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	conv := seedN(t, st, source.Signal, "Alex", 2)
	seedN(t, st, source.Signal, "Blair", 2)

	client := &fakeClient{respFn: func(p string) (string, error) { return scoreEveryMessage(p) }}
	opts := baseOpts()
	opts.OnlyConversationID = conv
	sum, err := Run(ctx, st, client, opts)
	if err != nil {
		t.Fatalf("Run on an eligible target: %v", err)
	}
	if sum.Conversations != 1 || sum.RowsWritten == 0 {
		t.Errorf("summary = %+v, want 1 conversation with rows written", sum)
	}
	if client.sawConversation("Blair") {
		t.Error("a targeted run scored a conversation other than its target")
	}
}

func TestRunResetClearsScoresButNotOptOuts(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	convA := seedN(t, st, source.Signal, "Alex", 2)

	client := &fakeClient{respFn: func(p string) (string, error) { return scoreEveryMessage(p) }}
	if _, err := Run(ctx, st, client, baseOpts()); err != nil {
		t.Fatal(err)
	}
	if countRows(t, st) == 0 {
		t.Fatal("no rows to reset")
	}

	var contactA int64
	if err := st.DB().QueryRow(`SELECT contact_id FROM conversations WHERE id = ?`, convA).Scan(&contactA); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSentimentOptOut(ctx, contactA, true); err != nil {
		t.Fatal(err)
	}

	opts := baseOpts()
	opts.Reset = true
	if _, err := Run(ctx, st, client, opts); err != nil {
		t.Fatal(err)
	}
	if out, _ := st.IsSentimentOptedOut(ctx, contactA); !out {
		t.Error("--reset cleared an opt-out")
	}
}

func TestRunRequiresAModel(t *testing.T) {
	st := openStore(t)
	opts := baseOpts()
	opts.Model = ""
	if _, err := Run(context.Background(), st, &fakeClient{}, opts); err == nil {
		t.Fatal("Run accepted an empty model, want error")
	}
}

// TestRunConcurrentConversations exercises the worker pool; the value is in
// running it under -race, which CI does.
func TestRunConcurrentConversations(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	for i := range 12 {
		seedN(t, st, source.Signal, fmt.Sprintf("Contact%02d", i), 6)
	}

	client := &fakeClient{respFn: func(p string) (string, error) { return scoreEveryMessage(p) }}
	opts := baseOpts()
	opts.Concurrency = 6
	opts.BatchSize = 2

	sum, err := Run(ctx, st, client, opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Conversations != 12 {
		t.Errorf("Conversations = %d, want 12", sum.Conversations)
	}
	if got, want := countRows(t, st), 12*6; got != want {
		t.Errorf("rows = %d, want %d", got, want)
	}
}

func TestRunCancellationStops(t *testing.T) {
	st := openStore(t)
	for i := range 6 {
		seedN(t, st, source.Signal, fmt.Sprintf("Contact%02d", i), 4)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := &fakeClient{respFn: func(p string) (string, error) { return scoreEveryMessage(p) }}
	if _, err := Run(ctx, st, client, baseOpts()); err == nil {
		t.Fatal("Run on a cancelled context returned nil, want error")
	}
}

// TestSystemPromptCarriesAnchorsAndFraming guards the two prompt requirements
// that are easy to lose in a later edit: the anchors with their keying, and the
// scoring-text-not-people framing.
func TestSystemPromptCarriesAnchorsAndFraming(t *testing.T) {
	lex := testLexicon(t)
	p := systemPrompt(lex)

	for _, c := range lex.Constructs {
		if !strings.Contains(p, c.Name) {
			t.Errorf("prompt does not mention construct %q", c.Name)
		}
		for _, a := range c.Anchors {
			if !strings.Contains(p, a.Text) {
				t.Errorf("prompt is missing anchor %q for %q", a.Text, c.Name)
			}
		}
	}
	if !strings.Contains(p, "+ ") || !strings.Contains(p, "- ") {
		t.Error("prompt does not show keying direction on anchors")
	}
	if !strings.Contains(p, "scoring text, not people") {
		t.Error("prompt lost the scoring-text-not-people framing required by SPEC-0027")
	}
	if !strings.Contains(strings.ToLower(p), "omit") {
		t.Error("prompt does not instruct the model to omit non-salient constructs")
	}
}

// TestBuildPromptCapsRunawayBodies guards the wedge: a context-length rejection
// is a transport error, which is fatal and does NOT advance the cursor, so an
// uncapped body would abort every future run at the same batch — and --reset
// would replay straight into it. The cap keeps a batch bounded no matter what
// someone pasted into a chat.
func TestBuildPromptCapsRunawayBodies(t *testing.T) {
	huge := strings.Repeat("a", 500_000)
	p := buildPrompt("Alex", []store.MessageView{
		{Hash: "h1", TS: "2023-05-01 10:00:00", Body: huge},
		{Hash: "h2", TS: "2023-05-01 10:01:00", Body: "short one"},
	})
	if len(p) > 4*maxBodyRunes {
		t.Errorf("prompt is %d bytes for one oversized message; the cap is not holding", len(p))
	}
	if !strings.Contains(p, "truncated") {
		t.Error("a truncated body is not marked as truncated")
	}
	if !strings.Contains(p, "short one") {
		t.Error("an oversized message swallowed the rest of the batch")
	}

	// Multi-byte text must not be cut mid-rune: the cap counts runes, and the
	// rendered prompt must still be valid UTF-8.
	p = buildPrompt("Alex", []store.MessageView{
		{Hash: "h1", TS: "2023-05-01 10:00:00", Body: strings.Repeat("日", maxBodyRunes+50)},
	})
	if !utf8.ValidString(p) {
		t.Error("truncation cut a multi-byte rune in half")
	}

	// A body under the cap is passed through untouched.
	p = buildPrompt("Alex", []store.MessageView{
		{Hash: "h1", TS: "2023-05-01 10:00:00", Body: "  i got the job!!  "},
	})
	if !strings.Contains(p, "i got the job!!") || strings.Contains(p, "truncated") {
		t.Errorf("a short body was altered: %q", p)
	}
}

func countRows(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM message_sentiment`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestRunContinuesAfterConversationFailure (issue #452): after the client's
// retries are exhausted, one failing conversation is recorded in Summary.Errors
// and the run continues — the surviving conversation still gets scored, and the
// failed one keeps its cursor for the next run.
func TestRunContinuesAfterConversationFailure(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	dead := seedN(t, st, source.Signal, "Failing", 8)
	alive := seedN(t, st, source.Signal, "Surviving", 4)

	opts := baseOpts()
	opts.BatchSize = 4
	opts.Concurrency = 1

	client := &fakeClient{respFn: func(p string) (string, error) {
		// Fail only batches belonging to the "Failing" conversation.
		if strings.Contains(p, "Failing") {
			return "", errors.New("simulated transport failure")
		}
		return scoreEveryMessage(p)
	}}
	sum, err := Run(ctx, st, client, opts)
	if err != nil {
		t.Fatalf("one dead conversation must not abort the run: %v", err)
	}
	if len(sum.Errors) != 1 || !strings.Contains(sum.Errors[0], "Failing") {
		t.Fatalf("Errors = %v, want exactly the failing conversation", sum.Errors)
	}
	if sum.MessagesScored != 4 {
		t.Fatalf("MessagesScored = %d, want the surviving conversation's 4", sum.MessagesScored)
	}
	// The failed conversation must have no persisted state at all: its first
	// batch failed, so nothing was written and the next run starts it from
	// the top. The surviving conversation is unaffected.
	if _, _, ok, err := st.GetSentimentState(ctx, dead); err != nil || ok {
		t.Fatalf("failed conversation must have no cursor state: ok %v err %v", ok, err)
	}
	if got, _, ok, err := st.GetSentimentState(ctx, alive); err != nil || !ok || got == "" {
		t.Fatalf("surviving conversation state: hash %q ok %v err %v", got, ok, err)
	}
}
