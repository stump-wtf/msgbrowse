# ADR-0012: Adopt the "slate" design system and a dense-log transcript

- **Status:** Accepted
- **Date:** 2026-06-28
- **Relates to:** [ADR-0006](0006-web-stack-htmx.md) (web stack/CSP), [ADR-0007](0007-frontend-styling-tailwind-daisyui.md) (Tailwind + daisyUI), [ADR-0010](0010-security-privacy-posture.md) (no CDN / self-hosted assets)
- **Supersedes (in part):** the visual-token and chat-bubble-transcript choices in [ADR-0007](0007-frontend-styling-tailwind-daisyui.md) (the bubble transcript is ADR-0007's component vocabulary + `internal/web/templates/partials.html`, not a SPEC-0004 requirement). [SPEC-0004](../openspec/specs/web-ui/spec.md) is superseded **visually only** — its behavioral requirements (chronological order, keyset paging) are kept by SPEC-0006.
- **Design source:** [docs/design/redesign-handoff.md](../design/redesign-handoff.md) + `docs/design/msgbrowse-redesign.dc.html`

## Context

A high-fidelity redesign handoff (the "Selected Direction") replaces the current
look. The shipped UI uses daisyUI's stock `dim`/`winter` themes and a **chat-
bubble** transcript; the redesign specifies a bespoke dark **"slate"** system
(base `#0f1216`, slate-blue accent `#6f93d6`) and a **dense-log** transcript
(timestamp gutter + colored sender rail + a 640px reading column), plus
redesigned Home, Search, Media, and a Journal screen. The handoff is the source
of truth for tokens, layout, and behavior.

Two questions had to be settled: (1) re-skin within the existing stack or migrate
off daisyUI, and (2) keep a light theme even though the brief is dark-only.

## Decision

1. **Stay on Tailwind + daisyUI ([ADR-0007](0007-frontend-styling-tailwind-daisyui.md)); implement slate as a custom daisyUI theme.**
   The handoff says to use the established framework. Define a daisyUI custom
   theme carrying the exact slate tokens as the **default (dark)**, and a
   **derived light variant** (`slate-light`) since the brief provides no light
   palette. Keep the header light/dark toggle ([ADR-0007](0007-frontend-styling-tailwind-daisyui.md)),
   re-pointed at the two slate themes.

2. **Hand-write CSS for the bespoke components.** The dense-log transcript, stat
   strip, result cards, editorial card, source pills, and presence dots are not
   daisyUI components. They live as small custom rules in
   `internal/web/tailwind/input.css` (alongside the existing lightbox/thumb
   rules), driven by the theme's CSS variables so both theme variants work. Where
   classes are chosen in Go, safelist them via `@source inline(...)` as today.

3. **Replace the chat-bubble transcript with the dense log.** Timestamp gutter
   (~76px, mono), a 3px sender-colored rail (accent for "Me"), and a 640px
   content column; day separators, centered system events, a faint accent wash
   on "Me" rows, and consecutive-sender grouping. This supersedes the bubble
   transcript in [SPEC-0004](../openspec/specs/web-ui/spec.md).

4. **Typography & numerals.** System sans for UI; system mono
   (`ui-monospace, …`) for timestamps, filenames, and counts; `tabular-nums` on
   all counts. No web fonts (preserves [ADR-0010](0010-security-privacy-posture.md)'s no-CDN posture).

5. **Constraints unchanged.** No Node at runtime (Tailwind standalone CLI + the
   committed `app.css`), server-rendered `html/template` + HTMX, strict CSP, and
   Heroicons (outline) inline SVG — all carry over from
   [ADR-0006](0006-web-stack-htmx.md)/[ADR-0007](0007-frontend-styling-tailwind-daisyui.md)/[ADR-0010](0010-security-privacy-posture.md).

## Consequences

- The redesign is built faithfully without a framework migration; daisyUI still
  provides primitives (drawer, menu, inputs, badges, tabs) while bespoke screens
  are custom CSS. The custom-CSS surface in `input.css` grows materially.
- The `slate-light` variant is **derived, not specified** — it is our
  interpretation of the dark tokens and may drift from a future designer-provided
  light palette.
- Moving off `dim`/`winter` and off chat bubbles is a visible, breaking UI change
  tracked as the **slate-redesign epic** (SPEC-0006); existing web tests that
  assert bubble markup (`chat-bubble`) will be updated per slice.
- The CSS drift guard still applies: rebuild `app.css` from a clean `.tools`
  cache before committing (see project memory / [ADR-0007](0007-frontend-styling-tailwind-daisyui.md)).
- Several screens need small backend additions (pinned conversations, "on this
  day", search-elapsed timing); these are called out per issue in the epic.
