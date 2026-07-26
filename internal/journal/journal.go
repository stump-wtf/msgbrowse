// Package journal builds the AI-editorialized journal over the message archive
// (ADR-0023). It has two layers:
//
//   - The MECHANICAL layer is deterministic and egress-free: a per-day rollup
//     (counts, per-source counts, top senders) derived purely from local
//     messages and cached in journal_days. It is always rebuilt on a run,
//     regardless of journal.digest_enabled.
//   - The DIGEST layer is optional and is the only network egress: for each day
//     lacking a current digest, one day's transcript is sent to the configured
//     LLM and the prose result cached in journal_digests, versioned by
//     (model, prompt_version) so a model swap or prompt edit invalidates it.
//
// Like internal/facts, digests are persisted per-day immediately, so a run that
// is interrupted (or capped by journal.max_days_per_run) resumes cleanly at the
// next uncached day. Conversations on journal.exclude_conversations are filtered
// before any transcript is assembled, so their content never reaches the LLM.
package journal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/llm"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// defaultDigestTimeout bounds a single day's digest call. It is deliberately
// looser than the facts extractor's per-batch budget: a whole day's
// cross-conversation transcript can be large, so a tight ceiling would truncate
// otherwise-fine days. It is only a ceiling — a fast model returns well under it.
const defaultDigestTimeout = 180 * time.Second

// Options configures a journal run.
type Options struct {
	// Model is the chat model used for digests; recorded with each digest so a
	// model change re-runs the affected days. Required only when DigestEnabled.
	Model string
	// DigestEnabled turns the LLM digest pass on or off. The mechanical layer is
	// built regardless (config.JournalConfig.DigestEnabled).
	DigestEnabled bool
	// DigestPrompt is the system prompt for the digest pass and the source of the
	// prompt_version cache key. Empty is treated as no prompt configured.
	DigestPrompt string
	// Exclude is the conversation-name denylist (journal.exclude_conversations);
	// matching conversations never contribute to a rollup or a transcript.
	Exclude []string
	// MaxDaysPerRun caps how many days a single run digests (0 = unbounded), so a
	// cron catch-up processes a bounded slice and reports the remainder.
	MaxDaysPerRun int
	// Since floors the day range ('YYYY-MM-DD', '' = all history).
	Since string
	// Day restricts the run to EXACTLY one day ('YYYY-MM-DD'); '' runs the whole
	// archive. Set by the web layer's per-day Rebuild control (#240). It is not
	// the same as Since, which is only a lower bound: rebuilding one 2019 day via
	// Since would re-scan every day after it too.
	Day string
	// Backfill digests eligible days OLDEST-first (fill in history) instead of the
	// default newest-first (keep recent days fresh). Only observable when
	// MaxDaysPerRun caps the run.
	Backfill bool
	// Regenerate wipes all cached digests before running so every day re-derives.
	Regenerate bool
	// DryRun makes no LLM calls and no writes: it reports the eligible day count
	// and a rough input-token estimate for the slice the next run would process.
	DryRun bool
	// Temperature and MaxTokens for the digest call; defaulted when zero (the LLM
	// client drops a zero value on the wire, ceding to the provider default).
	Temperature float32
	MaxTokens   int
	// Timeout bounds a single digest call; defaults to defaultDigestTimeout.
	Timeout time.Duration
	// Logger receives progress; defaults to slog.Default().
	Logger *slog.Logger
}

// Summary reports what a journal run did.
type Summary struct {
	Days            int   // mechanical days built/refreshed
	Digested        int   // days sent to the LLM and cached this run
	Cached          int   // eligible-check skips: days already current
	Skipped         int   // days attempted but with an unusable LLM response
	Remaining       int   // eligible days left unprocessed by the MaxDaysPerRun cap
	Eligible        int   // total days needing a digest (dry-run headline)
	EstimatedTokens int   // dry-run char/4 input-token estimate for the next run's slice
	DurationMS      int64 // wall-clock
}

