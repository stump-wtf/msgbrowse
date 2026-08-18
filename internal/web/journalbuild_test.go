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

	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// fakeJournalBuilder is a test double for the JournalBuilder seam. It never
// touches the network: RunJournal blocks on a release channel so a test can hold
// a job "in flight" and prove the single-flight guard coalesces a second start.
type fakeJournalBuilder struct {
	model    string
	digestOn bool
	release  chan struct{}
	started  int32
	lastDay  atomic.Value // string: day arg of the most recent RunJournal
	lastRegn atomic.Bool
	finished sync.WaitGroup
}

func newFakeJournalBuilder(model string, digestOn bool) *fakeJournalBuilder {
	b := &fakeJournalBuilder{model: model, digestOn: digestOn, release: make(chan struct{})}
	b.lastDay.Store("")
	return b
}

func (f *fakeJournalBuilder) ChatModel() string   { return f.model }
func (f *fakeJournalBuilder) DigestEnabled() bool { return f.digestOn }

func (f *fakeJournalBuilder) RunJournal(ctx context.Context, day string, regenerate bool) error {
	atomic.AddInt32(&f.started, 1)
	f.lastDay.Store(day)
	f.lastRegn.Store(regenerate)
	<-f.release
	f.finished.Done()
	return nil
}

func (f *fakeJournalBuilder) starts() int { return int(atomic.LoadInt32(&f.started)) }
func (f *fakeJournalBuilder) day() string { return f.lastDay.Load().(string) }

// journalPOST issues a privileged POST to a /journal/* route with the given
// origin, token and optional day field.
func journalPOST(t *testing.T, srv *Server, path, origin, token, day string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	if token != "" {
		form.Set(setupTokenField, token)
	}
	if day != "" {
		form.Set("day", day)
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

// seedJournalDay writes a mechanical day row (and optionally a digest) so the
// per-day controls have something real to target.
func seedJournalDay(t *testing.T, st *store.Store, day string, msgCount int, digestCount int) {
	t.Helper()
	ctx := context.Background()
	if err := st.PutJournalDay(ctx, store.JournalDay{
		Day: day, MessageCount: msgCount, ConversationCount: 1,
		SourceCounts: map[string]int{source.Signal: msgCount},
	}); err != nil {
		t.Fatalf("put journal day: %v", err)
	}
	if digestCount >= 0 {
		if err := st.PutDayDigest(ctx, store.JournalDigest{
			Day: day, Model: "test-chat", PromptVersion: "v1",
			Body: "a quiet day", Structured: "", Mood: "calm",
			MessageCount: digestCount,
		}); err != nil {
			t.Fatalf("put day digest: %v", err)
		}
	}
}

// --- Privileged-POST gate -------------------------------------------------

// TestJournalBuildRequiresToken: the build POSTs are privileged. Without a token
// they 403 and — critically — no job starts, so no billable LLM call is made.
func TestJournalBuildRequiresToken(t *testing.T) {
	for _, path := range []string{"/journal/build", "/journal/rebuild", "/journal/rebuild/day"} {
		t.Run(path, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			fb := newFakeJournalBuilder("test-chat", true)
			srv.SetJournalBuilder(fb)

			rec := journalPOST(t, srv, path, selfOrigin, "", "2026-06-01")
			if rec.Code != http.StatusForbidden {
				t.Errorf("tokenless POST status = %d, want 403", rec.Code)
			}
			if fb.starts() != 0 {
				t.Errorf("a rejected POST started %d jobs; it must start none", fb.starts())
			}
		})
	}
}

// TestJournalBuildRejectsCrossOrigin: a cross-origin POST is refused before any
// job starts, even with a valid token.
func TestJournalBuildRejectsCrossOrigin(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fb := newFakeJournalBuilder("test-chat", true)
	srv.SetJournalBuilder(fb)

	rec := journalPOST(t, srv, "/journal/build", "http://evil.example", mintToken(t, srv), "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin POST status = %d, want 403", rec.Code)
	}
	if fb.starts() != 0 {
		t.Errorf("cross-origin POST started %d jobs; it must start none", fb.starts())
	}
}

// --- Starting jobs --------------------------------------------------------

