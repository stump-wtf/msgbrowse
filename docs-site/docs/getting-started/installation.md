---
title: Installation
sidebar_position: 2
description: Install the msgbrowse CLI (Homebrew formula or go install) and the macOS desktop app (Homebrew cask) — pure-Go SQLite, no C toolchain.
---

# Installation

The SQLite driver is pure Go (with FTS5 built in), so `CGO_ENABLED=0` is the
normal build — there is **no C toolchain, no build tag, and no shared library**
to set up either way.

:::info CLI and desktop app are separate installs
The **CLI** ships as a Homebrew *formula*; the macOS **desktop app** ships as a
Homebrew *cask* under a different token. `brew install msgbrowse` gives you the
CLI only — it does not install the `.app`. Homebrew forbids a formula from
placing an application bundle in `/Applications`, which is exactly what casks
are for. Install either or both.
:::

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

## Install the desktop app (macOS)

The native app is a [Wails v2](https://wails.io) window over the same embedded
web server the CLI serves — same pages, same handlers. On macOS it also bundles
the three exporters and a Syncthing binary, so a fresh Mac with no Homebrew and
no Python can export and import offline.

```sh
brew tap stump-wtf/tap
brew install --cask msgbrowse-desktop
```

That installs the universal `.app` to `/Applications`. To remove it:

```sh
brew uninstall --cask msgbrowse-desktop          # app only
brew uninstall --zap --cask msgbrowse-desktop    # also deletes your data dir
```

:::warning The cask strips quarantine on purpose
The `.app` is **ad-hoc signed** (`codesign -s -`), not yet notarized with an
Apple Developer ID. Homebrew quarantines cask downloads by default, and
Gatekeeper then blocks not only the app but the exporter binaries it launches as
subprocesses — so export and import break, not just the first launch. The cask's
`postflight` runs `xattr -dr com.apple.quarantine` to prevent that. Installing by
hand from the [GitHub Release](https://github.com/stump-wtf/msgbrowse/releases)
means running that command yourself. Both go away once notarization lands.
:::

On Linux, download `msgbrowse-desktop_linux_amd64.tar.gz` from the release and
extract it — the tarball preserves the execute bit. The binary links the system
webview, so you need the WebKit2GTK runtime (`sudo apt-get install libgtk-3-0
libwebkit2gtk-4.1-0` on Ubuntu 24.04+ / Debian 13). Windows is not built yet;
use browser mode (`msgbrowse serve`) there.

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
