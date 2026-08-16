---
status: approved
date: 2026-06-27
implements: [ADR-0003, ADR-0005]
---

# SPEC-0001: Archive Ingestion

- **Capability:** ingestion
- **Source packages:** `internal/signal`, `internal/imessage`, `internal/ingest`, `internal/source`, `internal/store`
- **Related ADRs:** [ADR-0003](../../../adr/0003-dual-source-archive.md), [ADR-0005](../../../adr/0005-imessage-txt-parser.md), [ADR-0001](../../../adr/0001-sqlite-driver-mattn-cgo.md)

## Overview

msgbrowse imports message archives produced by two upstream CLI exporters —
`signal-export` (Signal Desktop, Markdown `chat.md`) and `imessage-exporter`
(Apple iMessage, `-f txt`) — into one unified, source-tagged SQLite store. Import
MUST be read-only against the archive, incremental, and idempotent: re-running
over an unchanged archive is a cheap no-op, and re-running over a changed
conversation correctly reflects edits and deletions without duplicating rows.

## Requirements

### REQ-0001-001: Read-only archive access

The importer MUST treat the archive as strictly read-only and MUST NOT create,
modify, or delete any file inside it. The SQLite database MUST live in a writable
data directory outside the archive.

#### Scenario: Import never writes to the archive
- **Given** a signal-export archive at `archive_root` and a writable `data_dir`
- **When** an ingest pass runs
- **Then** every file under `archive_root` is only opened for reading
- **And** all persisted state (messages, attachments, links, ingest bookkeeping) is written to the SQLite database under `data_dir`.

### REQ-0001-002: Signal chat.md parsing

The Signal parser MUST treat a line matching the bracketed-timestamp anchor
`[YYYY-MM-DD HH:MM:SS] <sender>: <inline body>` as the start of a new message and
MUST append every subsequent non-anchor line to the current message body with
internal newlines preserved. It MUST parse the timestamp with layout
`2006-01-02 15:04:05` as UTC for ordering while preserving the original bracketed
text. It MUST extract Markdown images `![alt](target)` as image attachments,
Markdown links `[text](target)` whose target is an `http(s)` URL as links and
otherwise as file attachments, and remaining bare `http(s)` URLs as links,
de-duplicating links by URL in first-seen order with trailing sentence
punctuation trimmed. The sender `Me` MUST mark the owner and the sender
`No-Sender` MUST mark a system event (`IsSystem`). It MUST capture signal-export's
reactions trailer — a final body line `(- <Name>: <emoji>, … -)` — as the
message's reactions (emoji + reactor) and MUST strip that trailer from the body,
so a reaction never appears in the body or as a standalone message.

#### Scenario: A reactions trailer is captured, not left in the body
- **Given** a `chat.md` entry whose body ends with a line `(- Me: 👍, MJ: ❤️ -)`
- **When** the parser reads the entry
- **Then** one message is emitted carrying two reactions (`👍` by `Me`, `❤️` by `MJ`)
- **And** the reactions trailer does NOT appear in the message body and no extra message is emitted.

#### Scenario: A multi-line message with media and a link
- **Given** a `chat.md` entry `[2022-01-01 10:00:00] Alice: see this ![cabin](media/cabin.jpg) https://example.com/x.`
- **When** the parser reads the entry
- **Then** one message is emitted with sender `Alice`, timestamp parsed to UTC, and original raw timestamp preserved
- **And** it has one image attachment `media/cabin.jpg`
- **And** it has one link `https://example.com/x` (the trailing period trimmed).

#### Scenario: Owner and system messages are flagged
- **Given** an entry whose sender is `Me` and another whose sender is `No-Sender`
- **When** the parser reads them
- **Then** the first message is the owner's and the second has `IsSystem` true.

### REQ-0001-003: iMessage txt parsing

