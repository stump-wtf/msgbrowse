package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// postWithHost issues a bodyless POST claiming a specific Host header, with any
// extra provenance headers, so each case states exactly the request a DNS-rebinding
// page (or a legitimate local client) would put on the wire.
func postWithHost(t *testing.T, srv *Server, path, host string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestReboundHostPOSTRefused is issue #526's regression test: attacker.example's A
// record points at 127.0.0.1, so the browser sends the rebound name as Host AND as
// Origin and stamps Sec-Fetch-Site: same-origin. Every provenance layer
// sameOriginGuard consults then agrees — because they are all derived from the
// request — so the request is refused for naming a host this server is never
// addressed by, and the pin is untouched.
func TestReboundHostPOSTRefused(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv := fixtureConversation(t, st, "Harper")

	rec := postWithHost(t, srv, "/c/"+itoa(conv.ID)+"/pin", "attacker.example:8787", map[string]string{
		"Origin":         "http://attacker.example:8787",
		"Sec-Fetch-Site": "same-origin",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rebound-host POST status = %d, want 400", rec.Code)
	}
	if pinState(t, st, conv.ID) {
		t.Error("rebound-host POST pinned the conversation")
	}
}

// TestReboundHostRefusedWithoutProvenance: the blanket guard deliberately lets a
// header-less caller through (curl, a CLI), and a rebound page can send no Origin
// at all on a simple POST. The pin does not depend on the client saying anything:
// the Host it claims is enough to refuse it.
func TestReboundHostRefusedWithoutProvenance(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv := fixtureConversation(t, st, "Harper")

	rec := postWithHost(t, srv, "/c/"+itoa(conv.ID)+"/pin", "attacker.example:8787", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("header-less rebound-host POST status = %d, want 400", rec.Code)
	}
	if pinState(t, st, conv.ID) {
		t.Error("header-less rebound-host POST pinned the conversation")
	}
}

// TestHostPinRunsAheadOfOriginGuard: a request that fails the pin is refused for
// that reason, not argued with on the origin layers it would otherwise pass.
func TestHostPinRunsAheadOfOriginGuard(t *testing.T) {
	srv, _, _ := newTestServer(t)

	rec := postWithHost(t, srv, "/c/1/pin", "attacker.example:8787", map[string]string{"Sec-Fetch-Site": "cross-site"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rebound-host cross-site POST status = %d, want 400 (the pin runs first)", rec.Code)
	}
}

// TestLoopbackHostFormsAllowed pins the pass-through side: every form a legitimate
// local client actually uses — the configured bind, the desktop shell's ephemeral
// port, curl's -h override, IPv6 bracketed and bare, any case, with or without a
// port — is accepted, so the pin costs no working client anything.
func TestLoopbackHostFormsAllowed(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv := fixtureConversation(t, st, "Harper")

	hosts := []string{
		"127.0.0.1:8787",
		"127.0.0.1",
		"127.0.0.1:53124",
		"localhost:8787",
		"localhost",
		"LOCALHOST:8787",
		"[::1]:8787",
		"[::1]",
		"::1",
	}
	for _, host := range hosts {
		rec := postWithHost(t, srv, "/c/"+itoa(conv.ID)+"/pin", host, nil)
		if rec.Code != http.StatusSeeOther {
			t.Errorf("POST with Host %q = %d, want 303", host, rec.Code)
		}
	}
}

// TestNonLoopbackHostsRefused covers the refusal side beyond the rebinding shape:
// a routable address, a hostname, a wildcard Host and a missing Host are none of
// them forms this server is addressed by while ADR-0010's posture holds.
func TestNonLoopbackHostsRefused(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv := fixtureConversation(t, st, "Harper")

	hosts := []string{
		"attacker.example",
		"attacker.example:8787",
		"192.168.1.10:8787",
		"10.0.0.5:8787",
		"0.0.0.0:8787",
		"msgbrowse.internal",
		"127.0.0.1.evil.example",
		"",
	}
	for _, host := range hosts {
		rec := postWithHost(t, srv, "/c/"+itoa(conv.ID)+"/pin", host, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST with Host %q = %d, want 400", host, rec.Code)
		}
	}
	if pinState(t, st, conv.ID) {
		t.Error("a refused Host pinned the conversation")
	}
}

// TestReadsNotHostPinned: like the origin guard, the pin covers state changes. A
// read is not one, a cross-origin read is unreadable by the page that made it, and
// serving reads to whatever reaches the socket is what the app is for.
func TestReadsNotHostPinned(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, path := range []string{"/", "/search?q=msgbrowse", "/status"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "attacker.example:8787"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusBadRequest {
			t.Errorf("GET %s with a rebound Host was refused by the pin", path)
		}
	}
}

// TestMutatingRoutesAllHostPinned: one middleware, not a per-route copy, so every
// mutating route — including the privileged ones whose seams are unwired here —
// refuses a rebound Host before any handler runs.
func TestMutatingRoutesAllHostPinned(t *testing.T) {
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
		"/settings/llm",
		"/settings/devices/pair",
		"/setup/enable",
		"/setup/disable",
	}
	for _, path := range routes {
		rec := postWithHost(t, srv, path, "attacker.example:8787", map[string]string{
			"Origin": "http://attacker.example:8787",
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s with a rebound Host = %d, want 400", path, rec.Code)
		}
	}
}

// TestHostPinInertOnNonLoopbackBind: binding beyond loopback is ADR-0010's
// operator-owned opt-out (Listen warns about it), and there the address the user
// types is not knowable in advance. The pin then stands down rather than refusing
// every state change on a UI whose protection was never this one.
func TestHostPinInertOnNonLoopbackBind(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv := fixtureConversation(t, st, "Harper")
	srv.boundNonLoopback = true

	rec := postWithHost(t, srv, "/c/"+itoa(conv.ID)+"/pin", "nas.local:8787", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST to a non-loopback bind = %d, want 303", rec.Code)
	}
	if !pinState(t, st, conv.ID) {
		t.Error("POST to a non-loopback bind did not pin the conversation")
	}
}

// TestHostPinStillEnforcesOriginGuardOffLoopbackBind: standing the pin down does
// not stand down #41's guard — off the loopback posture a cross-origin POST is
// still refused, and still changes nothing.
func TestHostPinStillEnforcesOriginGuardOffLoopbackBind(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv := fixtureConversation(t, st, "Harper")
	srv.boundNonLoopback = true

	rec := postWithHost(t, srv, "/c/"+itoa(conv.ID)+"/pin", "nas.local:8787", map[string]string{
		"Origin": "http://evil.example",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST to a non-loopback bind = %d, want 403", rec.Code)
	}
	if pinState(t, st, conv.ID) {
		t.Error("cross-origin POST to a non-loopback bind pinned the conversation")
	}
}

// TestLoopbackHostFormUnit covers the parser directly, including the port and
// bracket handling a table of end-to-end requests cannot separate.
func TestLoopbackHostFormUnit(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.1:8787", true},
		{"[::1]:8787", true},
		{"[::1]", true},
		{"::1", true},
		{"localhost", true},
		{"LocalHost:8787", true},
		{"localhost:0", true},
		{" 127.0.0.1:8787 ", true},
		{"attacker.example:8787", false},
		{"127.0.0.1.evil.example", false},
		{"http://127.0.0.1:8787", false},
		{"127.0.0.1:8787:9999", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := loopbackHostForm(tc.host); got != tc.want {
			t.Errorf("loopbackHostForm(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}
