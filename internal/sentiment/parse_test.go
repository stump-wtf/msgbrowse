package sentiment

import (
	"math"
	"testing"

	"github.com/joestump/msgbrowse/internal/store"
)

func testLexicon(t *testing.T) *Lexicon {
	t.Helper()
	lex, err := BuildLexicon()
	if err != nil {
		t.Fatalf("building lexicon: %v", err)
	}
	return lex
}

func testMessages() []store.MessageView {
	return []store.MessageView{
		{Hash: "h1", TSUnix: 1000, Body: "one"},
		{Hash: "h2", TSUnix: 2000, Body: "two", IsOwner: true},
	}
}

func constant(id int64) func(store.MessageView) int64 {
	return func(store.MessageView) int64 { return id }
}

// byConstruct indexes parsed scores for assertions.
func byConstruct(scores []store.SentimentScore) map[string]store.SentimentScore {
	out := make(map[string]store.SentimentScore, len(scores))
	for _, s := range scores {
		out[s.Construct] = s
	}
	return out
}

// TestParseScoresIsDefensive is the REQ's scenario, verbatim: a fenced response
// carrying a 3.7 and an unknown construct "Sassiness" must strip the fence,
// clamp 3.7 to 1.0, drop "Sassiness", and keep the rest.
func TestParseScoresIsDefensive(t *testing.T) {
	lex := testLexicon(t)
	raw := "Here you go!\n```json\n" + `[
	  {"message": 1, "scores": {"Anxiety": 3.7, "Sassiness": 0.9, "Cheerfulness": 0.55}}
	]` + "\n```\nHope that helps."

	scores, err := parseScores(raw, testMessages(), lex, constant(7))
	if err != nil {
		t.Fatalf("parseScores: %v", err)
	}
	got := byConstruct(scores)
	if len(got) != 2 {
		t.Fatalf("parsed %d constructs (%v), want 2", len(got), got)
	}
	if a, ok := got["Anxiety"]; !ok || a.Score != 1.0 {
		t.Errorf("Anxiety = %+v, want score clamped to 1.0", a)
	}
	if _, ok := got["Sassiness"]; ok {
		t.Error(`unknown construct "Sassiness" was kept`)
	}
	if c := got["Cheerfulness"]; c.Score != 0.55 {
		t.Errorf("Cheerfulness score = %v, want 0.55", c.Score)
	}
	if c := got["Cheerfulness"]; c.MessageHash != "h1" || c.TSUnix != 1000 || c.ContactID != 7 {
		t.Errorf("provenance = %+v, want hash h1 / ts 1000 / contact 7", c)
	}
}

func TestParseScoresClampsBothEnds(t *testing.T) {
	lex := testLexicon(t)
	raw := `[{"message":1,"scores":{"Anger":-9.5,"Empathy":42}}]`
	got := byConstruct(mustParse(t, raw, lex))
	if got["Anger"].Score != -1 {
		t.Errorf("Anger = %v, want -1", got["Anger"].Score)
	}
	if got["Empathy"].Score != 1 {
		t.Errorf("Empathy = %v, want 1", got["Empathy"].Score)
	}
}

// TestParseScoresAppliesSalienceFloor pins the sparseness rule: weak scores are
// discarded rather than stored, so the table tracks expressive content instead
// of message count.
func TestParseScoresAppliesSalienceFloor(t *testing.T) {
	lex := testLexicon(t)
	raw := `[{"message":1,"scores":{"Anger":0.19,"Anxiety":-0.19,"Empathy":0.2,"Calmness":-0.21}}]`
	got := byConstruct(mustParse(t, raw, lex))

	for _, dropped := range []string{"Anger", "Anxiety"} {
		if _, ok := got[dropped]; ok {
			t.Errorf("%s scored below the salience floor but was kept", dropped)
		}
	}
	for _, kept := range []string{"Empathy", "Calmness"} {
		if _, ok := got[kept]; !ok {
			t.Errorf("%s scored at or above the salience floor but was dropped", kept)
		}
	}
}

