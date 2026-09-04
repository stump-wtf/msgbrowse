package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// base64Std standard-base64-encodes b (for data: URLs).
func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// OpenAIClient is an OpenAI-compatible implementation of Client. It targets any
// endpoint that speaks the OpenAI REST shapes (/v1/embeddings, /v1/chat/
// completions, /v1/audio/transcriptions) — OpenAI itself, a LiteLLM proxy,
// Ollama, vLLM, etc.
type OpenAIClient struct {
	baseURL    string
	apiKey     string
	chatModel  string
	embedModel string
	httpClient *http.Client
	retry      RetryConfig
	log        *slog.Logger
}

// Options configures an OpenAIClient.
type Options struct {
	BaseURL    string // e.g. http://127.0.0.1:4000/v1 (no trailing slash required)
	APIKey     string
	ChatModel  string
	EmbedModel string
	Timeout    time.Duration
	HTTPClient *http.Client // optional; for tests
	// Retry bounds transient-failure retries (issue #452); nil applies
	// DefaultRetry. Set Attempts=1 to disable retrying.
	Retry *RetryConfig
	// Logger receives one info line per retry; defaults to slog.Default().
	Logger *slog.Logger
}

// New constructs an OpenAIClient. BaseURL and the model names are required for
// the corresponding operations; an empty APIKey is allowed (some local
// proxies/Ollama accept any or no key).
func New(opts Options) *OpenAIClient {
	hc := opts.HTTPClient
	if hc == nil {
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		hc = &http.Client{Timeout: timeout}
	}
	retryCfg := DefaultRetry()
	if opts.Retry != nil {
		retryCfg = *opts.Retry
	}
	retryCfg = retryCfg.withDefaults()
	lg := opts.Logger
	if lg == nil {
		lg = slog.Default()
	}
	return &OpenAIClient{
		baseURL:    strings.TrimRight(opts.BaseURL, "/"),
		apiKey:     opts.APIKey,
		chatModel:  opts.ChatModel,
		embedModel: opts.EmbedModel,
		httpClient: hc,
		retry:      retryCfg,
		log:        lg,
	}
}

// --- Embeddings ---

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

// Embed implements Client.
func (c *OpenAIClient) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if c.embedModel == "" {
		return nil, fmt.Errorf("llm: embed model not configured")
	}
	var resp embedResponse
	if err := c.postJSON(ctx, "/embeddings", embedRequest{Model: c.embedModel, Input: inputs}, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) != len(inputs) {
		return nil, fmt.Errorf("llm: embeddings count mismatch: got %d, want %d", len(resp.Data), len(inputs))
	}
	// Reorder defensively by the provider-reported index.
	out := make([][]float32, len(inputs))
	for _, d := range resp.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("llm: embedding index %d out of range", d.Index)
		}
		out[d.Index] = d.Embedding
	}
	for i, v := range out {
		if len(v) == 0 {
			return nil, fmt.Errorf("llm: missing embedding for input %d", i)
		}
	}
	return out, nil
}

// --- Model listing ---

type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// ListModels implements Client.
func (c *OpenAIClient) ListModels(ctx context.Context) ([]string, error) {
	respBody, err := c.doWithRetry(ctx, "/models", func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
		if err != nil {
			return nil, err
		}
		c.setAuth(req)
		return req, nil
	})
	if err != nil {
		// A 404 means the endpoint doesn't support model listing.
		var se *statusError
		if errors.As(err, &se) && se.code == http.StatusNotFound {
			return nil, ErrModelsNotSupported
		}
		return nil, err
	}
	var resp modelsResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, ErrModelsNotSupported
	}
	out := make([]string, 0, len(resp.Data))
	for _, m := range resp.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

