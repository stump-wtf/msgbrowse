---
status: approved
date: 2026-06-28
implements: [ADR-0015]
---

# SPEC-0007: Onboarding (doctor, export, sync)

- **Capability:** onboarding
- **Source packages:** `internal/cli` (`doctor.go`, `export.go`, `sync.go`), `internal/store` (`OpenReadOnly`), `internal/ingest`, `internal/imessage`, `internal/imageconv`, `internal/embed`, `internal/facts`
- **Related ADRs:** [ADR-0015](../../../adr/0015-onboarding-doctor-export-sync.md), [ADR-0010](../../../adr/0010-security-privacy-posture.md), [ADR-0005](../../../adr/0005-imessage-txt-parser.md), [ADR-0014](../../../adr/0014-image-transcoding-external-converter.md)

## Overview

Onboarding turns a fresh install into a populated, browsable archive with minimal
friction: a read-only `doctor` that diagnoses setup, an `export` that orchestrates
the upstream exporters with correct flags, and an all-in-one `sync`. Orchestration
runs the user's own local tools (no secrets stored, no network egress beyond the
single configured LLM endpoint); see ADR-0015 for the posture decision.

## Requirements

### REQ-0007-001: `doctor` read-only diagnostics
`doctor` MUST report setup health WITHOUT creating or modifying anything: data_dir
+ DB presence and on-disk schema version (opened read-only, never migrated),
archive-root validity (including the "pointed at export/ itself" mistake),
attachment health (flagging absolute/out-of-archive paths with the copy-mode
re-export hint), image-converter presence, un-embedded count, and whether the
upstream exporters are on PATH. It MUST make no network calls except an opt-in
`--check-llm` TCP probe to the configured `llm.base_url`. Exit non-zero only when a
check fails.

#### Scenario: typo'd data_dir is reported, not created
- **Given** `--data-dir` points at a nonexistent path
- **When** `doctor` runs
- **Then** it reports the dir doesn't exist and creates nothing on disk.

*(Implemented in PR #43.)*

### REQ-0007-002: `export` orchestrates the upstream exporters
`export` MUST run `signal-export` and/or `imessage-exporter` into the configured
archive roots with correct flags — iMessage MUST use copy mode (`-c clone` or
equivalent) so attachments are bundled. Each tool MUST be located on PATH or via
an explicit override (`--signal-export-bin` / `--imessage-exporter-bin`, or
config); a tool that is required but absent MUST be a clear error naming the
missing tool and how to install it. A `--skip-on-error` flag MUST let a failing
source be logged and skipped instead of aborting the run. msgbrowse MUST NOT store
secrets or read the Keychain itself (the invoked tool does, with user consent).

#### Scenario: missing exporter is a clear error
- **Given** `imessage-exporter` is not on PATH and no `--imessage-exporter-bin`
- **When** `export` runs for iMessage
- **Then** it errors naming the missing tool + install hint (or, with `--skip-on-error`, warns and continues with the other source).

#### Scenario: iMessage export bundles attachments
- **Given** `export` runs `imessage-exporter`
- **Then** it passes copy mode so attachment files land under the archive (avoiding the reference-only/absolute-path trap).

### REQ-0007-003: `sync` end-to-end pipeline
`sync` MUST chain export → import (both sources) → media transcode → embed →
facts, honoring `--skip-on-error` and per-stage skip flags (e.g. `--no-export`,
`--no-embed`, `--no-facts`). LLM-dependent stages (embed, facts) MUST warn and
continue when no endpoint is reachable rather than failing the whole run. Each
stage MUST reuse the existing command logic (ingest/imessage/imageconv/embed/facts),
not reimplement it.

#### Scenario: one-command refresh
- **Given** configured archive roots and exporters on PATH
- **When** `sync` runs
- **Then** archives are re-exported, imported, HEIC transcoded, and (if an LLM is reachable) embeddings + facts updated — in one command.

#### Scenario: degrades without an LLM
- **Given** no reachable `llm.base_url`
- **When** `sync` runs
- **Then** export/import/media complete and embed/facts are skipped with a warning, exit success.
