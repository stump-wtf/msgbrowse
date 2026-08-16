---
status: approved
date: 2026-06-27
implements: [ADR-0004]
---

# SPEC-0003: MCP Server

- **Capability:** mcp
- **Source packages:** `internal/mcp` (`server.go`, `tools.go`), `internal/cli` (`mcp.go`)
- **Related ADRs:** [ADR-0004](../../../adr/0004-mcp-sdk-and-rag.md)

## Overview

msgbrowse exposes a Model Context Protocol server so an MCP client (Claude Desktop
/ Claude Code) can answer natural-language questions over the archive with
citation-faithful retrieval tools. The server MUST be read-only, MUST carry exact
provenance on every result, MUST be a thin adapter over the store, and MUST support
both stdio and streamable-HTTP transports.

## Requirements

### REQ-0003-001: Tool set

The server MUST register exactly these tools, built with the official
`modelcontextprotocol/go-sdk` (typed `AddTool` inferring JSON Schema from Go
structs): `list_conversations`, `get_conversation`, `search_messages`,
`semantic_search`, `get_context`, `list_media`, and `list_links`.

#### Scenario: All tools are advertised
- **Given** a connected MCP client
- **When** it lists tools
- **Then** `list_conversations`, `get_conversation`, `search_messages`, `semantic_search`, `get_context`, `list_media`, and `list_links` are present.

### REQ-0003-002: Citation-faithful results

Every message-bearing result MUST carry exact provenance: `message_id`, stable
`hash`, `conversation`, `source`, `sender`, and `timestamp`. The server MUST NOT
return a passage without its coordinates. The owner's sender MUST be normalized to
`Me`. Keyword hits carry the highlighted snippet as `text`; semantic-only hits
carry the message body and a similarity `score`.

#### Scenario: A search hit is fully cited
- **Given** a `search_messages` query that matches a message
- **When** the result is returned
- **Then** each hit includes message_id, hash, conversation, source, sender, and timestamp.

#### Scenario: list_media and list_links carry provenance
- **Given** a `list_media` or `list_links` call
- **Then** each item includes its conversation, source, timestamp, and owning message_id (and for links, the deduplicated URL with occurrence count).

### REQ-0003-003: Hybrid and semantic tools

`search_messages` MUST perform hybrid keyword + semantic retrieval (RRF fusion per
SPEC-0002) filterable by conversation, sender, source, and date range, defaulting
to 20 results (max 100). `semantic_search` MUST perform pure vector retrieval
returning the K most similar messages with a similarity score, and MUST return an
explicit error when no embedding model is configured. `get_conversation` MUST
return a transcript in chronological order optionally bounded by a date range.
`get_context` MUST return a window of messages around a given message id and MUST
error if the message id does not exist.

#### Scenario: semantic_search without a model errors clearly
- **Given** no embedding model configured
- **When** `semantic_search` is called
- **Then** it returns an error stating semantic search is unavailable.

#### Scenario: get_context on a missing message errors
- **Given** a `message_id` that does not exist
- **When** `get_context` is called
- **Then** it returns a "message not found" error.

### REQ-0003-004: Read-only, minimal egress

The server MUST NOT mutate the store or the archive. Its only network egress MUST
be embedding the query for semantic search, via the same `llm.Client` (and thus the
same local-by-default endpoint) as the rest of msgbrowse. Tools MUST be a thin
adapter calling the same store methods the web UI uses, so behavior cannot drift.

#### Scenario: No tool writes data
- **Given** any sequence of tool calls
- **When** they execute
- **Then** no message, attachment, link, contact, or archive file is created, modified, or deleted.

#### Scenario: Only egress is query embedding
- **Given** a `search_messages` or `semantic_search` call
- **When** it runs
- **Then** the only outbound network call is to the configured LLM endpoint to embed the query.

### REQ-0003-005: Transports

The server MUST serve over stdio by default and MUST support streamable HTTP when
selected (`--http`), binding the HTTP transport to a loopback address by default
(`127.0.0.1:8788`).

#### Scenario: Default stdio transport
- **Given** `msgbrowse mcp` with no flags
- **When** it starts
- **Then** it serves the MCP protocol over stdio.

#### Scenario: HTTP transport on loopback
- **Given** `msgbrowse mcp --http`
- **When** it starts
- **Then** it serves streamable HTTP on the loopback listen address.

### REQ-0003-006: Stderr-only logging

Because stdio is the default transport, all logging MUST go to stderr so it never
corrupts the JSON-RPC stream on stdout.

#### Scenario: Logs never touch stdout under stdio
- **Given** the server running over stdio
- **When** it logs progress or warnings
- **Then** those records are written to stderr only.
