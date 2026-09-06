package web

import (
	"net/http"
)

// The launch-at-login seam (issue #430): the desktop shell wires an
// Autostarter over its platform registration (LaunchAgent plist on macOS,
// XDG autostart entry on Linux), and the MCP settings page renders a
// launch-at-login toggle + accepts its POST. Browser mode (`msgbrowse
// serve`) never wires one, so the toggle renders nothing and the POST route
// is never registered — the same shape as the desktop open-url bridge
// (#179), never a dead control.
//
// The toggle boots the app with --hidden (menubar-only), per SPEC-0010
// "Menubar residency"; the default is OFF and lives entirely in the OS
// registration file's presence — no app config field to drift.
//
// @joestump-agent 09/04/2026 - Added with #430.

// Autostarter is the desktop shell's launch-at-login registration. Enabled
// reflects the registration file's presence (what launchd/XDG act on); the
// setters write/remove it.
type Autostarter interface {
	Enabled() bool
	Enable() error
	Disable() error
}

// SetAutostart wires the shell's registration into the settings surface and
// rebuilds the mux so POST /settings/autostart exists only when it can work.
// Must be called before serving (same contract as SetExternalOpener).
func (s *Server) SetAutostart(a Autostarter) {
	s.autostart = a
	s.mux = s.routes()
}

// autostartResultStates is the fixed enum of ?autostart= banner states after
// the POST's redirect. Anything else renders nothing.
var autostartResultStates = map[string]bool{"ok": true, "error": true}

// handleAutostart is POST /settings/autostart: flip launch-at-login to the
// submitted state, then redirect back to the MCP tab (PRG, like the pair
// POST). Gated by the same layered same-origin + per-session-token check as
// every privileged POST — a cross-origin page must not be able to plant a
// login item (SPEC-0013 §Security posture).
func (s *Server) handleAutostart(w http.ResponseWriter, r *http.Request) {
	if s.autostart == nil {
		s.notFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, setupBodyLimit)
	if !s.checkSetupPOST(w, r) {
		return
	}
	var state string
	switch r.PostFormValue("enabled") {
	case "on":
		if err := s.autostart.Enable(); err != nil {
			s.log.Warn("autostart: enable failed", "error", err)
		} else {
			state = "ok"
		}
	case "off":
		if err := s.autostart.Disable(); err != nil {
			s.log.Warn("autostart: disable failed", "error", err)
		} else {
			state = "ok"
		}
	}
	if state == "" {
		state = "error"
	}
	http.Redirect(w, r, "/settings/mcp?autostart="+state, http.StatusSeeOther)
}
