---
status: approved
date: 2026-06-27
implements: [ADR-0011]
---

# SPEC-0005: Contact facts

- **Capability:** contact-facts
- **Source packages:** `internal/facts`, `internal/store` (`facts.go`, `schema.go` v4, `query.go`), `internal/cli` (`facts.go`), `internal/web` (`templates/conversation.html`)
- **Related ADRs:** [ADR-0011](../../../adr/0011-contact-facts-extraction.md), [ADR-0003](../../../adr/0003-dual-source-archive.md), [ADR-0010](../../../adr/0010-security-privacy-posture.md)

## Overview

msgbrowse extracts durable, atomic, cited facts about a contact from their
messages using the configured chat model, and shows them on the conversation
view. Extraction MUST be a deliberate, separate step (network egress), MUST be
incremental and idempotent, MUST deduplicate per contact, MUST honor the exclude
list, and every fact MUST carry provenance back to a source message.

## Requirements

### REQ-0005-001: Dedicated, opt-in extraction command

Fact extraction MUST be a separate command (`msgbrowse facts`) that performs the
only network egress to `llm.base_url`. Import and serve MUST NOT trigger
extraction. The command MUST use `llm.chat_model` and expose `--reset`,
`--batch-size`, `--concurrency`, and `--conversation` flags.

#### Scenario: Extraction is explicit
- **Given** an imported archive with no facts
- **When** the user runs `msgbrowse facts`
- **Then** facts are extracted via `llm.base_url`; running `signal-import` or `serve` alone never calls the LLM.

### REQ-0005-002: Structured, categorized, cited facts

For each batch of a contact's real (non-system, non-empty) messages, the model
MUST be asked for a JSON array of `{fact, category, evidence}`. The parser MUST
tolerate code fences / surrounding prose, MUST coerce an unknown category to
`other`, and MUST clamp an out-of-range or missing `evidence` index to the last
message in the batch so every stored fact has provenance `(source,
source_message_hash, source_ts)`.

#### Scenario: Lenient parse with provenance
- **Given** a model response wrapped in a ```json fence with one unknown category and one out-of-range evidence index
- **When** the response is parsed
- **Then** the array is extracted, the unknown category becomes `other`, and the out-of-range citation binds to the last message in the batch.

### REQ-0005-003: Per-contact deduplication

Facts MUST be keyed to `contacts(id)` and deduplicated by normalized text
(`fact_hash = sha256(lower(trim(fact)))`, `UNIQUE(contact_id, fact_hash)`).
`PutFact` MUST be idempotent (`INSERT … ON CONFLICT DO NOTHING`). Facts MUST be
visible from every conversation linked to the same contact.

#### Scenario: Merged contact, single fact set
- **Given** a Signal and an iMessage conversation merged onto one contact
- **When** the same fact is extracted from each
- **Then** it is stored once and appears on both conversation pages.

### REQ-0005-004: Incremental, re-ingest-safe cursor

Each conversation MUST track an extraction cursor (`fact_state`) storing the last
processed message **hash** and the model used. A run MUST resume after that
message; a model change MUST re-scan from the start; a cursor whose hash no
longer exists (re-ingest) MUST restart from the top without error. A completed
contact MUST require zero LLM calls on a re-run with no new messages. There MUST
be no foreign key from `source_message_hash` to `messages`.

#### Scenario: Re-run is a no-op
- **Given** a contact whose messages were all extracted
- **When** `msgbrowse facts` runs again with no new messages
- **Then** no LLM call is made and no facts are added.

### REQ-0005-005: Honor the exclude list

`FactConversations` MUST exclude conversations whose name is in
`journal.exclude_conversations` **before** any message content is read, so
excluded threads are never sent to the LLM. Conversations without a linked
contact or without real messages MUST also be excluded.

#### Scenario: Excluded thread is never sent
- **Given** a conversation named in `journal.exclude_conversations`
- **When** extraction runs
- **Then** that conversation's content is never passed to the LLM and yields no facts.

### REQ-0005-006: Conversation-view display with jump links

The conversation view MUST render extracted facts (when present) grouped/labeled
by category, each linking to its supporting message via jump-to-context when that
message still exists, and MUST omit the panel entirely when a contact has no
facts. The UI MUST label facts as AI-generated and possibly wrong.

#### Scenario: Facts panel with provenance
- **Given** a contact with at least one stored fact whose source message exists
- **When** the conversation page is requested
- **Then** the page shows the fact, its category, and a link to `/c/{id}/at/{messageID}`.