The iMessage parser MUST target the imessage-exporter 4.2.0 txt format: a
timestamp line (`Mon DD, YYYY  H:MM:SS AM/PM` with a space-padded hour, optionally
followed by a parenthesized read receipt), then a sender line, then zero or more
body and attachment lines, terminated by a blank line. A line MUST be treated as a
new-message timestamp only when the WHOLE line is a timestamp (so body text that
merely begins with a date is not misread). It MUST detect attachments
heuristically: a spaceless, slash-bearing, non-URL path ending in a known
media/document extension is an attachment (image vs file by extension); everything
else is body text. It MUST capture a `Tapbacks:` block — its indented
`<reaction> by <reactor>` detail lines — as the message's reactions, mapping the
standard tapback words (Loved→❤️, Liked→👍, Disliked→👎, Laughed→😂,
Emphasized→‼️, Questioned→❓) to emoji and passing a custom emoji reaction through
as-is, so a tapback never becomes a standalone message. It MUST skip indented
quoted-reply detail and status notices (`This message responded to an earlier
message.`, `Attachment missing!`, deleted-message notices). It MUST extract bare
`http(s)` URLs from body as links. It MUST NOT persist an empty message (a
timestamp with no sender, body, attachment, link, or reaction).

#### Scenario: A message block with an attachment path
- **Given** a block `May 20, 2020  9:10:11 AM` / `Me` / `at the cabin` / `attachments/AB/CD/IMG_1234.HEIC`
- **When** the iMessage parser reads it
- **Then** one message is emitted with sender `Me`, body `at the cabin`, and one image attachment `attachments/AB/CD/IMG_1234.HEIC`.

#### Scenario: A body line that begins with a date is not a new message
- **Given** a body line `Jan 5, 2021 10:30:00 AM was great`
- **When** the parser reads it after a valid timestamp/sender
- **Then** it is appended to the current message body and does NOT start a new message.

#### Scenario: Tapbacks are captured as reactions; notices are dropped
- **Given** a block containing `Tapbacks:`, an indented `    Loved by Sample`, and `This message responded to an earlier message.`
- **When** the parser reads it
- **Then** the message carries a `❤️` reaction by `Sample` and no extra message is emitted
- **And** none of the `Tapbacks:` / notice lines appear in any emitted message body or attachment.

### REQ-0001-004: Malformed-line resilience

Parsing MUST NOT panic or abort a conversation on malformed input. The Signal
parser MUST skip and report (via the skip callback) any anchor whose timestamp is
invalid, any anchor with an empty sender, and any non-blank content that appears
before the first valid anchor; it MUST continue parsing the rest of the file. A
single failing conversation MUST NOT abort the whole ingest run — it MUST be
logged and counted as an error while other conversations proceed.

#### Scenario: A bad timestamp is skipped, not fatal
- **Given** a `chat.md` with one entry whose bracketed timestamp is invalid followed by valid entries
- **When** the file is parsed
- **Then** the bad entry is reported as a skipped line and counted
- **And** the valid entries are still emitted.

#### Scenario: One broken conversation does not abort the run
- **Given** two conversations where one raises a per-conversation error
- **When** the ingest pass runs
- **Then** the run records one error and still imports the healthy conversation
- **And** the failure is logged.

### REQ-0001-005: Incremental, idempotent import

For each conversation, the importer MUST record per-file bookkeeping
(`mtime`, `size`, content SHA-256 hash, message count, last-ingested time) in
`ingest_state` keyed by conversation id. On a subsequent run it MUST skip a
conversation whose `(mtime, size)` are unchanged without hashing; when metadata
changed but the content hash is identical it MUST refresh state only and not
re-parse; only when the content hash differs (or `--full` is set) MUST it re-parse
and replace messages. A `--full` option MUST force re-parsing of every
conversation regardless of state.

#### Scenario: Unchanged archive is a no-op
- **Given** a conversation already ingested with matching mtime and size
- **When** ingest runs again without `--full`
- **Then** the conversation is not re-parsed and no messages are re-written.

#### Scenario: A touched-but-identical file refreshes state only
- **Given** a conversation whose chat file mtime changed but whose content hash is unchanged
- **When** ingest runs
- **Then** the conversation is not re-parsed
- **And** its `ingest_state` mtime/size/last-ingested are updated.

#### Scenario: Changed content is re-parsed and replaced
- **Given** a conversation whose chat file content hash differs from the stored hash
- **When** ingest runs
- **Then** the conversation's messages are atomically replaced with the freshly parsed set.

