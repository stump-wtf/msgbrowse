package sentiment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/joestump/msgbrowse/internal/llm"
	"github.com/joestump/msgbrowse/internal/store"
)

// Options configures a scoring run.
type Options struct {
	// Model is the chat model used for scoring. It is stamped on every row and
	// on the cursor, so changing it rescans. Required.
	Model string
	// BatchSize is how many messages are sent per scoring call.
	BatchSize int
	// Concurrency is how many conversations are scored in parallel.
	Concurrency int
	// Exclude is the conversation-name denylist (journal.exclude_conversations).
	// Matching conversations are filtered in the store query, before any of
	// their content is read.
	Exclude []string
	// OnlyConversationID, when > 0, limits the run to a single conversation.
	OnlyConversationID int64
	// Reset wipes stored scores and cursors before running (never opt-outs).
	Reset bool
	// Temperature for the scoring call.
	Temperature float32
	// MaxTokens caps the scoring response.
	MaxTokens int
	// Logger receives progress; defaults to slog.Default().
	Logger *slog.Logger
}

// Summary reports what a scoring run did.
type Summary struct {
	Conversations   int
	MessagesScored  int
	RowsWritten     int
	Batches         int
	SkippedOptedOut int
	DurationMS      int64
	// Errors carries one entry per conversation that failed after the LLM
	// client's retries were exhausted (issue #452). Such a conversation no
	// longer aborts the run — the remaining conversations still get scored —
	// but the failures must not be silent: the entries surface in the run log
	// and the returned Summary.
	Errors []string
}

