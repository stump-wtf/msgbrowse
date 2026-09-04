// The Sentiment Tab's Card Data
//
// Coverage, the latest run's live / interrupted / finished classification, and
// the recent-run history for the Settings → Sentiment tab — the scoring
// counterpart to journalBuildData and factsBuildData, applying the same
// heartbeat reasoning so the card and its history table can never disagree about
// whether a run is live. The controls themselves live in sentiment.go.
//
// Coverage here answers a question no surface could answer at all before #367.
// On the live archive message_sentiment and sentiment_state were both empty
// across 2,438 conversations, and nothing distinguished "scoring has never run"
// from "scoring ran and found nothing" — because scoring had no in-app entry
// point, so it had simply never run. sentiment_state records one row per
// conversation the engine LOOKED AT, including the ones that yielded nothing, so
// Processed and Productive are reported separately rather than collapsed into
// one misleading number. Storage is sparse BY DESIGN (SPEC-0027: the model omits
// constructs it has no evidence for), so a processed-but-rowless conversation is
// the normal outcome for a thread of logistics chatter, not a failure.
//
// The generation is stated on the card because it is load-bearing rather than
// trivia: scores are only comparable within one (model, lexicon_version) pair
// and every consumer surface filters on it, so changing the chat model makes
// existing scores invisible and schedules a full rescan. A card that reported
// only a percentage would make that look like data loss.
//
// Governing: SPEC-0027 (sentiment) — the incremental cursor is what Processed
// counts, the exclude list bounds the denominator, and opt-outs are reported
// rather than silently shrinking it; ADR-0028 (why the cost statement is on the
// control); SPEC-0004 REQ-0004-010 (one pipeline, one tab, one shared
// run-history define).
//
// @joestump-agent 08/23/2026 - Added with the in-app scoring controls (#367).
package web

import (
	"context"
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/sentiment"
	"github.com/joestump/msgbrowse/internal/store"
)

const (
	// sentimentRunHistoryLimit caps the run-history table. Recent runs tell the
	// track record without turning the card into an unbounded log.
	sentimentRunHistoryLimit = 8
	// sentimentRunStaleAfter is how long an unfinished run's heartbeat may go
	// cold before it reads as interrupted rather than live — the same reasoning
	// as factsRunStaleAfter. Scoring heartbeats once per finished conversation,
	// and one long conversation against a slow local endpoint can take many
	// minutes, so the window is generous: reading a live run as crashed would let
	// a second billable run start alongside it.
	sentimentRunStaleAfter = 30 * time.Minute
)

// sentimentBuildData drives the Sentiment tab's scoring card. Every display
// value is precomputed here, per the logic-free-template rule the other cards
// follow.
type sentimentBuildData struct {
	// Available is true when a SentimentScorer is wired. False renders an
	// explanation and NO controls — never a disabled button shell.
	Available bool
	// Model is the current chat model, "" when unset. Like the facts pipeline's
	// and unlike the journal's, "" means the pipeline REFUSES to run.
	Model string
	// LexiconVersion is the curation this build scores under; with Model it is
	// the generation every consumer surface filters on.
	LexiconVersion string

	// Conversations is the eligible population (contact-linked, holding real
	// messages, not excluded); Processed is how many carry a scoring cursor
	// under the CURRENT generation; Productive is how many yielded score rows.
	Conversations int
	Processed     int
	Productive    int
	// Remaining is the eligible conversations scoring has never read under this
	// generation — the scope, and therefore the COST, of the next run.
	Remaining int
	Percent   int
	// Scores / ScoredMessages / Contacts are the archive-wide totals for the
	// generation; OptedOut is how many contacts excluded themselves.
	Scores         int
	ScoredMessages int
	Contacts       int
	OptedOut       int

	InProgress bool // live sentiment_runs heartbeat
	Stalled    bool // unfinished run whose heartbeat went cold
	// Running is the in-process single-flight flag. It bridges the gap between
	// the click and the first heartbeat row, so the buttons disable immediately
	// instead of flickering back to enabled for a second.
	Running bool
	// RunConversations / RunScores are the in-flight run's live counters.
	RunConversations int
	RunScores        int

	HasLastRun     bool
	LastFinished   string
	LastScores     int
	LastDurationMS int64
	LastError      string

	History []pipelineRunView
}

