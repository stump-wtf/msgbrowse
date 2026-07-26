package web

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/source"
)

// TestToolbarContextualTitle verifies the unified toolbar's contextual title
// (#152 Option A): "msgbrowse" on home, the active conversation's display name on
// a transcript page, and that the title links home. It also asserts Option A
// dropped the old global counts from the toolbar (they moved to Status under
// Settings → Diagnostics).
func TestToolbarContextualTitle(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	// Home: the toolbar shows the "msgbrowse" wordmark in the contextual title.
	home := get(t, srv, "/").Body.String()
	if !contains(home, "app-toolbar") {
		t.Error("home missing the unified toolbar")
	}
	if !contains(home, `class="toolbar-title" aria-label="msgbrowse home"`) {
		t.Error("home toolbar title should be the msgbrowse-home link")
	}
	// The label lives in its own span (the home glyph precedes it, so only the
	// text ellipsis-truncates); it still reads "msgbrowse" on home.
	if !contains(home, `<span class="toolbar-title-label">msgbrowse</span></a>`) {
		t.Error("home toolbar title should read 'msgbrowse'")
	}
	// The decorative home glyph is present inside the home link.
	if !contains(home, `aria-label="msgbrowse home"`) || !contains(home, "toolbar-title-label") {
		t.Error("home toolbar title should carry the home glyph beside the label")
	}
	// Option A removed the global counts from the toolbar entirely.
	if contains(home, "navbar-counts") || contains(home, " conversations · ") {
		t.Error("Option A should have removed the global counts from the toolbar")
	}

	// Transcript: the toolbar title becomes the conversation's display name.
	conv, err := st.GetConversation(ctx, "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	convPage := get(t, srv, "/c/"+itoa(conv.ID)).Body.String()
	if !contains(convPage, `<span class="toolbar-title-label">`+humanName(conv.Name)+"</span></a>") {
		t.Errorf("transcript toolbar title should read the conversation name %q", humanName(conv.Name))
	}
	// It is still the home link (clickable back to home), not a plain heading.
	if !contains(convPage, `class="toolbar-title" aria-label="msgbrowse home"`) {
		t.Error("transcript toolbar title should still link home")
	}
}

// TestToolbarSearchForm verifies the toolbar's search pill is a boosted GET form
// targeting /search with a labelled input (#152), so Enter runs a search on the
// existing /search page.
func TestToolbarSearchForm(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body := get(t, srv, "/").Body.String()

	if !contains(body, `<form action="/search" method="get" role="search"`) {
		t.Error("toolbar search form should be a GET to /search with role=search")
	}
	if !contains(body, "toolbar-search") {
		t.Error("toolbar missing the search pill")
	}
	// Boosted so Enter swaps the /search page into #main-content (boosted-nav).
	if !contains(body, `class="toolbar-search hidden sm:flex"`) ||
		!contains(body, `hx-boost="true"`) {
		t.Error("toolbar search form should be boosted (hx-boost)")
	}
	// The input is named q, labelled, and carries the placeholder.
	if !contains(body, `name="q"`) ||
		!contains(body, `aria-label="Search messages"`) ||
		!contains(body, `placeholder="Search messages"`) {
		t.Error("toolbar search input should be named q, labelled, and placeheld 'Search messages'")
	}
	// Below sm the pill is hidden and the sidebar Search link is gone (#190),
	// so an icon-only boosted /search link keeps search reachable everywhere.
	if !contains(body, `<a href="/search" class="toolbar-icon-btn toolbar-search-link sm:hidden" aria-label="Search"`) {
		t.Error("header missing the below-sm icon-only /search link")
	}
}

// TestToolbarIconButtonsLabelled verifies every icon-only toolbar control carries
// an aria-label and the header stays role=banner (#152 accessibility).
func TestToolbarIconButtonsLabelled(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body := get(t, srv, "/").Body.String()

	for _, want := range []string{
		`aria-label="Toggle sidebar"`, // mobile drawer toggle
		`aria-label="Toggle theme"`,   // theme toggle
		`aria-label="Settings"`,       // settings gear → /settings
	} {
		if !contains(body, want) {
			t.Errorf("toolbar icon button missing aria-label %q", want)
		}
	}
	// The theme toggle is still the existing data-theme-toggle control.
	if !contains(body, "data-theme-toggle") {
		t.Error("toolbar missing the data-theme-toggle control")
	}
	// The settings gear links to /settings.
	if !contains(body, `href="/settings"`) {
		t.Error("toolbar settings gear should link to /settings")
	}
	// The header keeps banner semantics (daisyUI dropped, but role=banner is the
	// implicit role of <header> not nested in a section/article — which holds here).
	if !contains(body, "<header ") {
		t.Error("toolbar should be a <header> (role=banner)")
	}
}

// TestSidebarPresenceDotAndSource verifies each conversation row renders a
// monogram avatar with a source-colored presence dot (REQ-0006-004), and that
// the filter input and CONVERSATIONS section are present (REQ-0006-003).
func TestSidebarPresenceDotAndSource(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body := get(t, srv, "/").Body.String()

	for _, want := range []string{
		"Filter conversations", // filter input placeholder
		"sidebar-filter",       // filter input shell
		"avatar-mono",          // monogram avatar
		"presence-dot",         // presence dot element
	} {
		if !contains(body, want) {
			t.Errorf("sidebar missing %q", want)
		}
	}
	// The fixture's conversations are Signal, so the presence dot carries the
	// signal source modifier (blue, derived from --color-info).
	if !contains(body, "presence-dot src-signal") {
		t.Errorf("sidebar presence dot missing src-signal source color")
	}
}

// TestSidebarListOnly pins the #190 sidebar reduction: the <aside> carries NO
// nav links at all (the header tabs + search pill own Search/Media now) — just
// the "Filter conversations" input directly above the Pinned/Conversations
// section heads, where sidebar.js binds the input by id.
func TestSidebarListOnly(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body := get(t, srv, "/").Body.String()

	asideStart := strings.Index(body, "<aside")
	asideEnd := strings.Index(body, "</aside>")
	if asideStart < 0 || asideEnd < 0 {
		t.Fatal("home missing the sidebar <aside>")
	}
	aside := body[asideStart:asideEnd]

	if strings.Contains(aside, "<nav") {
		t.Error("sidebar must carry no <nav> — its Search/Media links moved to the header (#190)")
	}
	for _, href := range []string{`href="/search"`, `href="/gallery"`, `href="/media"`} {
		if strings.Contains(aside, href) {
			t.Errorf("sidebar must not carry %s — the header owns that surface (#190)", href)
		}
	}
	filter := strings.Index(aside, `id="sidebar-filter"`)
	sections := strings.Index(aside, "sidebar-section-head")
	if filter < 0 || sections < 0 {
		t.Fatalf("sidebar missing filter/section (filter %d, sections %d)", filter, sections)
	}
	if !(filter < sections) {
		t.Errorf("sidebar order should be filter → sections (filter %d, sections %d)", filter, sections)
	}
}

// TestHeaderFullWidthShell pins the #190 shell skeleton: the header is a
// direct child of <body>, rendered BEFORE (outside) the drawer that holds the
// sidebar and content — full window width, traffic lights always over its
// left edge in the desktop shell — and the conversation header inside the
// content scroller pins at top-0 (the drifting 54px sticky offset, whose
// see-through slit was the owner's item 1, is structurally gone — the token
// is not spelled as the utility class here because Tailwind v4 scans Go
// comments too and would resurrect it into the built app.css).
func TestHeaderFullWidthShell(t *testing.T) {
	srv, st, _ := newTestServer(t)

	body := get(t, srv, "/").Body.String()
	header := strings.Index(body, "<header class=\"app-toolbar")
	drawer := strings.Index(body, `class="app-shell drawer`)
	aside := strings.Index(body, "<aside")
	if header < 0 || drawer < 0 || aside < 0 {
		t.Fatalf("home missing header/drawer/aside (header %d, drawer %d, aside %d)", header, drawer, aside)
	}
	if !(header < drawer && drawer < aside) {
		t.Errorf("the header must precede the drawer (full-width, above the sidebar): header %d, drawer %d, aside %d", header, drawer, aside)
	}

	// #197 review: the header lays out as [left cluster | tabs | right
	// cluster] IN FLOW (a 3-column grid in input.css) — the tab strip is a
	// grid column, not an absolutely-positioned overlay, so it can never
	// paint over the search pill/icon. Pin the flow order here.
	left := strings.Index(body, `<div class="toolbar-cluster toolbar-cluster-left">`)
	tabs := strings.Index(body, `<nav class="header-tabs"`)
	right := strings.Index(body, `<div class="toolbar-cluster toolbar-cluster-right">`)
	if left < 0 || tabs < 0 || right < 0 || !(left < tabs && tabs < right) {
		t.Errorf("header should flow [left cluster | tabs | right cluster] (left %d, tabs %d, right %d)", left, tabs, right)
	}

	// #main-content is the app's scroll container (#190); it must be
	// keyboard-focusable so PageDown/Space/arrows scroll without a pointer
	// (#197 review; axe scrollable-region-focusable).
	if !contains(body, `<main id="main-content" class="flex-1" tabindex="0"`) {
		t.Error(`#main-content scroller should carry tabindex="0"`)
	}

	conv, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	convPage := get(t, srv, "/c/"+itoa(conv.ID)).Body.String()
	if !contains(convPage, `class="conv-header sticky top-0 `) {
		t.Error("conv-header should pin at top-0 inside the #main-content scroller")
	}
	// Spelled "top-["+"54px]" so Tailwind v4's content scanner (which scans
	// .go files too) never sees the retired arbitrary-value token here — a
	// plain literal would resurrect the utility in a rebuilt app.css and trip
	// the CI drift guard against the committed artifact.
	if retired := "top-[" + "54px]"; contains(convPage, retired) {
		t.Errorf("conv-header still carries the retired %s offset", retired)
	}
}

// TestHeaderTabs is the #190 primary-nav contract, extended by #238: the header
// centers a Messages ("/") / Media ("/media") / Journal ("/journal") tab trio,
// boosted like all in-app nav, with the server marking the active tab on full
// loads — Messages on home and every /c/* transcript, Media on the gallery
// surface (/media aliases /gallery), Journal on /journal, NONE on
// Search/Settings/… (shell.js re-syncs the same rule after boosted swaps, which
// never re-render this shell).
func TestHeaderTabs(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}

	const (
		messagesActive = `href="/" data-nav-tab="messages" class="header-tab header-tab-active" aria-current="page"`
		messagesIdle   = `href="/" data-nav-tab="messages" class="header-tab"`
		mediaActive    = `href="/media" data-nav-tab="media" class="header-tab header-tab-active" aria-current="page"`
		mediaIdle      = `href="/media" data-nav-tab="media" class="header-tab"`
		journalActive  = `href="/journal" data-nav-tab="journal" class="header-tab header-tab-active" aria-current="page"`
		journalIdle    = `href="/journal" data-nav-tab="journal" class="header-tab"`
	)
	cases := []struct {
		route         string
		wantMessages  string
		wantMedia     string
		wantJournal   string
		activeSummary string
	}{
		{"/", messagesActive, mediaIdle, journalIdle, "Messages"},
		{"/c/" + itoa(conv.ID), messagesActive, mediaIdle, journalIdle, "Messages"},
		{"/gallery", messagesIdle, mediaActive, journalIdle, "Media"},
		{"/media", messagesIdle, mediaActive, journalIdle, "Media"},
		{"/journal", messagesIdle, mediaIdle, journalActive, "Journal"},
		{"/search", messagesIdle, mediaIdle, journalIdle, "none"},
		{"/settings", messagesIdle, mediaIdle, journalIdle, "none"},
	}
	for _, c := range cases {
		t.Run(c.route, func(t *testing.T) {
			body := get(t, srv, c.route).Body.String()
			if !contains(body, `<nav class="header-tabs" aria-label="Primary"`) {
				t.Fatal("page missing the centered header tab nav")
			}
			if !contains(body, c.wantMessages) || !contains(body, c.wantMedia) || !contains(body, c.wantJournal) {
				t.Errorf("%s should mark %s active (want %q, %q and %q)", c.route, c.activeSummary, c.wantMessages, c.wantMedia, c.wantJournal)
			}
			// All three tabs are boosted via the nav's hx-boost inheritance.
			navStart := strings.Index(body, `<nav class="header-tabs"`)
			navEnd := strings.Index(body[navStart:], "</nav>")
			if navStart < 0 || navEnd < 0 {
				t.Fatal("cannot delimit the header tab nav")
			}
			nav := body[navStart : navStart+navEnd]
			if !strings.Contains(nav, `hx-boost="true"`) {
				t.Error("header tabs should be boosted (hx-boost on the nav)")
			}
			// Exactly one tab may claim the page: two aria-current="page" links
			// in one nav is an a11y defect, not a cosmetic one.
			if got := strings.Count(nav, `aria-current="page"`); got > 1 {
				t.Errorf("%s marks %d tabs as aria-current=page; at most one may", c.route, got)
			}
			// Journal sits after Media (#238), so the strip reads
			// Messages · Media · Journal in DOM order.
			m, md, j := strings.Index(nav, `data-nav-tab="messages"`), strings.Index(nav, `data-nav-tab="media"`), strings.Index(nav, `data-nav-tab="journal"`)
			if !(m < md && md < j) {
				t.Errorf("tab order should be Messages, Media, Journal (got offsets %d, %d, %d)", m, md, j)
			}
		})
	}
}

