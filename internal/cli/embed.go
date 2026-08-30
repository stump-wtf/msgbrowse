package cli

import (
	"fmt"

	"github.com/joestump/msgbrowse/internal/embed"
	"github.com/spf13/cobra"
)

func newEmbedCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "embed",
		Short: "Compute embeddings for messages (for semantic search)",
		Long: "embed sends message text to the configured embedding model and stores the\n" +
			"resulting vectors for semantic search. It is incremental: only messages\n" +
			"without an embedding for the current model are processed, so re-running after\n" +
			"an import embeds just the new messages.\n" +
			"\n" +
			"This step performs network egress to llm.base_url. Point it at a local\n" +
			"endpoint (the default) to keep message content on the machine.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			prune, err := cmd.Flags().GetBool("prune")
			if err != nil {
				return err
			}
			batchSize, err := cmd.Flags().GetInt("batch-size")
			if err != nil {
				return err
			}

			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()

			sum, err := embed.Run(cmd.Context(), st, newLLMClient(cfg), embed.Options{
				EmbedModel: cfg.LLM.EmbedModel,
				BatchSize:  batchSize,
				Prune:      prune,
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"embed: %d embedded in %d batches (%d pruned) in %dms\n",
				sum.Embedded, sum.Batches, sum.Pruned, sum.DurationMS)
			return err
		},
	}
	cmd.Flags().Bool("prune", false, "remove embeddings whose message no longer exists before embedding")
	cmd.Flags().Int("batch-size", 0, "messages per embeddings request (0 = default 32; some backends reject larger batches)")
	return cmd
}
