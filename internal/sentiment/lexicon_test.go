package sentiment

import (
	"strings"
	"testing"
)

// lexiconFingerprintV1 pins the built lexicon for LexiconVersion "v1".
//
// This is the version-hash guard REQ "Embedded IPIP item table and versioned
// lexicon" asks for. Scores are only comparable within one
// (model, lexicon_version) generation, so a curation change that does not bump
// the version would silently mix incomparable rows in the read-side averages.
// Changing construct membership, the labels a construct draws from, the anchor
// selection rule, or the shipped item table all move this digest.
//
// If this test fails and the change was intentional: bump LexiconVersion, then
// update this constant to the digest the failure reports.
const lexiconFingerprintV1 = "40c1e8813b546896a3afe6c5d04eec2866b6eec0f57063ecc787a641bb8b9196"

func TestLexiconFingerprintPinned(t *testing.T) {
	lex, err := BuildLexicon()
	if err != nil {
		t.Fatalf("building lexicon: %v", err)
	}
	if LexiconVersion != "v1" {
		t.Fatalf("LexiconVersion = %q; this pin covers v1 — add a new pinned digest for the new version", LexiconVersion)
	}
	if got := lex.Fingerprint(); got != lexiconFingerprintV1 {
		t.Errorf("lexicon fingerprint = %s, want %s\n"+
			"The curation, the anchor-selection rule, or the embedded item table changed. "+
			"Bump LexiconVersion and update lexiconFingerprintV1 to the value above.", got, lexiconFingerprintV1)
	}
}

func TestLexiconCoversBothTiers(t *testing.T) {
	lex, err := BuildLexicon()
	if err != nil {
		t.Fatalf("building lexicon: %v", err)
	}

	domains := map[string]bool{}
	affects := map[string]bool{}
	for _, c := range lex.Constructs {
		switch c.Tier {
		case TierDomain:
			domains[c.Name] = true
		case TierAffect:
			affects[c.Name] = true
		default:
			t.Errorf("construct %q has unknown tier %q", c.Name, c.Tier)
		}
	}

	// The five Big Five domains, and the default affect tier named by the REQ.
	for _, want := range []string{"Extraversion", "Agreeableness", "Conscientiousness", "Emotional Stability", "Intellect/Openness"} {
		if !domains[want] {
			t.Errorf("missing Big Five domain %q", want)
		}
	}
	for _, want := range []string{
		"Anger", "Anxiety", "Depression", "Cheerfulness", "Hope/Optimism",
		"Calmness", "Vulnerability", "Self-consciousness", "Vitality/Enthusiasm/Zest", "Empathy",
	} {
		if !affects[want] {
			t.Errorf("missing affect facet %q", want)
		}
	}
	if got := len(domains); got != 5 {
		t.Errorf("domain constructs = %d, want 5", got)
	}
	if got := len(affects); got != 10 {
		t.Errorf("affect constructs = %d, want 10", got)
	}
}

// TestEveryConstructHasBothPolarities is the scenario attached to the REQ:
// every lexicon construct must resolve to at least one positively AND one
// negatively keyed anchor. A one-sided construct would show the model only one
// pole of the axis it is being asked to score.
func TestEveryConstructHasBothPolarities(t *testing.T) {
	lex, err := BuildLexicon()
	if err != nil {
		t.Fatalf("building lexicon: %v", err)
	}
	for _, c := range lex.Constructs {
		var pos, neg int
		for _, a := range c.Anchors {
			switch a.Key {
			case 1:
				pos++
			case -1:
				neg++
			default:
				t.Errorf("construct %q: anchor %q has key %d, want +1 or -1", c.Name, a.Text, a.Key)
			}
		}
		if pos == 0 || neg == 0 {
			t.Errorf("construct %q: %d positive / %d negative anchors, want at least one of each", c.Name, pos, neg)
		}
		if len(c.Anchors) == 0 || len(c.Anchors) > maxAnchors {
			t.Errorf("construct %q: %d anchors, want 1..%d", c.Name, len(c.Anchors), maxAnchors)
		}
	}
}

func TestAnchorsAreDedupedAndAboveAlphaFloor(t *testing.T) {
	items, err := Items()
	if err != nil {
		t.Fatalf("loading table: %v", err)
	}
	// Index the strongest alpha per (label, text) so we can check what was chosen.
	strongest := map[string]float64{}
	for _, it := range items {
		k := it.Label + "\x00" + it.Text
		if it.Alpha > strongest[k] {
			strongest[k] = it.Alpha
		}
	}

	lex, err := BuildLexicon()
	if err != nil {
		t.Fatalf("building lexicon: %v", err)
	}
	for _, c := range lex.Constructs {
		seen := map[string]bool{}
		for _, a := range c.Anchors {
			if seen[a.Text] {
				t.Errorf("construct %q: anchor %q appears twice", c.Name, a.Text)
			}
			seen[a.Text] = true

			var best float64
			for _, label := range c.Labels {
				if v := strongest[label+"\x00"+a.Text]; v > best {
					best = v
				}
			}
			if best < minAnchorAlpha {
				t.Errorf("construct %q: anchor %q has alpha %v, below the %v floor", c.Name, a.Text, best, minAnchorAlpha)
			}
		}
	}
}