// --- Chat ---

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float32       `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// Chat implements Client.
func (c *OpenAIClient) Chat(ctx context.Context, req ChatRequest) (string, error) {
	model := req.Model
	if model == "" {
		model = c.chatModel
	}
	if model == "" {
		return "", fmt.Errorf("llm: chat model not configured")
	}
	body := chatRequest{Model: model, Temperature: req.Temperature, MaxTokens: req.MaxTokens}
	for _, m := range req.Messages {
		body.Messages = append(body.Messages, chatMessage{Role: string(m.Role), Content: m.Content})
	}
	var resp chatResponse
	if err := c.postJSON(ctx, "/chat/completions", body, &resp); err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("llm: chat returned no choices")
	}
	return resp.Choices[0].Message.Content, nil
}

// --- Transcription ---

// Transcribe implements Client using the OpenAI /audio/transcriptions endpoint
// (multipart form). The model name "whisper-1" is the OpenAI default; a LiteLLM
// proxy maps it to whatever local/hosted ASR is configured.
func (c *OpenAIClient) Transcribe(ctx context.Context, audio []byte, filename string) (string, error) {
	if filename == "" {
		filename = "audio.m4a"
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(audio); err != nil {
		return "", err
	}
	if err := mw.WriteField("model", "whisper-1"); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/audio/transcriptions", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	c.setAuth(req)

	respBody, err := c.do(req)
	if err != nil {
		return "", err
	}
	var resp struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", fmt.Errorf("llm: decode transcription: %w", err)
	}
	return resp.Text, nil
}

// --- Vision ---

// Vision implements Client using a chat completion with an image_url content
// part (OpenAI vision shape; data: URL so no external fetch). mimeType is the
// image's content type (e.g. "image/jpeg").
func (c *OpenAIClient) Vision(ctx context.Context, image []byte, mimeType, prompt string) (string, error) {
	if c.chatModel == "" {
		return "", fmt.Errorf("llm: chat model not configured")
	}
	if prompt == "" {
		prompt = "Briefly describe this image in one sentence."
	}
	dataURL := "data:" + mimeType + ";base64," + base64Std(image)
	// Vision uses the array-content message shape, which differs from the plain
	// chat shape, so it is built inline here.
	payload := map[string]any{
		"model": c.chatModel,
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": prompt},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
				},
			},
		},
	}
	var resp chatResponse
	if err := c.postJSON(ctx, "/chat/completions", payload, &resp); err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("llm: vision returned no choices")
	}
	return resp.Choices[0].Message.Content, nil
}

// --- HTTP plumbing ---

// postJSON marshals body to JSON, POSTs it to baseURL+path with transient
// failure retry, and decodes the response into out.
func (c *OpenAIClient) postJSON(ctx context.Context, path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	respBody, err := c.doWithRetry(ctx, path, func() (*http.Request, error) {
		// A fresh request per attempt: the body reader is consumed by the
		// first send.
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		c.setAuth(req)
		return req, nil
	})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("llm: decode response from %s: %w", path, err)
	}
	return nil
}

// doWithRetry executes buildReq→do up to the configured attempt count,
// retrying only retryable failures (429/502/503/504 and client timeouts) with
// exponential backoff + jitter, logging each retry at info. Non-retryable
// errors return immediately; a cancelled context never schedules another
// attempt.
//
// @joestump-agent 09/04/2026 - Added with #452: transient-failure retry so
// one bad gateway response no longer aborts a multi-hour pass.
func (c *OpenAIClient) doWithRetry(ctx context.Context, path string, buildReq func() (*http.Request, error)) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < c.retry.Attempts; attempt++ {
		if attempt > 0 {
			d := c.retry.delay(attempt - 1)
			c.log.Info("retrying LLM request", "path", path, "attempt", attempt+1, "of", c.retry.Attempts, "delay", d.String(), "error", lastErr)
			timer := time.NewTimer(d)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		req, err := buildReq()
		if err != nil {
			return nil, err
		}
		body, err := c.do(req)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retryable(err) || req.Context().Err() != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *OpenAIClient) setAuth(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
}

// statusError is a non-2xx response from the endpoint. Error() keeps the exact
// "llm: /path returned CODE: BODY" format — web.SummarizeEmbedError parses it —
// while the typed form lets classifyError read the real status code instead of
// substring-matching digits that also appear in ports, paths, and body text.
type statusError struct {
	path string
	code int
	body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("llm: %s returned %d: %s", e.path, e.code, e.body)
}

// do executes the request and returns the body, mapping non-2xx to a
// *statusError that includes a (truncated) response body for diagnosis.
func (c *OpenAIClient) do(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: request to %s: %w", req.URL.Path, err)
	}
	defer resp.Body.Close()
	// Cap the response body to bound memory against a misbehaving endpoint. The
	// largest legitimate response is an embeddings batch: max batch (512) ×
	// large dims (e.g. 3072) as JSON ≈ ~14 MiB, so 64 MiB leaves ample headroom
	// while still bounding pathological responses.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(body)
		if len(snippet) > 500 {
			snippet = snippet[:500]
		}
		return nil, &statusError{path: req.URL.Path, code: resp.StatusCode, body: snippet}
	}
	return body, nil
}
