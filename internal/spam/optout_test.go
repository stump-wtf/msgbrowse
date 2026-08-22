package spam

import "testing"

func TestMatchOptOut(t *testing.T) {
	r := testRules(t, nil)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"bare stop", "STOP", EventStopSent},
		{"lowercase with punctuation", "stop.", EventStopSent},
		{"unsubscribe", "Unsubscribe", EventStopSent},
		// A complaint containing the word "stop" is NOT a formal opt-out.
		// Treating it as one would date the violation window from the wrong
		// message, which is exactly the error the record must not make.
		{"stop inside a sentence", "please stop texting me about solar", ""},
		{"canned notice verbatim", "This number is on the National Do Not Call Registry and is a personal wireless line.", EventNoticeSent},
		{"canned notice with autocorrect and a greeting", "Hi — this Number is on the national do not call registry, and is a Personal wireless line!!", EventNoticeSent},
		{"unrelated", "no thanks", ""},
		{"empty", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.MatchOptOut(tc.body); got != tc.want {
				t.Errorf("MatchOptOut(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// With no notice configured, only bare keywords count — a partial-prefix match
// against an empty string must never fire on every outbound message.
func TestMatchOptOutWithoutNotice(t *testing.T) {
	r := testRules(t, func(r *Rules) { r.CannedNotice = "" })
	if got := r.MatchOptOut("anything at all"); got != "" {
		t.Errorf("empty notice matched: %q", got)
	}
	if got := r.MatchOptOut("STOP"); got != EventStopSent {
		t.Errorf("keyword stopped working: %q", got)
	}
}

func TestValidEventType(t *testing.T) {
	if !ValidEventType("fcc_complaint_filed") {
		t.Error("known type rejected")
	}
	if ValidEventType("sued_them") {
		t.Error("unknown type accepted")
	}
}
