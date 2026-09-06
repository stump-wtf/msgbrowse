// Settings Sub-Navigation Overflow Guard
//
// The 2026-09-05 polish audit (F3) found the eleven Settings tabs clip below
// ~1000px window width: .settings-subnav was inline-flex with no wrap or
// scroll, so Status, Backups and Logs became unreachable in a narrow window
// (the desktop app ships an 893px layout). The strip now scrolls instead of
// clipping, matching the .header-tabs precedent.
//
// @joestump-agent 09/05/2026 - Added with the F3 fix (audit bcJFpa3t).
package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestSettingsSubnavScrollsWhenNarrow(t *testing.T) {
	raw, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("reading built app.css: %v", err)
	}
	re := regexp.MustCompile(`\.settings-subnav\{[^}]*\}`)
	m := re.FindAllString(string(raw), -1)
	joined := strings.Join(m, "\n")
	if len(m) == 0 {
		t.Fatal("built app.css has no .settings-subnav rule; run `make css`")
	}
	for _, want := range []string{"max-width:100%", "overflow-x:auto"} {
		if !strings.Contains(joined, want) {
			t.Errorf(".settings-subnav lost %s — the tab strip would clip unreachable tabs again in narrow windows (audit F3)", want)
		}
	}
}
