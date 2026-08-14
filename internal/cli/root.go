// Package cli wires msgbrowse's Cobra command tree to its Viper configuration.
//
// The root command owns the persistent flags shared by every subcommand and the
// config-loading lifecycle; each subcommand lives in its own file and receives a
// fully-resolved *config.Config via resolveConfig.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"charm.land/fang/v2"
	charmlog "github.com/charmbracelet/log"
	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/imessage"
	"github.com/joestump/msgbrowse/internal/ingest"
	"github.com/joestump/msgbrowse/internal/whatsapp"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// cfgFile holds the value of the global --config flag.
var cfgFile string

// v is the process-wide Viper instance, initialized in the root
// PersistentPreRunE after flag parsing.
var v *viper.Viper

// NewRootCommand builds the root `msgbrowse` command and attaches every
// subcommand. It is exported so tests can exercise the command tree.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "msgbrowse",
		Short: "Browse, search, and editorialize your message archives locally",
		Long: "msgbrowse is a self-hosted, local-only web app and MCP server over your\n" +
			"message-archive exports. It treats every archive as strictly read-only and\n" +
			"keeps all data on the machine; the only network egress is the configured\n" +
			"OpenAI-compatible LLM endpoint.\n" +
			"\n" +
			"Sources: signal-export (signal-import), imessage-exporter (imessage-import),\n" +
			"and WhatsApp-Chat-Exporter (whatsapp-import).",
		SilenceUsage:  true,
		SilenceErrors: true,
		// PersistentPreRunE loads config and binds flags once, before any
		// subcommand RunE. Using it (instead of cobra.OnInitialize) gives us the
		// invoked command's flag set directly.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return initConfig(cmd)
		},
	}

	// fang.WithVersion installs a --version flag on the root; pin its output to
	// the same line the `version` subcommand prints so the two agree.
	root.SetVersionTemplate(versionLine() + "\n")

	pf := root.PersistentFlags()
	pf.StringVar(&cfgFile, "config", "", "config file (default: ./config.yaml or $HOME/.config/msgbrowse/config.yaml)")
	pf.String("archive-root", "", "path to the signal-export archive (read-only)")
	pf.String("imessage-archive-root", "", "path to the imessage-exporter archive (read-only)")
	pf.String("whatsapp-archive-root", "", "path to the WhatsApp-Chat-Exporter output directory (read-only)")
	pf.String("data-dir", "", "writable directory for the database and caches")
	pf.String("log-level", "", "log level: debug, info, warn, error")

	root.AddCommand(
		newImportCommand(),
		newSignalImportCommand(),
		newIngestAliasCommand(),
		newIMessageImportCommand(),
		newWhatsAppImportCommand(),
		newDevicesCommand(),
		newDoctorCommand(),
		newExportCommand(),
		newSyncCommand(),
		newEmbedCommand(),
		newFactsCommand(),
		newMediaCommand(),
		newServeCommand(),
		newMCPCommand(),
		newWatchCommand(),
		newJournalCommand(),
		newBackupsCommand(),
		newVersionCommand(),
	)
	return root
}

// Execute runs the root command through fang (charm.land/fang/v2), the same
// help/error layering Crush uses: --help for every command renders fang's
// aligned command/flag tables, and failures render as its styled ERROR block
// — both re-skinned with the Slate palette via slateColorScheme (#330).
//
// It still installs the pretty default logger up front (so log lines emitted
// before per-command config resolution render nicely); fang owns only the
// help and error surfaces. Exit codes are unchanged: main sets them.
//
// fang also injects a hidden `man` subcommand and keeps cobra's completions —
// both harmless extras. Its signal handling (WithNotifySignal) is deliberately
// NOT used: this change is presentation-only, and Ctrl-C semantics belong to
// the commands' own context handling.
func Execute() error {
	configureLogger("info")
	return fang.Execute(
		context.Background(),
		NewRootCommand(),
		fang.WithVersion(Version),
		fang.WithCommit(Commit),
		fang.WithColorSchemeFunc(slateColorScheme),
		fang.WithErrorHandler(slateErrorHandler),
	)
}

// slateErrorHandler renders a command failure through fang's styled output
// while keeping msgbrowse's actionable failure hints (errorHint). The hint
// line inherits fang's error-text style, so it aligns under the message and
// colorprofile strips its styling automatically on non-color output.
func slateErrorHandler(w io.Writer, styles fang.Styles, err error) {
	// `doctor` already printed a full human-readable report to stdout; its
	// sentinel only exists to make the process exit non-zero. Don't re-report
	// it as a fang error block.
	if errors.Is(err, errDoctorFailed) {
		return
	}
	// fang's ErrorText title-cases the first word. Several of our errors lead
	// with a literal config key (`data_dir must not be empty`,
	// `device_sync.listen_addr …`, `archive_root …`), and capitalizing those
	// prints a key that does not exist in config.yaml or the environment. Drop
	// the transform and render the message verbatim.
	styles.ErrorText = styles.ErrorText.UnsetTransform()
	fang.DefaultErrorHandler(w, styles, err)
	if hint := errorHint(err); hint != "" {
		_, _ = fmt.Fprintln(w, styles.ErrorText.Render("Hint: "+hint))
	}
}

