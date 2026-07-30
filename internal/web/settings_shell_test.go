package web

import (
	"strings"
	"testing"
)

// The issue-#163 acceptance, extended by #175 with the Providers tab, by #191
// with the LLM tab, by #2 with the Backups tab, by #12 with the Contacts tab,
// and reordered in #223 (Providers first, "Settings" renamed "MCP"):
// Providers, MCP, LLM, Contacts, Status, Backups, and Logs render as one shell
// with sub-navigation — each page carries the shared h1 + the boosted sub-nav
// with its own tab active — while the old routes stay canonical, working URLs.

func TestSettingsShellSubNav(t *testing.T) {
	srv, _, _ := newTestServer(t)
	cases := []struct {
		route     string
		activeTab string
	}{
		{"/providers", `href="/providers" class="settings-tab settings-tab-active"`},
		{"/settings/mcp", `href="/settings/mcp" class="settings-tab settings-tab-active"`},
		{"/settings/llm", `href="/settings/llm" class="settings-tab settings-tab-active"`},
		{"/settings/contacts", `href="/settings/contacts" class="settings-tab settings-tab-active"`},
		{"/status", `href="/status" class="settings-tab settings-tab-active"`},
		{"/backups", `href="/backups" class="settings-tab settings-tab-active"`},
		{"/logs", `href="/logs" class="settings-tab settings-tab-active"`},
	}
	for _, c := range cases {
		t.Run(c.route, func(t *testing.T) {
			rec := get(t, srv, c.route)
			if rec.Code != 200 {
				t.Fatalf("status = %d", rec.Code)
			}
			body := rec.Body.String()
			if !contains(body, "settings-subnav") {
				t.Fatal("page missing the settings sub-nav")
			}
			if !contains(body, c.activeTab) {
				t.Errorf("page missing its active tab marker %q", c.activeTab)
			}
			if !contains(body, `aria-current="page"`) {
				t.Errorf("active tab missing aria-current")
			}
			// All seven sections stay reachable from every tab.
			for _, href := range []string{`href="/providers"`, `href="/settings/mcp"`, `href="/settings/llm"`, `href="/settings/contacts"`, `href="/status"`, `href="/backups"`, `href="/logs"`} {
				if !contains(body, href) {
					t.Errorf("sub-nav missing %s", href)
				}
			}
			// Exactly the seven tabs (#223 order: Providers, MCP, LLM, Contacts,
			// Status, Backups, Logs).
			if n := strings.Count(body, `class="settings-tab`); n != 7 {
				t.Errorf("sub-nav has %d tabs, want 7", n)
			}
			// Tab ordering (#223): Providers → MCP → LLM → Contacts → Status →
			// Backups → Logs.
			providersAt := strings.Index(body, `href="/providers"`)
			mcpAt := strings.Index(body, `href="/settings/mcp"`)
			llmAt := strings.Index(body, `href="/settings/llm"`)
			contactsAt := strings.Index(body, `href="/settings/contacts"`)
			statusAt := strings.Index(body, `href="/status"`)
			backupsAt := strings.Index(body, `href="/backups"`)
			logsAt := strings.Index(body, `href="/logs"`)
			if mcpAt < providersAt {
				t.Error("MCP tab should follow Providers (#223)")
			}
			if llmAt < mcpAt {
				t.Error("LLM tab should follow MCP (#223)")
			}
			if contactsAt < llmAt {
				t.Error("Contacts tab should follow LLM (#223)")
			}
			if statusAt < contactsAt {
				t.Error("Status tab should follow Contacts (#223)")
			}
			if backupsAt < statusAt {
				t.Error("Backups tab should follow Status (#223)")
			}
			if logsAt < backupsAt {
				t.Error("Logs tab should be last, after Backups (#223)")
			}
			// Exactly one h1 per page (accessibility: single h1).
			if n := strings.Count(body, "<h1"); n != 1 {
				t.Errorf("page has %d h1 elements, want 1", n)
			}
		})
	}
}

// TestSettingsRedirect: the old /settings URL redirects to /providers (#223),
// following the /setup → /providers precedent.
func TestSettingsRedirect(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := get(t, srv, "/settings")
	if rec.Code != 301 {
		t.Fatalf("/settings status = %d, want 301 redirect to /providers", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc != "/providers" {
		t.Errorf("/settings redirected to %q, want /providers", loc)
	}
}

// TestBuiltCSSCarriesSettingsShell guards the ADR-0012 drift rule for the new
// sub-nav + Providers polish classes: the committed app.css must carry them
// (rebuild: rm -rf .tools && make css).
func TestBuiltCSSCarriesSettingsShell(t *testing.T) {
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	out := string(css)
	for _, want := range []string{
		".settings-subnav",
		".settings-tab",
		".settings-tab-active",
		".setup-iconbtn",
		".setup-btn-danger",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("built app.css missing %q (rebuild: rm -rf .tools && make css)", want)
		}
	}
}
