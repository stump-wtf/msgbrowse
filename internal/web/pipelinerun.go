// Pipeline Run History — One Row Shape For Every Derived-Data Pipeline
//
// Every pipeline msgbrowse runs against the archive — semantic-search
// embeddings, journal digests, and (in flight) contact facts and sentiment
// scoring — keeps the same kind of track record: when a pass started, whether
// it finished, how much it produced, how long it took, and which model it used.
// This file holds the one view type all of them render through, so the recent-
// runs table is written once and parameterised per pipeline rather than
// hand-maintained per card (SPEC-0004 REQ-0004-010).
//
// Count is deliberately generic: the shared template labels the column from the
// caller's "unit" ("Embedded", "Digested"), so a new pipeline joins by naming
// its unit rather than by copying the table. Scope is optional — empty omits
// the column entirely, which is what a pipeline with no per-run scope wants.
//
// @joestump-agent 08/20/2026 - Extracted from the two near-identical run-history
// tables in status.html (embedRunView / journalRunView), which had already
// drifted apart by a column and were about to be copied twice more for facts
// and sentiment.
package web

// pipelineRunView is one row of a pipeline's recent-runs table, with its
// display strings precomputed (the logic-free-template rule). Badge is the
// sync-badge modifier suffix ("info" / "ok" / "warn" / "err").
type pipelineRunView struct {
	Started string // local "2006-01-02 15:04"
	// Scope is the run's subject when a pipeline has one ("Whole archive", a
	// single day). Empty means the pipeline has no scope concept and the
	// shared template omits the column.
	Scope    string
	Status   string // "Completed" | "Running" | "Interrupted" | "Failed"
	Badge    string // sync-badge-<Badge>
	Count    int    // rows produced, labelled by the caller's unit
	Duration string // "3.2s" for a finished run, "—" otherwise
	Model    string
	Error    string // abort reason on a failed run, else ""
}

// classify applies the shared live / stalled / failed / finished reasoning to a
// run row. inFlight and stale come from the pipeline's own heartbeat rules —
// each has its own staleness window — so the caller decides liveness and this
// decides how it reads. durationMS is rendered only for a terminal run: an
// in-flight pass has no meaningful duration yet.
func (v *pipelineRunView) classify(inFlight, stale bool, errText string, durationMS int64) {
	v.Duration = "—"
	switch {
	case inFlight && !stale:
		v.Status, v.Badge = "Running", "info"
	case inFlight:
		v.Status, v.Badge = "Interrupted", "warn"
	case errText != "":
		v.Status, v.Badge, v.Error = "Failed", "err", errText
		v.Duration = formatDurationMS(durationMS)
	default:
		v.Status, v.Badge = "Completed", "ok"
		v.Duration = formatDurationMS(durationMS)
	}
}
