package spam

import (
	"reflect"
	"testing"
)

func testRules(t *testing.T, mutate func(*Rules)) *Rules {
	t.Helper()
	r := Rules{
		MyNumbers:      []string{"+15555550100"},
		Allowlist:      []string{"+18005551212", "662265"},
		WatchAreaCodes: []string{"555"},
		NameVariants:   []string{"Jon", "Jonathan"},
		FlagAnyURL:     true,
		CannedNotice:   "This number is on the National Do Not Call Registry and is a personal wireless line.",
	}
	if mutate != nil {
		mutate(&r)
	}
	built, err := NewRules(r)
	if err != nil {
		t.Fatalf("NewRules: %v", err)
	}
	return built
}

func TestClassifyReasons(t *testing.T) {
	r := testRules(t, nil)

	cases := []struct {
		name       string
		identifier string
		body       string
		want       []string
	}{
		{"watched area code", "+15551110001", "hello", []string{"area_code:555"}},
		{"your name", "+14045550002", "Hi Jon, still interested?", []string{"name_variant:Jon"}},
		{"shortener beats bare url", "+14045550002", "see https://bit.ly/x", []string{"shortener:bit.ly"}},
		{"bare url", "+14045550002", "see https://acme.example/x", []string{"url"}},
		{"nothing", "+14045550002", "wrong number, sorry", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.Classify(tc.identifier, tc.body)
			if !reflect.DeepEqual(got.Reasons, tc.want) {
				t.Fatalf("reasons = %q, want %q", got.Reasons, tc.want)
			}
			if got.IsCandidate != (len(tc.want) > 0) {
				t.Errorf("IsCandidate = %v for reasons %q", got.IsCandidate, got.Reasons)
			}
		})
	}
}

// A non-matching message is still a recorded fact: the rolling-window contact
// counts are counts of contact, not counts of violation.
func TestClassifyKeepsExtractedFieldsOnNonCandidates(t *testing.T) {
	r := testRules(t, func(r *Rules) { r.FlagAnyURL = false })
	got := r.Classify("+14045550002", "call 555-123-4567 or email sales@acme.example")
	if got.IsCandidate {
		t.Fatalf("unexpected candidate: %q", got.Reasons)
	}
	if !reflect.DeepEqual(got.Phones, []string{"+15551234567"}) {
		t.Errorf("phones = %q", got.Phones)
	}
	if !reflect.DeepEqual(got.Emails, []string{"sales@acme.example"}) {
		t.Errorf("emails = %q", got.Emails)
	}
}

func TestMatchKeyCollapsesPhoneSpellings(t *testing.T) {
	want := MatchKey("+15551234567")
	for _, spelling := range []string{"5551234567", "(555) 123-4567", "1-555-123-4567", "+1 555.123.4567"} {
		if got := MatchKey(spelling); got != want {
			t.Errorf("MatchKey(%q) = %q, want %q", spelling, got, want)
		}
	}
	if MatchKey("A@B.example") != "a@b.example" {
		t.Errorf("email not folded: %q", MatchKey("A@B.example"))
	}
	// Short codes are below NormalizePhone's floor, so they arrive as handles;
	// they must still compare on digits or an allowlisted 2FA code never matches.
	if MatchKey("662265") != "662265" {
		t.Errorf("short code key = %q", MatchKey("662265"))
	}
}

func TestOwnerAndAllowlistRecognized(t *testing.T) {
	r := testRules(t, nil)
	if !r.IsMine("(555) 555-0100") {
		t.Error("own number not recognized across spellings")
	}
	if !r.IsAllowlisted("18005551212") {
		t.Error("allowlisted number not recognized")
	}
	if !r.IsAllowlisted("662265") {
		t.Error("allowlisted short code not recognized")
	}
	if r.IsAllowlisted("+15551110001") {
		t.Error("unrelated number reported as allowlisted")
	}
}

// The version stamp is what makes a rule change behave like a model change:
// findings from two rule sets must never be mixed, so any rule edit has to move
// the version and a re-order must not.
func TestVersionChangesWithRulesNotOrder(t *testing.T) {
	base := testRules(t, nil)

	reordered := testRules(t, func(r *Rules) { r.NameVariants = []string{"Jonathan", "Jon"} })
	if reordered.Version() != base.Version() {
		t.Error("re-ordering a list changed the ruleset version")
	}

	for name, mutate := range map[string]func(*Rules){
		"area code added": func(r *Rules) { r.WatchAreaCodes = append(r.WatchAreaCodes, "666") },
		"name added":      func(r *Rules) { r.NameVariants = append(r.NameVariants, "Johnny") },
		"url rule off":    func(r *Rules) { r.FlagAnyURL = false },
		"notice edited":   func(r *Rules) { r.CannedNotice += " Further messages will be documented." },
		"ratio changed":   func(r *Rules) { r.NoticeMatchRatio = 0.9 },
	} {
		t.Run(name, func(t *testing.T) {
			if testRules(t, mutate).Version() == base.Version() {
				t.Errorf("%s did not change the ruleset version", name)
			}
		})
	}
}

func TestDefaultsFillIn(t *testing.T) {
	r, err := NewRules(Rules{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.ShortenerDomains) == 0 || len(r.StopKeywords) == 0 || len(r.EntityKeywords) == 0 {
		t.Fatal("defaults not applied")
	}
	if r.NoticeMatchRatio != DefaultNoticeMatchRatio {
		t.Errorf("ratio = %v", r.NoticeMatchRatio)
	}
}
