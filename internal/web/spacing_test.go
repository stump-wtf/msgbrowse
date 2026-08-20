// Spacing Scale Guard
//
// ADR-0012 defined colour tokens but no spacing tokens, so every bordered
// surface picked its own padding by hand: eight card classes with eight
// different values, three of which stacked directly on top of each other on
// Home at 1rem/1.1rem, 1.15rem/1.25rem and 1.1rem/1.2rem. Differences that
// small do not read as hierarchy — they read as a crooked column.
//
// These tests keep a ninth hand-rolled number from creeping back. They assert
// against the SOURCE stylesheet, because that is where a regression would be
// written, plus one assertion against the built artifact so a stale app.css
// cannot silently ship the old values.
//
// @joestump-agent 08/20/2026 - Added with the spacing-scale work (#372).
package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// surfaceClasses are the bordered surfaces governed by the scale. A raw rem
// padding inside one of these blocks is the regression.
var surfaceClasses = []string{
	".notice-card",
	".home-card",
	".status-card",
	".setup-card",
	".stat-strip",
	".journal-day-card",
	".journal-day",
	".journal-cal",
	".result-card",
	".media-list-card",
	".setup-guide-panel",
}

// rawRemPadding matches a padding declaration with a literal rem value —
// exactly what the scale replaces. Token references (var(--surface-pad)) do not
// match.
var rawRemPadding = regexp.MustCompile(`padding:\s*[0-9.]+rem`)

func readSourceCSS(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("tailwind/input.css")
	if err != nil {
		t.Fatalf("read input.css: %v", err)
	}
	return string(b)
}

// classBlock returns the declaration block for a top-of-line class selector,
// so an assertion is anchored to the rule it means to guard rather than
// searched across the whole stylesheet.
func classBlock(t *testing.T, css, selector string) (string, bool) {
	t.Helper()
	needle := "\n" + selector + " {"
	i := strings.Index(css, needle)
	if i < 0 {
		return "", false
	}
	start := i + len(needle)
	end := strings.Index(css[start:], "\n}")
	if end < 0 {
		return "", false
	}
	return css[start : start+end], true
}

// TestSurfacesCarryNoRawPadding: every governed surface takes its padding from
// the scale. The check is anchored per rule — a stylesheet-wide search would
// pass on the pill and badge paddings that legitimately remain.
func TestSurfacesCarryNoRawPadding(t *testing.T) {
	css := readSourceCSS(t)
	for _, sel := range surfaceClasses {
		block, ok := classBlock(t, css, sel)
		if !ok {
			t.Errorf("%s: rule not found — if it was renamed, update surfaceClasses", sel)
			continue
		}
		if m := rawRemPadding.FindString(block); m != "" {
			t.Errorf("%s carries a hand-rolled %q — take it from the spacing scale "+
				"(--surface-pad / --surface-pad-dense / --surface-pad-roomy). If none of "+
				"the tiers fit, the scale is wrong: widen it rather than adding a ninth value.", sel, m)
		}
	}
}

// TestSpacingScaleIsDefined: the tokens exist and are what the surfaces
// reference. Without this, deleting the scale would make the test above pass
// trivially — every rule would carry no padding at all.
func TestSpacingScaleIsDefined(t *testing.T) {
	css := readSourceCSS(t)
	for _, token := range []string{
		"--space-1:", "--space-2:", "--space-3:", "--space-4:", "--space-5:", "--space-6:",
		"--surface-pad:", "--surface-pad-dense:", "--surface-pad-roomy:",
		"--surface-radius:", "--surface-radius-dense:", "--surface-radius-roomy:",
		"--stack-gap:", "--section-gap:",
	} {
		if !strings.Contains(css, token) {
			t.Errorf("spacing scale missing %s", token)
		}
	}
	// The primitive must actually apply the token, not merely declare it.
	block, ok := classBlock(t, css, ".surface,\n.notice-card,\n.home-card,\n.status-card,\n.setup-card,\n.stat-strip,\n.journal-day-card,\n.journal-day,\n.journal-cal,\n.result-card,\n.media-list-card,\n.setup-guide-panel")
	if !ok {
		// Selector list may be reordered by a later edit; fall back to finding
		// the declaration anywhere and assert it exists at least once.
		if !strings.Contains(css, "padding: var(--surface-pad)") {
			t.Error("no rule applies var(--surface-pad) — the primitive is not wired up")
		}
		return
	}
	if !strings.Contains(block, "padding: var(--surface-pad)") {
		t.Error("the surface primitive does not apply var(--surface-pad)")
	}
}

// TestBuiltCSSCarriesTheScale: app.css is committed and go:embed-served, so a
// source-only change that was never rebuilt would ship the old paddings. This
// catches the stale artifact.
func TestBuiltCSSCarriesTheScale(t *testing.T) {
	b, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	built := string(b)
	// Minified, so match on the token names rather than formatted declarations.
	for _, token := range []string{"--surface-pad", "--space-4", "--surface-radius"} {
		if !strings.Contains(built, token) {
			t.Errorf("built app.css is missing %s — run `make css` and commit the result", token)
		}
	}
	// The specific hand-rolled values the scale replaced must be gone from the
	// artifact's card rules. These three stacked on Home and are the reported
	// defect; if they are back, the rebuild picked up an un-migrated source.
	for _, gone := range []string{"1.15rem 1.25rem", "1.1rem 1.2rem", "1.3rem 1.4rem"} {
		if strings.Contains(built, gone) {
			t.Errorf("built app.css still contains the pre-scale padding %q", gone)
		}
	}
}
