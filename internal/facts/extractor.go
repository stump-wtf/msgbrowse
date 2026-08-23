// The Concrete Web Seam For In-App Fact Extraction
//
// Extractor is the object `msgbrowse serve` and the desktop shell wire behind
// the Settings → Facts tab's Extract / Re-extract controls (#366), so an in-app
// click runs the same pass the `msgbrowse facts` CLI does — over the LIVE store
// and the LIVE LLM client. Before this, extraction had no in-app entry point at
// all: the contact page rendered facts it had no way of ever obtaining.
//
// It holds the process's shared *llm.Holder (not a fixed client) so a job
// started right after a Settings → LLM save picks up the new endpoint and chat
// model: ChatModel and RunFacts both read the holder's CURRENT settings at call
// time. The web layer owns the single-flight guard and the detached goroutine;
// this type just supplies the store + client + config, exactly as
// journal.Builder does for the journal.
//
// Governing: SPEC-0005 (contact-facts) REQ-0005-001 (extraction stays a
// deliberate, opt-in pass that performs the only egress — a click is as
// deliberate as a command, and neither import nor serve ever triggers one),
// REQ-0005-005 (the exclude list is honored by passing config through);
// ADR-0011.
//
// @joestump-agent 08/23/2026 - Added with the in-app extraction controls (#366).
package facts

import (
	"context"
	"log/slog"
	"strings"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/llm"
	"github.com/joestump/msgbrowse/internal/store"
)

// Extractor is the concrete web.FactsExtractor.
type Extractor struct {
	store  *store.Store
	holder *llm.Holder
	cfg    config.JournalConfig
	log    *slog.Logger
}

// NewExtractor builds an Extractor over the shared store and live LLM holder.
// cfg is the JOURNAL config because that is where the conversation denylist
// lives (journal.exclude_conversations), which both LLM-backed passes honor —
// see REQ-0005-005 and ADR-0023.
func NewExtractor(st *store.Store, holder *llm.Holder, cfg config.JournalConfig, log *slog.Logger) *Extractor {
	if log == nil {
		log = slog.Default()
	}
	return &Extractor{store: st, holder: holder, cfg: cfg, log: log}
}

// ChatModel returns the CURRENTLY configured chat model (trimmed), "" when
// unset. Unlike journal.Builder's, this DOES gate the run: see RunFacts.
func (e *Extractor) ChatModel() string { return strings.TrimSpace(e.holder.ChatModel()) }

// RunFacts executes one extraction pass. reset wipes stored facts and cursors
// first; a normal run is incremental off each conversation's fact_state cursor,
// so it re-reads nothing already paid for (REQ-0005-004).
//
// It blocks until the pass finishes or aborts; the web layer calls it in a
// background goroutine, so ctx is a detached (non-request) context that outlives
// the HTTP call. The model is re-read HERE, on that goroutine, rather than
// trusted from the request: a concurrent Settings → LLM save can change it in
// the window between the click and the run, and a reset that deleted every fact
// and only then discovered it had no model would leave the archive emptier than
// it found it.
func (e *Extractor) RunFacts(ctx context.Context, reset bool) error {
	_, err := Run(ctx, e.store, e.holder, Options{
		Model:   strings.TrimSpace(e.holder.ChatModel()),
		Exclude: e.cfg.ExcludeConversations,
		Reset:   reset,
		Logger:  e.log,
	})
	return err
}
