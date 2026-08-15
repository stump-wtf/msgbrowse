package cli

import (
	"fmt"

	"github.com/joestump/msgbrowse/internal/sentiment"
	"github.com/spf13/cobra"
)

func newSentimentCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sentiment",
		Short: "Score what messages express against the IPIP-anchored lexicon",
		Long: "sentiment sends message batches to the configured chat model and stores the\n" +
			"constructs each message expresses, scored from -1 to +1 against a curated\n" +
			"subset of the public-domain IPIP taxonomy (five Big Five domains plus ten\n" +
			"affect facets), using IPIP marker items as prompt anchors.\n" +
			"\n" +
			"It scores TEXT, not people: the scores describe what a message expresses,\n" +
			"and are not a psychological assessment of anyone. Storage is sparse — most\n" +
			"messages express nothing on the list and produce no rows at all.\n" +
			"\n" +
			"It is incremental: a per-conversation cursor means re-running after an\n" +
			"import only scores new messages. Changing the chat model or shipping a new\n" +
			"lexicon version rescans, because scores from different generations are not\n" +
			"comparable.\n" +
			"\n" +
			"Conversations on journal.exclude_conversations, and contacts who have opted\n" +
			"out on their profile, are never sent to the LLM. This step performs network\n" +
			"egress to llm.base_url; point it at a local endpoint (the default) to keep\n" +
			"message content on the machine.\n" +
			"\n" +
			"The first run on a large archive is long. It is interruptible: the cursor is\n" +
			"persisted after every batch, so re-running resumes where it stopped.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			reset, err := cmd.Flags().GetBool("reset")
			if err != nil {
				return err
			}
			batch, err := cmd.Flags().GetInt("batch-size")
			if err != nil {
				return err
			}
			concurrency, err := cmd.Flags().GetInt("concurrency")
			if err != nil {
				return err
			}
			convID, err := cmd.Flags().GetInt64("conversation")
			if err != nil {
				return err
			}

			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()

			sum, err := sentiment.Run(cmd.Context(), st, newLLMClient(cfg), sentiment.Options{
				Model:              cfg.LLM.ChatModel,
				BatchSize:          batch,
				Concurrency:        concurrency,
				Exclude:            cfg.Journal.ExcludeConversations,
				OnlyConversationID: convID,
				Reset:              reset,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if _, err := fmt.Fprintf(out,
				"sentiment: %d scores written from %d messages across %d conversations (%d batches) in %dms\n",
				sum.RowsWritten, sum.MessagesScored, sum.Conversations, sum.Batches, sum.DurationMS); err != nil {
				return err
			}
			if sum.SkippedOptedOut > 0 {
				if _, err := fmt.Fprintf(out, "sentiment: skipped %d conversation(s) whose contact opted out\n", sum.SkippedOptedOut); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().Bool("reset", false, "wipe stored scores and cursors before running (opt-outs are kept)")
	cmd.Flags().Int("batch-size", 40, "messages per scoring call")
	cmd.Flags().Int("concurrency", 4, "conversations processed in parallel")
	cmd.Flags().Int64("conversation", 0, "limit scoring to a single conversation id")
	return cmd
}
