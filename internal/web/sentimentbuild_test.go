// In-app sentiment scoring — the controls, the guards, and the cost statement
//
// Covers what #367 added on the control side: the privileged POST gate on the
// Score / Rescore controls, the fixed-enum result banners, the single-flight and
// cross-process guards, the missing-model REFUSAL (the deliberate divergence
// from the journal, which builds its model-free layer anyway), and the cost
// statement that has to be on the control before the click.
//
// That last one is not decoration. #367 asserted the pass was "deterministic and
// local", "has no LLM cost and can run freely", "Egress: none" — none of which
// is true of the shipped engine: sentiment.Run refuses without a chat model and
// makes an llm.Chat call per batch, and ADR-0028 calls corpus scoring "the most
// expensive extraction". TestSentimentControlStatesItsCost is the guard that
// stops the free-and-local framing coming back the next time someone reads the
// issue instead of the code.
//
// Harness idiom follows factsbuild_test.go: a fake scorer that blocks on a
// release channel so a test can hold a job "in flight" and prove the guard
// coalesces a second start.
//
// @joestump-agent 08/23/2026 - Added with the in-app scoring controls (#367).
package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/sentiment"
	"github.com/joestump/msgbrowse/internal/store"
)

// fakeSentimentScorer is a test double for the SentimentScorer seam. It never
// touches the network: RunSentiment blocks on a release channel so a test can
// hold a job in flight.
type fakeSentimentScorer struct {
	model     string
	release   chan struct{}
	started   int32
	lastReset atomic.Bool
	finished  sync.WaitGroup
}

func newFakeSentimentScorer(model string) *fakeSentimentScorer {
	return &fakeSentimentScorer{model: model, release: make(chan struct{})}
}

func (f *fakeSentimentScorer) ChatModel() string      { return f.model }
func (f *fakeSentimentScorer) LexiconVersion() string { return sentiment.LexiconVersion }

func (f *fakeSentimentScorer) RunSentiment(ctx context.Context, reset bool) error {
	atomic.AddInt32(&f.started, 1)
	f.lastReset.Store(reset)
	<-f.release
	f.finished.Done()
	return nil
}

func (f *fakeSentimentScorer) starts() int { return int(atomic.LoadInt32(&f.started)) }

// sentimentPOST issues a privileged POST to a /sentiment/* route with the given
// origin and token.
func sentimentPOST(t *testing.T, srv *Server, path, origin, token string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	if token != "" {
		form.Set(setupTokenField, token)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// --- Privileged-POST gate -------------------------------------------------

// TestSentimentPOSTRequiresToken: both scoring POSTs are privileged. Without a
// token they 403 and — critically — no job starts, so no billable LLM call is
// made.
func TestSentimentPOSTRequiresToken(t *testing.T) {
	for _, path := range []string{"/sentiment/run", "/sentiment/reset"} {
		t.Run(path, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			sc := newFakeSentimentScorer("test-chat")
			srv.SetSentimentScorer(sc)

			rec := sentimentPOST(t, srv, path, selfOrigin, "")
			if rec.Code != http.StatusForbidden {
				t.Errorf("tokenless POST status = %d, want 403", rec.Code)
			}
			if sc.starts() != 0 {
				t.Errorf("a rejected POST started %d jobs; it must start none", sc.starts())
			}
		})
	}
}

// TestSentimentPOSTRejectsCrossOrigin: a cross-origin POST is refused before any
// job starts, even with a valid token.
func TestSentimentPOSTRejectsCrossOrigin(t *testing.T) {
	for _, path := range []string{"/sentiment/run", "/sentiment/reset"} {
		t.Run(path, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			sc := newFakeSentimentScorer("test-chat")
			srv.SetSentimentScorer(sc)

			rec := sentimentPOST(t, srv, path, "http://evil.example", mintToken(t, srv))
			if rec.Code != http.StatusForbidden {
				t.Errorf("cross-origin POST status = %d, want 403", rec.Code)
			}
			if sc.starts() != 0 {
				t.Errorf("cross-origin POST started %d jobs; it must start none", sc.starts())
			}
		})
	}
}

// --- Starting jobs --------------------------------------------------------

