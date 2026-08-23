package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/spam"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/spf13/cobra"
)

// newSpamCommand builds the `msgbrowse spam` tree: a local, deterministic
// evidence layer over the imported archive (ADR-0029 / SPEC-0028).
//
// It reads and reports. It never sends a message, never replies to a sender,
// and never files anything with the FCC, the FTC, or a carrier. You do those by
// hand; `spam event add` records that you did, with the confirmation number.
func newSpamCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "spam",
		Short: "Build an evidence record of unsolicited contact",
		Long: "spam turns the imported archive into a record of unsolicited contact: which\n" +
			"strangers messaged you, which of your rules each message tripped, when you\n" +
			"told them to stop, and what arrived afterwards.\n" +
			"\n" +
			"It reads and reports, and nothing else. No message is sent, no sender is\n" +
			"replied to, and nothing is filed with the FCC, the FTC, or a carrier — you\n" +
			"do those by hand and `spam event add` records that you did.\n" +
			"\n" +
			"Classification is local, deterministic, and regex-based. Unlike facts,\n" +
			"sentiment, and journal digests, this command performs NO network egress at\n" +
			"all: no LLM call, no carrier lookup. Nothing about your messages leaves the\n" +
			"machine when it runs.\n" +
			"\n" +
			"Nothing here is legal advice. It organizes facts; a lawyer decides what they\n" +
			"mean.",
	}
	cmd.AddCommand(
		newSpamScanCommand(),
		newSpamSendersCommand(),
		newSpamViolationsCommand(),
		newSpamEvidenceCommand(),
		newSpamSenderSetCommand(),
		newSpamEventCommand(),
	)
	return cmd
}

// rulesFromConfig maps the config block onto the scan's ruleset.
func rulesFromConfig(c config.SpamConfig) (*spam.Rules, error) {
	return spam.NewRules(spam.Rules{
		MyNumbers:        c.MyNumbers,
		Allowlist:        c.Allowlist,
		WatchAreaCodes:   c.WatchAreaCodes,
		NameVariants:     c.NameVariants,
		FlagAnyURL:       c.FlagAnyURL,
		ShortenerDomains: c.ShortenerDomains,
		EntityKeywords:   c.EntityKeywords,
		StopKeywords:     c.StopKeywords,
		CannedNotice:     c.CannedNotice,
		NoticeMatchRatio: c.CannedNoticeMatchRatio,
		Exclude:          c.ExcludeConversations,
	})
}

func newSpamScanCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Classify the archive against your spam rules",
		Long: "scan walks every one-to-one conversation, records who is a stranger, applies\n" +
			"your rules to each inbound message, detects the opt-outs you sent, and\n" +
			"recomputes which inbound messages arrived after one.\n" +
			"\n" +
			"It is incremental: a per-conversation cursor means a re-run after an import\n" +
			"only examines new messages. Changing ANY rule under `spam:` in the config\n" +
			"changes the ruleset version and rescans everything, because findings from two\n" +
			"rule sets are not comparable and must never share a dossier.\n" +
			"\n" +
			"Who counts as a stranger is decided by the address book. The default binary\n" +
			"links no address-book provider, so the scan falls back to examining only\n" +
			"threads named by a bare phone number or email address and says so in its\n" +
			"summary. Build with `-tags macoscontacts` on macOS, or run the desktop app,\n" +
			"to use real Contacts.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			rules, err := rulesFromConfig(cfg.Spam)
			if err != nil {
				return err
			}
			reset, err := cmd.Flags().GetBool("reset")
			if err != nil {
				return err
			}
			convID, err := cmd.Flags().GetInt64("conversation")
			if err != nil {
				return err
			}
			batch, err := cmd.Flags().GetInt("batch-size")
			if err != nil {
				return err
			}

			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()

			sum, err := spam.Run(cmd.Context(), st, spam.Options{
				Rules:              rules,
				AddressBook:        newContactResolver(slog.Default()),
				OnlyConversationID: convID,
				Reset:              reset,
				BatchSize:          batch,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "spam: ruleset %s — %d sender(s), %d message(s) examined, %d candidate(s), %d opt-out(s) detected in %dms\n",
				sum.RulesetVersion, sum.Senders, sum.MessagesScanned, sum.Candidates, sum.OptOutsDetected, sum.DurationMS)
			fmt.Fprintf(out, "spam: skipped %d in your address book, %d allowlisted, %d your own line\n",
				sum.SkippedInContact, sum.SkippedAllowlist, sum.SkippedOwner)
			if sum.Degraded {
				fmt.Fprintf(out, "spam: WARNING — no readable address book (%s). Only threads named by a bare phone/email were examined (%d skipped for not being handle-shaped). A thread named after a person you know is invisible to this run, and a thread still named by a number is treated as a stranger even if you know them. Build with -tags macoscontacts, or run the desktop app, for a complete answer.\n",
					sum.AddressBook, sum.SkippedNotShaped)
			}
			return nil
		},
	}
	cmd.Flags().Bool("reset", false, "clear findings, cursors and detected opt-outs first (your notes, statuses and filed events are kept)")
	cmd.Flags().Int64("conversation", 0, "limit the scan to a single conversation id")
	cmd.Flags().Int("batch-size", 200, "messages per transaction")
	return cmd
}

func newSpamSendersCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "senders",
		Short: "List the strangers who have messaged you",
		Long: "senders lists non-contact counterparties with their date range, how much they\n" +
			"sent, whether you opted out, and how many messages arrived afterwards.\n" +
			"\n" +
			"Statuses: `seen` (a stranger who tripped no rule — recorded so you can promote\n" +
			"them later), `watch` (tripped a rule; set automatically), `tracked` (you\n" +
			"promoted them by hand — this is the set a dossier is for), and `ignored`.\n" +
			"Defaults to watch and tracked.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			rules, err := rulesFromConfig(cfg.Spam)
			if err != nil {
				return err
			}
			statuses, err := cmd.Flags().GetStringSlice("status")
			if err != nil {
				return err
			}
			asJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return err
			}

			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()

			senders, err := st.ListSpamSenders(cmd.Context(), statuses)
			if err != nil {
				return err
			}
			counts, err := st.SpamCounts(cmd.Context(), rules.Version())
			if err != nil {
				return err
			}
			return renderSenders(cmd.OutOrStdout(), senders, counts, asJSON)
		},
	}
	cmd.Flags().StringSlice("status", []string{store.SpamStatusWatch, store.SpamStatusTracked}, "statuses to list: seen, watch, tracked, ignored (repeatable; empty for all)")
	cmd.Flags().Bool("json", false, "emit JSON instead of a table")
	return cmd
}

type senderRow struct {
	Source      string `json:"source"`
	Identifier  string `json:"identifier"`
	Status      string `json:"status"`
	FirstSeen   string `json:"first_seen,omitempty"`
	LastSeen    string `json:"last_seen,omitempty"`
	Inbound     int    `json:"inbound"`
	Candidates  int    `json:"candidates"`
	AfterOptOut int    `json:"after_opt_out"`
	Entity      string `json:"suspected_entity,omitempty"`
	Consent     string `json:"consent_status"`
}

func renderSenders(w io.Writer, senders []store.SpamSender, counts map[string]store.SpamSenderCounts, asJSON bool) error {
	rows := make([]senderRow, 0, len(senders))
	for _, s := range senders {
		c := counts[store.SpamCountsKey(s.Source, s.Identifier)]
		rows = append(rows, senderRow{
			Source:      s.Source,
			Identifier:  s.Identifier,
			Status:      s.Status,
			FirstSeen:   unixDay(s.FirstSeenUnix),
			LastSeen:    unixDay(s.LastSeenUnix),
			Inbound:     c.Inbound,
			Candidates:  c.Candidates,
			AfterOptOut: c.AfterOptOut,
			Entity:      s.SuspectedEntity,
			Consent:     s.ConsentStatus,
		})
	}
	if asJSON {
		return writeJSON(w, rows)
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "No senders recorded. Run `msgbrowse spam scan` first.")
		return err
	}
	fmt.Fprintf(w, "%-22s %-9s %-9s %-10s %-10s %8s %10s %12s\n",
		"IDENTIFIER", "SOURCE", "STATUS", "FIRST", "LAST", "INBOUND", "CANDIDATE", "AFTER OPT-OUT")
	for _, r := range rows {
		fmt.Fprintf(w, "%-22s %-9s %-9s %-10s %-10s %8d %10d %12d\n",
			truncate(r.Identifier, 22), r.Source, r.Status, r.FirstSeen, r.LastSeen,
			r.Inbound, r.Candidates, r.AfterOptOut)
	}
	return nil
}

func newSpamViolationsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "violations",
		Short: "Show every sender who contacted you after an opt-out",
		Long: "violations lists each sender who kept messaging after you told them to stop,\n" +
			"with the offending messages verbatim.\n" +
			"\n" +
			"Two counts are reported and they are not the same number. The trailing\n" +
			"12-month count is what a form usually asks for. The largest count in ANY\n" +
			"12-month window is the one the Do Not Call private right of action turns on,\n" +
			"because it hinges on more than one contact from the same entity within any\n" +
			"twelve-month period — not within the last twelve months specifically.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			rules, err := rulesFromConfig(cfg.Spam)
			if err != nil {
				return err
			}
			asJSON, err := cmd.Flags().GetBool("json")
			if err != nil {
				return err
			}

			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()

			senders, err := st.ListSpamSenders(cmd.Context(), nil)
			if err != nil {
				return err
			}
			var found []spam.Dossier
			now := time.Now()
			for _, s := range senders {
				d, err := spam.BuildDossier(cmd.Context(), st, s, rules.Version(), now)
				if err != nil {
					return err
				}
				if d.Stats.AfterOptOut > 0 {
					found = append(found, d)
				}
			}

			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSON(out, found)
			}
			if len(found) == 0 {
				_, err := fmt.Fprintln(out, "No messages after an opt-out on record.")
				return err
			}
			for _, d := range found {
				fmt.Fprintf(out, "%s (%s)\n", d.Sender.Identifier, d.Sender.Source)
				fmt.Fprintf(out, "  opt-out sent: %s (%s)\n", d.Stats.OptOutAt, d.Stats.OptOutType)
				fmt.Fprintf(out, "  inbound after opt-out: %d\n", d.Stats.AfterOptOut)
				fmt.Fprintf(out, "  inbound in trailing 12 months: %d\n", d.Stats.Trailing12Months)
				fmt.Fprintf(out, "  worst 12-month window: %d inbound (%s to %s)\n",
					d.Stats.WorstWindow.Count, d.Stats.WorstWindow.Start, d.Stats.WorstWindow.End)
				for _, m := range d.Messages {
					if !m.IsAfterOptOut {
						continue
					}
					fmt.Fprintf(out, "    %s  %s\n", m.Timestamp, oneLine(m.Body))
				}
				fmt.Fprintln(out)
			}
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "emit JSON instead of text")
	return cmd
}

func newSpamEvidenceCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "evidence",
		Short: "Export a per-sender dossier",
		Long: "evidence writes the record for one sender: every message verbatim with its\n" +
			"timestamp, the rules each tripped, the links, callback numbers and names\n" +
			"pulled out of the bodies, every recorded event with its confirmation number,\n" +
			"a SHA-256 of each body so a later alteration is detectable, and an explicit\n" +
			"list of what this record cannot establish.\n" +
			"\n" +
			"Markdown and JSON render from the same value, so they cannot disagree.\n" +
			"\n" +
			"Read the limitations section before relying on a dossier in a filing. The\n" +
			"large one: msgbrowse reads exporter output, and the iMessage text format\n" +
			"records local wall-clock time with no UTC offset, so these timestamps are\n" +
			"NOT timezone-qualified.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			rules, err := rulesFromConfig(cfg.Spam)
			if err != nil {
				return err
			}
			identifier, err := cmd.Flags().GetString("identifier")
			if err != nil {
				return err
			}
			if strings.TrimSpace(identifier) == "" {
				return fmt.Errorf("--identifier is required (the sender's number, email, or handle)")
			}
			format, err := cmd.Flags().GetString("format")
			if err != nil {
				return err
			}
			if err := validateDossierFormat(format); err != nil {
				return err
			}
			outDir, err := cmd.Flags().GetString("out")
			if err != nil {
				return err
			}
			toStdout, err := cmd.Flags().GetBool("stdout")
			if err != nil {
				return err
			}

			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()

			sender, err := resolveSpamSender(cmd, st, identifier)
			if err != nil {
				return err
			}
			d, err := spam.BuildDossier(cmd.Context(), st, sender, rules.Version(), time.Now())
			if err != nil {
				return err
			}

			if toStdout {
				out := cmd.OutOrStdout()
				if format == "json" {
					b, err := d.JSON()
					if err != nil {
						return err
					}
					_, err = out.Write(b)
					return err
				}
				_, err := io.WriteString(out, d.Markdown())
				return err
			}

			dir := outDir
			if dir == "" {
				dir = cfg.Spam.ExportDir
			}
			if dir == "" {
				dir = filepath.Join(cfg.DataDir, "spam-exports")
			}
			// A dossier is a plaintext copy of every message from one sender.
			// 0700/0600, same posture as backups (ADR-0026).
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return fmt.Errorf("create export dir: %w", err)
			}
			base := fmt.Sprintf("dossier-%s-%s", safeFilename(sender.Identifier), time.Now().Format("2006-01-02"))
			var written []string
			if format == "md" || format == "both" {
				p := filepath.Join(dir, base+".md")
				if err := os.WriteFile(p, []byte(d.Markdown()), 0o600); err != nil {
					return fmt.Errorf("write dossier: %w", err)
				}
				written = append(written, p)
			}
			if format == "json" || format == "both" {
				b, err := d.JSON()
				if err != nil {
					return err
				}
				p := filepath.Join(dir, base+".json")
				if err := os.WriteFile(p, b, 0o600); err != nil {
					return fmt.Errorf("write dossier: %w", err)
				}
				written = append(written, p)
			}
			for _, p := range written {
				fmt.Fprintln(cmd.OutOrStdout(), p)
			}
			return nil
		},
	}
	cmd.Flags().String("identifier", "", "the sender's number, email, or handle")
	cmd.Flags().String("format", "both", "md, json, or both")
	cmd.Flags().String("out", "", "output directory (default: spam.export_dir, else <data_dir>/spam-exports)")
	cmd.Flags().Bool("stdout", false, "write to stdout instead of a file")
	return cmd
}

func newSpamSenderSetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sender-set",
		Short: "Record your judgment about a sender",
		Long: "sender-set writes the columns a scan will never touch: the status you\n" +
			"promoted them to, who you believe is behind the number, and whether consent\n" +
			"was ever given.\n" +
			"\n" +
			"consent_status defaults to no_consent_on_record for every sender, because a\n" +
			"prior business relationship is the usual defense and the record has to say\n" +
			"plainly that none is on file until someone says otherwise.\n" +
			"\n" +
			"Only the flags you pass are written, so setting --notes does not blank the\n" +
			"suspected entity.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			identifier, err := cmd.Flags().GetString("identifier")
			if err != nil {
				return err
			}
			if strings.TrimSpace(identifier) == "" {
				return fmt.Errorf("--identifier is required")
			}

			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()

			sender, err := resolveSpamSender(cmd, st, identifier)
			if err != nil {
				return err
			}
			status := changedFlag(cmd, "status")
			if status != nil && !validSpamStatus(*status) {
				return fmt.Errorf("invalid --status %q (want seen, watch, tracked, or ignored)", *status)
			}
			consent := changedFlag(cmd, "consent")
			if consent != nil && !validSpamConsent(*consent) {
				return fmt.Errorf("invalid --consent %q (want %s, %s, %s, or %s)", *consent,
					store.SpamConsentNone, store.SpamConsentGiven, store.SpamConsentRevoked, store.SpamConsentDisputed)
			}
			if err := st.SetSpamSenderFields(cmd.Context(), sender.Source, sender.Identifier,
				status, changedFlag(cmd, "entity"), consent,
				changedFlag(cmd, "consent-notes"), changedFlag(cmd, "notes")); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "updated %s (%s)\n", sender.Identifier, sender.Source)
			return err
		},
	}
	cmd.Flags().String("identifier", "", "the sender's number, email, or handle")
	cmd.Flags().String("status", "", "seen, watch, tracked, or ignored")
	cmd.Flags().String("entity", "", "who you believe is behind the number (unconfirmed)")
	cmd.Flags().String("consent", "", "no_consent_on_record, consent_given, consent_revoked, or disputed")
	cmd.Flags().String("consent-notes", "", "why — this is where a prior business relationship is conceded or rebutted")
	cmd.Flags().String("notes", "", "anything else")
	return cmd
}

func newSpamEventCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "event",
		Short: "Record and list things that happened outside the message stream",
	}

	add := &cobra.Command{
		Use:   "add",
		Short: "Record something you did by hand",
		Long: "add records an action you took outside the archive — forwarding to 7726,\n" +
			"filing with the FCC or the FTC, a lawyer referral — with its confirmation\n" +
			"number. msgbrowse files nothing for you; this is how the record shows that\n" +
			"you did.\n" +
			"\n" +
			"--at defaults to now. Pass it when you are backfilling: an event is dated\n" +
			"when it HAPPENED, not when you typed it in. Adding a stop_sent or\n" +
			"notice_sent recomputes which inbound messages arrived after an opt-out\n" +
			"across the whole archive.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			rules, err := rulesFromConfig(cfg.Spam)
			if err != nil {
				return err
			}
			identifier, _ := cmd.Flags().GetString("identifier")
			eventType, _ := cmd.Flags().GetString("type")
			details, _ := cmd.Flags().GetString("details")
			atRaw, _ := cmd.Flags().GetString("at")
			source, _ := cmd.Flags().GetString("source")

			if strings.TrimSpace(identifier) == "" {
				return fmt.Errorf("--identifier is required")
			}
			if !spam.ValidEventType(eventType) {
				return fmt.Errorf("invalid --type %q (want one of: %s)", eventType, strings.Join(spam.ManualEventTypes, ", "))
			}
			at := time.Now()
			if atRaw != "" {
				parsed, perr := time.Parse(time.RFC3339, atRaw)
				if perr != nil {
					return fmt.Errorf("invalid --at %q (want RFC3339, e.g. 2026-08-20T10:15:00-07:00): %w", atRaw, perr)
				}
				at = parsed
			}

			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()

			// An event may name a sender no scan has reached yet, so resolution
			// is best-effort: fall back to the identifier as typed.
			canonical, src := identifier, source
			if sender, ferr := resolveSpamSender(cmd, st, identifier); ferr == nil {
				canonical, src = sender.Identifier, sender.Source
			}
			if src == "" {
				return fmt.Errorf("no sender %q on record — pass --source (signal, imessage, whatsapp) to create the event anyway", identifier)
			}

			if err := st.AddSpamEvent(cmd.Context(), store.SpamEvent{
				Source:      src,
				Identifier:  canonical,
				EventType:   eventType,
				EventAt:     at.Format(time.RFC3339),
				EventAtUnix: at.Unix(),
				Details:     details,
				Origin:      "manual",
			}); err != nil {
				return err
			}
			// An opt-out recorded today changes the flag on messages scanned
			// months ago, so the recompute is wholesale and runs here too.
			if err := st.RecomputeSpamAfterOptOut(cmd.Context(), rules.Version()); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "recorded %s for %s at %s\n", eventType, canonical, at.Format(time.RFC3339))
			return err
		},
	}
	add.Flags().String("identifier", "", "the sender's number, email, or handle")
	add.Flags().String("type", "", "one of: "+strings.Join(spam.ManualEventTypes, ", "))
	add.Flags().String("details", "", "confirmation number, ticket id, or note")
	add.Flags().String("at", "", "when it happened, RFC3339 (default: now)")
	add.Flags().String("source", "", "source to attribute the event to when the sender is not yet on record")

	list := &cobra.Command{
		Use:   "list",
		Short: "List recorded events",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			identifier, _ := cmd.Flags().GetString("identifier")

			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()

			source, canonical := "", ""
			if strings.TrimSpace(identifier) != "" {
				sender, ferr := resolveSpamSender(cmd, st, identifier)
				if ferr != nil {
					return ferr
				}
				source, canonical = sender.Source, sender.Identifier
			}
			events, err := st.ListSpamEvents(cmd.Context(), source, canonical)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(events) == 0 {
				_, err := fmt.Fprintln(out, "No events recorded.")
				return err
			}
			for _, e := range events {
				fmt.Fprintf(out, "%s  %-24s %-24s %-7s %s\n", e.EventAt, e.Identifier, e.EventType, e.Origin, e.Details)
			}
			return nil
		},
	}
	list.Flags().String("identifier", "", "limit to one sender")

	cmd.AddCommand(add, list)
	return cmd
}

