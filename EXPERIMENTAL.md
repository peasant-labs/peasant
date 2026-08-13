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
| Code map section (web) | `peasant web start --experimental` | runtime |

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

## 3. Code map section — `peasant web start --experimental`

The web dashboard's **code map** section is **shelved**: on a default server the
top-nav tab, the Cmd-K "go to code map" command, and the per-project "· map"
palette jumps do not appear. The shipped binary contains the full feature; only
its entry points in the persistent chrome are gated.

### Default (shipped)

```
$ peasant web start
```

The nav shows `analytics · changes`. The `/map` and `/map/<project>` routes (and
`/projects/<name>/<id>` viewer deep links, which belong to the map IA) stay
directly routable — in-page links such as a transcript's touched-files list
still work — but nothing in the shell advertises the section.

### Experimental

```
$ peasant web start --experimental
```

The started server answers `GET /api/v1/config/features` with
`{"experimental":true}` and the dashboard restores the full three-section nav
(`analytics · changes · code map`) plus the map palette commands. The flag is
per-server-process: the same binary serves either shape, and the background
fork (`peasant web start` without `--foreground`) forwards the flag.

The gate is read at runtime by the SPA in
`web/src/contexts/ServerFeaturesContext.tsx`, which fails closed (map hidden)
while loading and whenever the endpoint is unreachable.

---

## See also

- [docs/memory.md](docs/memory.md) — agent memory reference (experimental)
- [Annotation CLI reference](docs/cli/peasant_annotate.md) — annotation commands
- [docs/bestiary.md](docs/bestiary.md) — harness/provider type model
