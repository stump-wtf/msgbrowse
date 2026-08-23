// In-app fact extraction — the controls, the guards, and the two empty states
//
// Covers what #366 added: the privileged POST gate on the Extract / Re-extract
// controls, the fixed-enum result banners, the single-flight and cross-process
// guards, the missing-model REFUSAL (the deliberate divergence from the
// journal, which builds its model-free layer anyway), the cost statement that
// has to be on the control before the click, and the contact page's split
// between "never extracted" and "extracted, found nothing".
//
// Harness idiom follows journalbuild_test.go: a fake extractor that blocks on a
// release channel so a test can hold a job "in flight" and prove the guard
// coalesces a second start.
//
// @joestump-agent 08/23/2026 - Added with the in-app extraction controls (#366).
package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// fakeFactsExtractor is a test double for the FactsExtractor seam. It never
// touches the network: RunFacts blocks on a release channel so a test can hold
// a job in flight.
type fakeFactsExtractor struct {
	model     string
	release   chan struct{}
	started   int32
	lastReset atomic.Bool
	finished  sync.WaitGroup
}

func newFakeFactsExtractor(model string) *fakeFactsExtractor {
	return &fakeFactsExtractor{model: model, release: make(chan struct{})}
}

func (f *fakeFactsExtractor) ChatModel() string { return f.model }

func (f *fakeFactsExtractor) RunFacts(ctx context.Context, reset bool) error {
	atomic.AddInt32(&f.started, 1)
	f.lastReset.Store(reset)
	<-f.release
	f.finished.Done()
	return nil
}

func (f *fakeFactsExtractor) starts() int { return int(atomic.LoadInt32(&f.started)) }

// factsPOST issues a privileged POST to a /facts/* route with the given origin
// and token.
func factsPOST(t *testing.T, srv *Server, path, origin, token string) *httptest.ResponseRecorder {
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

// TestFactsPOSTRequiresToken: both extraction POSTs are privileged. Without a
// token they 403 and — critically — no job starts, so no billable LLM call is
// made.
func TestFactsPOSTRequiresToken(t *testing.T) {
	for _, path := range []string{"/facts/run", "/facts/reset"} {
		t.Run(path, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			fe := newFakeFactsExtractor("test-chat")
			srv.SetFactsExtractor(fe)

			rec := factsPOST(t, srv, path, selfOrigin, "")
			if rec.Code != http.StatusForbidden {
				t.Errorf("tokenless POST status = %d, want 403", rec.Code)
			}
			if fe.starts() != 0 {
				t.Errorf("a rejected POST started %d jobs; it must start none", fe.starts())
			}
		})
	}
}

// TestFactsPOSTRejectsCrossOrigin: a cross-origin POST is refused before any
// job starts, even with a valid token.
func TestFactsPOSTRejectsCrossOrigin(t *testing.T) {
	for _, path := range []string{"/facts/run", "/facts/reset"} {
		t.Run(path, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			fe := newFakeFactsExtractor("test-chat")
			srv.SetFactsExtractor(fe)

			rec := factsPOST(t, srv, path, "http://evil.example", mintToken(t, srv))
			if rec.Code != http.StatusForbidden {
				t.Errorf("cross-origin POST status = %d, want 403", rec.Code)
			}
			if fe.starts() != 0 {
				t.Errorf("cross-origin POST started %d jobs; it must start none", fe.starts())
			}
		})
	}
}

// --- Starting jobs --------------------------------------------------------

// TestFactsRunStartsIncrementalJob: the Extract control resumes from the stored
// cursors — it must never pass reset, which would discard work already paid for
// (REQ-0005-004).
func TestFactsRunStartsIncrementalJob(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fe := newFakeFactsExtractor("test-chat")
	fe.finished.Add(1)
	srv.SetFactsExtractor(fe)

	rec := factsPOST(t, srv, "/facts/run", selfOrigin, mintToken(t, srv))
	if rec.Code != http.StatusOK {
		t.Fatalf("run POST status = %d, want 200", rec.Code)
	}
	if !contains(rec.Body.String(), "Extracting contact facts") {
		t.Errorf("missing the started banner:\n%s", rec.Body.String())
	}
	waitFor(t, func() bool { return fe.starts() == 1 })
	if fe.lastReset.Load() {
		t.Error("Extract must not reset; it resumes from each conversation's cursor")
	}
	close(fe.release)
	fe.finished.Wait()
}

