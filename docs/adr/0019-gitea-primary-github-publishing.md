# ADR-0019: Gitea-primary development with GitHub as the publishing mirror

- **Status:** Accepted (not yet implemented — tracked in [#99](https://github.com/joestump/msgbrowse/issues/99))
- **Date:** 2026-07-03
- **Deciders:** Joe Stump
- **Related:** [ADR-0013](0013-pure-go-sqlite-driver.md), [ADR-0017](0017-desktop-shell-wails.md), [ADR-0016](0016-whatsapp-source-exporter.md)

## Context and Problem Statement

msgbrowse develops on GitHub (`joestump/msgbrowse`) with GitHub Actions CI,
GitHub Pages docs, and no published artifacts — users must clone and build.
The owner self-hosts Gitea (`gitea.stump.rocks`, Actions enabled, built-in OCI
registry) and wants the repository homed there — self-hosted, on his own
hardware — **without giving up GitHub as the public face**: the GitHub repo,
Pages docs site, GitHub Releases, and GitHub Packages (ghcr.io) must all keep
working, fed automatically from the Gitea side.

## Decision Drivers

- Self-hosting ethos: the canonical repo should live on owned infrastructure,
  like the rest of StumpCloud.
- Public reach: GitHub is where users find the project; Pages/Releases/ghcr.io
  are the distribution surfaces worth keeping.
- Release builds on owned runners: no GitHub-hosted minutes, native arm64
  available, and the same hardware that already runs Gitea Actions.
- Continuity: epics/stories in flight on the GitHub tracker must not break.

## Considered Options

1. **Gitea primary + native push-mirror to GitHub; publishing driven from
   Gitea Actions** — chosen.
2. GitHub primary + Gitea pull-mirror as a release builder only — inverts the
   ownership goal; the canonical repo stays on GitHub.
3. Full migration off GitHub (repo, issues, releases, pages all on Gitea) —
   loses the public face, breaks in-flight tracker items, docs URL churn.
4. Two independent repos pushed separately — drift guaranteed, no single
   source of truth.

## Decision Outcome

**Option 1**, describing the target state — *none of it is implemented yet*:
today the repository lives only on GitHub, there is no `.gitea/` directory,
and no release/publishing workflow exists; the migration is tracked in
[#99](https://github.com/joestump/msgbrowse/issues/99).

When implemented, `gitea.stump.rocks/joestump/msgbrowse` becomes the canonical
repository. A Gitea **push mirror** (sync-on-commit) will keep
`github.com/joestump/msgbrowse` current using a GitHub PAT, so mirrored pushes
still trigger GitHub Actions — Pages deployment and the existing PR checks
keep working unchanged. The CI checks will be ported to `.gitea/workflows/`
so Gitea PRs gate identically. Release publishing will run on Gitea Actions
runners: multi-arch container images push to **ghcr.io** (and optionally the
Gitea registry), and GitHub Releases are created via the GitHub API with
built artifacts attached.

The issue tracker **stays on GitHub for now** (in-flight epics #86+; the SDD
tracker config is unchanged); moving issues/PR review flow to Gitea is a
separate future decision, deliberately out of scope here.

### Consequences

- Good: canonical code on owned infrastructure; release builds on owned
  runners (native arm64); GitHub remains the zero-churn public face.
- Good: mirrored pushes use a PAT, so GitHub Actions (Pages, PR checks) fire
  exactly as before — no docs or CI regression.
- Bad: two Actions dialects to maintain (`.github/` and `.gitea/`), though
  Gitea Actions is workflow-syntax compatible for the checks we port.
- Bad: secrets management doubles — the GitHub PAT (repo + write:packages)
  lives in Gitea Actions secrets and needs rotation discipline.
- Neutral: contributors on GitHub can still open PRs there; those merge via
  the mirror's owner pulling them into Gitea (documented, expected-rare).
