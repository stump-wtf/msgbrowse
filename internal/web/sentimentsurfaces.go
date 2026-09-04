// The Sentiment Consumer Surfaces (#313, landed with #367)
//
// Three read-side surfaces fold the sparse per-message score rows into something
// a person can read: the contact profile's sentiment-over-time series, the
// contact profile's Big Five trait sketch, and the journal day view's mood
// strip. They are the half of the sentiment feature that was never built — the
// engine, the lexicon, the schema, ADR-0028 and SPEC-0027 all shipped, and
// `grep -r IPIP internal/web` returned nothing at all, so scoring an archive
// would have produced rows no surface could display.
//
// FOUR RULES RUN THROUGH ALL THREE, and each exists because getting it wrong is
// worse than not shipping the surface.
//
//  1. UNCERTAINTY IS PART OF THE OUTPUT. An IPIP sketch derived from text
//     messages is an indication of what someone EXPRESSED, not a psychological
//     assessment of who they are. internal/sentiment/prompt.go already frames
//     the scoring task that way ("You are scoring text, not people"), and the UI
//     must not undo it by rendering five confident bars labelled with clinical
//     trait names. So: the sketch is withheld entirely below a minimum sample
//     (traitSketchMinMessages), every surface states the sample it rests on, and
//     the templates carry the AI-generated + "expressed in messages, not a
//     psychological assessment" disclaimer next to the numbers rather than in a
//     footnote. SPEC-0027 requires this; it is also the requirement most likely
//     to be quietly under-delivered, because nothing breaks when it is.
//
//  2. OPT-OUT IS ENFORCED AT READ TIME, TWICE. Every store aggregate carries a
//     NOT EXISTS on contact_sentiment_optout inside the query, so no caller can
//     forget it (see internal/store/sentiment.go). The contact handler ALSO
//     checks the marker and suppresses the whole section, because a
//     structurally-empty surface would still render "no scores yet — run
//     scoring", which reads as an invitation to score someone who asked not to
//     be. Both layers, deliberately: the query is the guarantee, the handler is
//     the honesty.
//
//  3. GENERATION PINNING. Every read filters to the currently configured
//     (model, lexicon_version) — see currentSentimentGeneration. Averaging two
//     models' scores produces a number describing neither.
//
//  4. GRACEFUL DEGRADATION, NEVER A FABRICATED BASELINE. With no scores the
//     surfaces render an empty state that says what would fill it. They never
//     draw a flat neutral line, which would assert something the archive does
//     not know (SPEC-0017 REQ-0017-008's resilience contract, and the same
//     reasoning that made the calendar's untinted days a defect).
//
// The affect→valence taxonomy is deliberately NOT defined here. It lives with
// the lexicon it describes, in internal/sentiment, so the day strip and any
// other consumer fold identically — two mappings in one codebase is drift that
// compiles.
//
// Governing: SPEC-0027 (sentiment consumer surfaces + the uncertainty
// requirement), SPEC-0017 REQ-0017-008/009 (contact profile resilience and the
// boosted-partial contract — these render inside contact_content with no new
// shell), ADR-0023 (the journal's UTC day frame, which the day strip shares
// exactly: date(ts_unix,'unixepoch') with NO 'localtime'), ADR-0028.
//
// @joestump-agent 08/23/2026 - Added with the in-app scoring controls (#367),
// delivering #313's three surfaces.
package web

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/joestump/msgbrowse/internal/sentiment"
	"github.com/joestump/msgbrowse/internal/store"
)

const (
	// traitSketchMinMessages is the minimum number of SCORED MESSAGES (not score
	// rows — one expressive message can produce a dozen rows) a contact needs
	// before the Big Five sketch renders at all. SPEC-0027 sets the default at
	// 50. Below it the surface says more scored history is needed instead of
	// drawing five bars off a handful of messages, which would be false
	// precision dressed as a personality reading.
	traitSketchMinMessages = 50
	// sentimentMonthsShown caps the over-time series to the most recent buckets,
	// so a fifteen-year archive renders a readable strip rather than 180 rows.
	sentimentMonthsShown = 24
)

