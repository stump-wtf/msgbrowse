// Blanket same-origin guard for mutating routes (issue #41). ADR-0010 keeps the
// server loopback-only, no-auth and cookie-less, so classic CSRF has nothing to
// ride on today — but a page on any origin can already blind-POST to
// 127.0.0.1:8787, and the first higher-impact mutation (contact merge, fact
// edit, delete) or any exposure beyond loopback would turn that precedent into
// a real CSRF hole. The guard closes it cheaply: no tokens, no sessions.
//
// It lives in one middleware instead of per-route copies so a mutating route is
// covered the moment it is registered. Privileged POSTs (Setup, backups, device
// pairing, contact merge) keep the stricter checkSetupPOST gate on top of this:
// same-origin PLUS a per-session token, and zero provenance rejected. This
// blanket guard is deliberately looser on that one point — a request that states
// no provenance at all (curl, a CLI, a test) is allowed, because under ADR-0010
// anything that can reach the socket is already local, while a browser always
// states its origin on a state-changing request, which is the channel this
// closes.
//
// Governing: ADR-0010 (loopback-only, no-auth posture), issue #41.
package web

import (
	"net/http"
	"net/url"
)

// sameOriginGuard rejects a cross-origin state change with 403 before any
// handler runs. Safe methods (GET, HEAD, OPTIONS) pass through untouched: a read
// is not a state change, and serving reads to whatever reaches the loopback
// socket is the app's purpose.
func sameOriginGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mutatingMethod(r.Method) && !sameOriginOrUnknown(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// mutatingMethod reports whether a method is a state change rather than a read.
// RFC 9110 treats GET, HEAD and OPTIONS as safe; everything else — POST, PUT,
// PATCH, DELETE, and any method a client invents — is not.
func mutatingMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// sameOriginOrUnknown is the blanket guard's test: it rejects only what a client
// has actually identified as coming from somewhere else. A request that states
// nothing is not treated as an attack (see the package comment).
func sameOriginOrUnknown(r *http.Request) bool {
	same, stated := requestOrigin(r)
	return !stated || same
}

// requestOrigin reports whether r came from this server's own origin, and
// whether the client stated any provenance at all — stated false means no
// Origin, no Sec-Fetch-Site and no Referer, which is the curl-style caller.
//
// The layers mirror browser behavior:
//
//   - Origin present: it must equal the origin the client reached us on. A
//     cross-origin page's form or fetch POST carries its OWN Origin, which will
//     not match — that is exactly the request this rejects.
//   - No Origin but Sec-Fetch-Site present: only "same-origin" and "none" are
//     same-origin. "cross-site" and "cross-origin" are not; "same-site" (same
//     host, different port for an IP literal) is not same-origin either.
//   - Neither: fall back to Referer's origin.
//
// The server's own origin is derived from the request (Host plus the always-http
// loopback scheme, ADR-0010), so it is right for both the desktop shell's
// ephemeral port and `msgbrowse serve`'s configured loopback bind with no
// mode-specific configuration.
func requestOrigin(r *http.Request) (same, stated bool) {
	self := "http://" + r.Host

	if origin := r.Header.Get("Origin"); origin != "" {
		return origin == self, true
	}

	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true, true
	case "cross-site", "cross-origin", "same-site":
		return false, true
	}

	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		if err != nil {
			return false, true
		}
		return u.Scheme+"://"+u.Host == self, true
	}

	return false, false
}
