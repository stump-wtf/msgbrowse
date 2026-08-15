//go:build sentimentlive

// Package sentiment's live smoke test.
//
// This file is behind the `sentimentlive` build tag and is therefore NOT built
// by `go test ./...`, `make test`, `make check`, or CI. It exists because the
// faked-client tests can prove the engine handles a response correctly but say
// nothing about whether a real model, given this prompt and these anchors,
// returns anything usable. That question is worth answering deliberately, by a
// human, against their own endpoint — not on every commit.
//
// Run it against the configured llm.base_url:
//
//	go test -tags sentimentlive ./internal/sentiment/ -run TestLive -v
//
// It reads the same config the CLI does, scores a handful of fixture messages
// written to express obvious things, and prints what came back. It asserts only
// that the response is parseable and lands in range: asserting which constructs
// a given model picks would be pinning the model's judgment, which is exactly
// the thing that legitimately varies.
package sentiment

import (
	"context"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/llm"
	"github.com/joestump/msgbrowse/internal/store"
)

// liveFixtures are written to express something obvious, so a model that scores
// them all as empty is a signal the prompt is not landing.
var liveFixtures = []store.MessageView{
	{Hash: "live1", TSUnix: 1_700_000_000, TS: "2023-11-14 22:13:20", Body: "I got the job!! I have been grinning all afternoon, I can't believe it actually happened"},
	{Hash: "live2", TSUnix: 1_700_000_060, TS: "2023-11-14 22:14:20", Body: "honestly I have been dreading this all week and I can't stop turning it over at 3am"},
	{Hash: "live3", TSUnix: 1_700_000_120, TS: "2023-11-14 22:15:20", Body: "ok see you at 6", IsOwner: true},
	{Hash: "live4", TSUnix: 1_700_000_180, TS: "2023-11-14 22:16:20", Body: "that is the third time they have done this and I am completely done being polite about it"},
}

func TestLiveScoringAgainstConfiguredModel(t *testing.T) {
	v, err := config.Load("")
	if err != nil {
		t.Skipf("no usable config (%v); this test needs a configured llm.base_url", err)
	}
	cfg, err := config.Unmarshal(v)
	if err != nil {
		t.Skipf("config did not unmarshal (%v)", err)
	}
	if cfg.LLM.ChatModel == "" {
		t.Skip("llm.chat_model is not configured")
	}

	lex, err := BuildLexicon()
	if err != nil {
		t.Fatalf("building lexicon: %v", err)
	}
	client := llm.New(llm.Options{
		BaseURL:   cfg.LLM.BaseURL,
		APIKey:    cfg.LLM.APIKey,
		ChatModel: cfg.LLM.ChatModel,
		Timeout:   cfg.LLM.Timeout,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	contactFor := func(store.MessageView) int64 { return 1 }
	scores, err := score(ctx, client, lex, cfg.LLM.ChatModel, Options{Temperature: 0.2, MaxTokens: 2048},
		"Harper", liveFixtures, contactFor)
	if err != nil {
		t.Fatalf("live scoring call failed: %v", err)
	}

	t.Logf("model %q returned %d score rows for %d fixture messages", cfg.LLM.ChatModel, len(scores), len(liveFixtures))
	byMsg := map[string][]store.SentimentScore{}
	for _, s := range scores {
		byMsg[s.MessageHash] = append(byMsg[s.MessageHash], s)
	}
	for _, m := range liveFixtures {
		t.Logf("  %s: %q", m.Hash, m.Body)
		for _, s := range byMsg[m.Hash] {
			t.Logf("      %-26s %+.2f", s.Construct, s.Score)
		}
		if len(byMsg[m.Hash]) == 0 {
			t.Logf("      (nothing salient)")
		}
	}

	// Structural assertions only — the model's judgment is what varies, and
	// pinning it here would make this test a flake generator.
	known := map[string]bool{}
	for _, c := range lex.Constructs {
		known[c.Name] = true
	}
	for _, s := range scores {
		if !known[s.Construct] {
			t.Errorf("model returned construct %q, which is not in the lexicon (the parser should have dropped it)", s.Construct)
		}
		if s.Score < -1 || s.Score > 1 {
			t.Errorf("score %v for %q is out of range", s.Score, s.Construct)
		}
		if abs(s.Score) < salienceFloor {
			t.Errorf("score %v for %q is below the salience floor and should not have been kept", s.Score, s.Construct)
		}
	}
	if len(scores) == 0 {
		t.Error("the model scored nothing across four deliberately expressive messages — the prompt is probably not landing with this model")
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
