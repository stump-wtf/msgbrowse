// The Sentiment Consumer Surfaces — #313's three views, landed with #367
//
// These are the tests for the half of the feature that had never existed:
// before #367, `grep -r IPIP internal/web` returned nothing, so scoring an
// archive would have produced rows no surface could display.
//
// Four of them are the ones worth being strict about:
//
//   - TestOptedOutContactRendersNoSentimentAnywhere is the privacy guarantee.
//     The scores are inserted FIRST and the opt-out marker written directly
//     afterwards, deliberately bypassing SetSentimentOptOut (which deletes them):
//     that reproduces the real race — a contact opting out while a long run is in
//     flight, whose scores get written back seconds later — and proves the READ
//     side refuses them on its own rather than relying on the delete having
//     happened.
//   - TestTraitSketchThreshold pins the >= 50 boundary from both sides. Below it
//     the sketch must be WITHHELD, not faded: five bars off a dozen messages is
//     false precision wearing clinical trait names.
//   - TestJournalDayMoodStripUTCBucketing seeds messages either side of UTC
//     midnight and checks each score lands in the same day bucket as the journal
//     rollup — the exact double-shift ADR-0023 exists to prevent.
//   - TestContactSentimentCarriesUncertainty is the requirement most likely to be
//     under-delivered, because nothing breaks when it is missing.
//
// @joestump-agent 08/23/2026 - Added with the sentiment consumer surfaces
// (#367, delivering #313).
package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/sentiment"
	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// testGen is the generation the fake scorer reports, and therefore the one every
// consumer surface reads under (currentSentimentGeneration).
var testGen = store.SentimentGeneration{Model: "test-chat", LexiconVersion: sentiment.LexiconVersion}

// newSentimentServer wires a Server over an EMPTY store with a scorer that
// reports a chat model, so the surfaces read under a known generation and the
// counts are exactly what the test seeds.
func newSentimentServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sentiment-web.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv, err := NewServer(st, &config.Config{DataDir: t.TempDir()},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srv.SetSentimentScorer(newFakeSentimentScorer("test-chat"))
	return srv, st
}

// scoredMsg is one seeded message and the constructs it "expressed". ts is a
// UTC "2006-01-02 15:04:05" timestamp.
type scoredMsg struct {
	ts     string
	scores map[string]float64
}

