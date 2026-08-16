# ADR-0001: SQLite driver — mattn/go-sqlite3 (cgo) with FTS5

- **Status:** Superseded by [ADR-0013](0013-pure-go-sqlite-driver.md) (switched to pure-Go `modernc.org/sqlite`; the `sqlite-vec` path that drove the cgo choice is moot per [ADR-0002](0002-vector-backend.md))
- **Date:** 2026-06-27

## Context and Problem Statement

msgbrowse stores everything in a single SQLite database and needs both FTS5
(keyword search) and, later, a vector index (`sqlite-vec` / vec0) in that same
file. Which Go SQLite driver gives us FTS5 now and a viable path to in-process
vectors, while keeping the container image small and ideally cgo-free?

## Considered Options

- **`modernc.org/sqlite`** — pure Go, no cgo, FTS5 available. But it is a Go
  reimplementation of SQLite and cannot load C extensions, so `sqlite-vec`
  (a C extension) is not usable with it.
- **`ncruces/go-sqlite3`** — pure Go via a SQLite WASM build on wazero; FTS5
  available and an `asg017/sqlite-vec-go-bindings/ncruces` flavor exists.
- **`mattn/go-sqlite3`** — the mature cgo driver; FTS5 via the `sqlite_fts5`
  build tag; supports loadable extensions.

## Decision Outcome

Chosen: **`mattn/go-sqlite3` built with `-tags sqlite_fts5`**.

The pure-Go `ncruces` + `sqlite-vec` combination was empirically broken at the
time of this decision: the binding (`asg017/.../ncruces@v0.1.6`) pins an old
ncruces and its embedded WASM requires threads/atomics features that the current
wazero build rejects (`i32.atomic.store invalid as feature "" is disabled`). The
`asg017/.../cgo` binding does not compose with mattn (it needs a separate
`sqlite3.h`). mattn with FTS5 works out of the box and is the most widely
deployed, well-understood option.

### Consequences

- **Good:** Rock-solid FTS5; a clear path to `sqlite-vec` as a runtime-loadable
  extension; one database file.
- **Bad:** Requires cgo, so the build needs a C toolchain and the container is a
  static (musl) or glibc image rather than `scratch` from a pure-Go build. This
  is handled in the Dockerfile (Slice 7).
- The whole project builds/tests with `-tags sqlite_fts5` (wired into the
  Makefile and CI).

See [ADR 0002](0002-vector-backend.md) for how vectors build on this.
