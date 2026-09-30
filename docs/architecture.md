# Peasant architecture

This page shows how the parts of Peasant connect. It goes from the high level to the low level:

1. [System context](#system-context): Peasant, its user, and the systems it talks to.
2. [Containers](#containers): the runtime units and data stores inside Peasant.
3. [Components](#components): the Go packages and the web modules inside each container.
4. [Deployment](#deployment): where each container runs on the developer workstation.
5. [Dynamic views](#dynamic-views): the main flows between containers, in numbered order.
6. [Call sequences](#call-sequences): the same flows at the level of functions and files.
7. [Package map](#package-map): the role of each package under `internal/` and `cmd/`.

All diagrams are Mermaid, so GitHub renders them in place. The structural views follow the C4
model vocabulary of [`.claude/skills/c4-model`](../.claude/skills/c4-model/SKILL.md) and
are drawn as Mermaid flowcharts: each box names its element, its C4 type in square brackets,
and a short description, and a dashed outline marks a boundary. Every arrow is one relationship,
read as "source, label (technology), target". The dynamic views and the call sequences are
Mermaid sequence diagrams. Colors follow the C4 convention: dark blue for people, blue for
Peasant's own elements, and grey for external systems.

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

```mermaid
flowchart TB
  dev["<b>developer</b><br/>[Person]<br/>Works with AI coding agents<br/>and reviews the sessions."]:::person
  peasant["<b>peasant</b><br/>[Software System]<br/>Harvests, indexes, shows, and<br/>publishes agent sessions."]:::system
  stores["<b>agent session stores</b><br/>[Software System, external]<br/>Claude Code, Codex, Cursor,<br/>OpenCode, Pi, and Strike."]:::external
  git["<b>git repository</b><br/>[Software System, external]<br/>The project repository<br/>of the developer."]:::external
  village["<b>village</b><br/>[Software System, external]<br/>Registry and commons for<br/>published transcripts."]:::external
  models["<b>models.dev</b><br/>[Software System, external]<br/>Public catalog of model<br/>prices and limits."]:::external
  releases["<b>GitHub Releases</b><br/>[Software System, external]<br/>Hosts the peasant<br/>release archives."]:::external

  dev -->|"runs commands and opens the local web app<br/>(terminal, browser)"| peasant
  peasant -->|"reads sessions<br/>(JSONL, SQLite)"| stores
  peasant -->|"reads commits, sets hooks<br/>(git CLI)"| git
  git -->|"runs the upload hook<br/>(shell)"| peasant
  peasant -->|"publishes and pulls<br/>(HTTPS)"| village
  peasant -->|"syncs model prices<br/>(HTTPS)"| models
  peasant -->|"downloads upgrades<br/>(HTTPS)"| releases

  classDef person fill:#08427b,stroke:#052e56,color:#fff
  classDef system fill:#1168bd,stroke:#0b4884,color:#fff
  classDef container fill:#438dd5,stroke:#2e6295,color:#fff
  classDef component fill:#85bbf0,stroke:#5d82a8,color:#000
  classDef external fill:#999999,stroke:#6b6b6b,color:#fff
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

```mermaid
flowchart TB
  dev["<b>developer</b><br/>[Person]<br/>Works with AI coding agents<br/>and reviews the sessions."]:::person

  subgraph peasant["peasant [Software System]"]
    web["<b>peasant web app</b><br/>[Container: Next.js 15]<br/>Static SPA. Renders transcripts<br/>with fairtrade. Hosts /share."]:::container
    bin["<b>peasant binary</b><br/>[Container: Go]<br/>CLI commands. Under web start,<br/>serves REST, WebSocket, and the SPA."]:::container
    db[("<b>analytics database</b><br/>[Container: SQLite]<br/>peasant.db: sessions, entries,<br/>commits, publications, pulls.")]:::container
    sync[("<b>sync tree</b><br/>[Container: JSONL files]<br/>peasant-sync transcript copies<br/>and village-pulls downloads.")]:::container
    settings[("<b>settings files</b><br/>[Container: YAML, JSON]<br/>config.yaml and<br/>credentials.json.")]:::container
  end

  stores["<b>agent session stores</b><br/>[Software System, external]<br/>Local harness stores."]:::external
  git["<b>git repository</b><br/>[Software System, external]<br/>The project repository."]:::external
  village["<b>village</b><br/>[Software System, external]<br/>Transcript registry."]:::external

  dev -->|"opens localhost:8690<br/>(browser)"| web
  dev -->|"runs peasant commands<br/>(terminal)"| bin
  web -->|"calls the API<br/>(HTTP REST, WebSocket)"| bin
  bin -->|"reads and writes<br/>(SQL)"| db
  bin -->|"writes and reads<br/>(files)"| sync
  bin -->|"reads and writes<br/>(YAML, JSON)"| settings
  bin -->|"reads session files and rows<br/>(JSONL, SQLite)"| stores
  bin -->|"reads the log, sets hooks<br/>(git CLI)"| git
  bin -->|"publishes and pulls<br/>(HTTPS, JSON, multipart)"| village

  style peasant fill:none,stroke:#444,stroke-dasharray:6 4

  classDef person fill:#08427b,stroke:#052e56,color:#fff
  classDef system fill:#1168bd,stroke:#0b4884,color:#fff
  classDef container fill:#438dd5,stroke:#2e6295,color:#fff
  classDef component fill:#85bbf0,stroke:#5d82a8,color:#000
  classDef external fill:#999999,stroke:#6b6b6b,color:#fff
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

```mermaid
flowchart TB
  dev["<b>developer</b><br/>[Person]"]:::person

  subgraph binary["peasant binary [Container: Go]"]
    cli["<b>cli</b><br/>[Component: Go, Cobra]<br/>cmd/peasant: runHarvest<br/>builds the pipeline."]:::component
    config["<b>config</b><br/>[Component: Go package]<br/>internal/config: settings<br/>and SelectionMatcher."]:::component
    pipeline["<b>ingest pipeline</b><br/>[Component: Go package]<br/>internal/ingest: discover, diff,<br/>filter, write, mirror, index, compute."]:::component
    adapters["<b>harness adapters</b><br/>[Component: Go package]<br/>Adapter, indexer, and<br/>vocabulary for each harness."]:::component
    store["<b>store</b><br/>[Component: Go package]<br/>internal/store: SQLite<br/>schema and queries."]:::component
  end

  db[("<b>analytics database</b><br/>[Container: SQLite]")]:::container
  sync[("<b>sync tree</b><br/>[Container: JSONL files]")]:::container
  stores["<b>agent session stores</b><br/>[Software System, external]"]:::external

  dev -->|"runs peasant harvest<br/>(terminal)"| cli
  cli -->|"loads settings<br/>(Go call)"| config
  cli -->|"runs Pipeline.Run<br/>(Go call)"| pipeline
  pipeline -->|"discovers and parses sessions<br/>(Go call)"| adapters
  adapters -->|"reads files and rows<br/>(JSONL, SQLite)"| stores
  pipeline -->|"writes transcript copies<br/>(files)"| sync
  pipeline -->|"mirrors and indexes<br/>(Go call)"| store
  store -->|"reads and writes<br/>(SQL)"| db

  style binary fill:none,stroke:#444,stroke-dasharray:6 4

  classDef person fill:#08427b,stroke:#052e56,color:#fff
  classDef system fill:#1168bd,stroke:#0b4884,color:#fff
  classDef container fill:#438dd5,stroke:#2e6295,color:#fff
  classDef component fill:#85bbf0,stroke:#5d82a8,color:#000
  classDef external fill:#999999,stroke:#6b6b6b,color:#fff
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
| village client | `internal/village` | Village HTTP client for publish, pull, and schema negotiation. |
| auth | `internal/auth` | Loopback OAuth login and `credentials.json`. |
| store | `internal/store` | Same component as on the harvest path. |

The canonical detail path is `store` entries, then `transcript.EntriesToTurns`, then
`transcript.SessionToDetail`. `api.EntriesToTurns` and `api.SessionToDetail` are thin wrappers
over it. Sessions indexed into a managed generation take the snapshot branch,
`transcript.BuildSnapshotDetailBytes`, which produces the same `SessionDetailPayload`.

```mermaid
flowchart TB
  web["<b>peasant web app</b><br/>[Container: Next.js 15]"]:::container

  subgraph binary["peasant binary [Container: Go]"]
    api["<b>api</b><br/>[Component: Go package]<br/>internal/api: routes, WebSocket hub,<br/>SPA handler, sync handler."]:::component
    transcript["<b>transcript</b><br/>[Component: Go package]<br/>Builds turns and the<br/>session detail payload."]:::component
    push["<b>push</b><br/>[Component: Go package]<br/>internal/push: redacts, maps,<br/>and uploads selected sessions."]:::component
    vclient["<b>village client</b><br/>[Component: Go package]<br/>internal/village:<br/>Village HTTP client."]:::component
    auth["<b>auth</b><br/>[Component: Go package]<br/>internal/auth: loopback<br/>OAuth login."]:::component
    store["<b>store</b><br/>[Component: Go package]<br/>internal/store: SQLite<br/>schema and queries."]:::component
  end

  db[("<b>analytics database</b><br/>[Container: SQLite]")]:::container
  settings[("<b>settings files</b><br/>[Container: YAML, JSON]")]:::container
  village["<b>village</b><br/>[Software System, external]"]:::external

  web -->|"calls REST and WebSocket<br/>(HTTP, WS)"| api
  api -->|"reads sessions<br/>(Go call)"| store
  api -->|"builds session detail<br/>(Go call)"| transcript
  api -->|"runs the publish pipeline<br/>(Go call)"| push
  api -->|"loads credentials, starts login<br/>(Go call)"| auth
  push -->|"reads publication input, saves receipts<br/>(Go call)"| store
  push -->|"uploads<br/>(Go call)"| vclient
  auth -->|"reads and writes credentials.json<br/>(file)"| settings
  auth -->|"exchanges the login code<br/>(HTTPS)"| village
  vclient -->|"sends requests<br/>(HTTPS, JSON, multipart)"| village
  store -->|"reads and writes<br/>(SQL)"| db

  style binary fill:none,stroke:#444,stroke-dasharray:6 4

  classDef person fill:#08427b,stroke:#052e56,color:#fff
  classDef system fill:#1168bd,stroke:#0b4884,color:#fff
  classDef container fill:#438dd5,stroke:#2e6295,color:#fff
  classDef component fill:#85bbf0,stroke:#5d82a8,color:#000
  classDef external fill:#999999,stroke:#6b6b6b,color:#fff
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

```mermaid
flowchart TB
  dev["<b>developer</b><br/>[Person]"]:::person

  subgraph webapp["peasant web app [Container: Next.js 15]"]
    pages["<b>section pages</b><br/>[Component: React]<br/>Home, session viewer, analytics,<br/>review, map. Renders with fairtrade."]:::component
    share["<b>share wizard</b><br/>[Component: React]<br/>ShareWizardClient: select,<br/>labels, redact, submit."]:::component
    channel["<b>channel store</b><br/>[Component: TypeScript]<br/>WebSocketContext: one socket,<br/>cached topic data."]:::component
    rest["<b>REST clients</b><br/>[Component: TypeScript]<br/>lib/api fetch helpers."]:::component
  end

  bin["<b>peasant binary</b><br/>[Container: Go]"]:::container

  dev -->|"browses sessions<br/>(browser)"| pages
  dev -->|"publishes sessions<br/>(browser)"| share
  pages -->|"subscribes to topics<br/>(React hook)"| channel
  pages -->|"fetches summaries, map, review<br/>(function call)"| rest
  share -->|"lists, scans, pushes<br/>(function call)"| rest
  channel -->|"subscribes on /api/v1/ws<br/>(WebSocket)"| bin
  rest -->|"calls /api/v1<br/>(HTTP, JSON)"| bin

  style webapp fill:none,stroke:#444,stroke-dasharray:6 4

  classDef person fill:#08427b,stroke:#052e56,color:#fff
  classDef system fill:#1168bd,stroke:#0b4884,color:#fff
  classDef container fill:#438dd5,stroke:#2e6295,color:#fff
  classDef component fill:#85bbf0,stroke:#5d82a8,color:#000
  classDef external fill:#999999,stroke:#6b6b6b,color:#fff
```

## Deployment

Everything runs on the developer workstation. Paths follow XDG: the data dir is
`$XDG_DATA_HOME/peasant` (default `~/.local/share/peasant`), and the config dir is
`$XDG_CONFIG_HOME/peasant` (default `~/.config/peasant`). The `--data-dir` and `--config-dir`
flags override them.

```mermaid
flowchart TB
  subgraph workstation["developer workstation [Deployment Node: Linux, macOS, or WSL]"]
    subgraph browser["web browser [Deployment Node: browser]"]
      web["<b>peasant web app</b><br/>[Container: Next.js 15]"]:::container
    end
    subgraph process["peasant [Deployment Node: OS process]"]
      bin["<b>peasant binary</b><br/>[Container: Go]"]:::container
    end
    subgraph datadir["data dir [Deployment Node: XDG_DATA_HOME/peasant]"]
      db[("<b>analytics database</b><br/>[Container: SQLite]<br/>peasant.db")]:::container
      sync[("<b>sync tree</b><br/>[Container: JSONL files]<br/>peasant-sync, village-pulls")]:::container
    end
    subgraph configdir["config dir [Deployment Node: XDG_CONFIG_HOME/peasant]"]
      settings[("<b>settings files</b><br/>[Container: YAML, JSON]<br/>config.yaml, credentials.json")]:::container
    end
  end

  village["<b>village</b><br/>[Software System, external]"]:::external

  web -->|"calls the API on port 8690<br/>(HTTP, WebSocket)"| bin
  bin -->|"reads and writes<br/>(SQL)"| db
  bin -->|"reads and writes<br/>(files)"| sync
  bin -->|"reads and writes<br/>(files)"| settings
  bin -->|"publishes and pulls<br/>(HTTPS)"| village

  style workstation fill:none,stroke:#444,stroke-dasharray:6 4
  style browser fill:none,stroke:#444,stroke-dasharray:6 4
  style process fill:none,stroke:#444,stroke-dasharray:6 4
  style datadir fill:none,stroke:#444,stroke-dasharray:6 4
  style configdir fill:none,stroke:#444,stroke-dasharray:6 4

  classDef person fill:#08427b,stroke:#052e56,color:#fff
  classDef system fill:#1168bd,stroke:#0b4884,color:#fff
  classDef container fill:#438dd5,stroke:#2e6295,color:#fff
  classDef component fill:#85bbf0,stroke:#5d82a8,color:#000
  classDef external fill:#999999,stroke:#6b6b6b,color:#fff
```

## Dynamic views

### Harvest local agent sessions

```mermaid
sequenceDiagram
  autonumber
  actor dev as developer
  participant bin as peasant binary (Go)
  participant settings as settings files
  participant stores as agent session stores
  participant sync as sync tree
  participant db as analytics database
  dev->>bin: runs peasant harvest (terminal)
  bin->>settings: loads the selection (YAML)
  bin->>stores: discovers and reads sessions (JSONL, SQLite)
  bin->>sync: writes transcript copies (files)
  bin->>db: mirrors sessions and commits (SQL)
  bin->>db: indexes entries and metrics (SQL)
```

### Open a session in the local web app

`peasant web start` forks a `--foreground` child that runs the server, waits for
`/api/v1/health`, and opens the browser. `--no-browser` skips step 2.

```mermaid
sequenceDiagram
  autonumber
  actor dev as developer
  participant web as peasant web app (browser)
  participant bin as peasant binary (Go)
  participant db as analytics database
  dev->>bin: runs peasant web start (terminal)
  bin->>web: opens the app URL (OS launcher)
  web->>bin: loads the embedded SPA (HTTP)
  dev->>web: opens a session page (browser)
  web->>bin: subscribes to session_detail (WebSocket)
  bin->>db: reads entries (SQL)
  bin-->>web: sends SessionDetailReadPayload (WebSocket)
```

### Publish sessions with /share

The wizard steps are select, labels, redact, and submit. The scan result is cached per
`(level, session)`. The push refuses a session whose capture is not ready for publication, for
example a bounded preview. A capture whose only gap is an over-limit record with a placeholder
entry is ready and publishes with `diagnostics.partial` set.

```mermaid
sequenceDiagram
  autonumber
  actor dev as developer
  participant web as peasant web app (browser)
  participant bin as peasant binary (Go)
  participant db as analytics database
  participant settings as settings files
  participant village as village
  dev->>web: opens /share (browser)
  web->>bin: lists pushable sessions (HTTP)
  dev->>web: selects sessions and a redaction level
  web->>bin: requests the redaction scan (HTTP)
  bin->>db: reads publication input (SQL)
  dev->>web: confirms the review and consents
  web->>bin: requests the push (HTTP)
  bin->>settings: loads credentials.json (file)
  bin->>village: checks the schema version (HTTPS)
  bin->>village: publishes each session (HTTPS, multipart)
  bin->>db: saves the publication receipt (SQL)
```

### Upload from a git hook

The hook exists only after `peasant village hooks install --event post-commit` or
`--event pre-push`. The hook always exits 0, so a Village failure never blocks git.

```mermaid
sequenceDiagram
  autonumber
  actor dev as developer
  participant git as git repository
  participant bin as peasant binary (Go)
  participant db as analytics database
  participant village as village
  dev->>git: commits or pushes (git)
  git->>bin: runs peasant village push from the hook (shell)
  bin->>village: reads waiting PR prompt requests (HTTPS)
  bin->>db: reads pushable sessions (SQL)
  bin->>village: publishes each session (HTTPS)
  bin->>db: saves the publication receipt (SQL)
```

### Log in to Village

Village runs the GitHub sign-in in the browser. Peasant never calls the GitHub API for login.
Steps 2 and 3 happen in the developer's browser.

```mermaid
sequenceDiagram
  autonumber
  actor dev as developer
  participant bin as peasant binary (Go)
  participant village as village
  participant settings as settings files
  dev->>bin: runs peasant village login (terminal)
  bin->>village: opens the login page (browser)
  dev->>village: signs in with GitHub (browser)
  village->>bin: redirects to the 127.0.0.1 callback (HTTP)
  bin->>village: exchanges the code for an API key (HTTPS)
  bin->>settings: saves credentials.json (file, mode 0600)
```

## Call sequences

Solid arrows are calls. Dashed arrows are returns. An arrow from a participant to itself is a
step inside that participant. A note across all participants names a pipeline stage.

### Harvest pipeline

Entry: `cmd/peasant/cmd_harvest.go`. Pipeline: `internal/ingest/pipeline.go`. The durability
point is the SQLite commit, not the file write. A record over `defaults.MaxJSONLRecordBytes`
(256 MiB) becomes a stand-in line before the write and a placeholder entry at index time.

```mermaid
sequenceDiagram
  participant cli as cmd/peasant
  participant pipe as ingest.Pipeline
  participant ad as SourceAdapter
  participant sync as peasant-sync
  participant idx as TranscriptIndexer
  participant st as store.Store
  cli->>cli: runHarvest: loadRunConfig, buildSourceConfigs
  cli->>st: store.Open (migrations, install salt)
  cli->>pipe: NewPipeline(WithStore, WithIndexers, ...).Run
  Note over cli,st: DISCOVER, PREPARE, DIFF, FILTER
  pipe->>ad: Discover() for each enabled harness
  ad-->>pipe: []DiscoveredSession
  pipe->>pipe: diff against stored state, apply SelectionMatcher
  Note over cli,st: EXTRACT and WRITE (worker pool)
  pipe->>ad: processSession: capture source bytes
  pipe->>pipe: filterOversizedJSONLRecords (over 256 MiB)
  pipe->>sync: write transcript and metadata (rename)
  pipe->>pipe: LayeredDetection (--detect-commits)
  Note over cli,st: DB INSERT (drain loop)
  pipe->>st: mirrorDrainedBatch: MirrorArtifacts
  Note over cli,st: INDEX (parser pool, one serial writer)
  pipe->>idx: parseIndexMeta, IndexTranscript
  idx->>idx: record kind to Outcome to central lowering
  idx-->>pipe: entries and omission placeholders
  pipe->>st: IndexSessionEntries
  Note over cli,st: COMPUTE, ANNOTATE, CLEANUP, REPORT, AUDIT
  pipe->>st: indexComputeAndFinalize: metrics, ingest_log
  pipe-->>cli: PipelineResult
```

### Local server start

Entry: `cmd/peasant/cmd_web.go`. Server: `internal/api/server.go`.

```mermaid
sequenceDiagram
  actor dev as developer
  participant parent as CLI parent
  participant child as CLI --foreground
  participant st as store
  participant srv as api.Server
  participant br as browser
  dev->>parent: peasant web start
  parent->>child: runWebBackground: fork --foreground
  child->>child: runWebForeground: config.Load
  child->>st: store.Open
  child->>child: NewStoreDataProvider, NewProgressiveProvider, NewHub
  child->>srv: NewServer(embedded web/out).Listen
  parent->>srv: poll GET /api/v1/health
  srv-->>parent: 200 OK
  parent->>br: browser.Open(http://localhost:8690)
  br->>srv: GET / (spaHandler)
  srv-->>br: embedded Next.js static export
```

### Session detail over WebSocket

Client: `web/src/contexts/WebSocketContext.tsx`. Server: `internal/api/websocket.go`,
`internal/api/detail_navigation.go`, `internal/api/snapshot_detail.go`.

```mermaid
sequenceDiagram
  participant view as SessionDetailV2
  participant ws as WebSocketContext
  participant hub as api.Hub
  participant prov as DataProvider
  participant tr as transcript
  participant st as store
  view->>ws: useChannel(subscribe.sessionDetail(id))
  ws->>hub: WS subscribe, topic session_detail
  hub->>hub: sendSnapshots
  hub->>prov: SessionDetailReadForProvider
  prov->>st: read generation snapshot or stored entries
  st-->>prov: stored entries
  prov->>tr: BuildSnapshotDetailBytes or SessionToDetailValidated
  tr-->>prov: SessionDetailPayload
  prov->>prov: DecorateDetailReadPayload (relationship navigation)
  prov-->>hub: SessionDetailReadPayload
  hub-->>ws: WS session_detail message
  ws-->>view: adaptTranscript, fairtrade TranscriptViewer
```

### /share scan and publish

Client: `web/src/app/share/`. Server: `internal/api/sync_handler.go`, `internal/push/`,
`internal/village/`.

```mermaid
sequenceDiagram
  participant wiz as ShareWizardClient
  participant sh as api sync handler
  participant push as push.Pipeline
  participant vc as village client
  participant vapi as Village API
  Note over wiz,vapi: select step
  wiz->>sh: GET /api/v1/sync/sessions?view=grouped
  sh-->>wiz: pushable sessions
  Note over wiz,vapi: redact step, cached by level and session
  wiz->>sh: GET /api/v1/sync/redactions?session_id and level
  sh->>push: LoadPublicationInput
  sh->>sh: redact.NewRedactor(level).Detect
  sh-->>wiz: findings grouped by category
  Note over wiz,vapi: submit step
  wiz->>sh: POST /api/v1/sync/push
  sh->>sh: auth.LoadCredentials, village.NewVillageClient
  sh->>push: push.NewPipeline(...).Run
  push->>push: getTargetSessions, preflight (ValidatePublicationInput)
  push->>vc: negotiate: GetSchemaVersion
  vc->>vapi: GET /api/v1/schema/version
  push->>push: pushSession: re-redact, map to AuthoritativePublishRequest
  push->>vc: PublishAuthoritative
  vc->>vapi: POST /api/v1/transcripts/publish (multipart)
  vapi-->>vc: AuthoritativePublishResponse
  push->>push: UpdateOwner if needed, SavePublication, push_log
  push->>vc: PushAnnotationsSelected
  sh-->>wiz: push result
```

### Village login

`internal/auth/login.go` and `internal/auth/server.go`. The callback port is
`defaults.LoginCallbackPort` (17249) and falls back to an ephemeral port.

```mermaid
sequenceDiagram
  actor dev as developer
  participant cmd as cmd login
  participant auth as internal/auth
  participant br as browser
  participant vapi as Village API
  dev->>cmd: peasant village login
  cmd->>auth: LoginFrom(village URL)
  auth->>auth: startListener on 127.0.0.1:17249, new state
  auth->>br: open /api/v1/auth/cli/login?port and state
  br->>vapi: GET login, GitHub OAuth in the browser
  vapi-->>br: redirect to the loopback callback
  br->>auth: GET /callback?code and state
  auth->>auth: handleCallback: check state
  auth->>vapi: exchangeCode: POST /api/v1/auth/cli/exchange
  vapi-->>auth: api key, key id, username
  auth->>auth: SaveCredentialsFrom: credentials.json (0600)
  cmd-->>dev: logged in
```

### Git hook upload

`internal/githooks/script.go` renders the hook command. `cmd/peasant/cmd_push.go` runs it.

```mermaid
sequenceDiagram
  participant git as git
  participant hook as hook script
  participant cmd as cmd village push
  participant push as push.Pipeline
  participant st as store
  participant vapi as Village API
  git->>hook: post-commit or pre-push
  hook->>cmd: peasant village push --non-interactive --quiet --timeout
  cmd->>vapi: reportWaitingPromptRequests: GET /api/v1/users/me/prompt-requests
  cmd->>cmd: load config, credentials, redactor
  cmd->>push: runPushStages: Pipeline.Run
  push->>st: QueryPushCandidates (repository scope, selection)
  push->>vapi: negotiate, PublishAuthoritative for each session
  push->>st: SavePublication, push_log
  cmd-->>hook: warnings only
  hook-->>git: exit 0, git is never blocked
```

### Pull a transcript

`internal/pull/pipeline.go`. Pulled transcripts go to their own tables and never enter
`sessions`, so they are never push candidates.

```mermaid
sequenceDiagram
  participant cmd as cmd pull
  participant pull as pull.Pipeline
  participant vc as village client
  participant vapi as Village API
  participant fs as village-pulls
  participant st as store
  cmd->>pull: PullTranscript(ref)
  pull->>vc: NegotiatePull
  vc->>vapi: GET /api/v1/schema/version
  pull->>vc: GetPullTranscript
  vc->>vapi: GET /api/v1/pull/transcripts/ID
  pull->>vc: GetPullTranscriptContent
  vc->>vapi: GET .../content with If-None-Match
  vapi-->>vc: blob or 304
  pull->>vc: GetPullTranscriptAnnotations
  pull->>fs: write files and pull-manifest.json
  pull->>st: CommitPull (pulled_transcripts, pulled_annotations)
  pull-->>cmd: PullResult
```

### Kickstart

`cmd/peasant/cmd_kickstart.go` and `internal/tui/kickstart`. The single `Draft.Commit` is the
only write of `config.SelectionConfig`. Kickstart never publishes.

```mermaid
sequenceDiagram
  actor dev as developer
  participant cmd as cmd kickstart
  participant ad as adapters
  participant prog as kickstart.Program
  participant draft as settings.Draft
  participant pipe as ingest.Pipeline
  dev->>cmd: peasant kickstart
  cmd->>ad: ftueDiscover: Discover() for each harness
  ad-->>cmd: inventory and session listings
  cmd->>prog: runKickstartFlow: NewProgram
  dev->>prog: PhaseOAuth: connect or stay local
  dev->>prog: PhaseFlow: selection, publication, license, retention
  prog->>draft: Draft.Commit: config.SaveAtomic (SelectionConfig)
  prog->>pipe: PhaseIngest: local harvest with progress
  prog-->>dev: PhaseDone: next steps, no publish
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
