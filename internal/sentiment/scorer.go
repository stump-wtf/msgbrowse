// The Concrete Web Seam For In-App Sentiment Scoring
//
// Scorer is the object `msgbrowse serve` and the desktop shell wire behind the
// Settings → Sentiment tab's Score / Rescore controls (#367), so an in-app click
// runs the same pass the `msgbrowse sentiment` CLI does — over the LIVE store
// and the LIVE LLM client.
//
// Before this, scoring had no in-app entry point at all, and the consequence was
// visible on a real archive: message_sentiment and sentiment_state were both
// empty across 2,438 conversations, and nothing in internal/web referenced
// either table. The engine, the 300-item IPIP lexicon, the schema, ADR-0028 and
// SPEC-0027 all existed; the pipeline had simply never been reachable.
//
// It holds the process's shared *llm.Holder (not a fixed client) so a job
// started right after a Settings → LLM save picks up the new endpoint and chat
// model: ChatModel and RunSentiment both read the holder's CURRENT settings at
// call time. The web layer owns the single-flight guard and the detached
// goroutine; this type just supplies the store + client + config, exactly as
// journal.Builder and facts.Extractor do for their pipelines.
//
// Governing: ADR-0028 (IPIP-anchored scoring is ONE llm.Chat egress per batch to
// llm.base_url — a click adds no new outbound path, it just makes the existing
// one reachable), SPEC-0027 (opt-outs and the exclude list are honored inside
// Run, before any message body is read).
//
// @joestump-agent 08/23/2026 - Added with the in-app scoring controls (#367).
package sentiment

import (
	"context"
	"log/slog"
	"strings"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/llm"
	"github.com/joestump/msgbrowse/internal/store"
)

// Scorer is the concrete web.SentimentScorer.
type Scorer struct {
	store  *store.Store
	holder *llm.Holder
	cfg    config.JournalConfig
	log    *slog.Logger
}

// NewScorer builds a Scorer over the shared store and live LLM holder. cfg is
// the JOURNAL config because that is where the conversation denylist lives
// (journal.exclude_conversations), which every LLM-backed pass honors — see
// ADR-0023 and SPEC-0027.
func NewScorer(st *store.Store, holder *llm.Holder, cfg config.JournalConfig, log *slog.Logger) *Scorer {
	if log == nil {
		log = slog.Default()
	}
	return &Scorer{store: st, holder: holder, cfg: cfg, log: log}
}

// ChatModel returns the CURRENTLY configured chat model (trimmed), "" when
// unset. It gates the run: Run itself refuses without one, and the web layer
// refuses before starting anything at all.
func (s *Scorer) ChatModel() string { return strings.TrimSpace(s.holder.ChatModel()) }

// LexiconVersion returns the curation this build scores under. Paired with
// ChatModel it is the generation every consumer surface filters on, so the web
// layer reads it here rather than hard-coding the constant in three places.
func (s *Scorer) LexiconVersion() string { return LexiconVersion }

// RunSentiment executes one scoring pass. reset wipes stored scores and cursors
// first (never opt-outs); a normal run is incremental off each conversation's
// sentiment_state cursor, so it re-reads — and re-pays for — nothing already
// scored under the current generation.
//
// It blocks until the pass finishes or aborts; the web layer calls it in a
// background goroutine, so ctx is a detached (non-request) context that outlives
// the HTTP call. The model is re-read HERE, on that goroutine, rather than
// trusted from the request: a concurrent Settings → LLM save can change it in
// the window between the click and the run, and a reset that deleted every score
// and only then discovered it had no model would leave the archive emptier than
// it found it.
func (s *Scorer) RunSentiment(ctx context.Context, reset bool) error {
	_, err := Run(ctx, s.store, s.holder, Options{
		Model:   strings.TrimSpace(s.holder.ChatModel()),
		Exclude: s.cfg.ExcludeConversations,
		Reset:   reset,
		Logger:  s.log,
	})
	return err
}
