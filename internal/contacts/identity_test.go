package contacts

import "testing"

// TestDeriveIdentityPerSource covers the shapes each source actually produces,
// which is the whole point of issue #363: the old code treated all three as
// interchangeable strings and so could never match across them.
func TestDeriveIdentityPerSource(t *testing.T) {
	cases := []struct {
		name       string
		convName   string
		hint       SourceIdentity
		wantID     string
		wantKind   Kind
		wantGroup  bool
		wantName   string
		wantStrong bool
	}{{
		name:       "whatsapp JID supplies a real phone the display name hides",
		convName:   "Chelsea Stump",
		hint:       SourceIdentity{Identifier: "15551234567"},
		wantID:     "15551234567",
		wantKind:   KindPhone,
		wantName:   "Chelsea Stump",
		wantStrong: true,
	}, {
		name:       "imessage conversation named by its handle",
		convName:   "+1 (555) 123-4567",
		wantID:     "+15551234567",
		wantKind:   KindPhone,
		wantName:   "+1 (555) 123-4567",
		wantStrong: true,
	}, {
		name:       "imessage conversation named by an email handle",
		convName:   "Chelsea@Example.COM",
		wantID:     "chelsea@example.com",
		wantKind:   KindEmail,
		wantName:   "Chelsea@Example.COM",
		wantStrong: true,
	}, {
		// The Signal case, and the reason this issue exists: signal-export
		// writes export/<Display Name>/chat.md and nothing in it carries a
		// number, so the profile name is genuinely all there is.
		name:       "signal profile name yields a weak handle, not a phone",
		convName:   "ChelseaStump",
		wantID:     "ChelseaStump",
		wantKind:   KindHandle,
		wantName:   "ChelseaStump",
		wantStrong: false,
	}, {
		name:      "comma-joined imessage thread is a group with no identity",
		convName:  "me@example.com, chelsea@example.com",
		wantGroup: true,
		wantName:  "me@example.com, chelsea@example.com",
	}, {
		name:      "whatsapp group flag is honoured even with a plain name",
		convName:  "Trip planning",
		hint:      SourceIdentity{IsGroup: true},
		wantGroup: true,
		wantName:  "Trip planning",
	}, {
		// A surname-first contact name has a comma but only one handle-shaped
		// part, so it must not be mistaken for a participant list.
		name:       "surname-first personal name is not a group",
		convName:   "Stump, Joe",
		wantID:     "Stump, Joe",
		wantKind:   KindHandle,
		wantName:   "Stump, Joe",
		wantStrong: false,
	}, {
		name:       "importer hint that is not handle-shaped falls back to the name",
		convName:   "Harper",
		hint:       SourceIdentity{Identifier: "not-a-handle"},
		wantID:     "Harper",
		wantKind:   KindHandle,
		wantName:   "Harper",
		wantStrong: false,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DeriveIdentity(c.convName, c.hint)
			if got.Identifier != c.wantID {
				t.Errorf("Identifier = %q, want %q", got.Identifier, c.wantID)
			}
			if got.IdentifierKind != c.wantKind {
				t.Errorf("IdentifierKind = %q, want %q", got.IdentifierKind, c.wantKind)
			}
			if got.IsGroup != c.wantGroup {
				t.Errorf("IsGroup = %v, want %v", got.IsGroup, c.wantGroup)
			}
			if got.DisplayName != c.wantName {
				t.Errorf("DisplayName = %q, want %q", got.DisplayName, c.wantName)
			}
			if got.HasRealHandle() != c.wantStrong {
				t.Errorf("HasRealHandle = %v, want %v", got.HasRealHandle(), c.wantStrong)
			}
		})
	}
}

