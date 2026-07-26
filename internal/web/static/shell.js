// App-shell behaviors for the #190 full-width-header layout. Self-hosted
// (served from /static under script-src 'self') so it runs under the strict
// CSP — no inline handlers.
//
// 1. Header-tab active state. The Messages/Media tabs live in the page_start
//    shell, which boosted partial renders never re-render — the server marks
//    the active tab (.NavTab) only on full loads. syncTabs() re-derives the
//    state from location.pathname after every boosted swap / history move,
//    exactly like sidebar.js's syncActive does for conversation rows:
//      - "/" and every /c/* transcript read as Messages;
//      - /media (the tab's URL) and /gallery* (the canonical media surface it
//        aliases — gallery tab/filter links stay on /gallery) read as Media;
//      - /journal reads as Journal (#238); its year/month/day navigation is
//        query-param only, so the pathname match needs no prefix;
//      - every other route (Search, Settings, …) activates none.
//    Keep in lockstep with baseData.NavTab (internal/web/handlers.go).
//
// 2. Infinite-scroll keep-alive. htmx 2.0.4 re-checks hx-trigger="revealed"
//    sentinels only after a *window* scroll/resize event sets its dirty flag,
//    but the #190 shell moves all scrolling off the window into inner
//    containers (#main-content, the sidebar list) whose scroll events do not
//    bubble. The capture-phase listener below (capture, because scroll does
//    not bubble) forwards inner scrolls as a synthetic window scroll so the
//    transcript/gallery load-more sentinels keep firing. Document scrolls are
//    excluded: those already reach window natively, and the synthetic event —
//    dispatched ON window, so it never re-enters this document-level capture
//    listener — must not double-fire for them.
//
// 3. Back/forward reading position (#197 review). htmx's history restore
//    re-applies only the *window* scroll (window.scrollTo), but this shell
//    scrolls #main-content, so Back into a long transcript landed at the
//    top. saveScroll stashes the scroller's position in sessionStorage on
//    htmx:beforeHistorySave, keyed by the event's detail.path — htmx's own
//    path-for-history — NOT by location: when htmx handles popstate it saves
//    the page being LEFT after the URL has already flipped to the
//    destination, so a location-derived key would clobber the destination's
//    saved offset with the departing page's (live-reproduced during #197
//    verification). restoreScroll re-applies the destination's offset after
//    htmx swaps the snapshot back in — on htmx:historyRestore ONLY, which
//    htmx 2.0.4 fires after the swap on both history-cache paths (hit, and
//    miss via its server refetch). It must NOT also run on popstate: that
//    fires before htmx's own popstate handler (shell.js loads first), while
//    the DEPARTING page is still mounted but location already names the
//    destination — so it would scroll the departing page to the
//    destination's offset (clamped), which htmx:beforeHistorySave then
//    persists under the departing page's key, corrupting both entries on
//    every traversal (the back/forward/back corruption fixed after #197).
//    Boosted forward navigations never restore: each swap is a fresh
//    scroller that intentionally lands at the top (#190).
(function () {
  "use strict";

  function syncTabs(name) {
    var tabs = document.querySelectorAll(".header-tabs [data-nav-tab]");
    var active = null;
    for (var i = 0; i < tabs.length; i++) {
      var tab = tabs[i];
      var on = tab.getAttribute("data-nav-tab") === name;
      tab.classList.toggle("header-tab-active", on);
      if (on) {
        tab.setAttribute("aria-current", "page");
        active = tab;
      } else {
        tab.removeAttribute("aria-current");
      }
    }
    // Below ~360px the three tabs no longer fit and the strip becomes a
    // hidden-scrollbar scroll container (input.css narrow-width tier 1). Keep
    // the ACTIVE tab in view so the page you are on is never the tab scrolled
    // off the edge — otherwise the narrowest viewports would hide Journal, the
    // tab #238 exists to expose. "nearest" scrolls only the overflowing strip
    // and only when it actually overflows: it is a no-op at every width where
    // the tabs fit, and it never scrolls the page or #main-content.
    if (active && active.scrollIntoView) {
      active.scrollIntoView({ block: "nearest", inline: "nearest" });
    }
  }

  function sync() {
    var path = window.location.pathname;
    var name = "";
    if (path === "/" || path.indexOf("/c/") === 0) {
      name = "messages";
    } else if (path === "/media" || path.indexOf("/gallery") === 0) {
      name = "media";
    } else if (path === "/journal") {
      name = "journal";
    }
    syncTabs(name);
  }

  // Same event set sidebar.js uses for its active-row sync: after every
  // boosted swap settles (hx-push-url has updated location by then), on
  // back/forward, and after htmx rebuilds the body from a history snapshot.
  document.addEventListener("htmx:afterSettle", sync);
  window.addEventListener("popstate", sync);
  document.addEventListener("htmx:historyRestore", sync);
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", sync);
  } else {
    sync();
  }

  document.addEventListener(
    "scroll",
    function (e) {
      if (e.target !== document) window.dispatchEvent(new Event("scroll"));
    },
    true
  );

  function scrollKey(path) {
    return (
      "msgbrowse:scroll:" +
      (path || window.location.pathname + window.location.search)
    );
  }

  function saveScroll(e) {
    var main = document.getElementById("main-content");
    if (!main) return;
    var path = e && e.detail && e.detail.path;
    try {
      sessionStorage.setItem(scrollKey(path), String(main.scrollTop));
    } catch (err) {
      // Storage unavailable (private mode/quota): reading position is lost,
      // navigation still works.
    }
  }

  function restoreScroll() {
    var main = document.getElementById("main-content");
    if (!main) return;
    var saved = null;
    try {
      saved = sessionStorage.getItem(scrollKey());
    } catch (err) {
      return;
    }
    if (saved !== null) main.scrollTop = parseInt(saved, 10) || 0;
  }

  document.addEventListener("htmx:beforeHistorySave", saveScroll);
  document.addEventListener("htmx:historyRestore", restoreScroll);
})();