// errorHint maps known failures to an actionable next step. Matching is on
// sentinel errors (errors.Is), not strings, so it stays robust as messages
// evolve.
func errorHint(err error) string {
	switch {
	case errors.Is(err, ingest.ErrExportDirNotFound):
		return "archive_root must be the folder that CONTAINS export/ — e.g. .../Signal-Archive, not .../Signal-Archive/export. " +
			"In Docker, point MSGBROWSE_ARCHIVE_HOST in .env at that folder."
	case errors.Is(err, imessage.ErrArchiveNotFound):
		return "imessage_archive_root must point at the imessage-exporter output directory " +
			"(the folder of <ChatName>.txt files). Set --imessage-archive-root or MSGBROWSE_IMESSAGE_ARCHIVE_ROOT."
	case errors.Is(err, whatsapp.ErrArchiveNotFound):
		return "whatsapp_archive_root must point at the WhatsApp-Chat-Exporter output directory " +
			"(the folder containing result.json). Set --whatsapp-archive-root or MSGBROWSE_WHATSAPP_ARCHIVE_ROOT."
	}
	return ""
}

// initConfig loads Viper and binds the persistent flags onto their config keys.
// Only flags the user actually changed override file/env values, because Viper
// consults flag.Changed when reading a bound pflag.
func initConfig(cmd *cobra.Command) error {
	var err error
	v, err = config.Load(cfgFile)
	if err != nil {
		return err
	}
	pf := cmd.Root().PersistentFlags()
	if err := v.BindPFlag("archive_root", pf.Lookup("archive-root")); err != nil {
		return err
	}
	if err := v.BindPFlag("imessage_archive_root", pf.Lookup("imessage-archive-root")); err != nil {
		return err
	}
	if err := v.BindPFlag("whatsapp_archive_root", pf.Lookup("whatsapp-archive-root")); err != nil {
		return err
	}
	if err := v.BindPFlag("data_dir", pf.Lookup("data-dir")); err != nil {
		return err
	}
	if err := v.BindPFlag("log_level", pf.Lookup("log-level")); err != nil {
		return err
	}
	return nil
}

// resolveConfig unmarshals and validates the active configuration and configures
// the default slog logger. Every subcommand calls this at the top of its RunE.
func resolveConfig() (*config.Config, error) {
	cfg, err := config.Unmarshal(v)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	configureLogger(cfg.LogLevel)
	return cfg, nil
}

// configureLogger installs a charmbracelet/log logger (pretty, colorized,
// leveled) as the slog default. charmbracelet/log implements slog.Handler, so
// every existing slog call across import/serve/mcp gets the styled output with
// no call-site changes. Output goes to STDERR — important for `mcp` over stdio,
// whose JSON-RPC stream must stay on stdout uncorrupted.
func configureLogger(level string) {
	slog.SetDefault(slog.New(newLogHandler(level)))
}

// newLogHandler builds the charmbracelet/log handler at the requested level.
func newLogHandler(level string) slog.Handler {
	return newCharmLogger(level)
}

// newCharmLogger builds the styled charmbracelet logger at the requested
// level. Exposed separately from newLogHandler for components that take a
// *charmlog.Logger directly (the devices pairing/listener stack) so their
// output matches the CLI's slog styling.
func newCharmLogger(level string) *charmlog.Logger {
	lvl := charmlog.InfoLevel
	switch level {
	case "debug":
		lvl = charmlog.DebugLevel
	case "warn":
		lvl = charmlog.WarnLevel
	case "error":
		lvl = charmlog.ErrorLevel
	}
	logger := charmlog.NewWithOptions(os.Stderr, charmlog.Options{
		Level:           lvl,
		ReportTimestamp: true,
		TimeFormat:      "15:04:05",
	})
	// Prefix each level with an emoji so status reads at a glance. We keep the
	// level word (and charm's color) and just swap the rendered label string.
	styles := charmlog.DefaultStyles()
	for level, label := range map[charmlog.Level]string{
		charmlog.DebugLevel: "🔍 DEBUG",
		charmlog.InfoLevel:  "✅ INFO",
		charmlog.WarnLevel:  "⚠️ WARN",
		charmlog.ErrorLevel: "🛑 ERROR",
		charmlog.FatalLevel: "💀 FATAL",
	} {
		// Drop the default 4-cell MaxWidth (it would truncate "✅ INFO" → "✅ I");
		// keep the level's color from DefaultStyles.
		styles.Levels[level] = styles.Levels[level].SetString(label).UnsetMaxWidth()
	}
	logger.SetStyles(styles)
	return logger
}