// TestFoldDisplayNameRefusesWeakEvidence: the folding is what makes
// display-name matching possible, so its refusals are the safety property. A
// name too short, or one that is really a handle, must never fold to a match.
func TestFoldDisplayNameRefusesWeakEvidence(t *testing.T) {
	equal := [][2]string{
		{"ChelseaStump", "Chelsea Stump"},
		{"chelsea-stump", "Chelsea Stump"},
		{"Chelsea  Stump ", "chelseastump"},
	}
	for _, p := range equal {
		a, b := FoldDisplayName(p[0]), FoldDisplayName(p[1])
		if a == "" || a != b {
			t.Errorf("FoldDisplayName(%q)=%q and (%q)=%q should match and be non-empty", p[0], a, p[1], b)
		}
	}
	// Real Signal profile names from the reported archive. Folding these to a
	// match would group strangers.
	for _, short := range []string{"AT", "Alex", "", "  ", "Joe"} {
		if got := FoldDisplayName(short); got != "" {
			t.Errorf("FoldDisplayName(%q) = %q, want \"\" (too short to be evidence)", short, got)
		}
	}
	// Handles are matched precisely by the strong reasons; folding one would
	// strip the punctuation that makes it a handle.
	for _, handle := range []string{"+15551234567", "chelsea@example.com"} {
		if got := FoldDisplayName(handle); got != "" {
			t.Errorf("FoldDisplayName(%q) = %q, want \"\" (handles use strong matching)", handle, got)
		}
	}
}

// TestDisplayNameCandidatesOnlyBridgeSources: the reason exists to join one
// person across sources. Two same-source contacts sharing a name are far more
// likely to be two people, and merging them blends two archives.
func TestDisplayNameCandidatesOnlyBridgeSources(t *testing.T) {
	rules := MatchRules{MatchDisplayName: true}

	crossSource := []StoredIdentifier{
		{ContactID: 1, Source: "signal", Raw: "ChelseaStump"},
		{ContactID: 2, Source: "imessage", Raw: "chelsea@example.com"},
	}
	names := []DisplayNamed{
		{ContactID: 1, Name: "ChelseaStump"},
		{ContactID: 2, Name: "Chelsea Stump"},
	}
	got := CandidatesWithNames(crossSource, nil, names, rules)
	if len(got) != 1 {
		t.Fatalf("cross-source name match: got %d candidates, want 1: %+v", len(got), got)
	}
	if got[0].Reason != ReasonDisplayName {
		t.Errorf("reason = %q, want %q", got[0].Reason, ReasonDisplayName)
	}

	sameSource := []StoredIdentifier{
		{ContactID: 1, Source: "signal", Raw: "ChelseaStump"},
		{ContactID: 2, Source: "signal", Raw: "Chelsea Stump"},
	}
	if got := CandidatesWithNames(sameSource, nil, names, rules); len(got) != 0 {
		t.Errorf("same-source name match should not be suggested, got %+v", got)
	}

	// Off by rule means silent.
	if got := CandidatesWithNames(crossSource, nil, names, MatchRules{}); len(got) != 0 {
		t.Errorf("display-name matching ran with the rule off: %+v", got)
	}
}

// TestStrongReasonBeatsDisplayName: when two contacts match by both a phone and
// a name, the candidate reports the phone — the specific evidence — so the
// review queue does not present a weak reason for a strong match.
func TestStrongReasonBeatsDisplayName(t *testing.T) {
	stored := []StoredIdentifier{
		{ContactID: 1, Source: "signal", Raw: "+15551234567"},
		{ContactID: 2, Source: "imessage", Raw: "15551234567"},
	}
	names := []DisplayNamed{
		{ContactID: 1, Name: "Chelsea Stump"},
		{ContactID: 2, Name: "ChelseaStump"},
	}
	got := CandidatesWithNames(stored, nil, names, MatchRules{MatchPhone: true, MatchDisplayName: true})
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	if got[0].Reason != ReasonPhone {
		t.Errorf("reason = %q, want %q (the stronger evidence)", got[0].Reason, ReasonPhone)
	}
}