// TestJournalReachableFromEveryPage is the guard whose absence let the journal
// ship with no link pointing at it (#237/#238): every primary surface must
// carry a link to /journal in the shared shell, so the feature is never again a
// URL you have to know. The shell is rendered by page_start, so this covers any
// future page that uses it.
func TestJournalReachableFromEveryPage(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	for _, route := range []string{"/", "/c/" + itoa(conv.ID), "/media", "/gallery", "/search", "/settings", "/journal", "/status"} {
		if body := get(t, srv, route).Body.String(); !contains(body, `href="/journal"`) {
			t.Errorf("%s does not link to /journal — the journal must be reachable from every page", route)
		}
	}
}

// TestJournalEmptyStateIsNotADeadEnd (#238): the journal is now a nav
// destination, so the pre-build empty state has to explain the surface and
// offer a way forward rather than only naming a CLI command.
func TestJournalEmptyStateIsNotADeadEnd(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body := get(t, srv, "/journal").Body.String()
	if !contains(body, "No journal yet.") {
		t.Fatal("expected the empty-journal notice on a store with no journal days")
	}
	if !contains(body, `href="/settings/llm"`) {
		t.Error("the empty state should link to the LLM settings tab, where the digest model is configured")
	}
}

// TestSelectedRowAccentRail verifies the open conversation's sidebar row carries
// the selected modifier (accent left rail + #1b2330 tint, REQ-0006-003) and that
// non-open rows do not.
func TestSelectedRowAccentRail(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}

	open := get(t, srv, "/c/"+itoa(conv.ID)).Body.String()
	if !contains(open, "conv-row-selected") {
		t.Error("open conversation row missing the selected accent rail modifier")
	}
	// The selected modifier must hang off the active conversation's own row link.
	if !contains(open, `href="/c/`+itoa(conv.ID)+`" class="conv-row conv-row-selected"`) {
		t.Errorf("selected modifier not on the active conversation row")
	}

	// On a non-conversation page nothing is selected.
	home := get(t, srv, "/").Body.String()
	if contains(home, "conv-row-selected") {
		t.Error("home page should not mark any conversation row selected")
	}
}

