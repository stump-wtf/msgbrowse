package web

// The calendar's mood-coverage contract (#370).
//
// The headline test here is TestJournalNoDayWithMessagesRendersBare, and it is
// the one that would have caught the reported bug: before this change a day with
// messages and no digest emitted `class="cal-day"` and nothing else, which is not
// a designed state — it is the absence of one, and it read as "unremarkable" on
// 329 days that had simply never been analysed.
//
// The rest defend the properties that make that fix mean something: the legend
// names every state the grid can produce, the two mood SOURCES stay
// distinguishable, and the affect-valence table cannot silently drift away from
// internal/sentiment's lexicon.

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/sentiment"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// sentimentGenV1 is the generation the test fixtures score under. It only has to
// be internally consistent — nothing here depends on a real model name.
var sentimentGenV1 = store.SentimentGeneration{Model: "test-model", LexiconVersion: sentiment.LexiconVersion}

// upbeatScores / tenseScores are construct sets that clear sentimentMinScores
// (3 rows) and land either side of sentimentMoodThreshold. Written as fixtures
// rather than inline so a threshold change breaks one place, not five.
var (
	upbeatScores = map[string]float64{"Cheerfulness": 0.8, "Hope/Optimism": 0.7, "Empathy": 0.6}
	tenseScores  = map[string]float64{"Anger": 0.8, "Anxiety": 0.7, "Depression": 0.6}
)

// seedSentimentForDay scores every message the conversation has on `day`,
// through the real PutSentimentBatch so the sentiment_state cursor is written
// too — LatestSentimentGeneration reads that cursor, so a test that inserted
// rows behind its back would score days the page then refuses to tint.
func seedSentimentForDay(t *testing.T, st *store.Store, convID int64, day string, scores map[string]float64) {
	t.Helper()
	rows, err := st.DB().Query(
		`SELECT hash, ts_unix FROM messages WHERE conversation_id = ? AND date(ts_unix,'unixepoch') = ? ORDER BY ts_unix, id`,
		convID, day)
	if err != nil {
		t.Fatalf("read messages for %s: %v", day, err)
	}
	defer rows.Close()

	var cid int64
	if err := st.DB().QueryRow(`SELECT contact_id FROM conversations WHERE id = ?`, convID).Scan(&cid); err != nil {
		t.Fatalf("contact id: %v", err)
	}

	// Deterministic construct order so the same fixture produces the same rows
	// on every run.
	names := make([]string, 0, len(scores))
	for name := range scores {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []store.SentimentScore
	var last string
	for rows.Next() {
		var hash string
		var tsUnix int64
		if err := rows.Scan(&hash, &tsUnix); err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			out = append(out, store.SentimentScore{
				MessageHash: hash, Construct: name, Score: scores[name], TSUnix: tsUnix, ContactID: cid,
			})
		}
		last = hash
	}
	if len(out) == 0 {
		t.Fatalf("no messages on %s to score", day)
	}
	if err := st.PutSentimentBatch(context.Background(), convID, sentimentGenV1, last, out); err != nil {
		t.Fatalf("PutSentimentBatch: %v", err)
	}
}

// calDayClassRE matches a rendered day cell's class list. The `<a` restricts it
// to cells that link somewhere, i.e. days WITH messages — the population the
// acceptance criterion is about.
var calDayClassRE = regexp.MustCompile(`<a class="(cal-day[^"]*)"`)

// calDayModifierRE pulls every `cal-day--<state>` token out of a fragment.
var calDayModifierRE = regexp.MustCompile(`cal-day--([a-z]+)`)

