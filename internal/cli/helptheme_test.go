package cli

import (
	"fmt"
	"image/color"
	"math"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"charm.land/fang/v2"
	"charm.land/lipgloss/v2"
)

// themeSheetPath points at the web UI's daisyUI theme source. The CLI scheme
// in helptheme.go copies its hexes by hand; this test is the guard that keeps
// the copy honest (#330).
const themeSheetPath = "../../internal/web/tailwind/input.css"

// parseThemeSheet extracts the two daisyUI custom-theme blocks the CLI scheme
// mirrors (slate, slate-light) from the Tailwind source and returns, per
// theme, a map of token name → hex.
func parseThemeSheet(t *testing.T) map[string]map[string]string {
	t.Helper()
	raw, err := os.ReadFile(themeSheetPath)
	if err != nil {
		t.Fatalf("reading theme sheet %s: %v", themeSheetPath, err)
	}
	themes := parseThemeBlocks(string(raw))
	for _, want := range []string{"slate", "slate-light"} {
		if len(themes[want]) == 0 {
			t.Fatalf("no %q theme block found in %s (found themes: %v)", want, themeSheetPath, themeNames(themes))
		}
	}
	return themes
}

var (
	themeBlockRe = regexp.MustCompile(`(?s)@plugin "[^"]*theme/index\.js" \{(.*?)\}`)
	themeNameRe  = regexp.MustCompile(`name:\s*"([a-z0-9-]+)"`)
	themeTokenRe = regexp.MustCompile(`--color-([a-z0-9-]+):\s*(#[0-9a-fA-F]{6})\b`)
)

// parseThemeBlocks returns every named daisyUI theme block in a Tailwind
// sheet, keyed by theme name.
//
// Blocks it does not recognize are collected, not rejected. The guard used to
// t.Fatalf on any block whose name was not slate or slate-light, which meant
// adding a perfectly legitimate third theme to the web UI reddened the CLI
// test suite — a check on the CLI colour scheme could be broken by a change
// with nothing to do with the CLI (#333, M5). The drift assertions below still
// demand slate and slate-light specifically, so a *missing* mirror still fails.
func parseThemeBlocks(raw string) map[string]map[string]string {
	themes := make(map[string]map[string]string)
	for _, block := range themeBlockRe.FindAllStringSubmatch(raw, -1) {
		name := themeNameRe.FindStringSubmatch(block[1])
		if name == nil {
			continue
		}
		tokens := make(map[string]string)
		for _, tok := range themeTokenRe.FindAllStringSubmatch(block[1], -1) {
			tokens[tok[1]] = strings.ToLower(tok[2])
		}
		themes[name[1]] = tokens
	}
	return themes
}

// TestParseThemeBlocksIgnoresUnknownThemes pins M5: a third daisyUI theme in
// input.css must be parsed past, not fatal, and must not disturb the two the
// CLI actually mirrors.
func TestParseThemeBlocksIgnoresUnknownThemes(t *testing.T) {
	const sheet = `
@plugin "../theme/index.js" {
  name: "slate";
  --color-base-100: #0f1216;
}
@plugin "../theme/index.js" {
  name: "solarized";
  color-scheme: light;
  --color-base-100: #fdf6e3;
}
@plugin "../theme/index.js" {
  name: "slate-light";
  --color-base-100: #f7f9fc;
}
`
	themes := parseThemeBlocks(sheet)
	for name, want := range map[string]string{
		"slate":       "#0f1216",
		"slate-light": "#f7f9fc",
		"solarized":   "#fdf6e3",
	} {
		if got := themes[name]["base-100"]; got != want {
			t.Errorf("theme %q: base-100 = %q, want %q", name, got, want)
		}
	}
}

func themeNames(themes map[string]map[string]string) []string {
	names := make([]string, 0, len(themes))
	for name := range themes {
		names = append(names, name)
	}
	return names
}

// hexOf renders a color.Color (our schemes only ever carry lipgloss hex
// colors) back to its #rrggbb string.
func hexOf(t *testing.T, c color.Color) string {
	t.Helper()
	if c == nil {
		t.Fatal("nil color in scheme")
	}
	r, g, b, a := c.RGBA()
	if a != 0xffff {
		t.Fatalf("color %v is not opaque (alpha %#x)", c, a)
	}
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}

// deadRoles are the fang.ColorScheme fields nothing renders: fang's makeStyles
// never reads them, and neither does slateStyles. They are left unset on
// purpose (see helptheme.go), so the drift and contrast tables below must not
// address them — proving a contrast ratio for a colour that never reaches a
// terminal is proving nothing (#333, M4).
var deadRoles = []string{"Help", "Dash", "ErrorDetails"}

