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

5. **Spacing scale and one surface primitive (added 2026-08-20, issue #372).**
   Spacing is tokenised the way colour already was. `--space-1`…`--space-7`
   (4/8/12/16/20/24/32px) are the only spacing values; every padding, gap and
   radius on a bordered surface derives from them via three named tiers —
   `--surface-pad-dense` (list rows), `--surface-pad` (ordinary cards), and
   `--surface-pad-roomy` (the journal day card) — each paired with a matching
   radius token. Vertical rhythm comes from `--stack-gap` (between sibling
   cards) and `--section-gap` (between page sections).

   A single rule, the `.surface` primitive, carries border + radius +
   background + padding for every bordered surface; individual card classes
   compose from it and override only what genuinely differs. A surface needing
   a value off the scale means the scale is wrong: widen it rather than adding
   a bespoke number.

   **Why this became a decision.** The original ADR tokenised colour but not
   spacing, so each card class was written independently and drifted — eight
   card classes carried eight different paddings, and three of them
   (`.stat-cell` 1/1.1rem, `.home-card` 1.15/1.25rem, `.status-card`
   1.1/1.2rem) stacked directly on top of each other on Home. Differences of
   0.05–0.15rem are too small to read as hierarchy and large enough to make a
   column of cards look crooked. The scale's steps were chosen to match the
   rhythm the templates already used (`space-y-5`, `mb-8`), so adopting it is a
   de-duplication rather than a restyle.

6. **Constraints unchanged.** No Node at runtime (Tailwind standalone CLI + the
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
- Spacing is now a decision rather than a per-class convention. The guard tests
  in `internal/web/spacing_test.go` fail if a card class regrows a hand-rolled
  `rem` padding, if the scale is deleted, or if `app.css` is committed stale.
  Pills, badges, tabs and inputs keep their own small paddings — the scale
  governs bordered *surfaces*, not every element in the stylesheet.
- Several screens need small backend additions (pinned conversations, "on this
  day", search-elapsed timing); these are called out per issue in the epic.
