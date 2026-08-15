package sentiment

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// LexiconVersion stamps every score row written under this curation. Any change
// to construct membership, the labels a construct draws from, or the
// anchor-selection rule MUST bump it: scores are only comparable within one
// (model, lexicon_version) generation, and the read-side aggregates filter on
// it. TestLexiconFingerprintPinned enforces the bump by hashing the built
// lexicon — if you changed the curation and did not bump this, that test fails
// and tells you so.
const LexiconVersion = "v1"

// Anchor-selection parameters (ADR-0028 §1, design "Anchor selection").
const (
	// minAnchorAlpha is the reliability floor for an assignment to serve as a
	// marker item.
	minAnchorAlpha = 0.75
	// maxAnchors caps how many anchors a construct contributes to the prompt.
	// Six is enough to orient the model on both poles without crowding out the
	// messages being scored.
	maxAnchors = 6
)

// Tier separates the two halves of the taxonomy. Domain constructs are the Big
// Five axes and are bipolar — sign carries direction (reserved ↔ gregarious).
// Affect constructs are the facets a message can express and live mostly in the
// positive range.
type Tier string

const (
	TierDomain Tier = "domain"
	TierAffect Tier = "affect"
)

// Construct is one curated scoring target: the name the model is asked to score
// and the table label(s) its anchors are drawn from.
type Construct struct {
	Name   string
	Tier   Tier
	Labels []string
}

// Anchor is a marker item shown to the model to orient a construct, carrying
// the keying direction that says which pole it marks.
type Anchor struct {
	Text string
	Key  int
}

// ScoredConstruct is a curated construct with its anchors resolved from the
// embedded table.
type ScoredConstruct struct {
	Construct
	Anchors []Anchor
}

// Lexicon is the built, versioned curation handed to the scoring engine.
type Lexicon struct {
	Version    string
	Constructs []ScoredConstruct
}

// curation is the lexicon definition: five Big Five domains plus the default
// affect tier of ten facets (SPEC-0027 REQ "Embedded IPIP item table and
// versioned lexicon").
//
// Two notes on how labels map onto the table:
//
//   - Intellect/Openness draws from two same-direction sibling labels
//     ("Intellect", "Intellectual Openness") because the table has no bare
//     "Openness" label. This is the sibling case the design calls out.
//   - Emotional Stability deliberately draws ONLY from its own label, even
//     though "Neuroticism" is present and larger. The two are opposite poles of
//     one axis, so merging them without inverting Neuroticism's keying would
//     produce anchors that contradict each other. Emotional Stability has both
//     polarities on its own, so the inversion is not worth the footgun.
var curation = []Construct{
	{Name: "Extraversion", Tier: TierDomain, Labels: []string{"Extraversion"}},
	{Name: "Agreeableness", Tier: TierDomain, Labels: []string{"Agreeableness"}},
	{Name: "Conscientiousness", Tier: TierDomain, Labels: []string{"Conscientiousness"}},
	{Name: "Emotional Stability", Tier: TierDomain, Labels: []string{"Emotional Stability"}},
	{Name: "Intellect/Openness", Tier: TierDomain, Labels: []string{"Intellect", "Intellectual Openness"}},

	{Name: "Anger", Tier: TierAffect, Labels: []string{"Anger"}},
	{Name: "Anxiety", Tier: TierAffect, Labels: []string{"Anxiety"}},
	{Name: "Depression", Tier: TierAffect, Labels: []string{"Depression"}},
	{Name: "Cheerfulness", Tier: TierAffect, Labels: []string{"Cheerfulness"}},
	{Name: "Hope/Optimism", Tier: TierAffect, Labels: []string{"Hope/Optimism"}},
	{Name: "Calmness", Tier: TierAffect, Labels: []string{"Calmness"}},
	{Name: "Vulnerability", Tier: TierAffect, Labels: []string{"Vulnerability"}},
	{Name: "Self-consciousness", Tier: TierAffect, Labels: []string{"Self-consciousness"}},
	{Name: "Vitality/Enthusiasm/Zest", Tier: TierAffect, Labels: []string{"Vitality/Enthusiasm/Zest"}},
	{Name: "Empathy", Tier: TierAffect, Labels: []string{"Empathy"}},
}

var loadLexicon = sync.OnceValues(func() (*Lexicon, error) {
	items, err := Items()
	if err != nil {
		return nil, err
	}
	return buildLexicon(items, curation)
})

// BuildLexicon resolves the curation against the embedded item table. It fails
// loudly rather than degrading: a label that no longer exists in the table, or a
// construct left without both keying directions, is a curation bug that must
// not reach a scoring run silently.
func BuildLexicon() (*Lexicon, error) { return loadLexicon() }

