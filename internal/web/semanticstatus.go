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

// embedRunHistory returns the most recent index runs classified for display
// (newest first, capped at n) — the same heartbeat reasoning overviewEmbedding
// applies to the single latest run, so the table and the line above it can
// never disagree about whether a run is live.
func (s *Server) embedRunHistory(ctx context.Context, n int) ([]embedRunView, error) {
	runs, err := s.store.RecentEmbedRuns(ctx, n)
	if err != nil {
		return nil, err
	}
	out := make([]embedRunView, 0, len(runs))
	for _, r := range runs {
		v := embedRunView{
			Started:  r.StartedAt.Local().Format(overviewTimeFormat),
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
