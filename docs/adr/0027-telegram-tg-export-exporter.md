# ADR-0027: Telegram extraction via a purpose-built `tg-export` exporter (supersedes ADR-0022)

- **Status:** Accepted
- **Date:** 2026-07-30 (pivot decided 2026-07-08; recorded here after the fact)
- **Deciders:** Joe Stump
- **Supersedes:** [ADR-0022](0022-telegram-source-delegated-exporter.md)
- **Related:** [ADR-0020](0020-bundled-exporters-guided-setup.md), [ADR-0016](0016-whatsapp-source-exporter.md), [ADR-0010](0010-security-privacy-posture.md), [SPEC-0015](../openspec/specs/telegram-source/spec.md) (telegram-source — revision tracked in #296)

## Context and Problem Statement

ADR-0022 delegated Telegram extraction to `tdl` (iyear/tdl) on the
load-bearing claim that its export command produces machine-readable JSON
with "per-message id, date, sender, text, media references". Verification
against `tdl`'s source refuted that claim before the implementation started:

- The export message shape in `app/chat/export.go` is
  `Message{ID, Type, File, Date?, Text?, Raw?}` — **there is no sender field
  in any mode**. `Date` and `Text` are emitted only with `--with-content`.
- `tdl` says so itself at runtime:
  *"WARN: Export only generates minimal JSON for tdl download, not for
  backup."* Its export exists to feed its own media downloader, not to back
  up conversations.
- The only way to obtain sender attribution is `--raw`, which embeds the
  raw MTProto `tg.Message` — parsing that would drag Telegram's wire-protocol
  churn (peer-id resolution, entity tables, layer changes) directly into
  msgbrowse, the exact idiom violation ADR-0022 itself rejected when it
  refused an in-house MTProto client.

A conversation archive without senders is not an archive. The tool selection
in ADR-0022 is therefore refuted; the delegation idiom it reinstated —
**msgbrowse never writes exporters** — stands untouched and governs the
replacement.

ADR-0022 merged (PR #207) with the refutation already known, and the
replacement was scoped but never executed. This ADR records both the
refutation and the standing replacement decision so the decision record on
`main` matches reality.

## Decision Drivers

Unchanged from ADR-0022: the exporter-delegation idiom, no credential
custody by msgbrowse (SPEC-0013), bundled-toolchain fit (ADR-0020),
onboarding coherence (SPEC-0013), and maintenance reality. Plus, made
explicit by the refutation:

* **Ingestion-fit output is the selection criterion.** The delegate's output
  must carry what conversation reconstruction needs — senders, timestamps,
  text, service events, media references — in a stable, documented shape.
  Popularity and maintenance cadence do not compensate for a schema that
  omits the sender.

## Considered Options

* **Delegate to `tdl`** — refuted: no sender field outside `--raw` (above).
* **Delegate to `tdl --raw`** — sender present but inside raw MTProto
  structures; msgbrowse would own wire-format churn. Idiom violation.
* **Parse Telegram Desktop's native in-app export** — full fidelity but a
  manual in-app dump per refresh; already rejected in ADR-0022 review.
* **Existing Telethon-based exporters** — effectively unmaintained or tiny
  (ADR-0022's survey stands).
* **Build `tg-export`, a purpose-built sibling exporter, and delegate to
  it** — chosen.

## Decision Outcome

Chosen option: **build and delegate to `tg-export`**, a small, standalone,
Joe-owned exporter CLI on **Telethon** (Python ≥ 3.11, MIT), bundled by
msgbrowse exactly as `signal-export` and `whatsapp-chat-exporter` are
(ADR-0020's Python-venv lane), and invoked with explicit argv on
Enable/Refresh.

Telethon owning the MTProto and session surface is appropriate *in a
dedicated exporter* — precisely how `signal-export` owns Signal Desktop's
key material — and keeps msgbrowse a reader of files on disk. What no
off-the-shelf tool offers, `tg-export` provides by design: an
ingestion-shaped contract with senders (`from.id` / `from.name` /
`from.is_self`), service events, reactions, media at relative paths, and
true incremental refresh via per-chat `max_message_id` anchors.

The v1 contract (schema_version 1: `manifest.json` + per-chat NDJSON +
`media/` tree; `login` / `export --since` / `chats` / `doctor` commands;
takeout-session bulk export; sentinel exit classification for
re-authorization guidance) is specified in the build brief
(https://claude.ai/code/artifact/a03e1cd5-b068-4651-8648-0114bfdc02ce) and
moves into durable repo docs — the `tg-export` repo and a revised SPEC-0015 —
as the pivot is executed. Execution state and the spec revision are tracked
in #296; **as of 2026-07-30 the `tg-export` repo does not exist yet**, and
SPEC-0015 carries a premise-refuted banner until its revision lands.

### Consequences

* Good, because the delegation idiom holds four sources wide, now with a
  delegate whose output is designed for ingestion rather than adapted to it.
* Good, because incremental refresh is real (per-chat id anchors), senders
  and service events are first-class, and NDJSON streams on both sides for
  multi-GB accounts.
* Good, because bundling reuses the existing ADR-0020 Python-venv lane —
  no new bundling machinery.
* Bad, because we now own an exporter: Telegram protocol churn arrives as
  `tg-export`/Telethon maintenance instead of a third-party pin bump. The
  mitigation is the same discipline the idiom prescribes — the churn lives
  in the dedicated tool, never in msgbrowse's parser.
* Bad, because network egress to Telegram's API during user-initiated
  Enable/Refresh remains (ADR-0022's ADR-0010 amendment carries over
  unchanged), and the one-time interactive login (code, optional 2FA) is
  honest friction the guided flow must own.
* Bad, because secret chats are out of scope by construction (device-bound
  E2E; not reachable via MTProto client APIs) — documented, not attempted.

### Confirmation

[SPEC-0015](../openspec/specs/telegram-source/spec.md) (as revised under #296) governs requirements. Compliance is
confirmed by: the delegation invariant asserted in the spec; parser unit
tests over synthetic fixtures of `tg-export`'s NDJSON contract
(schema_version-pinned); detection/authorization/guidance tests in
`internal/setup`; doctor coverage; venv bundle pinning per ADR-0020; and the
standard `CGO_ENABLED=0` build/vet/test gates on the msgbrowse side.

## More Information

The refutation evidence, verified 2026-07-30 directly against
https://github.com/iyear/tdl `app/chat/export.go`: the exported `Message`
struct carries `id`, `type`, `file`, optional `date`/`text`, optional `raw`
— no sender in any combination short of `--raw`'s full `tg.Message` embed.
ADR-0022 is retained unmodified below its superseded status line as the
record of the original selection and of the delegation idiom's rationale.
