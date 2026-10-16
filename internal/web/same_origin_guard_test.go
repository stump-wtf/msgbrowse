package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/joestump/msgbrowse/internal/store"
)

// postWithHeaders issues a bodyless form POST with the given request headers set,
// so each case can state exactly the provenance it wants the guard to read.
func postWithHeaders(t *testing.T, srv *Server, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// pinState reads the fixture conversation's pinned flag straight from the store,
// so a refused request is proven to have changed nothing rather than merely
// having answered 403.
func pinState(t *testing.T, st *store.Store, id int64) bool {
	t.Helper()
	conv, err := st.GetConversationByID(context.Background(), id)
	if err != nil || conv == nil {
		t.Fatalf("get conversation %d: %v", id, err)
	}
	return conv.Pinned
}

// TestPinRejectedCrossSite: a POST carrying Sec-Fetch-Site: cross-site is refused
// with 403 and the pin is untouched.
func TestPinRejectedCrossSite(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv := fixtureConversation(t, st, "Harper")

	rec := postWithHeaders(t, srv, "/c/"+itoa(conv.ID)+"/pin", map[string]string{"Sec-Fetch-Site": "cross-site"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST status = %d, want 403", rec.Code)
	}
	if pinState(t, st, conv.ID) {
		t.Error("cross-site POST pinned the conversation")
	}
}

// TestPinRejectedCrossOrigin: Sec-Fetch-Site: cross-origin — a page on another
// origin inside the same site — is refused the same way.
func TestPinRejectedCrossOrigin(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv := fixtureConversation(t, st, "Harper")

	rec := postWithHeaders(t, srv, "/c/"+itoa(conv.ID)+"/pin", map[string]string{"Sec-Fetch-Site": "cross-origin"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST status = %d, want 403", rec.Code)
	}
	if pinState(t, st, conv.ID) {
		t.Error("cross-origin POST pinned the conversation")
	}
}

// TestPinRejectedForeignOrigin is the blind-POST shape the issue names: another
// page's form posts to the loopback address and the browser stamps the attacker
// page's Origin.
func TestPinRejectedForeignOrigin(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv := fixtureConversation(t, st, "Harper")

	rec := postWithHeaders(t, srv, "/c/"+itoa(conv.ID)+"/pin", map[string]string{"Origin": "http://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign-Origin POST status = %d, want 403", rec.Code)
	}
	if pinState(t, st, conv.ID) {
		t.Error("foreign-Origin POST pinned the conversation")
	}
}

// TestPinRejectedCrossOriginReferer: with no Origin and no Sec-Fetch-Site, a
// cross-origin Referer still states provenance, and it states the wrong one.
func TestPinRejectedCrossOriginReferer(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv := fixtureConversation(t, st, "Harper")

	rec := postWithHeaders(t, srv, "/c/"+itoa(conv.ID)+"/pin", map[string]string{"Referer": "http://evil.example/page"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin Referer POST status = %d, want 403", rec.Code)
	}
	if pinState(t, st, conv.ID) {
		t.Error("cross-origin Referer POST pinned the conversation")
	}
}

// TestPinAllowedSameOriginOrigin: the app's own page posts with its own Origin,
// which must pass with the usual 303 back to the conversation.
func TestPinAllowedSameOriginOrigin(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv := fixtureConversation(t, st, "Harper")

	rec := postWithHeaders(t, srv, "/c/"+itoa(conv.ID)+"/pin", map[string]string{"Origin": selfOrigin})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("same-origin POST status = %d, want 303", rec.Code)
	}
	if !pinState(t, st, conv.ID) {
		t.Error("same-origin POST did not pin the conversation")
	}
}

// TestPinAllowedSameOriginSecFetchSite: a same-origin POST that states itself
// only through Sec-Fetch-Site is allowed.
func TestPinAllowedSameOriginSecFetchSite(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv := fixtureConversation(t, st, "Harper")

	rec := postWithHeaders(t, srv, "/c/"+itoa(conv.ID)+"/pin", map[string]string{"Sec-Fetch-Site": "same-origin"})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("Sec-Fetch-Site same-origin POST status = %d, want 303", rec.Code)
	}
	if !pinState(t, st, conv.ID) {
		t.Error("Sec-Fetch-Site same-origin POST did not pin the conversation")
	}
}

// TestPinAllowedWithoutProvenance pins the deliberate asymmetry with the
// privileged-POST gate: a header-less caller (curl, a CLI, a script on this
// machine) is not treated as a CSRF attempt, because under ADR-0010 anything
// that can reach the loopback socket can already do the same work directly.
func TestPinAllowedWithoutProvenance(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv := fixtureConversation(t, st, "Harper")

	rec := postWithHeaders(t, srv, "/c/"+itoa(conv.ID)+"/pin", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("header-less POST status = %d, want 303", rec.Code)
	}
	if !pinState(t, st, conv.ID) {
		t.Error("header-less POST did not pin the conversation")
	}
}

// TestMutatingRoutesAllGuarded: the guard is one middleware, not a per-route
// copy, so every mutating route — including ones whose seams are unwired in this
// server, which would otherwise 503 — answers 403 to a cross-origin POST before
// any handler runs.
func TestMutatingRoutesAllGuarded(t *testing.T) {
	srv, _, _ := newTestServer(t)
	routes := []string{
		"/c/1/pin",
		"/journal/build",
		"/journal/rebuild",
		"/facts/run",
		"/facts/reset",
		"/sentiment/run",
		"/status/index",
		"/backups/create",
		"/backups/restore",
		"/settings/contacts/merge",
		"/settings/contacts/split",
		"/setup/enable",
	}
	for _, path := range routes {
		rec := postWithHeaders(t, srv, path, map[string]string{"Origin": "http://evil.example"})
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s with a foreign Origin = %d, want 403", path, rec.Code)
		}
	}
}

// TestReadsUnguarded: the guard covers state changes only. A cross-origin GET
// still renders — reads are not state changes, and serving them to whatever
// reaches the loopback socket is what the app is for.
func TestReadsUnguarded(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, path := range []string{"/", "/search?q=msgbrowse", "/status"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set("Origin", "http://evil.example")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden {
			t.Errorf("GET %s was refused by the guard", path)
		}
	}
}

// TestGuardRunsBeforeRouting: a cross-origin request whose method is mutating
// but unregistered is refused outright rather than answered with a 405 that
// advertises the route, so probing cannot map the POST surface.
func TestGuardRunsBeforeRouting(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPut, "/c/1/pin", nil)
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin PUT status = %d, want 403", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "" {
		t.Errorf("refused PUT advertised Allow: %q", allow)
	}
}

