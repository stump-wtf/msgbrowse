package store

import (
	"context"
	"math"
	"testing"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

func TestEncodeDecodeVec(t *testing.T) {
	in := []float32{0, 1, -1, 3.14159, 1e-7}
	out, err := decodeVec(encodeVec(in), len(in))
	if err != nil {
		t.Fatal(err)
	}
	for i := range in {
		if in[i] != out[i] {
			t.Errorf("roundtrip[%d] = %v, want %v", i, out[i], in[i])
		}
	}
	if _, err := decodeVec(encodeVec(in), len(in)+1); err == nil {
		t.Error("expected dim-mismatch error")
	}
}

func TestCosine(t *testing.T) {
	a := []float32{1, 0}
	if got := cosine(a, []float32{1, 0}, norm(a)); math.Abs(got-1) > 1e-9 {
		t.Errorf("identical vectors cosine = %v, want 1", got)
	}
	if got := cosine(a, []float32{0, 1}, norm(a)); math.Abs(got) > 1e-9 {
		t.Errorf("orthogonal cosine = %v, want 0", got)
	}
	if got := cosine(a, []float32{2, 0}, norm(a)); math.Abs(got-1) > 1e-9 {
		t.Errorf("scaled-parallel cosine = %v, want 1", got)
	}
}

// seedEmbeddingCorpus seeds three messages and returns the store plus their
// hashes (m1, m2, m3).
func seedEmbeddingCorpus(t *testing.T) (*Store, string, string, string) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()
	conv, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	msgs := []signal.Message{
		msg("Harper", "2022-03-01 09:00:00", "Harper", "the lease agreement", nil, nil),
		msg("Harper", "2022-03-01 09:01:00", "Me", "lunch plans tomorrow", nil, nil),
		msg("Harper", "2022-03-01 09:02:00", "Harper", "rent and deposit terms", nil, nil),
	}
	if _, err := st.ReplaceConversationMessages(ctx, conv, source.Signal, msgs); err != nil {
		t.Fatal(err)
	}
	// Use the source-namespaced storage hash (what the store wrote), so the
	// embeddings we PUT below key to the rows that SemanticSearch joins against.
	return st, msgs[0].HashWithSource(source.Signal), msgs[1].HashWithSource(source.Signal), msgs[2].HashWithSource(source.Signal)
}

func TestEmbeddingLifecycle(t *testing.T) {
	st, h1, h2, _ := seedEmbeddingCorpus(t)
	ctx := context.Background()
	const model = "test-embed"

	// All three need embedding initially.
	if n, _ := st.CountMissingEmbeddings(ctx, model); n != 3 {
		t.Errorf("missing = %d, want 3", n)
	}
	targets, err := st.MessagesNeedingEmbedding(ctx, model, 10)
	if err != nil || len(targets) != 3 {
		t.Fatalf("targets = %d, err %v", len(targets), err)
	}

	// Embed two; one remains.
	if err := st.PutEmbedding(ctx, h1, model, []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutEmbedding(ctx, h2, model, []float32{0, 1}); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountMissingEmbeddings(ctx, model); n != 1 {
		t.Errorf("missing after 2 = %d, want 1", n)
	}

	// A different model still sees all three missing.
	if n, _ := st.CountMissingEmbeddings(ctx, "other-model"); n != 3 {
		t.Errorf("missing for other model = %d, want 3", n)
	}

	// Upsert is idempotent (no duplicate row, updates vector).
	if err := st.PutEmbedding(ctx, h1, model, []float32{0.5, 0.5}); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, st, `SELECT count(*) FROM embeddings`); n != 2 {
		t.Errorf("embeddings rows = %d, want 2", n)
	}
}

// TestEmbeddingsCoexistAcrossModels confirms the composite PK (message_hash,
// model): a message can hold vectors for two models at once, so switching
// models doesn't overwrite, and re-running under a prior model is a no-op.
func TestEmbeddingsCoexistAcrossModels(t *testing.T) {
	st, h1, _, _ := seedEmbeddingCorpus(t)
	ctx := context.Background()

	if err := st.PutEmbedding(ctx, h1, "model-a", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutEmbedding(ctx, h1, "model-b", []float32{0, 1, 0}); err != nil { // different dim, too
		t.Fatal(err)
	}
	// Both rows exist for the same hash.
	if n := scalar(t, st, `SELECT count(*) FROM embeddings WHERE message_hash = '`+h1+`'`); n != 2 {
		t.Errorf("rows for hash = %d, want 2 (one per model)", n)
	}
	// model-a is fully embedded for h1; only the other two messages remain.
	if mt, _ := st.MessagesNeedingEmbedding(ctx, "model-a", 10); len(mt) != 2 {
		t.Errorf("model-a missing = %d, want 2", len(mt))
	}
	// model-b likewise has h1 embedded.
	if mt, _ := st.MessagesNeedingEmbedding(ctx, "model-b", 10); len(mt) != 2 {
		t.Errorf("model-b missing = %d, want 2", len(mt))
	}
	// Re-embedding h1 under model-a updates in place (no new row).
	if err := st.PutEmbedding(ctx, h1, "model-a", []float32{0.7, 0.7}); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, st, `SELECT count(*) FROM embeddings WHERE message_hash = '`+h1+`'`); n != 2 {
		t.Errorf("after re-embed rows = %d, want 2", n)
	}
}

