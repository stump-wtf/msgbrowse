// Package facts extracts durable, cited facts about a contact from their chat
// messages using the configured LLM, and stores them for display on the
// conversation view.
//
// It is incremental and idempotent, mirroring the embed package: a per-
// conversation cursor (internal/store fact_state) records the last message fed
// to the extractor, so a re-run after a fresh import only analyzes new messages.
// Facts are deduplicated per contact by normalized text, so reprocessing — or
// extracting from two conversations merged onto one contact — never duplicates a
// fact. Like embed, this is a separate command that performs network egress to
// llm.base_url; a plain import never calls the LLM. Conversations on
// journal.exclude_conversations are never handed to the extractor.
package facts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/joestump/msgbrowse/internal/llm"
	"github.com/joestump/msgbrowse/internal/store"
)

// Categories is the allowlist of fact categories. The extractor is asked to use
// these; anything else is coerced to "other" so the UI can group reliably.
var Categories = []string{
	"personal",
	"work",
	"relationships",
	"preferences",
	"health",
	"location",
	"plans",
	"other",
}

func isKnownCategory(c string) bool {
	for _, k := range Categories {
		if k == c {
			return true
		}
	}
	return false
}

// systemPrompt instructs the model to return strict JSON. It is part of the
// effective prompt version; editing it changes extraction behavior. The category
// list is derived from Categories so the prompt and the validator
// (isKnownCategory) cannot drift.
var systemPrompt = `You extract durable, factual information about ONE person (the contact) from a transcript of their chat messages.

Rules:
- Return ONLY a JSON array, no prose, no markdown fences.
- Each element is an object: {"fact": string, "category": string, "evidence": integer}.
- "fact" is a single, atomic, self-contained statement about the contact, in third person, terse (e.g. "Has a dog named Biscuit", "Works as a nurse in Denver"). Phrase recurring facts consistently so duplicates collapse.
- "category" is one of: ` + strings.Join(Categories, ", ") + `.
- "evidence" is the 1-based number of the single message that best supports the fact.
- Only include facts that are clearly stated or strongly implied by the contact. Do NOT speculate, infer mood, or summarize events.
- Facts must be about the CONTACT, not about "You" (the archive owner).
- If there are no durable facts, return [].`

// errBadResponse marks an LLM response that could not be parsed into facts (as
// opposed to a transport error from the LLM call). A bad response is skipped and
// logged rather than aborting the whole run, so one deterministically-malformed
// batch can't wedge a conversation forever.
var errBadResponse = errors.New("unparseable facts response")

// rawFact is the model's per-fact JSON shape.
type rawFact struct {
	Fact     string `json:"fact"`
	Category string `json:"category"`
	Evidence int    `json:"evidence"`
}

// parsedFact is a validated fact bound to its supporting message.
type parsedFact struct {
	Fact     string
	Category string
	Msg      store.MessageView
}

// buildPrompt renders the numbered transcript the model sees. Only included
// (real) messages appear; their 1-based position is the evidence index the model
// cites. The owner is labeled "You" so the model can tell the contact apart.
func buildPrompt(contact string, included []store.MessageView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Contact: %s\n\nMessages:\n", contact)
	for i, m := range included {
		who := contact
		if m.IsOwner {
			who = "You"
		}
		date := m.TS
		if len(date) >= 10 {
			date = date[:10]
		}
		fmt.Fprintf(&b, "%d. [%s] %s: %s\n", i+1, date, who, strings.TrimSpace(m.Body))
	}
	return b.String()
}

