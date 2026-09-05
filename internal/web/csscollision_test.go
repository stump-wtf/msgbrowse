// daisyUI Class-Collision Guard
//
// The 2026-09-05 polish audit (F1) found Home collapsed into one overlapping
// box in every release since v0.8.23: our `.stack` vertical-rhythm helper was
// silently overridden by daisyUI's `.stack` component (inline-grid, all
// children in one grid area at descending opacity), because daisyUI was loaded
// whole and every component class shipped in app.css. The helper is now
// `.vstack` and the daisyUI plugin is restricted to the components we use
// (drawer, loading, link, plus its base items).
//
// These tests keep the collision from returning. They assert, from both ends:
//
//   - templates must not use a bare daisyUI component class outside the
//     allowlist (if a new one is genuinely needed, add it to the include list
//     in tailwind/input.css AND here, and prove no own-class collides);
//   - the built app.css must not ship rules for the collision-prone
//     components at all, so a future whole-daisyUI load fails the build.
//
// @joestump-agent 09/05/2026 - Added with the F1 fix (audit bcJFpa3t).
package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// daisyUIAllowedClasses are the daisyUI component classes templates may use.
// They must match the include list in tailwind/input.css.
var daisyUIAllowedClasses = map[string]bool{
	"drawer":           true, // app shell (partials.html)
	"drawer-toggle":    true,
	"drawer-content":   true,
	"drawer-side":      true,
	"drawer-overlay":   true,
	"md:drawer-open":   true,
	"loading":          true, // inline spinners (gallery, partials)
	"loading-dots":     true,
	"loading-md":       true,
	"link":             true, // bare link styling (contact.html)
	"link-hover":       true,
	"badge":            true, // fact-count chip (contact.html)
	"badge-sm":         true,
	"badge-neutral":    true,
	"theme-controller": true,
}

// daisyUIComponentClasses are class names daisyUI defines as components that
// we deliberately do NOT use. Any of these appearing as a selector in the
// built app.css means daisyUI was loaded whole again — or someone reintroduced
// the component — and any template token matching one of these names is a
// collision waiting to happen against our own selectors.
var daisyUIComponentClasses = []string{
	"alert", "avatar", "badge", "breadcrumbs", "btn", "button", "card",
	"carousel", "chat", "checkbox", "collapse", "countdown", "diff",
	"divider", "dropdown", "file-input", "footer", "hero", "indicator",
	"input", "join", "kbd", "label", "link", "list", "loading", "mask",
	"menu", "modal", "mockup-code", "mockup-window", "navbar", "pagination",
	"progress", "radial-progress", "radio", "range", "rating", "select",
	"skeleton", "stack", "stat", "status", "steps", "swap", "table",
	"table-zebra", "tabs", "textarea", "timeline", "toast", "toggle",
	"tooltip", "validator", "video",
}

var classAttrRe = regexp.MustCompile(`class="([^"]*)"`)

func TestTemplatesUseNoDisallowedDaisyUIClasses(t *testing.T) {
	tplDir := "templates"
	entries, err := os.ReadDir(tplDir)
	if err != nil {
		t.Fatalf("reading templates dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(tplDir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		for _, m := range classAttrRe.FindAllStringSubmatch(string(raw), -1) {
			for _, token := range strings.Fields(m[1]) {
				if daisyUIAllowedClasses[token] {
					continue
				}
				for _, comp := range daisyUIComponentClasses {
					if token == comp {
						t.Errorf("%s: class %q is a daisyUI component class not in the allowlist; either drop it or add the component to the include list in tailwind/input.css and to daisyUIAllowedClasses", e.Name(), token)
					}
				}
			}
		}
	}
}

func TestBuiltCSSShipsNoCollisionProneDaisyUIComponents(t *testing.T) {
	raw, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("reading built app.css: %v", err)
	}
	css := string(raw)
	for _, comp := range daisyUIComponentClasses {
		switch comp {
		case "drawer", "loading", "link", "badge": // on the include list; allowed to ship
			continue
		case "collapse", "table":
			// Tailwind core utilities share these names (visibility:collapse,
			// display:table), so a match here is not evidence of daisyUI.
			continue
		}
		// Match the component as a standalone selector (`.stack`, `.stack,`,
		// `.stack:hover`, `.stack>`) but not our longer compound names
		// (`.stack-gap-5`, `.link-card`, `.btn-search`).
		re := regexp.MustCompile(`\.` + regexp.QuoteMeta(comp) + `([ ,:{.>\[]|$)`)
		if re.MatchString(css) {
			t.Errorf("built app.css defines .%s (daisyUI component we do not include); daisyUI was probably loaded whole again — see tailwind/input.css", comp)
		}
	}
}

func TestHomeVStackHelperPresent(t *testing.T) {
	raw, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("reading built app.css: %v", err)
	}
	if !strings.Contains(string(raw), ".vstack>*+*{margin-top:var(--stack-gap)}") {
		t.Error("built app.css is missing the .vstack vertical-rhythm rule; run `make css` after editing tailwind/input.css")
	}
}