// schemeHexes flattens a fang.ColorScheme into role-name → hex, so the drift
// table below can address every role we map (and both halves of ErrorHeader).
// Unset roles are omitted rather than fatal.
func schemeHexes(t *testing.T, cs fang.ColorScheme) map[string]string {
	t.Helper()
	out := make(map[string]string)
	v := reflect.ValueOf(cs)
	pairType := reflect.TypeOf([2]color.Color{})
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		f := v.Field(i)
		if f.Type() == pairType {
			pair := f.Interface().([2]color.Color)
			out[name+" fg"] = hexOf(t, pair[0])
			out[name+" bg"] = hexOf(t, pair[1])
			continue
		}
		c, _ := f.Interface().(color.Color)
		if c == nil {
			continue
		}
		out[name] = hexOf(t, c)
	}
	return out
}

// TestSlateSchemeOmitsDeadRoles pins M4 from the other side: the three roles
// fang never paints must stay unmapped, so nobody re-adds them and quietly
// re-arms a contrast guarantee over colours that do not render.
func TestSlateSchemeOmitsDeadRoles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scheme fang.ColorScheme
	}{
		{"slate (dark)", slateColorScheme(lipgloss.LightDark(true))},
		{"slate-light", slateColorScheme(lipgloss.LightDark(false))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := reflect.ValueOf(tc.scheme)
			for _, role := range deadRoles {
				f := v.FieldByName(role)
				if !f.IsValid() {
					t.Fatalf("fang.ColorScheme no longer has a %s field; re-check which roles it consumes", role)
				}
				if !f.IsNil() {
					t.Errorf("%s: %s is mapped to %v, but fang's makeStyles never reads it", tc.name, role, f.Interface())
				}
			}
		})
	}
}

// roleToken maps each fang.ColorScheme role the CLI paints to the daisyUI
// token it must carry. The special value "dim" refers to the CLI-only derived
// muted hex (see helptheme.go), not a sheet token. Roles fang never reads are
// absent by design — see deadRoles.
var roleToken = map[string]string{
	"Base":           "base-content",
	"Title":          "primary",
	"Description":    "base-content",
	"Codeblock":      "base-200",
	"Program":        "primary",
	"DimmedArgument": "dim",
	"Comment":        "dim",
	"Flag":           "success",
	"FlagDefault":    "dim",
	"QuotedString":   "warning",
	"Argument":       "base-content",
	"ErrorHeader fg": "error-content",
	"ErrorHeader bg": "error",
}

// TestSlateColorSchemeMatchesThemeSheet pins every scheme role that carries a
// web token to that token's current value in input.css. If the web themes are
// restyled, this fails until helptheme.go is re-synced.
func TestSlateColorSchemeMatchesThemeSheet(t *testing.T) {
	themes := parseThemeSheet(t)

	dark := schemeHexes(t, slateColorScheme(lipgloss.LightDark(true)))
	light := schemeHexes(t, slateColorScheme(lipgloss.LightDark(false)))

	perTheme := []struct {
		name  string
		hexes map[string]string
		toks  map[string]string
		dim   string
		// deviations pins the roles that deliberately do NOT carry their token
		// (see helptheme.go); TestSlateCommandRole covers Command.
		deviations map[string]string
	}{
		{"slate (dark)", dark, themes["slate"], slateDark.dim, nil},
		{"slate-light", light, themes["slate-light"], slateLight.dim, map[string]string{
			"Flag":         lightFlag,
			"QuotedString": lightQuotedString,
		}},
	}

	for _, th := range perTheme {
		t.Run(th.name, func(t *testing.T) {
			for role, tok := range roleToken {
				if want, ok := th.deviations[role]; ok {
					if got := th.hexes[role]; got != want {
						t.Errorf("%s: %s = %s, want %s (contrast-driven deviation from --color-%s)",
							th.name, role, got, want, tok)
					}
					continue
				}
				want := th.dim
				if tok != "dim" {
					var ok bool
					want, ok = th.toks[tok]
					if !ok {
						t.Errorf("--color-%s missing from the %s theme block in %s", tok, th.name, themeSheetPath)
						continue
					}
				}
				if got := th.hexes[role]; got != want {
					t.Errorf("%s: %s = %s, want %s (--color-%s)", th.name, role, got, want, tok)
				}
			}
		})
	}
}

