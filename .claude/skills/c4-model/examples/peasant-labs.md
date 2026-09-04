# The peasant-labs workspace in C4

Six diagrams of this workspace, one per hand-drawn diagram type, each with the model tables it
was drawn from. The Code diagram is generated, not drawn, so it has no example. Every block passes `scripts/c4-lint.py`.

Provenance: derived on 2026-09-03 from the workspace guide (`CLAUDE.md` at the workspace root),
`peasant/develop/go.mod`, `peasant/develop/web/package.json`, `village/develop/backend/go.mod`,
`village/develop/frontend/package.json`, `village/develop/docker-compose.yml`, and
`village/develop/docs/railway-cloudflare-r2-activation.md`. Re-derive from those sources before
you reuse a diagram in a shipped document. The diagrams are examples of the notation first and
a description of the workspace second.

The libraries `schema`, `redact`, and `fairtrade` appear on no diagram as boxes. They are not
containers. They appear as technology text: fairtrade in the web container's description,
the schema wire contract in the publish relationship.

## System Landscape: peasant-labs

Elements:

| Name | Type | Technology | Description |
|---|---|---|---|
| developer | Person | | Records coding sessions with an AI agent. |
| reader | Person | | Browses published transcripts. |
| peasant | Software System | | Ingests, indexes, and serves the developer's agent sessions locally. |
| village | Software System | | Registry and commons for published transcripts. |
| agent session stores | Software System, external | | OpenCode, Claude Code, and Cursor local stores. |

Relationships:

| Source | Target | Intent | Technology |
|---|---|---|---|
| developer | peasant | reviews, redacts, and shares sessions | browser |
| reader | village | browses | browser |
| peasant | village | publishes a session | HTTPS |
| peasant | agent session stores | reads session files and rows | JSONL, SQLite |

```c4
System Landscape diagram: peasant-labs

+-----------------------------+                                  +-----------------------------+
| developer                   |                                  | reader                      |
| [Person]                    |                                  | [Person]                    |
| Records coding sessions     |                                  | Browses published           |
| with an AI agent.           |                                  | transcripts.                |
+-----------------------------+                                  +-----------------------------+
              |                                                                |
              | reviews, redacts, and                                          | browses
              | shares sessions (browser)                                      | (browser)
              v                                                                v
+-----------------------------+                                  +-----------------------------+
| peasant                     |-- publishes a session (HTTPS) -->| village                     |
| [Software System]           |                                  | [Software System]           |
| Ingests, indexes, and       |                                  | Registry and commons for    |
| serves the developer's      |                                  | published transcripts.      |
| agent sessions locally.     |                                  |                             |
+-----------------------------+                                  +-----------------------------+
              |
              | reads session files and rows (JSONL, SQLite)
              v
+-----------------------------+
| agent session stores        |
| [Software System, external] |
| OpenCode, Claude Code, and  |
| Cursor local stores.        |
+-----------------------------+

Key:
  Solid box = element. [Type] = C4 abstraction. "external" = outside the scope of this diagram.
  Arrow = one relationship, read as "source, label (technology), target".
```

## System Context: peasant

Same elements as the landscape, with village now external because the scope is peasant alone.
The reader is not connected to peasant and so does not appear.

```c4
System Context diagram: peasant

+-----------------------------+
| developer                   |
| [Person]                    |
| Records coding sessions     |
| with an AI agent.           |
+-----------------------------+
              |
              | reviews, redacts, and
              | shares sessions (browser)
              v
+-----------------------------+                                  +-----------------------------+
| peasant                     |-- publishes a session (HTTPS) -->| village                     |
| [Software System]           |                                  | [Software System, external] |
| Ingests, indexes, and       |                                  | Registry and commons for    |
| serves the developer's      |                                  | published transcripts.      |
| agent sessions locally.     |                                  |                             |
+-----------------------------+                                  +-----------------------------+
              |
              | reads session files and rows (JSONL, SQLite)
              v
+-----------------------------+
| agent session stores        |
| [Software System, external] |
| OpenCode, Claude Code, and  |
| Cursor local stores.        |
+-----------------------------+

Key:
  Solid box = element. [Type] = C4 abstraction. "external" = outside the scope of this diagram.
  Arrow = one relationship, read as "source, label (technology), target".
```

## Container: peasant

Elements:

| Name | Type | Technology | Description |
|---|---|---|---|
| peasant web | Container | Next.js 15 | Renders transcripts with fairtrade. Hosts /share. |
| peasant backend | Container | Go | Ingests, indexes, redacts, and serves sessions. |
| peasant session store | Container | SQLite | Sessions, turns, commits, and share state. |

