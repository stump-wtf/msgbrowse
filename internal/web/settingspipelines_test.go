// Settings Pipeline Tabs — Shared Run History And Boosted Navigation
//
// Covers the machinery the #368 split introduced rather than the card bodies
// (whose idle / running / never-run / unavailable / not-configured states are
// already covered by semanticindex_test.go and journalbuild_test.go, now
// pointed at the new tabs): the single run-history define both pipelines
// render through, and the boosted-partial contract each new tab has to honour
// like every other Settings surface.
//
// @joestump-agent 08/20/2026 - Added with the Settings tab split (#368).
package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/store"
)

// TestSharedRunHistoryLabelsEachPipeline: both pipelines render their recent
// runs through the one pipeline_run_history define, which labels the count
// column per pipeline. SPEC-0004 REQ-0004-010 requires the single define — the
// two hand-maintained copies it replaces had already drifted apart by a column,
// and facts + sentiment were about to copy it twice more.
func TestSharedRunHistoryLabelsEachPipeline(t *testing.T) {
	srv, st := newPipelineServer(t)
	ctx := context.Background()
	seedJournalDays(t, st, "Alice", []string{"2026-01-01"})

	fin := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	eid, err := st.BeginEmbedRun(ctx, "test-embed", fin.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishEmbedRun(ctx, store.EmbedRun{
		ID: eid, FinishedAt: fin, DurationMS: 1200, Embedded: 3, Batches: 1,
	}); err != nil {
		t.Fatal(err)
	}
	jid, err := st.BeginJournalRun(ctx, "test-chat", "", fin.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishJournalRun(ctx, store.JournalRun{
		ID: jid, FinishedAt: fin, DurationMS: 900, Digested: 7,
	}); err != nil {
		t.Fatal(err)
	}

	search := get(t, srv, "/settings/search-index").Body.String()
	journal := get(t, srv, "/settings/journal").Body.String()

	for _, want := range []string{"Recent runs", ">Embedded<", "Completed"} {
		if !contains(search, want) {
			t.Errorf("search-index history missing %q", want)
		}
	}
	for _, want := range []string{"Recent runs", ">Digested<", "Completed"} {
		if !contains(journal, want) {
			t.Errorf("journal history missing %q", want)
		}
	}
	// The count column is labelled per pipeline, so neither tab wears the
	// other's unit.
	if contains(search, ">Digested<") {
		t.Error("search-index history labels its count column Digested")
	}
	if contains(journal, ">Embedded<") {
		t.Error("journal history labels its count column Embedded")
	}
	// Scope is journal-only: embeddings have no per-run scope, and the shared
	// define drops the column rather than rendering a dead one.
	if !contains(journal, ">Scope<") || !contains(journal, "Whole archive") {
		t.Error("journal history missing its Scope column")
	}
	if contains(search, ">Scope<") {
		t.Error("search-index history renders a Scope column it has no data for")
	}
}

// TestRunHistoryOmittedWhenNoRuns: a pipeline that has never run renders no
// empty table shell — the same "never" story the coverage line tells.
func TestRunHistoryOmittedWhenNoRuns(t *testing.T) {
	srv, _ := newPipelineServer(t)
	for _, tab := range []string{"/settings/search-index", "/settings/journal"} {
		body := get(t, srv, tab).Body.String()
		if contains(body, "Recent runs") {
			t.Errorf("%s renders a run-history table with no runs recorded", tab)
		}
	}
}

// TestRunHistoryEscapesRunErrors: run error strings are endpoint output, not
// trusted markup. The shared define is the one place they render now, so this
// is the one place the escaping has to hold.
func TestRunHistoryEscapesRunErrors(t *testing.T) {
	srv, st := newPipelineServer(t)
	ctx := context.Background()
	id, err := st.BeginEmbedRun(ctx, "test-embed", time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishEmbedRun(ctx, store.EmbedRun{
		ID: id, FinishedAt: time.Now(), Error: `<script>alert(1)</script>`,
	}); err != nil {
		t.Fatal(err)
	}
	body := get(t, srv, "/settings/search-index").Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("a run error string reached the page as live markup")
	}
	if !contains(body, "&lt;script&gt;") {
		t.Error("the escaped run error is missing entirely")
	}
}

// TestPipelineTabsServeBoostedPartials: both tabs honour the boosted-navigation
// contract every other Settings surface follows (SPEC-0008 REQ-0008-006) — a
// partial request gets <title> + <main id="main-content"> and no document
// shell, so switching tabs swaps only the main region.
func TestPipelineTabsServeBoostedPartials(t *testing.T) {
	srv, _ := newPipelineServer(t)
	for _, tab := range []string{"/settings/search-index", "/settings/journal"} {
		t.Run(tab, func(t *testing.T) {
			rec := getPartial(t, srv, tab)
			if rec.Code != 200 {
				t.Fatalf("status = %d", rec.Code)
			}
			body := rec.Body.String()
			if !contains(body, `<main id="main-content"`) {
				t.Error("partial missing the swap target")
			}
			if !contains(body, "<title>") {
				t.Error("partial missing the title htmx lifts into history")
			}
			if contains(body, "<!DOCTYPE html>") || contains(body, "<body") {
				t.Error("partial carried the full document shell")
			}
			// The per-session setup token must not be snapshotted into htmx's
			// localStorage history cache, same opt-out the other privileged
			// surfaces use.
			if !contains(body, `hx-history="false"`) {
				t.Error("partial missing hx-history=\"false\" on the token-bearing region")
			}
		})
	}
}
