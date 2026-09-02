---
status: draft
date: 2026-08-26
implements: [ADR-0030]
extends: [SPEC-0002]
---

# SPEC-0019: Late-interaction semantic search

- **Capability:** late-interaction-search
- **Target packages:** `internal/store` (`vector.go`, `schema.go` migration,
  MaxSim scoring), `internal/embed` (ColBERT encoder wiring), `internal/web`
  (Status surface), `cmd/msgbrowse` (`embed` flags)
- **Extends:** [SPEC-0002 (Search)](../search/spec.md) — replaces the retrieval
  mechanism behind REQ-0002-004/005; keyword search, hybrid fusion, and the
  web/MCP result contract are unchanged.
- See [ADR-0030](../../../adr/0030-late-interaction-retrieval.md) for the why.

## Overview

Semantic search moves from one pooled vector per message to a ColBERT-style
late-interaction index: each message stores one embedding per token, and queries
are scored with MaxSim in Go. The encoder is a locally-run model — not the hosted
OpenAI-compatible endpoint, which cannot emit multi-vectors. All existing
filters, caps, provenance, and observability surfaces from SPEC-0002 carry over.

## Requirements

### Requirement: Multi-vector storage with model coexistence

The store MUST persist per-token embeddings as a quantized multi-vector blob
keyed by `(message_hash, model)`, exactly mirroring the schema-v3 coexistence
rule: switching models MUST NOT require deleting the previous model's vectors.
Each token vector MUST be stored at **int8** precision per dimension (~130 B per
token at 128 dimensions, including per-vector norm bookkeeping) with a documented
layout. Messages whose embeddings are missing or stale for a model MUST be
discoverable the same way `MessagesNeedingEmbedding` does today.

#### Scenario: Model switch keeps both indexes

- **Given** messages embedded under model `colbertv2`
- **When** a new model `colbert-v3` is configured and backfilled
- **Then** both models' vectors remain queryable and no `colbertv2` rows are deleted.

#### Scenario: Storage growth is bounded

- **Given** a message of at most 256 tokens
- **When** its multi-vector is written
- **Then** the blob is at most 32 KiB (~130 B/token at 128 dimensions).

Per-message cost tracks token count, where today's pooled vector is a flat 6 KiB
regardless of length. Break-even is around 47 tokens (6 KiB ÷ ~130 B): shorter
messages — the large majority of an archive — cost *less* than the index they
replace, and only long ones cost more.

### Requirement: Local late-interaction encoder

Embedding generation for late-interaction search MUST run against a locally
hosted encoder endpoint (OpenAI-compatible batch interface or an equivalent),
never the public hosted embeddings API used by the legacy single-vector path.
Tokenization and the `[Q]`/`[D]` marker convention MUST match what the scoring
code assumes; chunking MUST NOT split inside a message body.

#### Scenario: Corpus embed runs offline

- **Given** `msgbrowse embed --model colbertv2` runs against the local gateway
- **When** the run completes
- **Then** every non-system, non-empty message has a multi-vector row for that model.

#### Scenario: Encoder unavailable fails loudly

- **Given** the local encoder endpoint is unreachable
- **When** an embed run starts
- **THEN** the run records a failed state naming the endpoint, and the corpus is
  left consistent (no partial-message vectors).

### Requirement: MaxSim ranking with existing filters

Semantic search MUST score candidates with MaxSim (per query token, best cosine
against document tokens, summed) over the same filter set as today
(conversation, source, sender, time bounds) and the same default/max caps.
Scores MUST be monotonically comparable within one query so ordering is stable;
the zero-hit behavior on empty queries MUST be preserved from SPEC-0002.

#### Scenario: Token-level match beats pooled similarity

- **Given** two messages where the query's rare token appears verbatim in only one
- **When** semantic search runs
- **THEN** the verbatim-match message outranks the pooled-similar one.

#### Scenario: Filters still apply

- **Given** a query restricted to one conversation
- **When** MaxSim runs
- **THEN** no candidate outside that conversation is scored or returned.

### Requirement: Coexistence and cutover

During migration, hybrid search MUST keep working: it MAY rank single-vector
results alongside late-interaction results via the existing RRF fusion until the
multi-vector index is complete, after which the store configuration names exactly
one active semantic backend. Reverting to the legacy backend MUST remain possible
without data loss while any model's vectors exist.

#### Scenario: Hybrid works mid-backfill

- **Given** half the corpus has multi-vectors
- **When** hybrid search runs over covered messages
- **THEN** results fuse correctly and uncovered messages fall through to other signals.

### Requirement: Observability parity

The Status surface MUST report late-interaction index health: total/capacity,
messages covered vs pending per model, last completed run, storage bytes, and
encoder endpoint reachability. Drift between recorded coverage and actual rows
MUST be detectable (verify mode), matching the spirit of REQ-0002-009.

#### Scenario: Backfill progress visible

- **Given** a long embed run is in flight
- **When** Status refreshes
- **THEN** pending count decreases toward zero and completed-run metadata updates.