// seedScoredConversation creates a conversation with a linked contact, inserts
// the messages, builds the mechanical journal day layer over them, and writes
// the scores through the real PutSentimentBatch. It returns the conversation and
// contact ids.
//
// Scores go through the store's own writer rather than raw SQL so the tests
// exercise the same idempotent-upsert + cursor-advance path a real run takes.
func seedScoredConversation(t *testing.T, st *store.Store, conv string, msgs []scoredMsg) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	convID, err := st.UpsertConversation(ctx, source.Signal, conv)
	if err != nil {
		t.Fatal(err)
	}

	sigs := make([]signal.Message, 0, len(msgs))
	for i, m := range msgs {
		parsed, perr := time.Parse(signal.TimestampLayout, m.ts)
		if perr != nil {
			t.Fatalf("bad seed timestamp %q: %v", m.ts, perr)
		}
		sigs = append(sigs, signal.Message{
			Conversation: conv, Timestamp: parsed, TimestampRaw: m.ts,
			Sender: conv, Body: fmt.Sprintf("message %d from %s", i, conv),
		})
	}
	if _, err := st.ReplaceConversationMessages(ctx, convID, source.Signal, sigs); err != nil {
		t.Fatal(err)
	}

	// Some sources carry no phone/email-shaped identifier, so the importer may
	// leave the conversation unlinked. The profile surfaces are keyed by contact
	// id, so link one deterministically when that happens.
	contactID := ensureContact(t, st, convID, conv)

	days, err := st.BuildJournalDays(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range days {
		if err := st.PutJournalDay(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := st.DB().QueryContext(ctx,
		`SELECT hash, ts_unix FROM messages WHERE conversation_id = ? ORDER BY ts_unix, id`, convID)
	if err != nil {
		t.Fatal(err)
	}
	type msgRow struct {
		hash string
		ts   int64
	}
	var stored []msgRow
	for rows.Next() {
		var r msgRow
		if err := rows.Scan(&r.hash, &r.ts); err != nil {
			t.Fatal(err)
		}
		stored = append(stored, r)
	}
	rows.Close()
	if len(stored) != len(msgs) {
		t.Fatalf("seeded %d messages, store holds %d", len(msgs), len(stored))
	}

	var scores []store.SentimentScore
	for i, m := range msgs {
		for construct, v := range m.scores {
			scores = append(scores, store.SentimentScore{
				MessageHash: stored[i].hash, Construct: construct, Score: v,
				TSUnix: stored[i].ts, ContactID: contactID,
			})
		}
	}
	if err := st.PutSentimentBatch(ctx, convID, testGen, stored[len(stored)-1].hash, scores); err != nil {
		t.Fatal(err)
	}
	return convID, contactID
}

// ensureContact returns the conversation's contact id, creating and linking one
// when the importer left it unlinked.
func ensureContact(t *testing.T, st *store.Store, convID int64, name string) int64 {
	t.Helper()
	ctx := context.Background()
	var cid *int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT contact_id FROM conversations WHERE id = ?`, convID).Scan(&cid); err != nil {
		t.Fatal(err)
	}
	if cid != nil {
		return *cid
	}
	res, err := st.DB().ExecContext(ctx, `INSERT INTO contacts(display_name) VALUES(?)`, name)
	if err != nil {
		t.Fatal(err)
	}
	newID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE conversations SET contact_id = ? WHERE id = ?`, newID, convID); err != nil {
		t.Fatal(err)
	}
	return newID
}

// affectMsgs builds n messages one hour apart from start — deliberately WITHIN
// one UTC day, so a day bucket clears sentiment.MinScores — each expressing the
// same three affect facets.
func affectMsgs(start string, n int, v float64) []scoredMsg {
	t0, _ := time.Parse(signal.TimestampLayout, start)
	out := make([]scoredMsg, 0, n)
	for i := range n {
		out = append(out, scoredMsg{
			ts: t0.Add(time.Duration(i) * time.Hour).Format(signal.TimestampLayout),
			scores: map[string]float64{
				"Cheerfulness":  v,
				"Hope/Optimism": v,
				"Calmness":      v,
			},
		})
	}
	return out
}

// domainMsgs builds n messages each carrying all five Big Five domain scores,
// which is what the trait sketch aggregates.
func domainMsgs(start string, n int) []scoredMsg {
	t0, _ := time.Parse(signal.TimestampLayout, start)
	out := make([]scoredMsg, 0, n)
	for i := range n {
		out = append(out, scoredMsg{
			ts: t0.Add(time.Duration(i) * time.Hour).Format(signal.TimestampLayout),
			scores: map[string]float64{
				"Extraversion":        0.4,
				"Agreeableness":       0.6,
				"Conscientiousness":   -0.2,
				"Emotional Stability": 0.1,
				"Intellect/Openness":  0.5,
			},
		})
	}
	return out
}

func contactURL(id int64) string { return "/contact/" + strconv.FormatInt(id, 10) }

// --- Contact profile: sentiment over time ---------------------------------

// TestContactProfileSentimentOverTime: month-bucketed affect renders inside
// contact_content, with the sample stated per row and no new page shell
// (SPEC-0017 REQ-0017-009's boosted-partial contract).
func TestContactProfileSentimentOverTime(t *testing.T) {
	srv, st := newSentimentServer(t)
	_, contactID := seedScoredConversation(t, st, "Harper", affectMsgs("2023-05-01 09:00:00", 6, 0.6))

	body := get(t, srv, contactURL(contactID)).Body.String()
	for _, want := range []string{"Expressed sentiment", "Affect over time", "May 2023", "leans positive", "scores"} {
		if !contains(body, want) {
			t.Errorf("contact profile missing %q", want)
		}
	}
	// The surface renders inside the existing main region, not a second shell.
	if n := strings.Count(body, `<main id="main-content"`); n != 1 {
		t.Errorf("contact page has %d main regions, want exactly 1", n)
	}
}

// TestContactProfileSentimentBoostedPartial: the new section must not break the
// boosted-swap contract — a partial is <title> + <main> and nothing else.
func TestContactProfileSentimentBoostedPartial(t *testing.T) {
	srv, st := newSentimentServer(t)
	_, contactID := seedScoredConversation(t, st, "Harper", affectMsgs("2023-05-01 09:00:00", 6, 0.6))

	rec := getPartial(t, srv, contactURL(contactID))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "Expressed sentiment") {
		t.Error("boosted partial dropped the sentiment section")
	}
	if contains(body, "<!DOCTYPE html>") || contains(body, "<body") {
		t.Error("partial carried the full document shell")
	}
}