// Run builds the mechanical journal for every day on or after Since, then (when
// DigestEnabled) digests the days lacking a current digest, bounded by
// MaxDaysPerRun. DryRun makes no writes and no LLM calls. A digest transport
// error aborts the run; because each day is persisted as it completes, a re-run
// resumes where this one stopped.
func Run(ctx context.Context, st *store.Store, client llm.Client, opts Options) (sum Summary, err error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if opts.Temperature == 0 {
		opts.Temperature = 0.2 // low, for stable structured JSON (like facts)
	}
	if opts.MaxTokens == 0 {
		opts.MaxTokens = 2048 // a structured object (highlights/media/links) is larger than prose
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultDigestTimeout
	}
	model := strings.TrimSpace(opts.Model)
	start := time.Now()

	// --- Run log (#240) ---
	// The durable record the Journal page reads: begin, a heartbeat per digested
	// day, and a terminal write. Skipped entirely for a dry run, which makes no
	// writes at all.
	//
	// The terminal write is DEFERRED so it happens on every exit path — including
	// the fatal transport error mid-digest and a cancelled context. A run that
	// dies without it leaves the page reading "building…" until the heartbeat
	// goes stale, which is a worse failure than reporting the error. It writes
	// through a context detached from cancellation for the same reason: the usual
	// way a run dies is ctx being cancelled, and the record must still land.
	var runID int64
	if !opts.DryRun {
		var berr error
		// A failed INSERT must NOT abort the pass: run logging is bookkeeping,
		// and losing it is not a reason to refuse the work the user asked for.
		// runID stays 0, the heartbeat and terminal write are skipped, and the
		// build proceeds — the same posture embed.Run takes. (SQLITE_BUSY here is
		// realistic: a concurrent ingest can hold the write lock past the 5s
		// busy_timeout.)
		runID, berr = st.BeginJournalRun(ctx, model, opts.Day, start)
		if berr != nil {
			log.Warn("journal: could not record run start; continuing without a run log", "error", berr)
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
			if ferr := st.FinishJournalRun(finishCtx, store.JournalRun{
				ID: runID, FinishedAt: time.Now(),
				DurationMS: time.Since(start).Milliseconds(),
				Days:       sum.Days, Digested: sum.Digested,
				Cached: sum.Cached, Skipped: sum.Skipped, Error: msg,
			}); ferr != nil {
				log.Error("journal: could not record run completion", "error", ferr)
			}
		}()
	}

	// --- Mechanical layer (always, unless dry-run, which is read-only) ---
	var days []store.JournalDay
	if opts.Day != "" {
		// Single-day scope: bounded on BOTH ends, so a per-day rebuild never
		// re-scans everything newer the way BuildJournalDays(since) would.
		d, ok, derr := st.BuildJournalDay(ctx, opts.Day, opts.Exclude)
		if derr != nil {
			return sum, derr
		}
		if ok {
			days = []store.JournalDay{d}
		}
	} else {
		days, err = st.BuildJournalDays(ctx, opts.Since, opts.Exclude)
		if err != nil {
			return sum, err
		}
	}
	if !opts.DryRun {
		for _, d := range days {
			if err := ctx.Err(); err != nil {
				return sum, err
			}
			if err := st.PutJournalDay(ctx, d); err != nil {
				return sum, err
			}
			sum.Days++
		}
		log.Info("journal: mechanical layer built", "days", sum.Days, "since", orAll(opts.Since))
	} else {
		sum.Days = len(days)
	}

	// --- Digest layer (optional) ---
	if !opts.DigestEnabled {
		sum.DurationMS = time.Since(start).Milliseconds()
		return sum, nil
	}
	if model == "" {
		// digest_enabled but no chat model configured: build the mechanical layer
		// and stop short of egress rather than erroring — the journal still works,
		// digests simply wait until llm.chat_model is set.
		log.Warn("journal: digests skipped (llm.chat_model not configured)")
		sum.DurationMS = time.Since(start).Milliseconds()
		return sum, nil
	}
	if strings.TrimSpace(opts.DigestPrompt) == "" {
		log.Warn("journal: digests skipped (journal.digest_prompt is empty)")
		sum.DurationMS = time.Since(start).Milliseconds()
		return sum, nil
	}

	// Regenerate deliberately DELETES NOTHING.
	//
	// digestCurrent returns false for every day when Regenerate is set, so the
	// whole scope is already eligible, and PutDayDigest upserts — each cached
	// digest is replaced at the moment its replacement succeeds. Clearing first
	// (the old ResetDigests / DeleteDayDigest path) bought nothing and opened a
	// window with real teeth: the endpoint being down is not caught by the
	// model != "" check above, so a "Rebuild all 1,842 digests" click against a
	// dead endpoint deleted all 1,842 rows and then failed on the first call —
	// destroying an archive's worth of billable output with no partial restore,
	// while the banner said it was re-deriving them. The scoped version was
	// worse still: a day whose messages had since been excluded returned
	// ok=false from BuildJournalDay, so the delete landed, nothing rebuilt it,
	// and the run recorded a clean success.
	//
	// The tradeoff is that a digest for a day that no longer qualifies survives
	// a regenerate rather than being swept. That is the right way round: a stale
	// row is recoverable, a deleted one is not.

	pv := promptVersion(opts.DigestPrompt)

	// Determine the eligible days (missing or stale digest). Regenerate makes
	// every day eligible (the cache was cleared for a real run; a dry-run treats
	// it the same to estimate honestly).
	var eligible []store.JournalDay
	for _, d := range days {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		current, err := digestCurrent(ctx, st, d, model, pv, opts.Regenerate)
		if err != nil {
			return sum, err
		}
		if current {
			sum.Cached++
			continue
		}
		eligible = append(eligible, d)
	}
	sum.Eligible = len(eligible)

	// Order + cap the slice this run processes.
	if opts.Backfill {
		reverse(eligible)
	}
	process := eligible
	if opts.MaxDaysPerRun > 0 && len(process) > opts.MaxDaysPerRun {
		process = eligible[:opts.MaxDaysPerRun]
	}
	sum.Remaining = len(eligible) - len(process)

	if opts.DryRun {
		for _, d := range process {
			lines, err := st.DayTranscript(ctx, d.Day, opts.Exclude)
			if err != nil {
				return sum, err
			}
			if len(lines) == 0 {
				continue
			}
			sum.EstimatedTokens += estimateTokens(opts.DigestPrompt, renderDayUser(d.Day, lines))
		}
		sum.DurationMS = time.Since(start).Milliseconds()
		return sum, nil
	}

	log.Info("journal: digesting", "model", model, "eligible", sum.Eligible, "this_run", len(process), "remaining", sum.Remaining)
	// beat refreshes the run's heartbeat and live counters. It is called on
	// EVERY processed day, not only successfully digested ones: a run grinding
	// through days that skip (unparseable response) or have no transcript would
	// otherwise write no updated_at at all, cross journalRunStaleAfter while
	// perfectly alive, and get mislabelled Interrupted — which also re-opens the
	// cross-process guard and lets a second (billable) run start alongside it.
	beat := func() {
		if runID == 0 {
			return
		}
		if herr := st.UpdateJournalRunProgress(ctx, runID, sum.Days, sum.Digested, time.Now()); herr != nil {
			log.Warn("journal: heartbeat failed", "error", herr)
		}
	}
	beat() // the mechanical layer and eligibility scan can themselves be slow
	for _, d := range process {
		beat()
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		lines, err := st.DayTranscript(ctx, d.Day, opts.Exclude)
		if err != nil {
			return sum, err
		}
		if len(lines) == 0 {
			continue // no real content after exclusion; nothing to digest
		}
		raw, derr := digestDay(ctx, client, model, opts, d.Day, lines)
		if derr != nil {
			// Transport/LLM error: fatal, resumable. Days already persisted stay;
			// the next run resumes at this day. The deferred run-finish records
			// this reason, so the page says what went wrong instead of showing a
			// run that never ended.
			return sum, fmt.Errorf("digest %s: %w", d.Day, derr)
		}
		pd, perr := parseDigest(raw)
		if perr != nil {
			// A malformed / empty / truncated JSON response must not wedge the run:
			// skip this day so a re-run retries it (same posture as an empty body).
			log.Warn("journal: unparseable digest response, skipping day", "day", d.Day, "error", perr)
			sum.Skipped++
			continue
		}
		if perr := st.PutDayDigest(ctx, store.JournalDigest{
			Day: d.Day, Model: model, PromptVersion: pv,
			Body:       pd.Summary,   // plain-text summary: fallback + empty-response guard
			Structured: pd.Canonical, // canonical JSON of the editorial digest
			Mood:       pd.Mood,      // denormalized for the calendar tint
			// The day's count AT DIGEST TIME. This is the whole basis of the
			// staleness marker: without it the UI cannot tell a current digest
			// from one written before more messages landed on the day.
			MessageCount: d.MessageCount,
		}); perr != nil {
			return sum, perr
		}
		sum.Digested++
		beat()
	}

	sum.DurationMS = time.Since(start).Milliseconds()
	log.Info("journal: complete", "days", sum.Days, "digested", sum.Digested, "cached", sum.Cached,
		"skipped", sum.Skipped, "remaining", sum.Remaining, "duration_ms", sum.DurationMS)
	return sum, nil
}

