# Peasant architecture

This page shows how the parts of Peasant connect. It goes from the high level to the low level:

1. [System context](#system-context): Peasant, its user, and the systems it talks to.
2. [Containers](#containers): the runtime units and data stores inside Peasant.
3. [Components](#components): the Go packages and the web modules inside each container.
4. [Deployment](#deployment): where each container runs on the developer workstation.
5. [Dynamic views](#dynamic-views): the main flows between containers, in numbered order.
6. [Call sequences](#call-sequences): the same flows at the level of functions and files.
7. [Package map](#package-map): the role of each package under `internal/` and `cmd/`.

The static and dynamic diagrams use the C4 model in the ASCII notation of
[`.claude/skills/c4-model`](../.claude/skills/c4-model/SKILL.md). Every `c4` block passes
`python3 .claude/skills/c4-model/scripts/c4-lint.py docs/architecture.md`. The call sequences
are plain lifeline diagrams. They are not C4 diagrams and the lint does not check them.

The libraries `github.com/peasant-labs/schema`, `github.com/peasant-labs/redact`, and
`@peasant-labs/fairtrade` are not containers. They show as technology text or in the package
map, never as boxes.

Provenance: derived from the source tree on this branch. The main sources are `cmd/peasant`,
`internal/ingest/pipeline.go`, `internal/ingest/adapter.go`, `internal/api/server.go`,
`internal/push/pipeline.go`, `internal/pull/pipeline.go`, `internal/auth`,
`internal/githooks`, `internal/defaults`, `embed.go`, and `web/src`. When the code and a
diagram disagree, the diagram is wrong. Re-derive the tables from the source before you redraw.

## System context

Peasant is local-first. It reads the session stores of AI coding harnesses on the developer's
machine, keeps its own copy and index, and shows the sessions in a local web app. It sends data
off the machine only when the developer publishes, pulls, logs in, syncs model prices, or
upgrades. There is no telemetry and no background upload. The git hook upload runs only after
the developer installs the hook with `peasant village hooks install`.

Elements:

| Name | Type | Description |
|---|---|---|
| developer | Person | Works with AI coding agents and reviews the sessions. |
| peasant | Software System | Harvests, indexes, shows, and publishes agent sessions. |
| agent session stores | Software System, external | Claude Code, Codex, Cursor, OpenCode, Pi, and Strike stores. |
| git repository | Software System, external | The project repository of the developer. |
| village | Software System, external | Registry and commons for published transcripts. |
| models.dev | Software System, external | Public catalog of model prices and limits. |
| GitHub Releases | Software System, external | Hosts the peasant release archives. |

Relationships:

| Source | Target | Intent | Technology | Trigger |
|---|---|---|---|---|
| developer | peasant | runs commands and opens the local web app | terminal, browser | user |
| peasant | agent session stores | reads sessions | JSONL, SQLite | `harvest`, `kickstart`, web ingest |
| peasant | git repository | reads commits, sets hooks | git CLI | `harvest --detect-commits`, `village hooks` |
| git repository | peasant | runs the upload hook | shell | only after `village hooks install` |
| peasant | village | publishes and pulls | HTTPS | `village push`, `/share`, `village transcripts pull` |
| peasant | models.dev | syncs model prices | HTTPS | `peasant models sync` |
| peasant | GitHub Releases | downloads upgrades | HTTPS | `peasant upgrade` |

```c4
System Context diagram: peasant

+----------------------------+
| developer                  |
| [Person]                   |
| Works with AI coding       |
| agents and reviews them.   |
+----------------------------+
        | runs commands and opens the local web app (terminal, browser)
        v
+----------------------------+                                       +-----------------------------+
| peasant                    |                                       | agent session stores        |
| [Software System]          |-- reads sessions (JSONL, SQLite) ---->| [Software System, external] |
| Harvests local agent       |                                       | Claude Code, Codex, Cursor, |
| sessions, indexes them,    |                                       | OpenCode, Pi, and Strike.   |
| shows them in a local      |                                       +-----------------------------+
| web app, and publishes     |
| the sessions that the      |                                       +-----------------------------+
| developer selects.         |                                       | git repository              |
|                            |-- reads commits, sets hooks (git) --->| [Software System, external] |
|                            |<-- runs the upload hook (shell) ------| The project repository      |
|                            |                                       | of the developer.           |
|                            |                                       +-----------------------------+
|                            |
|                            |                                       +-----------------------------+
|                            |                                       | village                     |
|                            |-- publishes and pulls (HTTPS) ------->| [Software System, external] |
|                            |                                       | Registry and commons for    |
|                            |                                       | published transcripts.      |
|                            |                                       +-----------------------------+
|                            |
|                            |                                       +-----------------------------+
|                            |                                       | models.dev                  |
|                            |-- syncs model prices (HTTPS) -------->| [Software System, external] |
|                            |                                       | Public catalog of model     |
|                            |                                       | prices and limits.          |
|                            |                                       +-----------------------------+
|                            |
|                            |                                       +-----------------------------+
|                            |                                       | GitHub Releases             |
|                            |-- downloads upgrades (HTTPS) -------->| [Software System, external] |
|                            |                                       | Hosts the peasant release   |
|                            |                                       | archives.                   |
+----------------------------+                                       +-----------------------------+

Key:
  Solid box = element. Double-line box = boundary. [Type] = C4 abstraction.
  "external" = outside the scope of this diagram. Arrow = one relationship, read as
  "source, label (technology), target".
```

## Containers

Peasant ships as one static Go binary. The same binary runs the CLI commands and, under
`peasant web start`, the local HTTP server. The web app is a Next.js static export that the
binary embeds with `//go:embed all:web/out` in `embed.go` and serves on port 8690.

Harvest stores transcripts as recorded. It does not redact them. Redaction runs on the way
out: in the `/share` scan, in `peasant village push`, and in `peasant export`.

Elements:

| Name | Type | Technology | Description |
|---|---|---|---|
| peasant web app | Container | Next.js 15, React 19 | Static SPA. Renders transcripts with fairtrade. Hosts `/share`. |
| peasant binary | Container | Go, Cobra, net/http | CLI commands and, under `web start`, REST, WebSocket, and the SPA. |
| analytics database | Container | SQLite | `peasant.db`: sessions, entries, commits, annotations, publications, pulls. |
| sync tree | Container | JSONL files | `peasant-sync/` transcript copies and `village-pulls/` downloads. |
| settings files | Container | YAML, JSON | `config.yaml` (selection, publication defaults) and `credentials.json`. |

Relationships:

| Source | Target | Intent | Technology |
|---|---|---|---|
| developer | peasant web app | opens localhost:8690 | browser |
| developer | peasant binary | runs peasant commands | terminal |
| peasant web app | peasant binary | calls the API | HTTP REST, WebSocket `/api/v1/ws` |
| peasant binary | analytics database | reads and writes | SQL, `zombiezen.com/go/sqlite` |
| peasant binary | sync tree | writes and reads | files |
| peasant binary | settings files | reads and writes | YAML, JSON |
| peasant binary | agent session stores | reads session files and rows | JSONL, SQLite |
| peasant binary | git repository | reads the log and sets hooks | git CLI |
| peasant binary | village | publishes and pulls | HTTPS, JSON, multipart |

```c4
Container diagram: peasant

   +----------------------------+
   | developer                  |
   | [Person]                   |-- runs peasant commands (terminal) -----------+
   | Works with AI coding       |                                               |
   | agents and reviews them.   |                                               |
   +----------------------------+                                               |
                              | opens localhost:8690                            |
                              | (browser)                                       |
+== peasant [Software System] |=================================================|==================+
|                             v                                                 v                  |
|  +----------------------------+                                 +-----------------------------+  |
|  | peasant web app            |                                 | peasant binary              |  |
|  | [Container: Next.js 15]    |-- calls the API (HTTP, WS) ---->| [Container: Go]             |  |
|  | Static SPA. Renders        |                                 | One static binary. CLI      |  |
|  | transcripts with           |                                 | commands harvest, index,    |  |
|  | fairtrade. Hosts /share.   |                                 | publish, and pull. Under    |  |
|  +----------------------------+                                 | web start, it serves        |  |
|                                                                 | REST, WebSocket, and the    |  |
|  +----------------------------+                                 | embedded web app.           |  |
|  | analytics database         |                                 |                             |  |
|  | [Container: SQLite]        |<-- reads and writes (SQL) ------|                             |  |
|  | peasant.db: sessions,      |                                 |                             |  |
|  | entries, commits, pubs.    |                                 |                             |  |
|  +----------------------------+                                 |                             |  |
|                                                                 |                             |  |
|  +----------------------------+                                 |                             |  |
|  | sync tree                  |                                 |                             |  |
|  | [Container: JSONL files]   |<-- writes and reads (files) ----|                             |  |
|  | peasant-sync: saved        |                                 |                             |  |
|  | transcript copies.         |                                 |                             |  |
|  +----------------------------+                                 |                             |  |
|                                                                 |                             |  |
|  +----------------------------+                                 |                             |  |
|  | settings files             |                                 |                             |  |
|  | [Container: YAML, JSON]    |<-- reads, writes (YAML, JSON) --|                             |  |
|  | config.yaml, credentials.  |                                 |                             |  |
|  +----------------------------+                                 +-----------------------------+  |
|                                                                   |   |           |              |
+===================================================================|===|===========|==============+
          +-- reads session files and rows (JSONL, SQLite) ---------+   |           | publishes
          |                                                             |           | and pulls
          |                         +-- reads log, sets hooks (git) ----+           | (HTTPS, JSON)
          |                         |                                               |
          v                         v                                               v
+-----------------------------+   +-----------------------------+   +-----------------------------+
| agent session stores        |   | git repository              |   | village                     |
| [Software System, external] |   | [Software System, external] |   | [Software System, external] |
| Local harness stores.       |   | The project repository.     |   | Transcript registry.        |
+-----------------------------+   +-----------------------------+   +-----------------------------+

Key:
  Solid box = element. Double-line box = boundary. [Type] = C4 abstraction.
  "external" = outside the scope of this diagram. Arrow = one relationship, read as
  "source, label (technology), target".
```

## Components

### Harvest path

`peasant harvest` (alias `peasant ingest`) builds one `ingest.Pipeline` and runs it. The
pipeline asks one adapter per enabled harness to discover sessions, writes a copy of each
changed transcript to the sync tree, mirrors the session rows into SQLite, and then indexes the
transcript into `session_entries`. Each harness declares its record kinds in
`internal/ingest/*_vocabulary.go`. The indexer maps each kind to an `indexformat.Outcome`, and
one central lowering in `record_kinds_lowering.go` decides the stored entry shape.

| Component | Package | Description |
|---|---|---|
| cli | `cmd/peasant` | Cobra command tree. `runHarvest` builds the pipeline options. |
| config | `internal/config` | Loads `config.yaml` and compiles the `SelectionMatcher`. |
| ingest pipeline | `internal/ingest` | Stages DISCOVER to AUDIT. Owns write, mirror, and index. |
| harness adapters | `internal/ingest` | `SourceAdapter`, `TranscriptIndexer`, and vocabulary per harness. |
| store | `internal/store` | SQLite schema, migrations, readers, and writers. |

```c4
Component diagram: peasant binary, harvest path

   +----------------------------+
   | developer                  |
   | [Person]                   |
   +----------------------------+
          | runs peasant harvest (terminal)
          |
+=========|========================== peasant binary [Container: Go] ==============================+
|         v                                                                                        |
|  +----------------------------+                                 +-----------------------------+  |
|  | cli                        |                                 | config                      |  |
|  | [Component: Go, Cobra]     |-- loads settings (Go call) ---->| [Component: Go package]     |  |
|  | cmd/peasant: runHarvest    |                                 | internal/config: settings,  |  |
|  | builds the pipeline.       |                                 | selection matcher.          |  |
|  +----------------------------+                                 +-----------------------------+  |
|         | runs Pipeline.Run (Go call)                                                            |
|         |                                                                                        |
|         v                                                                                        |
|  +----------------------------+                                 +-----------------------------+  |
|  | ingest pipeline            |                                 | harness adapters            |  |
|  | [Component: Go package]    |-- parses sessions (Go call) --->| [Component: Go package]     |  |
|  | internal/ingest: discover, |                                 | Adapter, indexer, and       |  |
|  | diff, filter, write,       |                                 | vocabulary for each         |  |
|  | mirror, index, compute.    |                                 | harness.                    |  |
|  |                            |                                 +-----------------------------+  |
|  |                            |                                               |                  |
|  |                            |                                               |                  |
|  |                            |                                               | reads files      |
|  |                            |-- writes copies (files) --+                   | and rows         |
|  |                            |                           |                   | (JSONL,          |
|  |                            |                           |                   | SQLite)          |
|  +----------------------------+                           |                   |                  |
|         | mirrors and indexes (Go call)                   |                   |                  |
|         |                                                 |                   |                  |
|         v                                                 |                   |                  |
|  +----------------------------+                           |                   |                  |
|  | store                      |                           |                   |                  |
|  | [Component: Go package]    |                           |                   |                  |
|  | internal/store: SQLite     |                           |                   |                  |
|  | schema and queries.        |                           |                   |                  |
|  +----------------------------+                           |                   |                  |
|         |                                                 |                   |                  |
+=========|=================================================|===================|==================+
          | reads and writes                                |                   |
          | (SQL, sqlite)                                   |                   |
          v                                                 v                   v
+-----------------------------+   +-----------------------------+   +-----------------------------+
| analytics database          |   | sync tree                   |   | agent session stores        |
| [Container: SQLite]         |   | [Container: JSONL files]    |   | [Software System, external] |
+-----------------------------+   +-----------------------------+   +-----------------------------+

Key:
  Solid box = element. Double-line box = boundary. [Type] = C4 abstraction.
  "external" = outside the scope of this diagram. Arrow = one relationship, read as
  "source, label (technology), target".
```

### Serve and publish path

`peasant web start` wires `internal/api` over the store. The WebSocket hub pushes session lists
and session detail. The REST routes serve lists, the code map, review, annotations, and the
`/share` sync endpoints. The sync handler runs the same `push.Pipeline` as
`peasant village push`.

| Component | Package | Description |
|---|---|---|
| api | `internal/api` | `Server`, `Hub`, `StoreDataProvider`, `spaHandler`, sync handler. |
| transcript | `internal/transcript` | `EntriesToTurns`, `SessionToDetail`, snapshot detail builders. |
| push | `internal/push` | Target selection, preflight, re-redaction, mapping, upload, receipts. |
| village client | `internal/village`, `internal/auth` | Village HTTP client and the loopback OAuth login. |
| store | `internal/store` | Same component as on the harvest path. |

The canonical detail path is `store` entries, then `transcript.EntriesToTurns`, then
`transcript.SessionToDetail`. `api.EntriesToTurns` and `api.SessionToDetail` are thin wrappers
over it. Sessions indexed into a managed generation take the snapshot branch,
`transcript.BuildSnapshotDetailBytes`, which produces the same `SessionDetailPayload`.

```c4
Component diagram: peasant binary, serve and publish path

   +----------------------------+
   | peasant web app            |
   | [Container: Next.js 15]    |
   +----------------------------+
          | calls REST and
          | WebSocket (HTTP, WS)
          |
+=========|========================== peasant binary [Container: Go] ==============================+
|         v                                                                                        |
|  +----------------------------+                                 +-----------------------------+  |
|  | api                        |                                 | transcript                  |  |
|  | [Component: Go package]    |-- builds detail (Go call) ----->| [Component: Go package]     |  |
|  | internal/api: routes,      |                                 | Builds turns and the        |  |
|  | WebSocket hub, SPA         |                                 | session detail payload.     |  |
|  | handler, sync handler.     |                                 +-----------------------------+  |
|  |                            |                                                                  |
|  |                            |                                                                  |
|  |                            |                                 +-----------------------------+  |
|  |                            |                                 | push                        |  |
|  |                            |-- runs publish (Go call) ------>| [Component: Go package]     |  |
|  |                            |                                 | internal/push: redacts,     |  |
|  |                            |                                 | maps, and uploads the       |  |
|  |                            |                                 | selected sessions.          |  |
|  |                            |                                 |                             |  |
|  +----------------------------+                                 |                             |  |
|         | reads sessions (Go call)                              |                             |  |
|         |                                                       |                             |  |
|         v                                                       |                             |  |
|  +----------------------------+                                 |                             |  |
|  | store                      |                                 |                             |  |
|  | [Component: Go package]    |<-- reads rows (Go call) --------|                             |  |
|  | internal/store: SQLite     |                                 +-----------------------------+  |
|  | schema and queries.        |                                               | uploads          |
|  +----------------------------+                                               | (Go call)        |
|         |                                                                     v                  |
|         | reads and writes                                      +-----------------------------+  |
|         | (SQL, sqlite)                                         | village client              |  |
|         |                                                       | [Component: Go package]     |  |
|         |                                                       | internal/village and        |  |
|         |                                                       | internal/auth.              |  |
|         |                                                       +-----------------------------+  |
|         |                                                                     |                  |
+=========|=====================================================================|==================+
          |                                                                     | sends requests
          |                                                                     | (HTTPS, JSON)
          v                                                                     v
+-----------------------------+                                     +-----------------------------+
| analytics database          |                                     | village                     |
| [Container: SQLite]         |                                     | [Software System, external] |
+-----------------------------+                                     +-----------------------------+

Key:
  Solid box = element. Double-line box = boundary. [Type] = C4 abstraction.
  "external" = outside the scope of this diagram. Arrow = one relationship, read as
  "source, label (technology), target".
```

### Web app

The SPA opens one WebSocket and keeps topic data in a channel store. Pages read lists and
session detail from that store and call REST helpers for the rest. Transcript rendering comes
from `@peasant-labs/fairtrade/ui` and `/graph`. Peasant only adapts local data to it.

| Route | Main file | Data |
|---|---|---|
| `/` | `web/src/app/page.tsx` | WS `sessions`, REST project summaries |
| `/projects/...` | `web/src/app/projects/` | WS `session_detail`, rendered by `SessionDetailV2` |
| `/share` | `web/src/app/share/` | REST `/api/v1/sync/*` |
| `/review/...` | `web/src/app/review/` | REST `/api/v1/review/*` |
| `/map/...` | `web/src/app/map/` | REST `/api/v1/map/*`, capability gated |
| `/analytics` | `web/src/app/analytics/` | WS `quality`, rendered by fairtrade analytics |

```c4
Component diagram: peasant web app

   +----------------------------+
   | developer                  |
   | [Person]                   |-- publishes sessions (browser) ---------------+
   +----------------------------+                                               |
          | browses sessions (browser)                                          |
          |                                                                     |
+=========|========================== peasant web app [Container: Next.js 15] ==|==================+
|         |                                                                     |                  |
|         v                                                                     v                  |
|  +----------------------------+                                 +-----------------------------+  |
|  | section pages              |                                 | share wizard                |  |
|  | [Component: React]         |                                 | [Component: React]          |  |
|  | Home, session viewer,      |                                 | ShareWizardClient: select,  |  |
|  | analytics, review, map.    |                                 | labels, redact, submit.     |  |
|  | Renders with fairtrade.    |                                 +-----------------------------+  |
|  |                            |                                                                  |
|  |                            |                                                       | scans,   |
|  |                            |                                                       | pushes   |
|  |                            |-- fetches lists (call) ---------------+               | (call)   |
|  |                            |                                       |               |          |
|  |                            |                                       |               |          |
|  +----------------------------+                                       v               v          |
|         | subscribes (React hook)                               +-----------------------------+  |
|         v                                                       | REST clients                |  |
|  +----------------------------+                                 | [Component: TypeScript]     |  |
|  | channel store              |                                 | lib/api fetch helpers.      |  |
|  | [Component: TypeScript]    |                                 +-----------------------------+  |
|  | WebSocketContext: one      |                                               |                  |
|  | socket, topic cache.       |                                               |                  |
|  +----------------------------+                                               |                  |
|         |                                                                     |                  |
|         |                                                                     |                  |
|         | subscribes to                                                       | calls /api/v1    |
|         | topics (WebSocket)                                                  | (HTTP, JSON)     |
|         |                                                                     |                  |
|         |                                                                     |                  |
+=========|=====================================================================|==================+
          |                                                                     |
          v                                                                     v
   +--------------------------------------------------------------------------------------------+
   | peasant binary                                                                             |
   | [Container: Go]                                                                            |
   +--------------------------------------------------------------------------------------------+

Key:
  Solid box = element. Double-line box = boundary. [Type] = C4 abstraction.
  "external" = outside the scope of this diagram. Arrow = one relationship, read as
  "source, label (technology), target".
```

## Deployment

Everything runs on the developer workstation. Paths follow XDG: the data dir is
`$XDG_DATA_HOME/peasant` (default `~/.local/share/peasant`), and the config dir is
`$XDG_CONFIG_HOME/peasant` (default `~/.config/peasant`). The `--data-dir` and `--config-dir`
flags override them.

```c4
Deployment diagram: peasant, developer workstation

+== developer workstation [Deployment Node: Linux, macOS, or WSL] =================================+
|                                                                                                  |
| +== web browser [Deployment Node: browser] ==+   +== peasant [Deployment Node: OS process] ====+ |
| |                                            |   |                                             | |
| |  +-------------------------+               |   |     +-------------------------+             | |
| |  | peasant web app         |-- calls (HTTP, WS) ---->| peasant binary          |             | |
| |  | [Container: Next.js 15] |               |   |     | [Container: Go]         |             | |
| |  +-------------------------+               |   |     +-------------------------+             | |
| |                                            |   |        |         |                          | |
| +============================================+   +========|=========|==========================+ |
|                        +-- reads, writes (files) ---------+         | reads, writes (files)      |
|                        v                                            v                            |
| +== config dir [Deployment Node: XDG path] ==+   +== data dir [Deployment Node: XDG path] =====+ |
| |                                            |   |                                             | |
| |  +-------------------------+               |   |     +----------------------------+          | |
| |  | settings files          |               |   |     | analytics database         |          | |
| |  | [Container: YAML, JSON] |               |   |     | [Container: SQLite]        |          | |
| |  | config.yaml, creds.     |               |   |     | peasant.db file.           |          | |
| |  +-------------------------+               |   |     +----------------------------+          | |
| |                                            |   |                                             | |
| +============================================+   |     +----------------------------+          | |
|                                                  |     | sync tree                  |          | |
|                                                  |     | [Container: JSONL files]   |          | |
|                                                  |     | peasant-sync, pulls.       |          | |
|                                                  |     +----------------------------+          | |
|                                                  |                                             | |
|                                                  +=============================================+ |
|                                                                                                  |
+==================================================================================================+

Key:
  Double-line box = deployment node, nested where one runs inside another. Solid box = one
  container instance. [Type] = C4 abstraction. Arrow = one relationship, read as
  "source, label (technology), target".
```

## Dynamic views

### Harvest local agent sessions

```c4
Dynamic diagram: harvest local agent sessions

   +----------------------+
   | developer            |
   | [Person]             |
   +----------------------+
          | 1. runs peasant harvest (terminal)
          |
          v
   +----------------------+                                          +-----------------------------+
   | peasant binary       |                                          | settings files              |
   | [Container: Go]      |-- 2. loads the selection (YAML) -------->| [Container: YAML, JSON]     |
   |                      |                                          +-----------------------------+
   |                      |
   |                      |                                          +-----------------------------+
   |                      |                                          | agent session stores        |
   |                      |-- 3. reads sessions (JSONL, SQLite) ---->| [Software System, external] |
   |                      |                                          +-----------------------------+
   |                      |
   |                      |                                          +-----------------------------+
   |                      |                                          | sync tree                   |
   |                      |-- 4. writes transcript copies (files) -->| [Container: JSONL files]    |
   |                      |                                          +-----------------------------+
   |                      |
   |                      |                                          +-----------------------------+
   |                      |-- 5. mirrors sessions, commits (SQL) --->| analytics database          |
   |                      |-- 6. indexes entries, metrics (SQL) ---->| [Container: SQLite]         |
   |                      |                                          |                             |
   +----------------------+                                          +-----------------------------+

Key:
  Solid box = element. [Type] = C4 abstraction. Numbered arrow = one interaction, in order.
  Read as "source, N. label (technology), target".
```

### Open a session in the local web app

`peasant web start` forks a `--foreground` child that runs the server, waits for
`/api/v1/health`, and opens the browser. `--no-browser` skips step 2.

```c4
Dynamic diagram: open a session in the local web app

   +-------------------------+
   | developer               |
   | [Person]                |-- 1. runs peasant web start (terminal) --------------+
   +-------------------------+                                                      |
          | 4. opens a session page (browser)                                       |
          |                                                                         |
          |                                                                         |
          v                                                                         v
   +-------------------------+                                       +-----------------------------+
   | peasant web app         |                                       | peasant binary              |
   | [Container: Next.js 15] |<-- 2. opens the app URL (launcher) ---| [Container: Go]             |
   |                         |                                       |                             |
   |                         |-- 3. loads the embedded SPA (HTTP) -->|                             |
   |                         |                                       |                             |
   |                         |-- 5. subscribes to the detail (WS) -->|                             |
   |                         |                                       |                             |
   |                         |                                       |                             |
   |                         |<-- 7. sends the detail payload (WS) --|                             |
   |                         |                                       |                             |
   +-------------------------+                                       +-----------------------------+
                                                                          | 6. reads entries
                                                                          | (SQL)
                                                                          |
                                                                          v
                                                                     +-----------------------------+
                                                                     | analytics database          |
                                                                     | [Container: SQLite]         |
                                                                     +-----------------------------+

Key:
  Solid box = element. [Type] = C4 abstraction. Numbered arrow = one interaction, in order.
  Read as "source, N. label (technology), target".
```

### Publish sessions with /share

The wizard steps are select, labels, redact, and submit. The scan result is cached per
`(level, session)`. The push refuses a session whose capture is not ready for publication, for
example a bounded preview. A capture whose only gap is an over-limit record with a placeholder
entry is ready and publishes with `diagnostics.partial` set.

```c4
Dynamic diagram: publish sessions with /share

   +-------------------------+
   | developer               |
   | [Person]                |
   +-------------------------+
        | 1. opens /share (browser)
        | 3. selects sessions and a level (browser)
        | 6. confirms the review and consents (browser)
        v
   +-------------------------+                                       +-----------------------------+
   | peasant web app         |                                       | peasant binary              |
   | [Container: Next.js 15] |-- 2. lists sessions (HTTP) ---------->| [Container: Go]             |
   |                         |                                       |                             |
   |                         |                                       |                             |
   |                         |-- 4. requests the scan (HTTP) ------->|                             |
   |                         |                                       |                             |
   |                         |                                       |                             |
   |                         |-- 7. requests the push (HTTP) ------->|                             |
   |                         |                                       |                             |
   |                         |                                       |                             |
   +-------------------------+                                       |                             |
                                                                     |                             |
                                                                     |                             |
                                                                     |                             |
                                                                     |                             |
   +-------------------------+                                       |                             |
   | analytics database      |                                       |                             |
   | [Container: SQLite]     |<-- 5. reads publication input (SQL) --|                             |
   |                         |                                       |                             |
   |                         |                                       |                             |
   |                         |<-- 11. saves the receipt (SQL) -------|                             |
   |                         |                                       |                             |
   +-------------------------+                                       |                             |
                                                                     |                             |
   +-------------------------+                                       |                             |
   | settings files          |<-- 8. loads credentials.json (file) --|                             |
   | [Container: YAML, JSON] |                                       +-----------------------------+
   +-------------------------+                                              | 9. checks the schema
                                                                            | version (HTTPS)
                                                                            | 10. publishes each
                                                                            | session (HTTPS)
                                                                            v
                                                                     +-----------------------------+
                                                                     | village                     |
                                                                     | [Software System, external] |
                                                                     +-----------------------------+

Key:
  Solid box = element. [Type] = C4 abstraction. Numbered arrow = one interaction, in order.
  Read as "source, N. label (technology), target".
```

### Upload from a git hook

The hook exists only after `peasant village hooks install --event post-commit` or
`--event pre-push`. The hook always exits 0, so a Village failure never blocks git.

```c4
Dynamic diagram: upload from a git hook

   +----------------------+
   | developer            |
   | [Person]             |
   +----------------------+
          | 1. commits or pushes (git)
          |
          v
   +-----------------------------+
   | git repository              |
   | [Software System, external] |
   +-----------------------------+
          | 2. runs peasant village push from the hook (git hook, shell)
          |
          v
   +----------------------+                                          +-----------------------------+
   | peasant binary       |                                          | village                     |
   | [Container: Go]      |-- 3. reads PR prompt requests (HTTPS) -->| [Software System, external] |
   |                      |                                          |                             |
   |                      |-- 5. publishes each session (HTTPS) ---->|                             |
   |                      |                                          |                             |
   |                      |                                          +-----------------------------+
   |                      |
   |                      |                                          +-----------------------------+
   |                      |                                          | analytics database          |
   |                      |-- 4. reads pushable sessions (SQL) ----->| [Container: SQLite]         |
   |                      |                                          |                             |
   |                      |-- 6. saves the receipt (SQL) ----------->|                             |
   |                      |                                          |                             |
   |                      |                                          +-----------------------------+
   +----------------------+

Key:
  Solid box = element. [Type] = C4 abstraction. Numbered arrow = one interaction, in order.
  Read as "source, N. label (technology), target".
```

### Log in to Village

Village runs the GitHub sign-in in the browser. Peasant never calls the GitHub API for login.

```c4
Dynamic diagram: log in to Village

   +----------------------+
   | developer            |
   | [Person]             |-- 3. signs in with GitHub (browser) --------------------+
   +----------------------+                                                         |
          | 1. runs peasant village login (terminal)                                |
          |                                                                         |
          |                                                                         |
          v                                                                         v
   +----------------------+                                          +-----------------------------+
   | peasant binary       |                                          | village                     |
   | [Container: Go]      |-- 2. opens the login page (browser) ---->| [Software System, external] |
   |                      |                                          |                             |
   |                      |<-- 4. redirects to the callback (HTTP) --|                             |
   |                      |                                          |                             |
   |                      |-- 5. exchanges the code (HTTPS) -------->|                             |
   |                      |                                          |                             |
   |                      |                                          +-----------------------------+
   |                      |
   +----------------------+
          | 6. saves credentials.json (file, mode 0600)
          |
          v
   +-------------------------+
   | settings files          |
   | [Container: YAML, JSON] |
   +-------------------------+

Key:
  Solid box = element. [Type] = C4 abstraction. Numbered arrow = one interaction, in order.
  Read as "source, N. label (technology), target".
```

## Call sequences

Solid arrows (`--->`) are calls. Dotted arrows (`<...`) are returns. `--+` and `<-+` mark a
step inside one participant. A dashed row names a pipeline stage.

### Harvest pipeline

Entry: `cmd/peasant/cmd_harvest.go`. Pipeline: `internal/ingest/pipeline.go`. The durability
point is the SQLite commit, not the file write. A record over `defaults.MaxJSONLRecordBytes`
(256 MiB) becomes a stand-in line before the write and a placeholder entry at index time.

```text
 cmd/peasant    ingest.Pipeline    SourceAdapter    peasant-sync         Indexer        store.Store
      |                |                 |                |                 |                |
      |--+ runHarvest: loadRunConfig, buildSourceConfigs  |                 |                |
      |<-+             |                 |                |                 |                |
      | store.Open (migrations, install salt)             |                 |                |
      |------------------------------------------------------------------------------------->|
      | NewPipeline(WithStore, WithIndexers, ...).Run     |                 |                |
      |--------------->|                 |                |                 |                |
---- DISCOVER, PREPARE, DIFF, FILTER ---------------------------------------------------------
      |                | Discover() for each enabled harness                |                |
      |                |---------------->|                |                 |                |
      |                | []DiscoveredSession              |                 |                |
      |                |<................|                |                 |                |
      |                |--+ diff vs stored state; SelectionMatcher          |                |
      |                |<-+              |                |                 |                |
---- EXTRACT and WRITE (worker pool) ---------------------------------------------------------
      |                | processSession: capture source bytes               |                |
      |                |---------------->|                |                 |                |
      |                |--+ filterOversizedJSONLRecords (> 256 MiB)         |                |
      |                |<-+              |                |                 |                |
      |                | write transcript + metadata (rename)               |                |
      |                |--------------------------------->|                 |                |
      |                |--+ LayeredDetection (--detect-commits)             |                |
      |                |<-+              |                |                 |                |
---- DB INSERT (drain loop) ------------------------------------------------------------------
      |                | mirrorDrainedBatch: MirrorArtifacts                |                |
      |                |-------------------------------------------------------------------->|
---- INDEX (parser pool, one serial writer) --------------------------------------------------
      |                | parseIndexMeta, IndexTranscript  |                 |                |
      |                |--------------------------------------------------->|                |
      |                | entries via Outcome lowering     |                 |                |
      |                |<...................................................|                |
      |                | IndexSessionEntries              |                 |                |
      |                |-------------------------------------------------------------------->|
---- COMPUTE, ANNOTATE, CLEANUP, REPORT, AUDIT -----------------------------------------------
      |                | indexComputeAndFinalize: metrics, ingest_log       |                |
      |                |-------------------------------------------------------------------->|
      | PipelineResult |                 |                |                 |                |
      |<...............|                 |                |                 |                |
```

### Local server start

Entry: `cmd/peasant/cmd_web.go`. Server: `internal/api/server.go`.

```text
 developer        CLI parent     CLI --foreground       store          api.Server          browser
     |                 |                 |                |                 |                 |
     | peasant web start                 |                |                 |                 |
     |---------------->|                 |                |                 |                 |
     |                 | runWebBackground: fork --foreground                |                 |
     |                 |---------------->|                |                 |                 |
     |                 |                 |--+ runWebForeground: config.Load |                 |
     |                 |                 |<-+             |                 |                 |
     |                 |                 | store.Open     |                 |                 |
     |                 |                 |--------------->|                 |                 |
     |                 |                 |--+ NewStoreDataProvider, NewHub  |                 |
     |                 |                 |<-+             |                 |                 |
     |                 |                 | NewServer(embedded web/out).Listen                 |
     |                 |                 |--------------------------------->|                 |
     |                 | poll GET /api/v1/health          |                 |                 |
     |                 |--------------------------------------------------->|                 |
     |                 | 200 OK          |                |                 |                 |
     |                 |<...................................................|                 |
     |                 | browser.Open(localhost:8690)     |                 |                 |
     |                 |--------------------------------------------------------------------->|
     |                 |                 |                |                 | GET / (spaHandler)
     |                 |                 |                |                 |<----------------|
     |                 |                 |                |                 | SPA assets      |
     |                 |                 |                |                 |................>|
```

### Session detail over WebSocket

Client: `web/src/contexts/WebSocketContext.tsx`. Server: `internal/api/websocket.go`,
`internal/api/detail_navigation.go`, `internal/api/snapshot_detail.go`.

```text
 SessionDetailV2 WebSocketContext     api.Hub       DataProvider     transcript          store
        |                |               |                |               |                |
        | useChannel(sessionDetail(id))  |                |               |                |
        |--------------->|               |                |               |                |
        |                | WS subscribe session_detail    |               |                |
        |                |-------------->|                |               |                |
        |                |               |--+ sendSnapshots               |                |
        |                |               |<-+             |               |                |
        |                |               | SessionDetailReadForProvider   |                |
        |                |               |--------------->|               |                |
        |                |               |                | read snapshot or entries       |
        |                |               |                |------------------------------->|
        |                |               |                | stored entries|                |
        |                |               |                |<...............................|
        |                |               |                | BuildSnapshotDetailBytes       |
        |                |               |                |-------------->|                |
        |                |               |                | SessionDetailPayload           |
        |                |               |                |<..............|                |
        |                |               |                |--+ DecorateDetailReadPayload   |
        |                |               |                |<-+            |                |
        |                |               | SessionDetailReadPayload       |                |
        |                |               |<...............|               |                |
        |                | WS session_detail message      |               |                |
        |                |<..............|                |               |                |
        | adaptTranscript: fairtrade viewer               |               |                |
        |<...............|               |                |               |                |
```

### /share scan and publish

Client: `web/src/app/share/`. Server: `internal/api/sync_handler.go`, `internal/push/`,
`internal/village/`.

```text
 ShareWizard          sync handler          push.Pipeline       village client          Village API
      |                     |                     |                    |                     |
---- select step -----------------------------------------------------------------------------
      | GET /api/v1/sync/sessions                 |                    |                     |
      |-------------------->|                     |                    |                     |
      | pushable sessions   |                     |                    |                     |
      |<....................|                     |                    |                     |
---- redact step, cached by level and session ------------------------------------------------
      | GET /api/v1/sync/redactions               |                    |                     |
      |-------------------->|                     |                    |                     |
      |                     | LoadPublicationInput|                    |                     |
      |                     |-------------------->|                    |                     |
      |                     |--+ redact.NewRedactor(level).Detect      |                     |
      |                     |<-+                  |                    |                     |
      | findings by category|                     |                    |                     |
      |<....................|                     |                    |                     |
---- submit step -----------------------------------------------------------------------------
      | POST /api/v1/sync/push                    |                    |                     |
      |-------------------->|                     |                    |                     |
      |                     |--+ LoadCredentials, NewVillageClient     |                     |
      |                     |<-+                  |                    |                     |
      |                     | NewPipeline(...).Run|                    |                     |
      |                     |-------------------->|                    |                     |
      |                     |                     |--+ getTargetSessions, preflight          |
      |                     |                     |<-+                 |                     |
      |                     |                     | negotiate: GetSchemaVersion              |
      |                     |                     |------------------->|                     |
      |                     |                     |                    | GET /schema/version |
      |                     |                     |                    |-------------------->|
      |                     |                     |--+ pushSession: re-redact, map           |
      |                     |                     |<-+                 |                     |
      |                     |                     | PublishAuthoritative                     |
      |                     |                     |------------------->|                     |
      |                     |                     |                    | POST /transcripts/publish
      |                     |                     |                    |-------------------->|
      |                     |                     |                    | publish receipt     |
      |                     |                     |                    |<....................|
      |                     |                     |--+ UpdateOwner if needed, SavePublication|
      |                     |                     |<-+                 |                     |
      |                     |                     | PushAnnotationsSelected                  |
      |                     |                     |------------------->|                     |
      | push result         |                     |                    |                     |
      |<....................|                     |                    |                     |
```

### Village login

`internal/auth/login.go` and `internal/auth/server.go`. The callback port is
`defaults.LoginCallbackPort` (17249) and falls back to an ephemeral port.

```text
  developer             cmd login           internal/auth           browser             Village API
      |                     |                     |                    |                     |
      | peasant village login                     |                    |                     |
      |-------------------->|                     |                    |                     |
      |                     | LoginFrom(village URL)                   |                     |
      |                     |-------------------->|                    |                     |
      |                     |                     |--+ startListener 127.0.0.1:17249, state  |
      |                     |                     |<-+                 |                     |
      |                     |                     | open /api/v1/auth/cli/login              |
      |                     |                     |------------------->|                     |
      |                     |                     |                    | GitHub OAuth        |
      |                     |                     |                    |-------------------->|
      |                     |                     |                    | 302 to loopback     |
      |                     |                     |                    |<....................|
      |                     |                     | GET /callback?code&state                 |
      |                     |                     |<-------------------|                     |
      |                     |                     |--+ handleCallback: check state           |
      |                     |                     |<-+                 |                     |
      |                     |                     | exchangeCode: POST /api/v1/auth/cli/exchange
      |                     |                     |----------------------------------------->|
      |                     |                     | api key, key id, username                |
      |                     |                     |<.........................................|
      |                     |                     |--+ SaveCredentialsFrom (0600)            |
      |                     |                     |<-+                 |                     |
      | logged in           |                     |                    |                     |
      |<....................|                     |                    |                     |
```

### Git hook upload

`internal/githooks/script.go` renders the hook command. `cmd/peasant/cmd_push.go` runs it.

```text
     git          hook script    cmd village push   push.Pipeline         store         Village API
      |                |                 |                |                 |                |
      | post-commit or pre-push          |                |                 |                |
      |--------------->|                 |                |                 |                |
      |                | village push --non-interactive --quiet             |                |
      |                |---------------->|                |                 |                |
      |                |                 | reportWaitingPromptRequests      |                |
      |                |                 |-------------------------------------------------->|
      |                |                 |--+ load config, credentials, redactor             |
      |                |                 |<-+             |                 |                |
      |                |                 | runPushStages: Pipeline.Run      |                |
      |                |                 |--------------->|                 |                |
      |                |                 |                | QueryPushCandidates (repo scope) |
      |                |                 |                |---------------->|                |
      |                |                 |                | negotiate, PublishAuthoritative  |
      |                |                 |                |--------------------------------->|
      |                |                 |                | SavePublication, push_log        |
      |                |                 |                |---------------->|                |
      |                | warnings only   |                |                 |                |
      |                |<................|                |                 |                |
      | exit 0, git never blocked        |                |                 |                |
      |<...............|                 |                |                 |                |
```

### Pull a transcript

`internal/pull/pipeline.go`. Pulled transcripts go to their own tables and never enter
`sessions`, so they are never push candidates.

```text
 cmd pull        pull.Pipeline    village client     Village API      village-pulls         store
     |                 |                 |                |                 |                 |
     | PullTranscript(ref)               |                |                 |                 |
     |---------------->|                 |                |                 |                 |
     |                 | NegotiatePull   |                |                 |                 |
     |                 |---------------->|                |                 |                 |
     |                 |                 | GET /api/v1/schema/version       |                 |
     |                 |                 |--------------->|                 |                 |
     |                 | GetPullTranscript                |                 |                 |
     |                 |---------------->|                |                 |                 |
     |                 |                 | GET /api/v1/pull/transcripts/ID  |                 |
     |                 |                 |--------------->|                 |                 |
     |                 | GetPullTranscriptContent         |                 |                 |
     |                 |---------------->|                |                 |                 |
     |                 |                 | GET .../content, If-None-Match   |                 |
     |                 |                 |--------------->|                 |                 |
     |                 |                 | blob or 304    |                 |                 |
     |                 |                 |<...............|                 |                 |
     |                 | GetPullTranscriptAnnotations     |                 |                 |
     |                 |---------------->|                |                 |                 |
     |                 | write files, pull-manifest.json  |                 |                 |
     |                 |--------------------------------------------------->|                 |
     |                 | CommitPull (pulled_* tables)     |                 |                 |
     |                 |--------------------------------------------------------------------->|
     | PullResult      |                 |                |                 |                 |
     |<................|                 |                |                 |                 |
```

### Kickstart

`cmd/peasant/cmd_kickstart.go` and `internal/tui/kickstart`. The single `Draft.Commit` is the
only write of `config.SelectionConfig`. Kickstart never publishes.

```text
    developer      cmd kickstart     adapters          Program     settings.Draft   ingest.Pipeline
        |                |               |                |               |                |
        | peasant kickstart              |                |               |                |
        |--------------->|               |                |               |                |
        |                | ftueDiscover: Discover()       |               |                |
        |                |-------------->|                |               |                |
        |                | inventory, session listings    |               |                |
        |                |<..............|                |               |                |
        |                | runKickstartFlow: NewProgram   |               |                |
        |                |------------------------------->|               |                |
        | PhaseOAuth: connect or stay local               |               |                |
        |------------------------------------------------>|               |                |
        | PhaseFlow: selection, license, retention        |               |                |
        |------------------------------------------------>|               |                |
        |                |               |                | Draft.Commit: config.SaveAtomic|
        |                |               |                |-------------->|                |
        |                |               |                | PhaseIngest: local harvest     |
        |                |               |                |------------------------------->|
        | PhaseDone: next steps, no publish               |               |                |
        |<................................................|               |                |
```

## Package map

| Package | Role | Main callers |
|---|---|---|
| `cmd/peasant` | Cobra command tree: `harvest`, `web`, `kickstart`, `config`, `village`, `export`, `prune`, `sessions`, `annotate`, `metrics`, `models`, `upgrade`. | user, git hooks |
| `internal/ingest` | Discovery, write, mirror, index pipeline. Adapters, indexers, vocabularies, `SelectionMatcher`. | `harvest`, `kickstart`, api sync ingest |
| `internal/indexformat` | Outcome IR and index result versions. | ingest, store |
| `internal/store` | SQLite schema, migrations, readers, writers. | nearly every command |
| `internal/transcript` | Stored entries to turns to `SessionDetailPayload`. | api, export, tui |
| `internal/api` | HTTP server, WebSocket hub, data providers, sync handler. | `web start`, `tui` |
| `internal/push` | Publication pipeline and the push wizard. | `village push`, api sync handler |
| `internal/pull` | Pull pipeline for Village transcripts and annotations. | `village transcripts pull` |
| `internal/village` | Village HTTP client. | push, pull, api |
| `internal/auth` | Loopback OAuth login, `credentials.json`. | `village login`, api sync handler |
| `internal/githooks` | Installs, checks, and removes the upload hooks. | `village hooks` |
| `internal/gitops` | Read-only git for the code map and review. | codemap |
| `internal/codemap`, `internal/codegraph` | Code map and change review graphs. | api map and review routes |
| `internal/config` | Settings, selection, redaction policy. | CLI, api, push |
| `internal/defaults` | Paths, ports, limits, shared constants. | everywhere |
| `internal/sessionvisibility`, `internal/selectionprojection` | Scope lists and pickers to the selection. The scope is not access control. | api, kickstart |
| `internal/export` | Redacted session and annotation export. | `peasant export` |
| `internal/annotations` | Annotation type registry and validation. | api, `annotate` |
| `internal/metrics` | Session metrics and classifiers. | ingest, `metrics` |
| `internal/sessionorigin`, `internal/salt`, `internal/title`, `internal/projectlabel`, `internal/codexstate` | Origin labels, install salt, title redaction, project labels, Codex rollout pointer. | ingest, store, push |
| `internal/tui` and subpackages | Kickstart, config editor, harvest progress, push wizard, layout kit. | `kickstart`, `config`, `harvest`, `village push` |
| `internal/mock`, `internal/redactmock` | Mock data provider and generated mock redactions. | `web start`, `cmd/gen-mock-redactions` |
| `cmd/gen-*`, `cmd/peasant-guided-screenshots`, `cmd/peasant-origin-audit` | Code generators and opt-in audit tools. | developers |
