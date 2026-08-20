// The Status page's semantic-index RUN HISTORY: the recent-runs table beside
// the single latest-run line. The coverage + latest-run half of the card is
// already assembled by overviewEmbedding (internal/web/overview.go) and shared
// with the Overview card — this file adds only the track record, applying the
// same live / stalled / finished classification to each row. The Build / Reset
// controls and the live-refresh fragment live in semanticindex.go.
package web

import (
	"context"
	"fmt"
	"time"
)

// embedRunHistory returns the most recent index runs classified for display
// (newest first, capped at n) — the same heartbeat reasoning overviewEmbedding
// applies to the single latest run, so the table and the line above it can
// never disagree about whether a run is live.
//
// Rows are [pipelineRunView], the shape every pipeline's recent-runs table
// shares; Count carries the embedded-message count, which the Search index tab
// labels "Embedded". Embeddings have no per-run scope, so Scope stays empty and
// the shared table drops the column.
func (s *Server) embedRunHistory(ctx context.Context, n int) ([]pipelineRunView, error) {
	runs, err := s.store.RecentEmbedRuns(ctx, n)
	if err != nil {
		return nil, err
	}
	out := make([]pipelineRunView, 0, len(runs))
	for _, r := range runs {
		v := pipelineRunView{
			Started: r.StartedAt.Local().Format(overviewTimeFormat),
			Count:   r.Embedded,
			Model:   r.Model,
		}
		v.classify(r.InFlight(), time.Since(r.UpdatedAt) > embedRunStaleAfter, r.Error, r.DurationMS)
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