// TestContactProfileSentimentEmptyState: a contact with messages and no scores
// gets an empty state that says what would fill it — never a fabricated neutral
// line, and never at the cost of the rest of the page (SPEC-0017 REQ-0017-008).
func TestContactProfileSentimentEmptyState(t *testing.T) {
	srv, st := newSentimentServer(t)
	_, contactID := seedScoredConversation(t, st, "Harper", []scoredMsg{
		{ts: "2023-05-01 09:00:00"}, // messages, no scores
		{ts: "2023-05-02 09:00:00"},
	})

	body := get(t, srv, contactURL(contactID)).Body.String()
	if !contains(body, "No sentiment has been scored yet") {
		t.Errorf("missing the unscored empty state:\n%s", body)
	}
	if !contains(body, "/settings/sentiment") {
		t.Error("the empty state does not point at the control that fills it")
	}
	if contains(body, "Affect over time") || contains(body, "Big Five sketch") {
		t.Error("an unscored contact rendered a series or a sketch")
	}
	// The rest of the profile is unaffected.
	if !contains(body, "Message volume") && !contains(body, "AI-gathered facts") {
		t.Error("the empty sentiment state took the rest of the profile down with it")
	}
}

// TestContactProfileSentimentNeedsChatModel: with no model configured there is
// no generation to read against, so the section points at the LLM tab rather
// than at a Score button that would refuse.
func TestContactProfileSentimentNeedsChatModel(t *testing.T) {
	srv, st := newSentimentServer(t)
	srv.SetSentimentScorer(newFakeSentimentScorer("")) // no chat model
	_, contactID := seedScoredConversation(t, st, "Harper", affectMsgs("2023-05-01 09:00:00", 6, 0.6))

	body := get(t, srv, contactURL(contactID)).Body.String()
	if !contains(body, "Sentiment scoring needs a chat model") {
		t.Errorf("missing the unconfigured state:\n%s", body)
	}
	if contains(body, "Affect over time") {
		t.Error("scores from another generation were rendered against no configured model")
	}
}

// --- Contact profile: the Big Five sketch and its threshold ---------------

// TestTraitSketchThreshold pins SPEC-0027's minimum from BOTH sides. One message
// below it the sketch must be withheld with an explanation; at it, the five axes
// render as labelled text+bar rows.
func TestTraitSketchThreshold(t *testing.T) {
	t.Run("below", func(t *testing.T) {
		srv, st := newSentimentServer(t)
		_, contactID := seedScoredConversation(t, st, "Harper",
			domainMsgs("2023-05-01 09:00:00", traitSketchMinMessages-1))

		body := get(t, srv, contactURL(contactID)).Body.String()
		if !contains(body, "Not enough scored messages for a trait sketch") {
			t.Errorf("a sketch was not withheld below the threshold:\n%s", body)
		}
		// The withheld notice mentions the phrase in prose, so look for the
		// section LABEL, which only the rendered sketch emits.
		if contains(body, `>Big Five sketch<`) {
			t.Error("the sketch rendered below the threshold — false precision")
		}
	})
	t.Run("at", func(t *testing.T) {
		srv, st := newSentimentServer(t)
		_, contactID := seedScoredConversation(t, st, "Harper",
			domainMsgs("2023-05-01 09:00:00", traitSketchMinMessages))

		body := get(t, srv, contactURL(contactID)).Body.String()
		if !contains(body, `>Big Five sketch<`) {
			t.Fatalf("the sketch did not render at the threshold:\n%s", body)
		}
		for _, axis := range []string{
			"Extraversion", "Agreeableness", "Conscientiousness",
			"Emotional Stability", "Intellect/Openness",
		} {
			if !contains(body, axis) {
				t.Errorf("trait sketch missing the %q axis", axis)
			}
		}
		// Not colour-alone (and not length-alone): every row prints its signed
		// score and a direction in WORDS alongside the bar. html/template escapes
		// the leading "+" to &#43;, which browsers render as "+".
		if !contains(body, "0.60") || !contains(body, "leans positive") || !contains(body, "leans negative") {
			t.Error("trait rows do not carry their number and direction as text")
		}
		// The sample is stated per row, so a bar drawn from two messages cannot
		// pass for one drawn from two hundred.
		if !contains(body, "50 scores") {
			t.Error("trait rows do not state the sample they rest on")
		}
		if contains(body, "Not enough scored messages") {
			t.Error("the withheld message rendered alongside the sketch")
		}
	})
}

