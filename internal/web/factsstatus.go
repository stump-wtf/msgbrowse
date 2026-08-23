// The Facts Tab's Card Data
//
// Coverage, the latest run's live / interrupted / finished classification, and
// the recent-run history for the Settings → Facts tab — the fact-extraction
// counterpart to journalBuildData, applying the same heartbeat reasoning so the
// card and its history table can never disagree about whether a run is live.
// The controls themselves live in facts.go.
//
// Coverage here answers a question no surface could answer at all before #366.
// On a live archive contact_facts and fact_state were both empty across 2,438
// conversations, and nothing distinguished "extraction has never run" from
// "extraction ran and found nothing" — because extraction had no in-app entry
// point, so it had simply never run. fact_state records one row per
// conversation the extractor LOOKED AT, including the ones that yielded
// nothing, so Processed and Productive are reported separately rather than
// collapsed into one misleading number.
//
// Governing: SPEC-0005 (contact-facts) REQ-0005-004 (the incremental cursor is
// what Processed counts), REQ-0005-005 (the exclude list bounds the
// denominator); ADR-0011; SPEC-0004 REQ-0004-010 (one pipeline, one tab, one
// shared run-history define).
//
// @joestump-agent 08/23/2026 - Added with the in-app extraction controls (#366).
package web

import (
	"context"
	"time"

	"github.com/joestump/msgbrowse/internal/store"
)

const (
	// factsRunHistoryLimit caps the run-history table. Recent runs tell the
	// track record without turning the card into an unbounded log.
	factsRunHistoryLimit = 8
	// factsRunStaleAfter is how long an unfinished run's heartbeat may go cold
	// before it reads as interrupted rather than live — the same reasoning as
	// journalRunStaleAfter. Extraction heartbeats once per finished
	// conversation, and one long conversation against a slow local endpoint can
	// take minutes, so the window is generous: reading a live run as crashed
	// would let a second billable run start alongside it.
	factsRunStaleAfter = 30 * time.Minute
)

// factsBuildData drives the Facts tab's extraction card. Every display value is
// precomputed here, per the logic-free-template rule the other cards follow.
type factsBuildData struct {
	// Available is true when a FactsExtractor is wired. False renders an
	// explanation and NO controls — never a disabled button shell.
	Available bool
	// Model is the current chat model, "" when unset. Unlike the journal's, ""
	// means the pipeline REFUSES to run — see startFacts.
	Model string

	// Conversations is the eligible population (contact-linked, holding real
	// messages, not excluded); Processed is how many carry an extraction
	// cursor; Productive is how many of those yielded at least one fact.
	Conversations int
	Processed     int
	Productive    int
	// Remaining is the eligible conversations extraction has never read — the
	// scope, and therefore the COST, of the next incremental run.
	Remaining int
	Percent   int
	// Facts and Contacts are the archive-wide totals the card reports.
	Facts    int
	Contacts int

	InProgress bool // live fact_runs heartbeat
	Stalled    bool // unfinished run whose heartbeat went cold
	// Running is the in-process single-flight flag. It bridges the gap between
	// the click and the first heartbeat row, so the buttons disable immediately
	// instead of flickering back to enabled for a second.
	Running bool
	// RunConversations / RunFacts are the in-flight run's live counters.
	RunConversations int
	RunFacts         int

	HasLastRun     bool
	LastFinished   string
	LastFacts      int
	LastDurationMS int64
	LastError      string

	History []pipelineRunView
}

// Busy reports whether anything is running right now, from either signal. The
// template disables every control on it.
func (d factsBuildData) Busy() bool { return d.InProgress || d.Running }

// factsBuildStatus assembles the extraction card.
//
// Like JournalCoverage — and unlike EmbeddingCoverage — this reads a few
// thousand tiny conversation and fact_state rows rather than scanning messages,
// so it is safe on every render of the tab and on the 2s progress poll.
func (s *Server) factsBuildStatus(ctx context.Context) (factsBuildData, error) {
	d := factsBuildData{Running: s.factsJobRunning()}
	if e := s.factsExtractor; e != nil {
		d.Available = true
		d.Model = e.ChatModel()
	}

	// The SAME exclude list the extractor itself honors (REQ-0005-005), so the
	// card's denominator is the population a run would actually process. Without
	// it the card would report "1,200 of 2,438" forever on an archive whose
	// remaining threads are all excluded, and the user would keep paying for
	// runs chasing a number that cannot move.
	cov, err := s.store.FactCoverage(ctx, s.journalExclude)
	if err != nil {
		return d, err
	}
	d.Conversations, d.Processed, d.Productive = cov.Conversations, cov.Processed, cov.Productive
	d.Facts, d.Contacts, d.Remaining = cov.Facts, cov.Contacts, cov.Remaining()
	if cov.Conversations > 0 {
		d.Percent = cov.Processed * 100 / cov.Conversations
	}

	run, err := s.store.LatestFactRun(ctx)
	if err != nil {
		return d, err
	}
	switch {
	case run == nil:
		// Never extracted; the coverage line plus the template's hint carry the story.
	case run.InFlight() && time.Since(run.UpdatedAt) <= factsRunStaleAfter:
		d.InProgress = true
		d.RunConversations = run.Conversations
		d.RunFacts = run.FactsAdded
	case run.InFlight():
		d.Stalled = true
	default:
		d.HasLastRun = true
		d.LastFinished = run.FinishedAt.Local().Format(overviewTimeFormat)
		d.LastFacts = run.FactsAdded
		d.LastDurationMS = run.DurationMS
		d.LastError = run.Error
	}

	d.History, err = s.factsRunHistory(ctx, factsRunHistoryLimit)
	if err != nil {
		return d, err
	}
	return d, nil
}

// factsRunHistory returns the most recent extraction passes classified for
// display, newest first, capped at n.
//
// Rows are [pipelineRunView], shared with the journal and the semantic index;
// Count carries the facts-added count, which the Facts tab labels "Facts". Like
// the journal — and unlike embeddings — a pass HAS a scope (the whole archive,
// a reset, one conversation), so the shared table renders its Scope column. The
// scope is mapped from the run's fixed TOKEN, never printed raw.
func (s *Server) factsRunHistory(ctx context.Context, n int) ([]pipelineRunView, error) {
	runs, err := s.store.RecentFactRuns(ctx, n)
	if err != nil {
		return nil, err
	}
	out := make([]pipelineRunView, 0, len(runs))
	for _, r := range runs {
		v := pipelineRunView{
			Started: r.StartedAt.Local().Format(overviewTimeFormat),
			Scope:   factScopeLabel(r.Scope),
			Count:   r.FactsAdded,
			Model:   r.Model,
		}
		v.classify(r.InFlight(), time.Since(r.UpdatedAt) > factsRunStaleAfter, r.Error, r.DurationMS)
		out = append(out, v)
	}
	return out, nil
}

// factScopeLabel maps a stored fact_runs scope TOKEN to its display string. An
// unrecognized token reads as the default whole-archive scope rather than being
// printed verbatim: the column must never become a channel for text this code
// did not author.
func factScopeLabel(scope string) string {
	switch scope {
	case store.FactScopeReset:
		return "Reset & re-extract"
	case store.FactScopeConversation:
		return "Single conversation"
	default:
		return "Whole archive"
	}
}