// TestParseScoresSkipsMalformedEntryWithoutFailingBatch is the other half of the
// defensive-parsing REQ: one bad element must not cost the whole batch.
func TestParseScoresSkipsMalformedEntryWithoutFailingBatch(t *testing.T) {
	lex := testLexicon(t)
	raw := `[
	  {"message": 1, "scores": {"Empathy": 0.8}},
	  {"message": "not-a-number", "scores": {"Anger": 0.9}},
	  "a bare string where an object belongs",
	  {"message": 2, "scores": {"Calmness": 0.7}}
	]`
	scores := mustParse(t, raw, lex)
	if len(scores) != 2 {
		t.Fatalf("parsed %d scores, want 2 (the two well-formed entries)", len(scores))
	}
	got := byConstruct(scores)
	if _, ok := got["Empathy"]; !ok {
		t.Error("the entry before the malformed one was lost")
	}
	if _, ok := got["Calmness"]; !ok {
		t.Error("the entry after the malformed one was lost")
	}
}

// TestParseScoresDropsOutOfRangeCitations: a score attributed to a message that
// is not in the batch is dropped rather than clamped onto a neighbour. Wrong
// provenance is worse than missing provenance.
func TestParseScoresDropsOutOfRangeCitations(t *testing.T) {
	lex := testLexicon(t)
	for _, raw := range []string{
		`[{"message":0,"scores":{"Empathy":0.9}}]`,
		`[{"message":99,"scores":{"Empathy":0.9}}]`,
		`[{"message":-3,"scores":{"Empathy":0.9}}]`,
	} {
		if scores := mustParse(t, raw, lex); len(scores) != 0 {
			t.Errorf("%s parsed to %+v, want nothing", raw, scores)
		}
	}
}

func TestParseScoresRejectsNonFinite(t *testing.T) {
	lex := testLexicon(t)
	// encoding/json rejects bare NaN/Inf literals, so drive the coercion through
	// a value that survives unmarshalling as a float64 overflow instead.
	raw := `[{"message":1,"scores":{"Empathy":1e400}}]`
	scores, err := parseScores(raw, testMessages(), lex, constant(1))
	if err != nil {
		// A JSON number that overflows float64 is a parse error for the whole
		// element; either behaviour is acceptable so long as nothing non-finite
		// is stored.
		return
	}
	for _, s := range scores {
		if math.IsNaN(s.Score) || math.IsInf(s.Score, 0) {
			t.Errorf("non-finite score stored: %+v", s)
		}
	}
}

func TestParseScoresRequiresAnArray(t *testing.T) {
	lex := testLexicon(t)
	for _, raw := range []string{"", "I could not score these messages.", "{}"} {
		if _, err := parseScores(raw, testMessages(), lex, constant(1)); err == nil {
			t.Errorf("parseScores(%q) succeeded, want error", raw)
		}
	}
}

func TestParseScoresEmptyArrayIsValid(t *testing.T) {
	lex := testLexicon(t)
	scores, err := parseScores("[]", testMessages(), lex, constant(1))
	if err != nil {
		t.Fatalf("an empty array is a valid 'nothing salient' response: %v", err)
	}
	if len(scores) != 0 {
		t.Errorf("got %+v, want no scores", scores)
	}
}

func TestParseScoresAttributesToSender(t *testing.T) {
	lex := testLexicon(t)
	contactFor := func(m store.MessageView) int64 {
		if m.IsOwner {
			return store.OwnerContactID
		}
		return 42
	}
	raw := `[{"message":1,"scores":{"Empathy":0.9}},{"message":2,"scores":{"Anger":0.9}}]`
	scores, err := parseScores(raw, testMessages(), lex, contactFor)
	if err != nil {
		t.Fatal(err)
	}
	got := byConstruct(scores)
	if got["Empathy"].ContactID != 42 {
		t.Errorf("contact message attributed to %d, want 42", got["Empathy"].ContactID)
	}
	if got["Anger"].ContactID != store.OwnerContactID {
		t.Errorf("owner message attributed to %d, want OwnerContactID", got["Anger"].ContactID)
	}
}

func TestExtractJSONArray(t *testing.T) {
	tests := []struct{ in, want string }{
		{"[1,2]", "[1,2]"},
		{"```json\n[1,2]\n```", "[1,2]"},
		{"sure thing:\n[1,2]\nlet me know", "[1,2]"},
		{"no array here", ""},
		{"]backwards[", ""},
	}
	for _, tc := range tests {
		if got := extractJSONArray(tc.in); got != tc.want {
			t.Errorf("extractJSONArray(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func mustParse(t *testing.T, raw string, lex *Lexicon) []store.SentimentScore {
	t.Helper()
	scores, err := parseScores(raw, testMessages(), lex, constant(1))
	if err != nil {
		t.Fatalf("parseScores(%q): %v", raw, err)
	}
	return scores
}
