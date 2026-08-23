// Affect Valence — The One Table That Says Which Facets Read As Pleasant
//
// Every surface that folds per-message affect scores into a single "how did this
// read" number needs the same decision: which of the lexicon's affect facets
// push a bucket positive, and which push it negative. Cheerfulness is pleasant,
// Anger is not, and nothing in the score itself carries that — a +0.8 on Anxiety
// and a +0.8 on Calmness are both strongly expressed and mean opposite things.
//
// It lives HERE, next to the curation it describes, rather than in whichever
// consumer needed it first. Two different affect→valence mappings in one
// codebase is drift with a straight face: they would agree for a release and
// then quietly disagree the first time someone added a facet, and the two
// surfaces would tint the same day differently with no error anywhere.
// internal/store cannot import this package (it would close a cycle), which is
// exactly why the store's aggregates return per-(bucket, construct) rows and
// leave the fold to a caller that CAN.
//
// Domain-tier constructs are deliberately absent. The Big Five axes are bipolar
// personality dimensions, not affect — "high Conscientiousness" says nothing
// about whether a day felt good — and SPEC-0027 scopes per-day and
// over-time mood to the affect tier. TestAffectValenceCoversAffectTier checks
// this table against the built lexicon, so adding a facet to the curation
// without deciding its valence fails the build rather than silently dropping it
// out of every fold.
//
// @joestump-agent 08/23/2026 - Added with the sentiment consumer surfaces
// (#367/#313), hoisted out of the calendar's copy so both surfaces read one
// table.
package sentiment

// AffectValence maps each affect-tier construct to the direction it pushes a
// bucket's mean: +1 for a pleasant facet, -1 for an unpleasant one. A bucket's
// valence is the mean of every scored row weighted this way, which keeps the
// fold a single number regardless of which facets the model happened to find
// salient.
var AffectValence = map[string]float64{
	"Cheerfulness":             +1,
	"Hope/Optimism":            +1,
	"Calmness":                 +1,
	"Vitality/Enthusiasm/Zest": +1,
	"Empathy":                  +1,
	"Anger":                    -1,
	"Anxiety":                  -1,
	"Depression":               -1,
	"Vulnerability":            -1,
	"Self-consciousness":       -1,
}

// Valence returns a construct's weight and whether it is an affect-tier
// construct this build weights at all. A false second return means "not affect"
// (a Big Five domain, or a facet added to the curation without a valence), and
// callers MUST skip such a row rather than defaulting it to zero — a zero weight
// silently drags every mean toward neutral.
func Valence(construct string) (float64, bool) {
	w, ok := AffectValence[construct]
	return w, ok
}

// Fold parameters, shared by every surface that turns weighted affect into a
// three-state reading. They are here rather than per-surface for the same reason
// the table is: a calendar tint and a day strip that disagreed about where
// "neutral" ends would be describing the same data two ways.
const (
	// MoodThreshold is how far a bucket's mean valence must sit from zero before
	// it reads as pleasant or unpleasant rather than neutral. Scores are clamped
	// to [-1,+1] per message and averaged across a whole bucket, so bucket means
	// live close to zero; a wide band would paint everything neutral, a narrow
	// one would flip on noise.
	MoodThreshold = 0.12
	// MinScores is the minimum number of scored rows a bucket needs before it is
	// given a reading at all. One salient facet on one message is not a mood, and
	// reporting one would be exactly the false confidence SPEC-0027's uncertainty
	// requirement exists to prevent.
	MinScores = 3
)
