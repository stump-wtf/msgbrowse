---
status: approved
date: 2026-06-27
implements: [ADR-0006, ADR-0007]
---

# SPEC-0004: Web UI

> **Note:** superseded **visually** by [SPEC-0006](../web-ui-redesign/spec.md); the behavioral requirements below (chronological order, keyset paging, jump-to-context, safe rendering) still hold.

- **Capability:** web-ui
- **Source packages:** `internal/web` (`server.go`, `handlers.go`, `media.go`, `gallery.go`, `render.go`, `search.go`, `templates/`)
- **Related ADRs:** [ADR-0006](../../../adr/0006-web-stack-htmx.md), [ADR-0007](../../../adr/0007-frontend-styling-tailwind-daisyui.md), [ADR-0010](../../../adr/0010-security-privacy-posture.md), [ADR-0003](../../../adr/0003-dual-source-archive.md), [ADR-0005](../../../adr/0005-imessage-txt-parser.md)

## Overview

msgbrowse ships a server-rendered HTMX web UI: `net/http` with Go 1.22 pattern
routing, `html/template` (auto-escaping), HTMX for partial updates, daisyUI/Tailwind
styling, no SPA and no runtime build step (ADR-0006). It MUST bind loopback by
default, set a strict Content-Security-Policy and related headers, escape all
untrusted content, and serve archive media safely and source-aware.

## Requirements

### REQ-0004-001: Browse by conversation with transcript

The UI MUST list conversations in a sidebar (most-recent-activity first, with a
last-message preview, message count, and a deterministic monogram avatar) and MUST
render a selected conversation's transcript (`/c/{id}`) in chronological order. The
transcript MUST paginate via a keyset cursor on `(ts_unix, id)`.

#### Scenario: Conversation list and transcript
- **Given** ingested conversations
- **When** the index and a conversation page are requested
- **Then** the sidebar lists conversations newest-activity-first and the conversation page shows its messages oldest-first.

### REQ-0004-002: HTMX infinite scroll

The transcript MUST load further pages by HTMX when the load-more sentinel is
revealed (`hx-trigger="revealed"`), requesting `/c/{id}/messages?after_ts=...&after_id=...`
and swapping the rendered partial in place. The feature MUST degrade gracefully and
keyset cursors MUST avoid duplicate or skipped messages.

#### Scenario: Scrolling loads the next page
- **Given** a conversation with more messages than one page
- **When** the load-more sentinel is revealed
- **Then** HTMX fetches the next keyset page and appends it without duplicates.

### REQ-0004-003: Media and links gallery

The UI MUST provide a gallery (`/gallery`) with three tabs — images, files, links —
filterable by conversation, source, and date range, showing per-tab counts. File
entries MUST be decorated with on-disk size and content type computed on demand
from the read-only archive, and a file that cannot be stat'd MUST still render
(without size/type) rather than failing the listing. Links MUST be deduplicated by
URL, grouped by domain, and carry an occurrence count and the earliest message they
appeared in.

#### Scenario: Links are deduplicated and grouped
- **Given** the same URL posted multiple times
- **When** the links tab renders
- **Then** the URL appears once, grouped under its domain, with its total occurrence count and earliest-occurrence message.

#### Scenario: A missing file still lists
- **Given** a file attachment whose file is missing or renamed on disk
- **When** the files tab renders
- **Then** the row still appears, just without size/type, and the listing does not error.

### REQ-0004-004: Status page

The UI MUST provide a status page (`/status`) showing archive freshness
(conversation count, message count, newest message timestamp), the latest ingest
run summary, and the snapshot inventory with per-snapshot size, retention tier, and
total footprint. The page MUST state that snapshots are encrypted backups that
msgbrowse lists but never opens or decrypts.

#### Scenario: Status reflects last ingest and snapshots
- **Given** a recorded ingest run and a snapshot inventory
- **When** `/status` renders
- **Then** it shows the run's counts/duration and lists each snapshot with size and tier and the total footprint.

### REQ-0004-005: Loopback-only bind by default

The server MUST bind a loopback address by default (`127.0.0.1:8787`). Binding to a
non-loopback interface MUST be possible only by explicit configuration and MUST
emit a warning that the UI has no authentication (ADR-0010).

#### Scenario: Non-loopback bind warns
- **Given** a configured non-loopback listen address
- **When** the server starts
- **Then** it logs a warning that the UI is unauthenticated.

### REQ-0004-006: Strict CSP and security headers