func TestJournalBuildStartsJob(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fb := newFakeJournalBuilder("test-chat", true)
	fb.finished.Add(1)
	srv.SetJournalBuilder(fb)

	rec := journalPOST(t, srv, "/journal/build", selfOrigin, mintToken(t, srv), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("build POST status = %d, want 200", rec.Code)
	}
	if !contains(rec.Body.String(), "Building the journal") {
		t.Errorf("missing the started banner:\n%s", rec.Body.String())
	}
	waitFor(t, func() bool { return fb.starts() == 1 })
	if fb.day() != "" {
		t.Errorf("Build ran with day=%q, want the whole archive", fb.day())
	}
	if fb.lastRegn.Load() {
		t.Error("Build must not regenerate; it fills in what is missing")
	}
	close(fb.release)
	fb.finished.Wait()
}

func TestJournalRebuildAllRegenerates(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fb := newFakeJournalBuilder("test-chat", true)
	fb.finished.Add(1)
	srv.SetJournalBuilder(fb)

	rec := journalPOST(t, srv, "/journal/rebuild", selfOrigin, mintToken(t, srv), "")
	if !contains(rec.Body.String(), "Rebuilding every digest") {
		t.Errorf("missing the rebuild banner:\n%s", rec.Body.String())
	}
	waitFor(t, func() bool { return fb.starts() == 1 })
	if !fb.lastRegn.Load() {
		t.Error("Rebuild all must pass regenerate=true")
	}
	close(fb.release)
	fb.finished.Wait()
}

// TestJournalBuildNoModelStillBuilds documents the deliberate divergence from
// startReindex: an unset chat model does NOT refuse the job, because the
// mechanical day layer is real, egress-free work (REQ-0016-001). The banner
// explains that only day counts will appear.
func TestJournalBuildNoModelStillBuilds(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fb := newFakeJournalBuilder("", true)
	fb.finished.Add(1)
	srv.SetJournalBuilder(fb)

	rec := journalPOST(t, srv, "/journal/build", selfOrigin, mintToken(t, srv), "")
	if !contains(rec.Body.String(), "Building day counts only") {
		t.Errorf("missing the no-model banner:\n%s", rec.Body.String())
	}
	waitFor(t, func() bool { return fb.starts() == 1 })
	close(fb.release)
	fb.finished.Wait()
}

// --- Single-flight guard --------------------------------------------------

// TestJournalBuildSingleFlight: a second click while a job is in flight
// coalesces into "already in progress" rather than starting a duplicate run.
// Digests are billable, so a raced double-start costs real money.
func TestJournalBuildSingleFlight(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fb := newFakeJournalBuilder("test-chat", true)
	fb.finished.Add(1)
	srv.SetJournalBuilder(fb)

	journalPOST(t, srv, "/journal/build", selfOrigin, mintToken(t, srv), "")
	waitFor(t, func() bool { return fb.starts() == 1 })

	rec := journalPOST(t, srv, "/journal/build", selfOrigin, mintToken(t, srv), "")
	if !contains(rec.Body.String(), "already in progress") {
		t.Errorf("second POST missing the in-progress banner:\n%s", rec.Body.String())
	}
	if fb.starts() != 1 {
		t.Errorf("RunJournal invoked %d times; the guard must coalesce to 1", fb.starts())
	}
	close(fb.release)
	fb.finished.Wait()
}

