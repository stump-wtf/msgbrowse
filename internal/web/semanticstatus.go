// Semantic-search index status for the Status page: coverage, the classified
// latest run, and the recent-run history table. This is the display layer over
// the store's embed_runs bookkeeping (BeginEmbedRun / heartbeat / FinishEmbedRun)
// and the EmbeddingCoverage aggregate — assembled here so status.html stays
// logic-free. The in-app Build / Reset controls and the live-refresh fragment
// live in semanticindex.go.
package web

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// embedStatusData drives the Status page's "Semantic search index" card. All
// display strings are precomputed here (the logic-free-template rule).
type embedStatusData struct {
	// Configured is false while llm.embed_model is unset — the card then
	// renders a pointer to Settings → LLM instead of fake zeros.
	Configured bool
	Model      string
	// Embedded / Embeddable / Percent are the coverage aggregate: how much of
	// the embeddable corpus (non-system, non-blank messages) carries a vector
	// for the configured model.
	Embedded   int
	Embeddable int
	Percent    int
	// InProgress marks a live indexing run (an unfinished embed_runs row with
	// a fresh heartbeat); RunEmbedded is its progress so far.
	InProgress  bool
	RunEmbedded int
	// Stalled marks an unfinished run whose heartbeat went stale — the embed
	// process died before its terminal write.
	Stalled bool
	// HasLastRun gates the last-completed-run line.
	HasLastRun     bool
	LastFinished   string // local "2006-01-02 15:04"
	LastEmbedded   int
	LastDurationMS int64
	// LastModel is the embed model the last completed run used. LatestEmbedRun
	// is model-agnostic while coverage above is scoped to the configured model,
	// so when a user switches llm.embed_model the two halves can describe
	// different models; the template surfaces LastModel when it differs from
	// Model so the disagreement is explained rather than silent.
	LastModel string
	// LastError is the abort reason when the last run failed ("" on success).
	// Server-owned prose from the embed pipeline, escaped like everything else.
	LastError string
}

// embedRunView is one row of the Status page's index run-history table: a
// completed / running / interrupted / failed pass, with its display strings
// precomputed (the logic-free-template rule). Badge is the sync-badge modifier
// suffix ("info" / "ok" / "warn" / "err").
type embedRunView struct {
	Started  string // local "2006-01-02 15:04"
	Status   string // "Completed" | "Running" | "Interrupted" | "Failed"
	Badge    string // sync-badge-<Badge>
	Embedded int
	Duration string // "3.2s" for a finished run, "—" otherwise
	Model    string
	Error    string // abort reason on a failed run, else ""
}

// embedRunStaleAfter is how fresh an unfinished run's heartbeat must be to
// read as "indexing in progress". The heartbeat moves once per batch (a single
// /embeddings request), so anything older than this means the embed process
// died before its terminal write; report a crashed run rather than spinning
// forever. The window is generous on purpose: a large batch against a slow
// local endpoint can take many minutes, and misreading a live run as
// "Interrupted" invites a second concurrent `msgbrowse embed` against the same
// SQLite file — the costlier error.
const embedRunStaleAfter = 30 * time.Minute

// embedStatusTimeFormat is the freshness-stamp layout for the card's run times.
const embedStatusTimeFormat = "2006-01-02 15:04"

// embeddingStatus assembles the semantic-index status: coverage for the
// currently configured embed model (live via the LLM tab when wired, else boot
// config) plus the latest recorded index run, classified live / stalled /
// finished by its heartbeat.
func (s *Server) embeddingStatus(ctx context.Context) (embedStatusData, error) {
	model := strings.TrimSpace(s.currentLLM().EmbedModel)
	d := embedStatusData{Model: model, Configured: model != ""}
	if !d.Configured {
		return d, nil
	}
	cov, err := s.store.EmbeddingCoverage(ctx, model)
	if err != nil {
		return d, err
	}
	d.Embedded, d.Embeddable = cov.Embedded, cov.Embeddable
	if cov.Embeddable > 0 {
		d.Percent = cov.Embedded * 100 / cov.Embeddable
	}
	run, err := s.store.LatestEmbedRun(ctx)
	if err != nil {
		return d, err
	}
	switch {
	case run == nil:
		// Never indexed; the coverage line plus the template's run-it hint
		// carry the story.
	case run.InFlight() && time.Since(run.UpdatedAt) <= embedRunStaleAfter:
		d.InProgress = true
		d.RunEmbedded = run.Embedded
	case run.InFlight():
		d.Stalled = true
	default:
		d.HasLastRun = true
		d.LastFinished = run.FinishedAt.Local().Format(embedStatusTimeFormat)
		d.LastEmbedded = run.Embedded
		d.LastDurationMS = run.DurationMS
		d.LastError = run.Error
		d.LastModel = run.Model
	}
	return d, nil
}

// embedRunHistory returns the most recent index runs classified for display
// (newest first, capped at n), the same live/stalled/finished reasoning
// embeddingStatus applies to the single latest run.
func (s *Server) embedRunHistory(ctx context.Context, n int) ([]embedRunView, error) {
	runs, err := s.store.RecentEmbedRuns(ctx, n)
	if err != nil {
		return nil, err
	}
	out := make([]embedRunView, 0, len(runs))
	for _, r := range runs {
		v := embedRunView{
			Started:  r.StartedAt.Local().Format(embedStatusTimeFormat),
			Embedded: r.Embedded,
			Model:    r.Model,
			Duration: "—",
		}
		switch {
		case r.InFlight() && time.Since(r.UpdatedAt) <= embedRunStaleAfter:
			v.Status, v.Badge = "Running", "info"
		case r.InFlight():
			v.Status, v.Badge = "Interrupted", "warn"
		case r.Error != "":
			v.Status, v.Badge, v.Error = "Failed", "err", r.Error
			v.Duration = formatDurationMS(r.DurationMS)
		default:
			v.Status, v.Badge = "Completed", "ok"
			v.Duration = formatDurationMS(r.DurationMS)
		}
		out = append(out, v)
	}
	return out, nil
}

// formatDurationMS renders a run duration compactly: sub-second in ms, else
// seconds with one decimal (e.g. "840 ms", "3.2s").
func formatDurationMS(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%d ms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}