// Run scores every eligible conversation incrementally, honoring the exclude
// list and per-contact opt-outs, using bounded concurrency.
//
// A transport/LLM failure aborts the current CONVERSATION; after the LLM
// client's retries are exhausted the conversation is recorded in Summary.Errors
// and the run continues with the next one (issue #452) — one dead conversation
// must not take down a pass that has been healthy for hours. Because the cursor
// is persisted after each batch — in the same transaction as that batch's
// scores — the next run resumes the failed conversation exactly where this one
// stopped rather than from the top. A cancelled context still aborts the run.
func Run(ctx context.Context, st *store.Store, client llm.Client, opts Options) (sum Summary, err error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		return Summary{}, fmt.Errorf("sentiment: model not configured (set llm.chat_model)")
	}
	batch := opts.BatchSize
	if batch <= 0 || batch > 200 {
		batch = 40
	}
	workers := opts.Concurrency
	if workers <= 0 {
		workers = 4
	}
	// The LLM client omits zero temperature/max-tokens on the wire, which would
	// let the provider apply its own (often high) defaults. Scoring wants low,
	// bounded, near-deterministic output, so default them here.
	if opts.Temperature == 0 {
		opts.Temperature = 0.2
	}
	if opts.MaxTokens == 0 {
		opts.MaxTokens = 2048
	}
	start := time.Now()

	lex, err := BuildLexicon()
	if err != nil {
		return Summary{}, fmt.Errorf("sentiment: %w", err)
	}
	gen := store.SentimentGeneration{Model: model, LexiconVersion: lex.Version}

	// --- Run log (#367) ---
	// The durable record the Settings → Sentiment tab reads: begin, a heartbeat
	// per finished conversation, and a terminal write. It is also the
	// cross-process guard — `msgbrowse sentiment` and a running `msgbrowse serve`
	// share one SQLite file and nothing else, so this row is how the web layer
	// knows a CLI run is already in flight and refuses to start a second one
	// alongside it. Sentiment is the most expensive pass in the archive
	// (ADR-0028), so a raced double-start costs real money, not just duplicated
	// work.
	//
	// The terminal write is DEFERRED so it lands on every exit path, including a
	// fatal transport error mid-batch and a cancelled context. A run that dies
	// without it leaves the card reading "scoring…" until the heartbeat goes
	// stale — and that stale window is also a window in which no new run may
	// start. It writes through a context detached from cancellation for the same
	// reason: the usual way a run dies is ctx being cancelled, and the record
	// must still land.
	//
	// A failed INSERT must NOT abort the pass: run logging is bookkeeping, and
	// losing it is not a reason to refuse the work the user asked for. runID
	// stays 0, the heartbeat and terminal write are skipped, and scoring
	// proceeds — the posture facts.Run, journal.Run and embed.Run already take.
	runID, rerr := st.BeginSentimentRun(ctx, model, lex.Version, runScope(opts), start)
	if rerr != nil {
		log.Warn("sentiment: could not record run start; continuing without a run log", "error", rerr)
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
		if len(sum.Errors) > 0 {
			if msg != "" {
				msg += "; "
			}
			msg += fmt.Sprintf("%d conversation(s) failed: %s", len(sum.Errors), strings.Join(sum.Errors, "; "))
		}
		if ferr := st.FinishSentimentRun(finishCtx, store.SentimentRun{
			ID: runID, FinishedAt: time.Now(),
			DurationMS:    time.Since(start).Milliseconds(),
			Conversations: sum.Conversations, Messages: sum.MessagesScored,
			ScoresWritten: sum.RowsWritten, Batches: sum.Batches, Error: msg,
		}); ferr != nil {
			log.Error("sentiment: could not record run completion", "error", ferr)
		}
	}()

	if opts.Reset {
		if err := st.ResetSentiment(ctx); err != nil {
			return Summary{}, err
		}
		log.Info("sentiment reset: cleared existing scores and cursors (opt-outs kept)")
	}

	convs, err := st.SentimentConversations(ctx, opts.Exclude)
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
		// A targeted run that matches nothing is a mistake, not a no-op. Reporting
		// "0 scores written" and exiting 0 makes a typo'd id indistinguishable
		// from a conversation that is already fully scored.
		if len(convs) == 0 {
			return Summary{}, fmt.Errorf("sentiment: conversation %d is not eligible for scoring — unknown id, not linked to a contact, holds no real messages, or is on journal.exclude_conversations", opts.OnlyConversationID)
		}
	}

	// Opt-outs are resolved once up front. A conversation whose contact opted
	// out is dropped here, before a single message body is read — the same
	// posture the exclude list gets.
	optedOut, err := st.SentimentOptedOut(ctx)
	if err != nil {
		return Summary{}, err
	}
	var skipped int
	if len(optedOut) > 0 {
		kept := convs[:0]
		for _, c := range convs {
			if _, out := optedOut[c.ContactID]; out {
				skipped++
				continue
			}
			kept = append(kept, c)
		}
		convs = kept
	}

	if len(convs) == 0 {
		if opts.OnlyConversationID > 0 {
			return Summary{SkippedOptedOut: skipped}, fmt.Errorf("sentiment: conversation %d belongs to a contact who opted out of scoring", opts.OnlyConversationID)
		}
		log.Info("sentiment: no eligible conversations", "skipped_opted_out", skipped)
		return Summary{SkippedOptedOut: skipped, DurationMS: time.Since(start).Milliseconds()}, nil
	}
	log.Info("scoring sentiment", "model", model, "lexicon", lex.Version,
		"conversations", len(convs), "batch_size", batch, "workers", workers, "skipped_opted_out", skipped)

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
	// recordFailure captures a conversation-level failure (issue #452): the
	// entry lands in Summary.Errors and the run moves on. Only a cancelled
	// outer context — the user stopping the run — still aborts everything.
	recordFailure := func(sc store.SentimentConversation, err error) {
		mu.Lock()
		sum.Errors = append(sum.Errors, fmt.Sprintf("conversation %q (%s): %v", sc.Name, sc.Source, err))
		mu.Unlock()
		log.Warn("sentiment: conversation failed after retries; continuing with the next",
			"conversation", sc.Name, "source", sc.Source, "error", err)
	}

	jobs := make(chan store.SentimentConversation)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sc := range jobs {
				cs, err := scoreConversation(runCtx, st, client, lex, gen, batch, opts, sc, log)
				if err != nil {
					if ctx.Err() != nil {
						// The run is being shut down (not a conversation
						// failure); abort as before.
						fail(err)
						return
					}
					recordFailure(sc, err)
					continue
				}
				mu.Lock()
				sum.Conversations++
				sum.MessagesScored += cs.MessagesScored
				sum.RowsWritten += cs.RowsWritten
				sum.Batches += cs.Batches
				doneConvs, doneRows := sum.Conversations, sum.RowsWritten
				mu.Unlock()
				// Heartbeat outside the lock: the counters are already
				// snapshotted, and holding the aggregation mutex across a SQLite
				// write would serialize every worker behind it. A failed
				// heartbeat is logged and ignored — the run is the work, not the
				// bookkeeping.
				if runID != 0 {
					if herr := st.UpdateSentimentRunProgress(runCtx, runID, doneConvs, doneRows, time.Now()); herr != nil {
						log.Debug("sentiment: could not record run progress", "error", herr)
					}
				}
			}
		}()
	}
feed:
	for _, sc := range convs {
		select {
		case <-runCtx.Done():
			break feed
		case jobs <- sc:
		}
	}
	close(jobs)
	wg.Wait()

	sum.SkippedOptedOut = skipped
	if firstEr != nil {
		return sum, firstEr
	}
	// Every conversation failed: that is a run failure, not a partial success
	// — surface it so callers (and exit codes) treat it like one.
	if len(sum.Errors) > 0 && sum.Conversations == 0 {
		return sum, fmt.Errorf("sentiment: every conversation failed: %s", sum.Errors[0])
	}
	sum.DurationMS = time.Since(start).Milliseconds()
	log.Info("sentiment complete", "rows_written", sum.RowsWritten, "messages_scored", sum.MessagesScored,
		"conversations", sum.Conversations, "batches", sum.Batches, "duration_ms", sum.DurationMS)
	return sum, nil
}

// convStats is the per-conversation tally aggregated into the run Summary.
type convStats struct {
	MessagesScored int
	RowsWritten    int
	Batches        int
}