// resolveSpamSender turns a user-typed identifier into exactly one sender row,
// or an error that says what to do next. An ambiguous identifier (the same
// number on two sources) is an error rather than a silent pick — a dossier
// assembled from the wrong source is worse than no dossier.
func resolveSpamSender(cmd *cobra.Command, st *store.Store, identifier string) (store.SpamSender, error) {
	ctx := cmd.Context()
	matches, err := st.FindSpamSenders(ctx, identifier)
	if err != nil {
		return store.SpamSender{}, err
	}
	if len(matches) == 0 {
		// Try the canonical form of what was typed: "(555) 123-4567" and
		// "+15551234567" name the same sender.
		if canon := spam.MatchKey(identifier); canon != "" && canon != identifier {
			all, aerr := st.ListSpamSenders(ctx, nil)
			if aerr != nil {
				return store.SpamSender{}, aerr
			}
			for _, s := range all {
				if spam.MatchKey(s.Identifier) == canon {
					matches = append(matches, s)
				}
			}
		}
	}
	switch len(matches) {
	case 0:
		return store.SpamSender{}, fmt.Errorf("no sender matching %q on record — run `msgbrowse spam scan`, or check `msgbrowse spam senders --status seen`", identifier)
	case 1:
		return matches[0], nil
	default:
		var names []string
		for _, m := range matches {
			names = append(names, m.Source+":"+m.Identifier)
		}
		return store.SpamSender{}, fmt.Errorf("%q matches more than one sender (%s) — this identifier appears on multiple sources", identifier, strings.Join(names, ", "))
	}
}

// changedFlag returns a pointer to a string flag's value only when the user
// actually passed it, so an unset flag never blanks a stored column.
func changedFlag(cmd *cobra.Command, name string) *string {
	if !cmd.Flags().Changed(name) {
		return nil
	}
	v, err := cmd.Flags().GetString(name)
	if err != nil {
		return nil
	}
	return &v
}

func validSpamStatus(s string) bool {
	switch s {
	case store.SpamStatusSeen, store.SpamStatusWatch, store.SpamStatusTracked, store.SpamStatusIgnored:
		return true
	}
	return false
}

func validSpamConsent(s string) bool {
	switch s {
	case store.SpamConsentNone, store.SpamConsentGiven, store.SpamConsentRevoked, store.SpamConsentDisputed:
		return true
	}
	return false
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func unixDay(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).UTC().Format("2006-01-02")
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// safeFilename keeps a dossier filename to characters that behave the same on
// every filesystem: "+15551234567" is fine, an email address or a handle with a
// slash is not.
// validateDossierFormat rejects an unrecognized --format before any work
// happens.
//
// Checked only at the write, as it originally was, a bad value silently
// rendered Markdown under --stdout — the JSON branch is an equality test, so
// everything else fell through to it — and on the file path it still created
// the export directory before refusing. A command whose output is evidence
// must not quietly hand back a format nobody asked for.
func validateDossierFormat(format string) error {
	switch format {
	case "md", "json", "both":
		return nil
	default:
		return fmt.Errorf("invalid --format %q (want md, json, or both)", format)
	}
}

func safeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '+', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