// TestBuildLexiconFailsLoudlyOnLabelDrift is the "fails loudly if a construct's
// label no longer matches the table" half of the REQ. A silent skip here would
// ship a lexicon quietly missing a construct.
func TestBuildLexiconFailsLoudlyOnLabelDrift(t *testing.T) {
	items, err := Items()
	if err != nil {
		t.Fatalf("loading table: %v", err)
	}
	_, err = buildLexicon(items, []Construct{
		{Name: "Ghost", Tier: TierAffect, Labels: []string{"NoSuchLabelInTheTable"}},
	})
	if err == nil {
		t.Fatal("buildLexicon accepted a construct whose label is absent from the table, want error")
	}
	if !strings.Contains(err.Error(), "NoSuchLabelInTheTable") {
		t.Errorf("error %q does not name the missing label", err)
	}
}

func TestBuildLexiconFailsOnOnePolarity(t *testing.T) {
	// Two positively keyed items and nothing negative: the construct resolves
	// but cannot orient the axis, so the build must refuse it.
	items := []Item{
		{Instrument: "X", Alpha: 0.9, Key: 1, Text: "a", Label: "Lopsided"},
		{Instrument: "X", Alpha: 0.9, Key: 1, Text: "b", Label: "Lopsided"},
	}
	_, err := buildLexicon(items, []Construct{{Name: "Lopsided", Tier: TierAffect, Labels: []string{"Lopsided"}}})
	if err == nil {
		t.Fatal("buildLexicon accepted a construct with no negatively keyed anchor, want error")
	}
	if !strings.Contains(err.Error(), "anchor coverage") {
		t.Errorf("error %q does not explain the anchor-coverage failure", err)
	}
}

func TestBuildLexiconRejectsDuplicateConstructs(t *testing.T) {
	items := []Item{
		{Instrument: "X", Alpha: 0.9, Key: 1, Text: "a", Label: "L"},
		{Instrument: "X", Alpha: 0.9, Key: -1, Text: "b", Label: "L"},
	}
	defs := []Construct{
		{Name: "Dup", Tier: TierAffect, Labels: []string{"L"}},
		{Name: "Dup", Tier: TierAffect, Labels: []string{"L"}},
	}
	if _, err := buildLexicon(items, defs); err == nil {
		t.Fatal("buildLexicon accepted a duplicate construct name, want error")
	}
}

// TestAnchorSelectionKeepsBothPolesUnderTheCap guards the interleave: a
// construct with many strong positives and one weak negative must still surface
// that negative, rather than filling all maxAnchors slots from the positive
// pole and dropping the axis's other end.
func TestAnchorSelectionKeepsBothPolesUnderTheCap(t *testing.T) {
	var items []Item
	for _, text := range []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8"} {
		items = append(items, Item{Instrument: "X", Alpha: 0.99, Key: 1, Text: text, Label: "L"})
	}
	items = append(items, Item{Instrument: "X", Alpha: 0.75, Key: -1, Text: "n1", Label: "L"})

	anchors, err := selectAnchors(items)
	if err != nil {
		t.Fatalf("selectAnchors: %v", err)
	}
	if len(anchors) != maxAnchors {
		t.Errorf("anchors = %d, want %d", len(anchors), maxAnchors)
	}
	var sawNegative bool
	for _, a := range anchors {
		if a.Key == -1 {
			sawNegative = true
		}
	}
	if !sawNegative {
		t.Errorf("the sole negatively keyed anchor was crowded out by stronger positives: %+v", anchors)
	}
}

// TestBuildLexiconIsDeterministic guards against map-iteration order leaking
// into the anchors — which would make the fingerprint, and therefore the
// version guard, flap between runs.
func TestBuildLexiconIsDeterministic(t *testing.T) {
	items, err := Items()
	if err != nil {
		t.Fatalf("loading table: %v", err)
	}
	first, err := buildLexicon(items, curation)
	if err != nil {
		t.Fatalf("building lexicon: %v", err)
	}
	want := first.Fingerprint()
	for range 20 {
		again, err := buildLexicon(items, curation)
		if err != nil {
			t.Fatalf("building lexicon: %v", err)
		}
		if got := again.Fingerprint(); got != want {
			t.Fatalf("fingerprint is not stable across builds: %s != %s", got, want)
		}
	}
}