Every response MUST carry a strict Content-Security-Policy (`default-src 'none'`
with only same-origin `script-src`/`style-src`/`connect-src`/`font-src`, `img-src
'self' data:`, `base-uri 'none'`, `form-action 'self'`, `frame-ancestors 'none'`)
plus `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, and
`X-Frame-Options: DENY` (ADR-0010). Scripts and styles MUST be self-hosted (HTMX,
theme.js, app.css) so nothing loads from a third party (ADR-0006).

#### Scenario: Every response is hardened
- **Given** any request to the UI
- **When** the response is sent
- **Then** it carries the strict CSP and the nosniff / no-referrer / DENY headers.

### REQ-0004-007: Untrusted-content escaping

Message bodies are untrusted and MUST always be escaped (ADR-0010). `renderBody`
MUST escape all plain text (newlines to `<br>`), drop image Markdown (images render
as thumbnails), render Markdown links to URLs and bare URLs as anchors with
`rel="noopener noreferrer nofollow"` and `target="_blank"`, drop Markdown links to
media files, and never allow message content to inject markup or load/run external
resources.

#### Scenario: Markup in a body cannot inject
- **Given** a message body containing `<script>` and a bare URL
- **When** it is rendered in the transcript
- **Then** the `<script>` is escaped as text and the URL becomes a safe rel-protected anchor.

### REQ-0004-008: Path-traversal-safe, source-aware media serving

`/media/{id}/{path...}` MUST resolve the attachment under the archive for the
conversation's source — Signal: `<archive>/export/<conv>/<rel>`, iMessage:
`<imessage_archive>/<rel>` — through a containment check that neutralizes `..` and
rejects any path escaping the base directory (ADR-0003, ADR-0005, ADR-0010). It
MUST serve images inline, force download for non-images, and explicitly force
download for SVG (which can carry script) even where the image map excludes it.

#### Scenario: Traversal is rejected
- **Given** a media request whose relative path attempts `../../etc/passwd`
- **When** the handler resolves it
- **Then** the cleaned path is rejected with 400 and no file outside the base is served.

#### Scenario: SVG is never served inline
- **Given** a media request for a `.svg` file inside the archive
- **When** it is served
- **Then** it is sent as an attachment (download), not inline.

#### Scenario: Source selects the base directory
- **Given** an iMessage conversation's attachment
- **When** its media URL is requested
- **Then** the path is resolved under the iMessage archive root, not the Signal export tree.

### REQ-0004-009: daisyUI theming

The UI MUST apply daisyUI/Tailwind theming with a persisted light/dark theme
applied before paint (no flash of unstyled content) via a self-hosted `theme.js`
under `script-src 'self'`, and a theme toggle (ADR-0007).

#### Scenario: Saved theme applies without flicker
- **Given** a previously selected theme
- **When** a page loads
- **Then** the theme is applied before first paint by the self-hosted theme script.

### REQ-0004-010: Pipeline status lives in Settings, on one tab per pipeline

Every derived-data pipeline — semantic-search embeddings, journal digests, contact
facts, sentiment scoring — MUST expose its status and its build controls **only**
under Settings, and MUST have its **own tab**. There MUST be a Settings tab for
semantic-search / embeddings indexing and a separate Settings tab for journal
indexing; the two MUST NOT share one page, because their coverage figures, models,
run histories and costs are unrelated and reading them interleaved is what a
single combined "Status" tab produces.

Reading surfaces MUST NOT render pipeline status. Specifically, the Home page
(`/`) MUST NOT render the semantic-index card or any build control, and `/journal`
is governed by [SPEC-0016 REQ-0016-017](../journal/spec.md). Pipeline status MUST
NOT be duplicated across surfaces: each card renders on exactly one tab.

Run-history presentation MUST come from a single shared template `define`
parameterised per pipeline, so the pipelines' cards cannot drift apart.

#### Scenario: Home carries no index card
- **Given** an archive with a partially built semantic index
- **When** the user opens `/`
- **Then** the page renders reading content only, and the index coverage, progress and controls appear solely on the Settings search-index tab.

#### Scenario: The two indexes are separate tabs
- **Given** a user who wants to rebuild journal digests
- **When** they open Settings
- **Then** the journal tab carries digest coverage, chat model, built-through and Build / Rebuild, and the search-index tab carries embedding coverage, embed model and Build / Reset, with neither tab showing the other's figures.

### REQ-0004-011: LLM model selection is always a discovered dropdown — never free text

Any UI field that selects an LLM model MUST be a `<select>` whose options come from
the configured endpoint's `/v1/models` listing. A model name MUST NOT be accepted as
free text anywhere in the UI — not as a primary control, not as a fallback, and not
as an "or type your own" affordance. A `<datalist>`-backed text input does not
satisfy this requirement: it is a text input.

The currently-configured value MUST be preserved as a selected option even when the
endpoint's listing does not contain it (a config file or `MSGBROWSE_LLM_*`
environment variable may have set it, or the endpoint may have dropped the model),
and MUST survive a form save unchanged. Where a model is optional — an empty embed
model turns semantic search off — the "off" state MUST be an explicit option rather
than an empty field.

When discovery fails (endpoint unreachable, `/v1/models` unsupported, or an empty
listing) the control MUST degrade to a **disabled select** with an explanatory
banner and a retry, never to a text input; the saved value MUST remain displayed
and MUST NOT be cleared by saving the form.

Client-side control choice is not validation: the handler MUST reject a submitted
model identifier that is neither in the discovered listing nor the currently-saved
value, rather than writing an arbitrary string into the config file.

> **Why.** A typed model name is only discovered to be wrong at the next LLM call.
> For the journal that means a multi-thousand-call build that fails hours in, and
> for semantic search it means an index that silently never builds.

#### Scenario: Discovery unavailable still yields a dropdown
- **Given** an endpoint whose `/v1/models` returns 404
- **When** the LLM settings page renders
- **Then** the model fields render as disabled `<select>` controls showing the currently-saved model, with an explanatory banner and a refresh control, and no text input is present.

#### Scenario: A configured model absent from the listing survives a save
- **Given** a chat model set by environment variable that the endpoint does not list
- **When** the user saves the LLM settings form without changing the model
- **Then** the configured model is retained, and it is shown as the selected option marked as not currently offered.