// TestJournalNoDayWithMessagesRendersBare is the issue's headline acceptance
// criterion: every day that HAS messages carries a defined mood state. Not "most
// days", not "days we got round to" — every one, with `message_sentiment` empty,
// which is the state a real archive is in today.
func TestJournalNoDayWithMessagesRendersBare(t *testing.T) {
	srv, st := newJournalServer(t)
	seedJournalDays(t, st, "Harper", []string{
		"2023-05-01", "2023-05-02", "2023-05-03", "2023-05-15", "2023-05-28",
	})
	// One day out of five is digested — the same shape as the live archive, just
	// smaller. The other four are the bug.
	putDigest(t, st, "2023-05-01", "A calm day.", "upbeat", "")

	// Every assertion below reads the GRID region only. The legend now names the
	// same classes the cells carry — that is the point of it — so a whole-body
	// search would match the explanation instead of the thing explained.
	grid, _ := gridAndLegend(t, get(t, srv, "/journal?year=2023&month=5").Body.String())

	matches := calDayClassRE.FindAllStringSubmatch(grid, -1)
	if len(matches) != 5 {
		t.Fatalf("rendered %d linked day cells, want 5", len(matches))
	}
	// A cell is "defined" when it carries at least one state modifier that is not
	// purely interactional. cal-day--selected says which day is open; it says
	// nothing about whether the day has been analysed.
	for _, m := range matches {
		classes := m[1]
		defined := false
		for _, mod := range calDayModifierRE.FindAllStringSubmatch(classes, -1) {
			if mod[1] != "selected" {
				defined = true
				break
			}
		}
		if !defined {
			t.Errorf("day cell renders bare (class %q) — a day with messages must have a defined state", classes)
		}
	}
	// And specifically: the four undigested days are marked as never analysed,
	// not merely left uncoloured.
	if n := strings.Count(grid, "cal-day--unanalyzed"); n != 4 {
		t.Errorf("cal-day--unanalyzed appears %d times in the grid, want 4", n)
	}
}

// TestJournalLegendDocumentsEveryGridState diffs the classes the grid actually
// emits against the classes the legend explains. This is the property the old
// legend failed: it listed the four moods and nothing else, so the majority
// state on a partially-digested archive had no entry at all.
func TestJournalLegendDocumentsEveryGridState(t *testing.T) {
	srv, st := newJournalServer(t)
	// A month engineered to produce every state at once: digested, digested +
	// stale, sentiment-only, unanalysed, no-messages, and a leading blank.
	seedJournalDays(t, st, "Harper", []string{"2023-05-02", "2023-05-03", "2023-05-04", "2023-05-05"})
	putDigest(t, st, "2023-05-02", "Digested.", "quiet", "")
	staleDigest(t, st, "2023-05-03", "tense", 99)
	convID := conversationID(t, st, "Harper")
	seedSentimentForDay(t, st, convID, "2023-05-04", upbeatScores)

	body := get(t, srv, "/journal?year=2023&month=5&day=2023-05-02").Body.String()
	grid, legend := gridAndLegend(t, body)

	// Sanity: the fixture really did exercise the interesting states, otherwise
	// this test would pass vacuously the day the grid stops emitting them.
	for _, want := range []string{"cal-day--quiet", "cal-day--stale", "cal-day--inferred", "cal-day--unanalyzed", "cal-day--none"} {
		if !strings.Contains(grid, want) {
			t.Fatalf("fixture did not produce %s in the grid; the completeness check below would be vacuous", want)
		}
	}

	// cal-day--selected and cal-day--blank are interactional/structural, not
	// states of the underlying day, so they are not legend material.
	notLegendMaterial := map[string]bool{"selected": true, "blank": true}
	seen := map[string]bool{}
	for _, m := range calDayModifierRE.FindAllStringSubmatch(grid, -1) {
		state := m[1]
		if notLegendMaterial[state] || seen[state] {
			continue
		}
		seen[state] = true
		if !strings.Contains(legend, "cal-day--"+state) {
			t.Errorf("the grid renders cal-day--%s but the legend does not document it", state)
		}
	}
}

// TestJournalSentimentTintsUndigestedDays is the mixed-evidence case: with
// sentiment populated and digests partial, mood coverage is complete AND the two
// sources stay apart. The second half matters as much as the first — a calendar
// that tinted a sentiment-only day identically to a digested one would be
// claiming an editorial reading that never happened.
func TestJournalSentimentTintsUndigestedDays(t *testing.T) {
	srv, st := newJournalServer(t)
	seedJournalDays(t, st, "Harper", []string{"2023-05-01", "2023-05-02", "2023-05-03"})
	putDigest(t, st, "2023-05-01", "Digested and editorialized.", "quiet", "")
	convID := conversationID(t, st, "Harper")
	seedSentimentForDay(t, st, convID, "2023-05-02", upbeatScores)
	seedSentimentForDay(t, st, convID, "2023-05-03", tenseScores)

	grid, _ := gridAndLegend(t, get(t, srv, "/journal?year=2023&month=5").Body.String())

	// 100% mood coverage: nothing is left unanalysed.
	if strings.Contains(grid, "cal-day--unanalyzed") {
		t.Error("a day still renders unanalysed even though every day has a digest or sentiment")
	}
	// The sentiment fold produced the moods the scores imply, in the right
	// direction — an upbeat set must not come out tense.
	if !strings.Contains(grid, "cal-day--upbeat") || !strings.Contains(grid, "cal-day--tense") {
		t.Error("sentiment-derived upbeat/tense tints missing from the grid")
	}
	// Digest-backed vs sentiment-only: the two scored days are marked inferred,
	// and the digested day is not.
	if n := strings.Count(grid, "cal-day--inferred"); n != 2 {
		t.Errorf("cal-day--inferred appears %d times in the grid, want 2", n)
	}
	for _, m := range calDayClassRE.FindAllStringSubmatch(grid, -1) {
		if strings.Contains(m[1], "cal-day--quiet") && strings.Contains(m[1], "cal-day--inferred") {
			t.Errorf("the digested day is marked inferred (class %q) — digest moods must not be labelled as sentiment", m[1])
		}
	}
}

