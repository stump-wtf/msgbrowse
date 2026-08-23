package web

import (
	"strings"
	"testing"
)

// The issue-#163 acceptance, extended by #175 with the Providers tab, by #191
// with the LLM tab, by #2 with the Backups tab, by #12 with the Contacts tab,
// reordered in #223 (Providers first, "Settings" renamed "MCP"), extended
// again by #368 with the Search index and Journal tabs (one per derived-data
// pipeline, SPEC-0004 REQ-0004-010), and by #366 with the Facts tab (the
// contact-fact extraction pipeline): all ten render as one shell with
// sub-navigation — each page carries the shared h1 + the boosted sub-nav with
// its own tab active — while the old routes stay canonical, working URLs.

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
		{"/settings/search-index", `href="/settings/search-index" class="settings-tab settings-tab-active"`},
		{"/settings/journal", `href="/settings/journal" class="settings-tab settings-tab-active"`},
		{"/settings/facts", `href="/settings/facts" class="settings-tab settings-tab-active"`},
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
			// All ten sections stay reachable from every tab.
			for _, href := range []string{`href="/providers"`, `href="/settings/mcp"`, `href="/settings/llm"`, `href="/settings/contacts"`, `href="/settings/search-index"`, `href="/settings/journal"`, `href="/settings/facts"`, `href="/status"`, `href="/backups"`, `href="/logs"`} {
				if !contains(body, href) {
					t.Errorf("sub-nav missing %s", href)
				}
			}
			// Exactly the ten tabs (#223 order, extended by #368 and #366:
			// Providers, MCP, LLM, Contacts, Search index, Journal, AI, Status,
			// Backups, Logs).
			if n := strings.Count(body, `class="settings-tab`); n != 10 {
				t.Errorf("sub-nav has %d tabs, want 10", n)
			}
			// Tab ordering: Providers → MCP → LLM → Contacts → Search index →
			// Journal → Facts → Status → Backups → Logs.
			providersAt := strings.Index(body, `href="/providers"`)
			mcpAt := strings.Index(body, `href="/settings/mcp"`)
			llmAt := strings.Index(body, `href="/settings/llm"`)
			contactsAt := strings.Index(body, `href="/settings/contacts"`)
			searchIndexAt := strings.Index(body, `href="/settings/search-index"`)
			journalAt := strings.Index(body, `href="/settings/journal"`)
			factsAt := strings.Index(body, `href="/settings/facts"`)
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
			if searchIndexAt < contactsAt {
				t.Error("Search index tab should follow Contacts (#368)")
			}
			if journalAt < searchIndexAt {
				t.Error("Journal tab should follow Search index (#368)")
			}
			if factsAt < journalAt {
				t.Error("Facts tab should follow Journal (#366)")
			}
			if statusAt < factsAt {
				t.Error("Status tab should follow Facts (#366)")
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
