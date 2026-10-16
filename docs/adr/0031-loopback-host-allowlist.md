# ADR-0031: Pin the expected Host on state changes (loopback allowlist)

- **Status:** Proposed
- **Date:** 2026-10-16
- **Deciders:** Joe Stump, Jon Stump
- **Extends:**
  - [ADR-0010](0010-security-privacy-posture.md) — the loopback-only bind this
    depends on, and the warned non-loopback opt-out that decides where the pin
    applies.
- **Related:**
  - issue #41 / PR #525 — the same-origin guard whose baseline this pins; both
    deciding reviews on #525 (ids 4530, 4531) flagged this residual.
  - issue #526 — the DNS-rebinding path this closes.

## Context and Problem Statement

`sameOriginGuard` (#41) decides whether a mutating request came from this server
by deriving the server's own origin from the request itself:
`self := "http://" + r.Host`. Any check built that way can be answered by the
attacker. A page at `attacker.example` whose DNS A record resolves to
`127.0.0.1` makes the browser connect to the loopback socket, and the request
then carries `Host: attacker.example:8787`, `Origin: http://attacker.example:8787`
and `Sec-Fetch-Site: same-origin` — every layer agrees, because every layer is
derived from the same header. The blind POST #41 closes gets through.

This is not reachable today by anything worse than what an unauthenticated local
process could already do, which is why #525 shipped without closing it. It stops
being true the moment a higher-impact mutation lands, so it is closed now, while
the fix is one small middleware rather than a token ceremony retrofit.

The question the code cannot answer by itself: what should the server compare
`Host` against? ADR-0010 binds to loopback by default and only warns when the
operator overrides that, so a hardcoded loopback allowlist would refuse every
state change on a UI deliberately exposed to a LAN.

## Decision

Pin the expected `Host` instead of trusting the stated one, where ADR-0010's
posture holds:

1. A mutating request (`POST`, `PUT`, `PATCH`, `DELETE`, or any method a client
   invents) whose `Host` is not a loopback form — `127.0.0.1`, `localhost`, or a
   loopback address literal, with or without a port, IPv6 bracketed or bare,
   case-insensitive — is refused `400` before routing, ahead of the origin guard.
2. Safe methods (`GET`, `HEAD`, `OPTIONS`) are unaffected, matching #41: a read
   is not a state change, a cross-origin read is unreadable by the page that made
   it, and serving reads to whatever reaches the socket is the app's purpose.
3. When the operator has bound a non-loopback address — `Listen` already warns
   about exactly this — the pin stands down. A wildcard bind cannot know which
   address the user will type, and that exposure was never this guard's to cover:
   ADR-0010 assigns access control there to the operator, and rebinding buys
   nothing on a host the UI is openly reachable on.

## Considered Options

- **Origin/`Sec-Fetch-Site` only, as today.** Defeated by rebinding, because the
  baseline is request-derived. Rejected.
- **Loopback allowlist unconditionally.** Correct under the default posture, but
  it silently breaks the documented `--host` opt-out: every POST on a
  LAN-exposed UI starts answering 400 with no client-side workaround. Rejected.
- **Pin to the bound address.** Works for a specific bind (`127.0.0.2:8787`), and
  that is a subset of the loopback rule above; for a wildcard bind the bound
  address is not a value any client will send in `Host`. Rejected as strictly
  weaker than the loopback rule plus the opt-out.
- **Per-session tokens on every mutation.** Closes rebinding too, but ADR-0010
  chose loopback-over-auth deliberately; token ceremony on every route is a much
  larger change for the same exposure. Kept as the answer for the privileged
  POSTs that already carry one.

## Consequences

- The rebind path is closed for the default and desktop configurations, at the
  cost of one middleware and no client changes: curl, the desktop webview, and a
  browser at `127.0.0.1:8787` all send a loopback `Host` already.
- A mutating request that used to be judged on provenance headers is now refused
  earlier, so a test or tool that POSTs with an invented `Host` must name a
  loopback form. Existing tests moved to a `selfHost` constant for this.
- Off-posture (non-loopback) deployments get no *new* protection from this and
  keep the warning they already got; if UI authentication ever lands (ADR-0010's
  deferred item), it replaces both this pin and the origin guard's leniency about
  header-less callers.
- Residual, deliberately unaddressed: a rebound page can still issue *reads*, and
  any future mutation that is reachable over a non-loopback bind is unprotected by
  this pin. Both are ADR-0010's existing exposure, not a new one.
