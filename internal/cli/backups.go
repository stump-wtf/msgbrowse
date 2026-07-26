package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/joestump/msgbrowse/internal/backup"
	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/spf13/cobra"
)

func newBackupsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backups",
		Short: "Create, list, prune, and restore msgbrowse-owned snapshots",
		Long: "backups manages msgbrowse-owned snapshots of data_dir (the SQLite\n" +
			"database + embeddings) and the config file (ADR-0026). A snapshot is a\n" +
			"plaintext copy of the entire message corpus, created with VACUUM INTO\n" +
			"for consistency, listed, pruned by GFS policy, and restorable.\n" +
			"\n" +
			"Snapshot files are mode 0600 / directory 0700 — they contain the\n" +
			"entire message corpus (ADR-0013: no SQLCipher in-process).",
	}
	cmd.AddCommand(
		newBackupsCreateCommand(),
		newBackupsListCommand(),
		newBackupsPruneCommand(),
		newBackupsRestoreCommand(),
	)
	return cmd
}

func newBackupsCreateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "create",
		Short: "Create a snapshot of the database, embeddings, and config",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			m, err := newBackupManager(cfg)
			if err != nil {
				return err
			}
			snap, err := m.Create(cmd.Context())
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"backups: created %s (%s) in %s\n",
				snap.ID, humanBytes(snap.SizeBytes), m.Dir())
			return err
		},
	}
}

func newBackupsListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List msgbrowse-owned snapshots (newest first)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			m, err := newBackupManager(cfg)
			if err != nil {
				return err
			}
			snaps, err := m.List()
			if err != nil {
				return err
			}
			var total int64
			for _, s := range snaps {
				total += s.SizeBytes
				label := s.ID
				if s.PreRestore {
					label += " (pre-restore)"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  %s  %s\n",
					label,
					s.TakenAt.Format("2006-01-02 15:04:05"),
					humanBytes(s.SizeBytes),
					s.Tier)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d snapshots, %s total in %s\n",
				len(snaps), humanBytes(total), m.Dir())
			return nil
		},
	}
}

func newBackupsPruneCommand() *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Apply the GFS retention policy to old snapshots",
		Long: "prune previews which snapshots would be deleted under the configured\n" +
			"GFS retention policy. Pass --apply to actually delete them. Prune never\n" +
			"deletes the only snapshot — it keeps the newest regardless of policy.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			m, err := newBackupManager(cfg)
			if err != nil {
				return err
			}
			preview, err := m.PrunePreview()
			if err != nil {
				return err
			}
			if len(preview) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "backups: nothing to prune — all snapshots are within policy")
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "backups: %d snapshots would be pruned:\n", len(preview))
			for _, s := range preview {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s  %s\n", s.ID, s.TakenAt.Format("2006-01-02"), s.Tier)
			}
			if !apply {
				fmt.Fprintln(cmd.OutOrStdout(), "\nPreview only. Pass --apply to delete.")
				return nil
			}
			deleted, err := m.Prune(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "backups: deleted %d snapshots\n", len(deleted))
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "delete the snapshots (preview only without this flag)")
	return cmd
}

func newBackupsRestoreCommand() *cobra.Command {
	var confirm bool
	cmd := &cobra.Command{
		Use:   "restore <snapshot-id>",
		Short: "Restore a snapshot over the live database (guarded)",
		Long: "restore replaces the live database with the selected snapshot. Before\n" +
			"swapping, it takes a pre-restore snapshot of the current database so a\n" +
			"mid-failure is recoverable.\n" +
			"\n" +
			"This is destructive — it requires --confirm to proceed. Without --confirm,\n" +
			"it validates the target and prints what would be restored.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			m, err := newBackupManager(cfg)
			if err != nil {
				return err
			}
			id := args[0]
			target, err := m.RestoreTarget(id)
			if err != nil {
				return fmt.Errorf("snapshot %q not found: %w", id, err)
			}
			if !confirm {
				fmt.Fprintf(cmd.OutOrStdout(),
					"backups: would restore %s (taken %s, %s, tier %s)\n"+
						"A pre-restore snapshot of the current database would be taken first.\n"+
						"Pass --confirm to proceed.\n",
					target.ID, target.TakenAt.Format("2006-01-02 15:04"), humanBytes(target.SizeBytes), target.Tier)
				return nil
			}
			pre, err := m.Restore(cmd.Context(), id)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"backups: restored %s. Pre-restore snapshot: %s. Restart the app to use the restored database.\n",
				target.ID, pre.ID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&confirm, "confirm", false, "proceed with the destructive restore (required)")
	return cmd
}

// newBackupManager builds a backup.Manager from a resolved config. It ensures
// the snapshot directory exists (0700) and resolves the config file path.
func newBackupManager(cfg *config.Config) (*backup.Manager, error) {
	dir := backup.ResolveDir(*cfg)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create snapshot dir %q: %w", dir, err)
	}
	dbPath := filepath.Join(cfg.DataDir, store.DBFileName)
	return backup.NewManager(dir, dbPath, cfg.SourceFile, cfg.Backups.EffectiveRetention()), nil
}

// humanBytes formats a byte count as a human-readable string.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for d := n / unit; d >= unit; d /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