### REQ-0001-006: Atomic replace per conversation

Replacing a conversation's messages MUST be a single transaction that deletes the
conversation's existing messages (cascading to their attachments and links) and
inserts the new set. The replace MUST correctly reflect edits and deletions in the
source export.

#### Scenario: Replace is all-or-nothing
- **Given** a conversation being re-ingested with a new message set
- **When** `ReplaceConversationMessages` runs
- **Then** either all old rows are removed and all new rows inserted in one transaction, or the database is unchanged on failure.

### REQ-0001-007: Source-namespaced message hash

Each message MUST be stored under a stable hash that is namespaced by source so
that two conversations sharing a display name across sources (e.g. `signal:MJ` and
`imessage:MJ`) cannot collide on the globally-unique `messages.hash`. The hash MUST
be the SHA-256 of `(source‖conversation, raw-timestamp, sender, body, seq)` with
NUL separators, where `seq` disambiguates byte-identical messages within a
conversation. Re-parsing identical input MUST yield identical hashes.

#### Scenario: Same display name across sources does not collide
- **Given** a Signal conversation "MJ" and an iMessage conversation "MJ" each with a byte-identical message
- **When** both are imported
- **Then** the two messages receive distinct `messages.hash` values and both persist.

#### Scenario: Byte-identical messages within a conversation are disambiguated
- **Given** two messages in one conversation with identical timestamp, sender, and body
- **When** they are imported
- **Then** they receive `seq` 0 and 1 respectively and produce distinct hashes.

### REQ-0001-008: Source tagging and contacts bootstrap

Every conversation, message, and ingest run MUST be tagged with its source
(`signal` or `imessage`; the literals are persisted and MUST never be renamed).
`UpsertConversation` MUST find-or-create the `(source, name)` conversation and, on
first sight of a `(source, identifier)`, MUST auto-create a `contacts` row whose
`display_name` equals the conversation name and a linked `contact_identifiers`
row, then point `conversations.contact_id` at it. It MUST NOT silently merge
identities across identifiers or sources.

#### Scenario: A new identity bootstraps a contact
- **Given** a conversation name not yet seen for a source
- **When** `UpsertConversation` runs
- **Then** a contact and a `(source, identifier)` contact_identifier are created and the conversation is linked to that contact.

#### Scenario: Re-import does not duplicate a contact
- **Given** a conversation already linked to a contact
- **When** it is imported again
- **Then** no new contact or identifier is created.

### REQ-0001-009: Snapshot inventory (listed, never decrypted)

On each Signal ingest pass the importer MUST inventory the archive's
`.snapshots/` directory, recording one row per file matching
`db-YYYYMMDD-HHMMSS.tar` with its filename, taken-at time, byte size, and a GFS
retention tier (daily <= 14 days, monthly <= ~13 months, quarterly <= ~3 years, else
yearly) computed from age. Unrecognized files MUST be ignored, a missing
`.snapshots/` directory MUST NOT be an error, and the importer MUST NOT open or
decrypt any snapshot tar. The inventory MUST be replaced wholesale on each pass.

#### Scenario: Snapshots are listed without decryption
- **Given** an archive whose `.snapshots/` holds two valid `db-*.tar` files and a stray file
- **When** ingest runs
- **Then** the two snapshots are recorded with size and retention tier
- **And** the stray file is ignored
- **And** no snapshot tar is opened or decrypted.

#### Scenario: Missing snapshots directory is benign
- **Given** an archive with no `.snapshots/` directory
- **When** ingest runs
- **Then** the snapshot inventory is empty and the run does not error.

### REQ-0001-010: Ingest run summary

Each ingest pass MUST record a run summary (source, start/finish/duration,
conversations scanned/changed, messages total/added, snapshots seen, skipped
lines, errors) so the web UI status page can report freshness and per-source
health.

#### Scenario: A pass records its summary
- **Given** a completed ingest pass
- **When** it finishes
- **Then** an `ingest_runs` row is written tagging the source and counting scanned, changed, added, skipped, and errored items.
