// Package llm is msgbrowse's single, provider-agnostic gateway to a large
// language model. Everything — message embeddings, RAG answer synthesis,
// journal digests, image captioning, and audio transcription — goes through one
// OpenAI-compatible HTTP endpoint (by default a local LiteLLM proxy), so the
// rest of the app never knows or cares which concrete model is behind it.
//
// This package performs the ONLY network egress in msgbrowse. See SECURITY.md:
// pointing BaseURL at a hosted provider is a deliberate, documented choice; the
// default is a local route so message content never leaves the machine.
package llm

import (
	"context"
	"fmt"
)

// Client is the provider-agnostic interface the rest of msgbrowse depends on.
// All methods are safe for concurrent use.
type Client interface {
	// Embed returns one embedding vector per input string, in order. The
	// returned vectors all share the provider's embedding dimensionality.
	Embed(ctx context.Context, inputs []string) ([][]float32, error)

	// Chat returns a single completion for the given messages.
	Chat(ctx context.Context, req ChatRequest) (string, error)

	// Transcribe converts audio bytes (e.g. a voice message) to text. Used by
	// the editorialized journal so audio becomes first-class content. filename
	// hints the audio format to the provider (e.g. "voice.m4a").
	Transcribe(ctx context.Context, audio []byte, filename string) (string, error)

	// Vision returns a short caption/description of an image. Used by the
	// journal's media-first digests. mimeType is the image's content type.
	Vision(ctx context.Context, image []byte, mimeType, prompt string) (string, error)

	// ListModels returns the model ids advertised by the endpoint's
	// /v1/models endpoint. Returns ErrModelsNotSupported when the endpoint
	// returns 404 or a non-JSON body.
	ListModels(ctx context.Context) ([]string, error)
}

// ErrModelsNotSupported is returned by ListModels when the endpoint does
// not serve a /v1/models listing (404 or non-JSON body).
var ErrModelsNotSupported = fmt.Errorf("llm: endpoint does not support model listing")

// Role identifies the author of a chat message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one chat turn.
type Message struct {
	Role    Role
	Content string
}

// ChatRequest parameterizes a single completion. Model is optional; when empty
// the client's configured chat model is used.
type ChatRequest struct {
	Messages    []Message
	Model       string
	Temperature float32
	MaxTokens   int
}
