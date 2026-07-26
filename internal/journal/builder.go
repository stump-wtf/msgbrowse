package journal

import (
	"context"
	"log/slog"
	"strings"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/llm"
	"github.com/joestump/msgbrowse/internal/store"
)

// Builder is the concrete web.JournalBuilder (#240): the object `msgbrowse
// serve` and the desktop shell wire behind the Journal page's Build / Rebuild
// controls, so an in-app click runs the same pass the `msgbrowse journal` CLI
// does — over the LIVE store and the LIVE LLM client.
//
// It holds the process's shared *llm.Holder (not a fixed client) so a job
// started right after a Settings → LLM save picks up the new endpoint and chat
// model: ChatModel and RunJournal both read the holder's CURRENT settings at
// call time. The web layer owns the single-flight guard and the detached
// goroutine; this type just supplies the store + client + config.
type Builder struct {
	store  *store.Store
	holder *llm.Holder
	cfg    config.JournalConfig
	log    *slog.Logger
}

// NewBuilder builds a Builder over the shared store and live LLM holder.
func NewBuilder(st *store.Store, holder *llm.Holder, cfg config.JournalConfig, log *slog.Logger) *Builder {
	if log == nil {
		log = slog.Default()
	}
	return &Builder{store: st, holder: holder, cfg: cfg, log: log}
}

// ChatModel returns the CURRENTLY configured chat model (trimmed), "" when
// unset. Unlike the indexer's EmbedModel this does NOT gate the run: the
// mechanical day layer is deterministic, egress-free work (REQ-0016-001) and is
// exactly what an unconfigured user needs built. The web layer reports the
// missing model as an explanatory outcome instead of refusing.
func (b *Builder) ChatModel() string { return strings.TrimSpace(b.holder.ChatModel()) }

// DigestEnabled reports whether the LLM digest pass is configured on.
func (b *Builder) DigestEnabled() bool { return b.cfg.DigestEnabled }

// RunJournal executes one journal pass. day == "" builds the mechanical layer
// plus every missing digest; day != "" rebuilds exactly that day. regenerate
// clears the cached digests in scope first.
//
// It blocks until the pass finishes or aborts; the web layer calls it in a
// background goroutine, so ctx is a detached (non-request) context that outlives
// the HTTP call. The model is re-read HERE, on that goroutine, rather than
// trusted from the request: a concurrent Settings → LLM save can change it in
// the window between the click and the run, and a regenerate that deleted
// digests and only then discovered it had no model would leave the journal
// emptier than it found it.
func (b *Builder) RunJournal(ctx context.Context, day string, regenerate bool) error {
	_, err := Run(ctx, b.store, b.holder, Options{
		Model:         strings.TrimSpace(b.holder.ChatModel()),
		DigestEnabled: b.cfg.DigestEnabled,
		DigestPrompt:  b.cfg.DigestPrompt,
		Exclude:       b.cfg.ExcludeConversations,
		MaxDaysPerRun: b.cfg.MaxDaysPerRun,
		Day:           day,
		Regenerate:    regenerate,
		Logger:        b.log,
	})
	return err
}