// TestJournalSentimentHonorsOptOut: an opted-out contact's messages must not
// colour a day, and the day must fall back to the honest UNANALYSED state rather
// than keeping a tint derived from evidence that is no longer allowed to speak.
func TestJournalSentimentHonorsOptOut(t *testing.T) {
	srv, st := newJournalServer(t)
	seedJournalDays(t, st, "Harper", []string{"2023-05-01"})
	convID := conversationID(t, st, "Harper")
	seedSentimentForDay(t, st, convID, "2023-05-01", upbeatScores)

	if grid, _ := gridAndLegend(t, get(t, srv, "/journal?year=2023&month=5").Body.String()); !strings.Contains(grid, "cal-day--inferred") {
		t.Fatal("precondition failed: the scored day should be tinted from sentiment before the opt-out")
	}

	var cid int64
	if err := st.DB().QueryRow(`SELECT contact_id FROM conversations WHERE id = ?`, convID).Scan(&cid); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSentimentOptOut(context.Background(), cid, true); err != nil {
		t.Fatalf("SetSentimentOptOut: %v", err)
	}

	grid, _ := gridAndLegend(t, get(t, srv, "/journal?year=2023&month=5").Body.String())
	if strings.Contains(grid, "cal-day--inferred") {
		t.Error("an opted-out contact's messages still contribute to a day tint")
	}
	if !strings.Contains(grid, "cal-day--unanalyzed") {
		t.Error("the day should fall back to the explicit unanalysed state once its only evidence is withdrawn")
	}
}

// TestJournalDayCardNamesItsMoodSource: the editorial card must say when its
// mood came from sentiment rather than a digest. Reading "tense" on a card
// headed "This day, editorialized" is a claim about a day nothing editorialized.
func TestJournalDayCardNamesItsMoodSource(t *testing.T) {
	srv, st := newJournalServer(t)
	seedJournalDays(t, st, "Harper", []string{"2023-05-01"})
	convID := conversationID(t, st, "Harper")
	seedSentimentForDay(t, st, convID, "2023-05-01", tenseScores)

	body := get(t, srv, "/journal?day=2023-05-01").Body.String()
	if !strings.Contains(body, "from sentiment") {
		t.Error("the day card's mood chip should name sentiment as its source")
	}
	if !strings.Contains(body, "tense") {
		t.Error("the day card should carry the sentiment-derived mood")
	}
}

// TestMoodClassRejectsUnknownMood pins REQ-0016-016 at the seam where a
// model-derived string becomes a class attribute. The digest parser enforces the
// allowlist on the way in; this is the check on the way out, so a row written by
// an older build (or edited by hand) cannot put an arbitrary token in the DOM.
func TestMoodClassRejectsUnknownMood(t *testing.T) {
	for _, mood := range []string{"upbeat", "neutral", "quiet", "tense"} {
		if got := moodClass(mood); got != "cal-day--"+mood {
			t.Errorf("moodClass(%q) = %q, want cal-day--%s", mood, got, mood)
		}
	}
	for _, mood := range []string{"", "ecstatic", "UPBEAT", `x" onclick="alert(1)`, "cal-day--injected"} {
		if got := moodClass(mood); got != "" {
			t.Errorf("moodClass(%q) = %q, want \"\" — only the fixed enum may become a class", mood, got)
		}
	}
}

// TestJournalUnknownDigestMoodFallsBackToUnanalyzed is the same requirement seen
// from the grid: a digest carrying a mood outside the enum must not emit a class
// for it, and the day must land in a defined state anyway.
func TestJournalUnknownDigestMoodFallsBackToUnanalyzed(t *testing.T) {
	srv, st := newJournalServer(t)
	seedJournalDays(t, st, "Harper", []string{"2023-05-01"})
	putDigest(t, st, "2023-05-01", "Odd mood.", "ecstatic", "")

	grid, _ := gridAndLegend(t, get(t, srv, "/journal?year=2023&month=5").Body.String())
	if strings.Contains(grid, "cal-day--ecstatic") {
		t.Error("an out-of-enum mood reached a class attribute (REQ-0016-016)")
	}
	if !strings.Contains(grid, "cal-day--unanalyzed") {
		t.Error("a day whose only mood is out-of-enum should render as unanalysed, not bare")
	}
}