func buildLexicon(items []Item, defs []Construct) (*Lexicon, error) {
	if len(defs) == 0 {
		return nil, fmt.Errorf("lexicon curation is empty")
	}
	byLabel := make(map[string][]Item, 256)
	for _, it := range items {
		byLabel[it.Label] = append(byLabel[it.Label], it)
	}

	out := &Lexicon{Version: LexiconVersion, Constructs: make([]ScoredConstruct, 0, len(defs))}
	seenName := make(map[string]struct{}, len(defs))
	for _, def := range defs {
		if _, dup := seenName[def.Name]; dup {
			return nil, fmt.Errorf("lexicon construct %q is defined twice", def.Name)
		}
		seenName[def.Name] = struct{}{}
		if len(def.Labels) == 0 {
			return nil, fmt.Errorf("lexicon construct %q declares no labels", def.Name)
		}

		var pool []Item
		for _, label := range def.Labels {
			rows, ok := byLabel[label]
			if !ok {
				return nil, fmt.Errorf("lexicon construct %q draws from label %q, which is not in the IPIP item table — the table changed under the curation", def.Name, label)
			}
			pool = append(pool, rows...)
		}

		anchors, err := selectAnchors(pool)
		if err != nil {
			return nil, fmt.Errorf("lexicon construct %q: %w", def.Name, err)
		}
		out.Constructs = append(out.Constructs, ScoredConstruct{Construct: def, Anchors: anchors})
	}
	return out, nil
}

// selectAnchors picks up to maxAnchors marker items for one construct.
//
// Candidates are assignments at or above minAnchorAlpha with a determined
// keying direction, deduped by item text (the same item is often assigned by
// several instruments) keeping the highest-alpha assignment for keying.
//
// The two poles are then interleaved rather than simply taking the top
// maxAnchors by alpha. Straight truncation could return six same-signed
// anchors for a lopsided construct — satisfying the cap while violating the
// requirement that both directions be represented, and handing the model a
// one-sided picture of the axis.
func selectAnchors(pool []Item) ([]Anchor, error) {
	best := make(map[string]Item, len(pool))
	for _, it := range pool {
		if it.Alpha < minAnchorAlpha || (it.Key != 1 && it.Key != -1) {
			continue
		}
		cur, ok := best[it.Text]
		if !ok || it.Alpha > cur.Alpha {
			best[it.Text] = it
		}
	}

	var pos, neg []Item
	for _, it := range best {
		if it.Key == 1 {
			pos = append(pos, it)
		} else {
			neg = append(neg, it)
		}
	}
	// Deterministic order: strongest scale first, item text breaking ties, so
	// the built lexicon (and therefore its fingerprint) does not depend on map
	// iteration order.
	byStrength := func(s []Item) {
		sort.Slice(s, func(i, j int) bool {
			if s[i].Alpha != s[j].Alpha {
				return s[i].Alpha > s[j].Alpha
			}
			return s[i].Text < s[j].Text
		})
	}
	byStrength(pos)
	byStrength(neg)

	if len(pos) == 0 || len(neg) == 0 {
		return nil, fmt.Errorf("anchor coverage: %d positively and %d negatively keyed items at alpha >= %.2f, want at least one of each", len(pos), len(neg), minAnchorAlpha)
	}

	out := make([]Anchor, 0, maxAnchors)
	for i := 0; len(out) < maxAnchors && (i < len(pos) || i < len(neg)); i++ {
		if i < len(pos) && len(out) < maxAnchors {
			out = append(out, Anchor{Text: pos[i].Text, Key: pos[i].Key})
		}
		if i < len(neg) && len(out) < maxAnchors {
			out = append(out, Anchor{Text: neg[i].Text, Key: neg[i].Key})
		}
	}
	return out, nil
}

// Fingerprint is a stable digest of the built lexicon — construct names, tiers,
// source labels, and every resolved anchor with its keying. It exists so a test
// can pin it: changing the curation or the selection rule changes the digest,
// which fails that test until LexiconVersion is bumped alongside.
func (l *Lexicon) Fingerprint() string {
	var b strings.Builder
	for _, c := range l.Constructs {
		fmt.Fprintf(&b, "%s\x1f%s\x1f%s\x1e", c.Name, c.Tier, strings.Join(c.Labels, "\x1d"))
		for _, a := range c.Anchors {
			fmt.Fprintf(&b, "%+d\x1f%s\x1e", a.Key, a.Text)
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
