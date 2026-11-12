# Antigravity

Antigravity ships as two products. Each keeps its own store under `~/.gemini`, and each has a
capture-only layout in `internal/harnesslayout`:

| Product | Tool | Harness | Root |
| --- | --- | --- | --- |
| the command-line agent, `agy` | `antigravity-cli` | `antigravity-cli` | `{home}/.gemini/antigravity-cli` |
| the desktop application | `antigravity-ide` | `antigravity-ide` | `{home}/.gemini/antigravity` |

Peasant does not ingest either product. A capture records roots, shape, and metadata only.
`peasant harvest --harness antigravity-cli` and `--harness antigravity-ide` are refused as known
harnesses without a harvester. The evidence for the split is in
[polyrepo#77](https://github.com/peasant-labs/polyrepo/issues/77).

## Observed on

Linux x86_64, `antigravity-hub` 2.29.0 (binary `antigravity`) and `antigravity-cli` 1.5.3
(binary `agy`), both packaged by `numtide/llm-agents.nix`, two sessions per product. The roots
are home-relative and are declared for every operating system. macOS and Windows are
unverified. Antigravity does not document these formats, so expect drift between releases.

## One core, two products

Both products write the same per-session tree. The two layouts share one probe core,
`internal/harnesslayout/antigravity_core.go`; `antigravity_cli.go` and `antigravity_ide.go`
each declare what only that product writes.

```
<root>/
├── conversations/<id>.db                  conversation body, SQLite (also -wal, -shm)
├── brain/<id>/
│   ├── .system_generated/
│   │   ├── logs/transcript.jsonl          one record per step
│   │   ├── logs/transcript_full.jsonl
│   │   ├── logs/chunks/<log>/<n>.jsonl    the logs, cut into chunks
│   │   ├── steps/<n>/output.txt           output of one step
│   │   └── messages/                      desktop application only
│   ├── .user_uploaded/                    not captured
│   └── scratch/                           not captured
├── annotations/<id>.pbtxt                 protobuf text with the title
├── presence/<id>.lock
├── conversation_summaries.db              conversation index, SQLite
└── crashes/crash_<n>.log
```

Entries at the root that only one product writes:

| Product | Root entries | Per-session extras |
| --- | --- | --- |
| `antigravity-cli` | `history.jsonl`, `implicit/*.pb`, `knowledge/`, `log/`, `persistent_terminals/`, `settings.json`, `jetski_state.pbtxt`, `jetbox_summaries_proto.pb` | none |
| `antigravity-ide` | `daemon/hub_*.json`, `antigravity_state.pbtxt`, `agyhub_summaries_proto.pb` | `brain/<id>/.system_generated/messages/` |

`daemon/hub_<pid>.json` exists only while a hub process runs.

## Sessions

A session of a tool is a conversation whose body, `conversations/<id>.db`, or whose
`brain/<id>/` directory is under that tool's root. The conversation index is not a source of
sessions: each product's `conversation_summaries.db` lists the conversations of **both**
products (the desktop application tags the agent's rows with
`source = CORTEX_TRAJECTORY_SOURCE_CLI`). An index row with no body under the root is the
other product's conversation. So each conversation is captured once, by the tool whose root
holds its body, with that tool's harness. A missing root, an empty root, and a root with an
index and no bodies each give zero sessions and no error. `-wal` and `-shm` files are not
sessions.

## What a capture records

| Artifact | Format | What the capture records |
| --- | --- | --- |
| `conversation-body` | SQLite | table and column names as field paths `$.<table>.<column>` with the JSON type of the column's affinity; kinds `rows:<table>` (row count per table), `step_type:<n>` and `status:<n>` (census of `steps`) |
| `transcript-log`, `transcript-full-log`, `transcript-chunks` | JSONL | field paths and JSON types; record kinds `<type>/<source>` |
| `step-output`, `presence-lock`, `crash-logs`, `*.pb`, `knowledge`, `cli-logs`, `persistent-terminals`, `undelivered-messages` | files | file count and total size in `bytes`; the files are never opened |
| `annotation`, `cli-state`, `ide-state` | protobuf text | field paths and scalar types; never a value |
| `prompt-history`, `cli-settings`, `daemon-hub`, `agent-messages` | JSON, JSONL | field paths and JSON types |
| `conversation-index` | SQLite | this session's row only: `step_count` (kind `step_count`), `last_modified_time`, `workspace_uris`, `source` (kind `source:<value>`), and the type of `project_id` |

Object keys that are data, such as the message identifiers that key `read.json`, are recorded
as `*`. Metadata: `UserTurns` counts `USER_INPUT` records of `transcript.jsonl`,
`AssistantMsgs` counts `PLANNER_RESPONSE` records, `ToolCalls` counts `tool_calls` entries, and
the time range comes from `created_at` and the index's `last_modified_time`. `ProjectPath` is
the first file URI of `workspace_uris`; the default report removes it, as it removes every
path. `Title` is never set: an Antigravity title is derived from conversation text.

The chunk files are cut by size, so a record that straddles a cut does not decode and is
counted in `malformed`.

## The SQLite read guard

The conversation bodies and the index hold conversation text in named columns and in the
full-text tables. `antigravityOpen` is the only place the Antigravity code opens SQLite. It
opens read-only with a busy timeout and installs a SQLite authorizer before it returns the
connection:

- A read of an entry on the deny list makes SQLite reject the whole statement
  (`SQLITE_DENY`). The guard never substitutes `NULL` (`SQLITE_IGNORE`), which would hide the
  read. The list is `antigravityDeniedReads` in `antigravity_core.go`; the fixture
  [`antigravity_denied_reads.yaml`](../../internal/harnesslayout/testdata/antigravity_denied_reads.yaml)
  holds the same set, one refused statement per entry, and the test requires the two to be
  equal.
- On a conversation body, every column whose declared type is BLOB is refused as well, so a
  blob column that a later product version adds is refused without a code change. This rule
  applies to bodies only.
- No query uses `SELECT *`; one would touch a refused column and fail.

A refused statement fails with an error that names the table and the column, says the column
can hold conversation text, and says the fix: remove the column from the query, not from the
guard. The capture of that session then fails and is listed under `failures`. A read of the
full-text table is refused at the first read its module makes while the statement is prepared,
which is `conversations_fts_config`.

## Outside both layouts

- `~/.gemini/antigravity-cli/antigravity-oauth-token` and `installation_id` in both roots are
  credentials. No pattern matches them and the probes never open them.
- `~/.gemini/config/` (`config.json`, `projects/`, `plugins/`, `sidecars/`) is shared by both
  products and holds no session.
- `~/.config/Antigravity/` holds the desktop application's Electron caches. The Cline
  extension inside the desktop application keeps its tasks under
  `~/.config/Antigravity/User/globalStorage/saoudrizwan.claude-dev`, which is a root of the
  `cline` tool only.
- The agent's own installation directories in its root (`bin/`, `builtin/`, `cache/`) and its
  `cli.log` link are not captured.