// TestJournalBuildCrossProcessGuard: a run started by a separate `msgbrowse
// journal` process (a live journal_runs heartbeat) also blocks a build — the
// in-memory flag cannot see another process. A STALE heartbeat reads as crashed,
// so a build can still resume after a killed CLI run.
func TestJournalBuildCrossProcessGuard(t *testing.T) {
	srv, st, _ := newTestServer(t)
	fb := newFakeJournalBuilder("test-chat", true)
	srv.SetJournalBuilder(fb)
	ctx := context.Background()

	// A fresh in-flight run from "another process".
	id, err := st.BeginJournalRun(ctx, "test-chat", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rec := journalPOST(t, srv, "/journal/build", selfOrigin, mintToken(t, srv), "")
	if !contains(rec.Body.String(), "already in progress") {
		t.Errorf("a live cross-process run should block a build:\n%s", rec.Body.String())
	}
	if fb.starts() != 0 {
		t.Errorf("started %d jobs against a live cross-process run; want 0", fb.starts())
	}

	// Age the heartbeat past the stale threshold: the run reads as crashed, and a
	// build must be allowed to resume.
	if err := st.UpdateJournalRunProgress(ctx, id, 3, 3,
		time.Now().Add(-journalRunStaleAfter-time.Minute)); err != nil {
		t.Fatal(err)
	}
	fb.finished.Add(1)
	journalPOST(t, srv, "/journal/build", selfOrigin, mintToken(t, srv), "")
	waitFor(t, func() bool { return fb.starts() == 1 })
	close(fb.release)
	fb.finished.Wait()
}

// --- Per-day rebuild validation -------------------------------------------

// TestJournalRebuildDayRejectsBadDay: the per-day target must both parse AND
// exist in journal_days, so a typo or a hand-crafted POST can never spawn a job
// over an arbitrary range.
func TestJournalRebuildDayRejectsBadDay(t *testing.T) {
	for _, day := range []string{"", "not-a-day", "2019-13-45", "2026-06-01' OR 1=1--", "1999-01-01"} {
		t.Run("day="+day, func(t *testing.T) {
			srv, st, _ := newTestServer(t)
			fb := newFakeJournalBuilder("test-chat", true)
			srv.SetJournalBuilder(fb)
			seedJournalDay(t, st, "2026-06-01", 12, -1) // a real day, none of the above

			rec := journalPOST(t, srv, "/journal/rebuild/day", selfOrigin, mintToken(t, srv), day)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 with a rejection banner", rec.Code)
			}
			if !contains(rec.Body.String(), "not in the journal") {
				t.Errorf("missing the bad-day banner for %q", day)
			}
			if fb.starts() != 0 {
				t.Errorf("day %q started %d jobs; it must start none", day, fb.starts())
			}
		})
	}
}

func TestJournalRebuildDayStartsForKnownDay(t *testing.T) {
	srv, st, _ := newTestServer(t)
	fb := newFakeJournalBuilder("test-chat", true)
	fb.finished.Add(1)
	srv.SetJournalBuilder(fb)
	seedJournalDay(t, st, "2026-06-01", 12, 8)

	rec := journalPOST(t, srv, "/journal/rebuild/day", selfOrigin, mintToken(t, srv), "2026-06-01")
	if !contains(rec.Body.String(), "Rebuilding that day") {
		t.Errorf("missing the per-day banner:\n%s", rec.Body.String())
	}
	waitFor(t, func() bool { return fb.starts() == 1 })
	if fb.day() != "2026-06-01" {
		t.Errorf("RunJournal day = %q, want 2026-06-01", fb.day())
	}
	if !fb.lastRegn.Load() {
		t.Error("a per-day rebuild must regenerate that day's digest")
	}
	close(fb.release)
	fb.finished.Wait()
}

// --- Unavailable mode -----------------------------------------------------

// TestJournalBuildUnavailableRendersNoControls: with no builder wired the
// Status page (where the build card now lives) explains the CLI path and
// renders NO form and NO token — never a dead button. The Journal page
// renders no build machinery at all.
func TestJournalBuildUnavailableRendersNoControls(t *testing.T) {
	srv, _, _ := newTestServer(t) // no SetJournalBuilder
	body := get(t, srv, "/status").Body.String()

	if !contains(body, "unavailable in this mode") {
		t.Error("expected the unavailable explanation")
	}
	if contains(body, `action="/journal/build"`) {
		t.Error("no builder is wired, so no build form may render")
	}
	if jbody := get(t, srv, "/journal").Body.String(); contains(jbody, "unavailable in this mode") || contains(jbody, setupTokenField) {
		t.Error("the journal page must stay free of build machinery")
	}
	if contains(body, setupTokenField) {
		t.Error("no token may be minted into a page that renders no privileged form")
	}
}

