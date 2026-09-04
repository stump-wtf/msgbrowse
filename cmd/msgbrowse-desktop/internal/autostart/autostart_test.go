package autostart

// Headless tests for the launch-at-login registration (issue #430). Both
// platform renderers and the enable/disable/enable roundtrip run on Linux —
// the plist and desktop-entry bodies are pure template output, and the
// manager takes explicit paths via NewForPaths.
//
// @joestump-agent 09/04/2026 - Added with #430.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderPlistCarriesHiddenBoot(t *testing.T) {
	body, err := RenderPlist("/Applications/msgbrowse.app/Contents/MacOS/msgbrowse")
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		"rocks.stump.msgbrowse.desktop",
		"<string>/Applications/msgbrowse.app/Contents/MacOS/msgbrowse</string>",
		"<string>--hidden</string>",
		"<key>RunAtLoad</key>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist missing %q:\n%s", want, s)
		}
	}
}

func TestRenderDesktopEntryCarriesHiddenBoot(t *testing.T) {
	body, err := RenderDesktopEntry("/usr/local/bin/msgbrowse")
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		"[Desktop Entry]",
		"Exec=/usr/local/bin/msgbrowse --hidden",
		"Type=Application",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("desktop entry missing %q:\n%s", want, s)
		}
	}
}

func TestEnableDisableRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := NewForPaths(filepath.Join(dir, "autostart", "msgbrowse.desktop"))
	if m.Enabled() {
		t.Fatal("must start disabled")
	}
	if err := m.Enable(); err != nil {
		t.Fatal(err)
	}
	if !m.Enabled() {
		t.Fatal("enabled state not recorded")
	}
	body, err := os.ReadFile(m.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "--hidden") {
		t.Fatalf("registration must boot hidden:\n%s", body)
	}
	if err := m.Disable(); err != nil {
		t.Fatal(err)
	}
	if m.Enabled() {
		t.Fatal("disable did not remove registration")
	}
	// Disabling twice is success (a missing file is already disabled).
	if err := m.Disable(); err != nil {
		t.Fatalf("second disable: %v", err)
	}
}

func TestLaunchAgentPathAndXDGPatAreStable(t *testing.T) {
	if got := launchAgentPath("/Users/joe"); got != "/Users/joe/Library/LaunchAgents/rocks.stump.msgbrowse.desktop.plist" {
		t.Fatalf("launch agent path: %q", got)
	}
	if got := xdgAutostartPath("/home/joe/.config"); got != "/home/joe/.config/autostart/msgbrowse.desktop" {
		t.Fatalf("xdg path: %q", got)
	}
}