Relationships:

| Source | Target | Intent | Technology |
|---|---|---|---|
| developer | peasant web | opens the local app | browser, HTTP |
| peasant web | peasant backend | subscribes to session detail | WebSocket |
| peasant backend | peasant session store | reads and writes | SQL |
| peasant backend | agent session stores | reads session files and rows | JSONL, SQLite |
| peasant backend | village | publishes a session | HTTPS, schema wire |

```c4
Container diagram: peasant

+-----------------------------+
| developer                   |
| [Person]                    |
+-----------------------------+
              |
              | opens the local app (browser, HTTP)
              v
+== peasant [Software System] ===================================================================+
|                                                                                                |
|  +-----------------------------+                                                               |
|  | peasant web                 |                                                               |
|  | [Container: Next.js 15]     |                                                               |
|  | Renders transcripts with    |                                                               |
|  | fairtrade. Hosts /share.    |                                                               |
|  +-----------------------------+                                                               |
|                |                                                                               |
|                | subscribes to session detail (WebSocket)                                      |
|                v                                                                               |
|  +-----------------------------+                                +-----------------------------+ |
|  | peasant backend             |-- reads and writes (SQL) ----->| peasant session store       | |
|  | [Container: Go]             |                                | [Container: SQLite]         | |
|  | Ingests, indexes, redacts,  |                                | Sessions, turns, commits,   | |
|  | and serves sessions.        |                                | and share state.            | |
|  +-----------------------------+                                +-----------------------------+ |
|           |                 |                                                                  |
|           |                 +-- publishes a session (HTTPS, schema wire) --+                   |
|           |                                                                |                   |
+===========|================================================================|===================+
            | reads session files and rows                                   |
            | (JSONL, SQLite)                                                v
            v                                                  +-----------------------------+
+-----------------------------+                                | village                     |
| agent session stores        |                                | [Software System, external] |
| [Software System, external] |                                | Registry and commons for    |
| OpenCode, Claude Code, and  |                                | published transcripts.      |
| Cursor local stores.        |                                +-----------------------------+
+-----------------------------+

Key:
  Solid box = element. Double-line box = boundary. [Type] = C4 abstraction.
  "external" = outside the scope of this diagram. Arrow = one relationship, read as
  "source, label (technology), target".
```

## Component: peasant backend

Elements, all `[Component: Go package]`:

| Name | Package | Description |
|---|---|---|
| api | `internal/api` | Serves the session detail WebSocket topic. |
| transcript | `internal/transcript` | Turns stored entries into turns. |
| ingest | `internal/ingest` | Discovers sessions, detects commits, and materializes. |
| store | `internal/store` | Wraps the SQLite schema and queries. |

Relationships:

| Source | Target | Intent | Technology |
|---|---|---|---|
| peasant web | api | subscribes to session detail | WebSocket |
| api | transcript | builds turns | Go call |
| api | store | reads sessions and commits | Go call |
| ingest | store | writes sessions | Go call |
| store | peasant session store | reads and writes | SQL, sqlite3 |
| ingest | agent session stores | reads session files and rows | JSONL, SQLite |

```c4
Component diagram: peasant backend

+-----------------------------+
| peasant web                 |
| [Container: Next.js 15]     |
+-----------------------------+
                |
                | subscribes to session detail (WebSocket)
                v
+== peasant backend [Container: Go] =============================================================+
|                                                                                                |
|  +-----------------------------+                                +-----------------------------+ |
|  | api                         |-- builds turns (Go call) ----->| transcript                  | |
|  | [Component: Go package]     |                                | [Component: Go package]     | |
|  | Serves the session detail   |                                | Turns stored entries into   | |
|  | WebSocket topic.            |                                | turns.                      | |
|  +-----------------------------+                                +-----------------------------+ |
|                |                                                                               |
|                | reads sessions and commits (Go call)                                          |
|                v                                                                               |
|  +-----------------------------+                                +-----------------------------+ |
|  | store                       |<-- writes sessions (Go call) --| ingest                      | |
|  | [Component: Go package]     |                                | [Component: Go package]     | |
|  | Wraps the SQLite schema and |                                | Discovers sessions, detects | |
|  | queries.                    |                                | commits, and materializes.  | |
|  +-----------------------------+                                +-----------------------------+ |
|                |                                                     |                         |
+================|=====================================================|=========================+
                 | reads and writes (SQL, sqlite3)                     | reads session files
                 v                                                     | and rows (JSONL, SQLite)
+-----------------------------+                                        v
| peasant session store       |                         +-----------------------------+
| [Container: SQLite]         |                         | agent session stores        |
+-----------------------------+                         | [Software System, external] |
                                                        +-----------------------------+

Key:
  Solid box = element. Double-line box = boundary. [Type] = C4 abstraction.
  "external" = outside the scope of this diagram. Arrow = one relationship, read as
  "source, label (technology), target".
```

