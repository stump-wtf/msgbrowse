# ADR-0015: Onboarding — `doctor` diagnostics + orchestrated `export`/`sync`

- **Status:** Accepted
- **Date:** 2026-06-28
- **Relates to:** [ADR-0010](0010-security-privacy-posture.md) (egress / archive-only posture), [ADR-0005](0005-imessage-txt-parser.md) (iMessage source), [ADR-0014](0014-image-transcoding-external-converter.md) (external converter precedent)

## Context

Getting from "fresh install" to "populated, browsable archive" has sharp edges
the tool didn't help with. A real user ran `imessage-exporter` **without copy
mode**, so the `.txt` referenced original `~/Library/Messages/Attachments/...`
paths and **no media was in the archive** — `media` reported every image
"source-missing" with no explanation, and nothing told them their export was the
problem. msgbrowse historically only *reads* the two upstream exporters'
output; it never ran them and never touched sensitive sources (ADR-0010:
archive-only, "never read the macOS Keychain").

The user wants msgbrowse to "just handle this for people": diagnose the setup,
and optionally run the exports + the whole import pipeline.

## Decision

Add three onboarding commands.

1. **`doctor` — read-only diagnostics (no posture change).** Detects: data_dir/DB
   health + schema version; archive roots valid (incl. the "you passed
   `…/export` itself" mistake); **attachment health** — sampling stored image
   paths and flagging absolute/out-of-archive ones with the explicit
   "re-export with `-c/--copy-method`, then `import --full`" hint (the exact
   failure above); image converter presence + HEIC-pending count; un-embedded
   count; and whether `signal-export`/`imessage-exporter` are on PATH. Network is
   avoided except an opt-in `--check-llm` TCP probe to the one configured
   `llm.base_url` (no data sent).

2. **`export` — orchestrate the upstream exporters (the posture shift).** Runs
   `signal-export` and `imessage-exporter` with correct flags — notably iMessage
   **`-c clone`** so attachments are always bundled (eliminating the copy-mode
   trap). The tools are **required on PATH** (msgbrowse never auto-installs);
   their location may be overridden with `--signal-export-bin` /
   `--imessage-exporter-bin` (and config), and a missing tool is a clear error.
   A `--skip-on-error` flag continues past a failing source instead of aborting.

   This is the deliberate widening of ADR-0010: msgbrowse now *spawns* tools that
   read the Signal key (macOS Keychain) and need Full Disk Access to
   `~/Library/Messages`. msgbrowse itself **stores no secrets, reads no Keychain,
   and passes nothing sensitive** — it only invokes local, user-installed tools
   on the user's own machine, at the user's explicit request. It remains
   local-only (the exporters perform no network egress).

3. **`sync` — the all-in-one pipeline.** Chains export → import (both sources) →
   media (transcode) → embed → facts, with `--skip-on-error` and per-stage skip
   flags (e.g. `--no-export`, `--no-embed`). LLM-dependent stages (embed/facts)
   warn-and-continue when no endpoint is reachable. The granular commands stay
   for fine control.

## Consequences

- Onboarding becomes one command (`sync`) or a guided `doctor`; the copy-mode
  class of bug is detected and prevented.
- New **optional** runtime dependencies (`signal-export`, `imessage-exporter`),
  required only for `export`/`sync --export`; everything else still works without
  them. Consistent with ADR-0014's "optional external tool, detected on PATH,
  clear error if absent" precedent.
- The archive-only/no-Keychain posture is widened — but only via consensual
  invocation of the user's own tools, with no secret storage, and documented
  here + in SECURITY.md.
- `doctor` is a safe first slice; `export`/`sync` follow. Requirements:
  [SPEC-0007](../openspec/specs/onboarding/spec.md).
