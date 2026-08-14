package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// These are overridden at build time via -ldflags "-X ...". See the Makefile.
var (
	// Version is the semantic version or git describe string.
	Version = "dev"
	// Commit is the short git commit hash.
	Commit = "none"
	// BuildDate is the RFC3339 build timestamp.
	BuildDate = "unknown"
)

// versionLine is the single rendering of build metadata. Both surfaces use it:
// the `version` subcommand and the `--version` flag fang installs on the root
// (fang.WithVersion), which would otherwise print cobra's shorter default and
// disagree with the subcommand.
func versionLine() string {
	return fmt.Sprintf("msgbrowse %s (commit %s, built %s, %s)",
		Version, Commit, BuildDate, runtime.Version())
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), versionLine())
			return err
		},
	}
}
