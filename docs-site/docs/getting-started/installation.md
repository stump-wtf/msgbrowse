---
title: Installation
sidebar_position: 2
description: Install msgbrowse with Homebrew (preferred) or a single go install — pure-Go SQLite, no C toolchain.
---

# Installation

The SQLite driver is pure Go (with FTS5 built in), so `CGO_ENABLED=0` is the
normal build — there is **no C toolchain, no build tag, and no shared library**
to set up either way.

## Install the binary

### Homebrew (preferred)

```sh
brew tap stump-wtf/tap
brew install msgbrowse
msgbrowse version
```

The formula builds from source, so the binary is compiled on your machine. It
therefore never carries macOS's `com.apple.quarantine` attribute — no Gatekeeper
prompt and no `xattr -d com.apple.quarantine` step, which is what you would hit
with a downloaded prebuilt binary.

### `go install`

Requires **Go 1.25+** and nothing else:

```sh
go install github.com/joestump/msgbrowse/cmd/msgbrowse@latest
```

The binary lands in `$(go env GOBIN)` (or `$(go env GOPATH)/bin`) — make sure
that directory is on your `PATH`:

```sh
export PATH="$PATH:$(go env GOPATH)/bin"
msgbrowse version
```

:::note macOS Full Disk Access
Reading **live** iMessage data means reading `~/Library/Messages/chat.db`, which
requires **Full Disk Access** for your terminal (System Settings → Privacy &
Security → Full Disk Access). That is macOS TCC, a different mechanism from
Gatekeeper — no install method avoids it.
:::

## Install the exporters

msgbrowse reads archives produced by three upstream tools. Install whichever
sources you have — any subset (Signal-only, iMessage-only, …) works fine.

**Signal** — [`signal-export`](https://github.com/carderne/signal-export),
installed with pipx:

```sh
pipx install signal-export
```

:::tip
The pip *package* is `signal-export`, but the console *command* it installs is
`sigexport` — that is the binary `msgbrowse export` looks up on your `PATH`.
:::

**iMessage** — [`imessage-exporter`](https://github.com/ReagentX/imessage-exporter),
installed with Homebrew on macOS:

```sh
brew install imessage-exporter
```

**WhatsApp** — [`whatsapp-chat-exporter`](https://github.com/KnugiHK/WhatsApp-Chat-Exporter),
installed with pipx (the console command is `wtsexporter`):

```sh
pipx install whatsapp-chat-exporter
```

msgbrowse never auto-installs these tools and never touches the sensitive
sources (the Signal database, the macOS Keychain, `chat.db`,
`ChatStorage.sqlite`) itself — it only spawns your own, already-installed
exporters at your explicit request.

## Alternative: Docker

Prefer containers? The repo ships a `Dockerfile` and a compose stack — the
image is a fully static binary on a distroless base, running non-root with a
read-only root filesystem and all capabilities dropped.

```sh
git clone https://github.com/joestump/msgbrowse.git
cd msgbrowse
cp .env.example .env
# edit .env:
#   MSGBROWSE_ARCHIVE_HOST  → your archive's absolute path
#   MSGBROWSE_LLM_BASE_URL  → your LiteLLM proxy (…/v1), MSGBROWSE_LLM_API_KEY → its key

make up            # build + start msgbrowse (points at your external LiteLLM)
make signal-import # import the signal-export archive into the local DB
make embed         # compute embeddings for semantic search (optional)
# open http://127.0.0.1:8787
```

`make logs` tails the server; `make down` stops the stack. The archive is
mounted read-only (`:ro`), app data lives in a named volume, and the UI is
published to **host loopback only**.

:::tip
No LLM proxy yet? Run the bundled, fully local LiteLLM with `make up-bundled`
and set `MSGBROWSE_LLM_BASE_URL=http://litellm:4000/v1`. Until an endpoint is
reachable, `embed` and the journal fail — browsing and keyword search work
without any LLM.
:::

## About the LLM endpoint

msgbrowse only ever talks to **your own** OpenAI-compatible endpoint —
configure it with `MSGBROWSE_LLM_BASE_URL` (a `…/v1` URL) and
`MSGBROWSE_LLM_API_KEY` via env, `config.yaml`, or flags (see the
[configuration reference](../reference/configuration.md) for all keys and
their precedence). The default is a local proxy at `http://127.0.0.1:4000/v1`.
This endpoint is the **only** network egress in the entire application, and it
is optional: everything except `embed`, `facts`, semantic search, and the
journal works without it. The [security model](../reference/security-model.md)
documents exactly what is sent to it.

## Next step

With the binary and exporters installed, produce your first archives:
[Exporting your archives](exporting-your-archives.md).
