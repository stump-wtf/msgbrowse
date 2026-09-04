//go:build desktop

// About panel content for the native macOS About window (issue #429). The
// message is assembled here — pure Go, headlessly tested on Linux — from the
// build-time version/commit and the bundled-tool versions the startup
// integrity check (logBundledToolchain) resolves a moment after launch. The
// platform glue (about_platform_darwin.go) only renders the finished string.
//
// Governing: SPEC-0010 REQ "Native shell affordances" (Cmd+, / app-menu About).
//
// @joestump-agent 09/04/2026 - Added with #429: app-menu About item with the
// Cmd+, accelerator, carrying app version plus resolved tool versions.
package main

import (
	"strings"
	"sync"

	"github.com/joestump/msgbrowse/cmd/msgbrowse-desktop/internal/toolchain"
)

// aboutState assembles the About panel message. Tool versions arrive
// asynchronously (the startup integrity check spawns subprocesses off the
// launch path), so the message is read at click time and simply omits the
// tool block if the check has not finished yet.
type aboutState struct {
	version string
	commit  string

	mu    sync.Mutex
	tools []toolchain.ToolInfo
}

func newAboutState(version, commit string) *aboutState {
	return &aboutState{version: version, commit: commit}
}

// setTools records the verified bundled-tool versions for the About panel.
// Per-tool failures are omitted (they surface on the Logs page); the panel
// is a summary, not a diagnostic surface.
func (a *aboutState) setTools(infos []toolchain.ToolInfo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tools = append([]toolchain.ToolInfo(nil), infos...)
}

// message renders the About panel body: version and build always, then one
// line per verified bundled tool, mirroring what the web Logs page records
// at startup. A dev (non-bundled) build shows just version + build.
func (a *aboutState) message() string {
	a.mu.Lock()
	defer a.mu.Unlock()

	var b strings.Builder
	b.WriteString("Version " + a.version + "\nBuild " + a.commit)
	for _, t := range a.tools {
		if t.Version == "" {
			continue
		}
		b.WriteString("\n" + t.Name + " " + t.Version)
	}
	return b.String()
}