// digestCurrent reports whether a day already holds a CURRENT digest: produced by the
// current model and prompt_version. Regenerate forces false (re-derive).
func digestCurrent(ctx context.Context, st *store.Store, d store.JournalDay, model, pv string, regenerate bool) (bool, error) {
	if regenerate {
		return false, nil
	}
	view, ok, err := st.GetJournalDay(ctx, d.Day)
	if err != nil {
		return false, err
	}
	if !ok || view.DigestBody == "" && view.DigestStructured == "" {
		return false, nil
	}
	if view.DigestModel != model {
		return false, nil
	}
	// A digest written before more messages landed on the day describes an
	// incomplete day. The UI flags exactly this as "Out of date", so a plain
	// Build has to pick it up — otherwise the page marks a day stale and the
	// primary control silently skips it, leaving the per-day Rebuild as the only
	// way to fix something the app itself reported as wrong.
	if view.DigestStale() {
		return false, nil
	}
	_, _, storedPV, present, err := st.GetDayDigest(ctx, d.Day)
	if err != nil {
		return false, err
	}
	return present && storedPV == pv, nil
}

// digestDay sends one day's transcript to the LLM under a per-call timeout and
// returns the prose digest.
func digestDay(ctx context.Context, client llm.Client, model string, opts Options, day string, lines []store.DayTranscriptLine) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	return client.Chat(callCtx, llm.ChatRequest{
		Model:       model,
		Temperature: opts.Temperature,
		MaxTokens:   opts.MaxTokens,
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: opts.DigestPrompt},
			{Role: llm.RoleUser, Content: renderDayUser(day, lines)},
		},
	})
}