// Busy reports whether anything is running right now, from either signal. The
// template disables every control on it.
func (d sentimentBuildData) Busy() bool { return d.InProgress || d.Running }

// currentSentimentGeneration is the (model, lexicon_version) pair every
// sentiment READ filters on: the CURRENTLY CONFIGURED chat model plus the
// lexicon this build ships.
//
// SPEC-0027 says "currently configured", not "whatever ran last", and the
// difference matters. Scores from two models are not comparable, so a profile
// that averaged them would be reporting a number that describes neither. Pinning
// on the configured generation means changing the chat model hides the old
// scores immediately — which is the honest outcome, and the same event that
// schedules a rescan (a stored cursor whose generation differs rescans from the
// top).
//
// ok is false when no chat model is configured at all, and every caller treats
// that as "nothing to show" rather than reading across generations. The scorer
// seam is the source when one is wired, because it reads the LIVE holder; the
// boot config is the fallback so a browser-mode server with no scorer can still
// render scores an earlier CLI run produced under that same configured model.
func (s *Server) currentSentimentGeneration() (store.SentimentGeneration, bool) {
	model, lexicon := "", sentiment.LexiconVersion
	if sc := s.sentimentScorer; sc != nil {
		model, lexicon = sc.ChatModel(), sc.LexiconVersion()
	} else {
		model = s.llmBoot.ChatModel
	}
	if model == "" || lexicon == "" {
		return store.SentimentGeneration{}, false
	}
	return store.SentimentGeneration{Model: model, LexiconVersion: lexicon}, true
}

// sentimentBuildStatus assembles the scoring card.
//
// Like FactCoverage — and unlike EmbeddingCoverage — this reads a few thousand
// tiny conversation and cursor rows plus three indexed aggregates rather than
// scanning message bodies, so it is safe on every render of the tab and on the
// 2s progress poll.
func (s *Server) sentimentBuildStatus(ctx context.Context) (sentimentBuildData, error) {
	d := sentimentBuildData{Running: s.sentimentJobRunning(), LexiconVersion: sentiment.LexiconVersion}
	if sc := s.sentimentScorer; sc != nil {
		d.Available = true
		d.Model = sc.ChatModel()
		d.LexiconVersion = sc.LexiconVersion()
	}

	// The generation the coverage figures describe. With no model configured the
	// pipeline cannot run at all, so the counts are reported against the empty
	// generation — which yields zeros, matching the card's "cannot run" state
	// rather than showing totals from a model that is no longer selected.
	gen, _ := s.currentSentimentGeneration()

	// The SAME exclude list the engine itself honors, so the card's denominator
	// is the population a run would actually process. Without it the card would
	// report a percentage that can never move on an archive whose remaining
	// threads are all denylisted.
	cov, err := s.store.SentimentCoverage(ctx, gen, s.journalExclude)
	if err != nil {
		return d, err
	}
	d.Conversations, d.Processed, d.Productive = cov.Conversations, cov.Processed, cov.Productive
	d.Scores, d.ScoredMessages, d.Contacts = cov.Scores, cov.Messages, cov.Contacts
	d.OptedOut, d.Remaining = cov.OptedOut, cov.Remaining()
	if cov.Conversations > 0 {
		d.Percent = cov.Processed * 100 / cov.Conversations
	}

	run, err := s.store.LatestSentimentRun(ctx)
	if err != nil {
		return d, err
	}
	switch {
	case run == nil:
		// Never scored; the coverage line plus the template's hint carry the story.
	case run.InFlight() && time.Since(run.UpdatedAt) <= sentimentRunStaleAfter:
		d.InProgress = true
		d.RunConversations = run.Conversations
		d.RunScores = run.ScoresWritten
	case run.InFlight():
		d.Stalled = true
	default:
		d.HasLastRun = true
		d.LastFinished = run.FinishedAt.Local().Format(overviewTimeFormat)
		d.LastScores = run.ScoresWritten
		d.LastDurationMS = run.DurationMS
		d.LastError = run.Error
	}
	// "Last run" means the most recent FINISHED run (issue #443), independent
	// of whether a newer one is in flight or stalled.
	if !d.HasLastRun {
		if rerr := s.fillLastFinishedSentimentRun(ctx, &d); rerr != nil {
			return d, rerr
		}
	}

	d.History, err = s.sentimentRunHistory(ctx, sentimentRunHistoryLimit)
	if err != nil {
		return d, err
	}
	return d, nil
}