// sentimentBar is one labelled text+bar row. Every value is precomputed here so
// the template emits attributes only: the strict CSP forbids inline style, and
// SPEC-0027 forbids colour-alone encoding, so the label and the signed number
// carry the meaning and the bar is redundant reinforcement.
//
// Value/Max feed a <progress> element. Because a construct score is signed
// (-1..+1) it is mapped onto 0..progressSpan with zero at the midpoint, and the
// TEXT still shows the signed number — a viewer reading only the text loses
// nothing.
type sentimentBar struct {
	Label string
	// Text is the human reading of the number ("+0.34", "leaning tense").
	Text string
	// Detail is the sample the row rests on ("18 scores"), stated per row so a
	// bar drawn from two messages cannot pass for one drawn from two hundred.
	Detail string
	Value  int
	Max    int
	// Direction is the reading in WORDS ("leans positive" / "leans negative" /
	// "about even"). SPEC-0027 forbids colour-alone encoding, and a progress bar
	// alone is length-alone encoding with the same problem: a reader who cannot
	// judge a bar's midpoint gets the meaning from this string instead.
	Direction string
	// Positive/Negative/Neutral expose the same fact to the template without it
	// having to compare strings.
	Positive bool
	Negative bool
	Neutral  bool
}

// progressSpan is the integer span a signed [-1,+1] score is mapped onto for the
// <progress> element (zero sits at the midpoint).
const progressSpan = 200

// sentimentBarWords returns the three-state reading for a bar. modeValence
// selects the wording family: the profile's valence rows read "leans
// positive/negative/about even" (a valence direction), while per-construct
// facet rows read "expressed/absent/mixed" (issue #434: +0.60 on Anger is
// anger EXPRESSED — "leans positive" is actively wrong there). Both families
// share the same threshold and flag semantics.
func sentimentBarWords(mean float64, valence bool) (dir string, pos, neg, neutral bool) {
	switch {
	case mean >= sentiment.MoodThreshold:
		if valence {
			return "leans positive", true, false, false
		}
		return "expressed", true, false, false
	case mean <= -sentiment.MoodThreshold:
		if valence {
			return "leans negative", false, true, false
		}
		return "absent", false, true, false
	default:
		if valence {
			return "about even", false, false, true
		}
		return "mixed", false, false, true
	}
}

// newFacetBar is newSentimentBar with the facet wording family (#434).
func newFacetBar(label string, mean float64, n int) sentimentBar {
	b := newSentimentBar(label, mean, n)
	b.Direction, b.Positive, b.Negative, b.Neutral = sentimentBarWords(clamp01(mean), false)
	return b
}

func clamp01(m float64) float64 {
	if m > 1 {
		return 1
	}
	if m < -1 {
		return -1
	}
	return m
}

// newSentimentBar maps a signed mean in [-1,+1] onto a labelled row.
func newSentimentBar(label string, mean float64, n int) sentimentBar {
	clamped := mean
	if clamped > 1 {
		clamped = 1
	}
	if clamped < -1 {
		clamped = -1
	}
	b := sentimentBar{
		Label: label,
		Text:  fmt.Sprintf("%+.2f", clamped),
		Value: int((clamped + 1) * progressSpan / 2),
		Max:   progressSpan,
	}
	if n == 1 {
		b.Detail = "1 score"
	} else {
		b.Detail = fmt.Sprintf("%d scores", n)
	}
	b.Direction, b.Positive, b.Negative, b.Neutral = sentimentBarWords(clamped, true)
	return b
}

// sentimentProfile is the contact profile's whole sentiment section. Rendered is
// false for a contact who opted out, which suppresses the section entirely —
// heading, empty state and all.
type sentimentProfile struct {
	// Rendered gates the ENTIRE section. False means either the contact opted
	// out (no sentiment, no traits, no invitation to score them) or the archive
	// has no generation configured to read against.
	Rendered bool
	// Configured is false when no chat model is set, so the empty state can point
	// at the LLM tab rather than at a Score button that would refuse.
	Configured bool
	// ScoredMessages is the sample the whole section rests on; Months is the
	// over-time series, oldest first.
	ScoredMessages int
	Months         []sentimentBar
	// Traits is the Big Five sketch, non-empty ONLY at or above
	// traitSketchMinMessages. TraitsWithheld is true when there are scores but
	// not enough of them — the state that renders the "not enough scored
	// messages" explanation instead of a sketch.
	Traits         []sentimentBar
	TraitsWithheld bool
	// TraitThreshold is the minimum stated in the withheld message, so the number
	// in the prose and the number in the code cannot drift.
	TraitThreshold int
}

// HasScores reports whether anything was scored for this contact under the
// current generation.
func (p sentimentProfile) HasScores() bool { return p.ScoredMessages > 0 }