// renderDayUser builds the user message for a day's digest: a dated header and a
// numbered, thread-labeled transcript. The owner is rendered "You" so the model
// distinguishes the archive owner from other participants.
func renderDayUser(day string, lines []store.DayTranscriptLine) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Date: %s\n\nMessages:\n", day)
	for i, ln := range lines {
		who := ln.Sender
		if ln.IsOwner {
			who = "You"
		}
		hhmm := ln.TS
		if len(hhmm) >= 16 {
			hhmm = ln.TS[11:16] // "HH:MM" out of "YYYY-MM-DD HH:MM:SS"
		}
		fmt.Fprintf(&b, "%d. [%s %s · %s] %s: %s\n",
			i+1, hhmm, source.Label(ln.Source), ln.ConversationName, who, strings.TrimSpace(ln.Body))
		for _, a := range ln.Attachments {
			name := a.OriginalName
			if name == "" {
				name = a.RelPath
			}
			fmt.Fprintf(&b, "   (%s: %s)\n", a.Kind, name)
		}
		for _, l := range ln.Links {
			fmt.Fprintf(&b, "   (link: %s)\n", l.URL)
		}
	}
	return b.String()
}

// Moods is the mood allowlist the digest prompt offers and parseDigest enforces.
// Keep in sync with config.DefaultDigestPrompt's mood list; an unknown value is
// coerced to "neutral", so drift degrades gracefully rather than failing a day.
var Moods = []string{"upbeat", "neutral", "quiet", "tense"}

func isKnownMood(m string) bool {
	for _, k := range Moods {
		if k == m {
			return true
		}
	}
	return false
}

// errBadDigest marks a digest response that carried no usable JSON (as opposed to
// a transport error). Like facts' errBadResponse, the day is skipped-and-logged
// so one deterministically-bad response can't wedge the resumable run.
var errBadDigest = errors.New("unparseable digest response")

// Digest is the validated editorial digest — the canonical shape stored as JSON
// in journal_digests.structured and re-hydrated by the web layer for the day
// card. Every field is model-derived and untrusted (render escaped).
type Digest struct {
	Summary       string      `json:"summary"`
	People        []string    `json:"people"`
	Themes        []string    `json:"themes"`
	Mood          string      `json:"mood"`
	Highlights    []Highlight `json:"highlights"`
	StandoutMedia []string    `json:"standout_media"`
	NotableLinks  []string    `json:"notable_links"`
}

