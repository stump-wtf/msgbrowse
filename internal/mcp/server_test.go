package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/llm"
	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// embedClient is a fake llm.Client returning a deterministic [1,0] embedding so
// semantic search is testable without a network endpoint.
type embedClient struct{}

func (embedClient) Embed(_ context.Context, in []string) ([][]float32, error) {
	out := make([][]float32, len(in))
	for i := range in {
		out[i] = []float32{1, 0}
	}
	return out, nil
}
func (embedClient) Chat(context.Context, llm.ChatRequest) (string, error)          { return "", nil }
func (embedClient) Transcribe(context.Context, []byte, string) (string, error)     { return "", nil }
func (embedClient) Vision(context.Context, []byte, string, string) (string, error) { return "", nil }
func (embedClient) ListModels(context.Context) ([]string, error)          { return nil, nil }

// connect builds a server over a seeded store and returns a connected MCP
// client session.
func connect(t *testing.T, st *store.Store, withLLM bool) *mcpsdk.ClientSession {
	t.Helper()
	opts := Options{Version: "test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var srv *Server
	if withLLM {
		opts.EmbedModel = "test-embed"
		srv = NewServer(st, embedClient{}, opts)
	} else {
		srv = NewServer(st, nil, opts)
	}

	ctx := context.Background()
	t1, t2 := mcpsdk.NewInMemoryTransports()
	if _, err := srv.srv.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func seedStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "mcp.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	conv, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(ts, sender, body string, att []signal.Attachment, links []signal.Link) signal.Message {
		parsed, _ := time.Parse(signal.TimestampLayout, ts)
		return signal.Message{Conversation: "Harper", Timestamp: parsed, TimestampRaw: ts,
			Sender: sender, Body: body, Attachments: att, Links: links}
	}
	msgs := []signal.Message{
		mk("2022-03-01 09:00:00", "Harper", "the lease agreement is ready", nil, nil),
		mk("2022-03-01 09:01:00", "Me", "great, see https://maps.example.com/x", nil, []signal.Link{{URL: "https://maps.example.com/x"}}),
		mk("2022-03-01 09:02:00", "Harper", "photo attached", []signal.Attachment{{Kind: signal.KindImage, RelPath: "media/p.jpg", OriginalName: "p.jpg"}}, nil),
	}
	if _, err := st.ReplaceConversationMessages(ctx, conv, source.Signal, msgs); err != nil {
		t.Fatal(err)
	}
	// Embed the lease message near [1,0] and the others elsewhere. Key by the
	// source-namespaced storage hash (what the store wrote) so the embeddings
	// join to the message rows.
	_ = st.PutEmbedding(ctx, msgs[0].HashWithSource(source.Signal), "test-embed", []float32{1, 0.1})
	_ = st.PutEmbedding(ctx, msgs[1].HashWithSource(source.Signal), "test-embed", []float32{0.1, 1})
	_ = st.PutEmbedding(ctx, msgs[2].HashWithSource(source.Signal), "test-embed", []float32{0.2, 1})
	return st, msgs[0].HashWithSource(source.Signal)
}

func callTool(t *testing.T, cs *mcpsdk.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("tool %s returned error: %+v", name, res.Content)
	}
	out := map[string]any{}
	if sc, ok := res.StructuredContent.(json.RawMessage); ok {
		if err := json.Unmarshal(sc, &out); err != nil {
			t.Fatalf("decode %s output: %v", name, err)
		}
	} else if res.StructuredContent != nil {
		// Some SDK paths set a decoded map directly.
		b, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func TestListConversationsTool(t *testing.T) {
	st, _ := seedStore(t)
	cs := connect(t, st, false)
	out := callTool(t, cs, "list_conversations", nil)
	convs, _ := out["conversations"].([]any)
	if len(convs) != 1 {
		t.Fatalf("conversations = %d, want 1: %v", len(convs), out)
	}
}

func TestGetConversationTool(t *testing.T) {
	st, _ := seedStore(t)
	cs := connect(t, st, false)
	out := callTool(t, cs, "get_conversation", map[string]any{"name": "Harper"})
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Errorf("messages = %d, want 3", len(msgs))
	}
}

func TestSearchMessagesTool(t *testing.T) {
	st, _ := seedStore(t)
	cs := connect(t, st, true)
	out := callTool(t, cs, "search_messages", map[string]any{"query": "lease"})
	hits, _ := out["hits"].([]any)
	if len(hits) == 0 {
		t.Fatalf("no hits for 'lease'")
	}
	first, _ := hits[0].(map[string]any)
	if first["conversation"] != "Harper" {
		t.Errorf("hit provenance = %v", first)
	}
}

func TestSemanticSearchTool(t *testing.T) {
	st, _ := seedStore(t)
	cs := connect(t, st, true)
	out := callTool(t, cs, "semantic_search", map[string]any{"query": "rental contract", "k": float64(3)})
	hits, _ := out["hits"].([]any)
	if len(hits) == 0 {
		t.Fatalf("no semantic hits")
	}
	// The lease message (embedded near [1,0]) should rank first for query→[1,0].
	first, _ := hits[0].(map[string]any)
	text, _ := first["text"].(string)
	if want := "lease"; !contains(text, want) {
		t.Errorf("top semantic hit = %q, want it to contain %q", text, want)
	}
}

func TestSemanticSearchUnavailableWithoutLLM(t *testing.T) {
	st, _ := seedStore(t)
	cs := connect(t, st, false)
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "semantic_search", Arguments: map[string]any{"query": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("expected semantic_search to error without an embedding model")
	}
}

func TestGetContextTool(t *testing.T) {
	st, _ := seedStore(t)
	cs := connect(t, st, false)
	// Find a message id via get_conversation, then fetch context.
	conv := callTool(t, cs, "get_conversation", map[string]any{"name": "Harper"})
	msgs, _ := conv["messages"].([]any)
	m0, _ := msgs[0].(map[string]any)
	mid := m0["message_id"].(float64)
	out := callTool(t, cs, "get_context", map[string]any{"message_id": mid, "window": float64(1)})
	got, _ := out["messages"].([]any)
	if len(got) == 0 {
		t.Error("get_context returned nothing")
	}
}

func TestListMediaAndLinksTools(t *testing.T) {
	st, _ := seedStore(t)
	cs := connect(t, st, false)

	media := callTool(t, cs, "list_media", map[string]any{"kind": "image"})
	if m, _ := media["media"].([]any); len(m) != 1 {
		t.Errorf("media images = %d, want 1", len(m))
	}
	links := callTool(t, cs, "list_links", map[string]any{"domain": "maps.example.com"})
	if l, _ := links["links"].([]any); len(l) != 1 {
		t.Errorf("links = %d, want 1", len(l))
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
