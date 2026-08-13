# Experimental features

Some features are **shelved** in the default build — present in the source tree
but gated off so the shipped binary and web app never expose them. This keeps
the default surface stable while work continues behind a flag. This document is
the entry point for turning those features on.

There are three independent gates:

| Feature | Gate | Mechanism |
|---------|------|-----------|
| `peasant memory` (agent memory / lessons) | Go build tag `experimental` | compile-time |
| `/review` page real-data mode (web) | env `NEXT_PUBLIC_EXPERIMENTAL_REVIEW=1` | runtime |
| Code map navigation entry points (web) | `peasant web start --experimental` | runtime |

---

## 1. `peasant memory` — Go build tag

The `peasant memory` command group (agent memory: lesson extraction, embedding,
retrieval, and prompt injection) is gated behind the `experimental` Go build tag.

### Default build (shipped)

`make build` and the published binaries **exclude** the implementation. The
`memory` command is hidden from `peasant --help`, and invoking it returns an
actionable error:

```
$ peasant memory
Error: peasant memory is experimental; rebuild with -tags=experimental
```

### Experimental build

Rebuild with the tag to unlock the full command tree:

```bash
go build -tags=experimental -o bin/peasant ./cmd/peasant
```

This enables `peasant memory build / embed / inject on|off / retrieve / augment /
eval / list`. Run the experimental test suites with the same tag:

```bash
go test -race ./...                                              # default (memory excluded)
go test -race -tags=experimental ./internal/memory/... ./internal/store/...
```

> **Note:** the `memory inject on` hook calls `peasant memory retrieve`, so the
> binary on the hook's path must itself be an `-tags=experimental` build —
> otherwise the hook hits the experimental-gate error above.

### Build-tag seam (brief)

The gating uses a **paired-file seam** so the static command registry in
`cmd/peasant/main.go` is identical in both builds:

- `cmd/peasant/cmd_memory.go` — `//go:build experimental` — the full
  `BuildMemoryCommand()` with all subcommands.
- `cmd/peasant/cmd_memory_stub.go` — `//go:build !experimental` — a hidden stub
  `BuildMemoryCommand()` that returns the actionable error.

Exactly one definition compiles per build; there is no conditional `append()`
restructuring of the command list.

See **[docs/memory.md](docs/memory.md)** for the full command reference, the
friction-episode → lesson → embedding → retrieval pipeline, the `UserPromptSubmit`
hook, and storage details (the `lessons` table, DB migration v28).

---

## 2. `/review` page — runtime env flag

The web dashboard's `/review` page is **shelved**: it is removed
from the navigation bar and, in the default build, runs **mock-only** with a
prominent persistent banner reading *"DEPRECATED — mock data, not real sessions."*

### Default build (shipped): mock-only

`/review` renders only its bundled mock fixtures and issues **no** real-data
fetch, WebSocket subscription, or annotation write. The shipped build never sets
the flag, so this is what end users see if they navigate directly to `/review`.

### Experimental: real-data mode

Setting the runtime flag at build time restores the page's original real-data
behavior (dev-only):

```bash
NEXT_PUBLIC_EXPERIMENTAL_REVIEW=1 pnpm dev     # (within web/)
# or for a production-style build:
NEXT_PUBLIC_EXPERIMENTAL_REVIEW=1 pnpm build
```

The gate is read in `web/src/app/review/mock-fixtures.ts` as
`REVIEW_EXPERIMENTAL` (`process.env.NEXT_PUBLIC_EXPERIMENTAL_REVIEW === '1'`).
When unset (the default), the page is mock-only with the deprecation banner; when
`=1`, the page re-enables real fetches/WebSocket/writes.

---

## 3. Code map navigation entry points — `peasant web start --experimental`

This is a **discoverability gate**, not a route gate. On a default server the
code map's persistent *entry points* — the top-nav tab, the Cmd-K "go to code
map" command, and the per-project "· map" palette jumps — are hidden. It never
removes a route: `/map`, `/map/<project>`, and the `/projects/<name>/<id>` viewer
deep links (which belong to the map IA) stay directly mounted and reachable in
every mode. The shipped binary always contains the full feature; only its
entry points in the persistent chrome are gated.

The `--experimental` flag currently maps to exactly one capability token,
`code_map_navigation_v1`. It is not an "everything experimental" switch: each
gated navigation surface is advertised by its own token, and this flag turns on
just the code-map navigation entry points.

### Default (shipped)

```
$ peasant web start
```

The nav shows `analytics · changes`. The map entry points are absent from the
nav and the command palette, but the `/map`, `/map/<project>`, and
`/projects/<name>/<id>` routes stay directly routable — in-page links such as a
transcript's touched-files list still work — so nothing in the shell advertises
the section while every route remains available.

### Experimental

```
$ peasant web start --experimental
```

The dashboard restores the full three-section nav
(`analytics · changes · code map`) plus the map palette commands. The flag is
per-server-process: the same binary serves either shape, and the background
fork (`peasant web start` without `--foreground`) forwards the flag.

### Capability advertisement

The server advertises its enabled UI capability tokens on
`GET /api/v1/config/capabilities` (part of the schema-owned Peasant Local API,
version 0.8.0). It answers with a `UICapabilitiesResponse` envelope:

```
$ curl -s http://localhost:8690/api/v1/config/capabilities
# default:       {}                                     (no tokens)
# --experimental:{"uiCapabilities":["code_map_navigation_v1"]}
```

An omitted or empty `uiCapabilities` array means no experimental navigation is
advertised. Tokens come from a closed, Peasant-owned inventory; the wire envelope
is owned by the [`github.com/peasant-labs/schema`](https://github.com/peasant-labs/schema)
module. The gate is read once at runtime by the SPA in
`web/src/contexts/ServerCapabilitiesContext.tsx`, which validates the response and
**fails closed** (map entry points hidden) while loading and whenever the endpoint
is unreachable, malformed, or advertises no known token.

---

## See also

- [docs/memory.md](docs/memory.md) — agent memory reference (experimental)
- [Annotation CLI reference](docs/cli/peasant_annotate.md) — annotation commands
- [docs/bestiary.md](docs/bestiary.md) — harness/provider type model