// contactSentiment assembles the profile's sentiment section.
//
// The opt-out check is FIRST and short-circuits before any aggregate is read.
// The store queries would return nothing anyway — the guarantee lives in the SQL
// — but returning a zero-value section here is what stops the page rendering the
// "no scores yet, run scoring" empty state for someone who has asked not to be
// scored.
func (s *Server) contactSentiment(ctx context.Context, contactID int64) (sentimentProfile, error) {
	var p sentimentProfile
	p.TraitThreshold = traitSketchMinMessages

	optedOut, err := s.store.IsSentimentOptedOut(ctx, contactID)
	if err != nil {
		return p, err
	}
	if optedOut {
		return p, nil // Rendered stays false: no sentiment, no traits, no hint
	}

	gen, ok := s.currentSentimentGeneration()
	p.Rendered = true
	p.Configured = ok
	if !ok {
		return p, nil // nothing to read against; the empty state points at /settings/llm
	}

	p.ScoredMessages, err = s.store.ContactScoredMessages(ctx, contactID, gen)
	if err != nil {
		return p, err
	}
	if p.ScoredMessages == 0 {
		return p, nil // empty state; NEVER a fabricated neutral line
	}

	months, err := s.store.ContactSentimentMonths(ctx, contactID, gen)
	if err != nil {
		return p, err
	}
	p.Months = affectBars(months, monthLabel)

	constructs, err := s.store.ContactSentimentConstructs(ctx, contactID, gen)
	if err != nil {
		return p, err
	}
	if p.ScoredMessages >= traitSketchMinMessages {
		p.Traits = traitBars(constructs)
	} else {
		p.TraitsWithheld = true
	}
	return p, nil
}

// affectBars folds per-(bucket, construct) rows into one weighted-mean bar per
// bucket, oldest bucket first, capped at sentimentMonthsShown.
//
// Only AFFECT-tier constructs contribute: the Big Five axes are bipolar
// personality dimensions, and folding "high Conscientiousness" into how a month
// felt would be a category error. sentiment.Valence answers which is which, and
// a construct it does not weight is SKIPPED rather than defaulted to zero — a
// zero weight would silently drag every mean toward neutral.
//
// A bucket below sentiment.MinScores is dropped entirely rather than drawn
// faintly. One salient facet on one message is not a month.
func affectBars(rows []store.SentimentBucketConstruct, label func(string) string) []sentimentBar {
	type acc struct {
		weighted float64
		n        int
	}
	byBucket := map[string]*acc{}
	var order []string
	for _, r := range rows {
		w, ok := sentiment.Valence(r.Construct)
		if !ok {
			continue // domain tier, or a construct this build does not weight
		}
		a := byBucket[r.Bucket]
		if a == nil {
			a = &acc{}
			byBucket[r.Bucket] = a
			order = append(order, r.Bucket)
		}
		a.weighted += w * r.Sum
		a.n += r.N
	}
	sort.Strings(order) // "YYYY-MM" / "YYYY-MM-DD" sort chronologically as text

	out := make([]sentimentBar, 0, len(order))
	for _, bucket := range order {
		a := byBucket[bucket]
		if a.n < sentiment.MinScores {
			continue
		}
		out = append(out, newSentimentBar(label(bucket), a.weighted/float64(a.n), a.n))
	}
	if len(out) > sentimentMonthsShown {
		out = out[len(out)-sentimentMonthsShown:] // keep the most recent
	}
	return out
}

// traitBars folds whole-history construct rows into the Big Five sketch: the
// mean SIGNED score per domain construct, in the lexicon's declared order so the
// five axes always appear in the same places.
//
// Domain scores are NOT valence-weighted. They are bipolar by construction — the
// sign already carries direction (reserved ↔ gregarious) — so weighting them
// would invert half the axis.
func traitBars(rows []store.SentimentBucketConstruct) []sentimentBar {
	lex, err := sentiment.BuildLexicon()
	if err != nil {
		return nil // a broken curation is a build-time bug; render nothing rather than guess
	}
	sums := map[string]float64{}
	counts := map[string]int{}
	for _, r := range rows {
		sums[r.Construct] += r.Sum
		counts[r.Construct] += r.N
	}
	var out []sentimentBar
	for _, c := range lex.Constructs {
		if c.Tier != sentiment.TierDomain {
			continue
		}
		n := counts[c.Name]
		if n == 0 {
			continue // the model never found this axis salient; say nothing about it
		}
		out = append(out, newSentimentBar(c.Name, sums[c.Name]/float64(n), n))
	}
	return out
}

// monthLabel renders a "YYYY-MM" bucket as "Jan 2026". An unparseable bucket
// falls back to itself rather than being dropped.
func monthLabel(bucket string) string {
	if len(bucket) != 7 {
		return bucket
	}
	months := [...]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	m := 0
	if _, err := fmt.Sscanf(bucket[5:7], "%d", &m); err != nil || m < 1 || m > 12 {
		return bucket
	}
	return months[m-1] + " " + bucket[:4]
}