// TestSlateCommandRole documents the one deliberate deviation from the token
// sheet: on a light terminal slate-light's secondary (#5b7cc0) misses the
// 4.5:1 normal-text floor, so Command falls back to primary there (and only
// there — the dark theme keeps the secondary accent).
func TestSlateCommandRole(t *testing.T) {
	themes := parseThemeSheet(t)

	dark := slateColorScheme(lipgloss.LightDark(true))
	if got, want := hexOf(t, dark.Command), themes["slate"]["secondary"]; got != want {
		t.Errorf("dark Command = %s, want secondary %s", got, want)
	}
	light := slateColorScheme(lipgloss.LightDark(false))
	if got, want := hexOf(t, light.Command), themes["slate-light"]["primary"]; got != want {
		t.Errorf("light Command = %s, want primary %s (contrast-driven fallback)", got, want)
	}
}

// TestSlateContrast keeps the whole mapping honest, not just the derived dim
// hexes: every foreground role must hold a ≥4.5:1 WCAG contrast ratio against
// both surfaces it renders on — the terminal background (base-100) and the
// usage codeblock fang paints (base-200) — plus the ERROR badge's own pairing.
// This is what forces the light-mode substitutes documented in helptheme.go.
//
// It asserts over the roles that actually render. It used to reflect over
// every field of fang.ColorScheme, including Help, Dash and ErrorDetails,
// which fang consumes nowhere — those are unmapped now and schemeHexes drops
// them (#333, M4).
//
// Note what this does NOT cover: the ERROR message body. fang builds
// styles.ErrorText with no Foreground, so error text takes the terminal's
// default colour and no mapping here governs it. Only the badge does.
func TestSlateContrast(t *testing.T) {
	// Codeblock is a background, not text, and ErrorHeader is checked as a
	// pair below; every other mapped role is foreground text.
	skip := map[string]bool{"Codeblock": true, "ErrorHeader fg": true, "ErrorHeader bg": true}

	perPalette := []struct {
		name   string
		p      slatePalette
		scheme fang.ColorScheme
	}{
		{"slate (dark)", slateDark, slateColorScheme(lipgloss.LightDark(true))},
		{"slate-light", slateLight, slateColorScheme(lipgloss.LightDark(false))},
	}
	for _, pp := range perPalette {
		t.Run(pp.name, func(t *testing.T) {
			hexes := schemeHexes(t, pp.scheme)
			checked := 0
			for role, fg := range hexes {
				if skip[role] {
					continue
				}
				checked++
				for _, bgTok := range []string{"base100", "base200"} {
					bg := pp.p.field(bgTok)
					if ratio := contrastRatio(fg, bg); ratio < 4.5 {
						t.Errorf("%s %s on %s %s: contrast %.2f:1, want ≥4.5:1 — re-derive the hex (see helptheme.go)",
							role, fg, bgTok, bg, ratio)
					}
				}
			}
			// Dropping the dead roles must not hollow this out: every
			// remaining fang.ColorScheme field is a foreground we paint,
			// except Codeblock (a background) and ErrorHeader (a pair,
			// checked on its own below).
			if want := reflect.TypeOf(fang.ColorScheme{}).NumField() - len(deadRoles) - 2; checked != want {
				t.Errorf("contrast checked %d roles, want %d — the mapping and this test have drifted", checked, want)
			}
			if ratio := contrastRatio(hexes["ErrorHeader fg"], hexes["ErrorHeader bg"]); ratio < 4.5 {
				t.Errorf("ErrorHeader %s on %s: contrast %.2f:1, want ≥4.5:1",
					hexes["ErrorHeader fg"], hexes["ErrorHeader bg"], ratio)
			}
		})
	}
}

func (p slatePalette) field(name string) string {
	switch name {
	case "base100":
		return p.base100
	case "base200":
		return p.base200
	case "baseContent":
		return p.baseContent
	case "primary":
		return p.primary
	case "secondary":
		return p.secondary
	case "success":
		return p.success
	case "warning":
		return p.warning
	case "errColor":
		return p.errColor
	case "errContent":
		return p.errContent
	case "dim":
		return p.dim
	}
	return ""
}

// contrastRatio computes the WCAG 2.x contrast ratio between two #rrggbb hexes.
func contrastRatio(a, b string) float64 {
	la, lb := relLuminance(a), relLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func relLuminance(hex string) float64 {
	var r, g, b uint8
	if _, err := fmt.Sscanf(strings.ToLower(hex), "#%02x%02x%02x", &r, &g, &b); err != nil {
		return 0
	}
	return 0.2126*chanLuminance(r) + 0.7152*chanLuminance(g) + 0.0722*chanLuminance(b)
}

func chanLuminance(c uint8) float64 {
	v := float64(c) / 255
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}