func TestFactsResetStartsResetJob(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fe := newFakeFactsExtractor("test-chat")
	fe.finished.Add(1)
	srv.SetFactsExtractor(fe)

	rec := factsPOST(t, srv, "/facts/reset", selfOrigin, mintToken(t, srv))
	if !contains(rec.Body.String(), "Re-extracting every conversation") {
		t.Errorf("missing the reset banner:\n%s", rec.Body.String())
	}
	waitFor(t, func() bool { return fe.starts() == 1 })
	if !fe.lastReset.Load() {
		t.Error("Re-extract must pass reset=true")
	}
	close(fe.release)
	fe.finished.Wait()
}

// TestFactsNoModelRefuses documents the deliberate divergence from
// startJournal: with no chat model NOTHING starts. Every fact is model output,
// so unlike the journal there is no useful model-free layer to fall back on —
// and a reset that cleared every stored fact before discovering it had no model
// would leave the archive emptier than it found it.
func TestFactsNoModelRefuses(t *testing.T) {
	for _, path := range []string{"/facts/run", "/facts/reset"} {
		t.Run(path, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			fe := newFakeFactsExtractor("")
			srv.SetFactsExtractor(fe)

			rec := factsPOST(t, srv, path, selfOrigin, mintToken(t, srv))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 with a refusal banner", rec.Code)
			}
			if !contains(rec.Body.String(), "No chat model is configured") {
				t.Errorf("missing the no-model banner:\n%s", rec.Body.String())
			}
			if fe.starts() != 0 {
				t.Errorf("started %d jobs with no model; it must start none", fe.starts())
			}
		})
	}
}

// TestFactsUnavailableReportsItself: with no extractor wired the POST reports
// itself unavailable rather than 500ing or silently doing nothing.
func TestFactsUnavailableReportsItself(t *testing.T) {
	srv, _, _ := newTestServer(t) // no SetFactsExtractor
	rec := factsPOST(t, srv, "/facts/run", selfOrigin, mintToken(t, srv))
	if !contains(rec.Body.String(), "not available here") {
		t.Errorf("missing the unavailable banner:\n%s", rec.Body.String())
	}
}

// --- Single-flight and cross-process guards -------------------------------

// TestFactsSingleFlight: a second click while a job is in flight coalesces into
// "already in progress" rather than starting a duplicate run. Extraction is
// billable across the whole archive, so a raced double-start costs real money.
func TestFactsSingleFlight(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fe := newFakeFactsExtractor("test-chat")
	fe.finished.Add(1)
	srv.SetFactsExtractor(fe)

	factsPOST(t, srv, "/facts/run", selfOrigin, mintToken(t, srv))
	waitFor(t, func() bool { return fe.starts() == 1 })

	rec := factsPOST(t, srv, "/facts/run", selfOrigin, mintToken(t, srv))
	if !contains(rec.Body.String(), "already in progress") {
		t.Errorf("second POST missing the in-progress banner:\n%s", rec.Body.String())
	}
	if fe.starts() != 1 {
		t.Errorf("RunFacts invoked %d times; the guard must coalesce to 1", fe.starts())
	}
	close(fe.release)
	fe.finished.Wait()
}