// Highlight is one notable moment: prose plus an optional "HH:MM" time ("" when
// the model gave no valid time).
type Highlight struct {
	Text string `json:"text"`
	Time string `json:"time"`
}

// parsedDigest carries the promoted summary/mood plus the canonical JSON of the
// validated Digest (for journal_digests.structured).
type parsedDigest struct {
	Summary   string
	Mood      string
	Canonical string
}

// extractJSONObject returns the substring from the first '{' to the last '}',
// stripping markdown fences and surrounding prose — the object twin of facts'
// extractJSONArray. Returns "" when no object is present.
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < 0 || end < start {
		return ""
	}
	return s[start : end+1]
}

// parseDigest turns a raw model response into a validated, canonicalized digest.
// It tolerates fences/prose (extractJSONObject) and COERCES every field rather
// than failing on any one (trim + drop empty list items, unknown mood → neutral,
// blank a malformed highlight time) — only a total absence of JSON or an empty
// summary is an error, mirroring facts.parseFacts.
func parseDigest(raw string) (parsedDigest, error) {
	obj := extractJSONObject(raw)
	if obj == "" {
		return parsedDigest{}, errBadDigest
	}
	var rd Digest
	if err := json.Unmarshal([]byte(obj), &rd); err != nil {
		return parsedDigest{}, fmt.Errorf("%w: %v", errBadDigest, err)
	}
	summary := strings.TrimSpace(rd.Summary)
	if summary == "" {
		return parsedDigest{}, errBadDigest
	}
	mood := strings.ToLower(strings.TrimSpace(rd.Mood))
	if !isKnownMood(mood) {
		mood = "neutral"
	}
	clean := Digest{
		Summary:       summary,
		Mood:          mood,
		People:        cleanStrings(rd.People),
		Themes:        cleanStrings(rd.Themes),
		StandoutMedia: cleanStrings(rd.StandoutMedia),
		NotableLinks:  cleanStrings(rd.NotableLinks),
		Highlights:    cleanHighlights(rd.Highlights),
	}
	canonical, err := json.Marshal(clean)
	if err != nil {
		return parsedDigest{}, fmt.Errorf("%w: %v", errBadDigest, err)
	}
	return parsedDigest{Summary: summary, Mood: mood, Canonical: string(canonical)}, nil
}

// cleanStrings trims each item and drops the empties.
func cleanStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cleanHighlights keeps highlights with non-empty text; a malformed time is
// blanked (kept as "") rather than dropping the whole highlight.
func cleanHighlights(in []Highlight) []Highlight {
	out := make([]Highlight, 0, len(in))
	for _, h := range in {
		text := strings.TrimSpace(h.Text)
		if text == "" {
			continue
		}
		out = append(out, Highlight{Text: text, Time: normalizeHHMM(h.Time)})
	}
	return out
}

// normalizeHHMM returns the time as "HH:MM" when it parses, else "".
func normalizeHHMM(s string) string {
	if t, err := time.Parse("15:04", strings.TrimSpace(s)); err == nil {
		return t.Format("15:04")
	}
	return ""
}

// promptVersion is the cache key for a digest prompt: a sha256 of the normalized
// prompt text, the same recipe internal/store/facts.go uses for fact dedup. An
// edit to journal.digest_prompt changes this and invalidates every cached digest.
func promptVersion(prompt string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(prompt))))
	return hex.EncodeToString(sum[:])
}

// estimateTokens is the --dry-run heuristic: roughly 4 characters per token over
// the system + user payload. There is no tokenizer or price table in the
// codebase, so this is deliberately labeled an estimate by callers, not a cost.
func estimateTokens(system, user string) int {
	return (len([]rune(system)) + len([]rune(user))) / 4
}

// reverse flips a day slice in place (newest-first ⇄ oldest-first).
func reverse(days []store.JournalDay) {
	for i, j := 0, len(days)-1; i < j; i, j = i+1, j-1 {
		days[i], days[j] = days[j], days[i]
	}
}

// orAll renders an empty since floor as "all" for logging.
func orAll(since string) string {
	if since == "" {
		return "all"
	}
	return since
}