## Dynamic: publish a session with /share

Elements come from the container diagram. The order of the interactions is the content.

| Step | Source | Target | Intent | Technology |
|---|---|---|---|---|
| 1 | developer | peasant web | opens /share | browser, HTTP |
| 2 | peasant web | peasant backend | requests the session detail | WebSocket |
| 3 | developer | peasant web | confirms the redaction review and consents | browser |
| 4 | peasant web | peasant backend | requests publication | HTTP |
| 5 | peasant backend | village | publishes the redacted session | HTTPS, schema wire |

```c4
Dynamic diagram: publish a session with /share

+-----------------------------+
| developer                   |
| [Person]                    |
+-----------------------------+
      |                     |
      | 1. opens /share     | 3. confirms the redaction
      | (browser, HTTP)     | review and consents (browser)
      v                     v
+-----------------------------+
| peasant web                 |
| [Container: Next.js 15]     |
+-----------------------------+
      |                     |
      | 2. requests the     | 4. requests publication
      | session detail      | (HTTP)
      | (WebSocket)         |
      v                     v
+-----------------------------+
| peasant backend             |
| [Container: Go]             |
+-----------------------------+
              |
              | 5. publishes the redacted session (HTTPS, schema wire)
              v
+-----------------------------+
| village                     |
| [Software System]           |
| Registry and commons for    |
| published transcripts.      |
+-----------------------------+

Key:
  Solid box = element. [Type] = C4 abstraction. Numbered arrow = one interaction, in order.
  A label may span several shaft lines. Read as "source, N. label (technology), target".
```

## Deployment: village, production

Deployment nodes from the Railway and Cloudflare R2 activation notes. The village frontend and
backend ship as Docker images on Railway services. Metadata lives in Railway PostgreSQL.
Transcript blobs live in a private Cloudflare R2 bucket, reached over the S3 API. The Caddy
reverse proxy in `docker-compose.yml` is development only and does not appear here.

| Node | Technology | Holds |
|---|---|---|
| Railway project | Railway | the three Railway nodes below |
| frontend service | Docker image, Node.js | village frontend |
| backend service | Docker image | village backend |
| Railway PostgreSQL | managed Postgres | village database |
| Cloudflare R2 | object storage | village blob store |

```c4
Deployment diagram: village, production

+== Railway project [Deployment Node: Railway] ==============================+
|                                                                            |
|  +== frontend service [Deployment Node: Docker image, Node.js] ========+   |
|  |                                                                     |   |
|  |  +-----------------------------+                                    |   |
|  |  | village frontend            |                                    |   |
|  |  | [Container: Next.js 16]     |                                    |   |
|  |  +-----------------------------+                                    |   |
|  |                 |                                                   |   |
|  +=================|===================================================+   |
|                    | calls the API (HTTPS, JSON)                           |
|                    v                                                       |
|  +== backend service [Deployment Node: Docker image] ==================+   |
|  |                                                                     |   |
|  |  +-----------------------------+                                    |   |
|  |  | village backend             |                                    |   |
|  |  | [Container: Go]             |-- stores blobs (S3 API, HTTPS) --+ |   |
|  |  +-----------------------------+                                  | |   |
|  |                 |                                                 | |   |
|  +=================|=================================================|=+   |
|                    | stores metadata (SQL, TLS)                      |     |
|                    v                                                 |     |
|  +== Railway PostgreSQL [Deployment Node: managed Postgres] ======+  |     |
|  |                                                                |  |     |
|  |  +-----------------------------+                               |  |     |
|  |  | village database            |                               |  |     |
|  |  | [Container: PostgreSQL]     |                               |  |     |
|  |  +-----------------------------+                               |  |     |
|  |                                                                |  |     |
|  +================================================================+  |     |
|                                                                      |     |
+======================================================================|=====+
                                                                       v
                          +== Cloudflare R2 [Deployment Node: object storage] ==+
                          |                                                     |
                          |  +-----------------------------+                    |
                          |  | village blob store          |                    |
                          |  | [Container: S3 bucket]      |                    |
                          |  +-----------------------------+                    |
                          |                                                     |
                          +=====================================================+

Key:
  Double-line box = deployment node, nested where one runs inside another. Solid box = one
  container instance. [Type] = C4 abstraction. Arrow = one relationship, read as
  "source, label (technology), target".
```
