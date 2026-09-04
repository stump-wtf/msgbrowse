//go:build desktop

// Desktop wiring for the bundled exporter toolchain. All resolution and
// integrity logic lives in the pure-Go, Linux-tested internal/toolchain
// package; this file is the thin desktop-tagged seam that feeds it the running
// binary's real path via os.Executable() for the startup integrity log.
//
// The LIVE export-path resolution — the seam the Setup Enable flow (#133) calls
// before spawning an exporter — lives in the pure-Go embedded package
// (cmd/msgbrowse-desktop/internal/embedded.bundledResolver), which invokes
// toolchain.ResolveExporters at the export site: in a macOS .app it gets
// verified, bundled absolute paths (never $PATH); in the non-bundled Linux build
// it gets empty overrides so the export orchestration falls back to $PATH exactly
// as the bring-your-own CLI does (ADR-0020: only the .app bundles; the CLI is
// unchanged). Keeping that resolver in the untagged package means the whole
// export-path resolution is covered by the desktop module's CGO_ENABLED=0
// headless suite.
//
// Governing: ADR-0020 (bundled exporter toolchain + guided setup), SPEC-0013
// REQ "Bundled toolchain resolution", REQ "Bundled tool integrity and version
// check".
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/joestump/msgbrowse/cmd/msgbrowse-desktop/internal/toolchain"
)

// logBundledToolchain runs the bundled-tool integrity + version check once at
// startup and logs the outcome, recording each tool's pinned version for the
// About panel (issue #429) and warning (never crashing) on a corrupt bundle.
// It is a
// no-op in the non-bundled build (Locate returns ErrNotBundled), so dev runs
// and the Linux desktop build stay quiet. The check is time-boxed so a wedged
// bundled binary cannot stall launch.
func logBundledToolchain(ctx context.Context, log *slog.Logger, about *aboutState) {
	exe, err := os.Executable()
	if err != nil {
		log.Warn("could not resolve executable for bundled-toolchain check", "error", err)
		return
	}
	r, err := toolchain.Locate(exe)
	if err != nil {
		if errors.Is(err, toolchain.ErrNotBundled) {
			// Non-bundled build: exporters come from $PATH via the CLI path. Nothing
			// to verify, nothing to log at info level.
			log.Debug("desktop toolchain: not a macOS .app bundle; exporters resolve via PATH")
			return
		}
		log.Warn("could not locate bundled toolchain", "error", err)
		return
	}

	vctx, cancel := context.WithTimeout(ctx, bundledVerifyTimeout)
	defer cancel()
	infos, errs := r.Verify(vctx, nil)
	// Feed the verified versions to the About panel (issue #429); the About
	// message omits the tool block until this lands.
	if about != nil {
		about.setTools(infos)
	}
	for _, in := range infos {
		log.Info("bundled tool verified", "tool", in.Name, "version", in.Version, "path", in.Path)
	}
	for _, e := range errs {
		// A per-tool error is surfaced later by Setup as a broken source card; at
		// startup it is a warning, and the app keeps running.
		log.Warn("bundled tool failed integrity check", "tool", e.Name, "path", e.Path, "error", e.Err)
	}
}

// bundledVerifyTimeout bounds the startup integrity check so a hung bundled
// binary (e.g. a broken relocatable Python) reads as a per-tool failure instead
// of blocking the window from opening.
const bundledVerifyTimeout = 10 * time.Second
