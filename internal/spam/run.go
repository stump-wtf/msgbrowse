package spam

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/contacts"
	"github.com/joestump/msgbrowse/internal/store"
)

// Options configures one scan.
type Options struct {
	// Rules is the classification policy. Required.
	Rules *Rules
	// AddressBook decides who counts as a stranger. Nil means
	// contacts.Unavailable, which puts the scan in degraded mode (see Summary).
	AddressBook contacts.Resolver
	// OnlyConversationID limits the scan to one conversation.
	OnlyConversationID int64
	// Reset clears derived findings, cursors, and detected opt-outs first.
	// Human judgments and manually filed events survive it.
	Reset bool
	// BatchSize is how many messages are read and written per transaction.
	BatchSize int
	// Now supplies the current time; defaults to time.Now.
	Now func() time.Time
	// Logger receives progress; defaults to slog.Default().
	Logger *slog.Logger
}

// Governing: ADR-0029 (unsolicited-contact evidence)
// Implements: SPEC-0028 REQ-0028-013 "Scan-environment provenance"
//
// ProviderNamer is an optional interface a contacts.Resolver may implement to
// identify itself in a scan-environment stamp. A resolver that does not is
// recorded as "unknown" rather than guessed at.
type ProviderNamer interface {
	ProviderName() string
}

// scanEnv is the value stamped onto every row a scan writes
// (SPEC-0028 REQ-0028-013): the address-book resolver's identity and the
// availability that decided the stranger predicate, joined as
// "provider/availability" — e.g. "macoscontacts/available", "none/absent".
//
// Both halves are needed and neither implies the other. Availability alone
// cannot distinguish "no provider on this platform" from "a provider exists but
// the TCC grant is missing", which are different things to tell a reader.
// Provider alone cannot say whether it actually worked. The degraded flag the
// requirement also asks for is not stored separately because it is fully
// determined by the availability half (degraded <=> it is not "available",
// which is the branch below); storing it too would be denormalized and could
// disagree with itself.
func scanEnv(book contacts.Resolver, availability contacts.Availability) string {
	provider := "unknown"
	if n, ok := book.(ProviderNamer); ok {
		provider = n.ProviderName()
	}
	return provider + "/" + availability.String()
}

// Summary is what one scan did.
type Summary struct {
	RulesetVersion   string
	Conversations    int
	MessagesScanned  int
	Findings         int
	Candidates       int
	OptOutsDetected  int
	Senders          int
	SkippedInContact int
	SkippedAllowlist int
	SkippedOwner     int
	SkippedNotShaped int
	// AddressBook is "available", "needs-permission", or "absent".
	AddressBook string
	// ScanEnv is the stamp written onto every row this scan produced:
	// "provider/availability" (SPEC-0028 REQ-0028-013).
	ScanEnv string
	// Degraded is true when the address book could not be read and the scan
	// fell back to the identifier-shape heuristic. A degraded run is still
	// useful, but it cannot tell a stranger from a friend whose thread is named
	// by a bare number, so the caller MUST surface this.
	Degraded   bool
	DurationMS int64
}