// scoreConversation walks one conversation from its stored cursor, scoring and
// persisting batch by batch.
func scoreConversation(ctx context.Context, st *store.Store, client llm.Client, lex *Lexicon, gen store.SentimentGeneration, batch int, opts Options, sc store.SentimentConversation, log *slog.Logger) (convStats, error) {
	var stats convStats

	// Resolve the resume point. A stored generation that differs from the
	// current one means these scores were produced by another model or another
	// lexicon: rescan from the top so the new generation re-derives everything.
	// Writes are idempotent, so a restart is always safe.
	var cursorTS, cursorID int64
	if lastHash, storedGen, ok, err := st.GetSentimentState(ctx, sc.ID); err != nil {
		return stats, err
	} else if ok && storedGen == gen {
		if ts, id, found, err := st.ResolveCursor(ctx, sc.ID, lastHash); err != nil {
			return stats, err
		} else if found {
			cursorTS, cursorID = ts, id
		}
		// found == false means the cursor's message is gone (re-ingest); the
		// zero cursor restarts this conversation from the top.
	}

	// contactFor attributes a score to the message's SENDER, which is what both
	// consumer surfaces aggregate on: the profile shows one contact's expressed
	// affect, the journal aggregates everyone's. The owner has no contacts row,
	// so their messages are attributed to store.OwnerContactID.
	contactFor := func(m store.MessageView) int64 {
		if m.IsOwner {
			return store.OwnerContactID
		}
		return sc.ContactID
	}

	const maxBatches = 100_000 // defensive backstop; the cursor always advances
	for range maxBatches {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		page, err := st.GetMessages(ctx, sc.ID, cursorTS, cursorID, batch, false)
		if err != nil {
			return stats, err
		}
		if len(page.Messages) == 0 {
			break
		}

		included := realMessages(page.Messages)
		// Anchor the persisted cursor on the last REAL message when there is
		// one: re-ingest tends to reformat volatile system lines (changing their
		// hash), and anchoring on one would then fail to resolve and force a
		// full rescan. The in-memory keyset still advances past the whole batch.
		lastHash := page.Messages[len(page.Messages)-1].Hash
		if len(included) > 0 {
			lastHash = included[len(included)-1].Hash
		}

		var scores []store.SentimentScore
		if len(included) > 0 {
			parsed, err := score(ctx, client, lex, gen.Model, opts, sc.Name, included, contactFor)
			switch {
			case err == nil:
				scores = parsed
				stats.MessagesScored += len(included)
				stats.Batches++
			case errors.Is(err, errBadResponse):
				// One malformed response must not abort the run or wedge this
				// conversation: log and advance past the batch. A re-run will
				// not help a deterministically-bad batch; --reset can retry.
				log.Warn("skipping batch with unparseable sentiment response",
					"conversation", sc.Name, "source", sc.Source, "error", err)
			default:
				// Transport/LLM error: abort WITHOUT advancing the cursor, so
				// the next run resumes at this batch rather than skipping it.
				return stats, fmt.Errorf("conversation %q (%s): %w", sc.Name, sc.Source, err)
			}
		}

		// Scores and the cursor advance together, in one transaction.
		if err := st.PutSentimentBatch(ctx, sc.ID, gen, lastHash, scores); err != nil {
			return stats, err
		}
		stats.RowsWritten += len(scores)
		cursorTS, cursorID = page.NextTSUnix, page.NextID
		if !page.HasMore {
			break
		}
	}
	if stats.RowsWritten > 0 {
		log.Debug("scored sentiment", "conversation", sc.Name, "source", sc.Source, "rows", stats.RowsWritten)
	}
	return stats, nil
}

// score calls the LLM for one batch and parses the response.
func score(ctx context.Context, client llm.Client, lex *Lexicon, model string, opts Options, contact string, included []store.MessageView, contactFor func(store.MessageView) int64) ([]store.SentimentScore, error) {
	resp, err := client.Chat(ctx, llm.ChatRequest{
		Model:       model,
		Temperature: opts.Temperature,
		MaxTokens:   opts.MaxTokens,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: systemPrompt(lex)},
			{Role: llm.RoleUser, Content: buildPrompt(contact, included)},
		},
	})
	if err != nil {
		return nil, err // transport/LLM error: fatal, resumable
	}
	parsed, perr := parseScores(resp, included, lex, contactFor)
	if perr != nil {
		return nil, fmt.Errorf("%w: %v", errBadResponse, perr)
	}
	return parsed, nil
}

// realMessages drops system messages and empty bodies — there is nothing to
// score in them, and excluding them keeps the cited indices meaningful.
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

// runScope maps a run's options to the FIXED sentiment_runs scope token the web
// layer renders through a lookup. Reset wins over a single-conversation run:
// a --reset --conversation pass clears everything, which is the property a
// reader of the history needs to know about.
func runScope(opts Options) string {
	switch {
	case opts.Reset:
		return store.SentimentScopeReset
	case opts.OnlyConversationID > 0:
		return store.SentimentScopeConversation
	default:
		return store.SentimentScopeArchive
	}
}
