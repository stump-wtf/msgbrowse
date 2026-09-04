//go:build desktop

// Headless tests for the About panel content (issue #429): the message must
// always carry version + build, list verified bundled tools, and omit the
// tool block until the async integrity check lands.
//
// @joestump-agent 09/04/2026 - Added with #429.
package main

import (
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/cmd/msgbrowse-desktop/internal/toolchain"
)

func TestAboutMessageShowsVersionAndBuild(t *testing.T) {
	a := newAboutState("v0.8.3", "adc269e")
	got := a.message()
	if !strings.Contains(got, "Version v0.8.3") || !strings.Contains(got, "Build adc269e") {
		t.Fatalf("message missing version/build:\n%s", got)
	}
	if strings.Contains(got, "\n") && len(strings.Split(got, "\n")) > 2 {
		t.Fatalf("no tools recorded yet, expected two lines:\n%s", got)
	}
}

func TestAboutMessageListsVerifiedTools(t *testing.T) {
	a := newAboutState("v0.8.3", "adc269e")
	a.setTools([]toolchain.ToolInfo{
		{Name: "sigexport", Version: "2.4.1"},
		{Name: "syncthing", Version: "v1.27.2"},
	})
	got := a.message()
	for _, want := range []string{"sigexport 2.4.1", "syncthing v1.27.2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("message missing %q:\n%s", want, got)
		}
	}
}

func TestAboutMessageSkipsUnverifiedTools(t *testing.T) {
	a := newAboutState("v0.8.3", "adc269e")
	a.setTools([]toolchain.ToolInfo{
		{Name: "python"}, // version probe never reported
		{Name: "sigexport", Version: "2.4.1"},
	})
	got := a.message()
	if strings.Contains(got, "python") {
		t.Fatalf("unversioned tool must be omitted:\n%s", got)
	}
	if !strings.Contains(got, "sigexport 2.4.1") {
		t.Fatalf("verified tool missing:\n%s", got)
	}
}

func TestAboutSetToolsReplacesSnapshot(t *testing.T) {
	a := newAboutState("v0.8.3", "adc269e")
	a.setTools([]toolchain.ToolInfo{{Name: "old", Version: "1"}})
	a.setTools([]toolchain.ToolInfo{{Name: "new", Version: "2"}})
	got := a.message()
	if strings.Contains(got, "old 1") || !strings.Contains(got, "new 2") {
		t.Fatalf("expected snapshot replacement, got:\n%s", got)
	}
}
