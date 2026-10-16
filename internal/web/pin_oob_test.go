package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/store"
)

// postPartial issues a POST as an HTMX boosted form submission (HX-Request set,
// redirect not followed — htmx receives the response body directly).
func postPartial(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Host = selfHost
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// fixtureConversation resolves a fixture conversation by name.
func fixtureConversation(t *testing.T, st *store.Store, name string) *store.ConversationSummary {
	t.Helper()
	conv, err := st.GetConversation(context.Background(), name)
	if err != nil || conv == nil {
		t.Fatalf("get conversation %q: %v", name, err)
	}
	return conv
}

// TestPinFirstPinSwapsSidebarOOB is the #176 regression: with ZERO pinned
// conversations, a boosted pin POST must carry an out-of-band sidebar fragment
// whose target id exists in the fresh base-page sidebar — the old fragment
// targeted #sidebar-pinned, which was only rendered once something was already
// pinned, so the first-ever pin's swap dropped silently.
func TestPinFirstPinSwapsSidebarOOB(t *testing.T) {
	srv, st, _ := newTestServer(t)
	cid := itoa(fixtureConversation(t, st, "Harper").ID)

	// The OOB target must exist in a fresh zero-pinned sidebar render — hidden
	// via class (never an inline style), but present in the DOM.
	home := get(t, srv, "/").Body.String()
	if !contains(home, `id="sidebar-pinned-section" class="hidden"`) {
		t.Fatal("zero-pinned sidebar missing the hidden #sidebar-pinned-section OOB target")
	}

	rec := postPartial(t, srv, "/c/"+cid+"/pin")
	if rec.Code != http.StatusOK {
		t.Fatalf("boosted pin POST status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// The response carries the OOB pinned-section wrapper, now visible (no
	// hidden class) with the header label and the pinned row inside it.
	if !contains(body, `id="sidebar-pinned-section" hx-swap-oob="true"`) {
		t.Error("pin response missing the OOB #sidebar-pinned-section swap")
	}
	if contains(body, `id="sidebar-pinned-section" class="hidden"`) {
		t.Error("OOB pinned section should be visible after the first pin")
	}
	if !contains(body, ">Pinned<") || !contains(body, `id="sidebar-pinned"`) {
		t.Error("OOB pinned section missing its header or list")
	}
	if !contains(body, `href="/c/`+cid+`"`) {
		t.Error("OOB pinned section missing the pinned conversation row")
	}
	// The unpinned list rides along so the row moves out of CONVERSATIONS.
	if !contains(body, `id="sidebar-conversations" `) {
		t.Error("pin response missing the OOB #sidebar-conversations swap")
	}
	// The button state flips in the same #main-content swap.
	if !contains(body, ">Unpin<") {
		t.Error("pin button should read 'Unpin' in the pin response")
	}
	// History must record the conversation URL, never the POST-only /pin route.
	if got := rec.Header().Get("HX-Push-Url"); got != "/c/"+cid {
		t.Errorf("HX-Push-Url = %q, want %q", got, "/c/"+cid)
	}
	// The row swap stales sidebar.js's captured filter list; the trigger tells
	// it to re-init. It must ride HX-Trigger-After-Settle — plain HX-Trigger
	// dispatches before the swap, so the re-init would capture the doomed rows.
	if got := rec.Header().Get("HX-Trigger-After-Settle"); got != pinnedSidebarTrigger {
		t.Errorf("HX-Trigger-After-Settle = %q, want %q", got, pinnedSidebarTrigger)
	}
	if got := rec.Header().Get("HX-Trigger"); got != "" {
		t.Errorf("HX-Trigger = %q, want unset (re-init must fire after settle)", got)
	}
}

// TestUnpinLastHidesSidebarSectionOOB: unpinning the only pinned conversation
// must OOB-swap the section back to its hidden state and flip the button to
// "Pin" (#176).
func TestUnpinLastHidesSidebarSectionOOB(t *testing.T) {
	srv, st, _ := newTestServer(t)
	cid := itoa(fixtureConversation(t, st, "Harper").ID)

	if rec := postPartial(t, srv, "/c/"+cid+"/pin"); rec.Code != http.StatusOK {
		t.Fatalf("pin POST status = %d, want 200", rec.Code)
	}
	rec := postPartial(t, srv, "/c/"+cid+"/pin")
	if rec.Code != http.StatusOK {
		t.Fatalf("unpin POST status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, `id="sidebar-pinned-section" class="hidden" hx-swap-oob="true"`) {
		t.Error("unpin response should OOB-swap the pinned section back to hidden")
	}
	if !contains(body, ">Pin<") {
		t.Error("pin button should read 'Pin' in the unpin response")
	}
	// And a fresh full render agrees with the swapped-in state.
	home := get(t, srv, "/").Body.String()
	if !contains(home, `id="sidebar-pinned-section" class="hidden"`) {
		t.Error("base sidebar should render the section hidden after the unpin")
	}
}

// TestPinPlainPOSTStillRedirects: the no-JS degradation path is unchanged — a
// plain form POST 303s back to the conversation and never emits fragments.
func TestPinPlainPOSTStillRedirects(t *testing.T) {
	srv, st, _ := newTestServer(t)
	cid := itoa(fixtureConversation(t, st, "Harper").ID)

	rec := post(t, srv, "/c/"+cid+"/pin")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("plain pin POST status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/c/"+cid {
		t.Errorf("pin redirect = %q, want %q", loc, "/c/"+cid)
	}
	if contains(rec.Body.String(), "hx-swap-oob") {
		t.Error("plain POST should not carry OOB fragments")
	}
}

// TestConversationMetaChipsDeepLinkToGallery (#177): the header's image/link/
// attachment count chips are boosted anchors into the gallery filtered to this
// conversation on the matching tab, keeping the meta-chip look; zero-count
// chips stay hidden; and the emitted URL round-trips through the gallery's own
// filter parsing.
func TestConversationMetaChipsDeepLinkToGallery(t *testing.T) {
	srv, st, _ := newTestServer(t)
	harper := fixtureConversation(t, st, "Harper")
	cid := itoa(harper.ID)

	// Harper has one image, one link, and one file in the fixture.
	body := get(t, srv, "/c/"+cid).Body.String()
	for _, tab := range []string{"images", "links", "files"} {
		// html/template escapes the & separator inside href attributes.
		want := `<a class="meta-chip" href="/gallery?conversation=` + cid + `&amp;tab=` + tab + `"`
		if !contains(body, want) {
			t.Errorf("conversation header missing the %s gallery chip (want %s)", tab, want)
		}
	}
	// Chips are boosted like the header's other in-app links.
	i := strings.Index(body, `href="/gallery?conversation=`)
	chipTag := body[strings.LastIndex(body[:i], "<a"):]
	chipTag = chipTag[:strings.Index(chipTag, ">")+1]
	for _, want := range []string{`hx-boost="true"`, `hx-target="#main-content"`, `hx-select="#main-content"`} {
		if !contains(chipTag, want) {
			t.Errorf("gallery chip missing %q (tag: %s)", want, chipTag)
		}
	}

	// Group Trip has no file attachments: its files chip must not render.
	group := fixtureConversation(t, st, "Group Trip")
	gbody := get(t, srv, "/c/"+itoa(group.ID)).Body.String()
	if contains(gbody, `tab=files`) {
		t.Error("zero-count files chip should stay hidden")
	}
	if !contains(gbody, `conversation=`+itoa(group.ID)+`&amp;tab=images`) {
		t.Error("Group Trip images chip missing")
	}

	// The chip URL round-trips: the gallery filtered to Harper shows Harper's
	// image and not Group Trip's.
	gal := get(t, srv, "/gallery?conversation="+cid+"&tab=images").Body.String()
	if !contains(gal, "cabin.jpg") || contains(gal, "sunset.png") {
		t.Error("chip deep link did not filter the gallery to the conversation")
	}
}