// parseFacts turns a raw model response into validated facts bound to messages.
// It tolerates code fences and surrounding prose — including prose that itself
// contains brackets — via llm.ExtractJSONArray, which returns the first span
// that decodes as a complete JSON array. Unknown categories become "other"; an
// out-of-range evidence index falls back to the last included message so every
// fact keeps provenance.
func parseFacts(raw string, included []store.MessageView) ([]parsedFact, error) {
	if len(included) == 0 {
		return nil, nil
	}
	body := llm.ExtractJSONArray(raw)
	if body == "" {
		return nil, fmt.Errorf("no JSON array in model response")
	}
	var rawFacts []rawFact
	if err := json.Unmarshal([]byte(body), &rawFacts); err != nil {
		return nil, fmt.Errorf("parse facts JSON: %w", err)
	}
	out := make([]parsedFact, 0, len(rawFacts))
	for _, rf := range rawFacts {
		fact := strings.TrimSpace(rf.Fact)
		if fact == "" {
			continue
		}
		cat := strings.ToLower(strings.TrimSpace(rf.Category))
		if !isKnownCategory(cat) {
			cat = "other"
		}
		// Map the 1-based evidence index onto the included slice; clamp out-of-
		// range (or missing, i.e. 0) citations to the last message so provenance
		// is always present rather than dangling.
		idx := rf.Evidence - 1
		if idx < 0 || idx >= len(included) {
			idx = len(included) - 1
		}
		out = append(out, parsedFact{Fact: fact, Category: cat, Msg: included[idx]})
	}
	return out, nil
}

// Options configures a facts run.
type Options struct {
	// Model is the chat model used for extraction; recorded with each fact and in
	// the cursor so a model change re-scans. Required.
	Model string
	// BatchSize is how many messages are sent per extraction call.
	BatchSize int
	// Concurrency is how many conversations are processed in parallel.
	Concurrency int
	// Exclude is the conversation-name denylist (journal.exclude_conversations);
	// matching conversations are never sent to the LLM.
	Exclude []string
	// OnlyConversationID, when > 0, limits the run to a single conversation.
	OnlyConversationID int64
	// Reset wipes all stored facts and cursors before running.
	Reset bool
	// Temperature for the extraction call (low keeps facts deterministic).
	Temperature float32
	// MaxTokens caps the extraction response.
	MaxTokens int
	// Logger receives progress; defaults to slog.Default().
	Logger *slog.Logger
}

// Summary reports what a facts run did.
type Summary struct {
	Conversations  int
	MessagesParsed int
	FactsAdded     int
	Batches        int
	DurationMS     int64
}

