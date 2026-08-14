// Command msgbrowse is a self-hosted, local-only browser, search engine, and
// AI-editorialized journal over your message archives.
//
// It imports from upstream exporters (signal-export and imessage-exporter) into
// a unified SQLite store and exposes several subcommands:
//
//	msgbrowse signal-import    import a signal-export Markdown archive
//	msgbrowse imessage-import  import an imessage-exporter (-f txt) archive
//	msgbrowse serve            run the local HTMX web UI
//	msgbrowse mcp              run the Model Context Protocol server
//	msgbrowse watch            re-ingest automatically when an archive changes
//	msgbrowse journal          (re)build the day-by-day journal + LLM digests
//
// Every imported archive is treated as strictly read-only. See README.md and
// ARCHITECTURE.md for the full design; SECURITY.md for the egress model.
package main

import (
	"os"

	"github.com/joestump/msgbrowse/internal/cli"
)

func main() {
	// Execute renders help and any error through fang (styled with the Slate
	// palette); main only sets the exit status so the failure isn't printed
	// twice.
	if err := cli.Execute(); err != nil {
		os.Exit(1)
	}
}
