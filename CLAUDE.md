# msgbrowse

Self-hosted, local-only browser, search engine, and AI-editorialized journal over
your Signal, iMessage, and WhatsApp archives. Go + HTMX + SQLite; nothing leaves
the machine except calls to one configurable OpenAI-compatible LLM endpoint —
plus, when opt-in device sync is enabled (ADR-0021), LAN-only Syncthing traffic
to explicitly paired devices.

See [README.md](README.md) for usage, [ARCHITECTURE.md](ARCHITECTURE.md) for the
layering, and [SECURITY.md](SECURITY.md) for the threat model.

## Architecture Context

- Architecture Decision Records are in docs/adr/
- Specifications are in docs/openspec/specs/

Each spec is a paired artifact: `spec.md` (requirements) and `design.md`
(architecture + rationale). ADRs use MADR format.

### SDD Configuration

#### Tracker

**Gitea is the source of truth.** Issues, pull requests and CI all live at
`https://gitea.stump.rocks/stump.wtf/msgbrowse`. The GitHub side
(`github.com/stump-wtf/msgbrowse`) is a downstream push mirror: it carries code
only, and has no issues and no pull requests. A `#123` reference in this repo
means a Gitea issue — resolving one against GitHub 404s.

This block previously read `github` / `joestump`, which pointed every tool that
reads it at the archived personal repo.

- **Type**: gitea
- **Host**: https://gitea.stump.rocks
- **Owner**: stump.wtf
- **Repo**: msgbrowse

> **Remote layout.** `origin` is Gitea (`stump.wtf/msgbrowse`); the GitHub
> mirror is the remote named `github`. It was the other way round until
> 2026-07-26, and the inversion was a trap worth remembering: `make check` runs
> the migration guard against `origin/main`, so with `origin` pointing at the
> stale GitHub fork it falsely reported "N shipped migration(s) modified", and
> repo-relative links resolved to GitHub, where none of these issues exist. If a
> checkout still has `origin` on GitHub, either fix the remotes or pass
> `make check MIGRATION_BASE_REF=<gitea-remote>/main`.

#### Branch Conventions
- **Enabled**: true
- **Prefix**: feature
- **Epic Prefix**: epic

#### PR Conventions
- **Enabled**: true
- **Ref Keyword**: Part of
- **Include Spec Reference**: true

#### Review
- **Max Pairs**: 2
- **Merge Strategy**: squash
- **Auto Cleanup**: false
