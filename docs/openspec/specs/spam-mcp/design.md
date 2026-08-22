# Design: Unsolicited-contact evidence — MCP tools

## Context

[ADR-0029 §7](../../../adr/0029-unsolicited-contact-evidence.md) deferred MCP
tools alongside the web surface. [SPEC-0030](spec.md) defines them; this document
covers the two decisions that are not obvious from the requirement list — why
exposing the record over MCP does not contradict ADR-0029's zero-egress
property, and why this capability is the first in msgbrowse to register its
tools conditionally.

The implementation template is `internal/mcp/tools.go` as it stands: a
`registerTools` call per tool, a typed in/out struct pair, and a thin method
over `internal/store`.

## Goals / Non-Goals

**Goals.** Let a model survey and read the evidence record. Carry provenance and
limitations into every payload. Keep the human-judgment fields behind an
explicit grant.

**Non-Goals.** No write tools. No scan trigger. No MCP resources or prompts. No
new derivation — these tools read what SPEC-0028 produced.

## Decisions

### Zero egress is a property of the scan, not a prohibition on reading

ADR-0029 §3's argument is that an archive the owner will not send to an LLM can
still be *classified*, because classification is regex and keyword matching. That
property is untouched by a read tool: `spam scan` still makes no network call,
and REQ-0028-001's test still passes unmodified.

The second half of the argument is that the message bodies a dossier contains
are already reachable over MCP. `search_messages` and `get_conversation`
([SPEC-0003](../mcp/spec.md)) return raw bodies today, so `get_spam_dossier` is a
re-presentation of text the server already serves, not a new disclosure class.

What is actually new is the non-derived half of `spam_senders` — `status`,
`suspected_entity`, `consent_status`, `consent_notes`, `notes`. Those exist
nowhere else in the archive, they are the owner's judgments about people they may
be building a case against, and REQ-0028-007 already protects them from being
overwritten by a machine. Everything distinctive about this capability's posture
follows from that one observation.

### Opt-in registration, gated on configuration rather than data

`mcp.expose_spam`, default false.

Gating on data state — "register when `spam_state` is non-empty" — was rejected
twice over. It makes the tool list change shape as a side effect of running a
scan, which surprises clients that enumerate tools once at connect time; and it
treats having scanned as consent to disclose, which it is not. Running a local
classifier and handing the result to a model are separate decisions, and the
product should not silently collapse them.

Registering unconditionally was also rejected. Four tools on top of seven is a
substantial increase in a list every connected model reads on every request, for
a capability most users never enable, and tool-list bloat degrades selection
across the board.

This is msgbrowse's first conditional tool surface, so the precedent matters more
than the immediate case. The rule it sets: a capability whose data includes
statements the user made *about other people* is opt-in; a capability that
derives from the archive alone is not.

### Read-only, and no `spam_scan` tool

A scan is local, deterministic and idempotent, which makes a tool for it look
harmless. It is not: a scan can invalidate a generation (REQ-0028-003), and its
deliberateness is what makes a generation correspond to a moment the owner chose.
An agent re-deriving an evidence record while answering a question is a bad
trade at any price.

The write tools are excluded on stronger grounds. A `consent_status` or a filed
FCC confirmation number recorded by a model is a fabricated legal fact inside a
database whose purpose is to be believed.

### Only the dossier tool carries bodies

Three of the four tools return counts, stats and judgments; only
`get_spam_dossier` returns verbatim messages. The common case is a survey — "who
has been contacting me after I told them to stop?" — and answering it should not
pull the corpus into the model's context.

### Limitations are structured output, not prose

`Dossier.JSON()` already carries `limitations`, so the dossier tool inherits it.
The three flatter tools carry the subset that bears on what they return: the
timestamp caveat, the carrier caveat, and the human-judgment caveat.

This is the same reasoning as ADR-0029 §6 — put the limits in the artifact, not
only in the docs — and it binds hardest here. A model given a timestamp with no
stated caveat will report it as a timestamp; asked when a sender messaged, it
answers "3:47pm" without the offset warning that makes the answer honest. The
consumer that most needs the limitations block is the one least able to infer it.

## Data flow

```
list_spam_senders    → ListSpamSenders + SpamCounts        → counts, no bodies
get_spam_sender      → GetSpamSender + ListSpamEvents      → judgments + events
list_spam_violations → violations query                    → after-opt-out counts
get_spam_dossier     → GetSpamSender → BuildDossier        → full record + limitations

every response ⊕ { ruleset_version, scan_env }
registration gated on mcp.expose_spam
```

## Risks / Trade-offs

**An opt-in surface is a surface people do not find.** Accepted. The CLI and the
web surface are the discoverable paths; MCP is for users who have decided they
want a model reading this record.

**Provenance depends on REQ-0028-013.** Until scan-environment stamping lands,
these tools cannot honestly report which stranger predicate produced a row, which
is why SPEC-0030 is blocked on it rather than shipping with a placeholder.

**A model may still over-claim.** The limitations block reduces this; it does not
eliminate it. Tool descriptions restate that the record is derived and is not
legal advice (REQ-0030-005), because the description is the only documentation a
model reliably reads.

## Testing

- With `mcp.expose_spam` unset, no spam tool is registered even on a scanned
  archive (REQ-0030-002).
- With it set on an unscanned archive, the tools register and return empty
  results rather than erroring (REQ-0030-002).
- No registered tool writes to any `spam_*` table or starts a scan
  (REQ-0030-003).
- `get_spam_dossier` output contains the limitations list, including the
  no-recorded-UTC-offset and carrier-not-established statements — the
  counterpart of the existing Markdown assertion (REQ-0030-004).
- Every tool response carries `ruleset_version` and `scan_env`, and reports
  degraded provenance when the underlying findings are degraded (REQ-0030-004).
- The three survey tools return no message bodies (REQ-0030-001).
