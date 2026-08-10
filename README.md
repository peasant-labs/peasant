# Peasant

Peasant is a CLI tool that ingests AI coding assistant session data (Claude Code, OpenCode, Codex) and provides
analytics via a local web dashboard and terminal UI. It normalizes session transcripts from
multiple providers into a unified schema, stores metrics in a local SQLite database, and serves
them through a web API.

## Installation

Peasant ships a single statically linked binary (no runtime dependencies) for
**linux** and **macOS** on **amd64** and **arm64**. Every release attaches
`.tar.gz` archives, `.deb`/`.rpm` packages, and a `checksums.txt` to its
[GitHub Release](https://github.com/peasant-labs/peasant/releases).

| Platform | Quickest path | Guide |
|----------|---------------|-------|
| Ubuntu / Debian | `sudo apt install ./peasant_<ver>_linux_<arch>.deb` | [docs/install/ubuntu.md](docs/install/ubuntu.md) |
| Fedora / CentOS | `sudo dnf install ./peasant_<ver>_linux_<arch>.rpm` | [GitHub Releases](https://github.com/peasant-labs/peasant/releases) |
| openSUSE | `sudo zypper install --allow-unsigned-rpm ./peasant_<ver>_linux_<arch>.rpm` | [GitHub Releases](https://github.com/peasant-labs/peasant/releases) |
| Arch Linux | GitHub release tarball (`linux`) | [docs/install/arch.md](docs/install/arch.md) |
| macOS | GitHub release tarball (`darwin`) | [docs/install/macos.md](docs/install/macos.md) |
| Nix | `nix profile install github:peasant-labs/peasant#peasant` | [docs/install/nix.md](docs/install/nix.md) |
| WSL | install as the underlying distro, then read the caveats | [docs/install/wsl.md](docs/install/wsl.md) |

Or download a `peasant_<version>_<os>_<arch>.tar.gz` archive, verify it against
`checksums.txt`, and put the `peasant` binary on your `PATH`.

> **Note:** `v0.1.0` publishes GitHub release archives plus `.deb` and `.rpm`
> packages. AUR, Homebrew, nixpkgs, hosted apt, and macOS signing remain deferred
> until separately approved; see the [release runbook](docs/release-runbook.md).

## Quick start

```bash
# Enter the dev shell (provides Go toolchain, gopls, staticcheck, delve)
nix develop

# Build the binary
make build          # outputs bin/peasant

# Run the first-time setup wizard
peasant kickstart

# Ingest sessions from all configured providers
peasant ingest

# Start the web dashboard (background, auto-opens browser)
peasant web start

# Stop the web dashboard
peasant web stop
```

## First-run onboarding

`peasant kickstart` discovers recordings from the configured harnesses and groups
them by canonical project identity. The project-first wizard lets you choose projects,
narrow them by branch or individual session, and apply one global harness filter
without widening that scope. You can keep the selected work local or authenticate and
publish it to Village, privately by default, after reviewing Standard redaction and a
schema-owned Creative Commons content license.

Final confirmation runs configuration save, ingest, and any requested publication as
one ordered journey. Durable successes remain applied, failures identify their exact
retry targets, and stored Village receipts are authoritative for the resulting URLs and
visibility. Changing future tracking never deletes local recordings or unpublishes
Village copies. See [the kickstart guide](docs/KICKSTART.md) for the complete flow,
privacy boundaries, and recovery behavior.

## Commands

| Command | Description |
|---------|-------------|
| `peasant ingest` | Run the data ingestion pipeline |
| `peasant ingest verify` | Verify database schema integrity |
| `peasant village push` | Push ingested transcripts + annotations to the Peasant village (incremental — server-manifest skip-gate + retraction; see [flags](#peasant-village-push-flags)) |
| `peasant login` / `peasant logout` | Authenticate with / disconnect from the village |
| `peasant models sync` | Fetch and sync model reference data from models.dev |
| `peasant sessions context` | Print grep-C-style turns around a session turn (terminal-rendered) |
| `peasant sessions list` | List ingested sessions |
| `peasant sessions tag` | Manage session tags (add/remove/list) |
| `peasant metrics compute` | Compute session metrics from stored transcripts |
| `peasant web start` | Start the web dashboard server (default port 8690) |
| `peasant web stop` | Stop the web dashboard server |
| `peasant tui` | Launch the terminal UI |
| `peasant kickstart` | Run the first-time setup wizard |
| `peasant export sessions` | Export session transcripts as JSON |
| `peasant export annotations` | Export annotations as JSONL |
| `peasant export friction` | Export friction episode annotations |
| `peasant annotate sample` | Sample sessions for annotation |
| `peasant annotate create` | Create an annotation on a session/turn |
| `peasant annotate import` | Import annotations from JSONL files |
| `peasant memory` | Agent-memory commands — **experimental** (`-tags=experimental`); see [Experimental features](#experimental-features) |
| `peasant version` | Print the version |

See [docs/KICKSTART.md](docs/KICKSTART.md) for the kickstart wizard flow and [docs/TUI.md](docs/TUI.md) for keyboard shortcuts.

For Village authentication (`peasant village login`, push, and pull), see
[docs/auth.md](docs/auth.md).

### `peasant village push` flags

| Flag | Description |
|------|-------------|
| `--dry-run` | Show what would be pushed (mirrors the real run exactly) without pushing |
| `--visibility <v>` | Set visibility (`private` or `public`) |
| `--timing` | Report per-phase timing (handshake/server split, redaction, annotation batches) to stderr + a per-upload JSONL under the state dir. Off by default. |
| `--concurrency <n>` | Parallel uploads + HTTP connection-pool size (default `max(1, NumCPU/2)`; raise toward `~2×NumCPU` for a large cold push). |
| `--annotation-id <ids>` / `--annotation-hash <hashes>` | Restrict the annotation push to specific annotations |
| `--non-interactive` / `--yes` | Skip the interactive wizard + public-consent prompt |
| `--quiet` / `--verbose` | Output verbosity (redaction report stays on stderr; summary on stdout) |

**How it stays fast (incremental push).** The CLI fetches the village's
**annotation manifest** (`GET /api/v1/annotations/manifest`) and uploads only the
annotations the village lacks (fail-safe → push-all if it's unreachable);
already-pushed transcripts are skipped via `pushed_at`; locally-superseded
annotations are **retracted** server-side. So a no-change re-push sends ~nothing
(sub-second). A genuinely cold push is bounded by the per-transcript S3
round-trip — see `--concurrency`. The push also runs a **version-negotiation
preflight** (`GET /api/v1/schema/version`): the village accepts a window of CLI
contract versions and the CLI downgrade-emits with a one-line notice if it's
ahead.

**Association anchors are source facts, annotations are interpretations.** When
commit detection observes a session-to-commit relationship, ingest maintains its
durable association row; the V40 migration backfills legacy current-projection
rows. Normal ingest and migration backfill do **not** create association-target
annotations just because an association exists. Those semantic records require an
explicit eligible local producer, such as a human label, annotator, miner,
classifier, or intentional local import. Pulled foreign annotations and data are
one-way and never become local push or re-push candidates. The push path sends
association anchors with the transcript before any annotation batch that may
target those anchors; legacy already-pushed sessions whose anchors were backfilled
are replayed once through the ordinary push candidate path.

### `peasant models sync` flags

| Flag | Description |
|------|-------------|
| `--provider <p>` | Filter models by provider (`anthropic`, `google`, `openai`) |

### `peasant sessions tag` subcommands

| Subcommand | Description |
|------------|-------------|
| `peasant sessions tag add <session> <tag>` | Add a tag to a session |
| `peasant sessions tag remove <session> <tag>` | Remove a tag from a session |
| `peasant sessions tag list <session>` | List all tags for a session |

### `peasant ingest verify` flags

| Flag | Description |
|------|-------------|
| `--verbose` | Show sample data from each table |

### `peasant ingest` flags

| Flag | Description |
|------|-------------|
| `--dry-run` | Show what would be ingested without writing |
| `--force` | Re-ingest all sessions (respects staleness) |
| `--include-active` | Also ingest sessions still being written |
| `--session <ids>` | Filter to specific session IDs (repeatable, comma-separated). Overrides the selection index. |
| `--since <duration>` | Filter to sessions from the last N period (e.g. `2w`, `3m`, `7d`) |
| `--source-provider <p>` | Override source provider (`claude`, `opencode`) |
| `--source-path <path>` | Override source path for the provider (replaces config, not additive) |
| `--output <path>` | Override output base path |
| `--json` | Output as JSON instead of human-readable |
| `--verbose` | Show file-level detail with subagent expansion |
| `--reindex` | Re-process sessions with stale or missing index data |
| `--detect-commits` | Detect and store git commits linked to each session |
| `--config <path>` | Path to config file |

When a selection index is configured (via `peasant kickstart`), ingest automatically filters to only
the selected projects, branches, and sessions. The `--session` flag overrides this filter.

### `peasant web start` flags

| Flag | Description |
|------|-------------|
| `--port <n>` | Port to listen on (default 8690) |
| `--foreground` | Run in foreground instead of forking to background |
| `--no-browser` | Do not auto-open browser |
| `--dev` | Proxy to Next.js dev server on localhost:3000 (implies --foreground) |
| `--mock-data-store <sections>` | Use mock data for specific sections (replaces config, not additive) |

### `peasant tui` flags

| Flag | Description |
|------|-------------|
| `--mock-data-store <sections>` | Use mock data for specific sections (replaces config, not additive) |

## Supported providers

| Harness | Wire value | Session format |
|---------|------------|----------------|
| Claude Code | `claude-code` | JSONL |
| OpenCode | `opencode` | JSON |
| Codex | `codex` | JSONL (`rollout-*.jsonl`) |

Harness identity comes from [`peasant-labs/bestiary`](https://github.com/peasant-labs/bestiary)
(`Harness` is a type alias re-exported via `internal/defaults`). Further harnesses
(`gemini-cli`, `antigravity`) are known to the schema for forthcoming support.
(The legacy bare wire values `claude`/`gemini` were renamed to `claude-code`/`gemini-cli`
in the bestiary migration; temporary `LegacyHarness*` deprecation shims remain until launch.)

## Ingest pipeline

`peasant ingest` runs a 9-stage pipeline (`internal/ingest/pipeline.go`):

```
DISCOVER -> DIFF -> FILTER -> EXTRACT+WRITE -> DB INSERT -> INDEX -> COMPUTE -> CLEANUP -> REPORT
```

1. **DISCOVER** — For each enabled provider, create adapter via factory and call `Discover()` to
   enumerate sessions on disk.
2. **DIFF** — Classify each session as `New`, `Updated`, `Unchanged`, or `Active` by comparing
   source modification times and metadata schema version against previously written output.
3. **FILTER** — Skip `Unchanged` and `Active` sessions unless `--force` or `--include-active` are
   set. When a selection index is configured, also skip sessions not matching the project/branch
   allowlist (see [Selection Index](#selection-index)).
4. **EXTRACT+WRITE** — Extract metadata and token metrics; atomically write output via a temp
   directory followed by a rename.
5. **DB INSERT** — Upsert dimension rows (projects, host_slugs) and session rows into SQLite.
   Best-effort; failures are non-fatal.
6. **INDEX** — Parse the redacted transcript (on disk) into `session_entries` rows via
   provider-specific indexers (Claude JSONL, OpenCode JSON). Best-effort; failures are non-fatal.
7. **COMPUTE** — Run 16 metric functions over indexed entries to populate `session_metrics`.
   Only runs for sessions that were successfully indexed.
8. **CLEANUP** — Remove orphan `.tmp-*` directories left by interrupted prior runs.
9. **REPORT** — Return a `PipelineResult` with summary counts (new, updated, unchanged, active,
   errors) and per-session results.

## Transcript representation

Peasant uses a two-layer architecture for session transcripts:

**Storage layer (DB):** The `session_entries` table stores a normalized entry tree. Each content
block (text, tool_use, tool_result, thinking) is a separate row at depth=1, with the parent
message at depth=0. This granularity enables per-entry annotation targeting, fine-grained metrics,
and cross-provider queries. Each depth=1 entry carries a `part_type` column preserving the
provider's original label (e.g. `reasoning`, `step-start`, `patch` for OpenCode; `thinking`,
`text` for Claude).

**Display layer:** `EntriesToTurns` (`internal/api/store_adapter.go`) folds the entry tree into
display-ready turns: depth=1 tool children become `ToolCalls` on their parent, structural
duplicates are suppressed, and empty entries are filtered. `SessionToDetail`
(`internal/api/websocket.go`) wraps the turns with session metadata to produce
`SessionDetailPayload` (`github.com/peasant-labs/schema`, `local_api.go`).

**`SessionDetailPayload` is the canonical transcript format.** Both the web dashboard (via
WebSocket `session_detail` channel) and the CLI export (`peasant export sessions`) produce this
exact type. There is one conversion path:

```
DB (session_entries)
  → store.ListEntries()        # raw rows
  → api.EntriesToTurns()       # fold, suppress, filter → []ingest.Turn
  → api.SessionToDetail()      # wrap with metadata → *SessionDetailPayload
```

The export additionally overlays full (untruncated) content from the source transcript, since the
DB stores truncated `content_preview`. The exported JSON uses the same camelCase field names as
the WebSocket payload.

### Session-to-commit association architecture

`session_commits` is the mutable current projection of commits observed for a
session. `session_commit_associations` is the durable identity ledger for those
observations. Each ledger row owns one opaque association ID for one
`(session_id, observed_commit_hash)` pair. Re-ingest may replace the current
`session_commits` projection, but it must not erase the original association row
because map/review rewrite history and Village annotation validation need a
stable identity.

Commit detection creates ledger rows only for session-to-commit relationships it
observes. The V40 migration backfills ledger rows from legacy current-projection
data; a normal ingest with no detected commits creates no association row.

`annotation_target_associations` adds the local annotation target arm for those
durable IDs. It stores only the association ID; the ledger remains the authority
for the enclosing session and observed commit hash. This separation is
intentional: the association ledger records the source fact that a session
observed a commit, while an association-target annotation records an explicit
interpretation about that relationship. Normal ingest and backfill therefore
create ledger rows only. They do not auto-mint annotations such as "association
exists" or "confirmed by recorded commit" because that would duplicate source
state, create noisy push/pull data, and blur factual provenance with semantic
labels.

Pulled foreign annotations and data remain one-way and are never local push or
re-push candidates.

## Directory layout

Peasant follows the [XDG Base Directory Specification](https://specifications.freedesktop.org/basedir-spec/latest/).
All paths respect their corresponding `XDG_*` environment variable if set.

| XDG Variable | Default Path | Contents |
|---|---|---|
| `XDG_CONFIG_HOME` | `~/.config/peasant/` | `config.yaml` |
| `XDG_DATA_HOME` | `~/.local/share/peasant/` | `peasant.db` (analytics SQLite), `peasant-sync/` (ingested transcripts + metadata) |
| `XDG_STATE_HOME` | `~/.local/state/peasant/` | `web:{port}.pid` (server PID file) |

### Ingested output

```
~/.local/share/peasant/
├── peasant.db
└── peasant-sync/
    └── {hostSlug}/
        └── {sessionId}/
            ├── {sessionId}--transcript.{jsonl|json}
            └── {sessionId}--metadata.json
```

See [docs/pipeline.md](docs/pipeline.md) for the ingest write flow, staging
directory behavior, and the distinction between on-disk transcripts and the
canonical `SessionDetailPayload` representation.

## Analytics schema

The SQLite database uses a BCNF-normalized schema with migrations from **v1 to v41** (the
latest is `migrationV41` in `internal/store/migrations.go`; the next new migration is V42).
All tables use `STRICT` mode.

### Migration history (early milestones)

The DDL below shows the **v1 core**; later migrations add the `session_entries`/`session_entries_ext`
indexing tables, the `models` reference table, the annotation tables, the agent-memory tables
(`lessons`, `memory_injection_log`, `lesson_sources`), cost columns, pull/license/search data,
and the association ledger/target arm - see `internal/store/migrations.go` and the
per-migration test files for the authoritative list.

| Version | Feature |
|---------|---------|
| v1 | Initial 6-table schema (projects, host_slugs, sessions, session_metrics, daily_summary, daily_summary_harness) |
| v2 | SessionMetrics widened with v2 quality columns |
| v3 | IngestLog audit table |
| v4 | Push tracking (`pushed_at`, `push_log`) + Full-depth indexing (`depth`, `parent_index`, `tool_input`, `tool_output`) |
| v5 | SessionEntriesExt EAV table for known keys (`tokens_reasoning`, `cache_read`, `cache_write`, `model_id`) |
| v6 | Models reference table for model enrichment from models.dev |
| v7 | Cost analytics columns (`cost_*_usd`) on session_metrics and daily_summary |
| v8 | DailySummaryByProject table + acceptance_rate column |
| v9 | Tags column on sessions, scope column on session_metrics |
| v10–v28 | Annotation infrastructure, agent-memory (`lessons` + embeddings), type/origin dimension tables |
| v29-v33 | lessons FK/dedup refinements, `memory_injection_log`, `lesson_sources`, `model_harness` rename |
| v34-v38 | Village pull tables, transcript search index, custom labels, local and pulled license mirrors |
| v39-v41 | turn label seeds, durable session-to-commit association ledger, association annotation target arm |

### Table overview

| Table | Kind | Description |
|-------|------|-------------|
| `projects` | Dimension | One row per project; keyed by `project_hash` |
| `host_slugs` | Dimension | One row per host/remote; keyed by `host_slug` |
| `sessions` | Fact | One row per ingested session; references both dimension tables |
| `session_metrics` | Fact | Token counts and turn/tool stats for one session |
| `session_commits` | Fact/projection | Current commit observations for one session; replaced on re-ingest |
| `session_commit_associations` | Durable ledger | Stable IDs for session-to-observed-commit facts, used by map/review and association annotations |
| `annotation_target_associations` | Annotation target arm | Explicit semantic annotations about a durable session/commit association |
| `daily_summary` | Aggregate | Cross-provider daily rollup; recomputed on every ingest |
| `daily_summary_harness` | Aggregate | Per-provider daily rollup; composite PK `(date_utc, model_harness)` |

### DDL

```sql
CREATE TABLE projects (
    project_hash TEXT PRIMARY KEY,
    project_name TEXT NOT NULL,
    project_path TEXT NOT NULL
) STRICT;

CREATE TABLE host_slugs (
    host_slug  TEXT PRIMARY KEY,
    git_remote TEXT
) STRICT;

CREATE TABLE sessions (
    session_id     TEXT PRIMARY KEY,
    parent_id      TEXT REFERENCES sessions(session_id),
    model_harness  TEXT NOT NULL CHECK (model_harness IN ('claude','opencode','codex','gemini')),
    model_id       TEXT NOT NULL,
    host_slug      TEXT NOT NULL REFERENCES host_slugs(host_slug),
    project_hash   TEXT NOT NULL REFERENCES projects(project_hash),
    start_ms       INTEGER NOT NULL,
    end_ms         INTEGER NOT NULL,
    ingested_ms    INTEGER NOT NULL,
    source_path    TEXT NOT NULL,
    source_format  TEXT NOT NULL CHECK (source_format IN ('jsonl','json')),
    schema_version INTEGER NOT NULL DEFAULT 1,
    git_branch     TEXT,
    git_worktree   TEXT,
    git_tracking   TEXT,
    tool_version   TEXT
) STRICT;

-- Indexes on sessions for common query patterns
CREATE INDEX idx_sessions_start    ON sessions(start_ms);
CREATE INDEX idx_sessions_harness  ON sessions(model_harness);
CREATE INDEX idx_sessions_project  ON sessions(project_hash);
CREATE INDEX idx_sessions_host     ON sessions(host_slug);
CREATE INDEX idx_sessions_parent   ON sessions(parent_id) WHERE parent_id IS NOT NULL;

CREATE TABLE session_metrics (
    session_id      TEXT PRIMARY KEY REFERENCES sessions(session_id),
    turn_count      INTEGER NOT NULL DEFAULT 0,
    tool_call_count INTEGER NOT NULL DEFAULT 0,
    subagent_count  INTEGER NOT NULL DEFAULT 0,
    duration_ms     INTEGER NOT NULL DEFAULT 0,
    tokens_in       INTEGER NOT NULL DEFAULT 0,
    tokens_out      INTEGER NOT NULL DEFAULT 0,
    tokens_total    INTEGER NOT NULL GENERATED ALWAYS AS (tokens_in + tokens_out) STORED
) STRICT;

CREATE TABLE daily_summary (
    date_utc        TEXT PRIMARY KEY,
    session_count   INTEGER NOT NULL DEFAULT 0,
    tokens_in       INTEGER NOT NULL DEFAULT 0,
    tokens_out      INTEGER NOT NULL DEFAULT 0,
    tokens_total    INTEGER NOT NULL DEFAULT 0,
    avg_duration_ms REAL    NOT NULL DEFAULT 0,
    avg_turns       REAL    NOT NULL DEFAULT 0,
    tool_call_count INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE TABLE daily_summary_harness (
    date_utc        TEXT NOT NULL,
    model_harness   TEXT NOT NULL CHECK (model_harness IN ('claude','opencode','codex','gemini')),
    session_count   INTEGER NOT NULL DEFAULT 0,
    tokens_in       INTEGER NOT NULL DEFAULT 0,
    tokens_out      INTEGER NOT NULL DEFAULT 0,
    tokens_total    INTEGER NOT NULL DEFAULT 0,
    avg_duration_ms REAL    NOT NULL DEFAULT 0,
    avg_turns       REAL    NOT NULL DEFAULT 0,
    tool_call_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (date_utc, model_harness)
) STRICT, WITHOUT ROWID;
```

### Key design notes

- `tokens_total` in `session_metrics` is a `GENERATED ALWAYS AS (tokens_in + tokens_out) STORED`
  column — it is never written directly.
- `daily_summary` and `daily_summary_harness` are recomputed inside the `InsertSessions`
  transaction on every ingest; callers do not need to trigger summary updates separately.
- `parent_id` on `sessions` is a self-referencing FK supporting subagent session trees.
- The `host_slug` and `project_hash` FK relationships enforce referential integrity; the
  dimension rows are upserted before the fact rows within the same transaction.

## Configuration

Peasant reads its configuration from:

```
$XDG_CONFIG_HOME/peasant/config.yaml   # if XDG_CONFIG_HOME is set
~/.config/peasant/config.yaml          # otherwise
```

If no config file exists, Peasant uses built-in defaults and prints a notice directing you to
`peasant kickstart`. CLI flags (`--source-provider`, `--source-path`, `--output`) override the
config file for a single run.

### Selection index

The kickstart wizard saves the selection index in `config.yaml`. This example shows its persisted
shape.

<!-- verified-example: selection-config -->
```yaml
selection:
  mode: selected
  autoIngestNewBranches: true
  harnesses:
    claude-code:
      projects:
        - gitRemote: https://github.com/example/atlas.git
          clonePaths:
            - /projects/atlas
            - /projects/atlas-review
          branches:
            - main
            - release
      sessions:
        - 11111111-1111-4111-8111-111111111111
```

Kickstart writes the real `clonePaths` values. The paths in this example are sample paths.

| Field or value | Purpose |
|----------------|---------|
| `mode: all` | Do not apply a selection filter. |
| `mode: selected` | Apply the rules under `harnesses`. |
| `autoIngestNewBranches` | Store the choice for newly found branches. |
| `harnesses` | Group project and explicit session rules by recording harness. |
| `projects[].gitRemote` / `name` | Store the project label fields. |
| `projects[].clonePaths` | Store the resolved physical clone paths for one project entry. |
| `projects[].branches` | Store an optional branch list. An empty list means all branches for the entry. |
| `sessions` | Store explicit session IDs. |

Use the kickstart guide for each user job:

| User job | Guide |
|----------|-------|
| Run kickstart again or change saved choices | [Restore saved choices](docs/KICKSTART.md#restore-saved-choices) |
| Understand `clonePaths` and local clone identity | [Physical clone identity](docs/KICKSTART.md#physical-clone-identity) |
| Open a config that uses `mode: all` | [Convert an old all-projects setting](docs/KICKSTART.md#convert-an-old-all-projects-setting) |
| Save when no selected work is available | [Save with no effective project](docs/KICKSTART.md#save-with-no-effective-project) |
| Understand project lists, push choices, direct links, and manual deletion | [Viewer lists and stored data](docs/KICKSTART.md#viewer-lists-and-stored-data) |

## Web dashboard

The web dashboard (`peasant web start`) serves a Next.js frontend that connects to the backend via
WebSocket. The server pushes data on 6 channels, each broadcast every 5 seconds:

| Channel | Message type | Payload | Page |
|---------|-------------|---------|------|
| `dashboard` | `dashboard` | KPIs: total sessions/tokens, avg duration/turns, acceptance rate | `/` |
| `sessions` | `sessions` | Session list with summary stats | `/sessions` |
| `session_detail` | `session_detail` | Full session with turns/tool calls | `/sessions/detail?id=...` |
| `trends` | `trends` | Daily token/session counts | `/trends` |
| `quality` | `quality` | Per-session quality metrics (outcome, signal density, retry loops, effectiveAnnotations) | `/` (charts) |
| `annotations` | `annotations` | Annotation updates (stub — not yet implemented) | — |

### WebSocket protocol

Connect to `/api/v1/ws` and send JSON messages to subscribe. Subscriptions use a
`ChannelSubscription` struct with `topic`, and optionally `axis` and `id` fields:

```json
{"type": "subscribe", "channels": [{"topic": "dashboard"}, {"topic": "quality"}]}
{"type": "subscribe", "channels": [{"topic": "session_detail", "id": "SESSION_ID"}]}
{"type": "subscribe", "channels": [{"topic": "annotations", "axis": "session", "id": "SESSION_ID"}]}
{"type": "unsubscribe", "channels": [{"topic": "dashboard"}]}
```

The server responds with `{"type": "connected", "version": "..."}` on connect, then pushes
snapshots immediately on subscribe and every `ServerBroadcastTick` (5s) thereafter.

Topic-specific fields:
- `session_detail` requires `id` (session ID)
- `annotations` requires `axis` (`type`, `session`, or `project`) and `id`
- All other topics have no additional fields

### REST endpoints

| Route | Method | Description |
|-------|--------|-------------|
| `/api/v1/health` | GET | Health check (`{"status": "ok"}`) |
| `/api/v1/ws` | GET | WebSocket upgrade |
| `/api/v1/config/mock` | GET | Current mock configuration |
| `/api/v1/sessions` | GET | Session list (REST alternative to WS) |
| `/api/v1/shutdown` | POST | Graceful server shutdown |

### Running in dev mode

```bash
# Backend serves static Next.js build (production mode)
peasant web start

# Backend proxies to Next.js dev server (hot reload)
peasant web start --dev              # implies --foreground

# In another terminal:
cd web && pnpm dev                # Next.js dev server on :3000
```

## Progressive mock system

Peasant supports granular control over mock vs real data for development and testing. By default mock
data is **disabled** (`DefaultMockEnabled = false` in `internal/defaults/mock.go`). Enable it via
config or CLI flags to overlay mock data on specific sections while the rest uses real data.

### Configuration

```yaml
# config.yaml
mock:
  enabled: true                        # Enable mock data globally
  web: [dashboard, sessions, trends]   # Use mock for these web sections
  tui: [sessions]                      # Use mock for these TUI sections
  api: [sessions]                      # Use mock for these API sections
```

### Available sections

| Component | Sections |
|-----------|----------|
| `web` | `dashboard`, `sessions`, `trends`, `metrics`, `qualitySessions` |
| `tui` | `sessions` |
| `api` | `sessions` |

### CLI override

CLI flags **replace** configured sections (not additive), following the `--source-path` convention:

```bash
# Use mock data for web dashboard only
peasant web start --mock-data-store=web,dashboard

# Use mock data for all web sections
peasant web start --mock-data-store=web

# Disable all mocks (even if config enables them)
peasant web start --mock-data-store=none

# Replace TUI sections
peasant tui --mock-data-store=tui
```

### Architecture

- **ProgressiveProvider** (`internal/api/progressive.go`): Decorator that routes requests to mock
  or real provider based on configuration
- **MockProvider** (`internal/mock/provider.go`): Mock data implementation registered via
  `MockProviderFactory` init() pattern to avoid import cycles
- **Typed enums** (`internal/defaults/mock.go`): `MockComponents.Web`, `MockSections.Dashboard`, etc.

### Agentic testing with the progressive mock system

The progressive mock system enables automated testing loops where an agent can start a server,
send requests, and verify responses programmatically. The key enabler is `--foreground`, which
runs the server in the calling process (no background fork), making it controllable from scripts
and agent sessions.

#### Pattern 1: curl/wget verification

Start the server in one process and probe it from another:

```bash
# Terminal 1: start server in foreground on a test port
./bin/peasant web start --port 9999 --foreground --no-browser

# Terminal 2: verify endpoints
# Health check
curl -s http://localhost:9999/api/v1/health | jq .

# Check mock config
curl -s http://localhost:9999/api/v1/config/mock | jq .

# REST sessions endpoint
curl -s http://localhost:9999/api/v1/sessions | jq '.sessions | length'

# WebSocket subscribe (one-shot with websocat)
echo '{"type":"subscribe","channels":["dashboard"]}' \
  | websocat ws://localhost:9999/api/v1/ws \
  | head -2 | jq .
```

Compare mock vs real data by running two servers:

```bash
# Server A: real data
./bin/peasant web start --port 9990 --foreground --no-browser &

# Server B: mock data for quality
./bin/peasant web start --port 9991 --foreground --no-browser --mock-data-store=web,qualitySessions &

# Compare session counts
REAL=$(curl -s http://localhost:9990/api/v1/sessions | jq '.sessions | length')
MOCK=$(curl -s http://localhost:9991/api/v1/sessions | jq '.sessions | length')
echo "Real: $REAL, Mock: $MOCK"

./bin/peasant web stop --port 9990
./bin/peasant web stop --port 9991
```

#### Pattern 2: Playwright/Puppeteer browser automation

See `TESTING.md` for planned Playwright patterns (shell invocation, example test cases, required
frontend changes). These are **not yet verified** — no `web/e2e/` directory or `data-testid`
attributes exist yet.

#### Pattern 3: Go integration tests

Existing Go tests demonstrate the patterns for testing the progressive provider and store adapter:

- **Mock/real routing:** `internal/api/progressive_test.go` —
  `TestProgressiveProvider_MockEnabled_RoutesToMock`,
  `TestProgressiveProvider_MockDisabled_RoutesToReal`,
  `TestProgressiveProvider_QualitySessions_RoutesToMock`
- **Quality metrics round-trip:** `internal/api/store_adapter_test.go:595` —
  `TestStoreDataProvider_QualitySessions_WithQualityMetrics`
- **Session detail with entries/turns:** `internal/api/store_adapter_test.go:692` —
  `TestStoreDataProvider_SessionByID_WithEntries`
- **Empty session handling:** `internal/api/store_adapter_test.go:812` —
  `TestStoreDataProvider_SessionByID_NoEntries`

These tests use `httptest.NewServer` with `NewHub(provider)` to stand up a real HTTP server
backed by an in-memory SQLite store. The WebSocket E2E pattern in `AGENTS.md` extends this with
`github.com/coder/websocket` for channel subscription testing.

## Experimental features

Some features are shelved in the default build and gated behind a flag: the
`peasant memory` command group (Go build tag `-tags=experimental`) and the
`/review` page's real-data mode (env `NEXT_PUBLIC_EXPERIMENTAL_REVIEW=1`). See
[EXPERIMENTAL.md](EXPERIMENTAL.md) for how to enable them, and
[docs/memory.md](docs/memory.md) for the agent-memory reference.

## Development

```bash
# Enter dev shell
nix develop

# Build
make build

# Run all quality gates (fmt + vet + ast-grep + tests) — required before merging
make check

# Run tests with the mandatory race detector
go test -race ./...

# Run a single package
go test -race ./internal/ingest/ -v

# Run a single test
go test -race ./internal/ingest/ -run TestPipeline_DryRun -v

# Static analysis (ast-grep rules)
ast-grep scan --config sgconfig.yml .

# Full-stack end-to-end harness — podman: Postgres + MinIO + real village + real peasant CLI.
# Build-tagged `e2e` (OUT of `make check`); needs podman + a village checkout.
make e2e            # asserted: ingest fixture → push (skip-gate + retraction) → village secret-scan
make demo           # same harness, verbose + unasserted ("watch it happen")
```

See [docs/e2e.md](docs/e2e.md) for the harness walkthrough,
[docs/e2e-fixture.md](docs/e2e-fixture.md) for committed-fixture provenance, and
[`TESTING.md`](TESTING.md) for the full E2E testing map (WebSocket E2E, fixture
meta-tests in `make check`, and this podman harness). The always-on fixture
meta-tests DO run inside `make check`.

### Web build & lockfiles

`web/package.json` is the source of truth for web dependencies, and
`web/pnpm-lock.yaml` is the only lockfile. `make web` requires the pnpm version
pinned by `packageManager`; it fails if pnpm is unavailable and never resolves a
second dependency tree with npm.

After any dependency change, regenerate and commit the pnpm lockfile:

```bash
cd web
pnpm install
git add package.json pnpm-lock.yaml
```

Use `pnpm install --frozen-lockfile` to reproduce the committed dependency graph
without modifying it. CI and artifact-producing workflows use the same pnpm-only
path.

### Package map

| Package | Description |
|---------|-------------|
| `cmd/peasant` | CLI entry point; Cobra command wiring |
| `internal/ingest` | 9-stage pipeline, source adapters (Claude, OpenCode), diff logic |
| `internal/store` | SQLite data access layer; schema migrations; read/write queries |
| `internal/api` | HTTP server, WebSocket hub, and `ProgressiveProvider` for mock/real data routing |
| `internal/mock` | Mock data provider implementation for development/testing |
| `internal/config` | Config loading, validation, and defaults |
| `internal/defaults` | Single source of truth for all cross-package constants |
| `internal/tui` | Bubbletea terminal UI |
| `github.com/peasant-labs/redact` | External public module owning redaction levels, detection, rewriting, and validation |
| `internal/testutil` | Shared test fixtures: `MemFS`, `StubGitResolver`, `StubAdapter` |
| `github.com/peasant-labs/schema` (external module) | Unified schema/wire types for peasant push + pull, consumed via `go.mod` pin. The Go SOT + generated OpenAPI specs live in that module's repo, not in peasant. |

### Working with the OpenAPI specs (they live in the schema module)

The OpenAPI specs are the **single source of truth** owned by the
`github.com/peasant-labs/schema` module, generated from its Go source and held
byte-fresh + immutable by that module's own gates. Peasant **consumes** the
published module by pinning it in `go.mod`; it does not regenerate or vendor the
specs. To change the contract, land it in the schema repo (regenerate there, and
version-bump for any released-surface change) and then re-pin peasant's `go.mod`.

See [`docs/contract/versioning-procedure.md`](docs/contract/versioning-procedure.md).

### Schema versioning

The PublishRequest schema version is defined in `internal/defaults/schema.go` as `PublishSchemaVersion`. Bump this constant for a wire-format change; the versioned OpenAPI specs are regenerated in the `github.com/peasant-labs/schema` module (peasant has no schema-gen command), and peasant re-pins the module afterwards.

## Schema versions explained

Peasant uses three distinct schema versions for different data stores and APIs:

| Schema | Location | Current | Purpose |
|--------|----------|---------|---------|
| **Database** | `internal/store/migrations.go` | v41 | SQLite `peasant.db` tables and columns |
| **Metadata** | `internal/ingest/metadata.go` | v3 | `{sessionId}--metadata.json` files |
| **Publish (content contract)** | `internal/defaults/schema.go` | `"0.1.1"` (`PushContractVersion`) | `peasant village push` wire format; negotiated against the village's `[Min, Current]` window |

> Distinct from the **`github.com/peasant-labs/schema` module version** (`v0.1.0`, published), which is how
> peasant and village pin the shared types — bumped in lockstep at each release (tag the schema module → regen OpenAPI/TS → deploy village → re-pin).

### When to bump each schema

| Schema | Bump when | Example change |
|--------|-----------|----------------|
| **Database (vN)** | Adding tables, columns, or constraints to SQLite | New `tags` table → v9 |
| **Metadata (vN)** | Changing the JSON structure written to `--metadata.json` | New field in `UnifiedMetadata` |
| **Publish ("M.m")** | Changing the JSON structure sent to village API | Breaking change to `PublishRequest` |

### Database migration workflow

New database features require a migration in `internal/store/migrations.go`:

```go
// internal/store/migrations.go
var migrations = []Migration{
    {Version: 1, DDL: "CREATE TABLE ...", Description: "initial schema"},
    // ...
    {Version: 10, DDL: "CREATE TABLE new_feature...", Description: "new feature"},
}
```

Run verification after migrations:

```bash
./bin/peasant ingest verify
```

### Database verification

SQL scripts for querying and verifying the database are in `scripts/query-db.sql`:

```bash
# Run a specific query
sqlite3 ~/.local/share/peasant/peasant.db < scripts/query-db.sql

# Or run individual queries
sqlite3 ~/.local/share/peasant/peasant.db "SELECT COUNT(*) FROM sessions;"
sqlite3 ~/.local/share/peasant/peasant.db "PRAGMA table_info(sessions);"
```

The script includes queries for:
- Database overview (table list, row counts)
- Sessions (recent, by provider, pushed, tagged)
- Session entries (by type, depth, tool usage)
- Session metrics (costs, outcomes)
- Daily summaries (by date, provider, project)
- Models (from models.dev)
- Session entries ext (EAV attributes)
- Ingest/push logs
- Debug checks (orphaned rows, duplicates)

## License

Peasant is licensed under the **Apache License, Version 2.0** (`Apache-2.0`). The full
text is in [`LICENSE`](LICENSE).

This covers the Peasant software itself. It is separate from the **transcript
licenses** (`CC0-1.0` / `CC-BY-4.0` / `CC-BY-SA-4.0`) that you attach to session data
you publish with `peasant village push --license`; those govern the content you share,
not this tool.
