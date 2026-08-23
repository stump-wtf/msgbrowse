package sentiment

import "testing"

// TestAffectValenceCoversAffectTier: every affect-tier construct in the built
// lexicon must have a valence. Adding a facet to the curation without deciding
// whether it reads pleasant or unpleasant would silently drop it out of every
// fold — the surface would keep rendering, quietly ignoring a construct it was
// paying the model to produce.
func TestAffectValenceCoversAffectTier(t *testing.T) {
	lex, err := BuildLexicon()
	if err != nil {
		t.Fatalf("build lexicon: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range lex.Constructs {
		if c.Tier != TierAffect {
			continue
		}
		seen[c.Name] = true
		if _, ok := Valence(c.Name); !ok {
			t.Errorf("affect construct %q has no valence in AffectValence — decide its direction", c.Name)
		}
	}
	if len(seen) == 0 {
		t.Fatal("the lexicon has no affect-tier constructs at all")
	}
	// The reverse direction: a valence for a construct the lexicon no longer
	// has is a stale entry that will never fire again.
	for name := range AffectValence {
		if !seen[name] {
			t.Errorf("AffectValence weights %q, which is not an affect-tier construct in the lexicon", name)
		}
	}
}

// TestDomainConstructsHaveNoValence: the Big Five axes are bipolar personality
// dimensions, not affect. Weighting one would put "high Conscientiousness" into
// a day's mood, which SPEC-0027 scopes to the affect tier.
func TestDomainConstructsHaveNoValence(t *testing.T) {
	lex, err := BuildLexicon()
	if err != nil {
		t.Fatalf("build lexicon: %v", err)
	}
	for _, c := range lex.Constructs {
		if c.Tier != TierDomain {
			continue
		}
		if _, ok := Valence(c.Name); ok {
			t.Errorf("domain construct %q carries an affect valence", c.Name)
		}
	}
}
