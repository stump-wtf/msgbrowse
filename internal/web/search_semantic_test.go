package web

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

// TestSearchSemanticUnavailable: choosing semantic/hybrid with no indexer wired
// (browser mode) renders the "not available" explainer, not a crash or empty
// keyword results — the built index can't answer, and the UI says how to fix it.
func TestSearchSemanticUnavailable(t *testing.T) {
	srv, _, _ := newTestServer(t) // no SetIndexer → semanticAvailable() == false
	for _, mode := range []string{"semantic", "hybrid"} {
		rec := get(t, srv, "/search/results?q=lease&mode="+mode)
		if rec.Code != http.StatusOK {
			t.Fatalf("mode=%s status = %d", mode, rec.Code)
		}
		if body := rec.Body.String(); !contains(body, "Semantic search isn't available yet") {
			t.Errorf("mode=%s should show the unavailable explainer, got: %s", mode, body)
		}
	}
}

// TestSearchUnavailableExplainerPointsAtSearchIndexTab: the explainer above
// tells the user to go build the index, so it has to name the surface that
// actually carries the Build control. That moved from /status to
// /settings/search-index in #368; a link left on /status would send the user to
// a page with nothing to click on, which is the worse failure — the advice
// looks followed and nothing happens.
//
// @joestump 08/22/2026 - Added while reviewing #376, which orphaned this link.
func TestSearchUnavailableExplainerPointsAtSearchIndexTab(t *testing.T) {
	srv, _, _ := newTestServer(t) // no indexer → the explainer renders
	body := get(t, srv, "/search/results?q=lease&mode=semantic").Body.String()
	if !contains(body, `href="/settings/search-index"`) {
		t.Errorf("explainer should link the Search index tab, got: %s", body)
	}
	if contains(body, `href="/status"`) {
		t.Errorf("explainer still links /status, which no longer carries the Build control: %s", body)
	}
}

// TestSearchSemanticResults: with an indexer wired (an embed model + a query
// vector) and a stored embedding, semantic mode returns the message ranked by
// similarity, rendered through the result card with a score chip and WITHOUT an
// FTS <mark> (the vector path has no keyword highlight).
func TestSearchSemanticResults(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	const model = "test-embed"

	convID, err := st.UpsertConversation(ctx, source.Signal, "Vecky")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := time.Parse(signal.TimestampLayout, "2022-06-01 10:00:00")
	m := signal.Message{
		Conversation: "Vecky", Sender: "Vecky", Timestamp: parsed,
		TimestampRaw: "2022-06-01 10:00:00", Body: "the quarterly budget review",
	}
	if _, err := st.ReplaceConversationMessages(ctx, convID, source.Signal, []signal.Message{m}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutEmbedding(ctx, m.HashWithSource(source.Signal), model, []float32{1, 0}); err != nil {
		t.Fatal(err)
	}

	// A query vector parallel to the stored one scores it ~1.0.
	fi := &fakeIndexer{model: model, queryVec: []float32{1, 0}}
	srv.SetIndexer(fi)

	rec := get(t, srv, "/search/results?q=finances&mode=semantic")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "Vecky") {
		t.Errorf("semantic result missing the seeded conversation: %s", body)
	}
	if !contains(body, "score-chip") {
		t.Errorf("semantic result should carry a score chip: %s", body)
	}
	if contains(body, "<mark>") {
		t.Errorf("semantic result should not have an FTS highlight mark")
	}
	if !contains(body, "quarterly budget review") {
		t.Errorf("semantic result snippet should be the message body")
	}
}

// TestSearchSemanticDegradesWhenQueryEmbedFails: an indexer is wired and a model
// is set, but EmbedQuery returns nil (embedding endpoint down). Semantic mode
// must not 500 or invent results — it returns no hits gracefully.
func TestSearchSemanticDegradesWhenQueryEmbedFails(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fi := &fakeIndexer{model: "test-embed", queryVec: nil} // embedding "fails"
	srv.SetIndexer(fi)

	rec := get(t, srv, "/search/results?q=anything&mode=semantic")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := rec.Body.String(); !contains(body, "No messages match") {
		t.Errorf("failed-embed semantic search should show the empty state, got: %s", body)
	}
}

// TestSearchKeywordModeUnaffected: the default keyword mode still highlights and
// carries no score chip (the semantic affordance is scoped to the other modes).
func TestSearchKeywordModeUnaffected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := get(t, srv, "/search/results?q=lease")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "<mark>") {
		t.Errorf("keyword search should still highlight matches")
	}
	if contains(body, "score-chip") {
		t.Errorf("keyword results should not carry a score chip")
	}
	// The results meta now carries elapsed time.
	if !contains(body, "result") || !contains(body, "s") {
		t.Errorf("results meta should include count and elapsed")
	}
}
