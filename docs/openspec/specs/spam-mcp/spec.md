---
status: draft
date: 2026-08-22
implements: [ADR-0029]
extends: [SPEC-0028, SPEC-0003]
---

# SPEC-0030: Unsolicited-contact evidence — MCP tools

- **Capability:** spam-mcp
- **Target packages:** `internal/mcp` (`tools.go`, `server.go`),
  `internal/config` (`mcp` block)
- **Related ADRs:** [ADR-0029](../../../adr/0029-unsolicited-contact-evidence.md)
  §7 (which defers this surface), [ADR-0004](../../../adr/0004-mcp-sdk-and-rag.md),
  [ADR-0010](../../../adr/0010-security-privacy-posture.md)
- **Related specs:** [SPEC-0028](../spam-evidence/spec.md) — the evidence layer
  these tools read; [SPEC-0003](../mcp/spec.md) — the MCP server they extend;
  [SPEC-0029](../spam-web/spec.md) — the sibling presentation surface, which
  owns the write path these tools deliberately lack.

## Overview

Four read-only MCP tools over the unsolicited-contact evidence record: list the
senders, read one sender's stats and events, list the senders who contacted the
owner after an opt-out, and retrieve a full dossier.

The write path stays out. Recording a judgment or a filed complaint is a human
assertion, and `msgbrowse spam sender-set` / `spam event add` and the web forms
of SPEC-0029 remain its only entry points.

### The egress question, answered precisely

The reflex objection is that ADR-0029 §3 makes this the one msgbrowse derivation
with zero network egress, so exposing it over MCP contradicts the ADR. That is
the wrong cut, and stating the right one is the point of this section.

The zero-egress property in ADR-0029 §3 is a property of **the scan**: an
archive the owner will not send to an LLM can still be classified. Nothing in
that argument concerns what a user may later choose to ask a model *about* the
result. And `search_messages` and `get_conversation`
([SPEC-0003](../mcp/spec.md)) already return raw message bodies to whatever model
drives the client, so a dossier's verbatim messages are not a new disclosure
class — they are a re-presentation of text the MCP already serves.

What *is* new is the non-derived half of `spam_senders`: `status`,
`suspected_entity`, `consent_status`, `consent_notes`, `notes`. Those are the
owner's own judgments about people they may be building a case against, they
exist nowhere else in the archive, and REQ-0028-007 protects them from being
overwritten by a machine. They are the reason this surface is opt-in
(REQ-0030-002) and the reason it is read-only (REQ-0030-003).

## Scope

In scope: four read-only tools, their schemas and provenance fields, the
registration gate, and their tests.

Non-goals: any write tool; any tool that triggers a scan; MCP *resources* or
prompts for this capability; and every SPEC-0028 non-goal (carrier lookup, group
threads, attachment bytes, LLM classification).

## Requirements

### REQ-0030-001: The four tools

The MCP server MUST expose exactly these tools, named to match the existing
unprefixed-verb convention (`list_conversations`, `list_media`, `list_links`):

| Tool | Returns | Message bodies |
| --- | --- | --- |
| `list_spam_senders` | senders with counts, first/last seen, after-opt-out count, status; filterable by status | no |
| `get_spam_sender` | one sender's judgments, summary stats and recorded events | no |
| `list_spam_violations` | senders with contact after an opt-out, with trailing-12 and worst-window counts | no |
| `get_spam_dossier` | the full dossier of REQ-0028-011 | yes |

Only `get_spam_dossier` MUST return verbatim message bodies. The other three
MUST NOT, so a model can survey the record — which is the common case — without
the whole corpus entering its context.

`get_spam_dossier` MUST render from the same `spam.Dossier` value as the CLI and
the web surface, so the three cannot disagree.

#### Scenario: A survey does not pull the corpus
- **Given** a sender with 400 findings
- **When** `list_spam_senders` is called
- **Then** the response contains their counts and no message body.

### REQ-0030-002: Registration is opt-in