// TestSourceSlug verifies the Go-chosen source modifier classes used by presence
// dots and source pills (REQ-0006-004).
func TestSourceSlug(t *testing.T) {
	cases := map[string]string{
		source.Signal:   "src-signal",
		source.IMessage: "src-imessage",
		source.WhatsApp: "src-whatsapp",
		"":              "src-unknown",
		"bogus":         "src-unknown",
	}
	for in, want := range cases {
		if got := sourceSlug(in); got != want {
			t.Errorf("sourceSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuiltCSSCarriesShellComponents guards the CSS drift requirement: the
// committed, go:embed-served app.css must contain the bespoke shell + identity
// component rules and the source-derived colors, so the clean rebuild is what
// actually ships (REQ-0006-002/003/004; ADR-0012 drift guard).
func TestBuiltCSSCarriesShellComponents(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	out := string(css)
	for _, want := range []string{
		".app-toolbar",               // unified full-width header (#152 → #190)
		".toolbar-title",             // contextual title
		".toolbar-icon-btn",          // icon buttons (drawer/theme/settings)
		".toolbar-search",            // toolbar search pill
		".toolbar-cluster",           // in-flow header side clusters (#197 review)
		".toolbar-cluster-left",      // truncating (minmax(0,1fr)) title-side cluster
		".toolbar-cluster-right",     // min-content-guaranteed controls cluster
		".header-tabs",               // centered Messages/Media tab strip (#190)
		".header-tab",                // individual tab
		".header-tab-active",         // active-tab lift
		".sidebar-filter",            // filter input
		".avatar-mono",               // monogram avatar
		".presence-dot.src-signal",   // Signal presence dot
		".presence-dot.src-imessage", // iMessage presence dot
		".presence-dot.src-whatsapp", // WhatsApp presence dot (REQ-0009-007)
		".source-pill.src-signal",    // Signal source pill
		".source-pill.src-imessage",  // iMessage source pill
		".source-pill.src-whatsapp",  // WhatsApp source pill (REQ-0009-007)
		".conv-row-selected",         // selected-row modifier
		".pin-btn",                   // pin/unpin toggle (REQ-0006-010)
		".pin-btn-active",            // pinned-state fill
		"--color-info:#3b82f6",       // Signal blue token (slate)
		"--color-success:#34c759",    // iMessage green token (slate)
		"--color-whatsapp:#25d366",   // WhatsApp green token (slate)
		"--color-whatsapp:#128c7e",   // WhatsApp teal-green token (slate-light)
	} {
		if !strings.Contains(out, want) {
			t.Errorf("built app.css missing %q (rebuild: rm -rf .tools && make css)", want)
		}
	}
	// The retired 54px sticky-offset arbitrary-value utility must stay dead:
	// Tailwind v4 scans template AND Go comments, so a literal class token
	// anywhere in the tree would re-emit the rule into the built artifact
	// (#197 review, minor 4).
	if strings.Contains(out, "top:54px") {
		t.Error("built app.css resurrects the retired 54px sticky-offset utility (reword the comment token, then rm -rf .tools && make css)")
	}

	// The presence dot reads its color from the theme variables so both slate and
	// slate-light variants restyle automatically.
	if !strings.Contains(out, ".presence-dot.src-signal{background:var(--color-info)}") {
		t.Error("presence dot should derive its color from --color-info")
	}
	if !strings.Contains(out, ".presence-dot.src-whatsapp{background:var(--color-whatsapp)}") {
		t.Error("whatsapp presence dot should derive its color from --color-whatsapp")
	}
}

// TestPinnedSection covers REQ-0006-010 end to end: with nothing pinned the
// sidebar's PINNED section wrapper renders hidden (it is ALWAYS in the DOM as
// the pin toggle's OOB swap target — #176); POSTing to /c/{id}/pin
// 303-redirects back, marks the conversation pinned, and the sidebar then
// shows the section. A second POST unpins it. The pin button on the
// conversation header reflects the current state.
func TestPinnedSection(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	conv, err := st.GetConversation(ctx, "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	cid := itoa(conv.ID)

	// Nothing pinned: the wrapper is present but hidden (class, not inline
	// style — the strict CSP forbids the latter), and the header button reads
	// "Pin".
	home := get(t, srv, "/").Body.String()
	if !contains(home, `id="sidebar-pinned-section" class="hidden"`) {
		t.Error("PINNED section wrapper should render hidden when nothing is pinned")
	}
	convPage := get(t, srv, "/c/"+cid).Body.String()
	if !contains(convPage, `action="/c/`+cid+`/pin"`) {
		t.Error("conversation header missing the pin form POST")
	}
	if !contains(convPage, ">Pin<") {
		t.Error("pin button should read 'Pin' when unpinned")
	}

	// Pin via the POST route: expect a 303 redirect back to the conversation.
	rec := post(t, srv, "/c/"+cid+"/pin")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("pin POST status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/c/"+cid {
		t.Errorf("pin redirect = %q, want %q", loc, "/c/"+cid)
	}

	// Sidebar now shows the PINNED section listing the conversation.
	home = get(t, srv, "/").Body.String()
	if contains(home, `id="sidebar-pinned-section" class="hidden"`) {
		t.Error("PINNED section should be visible after pinning")
	}
	if !contains(home, ">Pinned<") || !contains(home, `id="sidebar-pinned"`) {
		t.Error("PINNED list container missing")
	}
	// The pinned row links to the conversation and uses the shared row markup.
	if !contains(home, `<ul id="sidebar-pinned"`) || !contains(home, `href="/c/`+cid+`"`) {
		t.Error("pinned conversation row missing from PINNED section")
	}

	// Header button now reads "Unpin".
	convPage = get(t, srv, "/c/"+cid).Body.String()
	if !contains(convPage, ">Unpin<") {
		t.Error("pin button should read 'Unpin' when pinned")
	}

	// Unpin: second POST flips it back; PINNED section hides again.
	if rec := post(t, srv, "/c/"+cid+"/pin"); rec.Code != http.StatusSeeOther {
		t.Fatalf("unpin POST status = %d, want 303", rec.Code)
	}
	home = get(t, srv, "/").Body.String()
	if !contains(home, `id="sidebar-pinned-section" class="hidden"`) {
		t.Error("PINNED section should hide again after unpinning")
	}
}

// TestShellScrollRestoreBindings pins the #197 reading-position event wiring
// in shell.js after the back/forward corruption fix (#190). restoreScroll may
// bind ONLY to htmx:historyRestore — htmx 2.0.4 fires it after the swap on
// both history-cache paths (hit and server-refetch miss). A popstate binding
// is the corruption itself: shell.js loads before htmx.min.js, so its
// popstate listener runs while the DEPARTING page is still mounted but
// location already names the destination — it scrolls the departing page to
// the destination's offset (clamped), which htmx:beforeHistorySave then
// persists under the departing page's key, clobbering both entries on every
// back/forward traversal. saveScroll must stay keyed by the event's
// detail.path (htmx's path-for-history), never by location, for the same
// URL-already-flipped reason.
func TestShellScrollRestoreBindings(t *testing.T) {
	b, err := os.ReadFile("static/shell.js")
	if err != nil {
		t.Fatalf("read shell.js: %v", err)
	}
	js := string(b)

	if !contains(js, `document.addEventListener("htmx:beforeHistorySave", saveScroll)`) {
		t.Error("shell.js should save the scroller offset on htmx:beforeHistorySave")
	}
	if !contains(js, `document.addEventListener("htmx:historyRestore", restoreScroll)`) {
		t.Error("shell.js should restore the scroller offset on htmx:historyRestore")
	}
	// saveScroll keys by htmx's detail.path, not location (the URL has already
	// flipped to the destination when htmx saves the departing page).
	if !contains(js, "e.detail.path") {
		t.Error("saveScroll should key by the htmx:beforeHistorySave detail.path")
	}
	// The corruption regression: restoring on popstate touches the departing
	// page's scroller with the destination's offset. The only popstate
	// listener shell.js may register is the idempotent tab-state sync.
	if contains(js, "popstate\", restoreScroll") {
		t.Error("restoreScroll must not run on popstate — it fires before htmx swaps the destination in and corrupts both pages' saved offsets")
	}
	if got := strings.Count(js, `"popstate"`); got != 1 {
		t.Errorf("shell.js should register exactly one popstate listener (sync), found %d", got)
	}
	if !contains(js, `window.addEventListener("popstate", sync)`) {
		t.Error("shell.js lost the popstate tab-state sync listener")
	}
}

// TestShellTabSyncKnowsJournal (#238) keeps shell.js's location sync in lockstep
// with baseData.NavTab. Boosted swaps never re-render the header, so the server
// marks the active tab only on full loads — if shell.js does not also map
// /journal to the journal tab, clicking Journal swaps the page but leaves the
// previous tab lit.
func TestShellTabSyncKnowsJournal(t *testing.T) {
	b, err := os.ReadFile("static/shell.js")
	if err != nil {
		t.Fatalf("read shell.js: %v", err)
	}
	js := string(b)
	if !contains(js, `path === "/journal"`) {
		t.Error("shell.js sync() should map /journal to a tab name")
	}
	if !contains(js, `name = "journal"`) {
		t.Error(`shell.js should resolve /journal to the "journal" tab, matching data-nav-tab and baseData.navTabJournal`)
	}
	// Below ~360px the three tabs do not fit and the strip becomes a
	// hidden-scrollbar scroll container. Without this, the tab you are ON is the
	// one that can sit off the right edge with no scrollbar to hint at it — i.e.
	// landing on /journal would show Messages/Media and no active tab, defeating
	// the point of #238. "nearest" scrolls only the overflowing strip, never the
	// page or #main-content.
	if !contains(js, `scrollIntoView({ block: "nearest", inline: "nearest" })`) {
		t.Error("syncTabs should scroll the active tab into view with block/inline nearest, so a scrolled strip never hides the current page's tab")
	}
}

// TestNarrowHeaderFitsThreeTabs (#238) guards the narrow-viewport trims that a
// third tab forced. Measured in a browser at 320px WITHOUT them, the tab strip
// grew 142px→209px, drove the minmax(0,1fr) left track to zero, and let the
// burger paint over "Messages" while the right cluster spilled past the toolbar
// — the overlap #197 fixed. These rules are what keep all three tabs reachable
// below sm; they live in input.css and must survive into the built artifact.
//
// Each assertion is ANCHORED inside its media-query block rather than searched
// across the whole file. The first cut of this test asserted
// strings.Contains(css, "overflow-x:auto"), which the artifact already satisfied
// three times over (.stats, the .overflow-x-auto utility, .copy-pre) — so it
// passed without the rule it was meant to guard existing at all. Anchoring by
// block keeps it honest; matching properties (not whole minified rules) keeps a
// Tailwind upgrade that reorders declarations from failing it spuriously.
func TestNarrowHeaderFitsThreeTabs(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	out := string(css)

	// mediaBlock returns the body of the first @media block with this exact
	// prelude, brace-matched so a nested block cannot truncate it.
	mediaBlock := func(prelude string) string {
		i := strings.Index(out, prelude+"{")
		if i < 0 {
			return ""
		}
		depth, start := 0, i+len(prelude)
		for j := start; j < len(out); j++ {
			switch out[j] {
			case '{':
				depth++
			case '}':
				if depth--; depth == 0 {
					return out[start+1 : j]
				}
			}
		}
		return ""
	}
	// rule returns the declarations of `selector{...}` within body.
	rule := func(body, selector string) string {
		i := strings.Index(body, selector+"{")
		if i < 0 {
			return ""
		}
		rest := body[i+len(selector)+1:]
		end := strings.Index(rest, "}")
		if end < 0 {
			return ""
		}
		return rest[:end]
	}

	tier1 := mediaBlock("@media (max-width:39.9375rem)")
	if tier1 == "" {
		t.Fatal("built app.css lost the below-sm narrow-width media block (rebuild: make css)")
	}
	strip := rule(tier1, ".header-tabs")
	for _, want := range []string{
		// Clamp to the grid track. Without it the tier-2 justify-self:center pill
		// is content-sized and overflows the track instead of scrolling, which put
		// the burger back underneath the strip at 320px (measured).
		"max-width:100%",
		"min-width:0",
		"overflow-x:auto",
		// The 4px padding is contract, not cosmetics: overflow-x makes this a
		// scroll container, a scroll container clips at its PADDING box, and a
		// tab's focus ring reaches 4px outside the tab (outline 2px at offset
		// 2px) — at the inherited 2px padding the ring was clipped away and
		// keyboard focus went invisible on every viewport below 640px.
		"padding:4px",
	} {
		if !strings.Contains(strip, want) {
			t.Errorf("below-sm .header-tabs missing %q (got %q; rebuild: make css)", want, strip)
		}
	}

	tier2 := mediaBlock("@media (max-width:28rem)")
	if tier2 == "" {
		t.Fatal("built app.css lost the tier-2 (28rem) block where three tabs stop fitting (rebuild: make css)")
	}
	for _, tc := range []struct{ selector, want, why string }{
		// Content-sized side tracks: the wide layout's minmax(0,1fr) left track
		// has a zero minimum, which is what let the burger be starved to nothing
		// and painted over by the strip.
		{".app-toolbar", "grid-template-columns:min-content minmax(0,1fr) auto",
			"tier-2 toolbar tracks, so the sidebar toggle cannot be starved to zero"},
		// .header-tabs is a grid item: its inline-flex blockifies and justify-self
		// defaults to stretch, so without this the bordered pill spans the whole
		// middle column with the labels jammed against its left edge.
		{".header-tabs", "justify-self:center",
			"tier-2 centering — without it the strip stretches the full column and the tabs jam left"},
		// Hiding the label alone was not enough: the remaining home glyph costs
		// ~25px, the difference between three tabs fitting at 360px and not.
		{".toolbar-title", "display:none",
			"tier-2 hiding of the WHOLE contextual title, not just its label"},
	} {
		if got := rule(tier2, tc.selector); !strings.Contains(got, tc.want) {
			t.Errorf("tier-2 %s missing %s\n  want: %s\n  got:  %q\n  (rebuild: make css)", tc.selector, tc.why, tc.want, got)
		}
	}
}

// TestThemeStillSelfHosted re-checks ADR-0010/REQ-0006-001: every script the
// shell loads is same-origin, so the strict CSP (script-src 'self') holds with
// the new sidebar.js.
func TestThemeStillSelfHosted(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := get(t, srv, "/")
	body := rec.Body.String()
	for _, src := range []string{"/static/theme.js", "/static/sidebar-toggle.js", "/static/sidebar.js", "/static/shell.js", "/static/htmx.min.js"} {
		if !contains(body, `src="`+src+`"`) {
			t.Errorf("page missing self-hosted script %q", src)
		}
	}
	// No off-origin script/style references slipped in. (SVG xmlns URLs are not
	// fetched, so we check src=/href= attributes specifically.)
	for _, bad := range []string{`src="http://`, `src="https://`, `href="https://cdn`} {
		if contains(body, bad) {
			t.Errorf("page references an off-origin asset (%q); CSP would block it", bad)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !contains(csp, "script-src 'self'") {
		t.Errorf("CSP no longer restricts scripts to self: %q", csp)
	}
}