// dayMoodStrip is the journal day view's additive mood reading. Rendered is
// false for a day with no usable scores, and the journal card then renders
// exactly as it did before — SPEC-0027 makes the strip additive and it MUST NOT
// alter the mechanical rollup or the digest.
type dayMoodStrip struct {
	Rendered bool
	// Mood is the fixed enum this fold can produce: "upbeat", "neutral" or
	// "tense", assigned from a switch over the day's mean. MoodClass is derived
	// from it HERE, from those three literals — no model output, request input or
	// stored string ever reaches a class attribute (the rule SPEC-0016
	// REQ-0016-016 sets for the calendar's tints).
	//
	// The journal's fourth mood, "quiet", is deliberately unreachable here. It
	// describes a low-energy DAY — the digest's editorial judgement about how
	// much happened — and nothing in a per-message affect score carries that.
	// Inventing it from a mean near zero would put an editorial word on a
	// mechanical number.
	Mood      string
	MoodClass string
	// Mean is the day's weighted affect mean and Scores the rows it came from —
	// the sample, stated because a mood folded from four scores is not the same
	// claim as one folded from four hundred.
	Mean   float64
	Scores int
	// Facets are the day's most strongly expressed affect constructs, strongest
	// first, so the strip says WHAT was expressed rather than only which way it
	// leaned.
	Facets []sentimentBar
}

// dayMoodFacetLimit caps the facet list so a talkative day does not render
// fifteen rows under the journal card.
const dayMoodFacetLimit = 5

// journalDayMood assembles the day view's mood strip for one UTC day.
//
// The day window is resolved by the store against the SAME UTC frame the
// journal's mechanical rollup uses (ADR-0023: date(ts_unix,'unixepoch'), no
// 'localtime'), so a message sent at 23:30 local on the 1st lands in the same
// bucket on both surfaces. Getting that wrong would double-shift the bucketing
// and make the strip disagree with the message count printed beside it.
//
// The journal denylist is passed through for the same reason the calendar passes
// it: a denylisted thread must not colour the journal any more than it may
// inflate the stat tiles.
func (s *Server) journalDayMood(ctx context.Context, day string) (dayMoodStrip, error) {
	var strip dayMoodStrip
	gen, ok := s.currentSentimentGeneration()
	if !ok {
		return strip, nil // nothing scored under a configured generation
	}
	rows, err := s.store.DaySentiment(ctx, day, gen, s.journalExclude)
	if err != nil {
		return strip, err
	}

	var weighted float64
	facetSums := map[string]float64{}
	facetCounts := map[string]int{}
	for _, r := range rows {
		w, ok := sentiment.Valence(r.Construct)
		if !ok {
			continue
		}
		weighted += w * r.Sum
		strip.Scores += r.N
		facetSums[r.Construct] += r.Sum
		facetCounts[r.Construct] += r.N
	}
	if strip.Scores < sentiment.MinScores {
		return strip, nil // additive: too little to say anything, so say nothing
	}

	strip.Rendered = true
	strip.Mean = weighted / float64(strip.Scores)
	switch {
	case strip.Mean >= sentiment.MoodThreshold:
		strip.Mood = "upbeat"
	case strip.Mean <= -sentiment.MoodThreshold:
		strip.Mood = "tense"
	default:
		strip.Mood = "neutral"
	}
	strip.MoodClass = "cal-day--" + strip.Mood

	names := make([]string, 0, len(facetSums))
	for name := range facetSums {
		names = append(names, name)
	}
	// Evidence-weighted order (#435): |mean| × sqrt(n), so a construct that
	// actually characterised the day outranks a single dramatic score, with
	// the name breaking ties so the order is stable across renders rather
	// than following map iteration. Facets under MinFacetScores are not
	// listed at all — they still count toward the total and mood fold, and
	// the card says so in one line.
	type facet struct {
		name   string
		weight float64
	}
	var listed []facet
	for _, name := range names {
		n := facetCounts[name]
		if n < sentiment.MinFacetScores {
			continue
		}
		mean := facetSums[name] / float64(n)
		listed = append(listed, facet{name, abs(mean) * math.Sqrt(float64(n))})
	}
	sort.Slice(listed, func(i, j int) bool {
		if listed[i].weight != listed[j].weight {
			return listed[i].weight > listed[j].weight
		}
		return listed[i].name < listed[j].name
	})
	for _, f := range listed {
		if len(strip.Facets) == dayMoodFacetLimit {
			break
		}
		n := facetCounts[f.name]
		strip.Facets = append(strip.Facets,
			newFacetBar(f.name, facetSums[f.name]/float64(n), n))
	}
	return strip, nil
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