// Run scans the archive for unsolicited contact and writes the evidence layer.
//
// It performs NO network I/O. Classification is local, deterministic, and
// regex-based, so this command is safe to run on an archive you would not send
// to an LLM (ADR-0029 §3).
//
// The scan is incremental in the same way fact extraction and sentiment scoring
// are: a per-conversation cursor anchored on a message content hash. Changing
// any rule changes the ruleset version, which rescans every conversation —
// findings from two rule generations are not comparable and must never share a
// dossier.
func Run(ctx context.Context, st *store.Store, opts Options) (sum Summary, retErr error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	if opts.Rules == nil {
		return Summary{}, fmt.Errorf("spam: no ruleset configured")
	}
	batch := opts.BatchSize
	if batch <= 0 || batch > 500 {
		batch = 200
	}
	book := opts.AddressBook
	if book == nil {
		book = contacts.Unavailable{}
	}
	start := now()
	version := opts.Rules.Version()
	sum = Summary{RulesetVersion: version}

	if opts.Reset {
		if err := st.ResetSpam(ctx); err != nil {
			return sum, err
		}
		log.Info("spam reset: cleared findings, cursors, and detected opt-outs (manual records kept)")
	}

	// Resolve the address book ONCE, in bulk. Asking it per identifier would be
	// N round trips into Contacts.framework for no extra accuracy.
	availability := book.Availability(ctx)
	sum.AddressBook = availability.String()
	sum.ScanEnv = scanEnv(book, availability)

	// REQ-0028-014: the run log starts once the environment is known, so even
	// a first-batch abort records what ran under which conditions. Everything
	// after this point — the reset, the address-book read, every conversation
	// — is stampable on the row when the run terminates, including on abort;
	// a scan that died halfway must be legible afterwards, not
	// indistinguishable from one never started.
	runID, runErr := st.BeginSpamRun(ctx, store.SpamRun{
		RulesetVersion: version,
		ScanEnv:        sum.ScanEnv,
		AddressBook:    sum.AddressBook,
	}, start)
	if runErr != nil {
		return sum, runErr
	}
	// Terminal write on EVERY exit path — happy or aborted (see above).
	defer func() { finishSpamRun(ctx, st, runID, start, now(), sum, retErr) }()

	known := map[string]struct{}{}
	if availability == contacts.Available {
		people, err := book.People(ctx)
		if err != nil {
			return sum, fmt.Errorf("spam: read address book: %w", err)
		}
		for _, p := range people {
			for _, id := range p.Identifiers {
				if k := MatchKey(id.Value); k != "" {
					known[k] = struct{}{}
				}
			}
		}
		log.Info("spam: address book read", "people", len(people), "identifiers", len(known))
	} else {
		// Degraded mode. spam-catcher's rule here was "treat everyone as a
		// stranger and warn", which is right when the tool only ever stores
		// non-contacts. msgbrowse already holds the whole archive, so that
		// rule would enroll every friend and relative as a spam sender on the
		// default (address-book-free) build. Instead the scan narrows to
		// threads whose name is a bare phone number or email — the shape an
		// unknown number takes in an export, because the exporter names a
		// known thread after the person — and says loudly that it did.
		sum.Degraded = true
		log.Warn("spam: no readable address book — scanning only phone/email-shaped threads; senders you know may be missing and known threads named by a bare number may be misfiled",
			"availability", sum.AddressBook)
	}

	convs, err := st.SpamConversations(ctx, opts.Rules.Exclude)
	if err != nil {
		return sum, err
	}
	if opts.OnlyConversationID > 0 {
		filtered := convs[:0]
		for _, c := range convs {
			if c.ID == opts.OnlyConversationID {
				filtered = append(filtered, c)
			}
		}
		convs = filtered
		// A targeted run that matches nothing is a typo, not a clean result.
		if len(convs) == 0 {
			return sum, fmt.Errorf("spam: conversation %d is not eligible — unknown id, a group thread, holds no real messages, or is on the exclude list", opts.OnlyConversationID)
		}
	}

	for _, c := range convs {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		identifier, ok := senderIdentifier(c.Name)
		if !ok {
			continue
		}
		switch {
		case opts.Rules.IsMine(identifier):
			sum.SkippedOwner++
			continue
		case opts.Rules.IsAllowlisted(identifier):
			sum.SkippedAllowlist++
			continue
		}
		if availability == contacts.Available {
			if _, inBook := known[MatchKey(identifier)]; inBook {
				sum.SkippedInContact++
				continue
			}
		} else if !shapedLikeAHandle(c.Name) {
			sum.SkippedNotShaped++
			continue
		}

		stats, err := scanConversation(ctx, st, opts.Rules, version, sum.ScanEnv, batch, c, identifier)
		if err != nil {
			return sum, err
		}
		sum.Conversations++
		sum.Senders++
		sum.MessagesScanned += stats.scanned
		sum.Findings += stats.findings
		sum.Candidates += stats.candidates
		sum.OptOutsDetected += stats.optOuts

		// Per-conversation heartbeat (REQ-0028-014): an unfinished row whose
		// updated_at went stale reads as crashed, not in progress.
		heartbeatSpamRun(ctx, st, runID, sum)
	}

	// Wholesale, never incremental: an opt-out detected in this run changes the
	// flag on messages scanned long before it. See RecomputeSpamAfterOptOut.
	if err := st.RecomputeSpamAfterOptOut(ctx, version); err != nil {
		return sum, err
	}

	sum.DurationMS = time.Since(start).Milliseconds()
	log.Info("spam scan complete", "ruleset", version, "conversations", sum.Conversations,
		"messages", sum.MessagesScanned, "candidates", sum.Candidates,
		"opt_outs", sum.OptOutsDetected, "duration_ms", sum.DurationMS)
	return sum, nil
}

// runRow converts the running Summary into the store row shape.
func runRow(sum Summary) store.SpamRun {
	return store.SpamRun{
		RulesetVersion:  sum.RulesetVersion,
		ScanEnv:         sum.ScanEnv,
		AddressBook:     sum.AddressBook,
		Degraded:        sum.Degraded,
		Conversations:   sum.Conversations,
		MessagesScanned: sum.MessagesScanned,
		Findings:        sum.Findings,
		Candidates:      sum.Candidates,
		OptOutsDetected: sum.OptOutsDetected,
		Senders:         sum.Senders,
	}
}

// heartbeatSpamRun refreshes a run's live counters. Best-effort by design: a
// failed heartbeat write must never abort a scan that is otherwise working,
// it only degrades the liveness signal for readers.
func heartbeatSpamRun(ctx context.Context, st *store.Store, id int64, sum Summary) {
	row := runRow(sum)
	if err := st.UpdateSpamRunProgress(ctx, id, row.Conversations,
		row.MessagesScanned, row.Findings, row.Candidates, row.OptOutsDetected,
		row.Senders, time.Now()); err != nil {
		slog.Debug("spam: run heartbeat failed", "err", err)
	}
}

