// The Journal page's build-card view data (#240): coverage, the latest run's
// live / interrupted / finished classification, and the recent-run history —
// the journal's counterpart to overviewEmbedding + embedRunHistory, applying the
// same heartbeat reasoning so the card and its history table can never disagree
// about whether a run is live. The controls themselves live in journalbuild.go.
package web

import (
	"context"
	"time"
)

// journalBuildData drives the Journal page's build card. Every display string is
// precomputed here, per the logic-free-template rule the overview cards follow.
type journalBuildData struct {
	// Available is true when a JournalBuilder is wired. False renders an
	// explanation and NO controls — never a disabled button shell.
	Available bool
	Model     string // current chat model ("" = digests will be skipped)
	DigestOn  bool

	Days     int // days with activity (journal_days rows)
	Digested int // of those, how many carry a digest
	Stale    int // of those, how many predate later messages on the same day
	Percent  int

	// BuiltThrough is the newest day the mechanical layer covers; NewestDay is
	// the newest day with messages at all. When they differ the layer is behind.
	BuiltThrough string
	NewestDay    string

	InProgress  bool // live journal_runs heartbeat
	Stalled     bool // unfinished run whose heartbeat went cold
	RunDigested int  // digests written so far in the live run
	// Running is the in-process single-flight flag. It bridges the gap between
	// the click and the first heartbeat row, so the buttons disable immediately
	// instead of flickering back to enabled for a second.
	Running bool

	HasLastRun     bool
	LastFinished   string
	LastDigested   int
	LastDurationMS int64
	LastError      string

	History []pipelineRunView
}

// Busy reports whether anything is running right now, from either signal. The
// template disables every control on it.
func (d journalBuildData) Busy() bool { return d.InProgress || d.Running }

// journalBuildStatus assembles the build card.
//
// Unlike EmbeddingCoverage this reads a few thousand tiny journal_days rows
// rather than scanning messages, so it is safe on every Journal render.
func (s *Server) journalBuildStatus(ctx context.Context) (journalBuildData, error) {
	d := journalBuildData{Running: s.journalJobRunning()}
	if b := s.journalBuilder; b != nil {
		d.Available = true
		d.Model = b.ChatModel()
		d.DigestOn = b.DigestEnabled()
	}

	cov, err := s.store.JournalCoverage(ctx)
	if err != nil {
		return d, err
	}
	d.Days, d.Digested, d.Stale = cov.Days, cov.Digested, cov.Stale
	d.BuiltThrough = cov.BuiltThrough
	if cov.Days > 0 {
		d.Percent = cov.Digested * 100 / cov.Days
	}
	// The mechanical layer can legitimately lag the archive (a fresh import with
	// no journal run since). Surfacing the newest MESSAGE day beside the newest
	// BUILT day says so without a GROUP BY over messages.
	if newest, err := s.store.NewestMessageTS(ctx); err == nil && len(newest) >= 10 {
		d.NewestDay = newest[:10]
	}

	run, err := s.store.LatestJournalRun(ctx)
	if err != nil {
		return d, err
	}
	switch {
	case run == nil:
		// Never built; the coverage line plus the template's hint carry the story.
	case run.InFlight() && time.Since(run.UpdatedAt) <= journalRunStaleAfter:
		d.InProgress = true
		d.RunDigested = run.Digested
	case run.InFlight():
		d.Stalled = true
	default:
		d.HasLastRun = true
		d.LastFinished = run.FinishedAt.Local().Format(overviewTimeFormat)
		d.LastDigested = run.Digested
		d.LastDurationMS = run.DurationMS
		d.LastError = run.Error
	}

	d.History, err = s.journalRunHistory(ctx, journalRunHistoryLimit)
	if err != nil {
		return d, err
	}
	return d, nil
}

// journalRunHistory returns the most recent journal passes classified for
// display, newest first, capped at n.
//
// Rows are [pipelineRunView], shared with the semantic index; Count carries the
// digested-day count, which the Journal tab labels "Digested". Unlike
// embeddings, a journal pass has a scope (the whole archive or one day), so the
// shared table renders its Scope column.
func (s *Server) journalRunHistory(ctx context.Context, n int) ([]pipelineRunView, error) {
	runs, err := s.store.RecentJournalRuns(ctx, n)
	if err != nil {
		return nil, err
	}
	out := make([]pipelineRunView, 0, len(runs))
	for _, r := range runs {
		v := pipelineRunView{
			Started: r.StartedAt.Local().Format(overviewTimeFormat),
			Scope:   "Whole archive",
			Count:   r.Digested,
			Model:   r.Model,
		}
		if r.Scope != "" {
			v.Scope = r.Scope
		}
		v.classify(r.InFlight(), time.Since(r.UpdatedAt) > journalRunStaleAfter, r.Error, r.DurationMS)
		out = append(out, v)
	}
	return out, nil
}