// TestSentimentRunStartsIncrementalJob: the Score control resumes from the
// stored cursors — it must never pass reset, which would discard work already
// paid for.
func TestSentimentRunStartsIncrementalJob(t *testing.T) {
	srv, _, _ := newTestServer(t)
	sc := newFakeSentimentScorer("test-chat")
	sc.finished.Add(1)
	srv.SetSentimentScorer(sc)

	rec := sentimentPOST(t, srv, "/sentiment/run", selfOrigin, mintToken(t, srv))
	if rec.Code != http.StatusOK {
		t.Fatalf("run POST status = %d, want 200", rec.Code)
	}
	if !contains(rec.Body.String(), "Scoring sentiment") {
		t.Errorf("missing the started banner:\n%s", rec.Body.String())
	}
	waitFor(t, func() bool { return sc.starts() == 1 })
	if sc.lastReset.Load() {
		t.Error("Score must not reset; it resumes from each conversation's cursor")
	}
	close(sc.release)
	sc.finished.Wait()
}

// TestSentimentResetStartsResetJob: the Rescore control passes reset, and its
// banner says every conversation is being paid for again.
func TestSentimentResetStartsResetJob(t *testing.T) {
	srv, _, _ := newTestServer(t)
	sc := newFakeSentimentScorer("test-chat")
	sc.finished.Add(1)
	srv.SetSentimentScorer(sc)

	rec := sentimentPOST(t, srv, "/sentiment/reset", selfOrigin, mintToken(t, srv))
	if rec.Code != http.StatusOK {
		t.Fatalf("reset POST status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "Rescoring every conversation") {
		t.Errorf("missing the reset banner:\n%s", body)
	}
	// The reset banner must promise the opt-outs survive: store.ResetSentiment
	// deliberately keeps them, and a banner that did not say so would read as a
	// privacy reset too.
	if !contains(body, "opted out stay opted out") {
		t.Error("the reset banner does not say opt-outs are preserved")
	}
	waitFor(t, func() bool { return sc.starts() == 1 })
	if !sc.lastReset.Load() {
		t.Error("Rescore must pass reset")
	}
	close(sc.release)
	sc.finished.Wait()
}

// --- Guards ---------------------------------------------------------------

// TestSentimentSecondClickCoalesces: the single-flight guard turns a second
// click during a live run into the "already in progress" banner rather than a
// second (billable) writer.
func TestSentimentSecondClickCoalesces(t *testing.T) {
	srv, _, _ := newTestServer(t)
	sc := newFakeSentimentScorer("test-chat")
	sc.finished.Add(1)
	srv.SetSentimentScorer(sc)

	if rec := sentimentPOST(t, srv, "/sentiment/run", selfOrigin, mintToken(t, srv)); rec.Code != 200 {
		t.Fatalf("first POST status = %d", rec.Code)
	}
	waitFor(t, func() bool { return sc.starts() == 1 })

	rec := sentimentPOST(t, srv, "/sentiment/run", selfOrigin, mintToken(t, srv))
	if !contains(rec.Body.String(), "A run is already in progress") {
		t.Errorf("second click did not coalesce:\n%s", rec.Body.String())
	}
	if sc.starts() != 1 {
		t.Errorf("second click started another job (%d total)", sc.starts())
	}
	close(sc.release)
	sc.finished.Wait()
}

// TestSentimentRefusesWhileCLIRunInFlight: the cross-process guard. A
// `msgbrowse sentiment` CLI against the same SQLite file is visible ONLY through
// the sentiment_runs heartbeat, and a direct POST would otherwise sail past the
// in-memory flag and start a second billable writer.
func TestSentimentRefusesWhileCLIRunInFlight(t *testing.T) {
	srv, st, _ := newTestServer(t)
	sc := newFakeSentimentScorer("test-chat")
	srv.SetSentimentScorer(sc)

	// An unfinished row with a fresh heartbeat: exactly what a live CLI run
	// looks like from this process.
	if _, err := st.BeginSentimentRun(context.Background(), "test-chat",
		sentiment.LexiconVersion, store.SentimentScopeArchive, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec := sentimentPOST(t, srv, "/sentiment/run", selfOrigin, mintToken(t, srv))
	if !contains(rec.Body.String(), "A run is already in progress") {
		t.Errorf("a live CLI run did not block a web start:\n%s", rec.Body.String())
	}
	if sc.starts() != 0 {
		t.Errorf("started %d jobs alongside a live CLI run; it must start none", sc.starts())
	}
}

// TestSentimentStaleRunDoesNotBlock: a crashed run whose heartbeat went cold
// reads as interrupted, not live, so scoring can resume after a killed CLI run.
func TestSentimentStaleRunDoesNotBlock(t *testing.T) {
	srv, st, _ := newTestServer(t)
	sc := newFakeSentimentScorer("test-chat")
	sc.finished.Add(1)
	srv.SetSentimentScorer(sc)

	if _, err := st.BeginSentimentRun(context.Background(), "test-chat", sentiment.LexiconVersion,
		store.SentimentScopeArchive, time.Now().Add(-2*sentimentRunStaleAfter)); err != nil {
		t.Fatal(err)
	}
	rec := sentimentPOST(t, srv, "/sentiment/run", selfOrigin, mintToken(t, srv))
	if !contains(rec.Body.String(), "Scoring sentiment") {
		t.Errorf("a stale run blocked a new start:\n%s", rec.Body.String())
	}
	waitFor(t, func() bool { return sc.starts() == 1 })
	close(sc.release)
	sc.finished.Wait()
}

// TestSentimentRefusesWithoutChatModel: the deliberate divergence from the
// journal. Scoring has NO model-free layer — every score is the model's output —
// so a run without one is refused outright rather than started and reported as
// having run.
func TestSentimentRefusesWithoutChatModel(t *testing.T) {
	for _, path := range []string{"/sentiment/run", "/sentiment/reset"} {
		t.Run(path, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			sc := newFakeSentimentScorer("") // no chat model configured
			srv.SetSentimentScorer(sc)

			rec := sentimentPOST(t, srv, path, selfOrigin, mintToken(t, srv))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			body := rec.Body.String()
			if !contains(body, "No chat model is configured") {
				t.Errorf("missing the refusal banner:\n%s", body)
			}
			if !contains(body, "Nothing was started") {
				t.Error("the refusal banner does not say the job was NOT started")
			}
			if sc.starts() != 0 {
				t.Errorf("started %d jobs with no model; a reset would have wiped every score first", sc.starts())
			}
		})
	}
}

// TestSentimentUnavailableWithoutScorer: browser / no-op mode wires no scorer.
// The POST reports itself unavailable and the tab renders the CLI path instead
// of a dead button.
func TestSentimentUnavailableWithoutScorer(t *testing.T) {
	srv, _, _ := newTestServer(t)

	rec := sentimentPOST(t, srv, "/sentiment/run", selfOrigin, mintToken(t, srv))
	if !contains(rec.Body.String(), "Sentiment scoring is not available here") {
		t.Errorf("missing the unavailable banner:\n%s", rec.Body.String())
	}
	body := get(t, srv, "/settings/sentiment").Body.String()
	if !contains(body, "msgbrowse sentiment") {
		t.Error("the unavailable card does not point at the CLI")
	}
	if contains(body, `action="/sentiment/run"`) {
		t.Error("the unavailable card rendered a form it cannot drive")
	}
}

// --- The tab itself -------------------------------------------------------

// TestSentimentTabRendersCoverageAndGeneration: the tab states what #367 found
// unanswerable — how much has been scored, and under which generation. The
// generation is not trivia: every consumer surface filters on
// (model, lexicon_version), so a card reporting only a model could not explain
// why scores vanished after a curation bump.
func TestSentimentTabRendersCoverageAndGeneration(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetSentimentScorer(newFakeSentimentScorer("test-chat"))

	body := get(t, srv, "/settings/sentiment").Body.String()
	for _, want := range []string{
		"Scoring coverage", "Generation", "test-chat", "lexicon " + sentiment.LexiconVersion,
		"Scores stored", "Last run", "never",
	} {
		if !contains(body, want) {
			t.Errorf("sentiment tab missing %q", want)
		}
	}
}

// TestSentimentControlStatesItsCost: the guard against the free-and-local
// framing coming back.
//
// #367 said the pass "has no LLM cost and can run freely" and listed
// "Egress: none". The engine refuses without a chat model and makes an llm.Chat
// call per batch; ADR-0028 rejected the local-lexicon alternative and calls
// corpus scoring "the most expensive extraction". So the control must state the
// price BEFORE the click — the "Rebuild all N digests" contract — and must never
// describe scoring as free, local, or egress-free.
func TestSentimentControlStatesItsCost(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetSentimentScorer(newFakeSentimentScorer("test-chat"))

	body := get(t, srv, "/settings/sentiment").Body.String()
	for _, want := range []string{"LLM endpoint", "costs at least", "most expensive pass"} {
		if !contains(body, want) {
			t.Errorf("the scoring control does not state its cost: missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"no LLM cost", "run freely", "egress-free", "no egress", "runs locally", "deterministic and local",
	} {
		if contains(strings.ToLower(body), strings.ToLower(forbidden)) {
			t.Errorf("the sentiment tab describes scoring as free/local (%q) — "+
				"sentiment.Run makes one llm.Chat call per batch and ADR-0028 calls it "+
				"the most expensive extraction", forbidden)
		}
	}
}

// TestSentimentTabCarriesUncertaintyFraming: SPEC-0027's uncertainty
// requirement reaches the pipeline tab too, not only the profile. A control that
// invited someone to score a whole archive without saying what the numbers are
// (and are not) is where the clinical reading starts.
func TestSentimentTabCarriesUncertaintyFraming(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetSentimentScorer(newFakeSentimentScorer("test-chat"))

	body := get(t, srv, "/settings/sentiment").Body.String()
	for _, want := range []string{"not a psychological assessment", "opt out"} {
		if !contains(body, want) {
			t.Errorf("sentiment tab missing the uncertainty framing %q", want)
		}
	}
}

// TestSentimentTabServesBoostedPartial: the tab honours the boosted-navigation
// contract every other Settings surface follows (SPEC-0008 REQ-0008-006), and
// keeps its per-session token out of htmx's history cache.
func TestSentimentTabServesBoostedPartial(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetSentimentScorer(newFakeSentimentScorer("test-chat"))

	rec := getPartial(t, srv, "/settings/sentiment")
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, `<main id="main-content"`) || !contains(body, "<title>") {
		t.Error("partial missing the swap target or the title htmx lifts into history")
	}
	if contains(body, "<!DOCTYPE html>") || contains(body, "<body") {
		t.Error("partial carried the full document shell")
	}
	if !contains(body, `hx-history="false"`) {
		t.Error("partial missing hx-history=\"false\" on the token-bearing region")
	}
}

// TestSentimentRunHistoryLabelsScored: the pipeline joins the ONE shared
// run-history define rather than copying the table a fourth time (SPEC-0004
// REQ-0004-010), labelling its count column with its own unit.
func TestSentimentRunHistoryLabelsScored(t *testing.T) {
	srv, st, _ := newTestServer(t)
	srv.SetSentimentScorer(newFakeSentimentScorer("test-chat"))
	ctx := context.Background()

	fin := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	id, err := st.BeginSentimentRun(ctx, "test-chat", sentiment.LexiconVersion,
		store.SentimentScopeReset, fin.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishSentimentRun(ctx, store.SentimentRun{
		ID: id, FinishedAt: fin, DurationMS: 900, ScoresWritten: 42,
	}); err != nil {
		t.Fatal(err)
	}

	body := get(t, srv, "/settings/sentiment").Body.String()
	for _, want := range []string{"Recent runs", ">Scored<", "Completed", "Reset &amp; rescore", "42"} {
		if !contains(body, want) {
			t.Errorf("sentiment run history missing %q", want)
		}
	}
	// The scope column renders a mapped label, never the stored token.
	if contains(body, ">reset<") {
		t.Error("the raw scope token reached the rendered table")
	}
}

// TestSentimentRunHistoryEscapesErrors: run error strings are endpoint output,
// not trusted markup.
func TestSentimentRunHistoryEscapesErrors(t *testing.T) {
	srv, st, _ := newTestServer(t)
	srv.SetSentimentScorer(newFakeSentimentScorer("test-chat"))
	ctx := context.Background()

	id, err := st.BeginSentimentRun(ctx, "test-chat", sentiment.LexiconVersion,
		store.SentimentScopeArchive, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishSentimentRun(ctx, store.SentimentRun{
		ID: id, FinishedAt: time.Now(), Error: `<script>alert(1)</script>`,
	}); err != nil {
		t.Fatal(err)
	}
	body := get(t, srv, "/settings/sentiment").Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("a run error string reached the page as live markup")
	}
	if !contains(body, "&lt;script&gt;") {
		t.Error("the escaped run error is missing entirely")
	}
}

// TestSentimentProgressFragmentIsCardOnly: the 2s poll re-renders JUST the card,
// so the "N conversations scored so far" line actually moves.
func TestSentimentProgressFragmentIsCardOnly(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetSentimentScorer(newFakeSentimentScorer("test-chat"))

	rec := get(t, srv, "/sentiment/run/progress")
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, `id="sentiment-build-card"`) {
		t.Error("progress fragment is not the card")
	}
	if contains(body, "<main") || contains(body, "settings-subnav") {
		t.Error("progress fragment carried the page shell")
	}
}