// TestJournalBuildUnavailablePOSTStartsNothing: a direct POST in that mode is
// still gated and reports itself rather than 500ing.
func TestJournalBuildUnavailablePOSTStartsNothing(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := journalPOST(t, srv, "/journal/build", selfOrigin, mintToken(t, srv), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !contains(rec.Body.String(), "not available here") {
		t.Errorf("missing the unavailable banner:\n%s", rec.Body.String())
	}
}

// --- Page contract --------------------------------------------------------

// TestStatusPageOptsOutOfHistory: the Status page renders the journal build
// forms with a live per-session token, so htmx must not snapshot it into
// localStorage. (The Journal page no longer renders any privileged form.)
func TestStatusPageOptsOutOfHistory(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetJournalBuilder(newFakeJournalBuilder("test-chat", true))
	if !contains(get(t, srv, "/status").Body.String(), `hx-history="false"`) {
		t.Error("a page rendering a live setup token must opt out of the htmx history cache")
	}
	if contains(get(t, srv, "/journal").Body.String(), setupTokenField) {
		t.Error("the journal page must not mint a setup token (no privileged forms there)")
	}
}

// TestJournalStaleDayCard: a digest written before later messages landed is
// marked out of date and offers a per-day rebuild; a current digest is not.
func TestJournalStaleDayCard(t *testing.T) {
	srv, st, _ := newTestServer(t)
	srv.SetJournalBuilder(newFakeJournalBuilder("test-chat", true))
	seedJournalDay(t, st, "2026-06-02", 40, 12) // 28 messages arrived since

	body := get(t, srv, "/journal?day=2026-06-02").Body.String()
	if !contains(body, "Out of date") {
		t.Error("a digest whose day has grown should be marked out of date")
	}
	if !contains(body, "28 more messages") {
		t.Errorf("expected the new-message count in the staleness note:\n%s", body)
	}
	if contains(body, `action="/journal/rebuild/day"`) {
		t.Error("the journal day card no longer carries a rebuild form (controls live on Status)")
	}
	if !contains(body, "/status") {
		t.Error("the stale note should point at the Status build surface")
	}
}

func TestJournalCurrentDayCardNotStale(t *testing.T) {
	srv, st, _ := newTestServer(t)
	srv.SetJournalBuilder(newFakeJournalBuilder("test-chat", true))
	seedJournalDay(t, st, "2026-06-03", 12, 12) // count unchanged since the digest

	if contains(get(t, srv, "/journal?day=2026-06-03").Body.String(), "Out of date") {
		t.Error("a digest matching the day's current count is not stale")
	}
}

// TestJournalLegacyDigestNotStale is the load-bearing case for the staleness
// rule: a digest written before the message_count column existed records 0,
// which means UNKNOWN. Reading that as "zero messages" would light up every
// pre-v15 digest as out of date the moment the column shipped.
func TestJournalLegacyDigestNotStale(t *testing.T) {
	srv, st, _ := newTestServer(t)
	srv.SetJournalBuilder(newFakeJournalBuilder("test-chat", true))
	seedJournalDay(t, st, "2026-06-04", 40, 0) // 0 = unknown, not "zero messages"

	if contains(get(t, srv, "/journal?day=2026-06-04").Body.String(), "Out of date") {
		t.Error("a legacy digest with an unknown captured count must not read as stale")
	}
}

// TestJournalDigestTextStaysEscaped: digest prose is LLM output derived from
// message content — the highest-value injection sink on the page.
func TestJournalDigestTextStaysEscaped(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	if err := st.PutJournalDay(ctx, store.JournalDay{
		Day: "2026-06-05", MessageCount: 3, ConversationCount: 1,
		SourceCounts: map[string]int{source.Signal: 3},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutDayDigest(ctx, store.JournalDigest{
		Day: "2026-06-05", Model: "m", PromptVersion: "v1",
		Body:         `<script>alert(1)</script>`,
		MessageCount: 3,
	}); err != nil {
		t.Fatal(err)
	}
	body := get(t, srv, "/journal?day=2026-06-05").Body.String()
	if contains(body, "<script>alert(1)</script>") {
		t.Error("digest prose reached the page unescaped")
	}
	if !contains(body, "&lt;script&gt;") {
		t.Error("expected the digest body to be html/template-escaped")
	}
}

// TestJournalRebuildAllStatesTheCount: re-running every digest is billable, so
// the count must be on the control BEFORE the click, not after.
func TestJournalRebuildAllStatesTheCount(t *testing.T) {
	srv, st, _ := newTestServer(t)
	srv.SetJournalBuilder(newFakeJournalBuilder("test-chat", true))
	for _, d := range []string{"2026-06-06", "2026-06-07", "2026-06-08"} {
		seedJournalDay(t, st, d, 5, 5)
	}
	body := get(t, srv, "/status").Body.String()
	if !contains(body, "Rebuild all 3 digests") {
		t.Errorf("the rebuild control should state how many digests it will regenerate:\n%s", body)
	}
}