// Run extracts facts from every eligible conversation (incrementally, honoring
// the exclude list) using bounded concurrency. A per-batch LLM failure aborts
// the run; because the cursor is persisted after each batch, the next run
// resumes where this one stopped.
func Run(ctx context.Context, st *store.Store, client llm.Client, opts Options) (sum Summary, err error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		return Summary{}, fmt.Errorf("facts: model not configured (set llm.chat_model)")
	}
	batch := opts.BatchSize
	if batch <= 0 || batch > 200 {
		batch = 60
	}
	workers := opts.Concurrency
	if workers <= 0 {
		workers = 4
	}
	// The LLM client omits a zero temperature/max-tokens on the wire (omitempty),
	// which would let the provider apply its own (often high) defaults. Extraction
	// wants low, bounded, near-deterministic output so the text-based dedup holds,
	// so default them here rather than relying on the provider.
	if opts.Temperature == 0 {
		opts.Temperature = 0.2
	}
	if opts.MaxTokens == 0 {
		opts.MaxTokens = 1024
	}
	start := time.Now()

	// --- Run log (#366) ---
	// The durable record the Settings → Facts tab reads: begin, a heartbeat per
	// finished conversation, and a terminal write. It is also the cross-process
	// guard — `msgbrowse facts` and a running `msgbrowse serve` share one SQLite
	// file and nothing else, so this row is how the web layer knows a CLI run is
	// already in flight and refuses to start a second, billable one alongside it.
	//
	// The terminal write is DEFERRED so it lands on every exit path, including a
	// fatal transport error mid-batch and a cancelled context. A run that dies
	// without it leaves the card reading "extracting…" until the heartbeat goes
	// stale — and that stale window is also a window in which no new run may
	// start. It writes through a context detached from cancellation for the same
	// reason: the usual way a run dies is ctx being cancelled, and the record
	// must still land.
	//
	// A failed INSERT must NOT abort the pass: run logging is bookkeeping, and
	// losing it is not a reason to refuse the work the user asked for. runID
	// stays 0, the heartbeat and terminal write are skipped, and extraction
	// proceeds — the posture journal.Run and embed.Run already take.
	runID, rerr := st.BeginFactRun(ctx, model, runScope(opts), start)
	if rerr != nil {
		log.Warn("facts: could not record run start; continuing without a run log", "error", rerr)
		runID = 0
	}
	defer func() {
		if runID == 0 {
			return // no row to finish
		}
		finishCtx := context.WithoutCancel(ctx)
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if ferr := st.FinishFactRun(finishCtx, store.FactRun{
			ID: runID, FinishedAt: time.Now(),
			DurationMS:    time.Since(start).Milliseconds(),
			Conversations: sum.Conversations, Messages: sum.MessagesParsed,
			FactsAdded: sum.FactsAdded, Batches: sum.Batches, Error: msg,
		}); ferr != nil {
			log.Error("facts: could not record run completion", "error", ferr)
		}
	}()

	if opts.Reset {
		if err := st.ResetFacts(ctx); err != nil {
			return Summary{}, err
		}
		log.Info("facts reset: cleared existing facts and cursors")
	}

	convs, err := st.FactConversations(ctx, opts.Exclude)
	if err != nil {
		return Summary{}, err
	}
	if opts.OnlyConversationID > 0 {
		filtered := convs[:0]
		for _, c := range convs {
			if c.ID == opts.OnlyConversationID {
				filtered = append(filtered, c)
			}
		}
		convs = filtered
	}
	if len(convs) == 0 {
		log.Info("facts: no eligible conversations")
		return Summary{DurationMS: time.Since(start).Milliseconds()}, nil
	}
	log.Info("extracting facts", "model", model, "conversations", len(convs), "batch_size", batch, "workers", workers)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu      sync.Mutex
		firstEr error
		once    sync.Once
	)
	fail := func(err error) {
		once.Do(func() { firstEr = err; cancel() })
	}

	jobs := make(chan store.FactConversation)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for fc := range jobs {
				cs, err := processConversation(runCtx, st, client, model, batch, opts, fc, log)
				if err != nil {
					fail(err)
					return
				}
				mu.Lock()
				sum.Conversations++
				sum.MessagesParsed += cs.MessagesParsed
				sum.FactsAdded += cs.FactsAdded
				sum.Batches += cs.Batches
				doneConvs, doneFacts := sum.Conversations, sum.FactsAdded
				mu.Unlock()
				// Heartbeat outside the lock: the counters are already snapshotted,
				// and holding the aggregation mutex across a SQLite write would
				// serialize every worker behind it. A failed heartbeat is logged and
				// ignored — the run is the work, not the bookkeeping.
				if runID != 0 {
					if herr := st.UpdateFactRunProgress(runCtx, runID, doneConvs, doneFacts, time.Now()); herr != nil {
						log.Debug("facts: could not record run progress", "error", herr)
					}
				}
			}
		}()
	}
feed:
	for _, fc := range convs {
		select {
		case <-runCtx.Done():
			break feed
		case jobs <- fc:
		}
	}
	close(jobs)
	wg.Wait()

	if firstEr != nil {
		return sum, firstEr
	}
	sum.DurationMS = time.Since(start).Milliseconds()
	log.Info("facts complete", "facts_added", sum.FactsAdded, "messages_parsed", sum.MessagesParsed,
		"conversations", sum.Conversations, "batches", sum.Batches, "duration_ms", sum.DurationMS)
	return sum, nil
}

// convStats is the per-conversation tally aggregated into the run Summary.
type convStats struct {
	MessagesParsed int
	FactsAdded     int
	Batches        int
}

