// Package autostart manages launch-at-login registration for the desktop
// shell (issue #430). Both supported platforms use a declarative file the
// session manager reads at login, so the whole thing is pure Go — no cgo, no
// SMAppService — and every path/renderer is headlessly testable on Linux:
//
//   - macOS: a LaunchAgent plist at ~/Library/LaunchAgents/
//     rocks.stump.msgbrowse.desktop.plist (RunAtLoad). Chosen over
//     SMAppService because the plist needs no new cgo beyond the Wails
//     bindings and works on every macOS the Wails v2 build targets;
//   - Linux: an XDG autostart entry at $XDG_CONFIG_HOME/autostart/
//     msgbrowse.desktop.
//
// Both registrations boot the app with --hidden (menubar-only, no window at
// login — SPEC-0010 "Menubar residency"), and both are written only when the
// user turns the toggle ON: the default is off. Disable removes the file.
//
// Governing: SPEC-0010 REQ "Native shell affordances" ("Open-at-login
// registration MAY be provided").
//
// @joestump-agent 09/04/2026 - Added with #430: LaunchAgent + XDG autostart
// registration behind the Settings toggle.
package autostart

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"text/template"
)

// launchAgentPath is the macOS LaunchAgent plist location for the given home
// directory. The label is reverse-DNS (rocks.stump.msgbrowse.desktop) so it
// never collides with the CLI binary's potential future agent.
func launchAgentPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", "rocks.stump.msgbrowse.desktop.plist")
}

// xdgAutostartPath is the Linux XDG autostart entry location for the given
// XDG_CONFIG_HOME (already defaulted to ~/.config by the caller).
func xdgAutostartPath(configHome string) string {
	return filepath.Join(configHome, "autostart", "msgbrowse.desktop")
}

// plistTmpl renders the LaunchAgent. ProgramArguments carries the resolved
// executable path plus --hidden so a login boot stays menubar-only.
var plistTmpl = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>rocks.stump.msgbrowse.desktop</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.}}</string>
		<string>--hidden</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
</dict>
</plist>
`))

// desktopEntryTmpl renders the XDG autostart entry.
var desktopEntryTmpl = template.Must(template.New("desktop").Parse(`[Desktop Entry]
Type=Application
Name=msgbrowse
Comment=Signal, iMessage, and WhatsApp archive browser
Exec={{.}} --hidden
X-GNOME-Autostart-enabled=true
`))

// Manager registers and unregisters the login item for one platform. The
// zero value is not usable; New returns nil when the runtime platform is not
// supported, which the shell treats as "hide the toggle".
type Manager struct {
	// path is the registration file this manager owns.
	path string
	// dir is the parent directory, created on Enable.
	dir string
}

// New returns a Manager for the runtime platform, or nil when unsupported.
func New() *Manager {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	switch runtime.GOOS {
	case "darwin":
		p := launchAgentPath(home)
		return &Manager{path: p, dir: filepath.Dir(p)}
	case "linux":
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" {
			configHome = filepath.Join(home, ".config")
		}
		p := xdgAutostartPath(configHome)
		return &Manager{path: p, dir: filepath.Dir(p)}
	default:
		return nil
	}
}

// NewForPaths builds a Manager over explicit paths — the seam the headless
// tests use; production callers use New.
func NewForPaths(path string) *Manager {
	return &Manager{path: path, dir: filepath.Dir(path)}
}

// Supported reports whether the runtime platform has an autostart mechanism.
func Supported() bool { return New() != nil }

// render produces the registration file body for the runtime platform.
func (m *Manager) render() ([]byte, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return RenderPlist(exe)
	case "linux":
		return RenderDesktopEntry(exe)
	default:
		return nil, errors.New("autostart: unsupported platform")
	}
}

// RenderPlist renders the macOS LaunchAgent body for the given executable
// path. Exported (with RenderDesktopEntry) so both renderers stay headlessly
// testable on Linux, where only the desktop entry runs in production.
func RenderPlist(exe string) ([]byte, error) {
	return renderTemplate(plistTmpl, exe)
}

// RenderDesktopEntry renders the Linux XDG autostart body for the given
// executable path.
func RenderDesktopEntry(exe string) ([]byte, error) {
	return renderTemplate(desktopEntryTmpl, exe)
}

func renderTemplate(t *template.Template, exe string) ([]byte, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, exe); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Enable writes the registration file (and its parent directory), pointing
// at the running executable. Overwrites any previous registration.
func (m *Manager) Enable() error {
	body, err := m.render()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return fmt.Errorf("autostart: create %s: %w", m.dir, err)
	}
	if err := os.WriteFile(m.path, body, 0o644); err != nil {
		return fmt.Errorf("autostart: write %s: %w", m.path, err)
	}
	return nil
}

// Disable removes the registration file. A missing file is success.
func (m *Manager) Disable() error {
	err := os.Remove(m.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("autostart: remove %s: %w", m.path, err)
	}
	return nil
}

// Enabled reports whether the registration file exists. It does NOT verify
// contents — the toggle reflects presence, the same thing launchd/XDG act on.
func (m *Manager) Enabled() bool {
	_, err := os.Stat(m.path)
	return err == nil
}
