// Where a calendar day's mood comes from, and what a day looks like when it has
// none (#370).
//
// The mood calendar had exactly ONE mood source — journal_digests.mood — and the
// LLM digest pass has never finished on a real archive: on the live database
// 3,920 days carry messages and only 3,591 carry a digest. The other 329
// rendered as a bare `cal-day` with a count subscript and no tint, which is not a
// state anyone chose. It reads as "unremarkable" and means "never analysed", and
// nothing in the legend told the two apart.
//
// So there are now three defined states, in precedence order, and no fourth:
//
//  1. DIGEST — the day has a cached digest with a mood. Full tint. This is an
//     editorial reading of the day, made by a model that actually read it.
//  2. SENTIMENT — no digest, but the day has affect-tier sentiment scores
//     (SPEC-0027 REQ-0027-009). Fainter tint plus a dashed edge, because it is a
//     mechanical fold of per-message affect scores, NOT an editorial reading.
//     The calendar must never imply the digest pass has read a day it has not.
//  3. UNANALYSED — neither. An explicit, legend-documented treatment. A day with
//     messages never falls through to bare `cal-day` again.
//
// A note on cost, because the issue that prompted this got it backwards.
// message_sentiment is NOT a free local lexicon score. ADR-0028 rejected a
// classical local lexicon outright; the IPIP lexicon is an anchor set fed to a
// chat model, and internal/sentiment.Run makes a billable Chat call per batch.
// Sentiment therefore narrows the untinted gap, it does not close it for free —
// which is exactly why state 3 has to be a real, documented state rather than a
// stopgap waiting to be deleted.
//
// Governing: SPEC-0016 REQ-0016-014 (the mood-tinted grid + legend),
// REQ-0016-016 (mood is a fixed enum mapped to a fixed class — never a
// model-supplied class or inline style), SPEC-0027 REQ-0027-009 (per-day mood
// from affect-tier scores, UTC-bucketed), ADR-0028 (why sentiment costs money).
//
// @joestump-agent 08/23/2026 - Split the single digest-only mood source into the
// three-state precedence above (#370).
package web

import (
	"context"

	"time"

	"github.com/joestump/msgbrowse/internal/journal"
	"github.com/joestump/msgbrowse/internal/store"
)

// affectValence maps each affect-tier construct in internal/sentiment's lexicon
// to the direction it pushes a day's tint: +1 for a pleasant facet, -1 for an
// unpleasant one. A day's valence is the mean of every scored row weighted this
// way, which keeps the fold a single number regardless of which facets the model
// happened to find salient in a given message.
//
// Domain-tier constructs (the Big Five axes) are deliberately absent. They are
// bipolar personality axes, not affect — "high Conscientiousness" says nothing
// about whether a day felt good — and REQ-0027-009 scopes the journal's per-day
// mood to the affect tier.
//
// This table is checked against the built lexicon by TestAffectValenceCoversLexicon,
// so adding a facet in internal/sentiment without deciding its valence here fails
// the build rather than silently dropping it out of every tint.
var affectValence = map[string]float64{
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

const (
	// sentimentMoodThreshold is how far a day's mean valence must sit from zero
	// before it reads as upbeat or tense rather than neutral. Scores are clamped
	// to [-1,+1] per message and averaged across a whole day, so day means live
	// close to zero; a wide band here would paint every day neutral, a narrow one
	// would flip the tint on noise.
	sentimentMoodThreshold = 0.12
	// sentimentMinScores is the minimum number of scored rows a day needs before
	// its tint is drawn at all. One salient facet on one message is not a mood,
	// and tinting a day off it would be exactly the false confidence this whole
	// change exists to remove — such a day stays UNANALYSED, which is honest.
	sentimentMinScores = 3
)

// moodClass maps a mood to its fixed CSS class, returning "" for anything not in
// the allowlist.
//
// Governing: SPEC-0016 REQ-0016-016. The mood string reaches this function from
// journal_digests.mood, which is model-derived: internal/journal.parseDigest
// enforces the allowlist on the way in, but a class attribute assembled from
// model output is precisely the thing the requirement forbids, so the allowlist
// is re-checked on the way out too. An unknown mood yields no class, which lands
// the day in the UNANALYSED state rather than emitting `cal-day--<whatever>`.
func moodClass(mood string) string {
	if mood == "" {
		return ""
	}
	for _, m := range journal.Moods {
		if m == mood {
			return "cal-day--" + m
		}
	}
	return ""
}

// sentimentMoods folds a month's per-(day, construct) sentiment aggregates into
// one mood per day, keyed "YYYY-MM-DD".
//
// Only three of the four moods are reachable here, on purpose. "quiet" describes
// a low-energy DAY — the digest's editorial judgement about how much happened —
// and nothing in a per-message affect score carries that. Inventing it from a
// mean near zero would put an editorial word on a mechanical number; a day whose
// valence sits in the middle band is "neutral", which is what the number
// actually says.
func sentimentMoods(rows []store.SentimentDayConstruct) map[string]string {
	type acc struct {
		weighted float64
		n        int
	}
	byDay := make(map[string]*acc)
	for _, r := range rows {
		w, ok := affectValence[r.Construct]
		if !ok {
			continue // domain tier, or a construct this build does not weight
		}
		a := byDay[r.Day]
		if a == nil {
			a = &acc{}
			byDay[r.Day] = a
		}
		a.weighted += w * r.Sum
		a.n += r.N
	}

	out := make(map[string]string, len(byDay))
	for day, a := range byDay {
		if a.n < sentimentMinScores {
			continue
		}
		mean := a.weighted / float64(a.n)
		switch {
		case mean >= sentimentMoodThreshold:
			out[day] = "upbeat"
		case mean <= -sentimentMoodThreshold:
			out[day] = "tense"
		default:
			out[day] = "neutral"
		}
	}
	return out
}

// monthSentimentMoods resolves the sentiment-derived mood for every day in the
// month, returning an empty map when nothing has been scored yet.
//
// Note what this function does NOT do: it does not read the opt-out set and pass
// it down. MonthSentiment enforces contact_sentiment_optout itself, so the
// privacy guarantee does not depend on this caller — or any future one —
// remembering to thread it through (SPEC-0027 REQ-0027-005).
func (s *Server) monthSentimentMoods(ctx context.Context, year int, month time.Month) (map[string]string, error) {
	gen, ok, err := s.store.LatestSentimentGeneration(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil // nothing scored yet: every undigested day stays UNANALYSED
	}
	rows, err := s.store.MonthSentiment(ctx, year, month, gen, s.journalExclude)
	if err != nil {
		return nil, err
	}
	return sentimentMoods(rows), nil
}