// TestSentimentMoodsThresholds exercises the fold directly: direction, the
// neutral band, and the minimum-evidence floor that keeps one stray score from
// colouring a whole day.
func TestSentimentMoodsThresholds(t *testing.T) {
	agg := func(day, construct string, sum float64, n int) store.SentimentDayConstruct {
		return store.SentimentDayConstruct{Day: day, Construct: construct, Sum: sum, N: n}
	}
	got := sentimentMoods([]store.SentimentDayConstruct{
		agg("2023-05-01", "Cheerfulness", 2.4, 3), // mean +0.8 → upbeat
		agg("2023-05-02", "Anger", 2.1, 3),        // mean -0.7 → tense
		agg("2023-05-03", "Cheerfulness", 0.09, 3),
		agg("2023-05-03", "Anger", 0.06, 3),            // mean ≈ +0.005 → neutral
		agg("2023-05-04", "Cheerfulness", 0.9, 1),      // below sentimentMinScores → no mood
		agg("2023-05-05", "Conscientiousness", 3.0, 3), // domain tier → not affect, no mood
	})
	want := map[string]string{"2023-05-01": "upbeat", "2023-05-02": "tense", "2023-05-03": "neutral"}
	if len(got) != len(want) {
		t.Fatalf("sentimentMoods = %v, want exactly %v", got, want)
	}
	for day, mood := range want {
		if got[day] != mood {
			t.Errorf("sentimentMoods[%s] = %q, want %q", day, got[day], mood)
		}
	}
}

// TestAffectValenceCoversLexicon is the drift guard. internal/store cannot import
// internal/sentiment without closing a cycle, so the affect taxonomy lives here —
// which means nothing but this test stops a facet added to the lexicon from
// being silently dropped out of every day tint.
func TestAffectValenceCoversLexicon(t *testing.T) {
	lex, err := sentiment.BuildLexicon()
	if err != nil {
		t.Fatalf("BuildLexicon: %v", err)
	}
	affect := map[string]bool{}
	for _, c := range lex.Constructs {
		if c.Tier != sentiment.TierAffect {
			continue
		}
		affect[c.Name] = true
		if _, ok := affectValence[c.Name]; !ok {
			t.Errorf("affect construct %q has no entry in affectValence — decide its valence, or the journal silently ignores it", c.Name)
		}
	}
	for name := range affectValence {
		if !affect[name] {
			t.Errorf("affectValence weights %q, which is not an affect-tier construct in the built lexicon", name)
		}
	}
}

// --- fixtures -------------------------------------------------------------

// conversationID looks up a seeded conversation by name.
func conversationID(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	id, err := st.UpsertConversation(context.Background(), source.Signal, name)
	if err != nil {
		t.Fatalf("conversation %q: %v", name, err)
	}
	return id
}

// staleDigest writes a digest whose captured message count disagrees with the
// day's current count, which is what makes the day render as out of date (#240).
func staleDigest(t *testing.T, st *store.Store, day, mood string, capturedCount int) {
	t.Helper()
	if err := st.PutDayDigest(context.Background(), store.JournalDigest{
		Day: day, Model: "m", PromptVersion: "pv", Body: "Written before more messages landed.",
		Mood: mood, MessageCount: capturedCount,
	}); err != nil {
		t.Fatal(err)
	}
}

// gridAndLegend splits a rendered journal page into the calendar grid and the
// legend beneath it, so a completeness check compares like with like instead of
// matching a class anywhere on the page.
func gridAndLegend(t *testing.T, body string) (grid, legend string) {
	t.Helper()
	gridStart := strings.Index(body, `class="journal-cal-grid"`)
	legendStart := strings.Index(body, `class="journal-cal-legend"`)
	legendEnd := strings.Index(body, `class="journal-stats"`)
	if gridStart < 0 || legendStart <= gridStart || legendEnd <= legendStart {
		t.Fatalf("could not locate the grid and legend regions (grid %d, legend %d, end %d)", gridStart, legendStart, legendEnd)
	}
	return body[gridStart:legendStart], body[legendStart:legendEnd]
}
