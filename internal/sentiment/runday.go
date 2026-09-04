package sentiment

// Per-day rescoring (issue #441): redo ONE UTC day's affect scores. The
// incremental pass walks conversations behind hash cursors and cannot express
// "rescore 2026-07-11"; RunDay replaces exactly that day's rows and leaves
// every cursor alone — the day's messages are already behind them, and the
// next incremental run must not re-walk them.
//
// The run is recorded in sentiment_runs with scope day:YYYY-MM-DD so the
// Settings → Sentiment history shows what the refresh did, exactly as whole-
// archive and per-conversation passes do.
//
// Governing: epic #431 / issue #441; UTC bucketing per ADR-0023.
//
// @joestump-agent 09/04/2026 - Added with #441.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/llm"
	"github.com/joestump/msgbrowse/internal/store"
)

// RunDay deletes and re-derives one UTC day's sentiment scores (gen = opts.Model
// × the current lexicon). Conversation-level failures after the LLM client's
// retries are exhausted are recorded in Summary.Errors and skipped, mirroring
// the whole-archive pass; a cancelled context aborts.
func RunDay(ctx context.Context, st *store.Store, client llm.Client, opts Options, day string) (sum Summary, err error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	// Local (not Summary) error list: Stack J branched before #452's
	// Summary.Errors landed on main, and the two must merge without conflict.
	var convErrs []string
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		return Summary{}, fmt.Errorf("sentiment: model not configured (set llm.chat_model)")
	}
	start := time.Now()

	lex, err := BuildLexicon()
	if err != nil {
		return Summary{}, fmt.Errorf("sentiment: %w", err)
	}
	gen := store.SentimentGeneration{Model: model, LexiconVersion: lex.Version}
	scope := fmt.Sprintf("day:%s", day)

	// Durable run record (#367 posture): begin, heartbeat-free (a day pass is
	// short), terminal write deferred through cancellation — bookkeeping must
	// land even when the caller walks away, and a failed INSERT must not abort
	// the pass (runID 0 disables the writes).
	runID, rerr := st.BeginSentimentRun(ctx, model, lex.Version, scope, start)
	if rerr != nil {
		log.Warn("sentiment: could not record day-run start; continuing without a run log", "error", rerr, "day", day)
		runID = 0
	}
	defer func() {
		if runID == 0 {
			return
		}
		finishCtx := context.WithoutCancel(ctx)
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if len(convErrs) > 0 {
			if msg != "" {
				msg += "; "
			}
			msg += fmt.Sprintf("%d conversation(s) failed: %s", len(convErrs), strings.Join(convErrs, "; "))
		}
		if ferr := st.FinishSentimentRun(finishCtx, store.SentimentRun{
			ID: runID, FinishedAt: time.Now(),
			DurationMS: time.Since(start).Milliseconds(),
			Conversations: sum.Conversations, Messages: sum.MessagesScored,
			ScoresWritten: sum.RowsWritten, Batches: sum.Batches, Error: msg,
		}); ferr != nil {
			log.Error("sentiment: could not record day-run completion", "error", ferr)
		}
	}()

	// Replace, don't merge: stale rows for constructs the new pass no longer
	// produces would survive conflict-nothing inserts.
	if n, derr := st.DeleteSentimentForDay(ctx, day, gen); derr != nil {
		return sum, derr
	} else if n > 0 {
		log.Info("sentiment: deleted existing scores for day", "day", day, "rows", n)
	}

	msgs, err := st.MessagesForDay(ctx, day, opts.Exclude)
	if err != nil {
		return sum, err
	}

	// Conversation identity for attribution + the eligibility report.
	convs, err := st.SentimentConversations(ctx, opts.Exclude)
	if err != nil {
		return sum, err
	}
	convByID := make(map[int64]store.SentimentConversation, len(convs))
	for _, c := range convs {
		convByID[c.ID] = c
	}

	batch := opts.BatchSize
	if batch <= 0 || batch > 200 {
		batch = 40
	}
	if opts.Temperature == 0 {
		opts.Temperature = 0.2
	}
	if opts.MaxTokens == 0 {
		opts.MaxTokens = 2048
	}

	// Group the day's messages per conversation, then score in batches with the
	// same score() the incremental pass uses.
	grouped := make(map[int64][]store.MessageView)
	order := []int64{} // deterministic conversation order for the log
	for _, dm := range msgs {
		if _, seen := grouped[dm.ConversationID]; !seen {
			order = append(order, dm.ConversationID)
		}
		grouped[dm.ConversationID] = append(grouped[dm.ConversationID], dm.MessageView)
	}

	for _, convID := range order {
		if ctx.Err() != nil {
			return sum, ctx.Err()
		}
		sc, ok := convByID[convID]
		if !ok {
			// Eligibility shifted (contact unlinked, excluded) between the two
			// reads; skip rather than score an ineligible conversation.
			continue
		}
		contactFor := func(m store.MessageView) int64 {
			if m.IsOwner {
				return store.OwnerContactID
			}
			return sc.ContactID
		}
		views := grouped[convID]
		written, batches, serr := scoreDayBatches(ctx, st, client, lex, gen, opts, sc, views, batch, contactFor)
		if serr != nil {
			if ctx.Err() != nil {
				return sum, ctx.Err()
			}
			convErrs = append(convErrs, fmt.Sprintf("conversation %q (%s): %v", sc.Name, sc.Source, serr))
			log.Warn("sentiment: day re-score failed for one conversation; continuing",
				"day", day, "conversation", sc.Name, "error", serr)
			continue
		}
		sum.Conversations++
		sum.MessagesScored += len(views)
		sum.RowsWritten += written
		sum.Batches += batches
	}

	sum.DurationMS = time.Since(start).Milliseconds()
	log.Info("sentiment day re-score complete", "day", day, "rows_written", sum.RowsWritten,
		"conversations", sum.Conversations, "duration_ms", sum.DurationMS)
	return sum, nil
}

// scoreDayBatches scores one conversation's slice of the day in batches,
// writing each batch without touching cursors.
func scoreDayBatches(ctx context.Context, st *store.Store, client llm.Client, lex *Lexicon, gen store.SentimentGeneration, opts Options, sc store.SentimentConversation, views []store.MessageView, batch int, contactFor func(store.MessageView) int64) (written, batches int, err error) {
	for start := 0; start < len(views); start += batch {
		end := start + batch
		if end > len(views) {
			end = len(views)
		}
		included := realMessages(views[start:end])
		if len(included) == 0 {
			continue
		}
		scores, serr := score(ctx, client, lex, gen.Model, opts, sc.Name, included, contactFor)
		if serr != nil {
			return written, batches, serr
		}
		// Cursor stays untouched (#441): the incremental pass owns it.
		if werr := st.PutSentimentScores(ctx, gen, scores); werr != nil {
			return written, batches, werr
		}
		written += len(scores)
		batches++
	}
	return written, batches, nil
}
