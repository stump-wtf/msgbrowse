// Lightbox Caption Link Guard
//
// The 2026-09-05 polish audit (F39) found the gallery lightbox caption link
// rendered with the theme text color over the black scrim — invisible in the
// dark theme. The caption link now carries an explicit light color.
//
// @joestump-agent 09/05/2026 - Added with the F39 fix (audit bcJFpa3t).
package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestLightboxCaptionLinkReadableOnScrim(t *testing.T) {
	raw, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("reading built app.css: %v", err)
	}
	re := regexp.MustCompile(`\.lightbox-caption a,\.lightbox-caption a:visited\{[^}]*\}`)
	m := re.FindString(string(raw))
	if m == "" {
		t.Fatal("built app.css has no .lightbox-caption a rule; run `make css`")
	}
	if !strings.Contains(m, "color:#fff") {
		t.Errorf(".lightbox-caption a must set an explicit light color for the black scrim, got: %s", m)
	}
}