// TestOptionsUnguarded keeps OPTIONS (a safe method per RFC 9110) out of the
// guard, so the 405 shape the security tests pin is unchanged for it.
func TestOptionsUnguarded(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodOptions, "/c/1/pin", nil)
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatal("OPTIONS was refused by the guard; it is a safe method")
	}
}

// TestSameOriginOrUnknown covers the helper's layering directly, including the
// future-proof fall-through for a Sec-Fetch-Site token no browser sends.
func TestSameOriginOrUnknown(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"origin match", map[string]string{"Origin": selfOrigin}, true},
		{"origin mismatch", map[string]string{"Origin": "http://evil.example"}, false},
		{"same-origin", map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		{"none", map[string]string{"Sec-Fetch-Site": "none"}, true},
		{"cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"cross-origin", map[string]string{"Sec-Fetch-Site": "cross-origin"}, false},
		{"same-site", map[string]string{"Sec-Fetch-Site": "same-site"}, false},
		{"unknown token falls back", map[string]string{"Sec-Fetch-Site": "from-network"}, true},
		{"referer match", map[string]string{"Referer": "http://example.com/c/1"}, true},
		{"referer mismatch", map[string]string{"Referer": "http://evil.example/"}, false},
		{"no provenance", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/c/1/pin", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			if got := sameOriginOrUnknown(req); got != tc.want {
				t.Errorf("sameOriginOrUnknown = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSameOriginPrivilegedGateUnchanged: the privileged POST gate still demands
// provenance, so a header-less POST that the blanket guard lets through is
// refused the moment a handler calls checkSetupPOST.
func TestSameOriginPrivilegedGateUnchanged(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/c/1/pin", nil)
	if !sameOriginOrUnknown(req) {
		t.Error("blanket guard refused a header-less POST")
	}
	if sameOrigin(req) {
		t.Error("privileged gate accepted a POST with no provenance")
	}
}
