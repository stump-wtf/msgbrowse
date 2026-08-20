// Reading Surfaces Carry No Pipeline Status
//
// The regression guard for SPEC-0016 REQ-0016-017 and SPEC-0004 REQ-0004-010:
// /journal and / are reading surfaces and MUST NOT render digest coverage, a
// progress bar, the chat model, built-through, run history, run error strings,
// or Build / Rebuild controls. Pipeline status belongs to one Settings tab per
// pipeline.
//
// This exists because the markup kept coming back. It was re-added to /journal
// across five separate requests — reasonably, since nothing forbade it and
// issue #274 positively required the Journal page to "keep its Build / Rebuild
// controls", so an agent following the written requirement produced the
// regression. #374 wrote the prohibition into the spec; this asserts it, which
// is the half that survives the next agent.
//
// The test deliberately drives the WORST case — a partially digested journal
// with a failed run recorded and an indexer wired — because that is the state
// that produced the original complaint: an eight-row table of failed LLM runs
// on top of the archive's most narrative surface. A guard run against an empty
// archive would pass while rendering nothing at all.
//
// @joestump-agent 08/20/2026 - Added with the Settings tab split (#368).
package web

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/store"
)

// newPipelineServer wires BOTH pipelines fully configured — an embed model, an
// indexer and a journal builder — so each card renders its full body rather
// than its "not configured" note. A guard against a half-configured server
// would pass on markers the page never had the data to render.
func newPipelineServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "pipelines-web.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{DataDir: t.TempDir()}
	cfg.LLM.EmbedModel = "test-embed"
	srv, err := NewServer(st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srv.SetIndexer(newFakeIndexer("test-embed"))
	srv.SetJournalBuilder(newFakeJournalBuilder("test-chat", true))
	return srv, st
}

// pipelineMarkers are the strings that betray a pipeline card. They are the
// literal titles, column headings and control labels the cards render, so a
// card returning to a reading surface trips at least one of them regardless of
// which pipeline it belongs to.
var pipelineMarkers = []string{
	"Build journal",
	"Rebuild all",
	"Digest coverage",
	"Recent runs",
	"Semantic search index",
	"Journal build",
	"Built through",
	"Reset &amp; rebuild",
	"Build index",
}

// TestReadingSurfacesCarryNoPipelineStatus asserts the prohibition on both
// reading surfaces, in the state most likely to violate it.
func TestReadingSurfacesCarryNoPipelineStatus(t *testing.T) {
	srv, st := newPipelineServer(t)
	seedPartialJournal(t, st)

	for _, surface := range []string{"/journal", "/"} {
		t.Run(surface, func(t *testing.T) {
			rec := get(t, srv, surface)
			if rec.Code != 200 {
				t.Fatalf("status = %d", rec.Code)
			}
			body := rec.Body.String()
			for _, marker := range pipelineMarkers {
				if contains(body, marker) {
					t.Errorf("%s renders pipeline status %q — it is a reading surface "+
						"(SPEC-0016 REQ-0016-017 / SPEC-0004 REQ-0004-010); the cards live on "+
						"/settings/search-index and /settings/journal", surface, marker)
				}
			}
			// No privileged build form may render here either: the controls are
			// what #274 kept re-authorising, and a form is how they arrive.
			for _, form := range []string{
				`action="/journal/build"`,
				`action="/journal/rebuild"`,
				`action="/status/index"`,
				`action="/status/index/reset"`,
			} {
				if contains(body, form) {
					t.Errorf("%s renders the privileged build form %s", surface, form)
				}
			}
		})
	}
}

// TestPipelineStatusRendersOnItsOwnTab is the other half of the guard: it
// proves the markers above are absent from the reading surfaces because the
// cards MOVED, not because they stopped rendering anywhere. Without this a
// deletion would satisfy the prohibition and quietly remove the feature.
func TestPipelineStatusRendersOnItsOwnTab(t *testing.T) {
	srv, st := newPipelineServer(t)
	seedPartialJournal(t, st)

	journal := get(t, srv, "/settings/journal").Body.String()
	for _, want := range []string{
		"Journal build",
		"Digest coverage",
		"Built through",
		`action="/journal/build"`,
		"Recent runs",
	} {
		if !contains(journal, want) {
			t.Errorf("/settings/journal missing %q", want)
		}
	}
	// Each pipeline's figures stay on its own tab: reading them interleaved is
	// what the combined Status tab produced (REQ-0004-010).
	if contains(journal, "Semantic search index") {
		t.Error("/settings/journal renders the semantic-index card — one tab per pipeline")
	}

	search := get(t, srv, "/settings/search-index").Body.String()
	for _, want := range []string{
		"Semantic search index",
		`action="/status/index"`,
		"Coverage",
	} {
		if !contains(search, want) {
			t.Errorf("/settings/search-index missing %q", want)
		}
	}
	if contains(search, "Journal build") {
		t.Error("/settings/search-index renders the journal build card — one tab per pipeline")
	}

	// Status keeps ingest/source health and sheds both pipelines.
	status := get(t, srv, "/status").Body.String()
	for _, gone := range []string{"Semantic search index", "Journal build", "Digest coverage"} {
		if contains(status, gone) {
			t.Errorf("/status still renders %q — it moved to its own tab (#368)", gone)
		}
	}
	if !contains(status, "Last ingest") {
		t.Error("/status lost the ingest health it is supposed to keep")
	}
}

// seedPartialJournal builds the state that produced the original complaint: days
// with messages, only some digested, and a failed run on the record.
func seedPartialJournal(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	seedJournalDays(t, st, "Alice", []string{"2026-01-01", "2026-01-02", "2026-01-03"})

	id, err := st.BeginJournalRun(ctx, "test-chat", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishJournalRun(ctx, store.JournalRun{
		ID: id, FinishedAt: time.Now(), Error: "provider unreachable",
	}); err != nil {
		t.Fatal(err)
	}
}