// processConversation walks one conversation from its stored cursor, extracting
// and storing facts batch by batch and advancing the cursor after each batch.
func processConversation(ctx context.Context, st *store.Store, client llm.Client, model string, batch int, opts Options, fc store.FactConversation, log *slog.Logger) (convStats, error) {
	var stats convStats

	// Resolve the resume point. A different stored model means the contact was
	// last analyzed by another model: re-scan from the start (dedup keeps it
	// safe) so the new model can re-derive everything.
	var cursorTS, cursorID int64
	if lastHash, stModel, ok, err := st.GetFactState(ctx, fc.ID); err != nil {
		return stats, err
	} else if ok && stModel == model {
		if ts, id, found, err := st.ResolveCursor(ctx, fc.ID, lastHash); err != nil {
			return stats, err
		} else if found {
			cursorTS, cursorID = ts, id
		}
	}

	const maxBatches = 100_000 // defensive backstop; the cursor always advances
	for b := 0; b < maxBatches; b++ {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		page, err := st.GetMessages(ctx, fc.ID, cursorTS, cursorID, batch, false)
		if err != nil {
			return stats, err
		}
		if len(page.Messages) == 0 {
			break
		}

		included := realMessages(page.Messages)
		// Anchor the persisted cursor on the last REAL message when there is one:
		// re-ingest tends to reformat volatile system lines (changing their hash),
		// and anchoring on a system line would then fail to resolve and force a
		// full re-scan. The in-memory keyset still advances past the whole batch
		// below, so nothing is reprocessed within this run.
		lastHash := page.Messages[len(page.Messages)-1].Hash
		if len(included) > 0 {
			lastHash = included[len(included)-1].Hash
		}

		added := 0
		if len(included) > 0 {
			parsed, err := extract(ctx, client, model, opts, fc.Name, included)
			switch {
			case err == nil:
				stats.MessagesParsed += len(included)
				for _, pf := range parsed {
					ok, err := st.PutFact(ctx, store.FactInput{
						ContactID:         fc.ContactID,
						Fact:              pf.Fact,
						Category:          pf.Category,
						Source:            fc.Source,
						SourceMessageHash: pf.Msg.Hash,
						SourceTS:          pf.Msg.TS,
						SourceTSUnix:      pf.Msg.TSUnix,
						Model:             model,
					})
					if err != nil {
						return stats, err
					}
					if ok {
						added++
					}
				}
				stats.FactsAdded += added
				stats.Batches++
			case errors.Is(err, errBadResponse):
				// One malformed response must not abort the whole run or wedge
				// this conversation: log it and advance past the batch (a re-run
				// won't help a deterministically-bad batch; --reset can retry).
				log.Warn("skipping batch with unparseable facts response",
					"conversation", fc.Name, "source", fc.Source, "error", err)
			default:
				// Transport/LLM error: abort. The cursor was not advanced for this
				// batch, so the next run resumes here.
				return stats, fmt.Errorf("conversation %q (%s): %w", fc.Name, fc.Source, err)
			}
		}
		// Advance the cursor past this batch (even an all-system or skipped batch)
		// so the next run does not reprocess it.
		if err := st.SetFactState(ctx, fc.ID, lastHash, model, added); err != nil {
			return stats, err
		}
		cursorTS, cursorID = page.NextTSUnix, page.NextID
		if !page.HasMore {
			break
		}
	}
	if stats.FactsAdded > 0 {
		log.Debug("extracted facts", "conversation", fc.Name, "source", fc.Source, "facts_added", stats.FactsAdded)
	}
	return stats, nil
}

// extract calls the LLM for one batch and parses the response into facts.
func extract(ctx context.Context, client llm.Client, model string, opts Options, contact string, included []store.MessageView) ([]parsedFact, error) {
	resp, err := client.Chat(ctx, llm.ChatRequest{
		Model:       model,
		Temperature: opts.Temperature,
		MaxTokens:   opts.MaxTokens,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: systemPrompt},
			{Role: llm.RoleUser, Content: buildPrompt(contact, included)},
		},
	})
	if err != nil {
		return nil, err // transport/LLM error: fatal, resumable
	}
	parsed, perr := parseFacts(resp, included)
	if perr != nil {
		return nil, fmt.Errorf("%w: %v", errBadResponse, perr)
	}
	return parsed, nil
}

// realMessages drops system messages and empty bodies — there is nothing to
// extract from them, and excluding them keeps the evidence indices meaningful.
func realMessages(msgs []store.MessageView) []store.MessageView {
	out := make([]store.MessageView, 0, len(msgs))
	for _, m := range msgs {
		if m.IsSystem || strings.TrimSpace(m.Body) == "" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// runScope maps a run's options onto the FIXED scope token recorded on its
// fact_runs row. It is a token, never prose: the web layer turns it into
// display text, so nothing request- or model-derived can reach the rendered run
// history through this column.
func runScope(opts Options) string {
	switch {
	case opts.Reset:
		return store.FactScopeReset
	case opts.OnlyConversationID > 0:
		return store.FactScopeConversation
	default:
		return store.FactScopeArchive
	}
}