// finishSpamRun stamps the terminal state on a run's row. Best-effort: a
// terminal write failure cannot change the scan's outcome, but without it
// the row would read as crashed when the process exits cleanly.
func finishSpamRun(ctx context.Context, st *store.Store, id int64, start, end time.Time, sum Summary, runErr error) {
	row := runRow(sum)
	row.ID = id
	row.FinishedAt = end
	row.DurationMS = end.Sub(start).Milliseconds()
	if runErr != nil {
		row.Error = runErr.Error()
	}
	if err := st.FinishSpamRun(ctx, row); err != nil {
		slog.Debug("spam: finish run log failed", "err", err)
	}
}

type convStats struct {
	scanned    int
	findings   int
	candidates int
	optOuts    int
}

// scanConversation walks one thread from its stored cursor, classifying and
// persisting batch by batch.
func scanConversation(ctx context.Context, st *store.Store, rules *Rules, version, env string, batch int, c store.SpamConversation, identifier string) (convStats, error) {
	var stats convStats

	// Resume point. A stored version that differs from the current ruleset
	// means those findings answer a different question: rescan from the top.
	// Every write is an idempotent upsert, so a restart is always safe.
	var cursorTS, cursorID int64
	if lastHash, storedVersion, ok, err := st.GetSpamState(ctx, c.ID); err != nil {
		return stats, err
	} else if ok && storedVersion == version {
		if ts, id, found, err := st.ResolveCursor(ctx, c.ID, lastHash); err != nil {
			return stats, err
		} else if found {
			cursorTS, cursorID = ts, id
		}
		// found == false means the cursor's message is gone (re-export); the
		// zero cursor restarts this conversation from the top.
	}

	const maxBatches = 100_000 // defensive backstop; the cursor always advances
	for range maxBatches {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		page, err := st.GetMessages(ctx, c.ID, cursorTS, cursorID, batch, false)
		if err != nil {
			return stats, err
		}
		if len(page.Messages) == 0 {
			break
		}

		var (
			findings []store.SpamFinding
			events   []store.SpamEvent
			anchor   string
			first    int64
			last     int64
		)
		for _, m := range page.Messages {
			if m.IsSystem || strings.TrimSpace(m.Body) == "" {
				continue
			}
			anchor = m.Hash
			stats.scanned++
			if first == 0 || m.TSUnix < first {
				first = m.TSUnix
			}
			if m.TSUnix > last {
				last = m.TSUnix
			}

			f := store.SpamFinding{
				MessageHash: m.Hash,
				Source:      c.Source,
				Identifier:  identifier,
				Direction:   store.SpamInbound,
				TSUnix:      m.TSUnix,
			}
			if m.IsOwner {
				f.Direction = store.SpamOutbound
				if kind := rules.MatchOptOut(m.Body); kind != "" {
					events = append(events, store.SpamEvent{
						Source:      c.Source,
						Identifier:  identifier,
						EventType:   kind,
						EventAt:     m.TS,
						EventAtUnix: m.TSUnix,
						Details:     "detected in an outbound message during scan",
						Origin:      "scan",
						MessageHash: m.Hash,
					})
					stats.optOuts++
				}
			} else {
				cl := rules.Classify(identifier, m.Body)
				f.Reasons, f.URLs, f.Phones = cl.Reasons, cl.URLs, cl.Phones
				f.Emails, f.Names, f.Entities = cl.Emails, cl.Names, cl.Entities
				f.IsCandidate = cl.IsCandidate
				if cl.IsCandidate {
					stats.candidates++
				}
			}
			findings = append(findings, f)
		}

		// Anchor the cursor on the last REAL message in the batch. Re-export
		// reformats volatile system lines (changing their hash), and anchoring
		// on one would fail to resolve later and force a full rescan.
		if anchor == "" {
			anchor = page.Messages[len(page.Messages)-1].Hash
		}
		sender := store.SpamSender{
			Source:           c.Source,
			Identifier:       identifier,
			ConversationName: c.Name,
			FirstSeenUnix:    first,
			LastSeenUnix:     last,
		}
		if err := st.PutSpamBatch(ctx, c.ID, version, env, anchor, sender, findings, events); err != nil {
			return stats, err
		}
		stats.findings += len(findings)

		cursorTS, cursorID = page.NextTSUnix, page.NextID
		if len(page.Messages) < batch {
			break
		}
	}
	return stats, nil
}

// senderIdentifier canonicalizes a conversation name into the identifier the
// evidence layer keys on. A blank or unnameable thread is skipped.
func senderIdentifier(convName string) (string, bool) {
	id := contacts.Normalize(convName)
	if id.IsZero() {
		return "", false
	}
	return id.Value, true
}

// shapedLikeAHandle reports whether a conversation name is a bare phone number
// or email — the shape an UNKNOWN counterparty takes in an export, since the
// exporter names a thread after the person when it can resolve one.
//
// This is the degraded-mode gate, and it is a heuristic, not a fact: a thread
// exported before the sender was added to Contacts still carries a number.
// SPEC-0028 requires the summary to say when a run relied on it.
func shapedLikeAHandle(convName string) bool {
	switch contacts.Normalize(convName).Kind {
	case contacts.KindPhone, contacts.KindEmail:
		return true
	default:
		return false
	}
}
