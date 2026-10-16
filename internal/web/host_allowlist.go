// Host allowlist for mutating routes (issue #526), the residual #525's deciding
// reviews both flagged. sameOriginGuard derives the server's own origin from the
// request (`self := "http://" + r.Host`), which is sound against an ordinary
// cross-origin page but not against DNS rebinding: a page at attacker.example
// whose A record resolves to 127.0.0.1 reaches this server on the loopback
// socket, and the browser then sends Host: attacker.example:8787 and
// Origin: http://attacker.example:8787 — so `self` equals the request's own
// Origin by construction, Sec-Fetch-Site comes back same-origin, and the blind
// POST #41 closes gets through. Any check that asks the request who the server
// is can be answered by the attacker.
//
// The fix is to stop asking: ADR-0010 guarantees the bind is loopback, so the
// expected Host is known in advance and this middleware pins it. A mutating
// request whose Host is not a loopback form is refused with 400 before routing,
// whatever its Origin, Sec-Fetch-Site or Referer claim.
//
// Scope mirrors sameOriginGuard: safe methods (GET, HEAD, OPTIONS) are not state
// changes, a cross-origin read is unreadable by the page that made it, and
// serving reads to whatever reaches the socket is the app's purpose. The pin runs
// ahead of the origin guard, so a rebound request is refused rather than argued
// with.
//
// Where the operator has opted out of ADR-0010's posture — an explicit
// non-loopback bind, warned about by Listen — the expected Host is no longer
// knowable (a wildcard bind is reached by whatever address the user typed), so
// the pin stands down and that exposure stays covered by the operator's own
// access control, exactly as ADR-0010 describes it. That opt-out is also where
// rebinding stops buying the attacker anything: the rebound name would have to
// resolve to an address the UI is actually reachable on, and that UI already has
// no authentication there.
//
// Governing: ADR-0010 (loopback-only posture), issue #41 (the origin guard this
// pins), issue #526, ADR-0031 (proposed: pin the expected Host, not the stated one).
package web

import (
	"net"
	"net/http"
	"strings"
)

// hostAllowlistGuard refuses a mutating request whose Host is not a loopback
// form. It reads the bind at request time, so a server constructed by a test (no
// listener, the posture's default) is pinned.
func (s *Server) hostAllowlistGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mutatingMethod(r.Method) && s.hostPinEnforced() && !loopbackHostForm(r.Host) {
			http.Error(w, "request Host must name a loopback address", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostPinEnforced reports whether the expected Host is knowable in advance,
// which is true whenever the server is bound where ADR-0010 says it is bound.
func (s *Server) hostPinEnforced() bool { return !s.boundNonLoopback }

// loopbackHostForm reports whether a Host header value names the loopback
// interface: "localhost" or a loopback address literal, with or without a port,
// bracketed or bare. Anything else — a hostname, a routable address, an empty
// Host — is not a form this server can be addressed by under ADR-0010.
func loopbackHostForm(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	// SplitHostPort handles both "127.0.0.1:8787" and "[::1]:8787"; a value with
	// no port (or a bare unbracketed IPv6, which no correct client sends) fails
	// it and is used as-is.
	if name, _, err := net.SplitHostPort(h); err == nil {
		h = name
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