// TestFactsCrossProcessGuard: a run started by a separate `msgbrowse facts`
// process (a live fact_runs heartbeat) also blocks a click — the in-memory flag
// cannot see another process. A STALE heartbeat reads as crashed, so extraction
// can still resume after a killed CLI run.
func TestFactsCrossProcessGuard(t *testing.T) {
	srv, st, _ := newTestServer(t)
	fe := newFakeFactsExtractor("test-chat")
	srv.SetFactsExtractor(fe)
	ctx := context.Background()

	id, err := st.BeginFactRun(ctx, "test-chat", store.FactScopeArchive, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rec := factsPOST(t, srv, "/facts/run", selfOrigin, mintToken(t, srv))
	if !contains(rec.Body.String(), "already in progress") {
		t.Errorf("a live cross-process run should block extraction:\n%s", rec.Body.String())
	}
	if fe.starts() != 0 {
		t.Errorf("started %d jobs against a live cross-process run; want 0", fe.starts())
	}

	// Age the heartbeat past the stale threshold: the run reads as crashed, and
	// a new extraction must be allowed to resume.
	if err := st.UpdateFactRunProgress(ctx, id, 3, 5,
		time.Now().Add(-factsRunStaleAfter-time.Minute)); err != nil {
		t.Fatal(err)
	}
	fe.finished.Add(1)
	factsPOST(t, srv, "/facts/run", selfOrigin, mintToken(t, srv))
	waitFor(t, func() bool { return fe.starts() == 1 })
	close(fe.release)
	fe.finished.Wait()
}

// --- The tab itself -------------------------------------------------------

// TestFactsTabUnavailableRendersNoControls: with no extractor wired the tab
// explains the CLI path and renders NO form and NO token — never a dead button.
func TestFactsTabUnavailableRendersNoControls(t *testing.T) {
	srv, _, _ := newTestServer(t) // no SetFactsExtractor
	body := get(t, srv, "/settings/facts").Body.String()

	if !contains(body, "unavailable in this mode") {
		t.Error("expected the unavailable explanation")
	}
	if contains(body, `action="/facts/run"`) || contains(body, `action="/facts/reset"`) {
		t.Error("no extractor is wired, so no extraction form may render")
	}
}

// TestFactsTabNoModelRendersNoControls: an extractor with no chat model renders
// the explanation and NO buttons — there is no model-free work for them to do,
// so a disabled-looking button would be a lie about what configuring the model
// would change.
func TestFactsTabNoModelRendersNoControls(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetFactsExtractor(newFakeFactsExtractor(""))
	body := get(t, srv, "/settings/facts").Body.String()

	if !contains(body, "extraction cannot run") {
		t.Error("expected the missing-model explanation on the model metric")
	}
	if contains(body, `action="/facts/run"`) {
		t.Error("controls rendered with no chat model configured")
	}
}

// TestFactsTabStatesCostBeforeClick: extraction is billable outbound LLM work
// over thousands of conversations, so the SCOPE has to be on the control at the
// moment of the decision — the "Rebuild all N digests" contract. This asserts
// the count reaches the button label, not merely a footnote.
func TestFactsTabStatesCostBeforeClick(t *testing.T) {
	srv, st, _ := newTestServer(t)
	srv.SetFactsExtractor(newFakeFactsExtractor("test-chat"))

	cov, err := st.FactCoverage(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Conversations == 0 {
		t.Fatal("fixture archive has no eligible conversations; the assertion below is vacuous")
	}
	body := get(t, srv, "/settings/facts").Body.String()
	want := "Extract facts from " + commaInt(int64(cov.Remaining())) + " conversations"
	if !contains(body, want) {
		t.Errorf("control does not state its scope before the click; want %q", want)
	}
	if !contains(body, "each one costs at least") {
		t.Error("missing the egress cost statement")
	}
}

// TestFactsTabShowsRunHistoryLabelledFacts: the run history renders through the
// SHARED pipeline_run_history define (SPEC-0004 REQ-0004-010) with this
// pipeline's own unit and scope labels, rather than a fourth hand-maintained
// copy of the table.
func TestFactsTabShowsRunHistoryLabelledFacts(t *testing.T) {
	srv, st, _ := newTestServer(t)
	srv.SetFactsExtractor(newFakeFactsExtractor("test-chat"))
	ctx := context.Background()

	fin := time.Now()
	id, err := st.BeginFactRun(ctx, "test-chat", store.FactScopeReset, fin.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishFactRun(ctx, store.FactRun{
		ID: id, FinishedAt: fin, DurationMS: 4200, Conversations: 12, FactsAdded: 9, Batches: 3,
	}); err != nil {
		t.Fatal(err)
	}

	body := get(t, srv, "/settings/facts").Body.String()
	for _, want := range []string{"Recent runs", ">Facts<", "Completed", "Reset &amp; re-extract"} {
		if !contains(body, want) {
			t.Errorf("facts run history missing %q", want)
		}
	}
	if contains(body, ">Digested<") || contains(body, ">Embedded<") {
		t.Error("facts history wears another pipeline's unit label")
	}
}

// TestFactsProgressFragmentIsCardOnly: the live-refresh endpoint returns JUST
// the card (no document shell), and it mints a token only when the controls
// render enabled — a 2s poll that minted every tick would evict the tokens
// armed on other open pages.
func TestFactsProgressFragmentIsCardOnly(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fe := newFakeFactsExtractor("test-chat")
	srv.SetFactsExtractor(fe)

	idle := get(t, srv, "/facts/run/progress")
	if idle.Code != http.StatusOK {
		t.Fatalf("progress status = %d", idle.Code)
	}
	body := idle.Body.String()
	if !contains(body, `id="facts-build-card"`) {
		t.Error("progress fragment is not the extraction card")
	}
	if contains(strings.ToLower(body), "<html") || contains(body, `id="main-content"`) {
		t.Error("progress fragment carried the page shell")
	}
	if !contains(body, `name="setup_token"`) {
		t.Error("an idle card must arm its forms with a token")
	}

	// While a run is in flight the buttons are disabled, so no token is minted.
	fe.finished.Add(1)
	factsPOST(t, srv, "/facts/run", selfOrigin, mintToken(t, srv))
	waitFor(t, func() bool { return fe.starts() == 1 })
	busy := get(t, srv, "/facts/run/progress").Body.String()
	if !contains(busy, "hx-get=\"/facts/run/progress\"") {
		t.Error("a busy card must keep polling itself")
	}
	if !contains(busy, "disabled") {
		t.Error("a busy card must disable its controls")
	}
	close(fe.release)
	fe.finished.Wait()
}

// TestFactsTabServesBoostedPartial: the new tab honours the boosted-navigation
// contract every other Settings surface follows (SPEC-0008 REQ-0008-006).
func TestFactsTabServesBoostedPartial(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetFactsExtractor(newFakeFactsExtractor("test-chat"))
	rec := getPartial(t, srv, "/settings/facts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, `<main id="main-content"`) || !contains(body, "<title>") {
		t.Error("partial missing the swap unit")
	}
	if contains(strings.ToLower(body), "<!doctype html>") || contains(strings.ToLower(body), "<body") {
		t.Error("partial carried the full document shell")
	}
	if !contains(body, `hx-history="false"`) {
		t.Error("the token-bearing region must opt out of htmx's history cache")
	}
}

// --- The contact page's two empty states ----------------------------------

// TestContactFactEmptyStatesAreDistinct is the heart of #366 on the reading
// side: fact_state records one row per conversation the extractor LOOKED AT, so
// "never analyzed" and "analyzed, nothing durable found" are different facts
// about the archive and must read differently. Before this they were the same
// blank panel, which made a configuration problem look like a verdict about a
// person.
func TestContactFactEmptyStatesAreDistinct(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	var convID, cid int64
	if err := st.DB().QueryRow(
		`SELECT id, contact_id FROM conversations WHERE name = 'Harper'`).Scan(&convID, &cid); err != nil {
		t.Fatalf("find Harper: %v", err)
	}
	path := "/contact/" + strconv.FormatInt(cid, 10)

	// Never extracted: the empty state points at the control that would fix it.
	before := get(t, srv, path).Body.String()
	if !contains(before, "Facts have not been extracted yet") {
		t.Errorf("missing the never-extracted empty state:\n%s", before)
	}
	if !contains(before, `href="/settings/facts"`) {
		t.Error("the never-extracted state must link to the extraction control")
	}
	if contains(before, "No durable facts found") {
		t.Error("an unanalyzed contact must not read as a considered finding")
	}

	// Extracted with no facts: a real, common outcome, and a different sentence.
	var hash string
	if err := st.DB().QueryRow(
		`SELECT hash FROM messages WHERE conversation_id = ? ORDER BY ts_unix, id LIMIT 1`,
		convID).Scan(&hash); err != nil {
		t.Fatalf("find message: %v", err)
	}
	if err := st.SetFactState(ctx, convID, hash, "test-chat", 0); err != nil {
		t.Fatal(err)
	}
	after := get(t, srv, path).Body.String()
	if !contains(after, "No durable facts found") {
		t.Errorf("missing the extracted-but-empty state:\n%s", after)
	}
	if contains(after, "Facts have not been extracted yet") {
		t.Error("an analyzed contact still reads as never analyzed")
	}

	// And a contact WITH facts renders neither empty state.
	if _, err := st.PutFact(ctx, store.FactInput{
		ContactID: cid, Fact: "Has a dog named Biscuit", Category: "personal",
		Source: source.Signal, SourceMessageHash: hash, SourceTS: "2023-05-01 10:00:00",
		SourceTSUnix: 1, Model: "test-chat",
	}); err != nil {
		t.Fatal(err)
	}
	withFacts := get(t, srv, path).Body.String()
	if contains(withFacts, "No durable facts found") || contains(withFacts, "have not been extracted yet") {
		t.Error("a contact with facts still renders an empty state")
	}
	if !contains(withFacts, "Has a dog named Biscuit") {
		t.Error("the fact itself is missing")
	}
}