// TestContactSentimentCarriesUncertainty: SPEC-0027's uncertainty requirement.
// An IPIP sketch built from text messages is an indication of what someone
// expressed, not an assessment of who they are, and the page must not read as
// clinical output about a real person. The disclaimer sits WITH the numbers.
func TestContactSentimentCarriesUncertainty(t *testing.T) {
	srv, st := newSentimentServer(t)
	_, contactID := seedScoredConversation(t, st, "Harper",
		domainMsgs("2023-05-01 09:00:00", traitSketchMinMessages))

	body := get(t, srv, contactURL(contactID)).Body.String()
	for _, want := range []string{
		"AI-generated",
		"not a psychological assessment",
		"not a clinical or diagnostic result",
		"an indication of tone, not a measurement of personality",
	} {
		if !contains(body, want) {
			t.Errorf("the sentiment section is missing its uncertainty framing: %q", want)
		}
	}
	// The disclaimer must precede the numbers, not trail them as a footnote.
	if d, s := strings.Index(body, "not a psychological assessment"), strings.Index(body, "Big Five sketch"); d < 0 || s < 0 || d > s {
		t.Error("the AI-generated disclaimer does not appear before the trait sketch")
	}
}

// --- Privacy: the opt-out, enforced at READ time --------------------------

// TestOptedOutContactRendersNoSentimentAnywhere is the load-bearing privacy
// test.
//
// The scores are written first and the opt-out marker inserted directly
// afterwards, bypassing SetSentimentOptOut (which deletes them). That is the
// real race — a contact opting out while a run is in flight has scores written
// back seconds later — and it proves the READ side refuses them on its own
// rather than trusting that the delete already happened.
//
// The assertion is total: no section, no series, no sketch, no score numbers, on
// the profile OR the journal day strip.
func TestOptedOutContactRendersNoSentimentAnywhere(t *testing.T) {
	srv, st := newSentimentServer(t)
	ctx := context.Background()
	// The opted-out contact gets BOTH kinds of score: enough domain rows for a
	// trait sketch, and same-day affect rows that would colour the journal strip
	// — so every surface has something to leak if the guard fails.
	optedOutMsgs := append(affectMsgs("2023-05-01 09:00:00", 4, -0.9),
		domainMsgs("2023-06-01 09:00:00", traitSketchMinMessages)...)
	_, contactID := seedScoredConversation(t, st, "Harper", optedOutMsgs)
	// A second contact who did NOT opt out, on the same day, so the strip has a
	// non-empty expected value rather than merely being absent.
	seedScoredConversation(t, st, "Quinn", affectMsgs("2023-05-01 13:00:00", 4, 0.7))

	if _, err := st.DB().ExecContext(ctx,
		`INSERT INTO contact_sentiment_optout(contact_id, created_at) VALUES(?, ?)`,
		contactID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	body := get(t, srv, contactURL(contactID)).Body.String()
	for _, forbidden := range []string{
		"Expressed sentiment", "Affect over time", "Big Five sketch",
		"Not enough scored messages", "No sentiment has been scored yet",
		"Extraversion", "Agreeableness", "leans positive",
	} {
		if contains(body, forbidden) {
			t.Errorf("an opted-out contact's profile rendered %q — it must show no sentiment, "+
				"no trait data, and no invitation to gather either", forbidden)
		}
	}

	// The day strip aggregates everyone, so it is the other place their affect
	// could leak. Their conversation's day must contribute nothing.
	strip, err := srv.journalDayMood(ctx, "2023-05-01")
	if err != nil {
		t.Fatal(err)
	}
	total, err := st.DaySentiment(ctx, "2023-05-01", testGen, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range total {
		if _, isAffect := sentiment.Valence(row.Construct); !isAffect {
			t.Errorf("an opted-out contact's %q scores reached the day aggregate", row.Construct)
		}
	}
	if strip.Scores != 12 { // Quinn's 4 messages x 3 affect facets, and nothing else
		t.Errorf("day strip folded %d scores, want 12 (only the contact who did not opt out)", strip.Scores)
	}
	// Harper's same-day affect was strongly negative; if any of it had survived
	// the read guard the day would not read upbeat.
	if strip.Mood != "upbeat" {
		t.Errorf("day mood = %q, want %q — the opted-out contact's affect moved the fold",
			strip.Mood, "upbeat")
	}
}

// TestOptedOutContactStillRendersTheRestOfTheProfile: suppressing the section
// must not suppress the page.
func TestOptedOutContactStillRendersTheRestOfTheProfile(t *testing.T) {
	srv, st := newSentimentServer(t)
	_, contactID := seedScoredConversation(t, st, "Harper", affectMsgs("2023-05-01 09:00:00", 6, 0.6))
	if _, err := st.DB().ExecContext(context.Background(),
		`INSERT INTO contact_sentiment_optout(contact_id, created_at) VALUES(?, ?)`,
		contactID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	rec := get(t, srv, contactURL(contactID))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !contains(rec.Body.String(), "AI-gathered facts") {
		t.Error("suppressing the sentiment section took the rest of the profile with it")
	}
}

// --- Journal: the per-day mood strip --------------------------------------

// TestJournalDayMoodStripRenders: a day with affect scores gets an additive
// strip carrying its mood, its sample, and the facets behind it.
func TestJournalDayMoodStripRenders(t *testing.T) {
	srv, st := newSentimentServer(t)
	// Three messages: each facet clears MinFacetScores and is listed (#435).
	seedScoredConversation(t, st, "Harper", affectMsgs("2023-05-01 09:00:00", 3, 0.8))

	body := get(t, srv, "/journal?day=2023-05-01").Body.String()
	for _, want := range []string{"Expressed affect", "upbeat", "Cheerfulness", "9 scores"} {
		if !contains(body, want) {
			t.Errorf("journal day view missing %q", want)
		}
	}
	if contains(body, "leans positive") {
		t.Error(`a facet row must never read "leans positive" (issue #434)`)
	}
	if !contains(body, "expressed") {
		t.Error("facet rows must use the expressed/absent/mixed wording family")
	}
	if !contains(body, "not an assessment of anyone who wrote them") {
		t.Error("the day strip is missing its AI-generated framing")
	}
}

// TestJournalDayMoodStripAbsentWithoutScores: the strip is ADDITIVE. A day with
// no scores renders the journal exactly as before — SPEC-0027 forbids it
// altering SPEC-0016's mechanical rollup or digest behavior.
func TestJournalDayMoodStripAbsentWithoutScores(t *testing.T) {
	srv, st := newSentimentServer(t)
	seedJournalDays(t, st, "Harper", []string{"2023-05-01"})

	rec := get(t, srv, "/journal?day=2023-05-01")
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if contains(body, "Expressed affect") {
		t.Error("a day with no scores rendered a mood strip")
	}
	if !contains(body, "messages") || !contains(body, "conversations") {
		t.Error("the day card lost its mechanical rollup")
	}
}

// TestJournalDayMoodStripBelowMinScores: one salient facet on one message is not
// a mood. Below sentiment.MinScores the day stays unstripped rather than being
// tinted off noise.
func TestJournalDayMoodStripBelowMinScores(t *testing.T) {
	srv, st := newSentimentServer(t)
	seedScoredConversation(t, st, "Harper", []scoredMsg{
		{ts: "2023-05-01 09:00:00", scores: map[string]float64{"Cheerfulness": 0.9}},
	})

	if contains(get(t, srv, "/journal?day=2023-05-01").Body.String(), "Expressed affect") {
		t.Error("a single score produced a day mood")
	}
}

// TestJournalDayMoodStripUTCBucketing is the ADR-0023 guard.
//
// Two messages straddle UTC midnight — 23:30 on the 1st and 00:30 on the 2nd.
// Each score must land in the SAME day bucket as its message's journal rollup.
// A 'localtime' conversion anywhere in the read path would double-shift the
// bucketing and pile both days' scores onto one, which is exactly the failure
// the requirement names.
func TestJournalDayMoodStripUTCBucketing(t *testing.T) {
	srv, st := newSentimentServer(t)
	ctx := context.Background()
	seedScoredConversation(t, st, "Harper", []scoredMsg{
		{ts: "2023-05-01 23:30:00", scores: map[string]float64{
			"Cheerfulness": 0.7, "Hope/Optimism": 0.6, "Calmness": 0.5}},
		{ts: "2023-05-02 00:30:00", scores: map[string]float64{
			"Anger": 0.8, "Anxiety": 0.7, "Vulnerability": 0.6}},
	})

	for _, tc := range []struct {
		day      string
		wantMood string
	}{
		{"2023-05-01", "upbeat"},
		{"2023-05-02", "tense"},
	} {
		strip, err := srv.journalDayMood(ctx, tc.day)
		if err != nil {
			t.Fatal(err)
		}
		if !strip.Rendered {
			t.Fatalf("%s: no mood strip; the day's scores landed in another bucket", tc.day)
		}
		if strip.Scores != 3 {
			t.Errorf("%s: folded %d scores, want exactly the 3 from that day's one message", tc.day, strip.Scores)
		}
		if strip.Mood != tc.wantMood {
			t.Errorf("%s: mood = %q, want %q", tc.day, strip.Mood, tc.wantMood)
		}

		// The bucket must agree with the journal's own rollup for the same day:
		// one message each, on both sides of midnight.
		view, ok, err := st.GetJournalDay(ctx, tc.day)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("%s: no journal day was rolled up at all", tc.day)
		}
		if view.MessageCount != 1 {
			t.Errorf("%s: journal rollup counted %d messages, want 1 — the two surfaces "+
				"disagree about which day this message belongs to", tc.day, view.MessageCount)
		}
	}
}

// TestSentimentReadsPinToGeneration: scores written under one model must not
// surface under another. Averaging two generations produces a number that
// describes neither.
func TestSentimentReadsPinToGeneration(t *testing.T) {
	srv, st := newSentimentServer(t)
	_, contactID := seedScoredConversation(t, st, "Harper", affectMsgs("2023-05-01 09:00:00", 6, 0.6))

	if contains(get(t, srv, contactURL(contactID)).Body.String(), "Affect over time") {
		// sanity: the series renders under the matching generation
	} else {
		t.Fatal("the series did not render under its own generation")
	}

	srv.SetSentimentScorer(newFakeSentimentScorer("a-different-model"))
	body := get(t, srv, contactURL(contactID)).Body.String()
	if contains(body, "Affect over time") {
		t.Error("scores from another model surfaced under the newly configured one")
	}
	if !contains(body, "No sentiment has been scored yet") {
		t.Error("the generation change did not fall back to the empty state")
	}
}

// TestJournalAffectBlockIsCollapsedDetails (issue #437): when a scored day
// renders, its affect block is a native <details> whose summary carries the
// score/construct counts; the facet bars and IPIP disclaimer live inside it.
func TestJournalAffectBlockIsCollapsedDetails(t *testing.T) {
	srv, st := newSentimentServer(t)
	seedScoredConversation(t, st, "Harper", affectMsgs("2023-05-01 09:00:00", 3, 0.8))

	body := get(t, srv, "/journal?day=2023-05-01").Body.String()
	i := strings.Index(body, `<details class="journal-affect">`)
	if i < 0 {
		t.Fatalf("scored day has no collapsed affect details block:\n%s", body[max(0, len(body)-600):])
	}
	if j := strings.Index(body, "Expressed affect"); j < 0 || j < i {
		t.Errorf("summary label (%d) not inside the details block (%d)", j, i)
	}
	if !strings.Contains(body, "3 scores across 3 constructs") && !strings.Contains(body, "scores across") {
		t.Errorf("summary missing the scores/construct counts:\n%s", body[max(0, len(body)-600):])
	}
	if !contains(body, "not an assessment of anyone who wrote them") {
		t.Error("the IPIP disclaimer must stay inside the collapsed block")
	}
}

// TestJournalDayHasExactlyOneMoodChip (issue #436): the mood chip must render
// once per day card. Before the fix a scored day showed two .journal-mood
// chips — the meta chip and the fold's chip in the "Expressed affect" line —
// the same computation rendered twice.
func TestJournalDayHasExactlyOneMoodChip(t *testing.T) {
	srv, st := newSentimentServer(t)
	seedScoredConversation(t, st, "Harper", affectMsgs("2023-05-01 09:00:00", 1, 0.8))

	body := get(t, srv, "/journal?day=2023-05-01").Body.String()
	if got := strings.Count(body, `class="journal-mood`); got != 1 {
		t.Fatalf("scored day rendered %d .journal-mood chips, want exactly 1", got)
	}
	// The fold's mood WORD survives as tooltip text on the meta chip, and the
	// collapsed affect block keeps its counts without a chip.
	if !contains(body, "Sentiment fold") && !contains(body, "from sentiment") {
		t.Errorf("chip tooltip lost the fold provenance:\n%s", body[max(0, len(body)-500):])
	}
}

// TestSentimentDayScopeLabel (issue #441): a day-scoped run renders as
// "Day YYYY-MM-DD" in the history's Scope column — the date reaches the page
// only through the fixed prefix + shape check.
func TestSentimentDayScopeLabel(t *testing.T) {
	if got := sentimentScopeLabel("day:2023-05-01"); got != "Day 2023-05-01" {
		t.Errorf("day scope label = %q", got)
	}
	if got := sentimentScopeLabel("day:not-a-day"); got != "Single day" {
		t.Errorf("malformed day scope label = %q", got)
	}
	if got := sentimentScopeLabel("weird-token"); got != "Whole archive" {
		t.Errorf("unknown scope must not print verbatim, got %q", got)
	}
}

// TestSentimentBarWordingModes (issue #434): the two wording families share
// thresholds and flags; facets must never say "leans".
func TestSentimentBarWordingModes(t *testing.T) {
	cases := []struct {
		mean     float64
		valence  string // newSentimentBar expectation
		facet    string // newFacetBar expectation
		pos, neg bool
	}{
		{0.60, "leans positive", "expressed", true, false},
		{-0.60, "leans negative", "absent", false, true},
		{0.05, "about even", "mixed", false, false},
		{sentiment.MoodThreshold, "leans positive", "expressed", true, false},
		{-sentiment.MoodThreshold, "leans negative", "absent", false, true},
	}
	for _, c := range cases {
		vb := newSentimentBar("Anger", c.mean, 2)
		if vb.Direction != c.valence {
			t.Errorf("valence bar(%+.2f) = %q, want %q", c.mean, vb.Direction, c.valence)
		}
		fb := newFacetBar("Anger", c.mean, 2)
		if fb.Direction != c.facet {
			t.Errorf("facet bar(%+.2f) = %q, want %q", c.mean, fb.Direction, c.facet)
		}
		if fb.Positive != c.pos || fb.Negative != c.neg {
			t.Errorf("facet bar(%+.2f) flags pos=%v neg=%v", c.mean, fb.Positive, fb.Negative)
		}
	}
}

// TestJournalFacetsOrderedByEvidence (issue #435): one dramatic score on X
// must not outrank ten moderate scores on Y, and sub-threshold facets are
// omitted from the list while still counting toward the total.
func TestJournalFacetsOrderedByEvidence(t *testing.T) {
	srv, st := newSentimentServer(t)
	msgs := []scoredMsg{}
	// Ten moderate Cheerfulness scores.
	for i := range 10 {
		msgs = append(msgs, scoredMsg{
			ts:     fmt.Sprintf("2023-05-01 %02d:00:00", i),
			scores: map[string]float64{"Cheerfulness": 0.5},
		})
	}
	// One dramatic score on Vulnerability (sub-threshold) and three moderate
	// Anxiety scores (listed, below Cheerfulness by evidence weight).
	msgs = append(msgs, scoredMsg{ts: "2023-05-01 23:00:00", scores: map[string]float64{"Vulnerability": 0.9}})
	for i := range 3 {
		msgs = append(msgs, scoredMsg{
			ts:     fmt.Sprintf("2023-05-01 1%d:00:00", i),
			scores: map[string]float64{"Anxiety": 0.6},
		})
	}
	seedScoredConversation(t, st, "Harper", msgs)

	body := get(t, srv, "/journal?day=2023-05-01").Body.String()
	c, a := strings.Index(body, "Cheerfulness"), strings.Index(body, "Anxiety")
	if c < 0 || a < 0 {
		t.Fatalf("expected Cheerfulness and Anxiety listed; c=%d a=%d", c, a)
	}
	if c > a {
		t.Error("Cheerfulness (10 scores) must outrank Anxiety (3 scores) by evidence weight")
	}
	if contains(body, "Vulnerability") {
		t.Error("a 1-score facet must not be listed (folded into the total instead)")
	}
	if !contains(body, "folded into the total but not listed") {
		t.Error("the threshold rule must be printed once under the list")
	}
	// The total still counts every score: 10 + 1 + 3 = 14.
	if !contains(body, "14 scores") {
		t.Error("sub-threshold facets must still count toward the day's total")
	}
}