The spam tools MUST NOT be registered unless the owner has explicitly enabled
them through configuration (`mcp.expose_spam`, default false). Every other
msgbrowse tool registers unconditionally; this is the first conditional tool
surface, and the precedent is set deliberately here.

Two reasons. The record contains judgments that exist nowhere else in the
archive and that the owner may reasonably not want a model to read at all, and
"they ran a scan" is not consent to that. And four tools is a substantial
increase over the existing seven for a capability most users never enable;
tool-list bloat degrades selection for everyone connected.

The gate MUST be configuration, not data state. Registering on the basis of
"findings exist" would make the tool list change shape as a side effect of
running a scan, which is both a surprise to clients that enumerate tools at
connect time and an implicit grant the owner never made.

When the tools are enabled but the archive has never been scanned, they MUST
register and return empty results with an explanatory message, rather than
erroring.

#### Scenario: Scanning does not grant access
- **Given** `mcp.expose_spam` unset and a scanned archive
- **When** a client lists tools
- **Then** no spam tool appears.

### REQ-0030-003: Read-only, with no scan trigger

Tools in this capability MUST NOT write to `spam_senders`, `spam_events`,
`spam_findings` or `spam_state`, and MUST NOT initiate a scan.

There MUST be no `spam_scan` tool. A scan is local and idempotent, which makes
one tempting, but it can invalidate a generation (REQ-0028-003), and its
deliberateness is what makes a generation correspond to a moment the owner
chose. Re-deriving an evidence record as a side effect of answering a question
is not a trade worth making.

There MUST be no tool writing `status`, `consent_status`, `consent_notes`,
`suspected_entity`, `notes`, or a manual event. REQ-0028-007 makes these
human-only against a *scan*; the same reasoning applies with more force to a
model, and a filed FCC confirmation number recorded by an LLM would be a
fabricated legal fact inside an evidence database.

#### Scenario: The capability holds no write tool
- **Given** the enabled spam tool set
- **When** it is enumerated
- **Then** every tool is read-only and none triggers a scan.

### REQ-0030-004: Every response carries its provenance and its limits

Every tool response MUST carry the `ruleset_version` it was read under and the
scan-environment provenance of REQ-0028-013.

`get_spam_dossier` MUST include the limitations list of REQ-0028-011 in its
structured output. A test MUST assert it, mirroring the tests that guard the
Markdown artifact and the rendered page.

The reasoning is the same one ADR-0029 §6 gives for putting limitations in the
artifact rather than only in the docs, and it binds hardest here. A model given
a timestamp with no stated caveat will report it as a timestamp; asked when a
sender messaged, it will answer "3:47pm" without the offset warning that makes
the answer honest. The consumer that most needs the limitations block is the one
least able to infer it, so the machine-readable output MUST be at least as
honest as the human-readable one.

The three tools that omit message bodies MUST still carry the limitations that
bear on what they *do* return — the timestamp caveat, the carrier caveat, and
the human-judgment caveat on `suspected_entity` and `consent_status`.

#### Scenario: A dossier read by a model states its own limits
- **Given** any sender with findings
- **When** `get_spam_dossier` is called
- **Then** the structured result contains the limitations list, including the
  no-recorded-UTC-offset and carrier-not-established statements.

#### Scenario: Provenance travels with every response
- **Given** findings written under a degraded stranger predicate
- **When** any spam tool is called
- **Then** the response reports the ruleset version and that the underlying
  findings are degraded.

### REQ-0030-005: Tool descriptions state the record's nature

Each tool's description MUST state that it reads a derived evidence record, that
the record is not legal advice, and — for `get_spam_dossier` — that it returns
verbatim message text.

A tool description is the only documentation a model reliably reads. It is where
"this is an evidence record about identifiable people, not a spam score" has to
be said, because it governs whether the model treats the result as a conclusion
or as material to be qualified.

#### Scenario: The description sets expectations
- **Given** the enabled spam tool set
- **When** descriptions are read
- **Then** each states that the record is derived and is not legal advice.