// fillLastFinishedSentimentRun finds the most recent finished scoring pass in
// the history window and fills the tile from it (issue #443).
func (s *Server) fillLastFinishedSentimentRun(ctx context.Context, d *sentimentBuildData) error {
	runs, err := s.store.RecentSentimentRuns(ctx, sentimentRunHistoryLimit)
	if err != nil {
		return err
	}
	for _, r := range runs {
		if r.InFlight() {
			continue
		}
		d.HasLastRun = true
		d.LastFinished = r.FinishedAt.Local().Format(overviewTimeFormat)
		d.LastScores = r.ScoresWritten
		d.LastDurationMS = r.DurationMS
		d.LastError = r.Error
		return nil
	}
	return nil
}

// sentimentRunHistory returns the most recent scoring passes classified for
// display, newest first, capped at n.
//
// Rows are [pipelineRunView], shared with the journal, the semantic index and
// the facts pipeline; Count carries the score-row count, which the Sentiment tab
// labels "Scored". Like the journal and facts — and unlike embeddings — a pass
// HAS a scope (the whole archive, a reset, one conversation), so the shared
// table renders its Scope column. The scope is mapped from the run's fixed
// TOKEN, never printed raw.
//
// The Model column carries the model AND the lexicon version, because a rescan
// caused by a curation bump is otherwise indistinguishable from a spurious one.
func (s *Server) sentimentRunHistory(ctx context.Context, n int) ([]pipelineRunView, error) {
	runs, err := s.store.RecentSentimentRuns(ctx, n)
	if err != nil {
		return nil, err
	}
	out := make([]pipelineRunView, 0, len(runs))
	for _, r := range runs {
		model := r.Model
		if r.LexiconVersion != "" {
			model += " · lexicon " + r.LexiconVersion
		}
		v := pipelineRunView{
			Started: r.StartedAt.Local().Format(overviewTimeFormat),
			Scope:   sentimentScopeLabel(r.Scope),
			Count:   r.ScoresWritten,
			Model:   model,
		}
		v.classify(r.InFlight(), time.Since(r.UpdatedAt) > sentimentRunStaleAfter, r.Error, r.DurationMS)
		out = append(out, v)
	}
	return out, nil
}

// sentimentScopeLabel maps a stored sentiment_runs scope TOKEN to its display
// string. An unrecognized token reads as the default whole-archive scope rather
// than being printed verbatim: the column must never become a channel for text
// this code did not author.
func sentimentScopeLabel(scope string) string {
	switch {
	case scope == store.SentimentScopeReset:
		return "Reset & rescore"
	case scope == store.SentimentScopeConversation:
		return "Single conversation"
	case strings.HasPrefix(scope, store.SentimentScopeDayPrefix):
		// The suffix is a server-generated day stamp (#441), but the display
		// rule stays conservative: the date reaches the page only through the
		// fixed prefix + shape check, never verbatim.
		if day, ok := strings.CutPrefix(scope, store.SentimentScopeDayPrefix); ok && len(day) == 10 {
			return "Day " + day
		}
		return "Single day"
	default:
		return "Whole archive"
	}
}