func TestSemanticSearchRanksAndFilters(t *testing.T) {
	st, h1, h2, h3 := seedEmbeddingCorpus(t)
	ctx := context.Background()
	const model = "test-embed"

	// Construct a tiny 2-D semantic space: "lease-ish" ≈ [1,0], "lunch" ≈ [0,1].
	if err := st.PutEmbedding(ctx, h1, model, []float32{1, 0.1}); err != nil { // lease agreement
		t.Fatal(err)
	}
	if err := st.PutEmbedding(ctx, h2, model, []float32{0.1, 1}); err != nil { // lunch
		t.Fatal(err)
	}
	if err := st.PutEmbedding(ctx, h3, model, []float32{0.9, 0.2}); err != nil { // rent/deposit
		t.Fatal(err)
	}

	// Query near the lease axis.
	hits, err := st.SemanticSearch(ctx, []float32{1, 0}, model, SemanticOptions{K: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("hits = %d, want 3", len(hits))
	}
	// The lunch message must rank last.
	if hits[2].Hash != h2 {
		t.Errorf("expected lunch message ranked last, got %q", hits[2].Hash)
	}
	// Scores are descending.
	if !(hits[0].Score >= hits[1].Score && hits[1].Score >= hits[2].Score) {
		t.Errorf("scores not descending: %v %v %v", hits[0].Score, hits[1].Score, hits[2].Score)
	}

	// K limits results.
	top1, err := st.SemanticSearch(ctx, []float32{1, 0}, model, SemanticOptions{K: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(top1) != 1 {
		t.Errorf("K=1 returned %d", len(top1))
	}

	// Sender filter narrows the candidate set (only Harper's messages).
	harperOnly, err := st.SemanticSearch(ctx, []float32{1, 0}, model, SemanticOptions{K: 10, Sender: "Harper"})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range harperOnly {
		if h.Sender != "Harper" {
			t.Errorf("sender filter leaked %q", h.Sender)
		}
	}
	if len(harperOnly) != 2 { // h1, h3
		t.Errorf("harper hits = %d, want 2", len(harperOnly))
	}

	// Dimension mismatch is skipped, not scored.
	mismatched, err := st.SemanticSearch(ctx, []float32{1, 0, 0}, model, SemanticOptions{K: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(mismatched) != 0 {
		t.Errorf("dim-mismatch query returned %d hits, want 0", len(mismatched))
	}
}

func TestPruneOrphanEmbeddings(t *testing.T) {
	st, h1, _, _ := seedEmbeddingCorpus(t)
	ctx := context.Background()
	const model = "test-embed"
	_ = st.PutEmbedding(ctx, h1, model, []float32{1, 0})
	// Orphan embedding for a hash with no message.
	_ = st.PutEmbedding(ctx, "deadbeefhash", model, []float32{0, 1})

	pruned, err := st.PruneOrphanEmbeddings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Errorf("pruned = %d, want 1", pruned)
	}
	if n := scalar(t, st, `SELECT count(*) FROM embeddings`); n != 1 {
		t.Errorf("remaining embeddings = %d, want 1", n)
	}
}

// TestSemanticSearchAttachmentAndLinkFilters pins the two filters the Search
// page's semantic / hybrid modes share with keyword search. Before they existed
// on SemanticOptions a ticked "Has attachment" was silently dropped on the
// vector path — and in hybrid only the keyword half honoured it, so the fused
// list mixed filtered and unfiltered hits. It also pins that a semantic hit
// carries the glyph flags, so the SAME message renders identically whichever
// mode surfaced it.
func TestSemanticSearchAttachmentAndLinkFilters(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	const model = "test-embed"

	conv, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	msgs := []signal.Message{
		msg("Harper", "2022-03-01 09:00:00", "Harper", "the lease agreement scan",
			[]signal.Attachment{{Kind: signal.KindFile, RelPath: "media/lease.pdf", OriginalName: "lease.pdf"}}, nil),
		msg("Harper", "2022-03-01 09:01:00", "Harper", "lease terms explained here",
			nil, []signal.Link{{URL: "https://example.com/lease"}}),
		msg("Harper", "2022-03-01 09:02:00", "Harper", "lease renewal thoughts", nil, nil),
	}
	if _, err := st.ReplaceConversationMessages(ctx, conv, source.Signal, msgs); err != nil {
		t.Fatal(err)
	}
	// All three sit on the same axis, so ONLY the filters can narrow the set.
	for _, m := range msgs {
		if err := st.PutEmbedding(ctx, m.HashWithSource(source.Signal), model, []float32{1, 0}); err != nil {
			t.Fatal(err)
		}
	}

	all, err := st.SemanticSearch(ctx, []float32{1, 0}, model, SemanticOptions{K: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered hits = %d, want 3", len(all))
	}

	withAtt, err := st.SemanticSearch(ctx, []float32{1, 0}, model, SemanticOptions{K: 10, HasAttachment: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(withAtt) != 1 {
		t.Fatalf("HasAttachment hits = %d, want 1 (the filter must narrow the vector scan)", len(withAtt))
	}
	if !withAtt[0].HasAttachment {
		t.Errorf("attachment hit should carry HasAttachment so the card renders the paperclip glyph: %+v", withAtt[0])
	}

	withLink, err := st.SemanticSearch(ctx, []float32{1, 0}, model, SemanticOptions{K: 10, HasLink: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(withLink) != 1 {
		t.Fatalf("HasLink hits = %d, want 1", len(withLink))
	}
	if !withLink[0].HasLink || withLink[0].HasAttachment {
		t.Errorf("link hit flags = link:%v att:%v, want link only", withLink[0].HasLink, withLink[0].HasAttachment)
	}

	// Both filters together: no message carries an attachment AND a link.
	both, err := st.SemanticSearch(ctx, []float32{1, 0}, model, SemanticOptions{K: 10, HasAttachment: true, HasLink: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 0 {
		t.Errorf("attachment+link hits = %d, want 0 (filters must AND)", len(both))
	}
}
