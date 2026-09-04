package web

// Tests for the launch-at-login seam (issue #430): the toggle renders only
// when the desktop shell wired a registration, the POST is gated by the same
// token + same-origin check as every privileged POST, and the route does not
// exist at all in browser mode.
//
// @joestump-agent 09/04/2026 - Added with #430.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAutostart records the last requested state; an injected error drives
// the banner's failure path.
type fakeAutostart struct {
	enabled  bool
	err      error
	enables  int
	disables int
}

func (f *fakeAutostart) Enabled() bool { return f.enabled }
func (f *fakeAutostart) Enable() error {
	f.enables++
	if f.err != nil {
		return f.err
	}
	f.enabled = true
	return nil
}
func (f *fakeAutostart) Disable() error {
	f.disables++
	if f.err != nil {
		return f.err
	}
	f.enabled = false
	return nil
}

// TestAutostartHiddenInBrowserMode: no registration wired — the settings
// page carries no card, and the POST route 404s rather than advertising a
// surface that cannot work.
func TestAutostartHiddenInBrowserMode(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := httptest.NewRequest(http.MethodGet, "/settings/mcp", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, rec)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /settings/mcp: %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "autostart") {
		t.Fatal("toggle card must not render without a wired registration")
	}

	w2 := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/settings/autostart", strings.NewReader("enabled=on"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	srv.Handler().ServeHTTP(w2, req)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("POST /settings/autostart unwired: got %d, want 404", w2.Code)
	}
}

// TestAutostartToggleRoundTrip: with the seam wired, the card renders, and a
// same-origin tokened POST flips the registration both ways via the PRG
// redirect.
func TestAutostartToggleRoundTrip(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fake := &fakeAutostart{}
	srv.SetAutostart(fake)

	get := func() string {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/settings/mcp", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET /settings/mcp: %d", w.Code)
		}
		return w.Body.String()
	}
	body := get()
	if !strings.Contains(body, "launch at login") {
		t.Fatal("toggle card missing from wired settings page")
	}

	tok := mintToken(t, srv)
	rec := contactPOST(t, srv, "/settings/autostart", selfOrigin, tok, map[string]string{"enabled": "on"}, nil)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "autostart=ok") {
		t.Fatalf("enable POST: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if !fake.enabled || fake.enables != 1 {
		t.Fatalf("enable not recorded: %+v", fake)
	}

	rec = contactPOST(t, srv, "/settings/autostart", selfOrigin, tok, map[string]string{"enabled": "off"}, nil)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "autostart=ok") {
		t.Fatalf("disable POST: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if fake.enabled || fake.disables != 1 {
		t.Fatalf("disable not recorded: %+v", fake)
	}
}

// TestAutostartPOSTRejectsCrossOrigin: a cross-origin page must not be able
// to plant a login item (SPEC-0013 posture).
func TestAutostartPOSTRejectsCrossOrigin(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fake := &fakeAutostart{}
	srv.SetAutostart(fake)
	tok := mintToken(t, srv)
	rec := contactPOST(t, srv, "/settings/autostart", "http://evil.example", tok, map[string]string{"enabled": "on"}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST: got %d, want 403", rec.Code)
	}
	if fake.enables != 0 {
		t.Fatal("cross-origin POST must not reach the registration")
	}
}

// TestAutostartPOSTReportsError: a registration failure redirects back with
// the fixed-enum error banner state, never a 500 page.
func TestAutostartPOSTReportsError(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetAutostart(&fakeAutostart{err: errBoom{}})
	tok := mintToken(t, srv)
	rec := contactPOST(t, srv, "/settings/autostart", selfOrigin, tok, map[string]string{"enabled": "on"}, nil)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "autostart=error") {
		t.Fatalf("failing POST: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }
