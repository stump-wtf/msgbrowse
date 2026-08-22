// HUD Nesting Guard
//
// Home's HUD (Conversations / Messages / Newest message) is the reference
// shape every stat tile in the app should use. #395 killed the double
// chrome that showed up when a hud was nested inside a bordered surface — a
// hud drawing its own border/background/radius/padding a SECOND time inside
// e.g. Status's "Archive freshness" card, since #377 folded both onto the
// exact same .surface primitive — by adding one CSS rule keyed off the
// surface family rather than a per-call-site modifier class.
//
// This test fails if that rule is deleted, weakened, or narrowed to no
// longer cover a real nesting site (.status-card) — regressions that would
// silently regrow the double border for a future caller with no template
// change to review.
//
// @joestump-agent 08/22/2026 - Added with the hud component work (#395).
package web

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/store"
)

// readBuiltCSS reads the go:embed-served, committed CSS artifact — mirrors
// TestBuiltCSSCarriesTheScale's inline read in spacing_test.go.
func readBuiltCSS(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	return string(b)
}

// nestedHUDRule matches the "kill nested hud chrome" selector regardless of
// how the surface-family list inside :is(...) is ordered or wrapped, as long
// as it still covers .status-card — a real nesting site (Status's "Archive
// freshness" card) — and still targets .hud as the descendant.
var nestedHUDRule = regexp.MustCompile(`(?s):is\([^)]*\.status-card[^)]*\)\s*\.hud\s*\{([^}]*)\}`)

// TestNestedHUDChromeIsStripped asserts the source stylesheet, not just the
// built artifact — this is where a regression gets written, and
// TestBuiltCSSCarriesTheScale-style coverage below catches a stale rebuild.
func TestNestedHUDChromeIsStripped(t *testing.T) {
	css := readSourceCSS(t)
	m := nestedHUDRule.FindStringSubmatch(css)
	if m == nil {
		t.Fatal("no CSS rule strips a nested hud's chrome when placed inside a surface-family " +
			"container (.status-card among others) — a hud nested inside .status-card (Status's " +
			"\"Archive freshness\" card) will draw its own border/background/radius a second time, " +
			"exactly the bug #395 fixed")
	}
	block := m[1]
	for _, want := range []string{"border: 0", "background: none", "border-radius: 0", "padding: 0"} {
		if !strings.Contains(block, want) {
			t.Errorf("nested-hud rule does not set %q — the inner chrome will still render", want)
		}
	}
}

// TestBuiltCSSStripsNestedHUDChrome mirrors TestBuiltCSSCarriesTheScale: the
// built, go:embed-served app.css must carry the same override, minified or
// not, so a source-only fix that was never rebuilt does not ship stale.
func TestBuiltCSSStripsNestedHUDChrome(t *testing.T) {
	built := readBuiltCSS(t)
	if !nestedHUDRule.MatchString(built) {
		t.Error("built app.css carries no nested-hud-chrome override for .status-card — run `make css` and commit the result")
	}
}

// The Three-Cell Contract
//
// .hud is a fixed three-column grid (`grid-template-columns: repeat(3,
// minmax(0, 1fr))`, input.css), but hudData.Cells is an ordered slice of
// arbitrary length and the "hud" define ranges over it. Nothing connects the
// two, so a future caller that builds a four-cell hud gets a fourth tile
// wrapped onto a second row — carrying the `.stat-cell + .stat-cell` left
// hairline rule at the start of that row — and a two-cell hud gets a third column
// of dead space inside the border. Both render without erroring.
//
// This pins the contract at the seam where it is actually decidable: every
// builder must emit exactly as many cells as the grid has columns, and the
// column count is read from the stylesheet rather than hardcoded, so
// widening the grid and widening the builders stay one change.
//
// @joestump-agent 08/22/2026 - Added in review of #398.

// hudColumns reads the .hud grid's column count out of the source stylesheet.
var hudGridRule = regexp.MustCompile(`\.hud\s*\{[^}]*grid-template-columns:\s*repeat\((\d+),`)

func hudColumns(t *testing.T) int {
	t.Helper()
	m := hudGridRule.FindStringSubmatch(readSourceCSS(t))
	if m == nil {
		t.Fatal(".hud declares no repeat(N, …) grid-template-columns — the " +
			"cell-count contract below cannot be checked against the grid")
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("unparseable .hud column count %q: %v", m[1], err)
	}
	return n
}

// TestHUDBuildersMatchTheGrid fails when a builder emits a cell count the
// .hud grid cannot lay out in one row.
func TestHUDBuildersMatchTheGrid(t *testing.T) {
	cols := hudColumns(t)
	for _, tc := range []struct {
		name string
		data hudData
	}{
		{"archiveHUD", archiveHUD(12, 3400, "2026-08-22", "mb-4")},
		{"contactVolumeHUD", contactVolumeHUD(store.ContactStats{
			TotalMessages: 100, SentMessages: 60, ReceivedMessages: 40,
		})},
		{"contactPaceHUD", contactPaceHUD(7, 12, "11 PM")},
	} {
		if got := len(tc.data.Cells); got != cols {
			t.Errorf("%s emits %d cells but .hud is a %d-column grid — the "+
				"extra cells wrap onto a second row (carrying the stat-cell "+
				"hairline rule) or leave dead columns inside the border; widen "+
				"grid in input.css and this test together", tc.name, got, cols)
		}
	}
}
