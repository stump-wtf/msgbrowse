package web

import (
	"io/fs"
	"strings"
	"testing"
)

// TestNoInlineStylesInTemplates enforces the CSP contract: the server sets
// `style-src 'self'` with no 'unsafe-inline', so inline style="" attributes (and
// <style> blocks) are blocked by the browser and silently do nothing. Any such
// attribute is a bug — styling must live in app.css via classes. This guard
// scans the embedded templates so the regression can't reach a browser.
func TestNoInlineStylesInTemplates(t *testing.T) {
	err := fs.WalkDir(templatesFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := templatesFS.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		content := string(b)
		if strings.Contains(content, "style=\"") || strings.Contains(content, "style='") {
			t.Errorf("%s contains an inline style attribute — forbidden by CSP (style-src 'self'); move it to a class in input.css", path)
		}
		if strings.Contains(content, "<style") {
			t.Errorf("%s contains a <style> block — forbidden by CSP; use app.css", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}
}

// TestSettingsTabsShareOneH1 (audit F8, 2026-09-05): every Settings tab's H1
// reads "Settings" — the MCP tab alone said "MCP", so the shell jumped a
// heading when tabbing into and out of it.
func TestSettingsTabsShareOneH1(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, path := range []string{"/providers", "/settings/mcp", "/settings/llm", "/settings/contacts"} {
		body := get(t, srv, path).Body.String()
		if !contains(body, `<h1 class="screen-h1">Settings</h1>`) {
			t.Errorf("%s: settings tab H1 is not the shared \"Settings\" heading", path)
		}
	}
}
