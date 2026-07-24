package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/joestump/msgbrowse/internal/journal"
	"github.com/spf13/cobra"
)

// errNotImplemented marks subcommands whose behavior lands in a later vertical
// slice. The command tree, flags, and config wiring are real today so the binary
// builds and the surface is stable.
var errNotImplemented = errors.New("not implemented yet (tracked in the project TODO)")

func newWatchCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "watch",
		Short: "Re-ingest automatically when the archive changes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := resolveConfig(); err != nil {
				return err
			}
			return errNotImplemented
		},
	}
}

func newJournalCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "journal",
		Short: "Rebuild the day-by-day journal and optional LLM digests",
		Long: "journal builds a per-day rollup of the archive (counts, sources, top people)\n" +
			"and, when journal.digest_enabled is set, sends each day's transcript to the\n" +
			"configured chat model and caches a prose digest. The mechanical rollup is\n" +
			"always built and never leaves the machine; only the digest pass performs\n" +
			"network egress to llm.base_url. Digests are cached and versioned by\n" +
			"(model, prompt): a model swap or a journal.digest_prompt edit re-derives the\n" +
			"affected days. It is incremental — a re-run only digests days without a\n" +
			"current digest — and resumable, so journal.max_days_per_run can bound a run\n" +
			"and the next one picks up the rest.\n" +
			"\n" +
			"Conversations on journal.exclude_conversations are never sent to the LLM.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			since, err := cmd.Flags().GetString("since")
			if err != nil {
				return err
			}
			backfill, err := cmd.Flags().GetBool("backfill")
			if err != nil {
				return err
			}
			regenerate, err := cmd.Flags().GetBool("regenerate")
			if err != nil {
				return err
			}
			dryRun, err := cmd.Flags().GetBool("dry-run")
			if err != nil {
				return err
			}
			if since != "" {
				if _, perr := time.Parse("2006-01-02", since); perr != nil {
					return fmt.Errorf("invalid --since %q (want YYYY-MM-DD): %w", since, perr)
				}
			}

			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()

			sum, err := journal.Run(cmd.Context(), st, newLLMClient(cfg), journal.Options{
				Model:         cfg.LLM.ChatModel,
				DigestEnabled: cfg.Journal.DigestEnabled,
				DigestPrompt:  cfg.Journal.DigestPrompt,
				Exclude:       cfg.Journal.ExcludeConversations,
				MaxDaysPerRun: cfg.Journal.MaxDaysPerRun,
				Since:         since,
				Backfill:      backfill,
				Regenerate:    regenerate,
				DryRun:        dryRun,
			})
			if err != nil {
				return err
			}

			if dryRun {
				_, err = fmt.Fprintf(cmd.OutOrStdout(),
					"journal (dry run): %d day(s) need a digest; ~%d input tokens for the next %d "+
						"(rough char/4 estimate, no tokenizer)\n",
					sum.Eligible, sum.EstimatedTokens, sum.Eligible-sum.Remaining)
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"journal: %d day(s) built, %d digested, %d cached, %d remaining (%dms)\n",
				sum.Days, sum.Digested, sum.Cached, sum.Remaining, sum.DurationMS)
			return err
		},
	}
	cmd.Flags().String("since", "", "only process days on or after this date (YYYY-MM-DD)")
	cmd.Flags().Bool("backfill", false, "digest eligible days oldest-first (fill in history) instead of newest-first")
	cmd.Flags().Bool("regenerate", false, "clear cached digests and re-derive every day")
	cmd.Flags().Bool("dry-run", false, "report the eligible day count and a rough token estimate; make no LLM calls")
	return cmd
}
