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
//
// @joestump-agent 08/22/2026 - Extended for #394 (the rest of #372's AC): a
// --control-pad-x/--control-pad-y axis for the one padding pattern that
// repeated identically across unrelated pill/badge classes, plus guards for
// .link-card's radius and the handful of surface-shaped selectors (the
// lightbox and setup-guide overlays, the video modal panel) that moved onto
// the existing scale in this pass. ADR-0012 records why the other ~47
// remaining literals — chrome, table cells, list rows, shape-defining
// padding, list indents — stay exempt rather than forced onto either axis.
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
	".hud",
	".journal-day-card",
	".journal-day",
	".journal-cal",
	".result-card",
	".media-list-card",
	".setup-guide-panel",
}

// controlClasses are the pill/badge selectors governed by the control axis
// (#394) — they carried the identical hand-rolled `1px 0.5rem` independently
// before this pass. A raw padding literal inside one of these blocks is the
// regression the axis exists to prevent.
var controlClasses = []string{
	".source-pill",
	".id-chip",
	".tier-pill",
	".setup-badge",
	".sync-badge",
}

// tokenizedPaddings are non-surface, non-control selectors that #394 moved
// onto the existing --space-* scale (overlay gutters and the video modal
// panel). Unlike surfaceClasses/controlClasses these are not a governed
// family with a shared primitive — each is checked individually for the
// exact declaration it should carry, so a literal creeping back in fails
// loudly instead of silently passing a stylesheet-wide search.
var tokenizedPaddings = []struct {
	selector string
	want     string
}{
	{".lightbox:target", "padding: var(--space-6)"},
	{".setup-guide", "padding: var(--space-5)"},
	{".video-modal-header", "padding: var(--space-3) var(--space-4)"},
	{".video-modal-body", "padding: var(--space-4)"},
}

// rawRemPadding matches a padding declaration with a literal rem value —
// exactly what the scale replaces. Token references (var(--surface-pad)) do not
// match.
var rawRemPadding = regexp.MustCompile(`padding:\s*[0-9.]+rem`)

// rawPadding is the broader form used for the control axis: it matches ANY
// literal padding value (rem or the `1px` these pills used), not just rem,
// since the pattern being guarded against is `1px 0.5rem`.
var rawPadding = regexp.MustCompile(`padding:\s*[0-9]`)

// rawRemRadius matches a literal rem border-radius — the .link-card
// regression this guards against (#394: its 0.75rem sat off the scale
// entirely).
var rawRemRadius = regexp.MustCompile(`border-radius:\s*[0-9.]+rem`)

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
		"--space-1:", "--space-2:", "--space-3:", "--space-4:", "--space-5:", "--space-6:", "--space-7:",
		"--surface-pad:", "--surface-pad-dense:", "--surface-pad-roomy:",
		"--surface-radius:", "--surface-radius-dense:", "--surface-radius-roomy:",
		"--stack-gap:", "--section-gap:",
		"--control-pad-y:", "--control-pad-x:",
	} {
		if !strings.Contains(css, token) {
			t.Errorf("spacing scale missing %s", token)
		}
	}
	// The primitive must actually apply the token, not merely declare it.
	block, ok := classBlock(t, css, ".surface,\n.notice-card,\n.home-card,\n.status-card,\n.setup-card,\n.hud,\n.journal-day-card,\n.journal-day,\n.journal-cal,\n.result-card,\n.media-list-card,\n.setup-guide-panel")
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

// TestControlClassesCarryNoRawPadding: every governed pill/badge takes its
// padding from the control axis, not a hand-rolled repeat of `1px 0.5rem`
// (#394). Mirrors TestSurfacesCarryNoRawPadding but for the control family.
func TestControlClassesCarryNoRawPadding(t *testing.T) {
	css := readSourceCSS(t)
	for _, sel := range controlClasses {
		block, ok := classBlock(t, css, sel)
		if !ok {
			t.Errorf("%s: rule not found — if it was renamed, update controlClasses", sel)
			continue
		}
		if m := rawPadding.FindString(block); m != "" {
			t.Errorf("%s carries a hand-rolled %q — take it from the control axis "+
				"(--control-pad-y / --control-pad-x) rather than repeating the literal "+
				"another pill/badge already carries.", sel, m)
		}
		if !strings.Contains(block, "padding: var(--control-pad-y) var(--control-pad-x)") {
			t.Errorf("%s does not apply the control axis tokens", sel)
		}
	}
}

// TestLinkCardRadiusOnScale: .link-card's radius was the "ninth value" #394
// flagged — 0.75rem, sitting off the scale between the dense and base tiers.
// It now resolves onto --surface-radius-dense; the padding stays a deliberate
// literal (ADR-0012) because it is asymmetric tile shape, not card rhythm, so
// this test checks only the radius, not the padding.
func TestLinkCardRadiusOnScale(t *testing.T) {
	css := readSourceCSS(t)
	block, ok := classBlock(t, css, ".link-card")
	if !ok {
		t.Fatal(".link-card: rule not found — if it was renamed, update this test")
	}
	if !strings.Contains(block, "border-radius: var(--surface-radius-dense)") {
		t.Error(".link-card does not apply var(--surface-radius-dense) — the #394 fix regressed")
	}
	if m := rawRemRadius.FindString(block); m != "" {
		t.Errorf(".link-card border-radius regrew a hand-rolled %q — #394 resolved this "+
			"onto --surface-radius-dense; widen the scale rather than hand-rolling a new radius", m)
	}
}

// TestTokenizedPaddingsStayTokenized: the overlay/modal selectors #394 moved
// onto the existing --space-* scale keep the exact tokenized declaration
// rather than drifting back to a literal.
func TestTokenizedPaddingsStayTokenized(t *testing.T) {
	css := readSourceCSS(t)
	for _, tp := range tokenizedPaddings {
		block, ok := classBlock(t, css, tp.selector)
		if !ok {
			t.Errorf("%s: rule not found — if it was renamed, update tokenizedPaddings", tp.selector)
			continue
		}
		if !strings.Contains(block, tp.want) {
			t.Errorf("%s: expected %q — a literal padding may have crept back in (#394)", tp.selector, tp.want)
		}
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
	for _, token := range []string{"--surface-pad", "--space-4", "--surface-radius", "--control-pad-x", "--control-pad-y"} {
		if !strings.Contains(built, token) {
			t.Errorf("built app.css is missing %s — run `make css` and commit the result", token)
		}
	}
	// The specific hand-rolled values the scale replaced must be gone from the
	// artifact's card rules. These three stacked on Home and are the reported
	// defect; if they are back, the rebuild picked up an un-migrated source.
	//
	// "1px 0.5rem" (#394) is the control-axis regression: five unrelated
	// pill/badge classes each hand-rolled this exact pair independently before
	// the control axis existed. "0.75rem" (as .link-card's border-radius) is
	// the off-scale radius #394 also fixed.
	for _, gone := range []string{"1.15rem 1.25rem", "1.1rem 1.2rem", "1.3rem 1.4rem", "1px 0.5rem"} {
		if strings.Contains(built, gone) {
			t.Errorf("built app.css still contains the pre-scale padding %q", gone)
		}
	}
}
