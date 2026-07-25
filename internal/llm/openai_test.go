package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *OpenAIClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(Options{
		BaseURL:    srv.URL + "/v1",
		APIKey:     "test-key",
		ChatModel:  "test-chat",
		EmbedModel: "test-embed",
		HTTPClient: srv.Client(),
	})
}

func TestEmbed(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header = %q", got)
		}
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "test-embed" {
			t.Errorf("model = %q", req.Model)
		}
		// Return vectors out of order to exercise index-based reordering.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[
			{"index":1,"embedding":[0.1,0.2]},
			{"index":0,"embedding":[0.3,0.4]}
		]}`)
	})

	vecs, err := c.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 {
		t.Fatalf("got %d vectors", len(vecs))
	}
	// index 0 → [0.3,0.4], index 1 → [0.1,0.2]
	if vecs[0][0] != 0.3 || vecs[1][0] != 0.1 {
		t.Errorf("vectors not reordered by index: %v", vecs)
	}
}

func TestEmbedCountMismatch(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[0.1]}]}`)
	})
	if _, err := c.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Error("expected count-mismatch error")
	}
}

func TestChat(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "test-chat" || len(req.Messages) != 2 {
			t.Errorf("unexpected request: %+v", req)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hello there"}}]}`)
	})
	got, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{
		{Role: RoleSystem, Content: "sys"}, {Role: RoleUser, Content: "hi"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello there" {
		t.Errorf("chat = %q", got)
	}
}

func TestTranscribe(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data") {
			t.Errorf("content-type = %q", ct)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		if r.FormValue("model") != "whisper-1" {
			t.Errorf("model field = %q", r.FormValue("model"))
		}
		_, _ = io.WriteString(w, `{"text":"transcribed words"}`)
	})
	got, err := c.Transcribe(context.Background(), []byte("fakeaudio"), "voice.m4a")
	if err != nil {
		t.Fatal(err)
	}
	if got != "transcribed words" {
		t.Errorf("transcribe = %q", got)
	}
}

func TestVision(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "image_url") || !strings.Contains(string(body), "data:image/png;base64,") {
			t.Errorf("vision payload missing image_url/data URL: %s", body)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"a cat"}}]}`)
	})
	got, err := c.Vision(context.Background(), []byte("PNGDATA"), "image/png", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "a cat" {
		t.Errorf("vision = %q", got)
	}
}

func TestListModels(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Method != "GET" {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[
			{"id":"nomic-embed-text"},
			{"id":"llama3"},
			{"id":"llama3:70b"}
		]}`)
	})
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("got %d models", len(models))
	}
	if models[0] != "nomic-embed-text" || models[1] != "llama3" || models[2] != "llama3:70b" {
		t.Errorf("models = %v", models)
	}
}

func TestListModels404(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not found"}`)
	})
	_, err := c.ListModels(context.Background())
	if err != ErrModelsNotSupported {
		t.Errorf("expected ErrModelsNotSupported, got %v", err)
	}
}

func TestListModelsNonJSON(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `not json`)
	})
	_, err := c.ListModels(context.Background())
	if err != ErrModelsNotSupported {
		t.Errorf("expected ErrModelsNotSupported, got %v", err)
	}
}

func TestErrorStatus(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	})
	_, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Errorf("expected 429 error, got %v", err)
	}
}
