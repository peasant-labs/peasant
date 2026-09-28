# Cursor

Cursor keeps session data in three places. Peasant ingests one of them. The other two have
capture-only layouts in `internal/harnesslayout`.

| Surface | Written by | Peasant coverage |
| --- | --- | --- |
| `~/.cursor/projects/<slug>/agent-transcripts/` JSONL | the IDE and the CLI | ingested by the `cursor` harness |
| `~/.cursor/chats/<md5>/<chatId>/store.db` and `acp-sessions/` | the CLI (`cursor-agent`) | layout `cursor-cli` |
| `<config>/Cursor/User/globalStorage/state.vscdb` and `workspaceStorage/` | the IDE | layout `cursor-ide-state` |

Cursor does not document any of these formats. Everything below is reverse-engineered by the
cited tools and, for the CLI, read from the shipped `cursor-agent` 2026.09.26 bundle. Expect
drift between releases.

## Ingested: agent transcripts

`internal/ingest/cursor.go` discovers
`{workspace}/agent-transcripts/{id}/{id}.jsonl` and
`{workspace}/agent-transcripts/{parent}/subagents/{id}.jsonl` below `~/.cursor/projects`. It
reads roles, content blocks (`text`, `thinking`, `tool_use`, `tool_result`), `message.model`,
token usage, and timestamps when present, and derives the project from the workspace slug.

The CLI writes these transcripts too: its `cli-transcript-writer` writes
`<data dir>/projects/<slug>/agent-transcripts/<id>/<id>.jsonl`, where the data dir is
`$CURSOR_DATA_DIR` or `~/.cursor` and the slug is the workspace path with every
non-alphanumeric run replaced by `-`. So CLI chats are already ingested when the transcript
exists.

What the transcripts lack: a user-given title, a reliable model (the adapter warns
`missing_model` when `message.model` is absent), the composer mode, and often timestamps. The
two stores below carry those.

## Layout `cursor-cli`

Roots: `{home}/.cursor` on every OS, and `{config}/cursor` on Linux. The CLI uses
`$CURSOR_CONFIG_DIR` if set, else `$XDG_CONFIG_HOME/cursor`, else `~/.cursor`. The `{config}`
root only matches when `XDG_CONFIG_HOME` is set; a custom `CURSOR_CONFIG_DIR` needs `--path`.

```
~/.cursor/
├── chats/
│   └── <md5 hex of the resolved cwd>/
│       └── <chatId (UUID)>/
│           ├── store.db              SQLite, WAL
│           │   ├── meta(key TEXT PRIMARY KEY, value TEXT)
│           │   │     "0" → hex(UTF-8 JSON agent record)
│           │   └── blobs(id TEXT PRIMARY KEY, data BLOB)
│           │         id = SHA-256 of data
│           │         JSON blobs: messages {role, content, createdAt?, providerOptions?}
│           │         other blobs: protobuf turn-graph nodes
│           ├── meta.json             chat sidecar
│           └── prompt_history.json   recent user prompts
└── acp-sessions/
    └── <sessionId>/
        ├── store.db                  same format
        └── meta.json                 {schemaVersion, cwd, title?}
```

The agent record is `JSON.stringify` of `{agentId, latestRootBlobId (hex), name, mode,
isRunEverything, approvalMode?, createdAt (ms), lastUsedModel?, lastDebugServerPort?,
currentPlanUri?, subagentInfo?, blobEncryptionKey}`, UTF-8 encoded, then hex-encoded. Older
releases were observed writing plain JSON; the probe accepts both. `name` defaults to
`New Agent`. The CLI deletes a store that has no root blob and no blobs, so `create-chat`
alone leaves nothing on disk.

`meta.json` holds `{schemaVersion: 1, createdAtMs, hasConversation, isSubagent?, title?,
updatedAtMs?, cwd?}`.

The probe opens `store.db` read-only and reads at most 20000 rows per table and no value over
16 MiB. It records:

- the columns of `meta` and `blobs` as `$.meta.*` and `$.blobs.*`;
- each meta row, decoded, under `$.meta.value`, with kind `meta`;
- each JSON blob under `$.blobs.data`, with its `role` as the kind (`user`, `assistant`,
  `tool`, `system`, or `no-role`); every other blob as kind `binary`, not decoded;
- `meta.json` and `prompt_history.json` as JSON shapes (one record per prompt).

Metadata sources:

| Field | Source |
| --- | --- |
| SessionID | agent record `agentId`, else the chat directory name |
| Title | `meta.json` `title`, else agent record `name` unless it is `New Agent` |
| ProjectPath | `meta.json` `cwd`, else agent record `workspacePath` (older releases) |
| Time range | agent record `createdAt`, message `createdAt`, `meta.json` `createdAtMs`/`updatedAtMs` |
| Models | agent record `lastUsedModel`, message `providerOptions.cursor.modelName` |
| User turns | `user` messages, except the injected `<user_info>` environment preamble |
| Assistant messages / tool calls | `assistant` messages / their `tool-call` parts |

The agent record carries `blobEncryptionKey`, which the CLI sends to the server as the
`x-blob-encryption-key` header. The probe never extracts it; the shape records only that the
field exists and is a string.

## Layout `cursor-ide-state`

Root: `{config}/Cursor/User` on every OS: `~/.config/Cursor/User` (or under
`$XDG_CONFIG_HOME`) on Linux, `~/Library/Application Support/Cursor/User` on macOS,
`%APPDATA%\Cursor\User` on Windows.

```
Cursor/User/
├── globalStorage/
│   └── state.vscdb                        SQLite
│       ├── cursorDiskKV(key TEXT UNIQUE, value BLOB)
│       │     composerData:<composerId>             session document (JSON)
│       │     bubbleId:<composerId>:<bubbleId>      one message (JSON)
│       │     checkpointId:<composerId>:…           file checkpoints
│       │     messageRequestContext:<composerId>:…  request context
│       │     codeBlockDiff:<composerId>:…          code-block diffs
│       │     composerVirtualRowHeights:<composerId> renderer cache
│       │     agentKv:blob:<sha>                    shared request cache (not read)
│       ├── ItemTable(key TEXT UNIQUE, value BLOB)  app state, cursorAuth/* (not read)
│       └── composerHeaders(composerId, workspaceId, createdAt, lastUpdatedAt, …, value)
│             newer builds: one header row per composer
└── workspaceStorage/
    └── <32-hex workspace id>/
        ├── state.vscdb                    ItemTable composer.composerData
        │                                  (newer: composer.composerHeaders)
        │                                  → {allComposers: [{composerId, name, …}]}
        └── workspace.json                 {"folder": "file:///…"} or {"workspace": "…"}
```

A `composerData` document carries `_v`, `composerId`, `name`, `createdAt`, `lastUpdatedAt`
(ms), `unifiedMode`, `modelConfig.modelName`, and either an inline `conversation` array
(older `_v`) or `fullConversationHeadersOnly: [{bubbleId, type}]`. A bubble carries `_v`,
`type` (1 user, 2 assistant), `text`, `richText`, `createdAt` (RFC 3339), `modelInfo.modelName`,
`tokenCount`, `thinking`, `capabilityType`, and for a tool call `toolFormerData` (`tool`,
`name`, `toolCallId`, `params`, `status`, `result`).

Discovery reads only the keys of the `composerData:` range, never values, so every composer
is a session, including drafts. It maps composers to workspaces through each workspace
database's `composer.composerData` or `composer.composerHeaders` entry. For each composer the
probe reads its `composerData` row, at most 20000 `bubbleId:<composerId>:` rows by an index
range scan, the counts of the auxiliary families, and its `composerHeaders` row. It never
selects any other key, so `cursorAuth/*` and the other `ItemTable` settings are never read.

Kinds: `composerData`, `bubble:user`, `bubble:assistant`, `bubble:type-<n>`,
`composerHeaders`, and the auxiliary family names. Fields: the table columns as
`$.cursorDiskKV.*` and `$.composerHeaders.*`, and the decoded values under
`$.cursorDiskKV.composerData`, `$.cursorDiskKV.bubbleId`, and `$.composerHeaders.value`.

Metadata sources:

| Field | Source |
| --- | --- |
| SessionID | the composer ID from the key |
| Title | `composerData.name`, else the header `name` |
| ProjectPath | header `workspaceIdentifier.uri.fsPath`, else `workspace.json` `folder`, else bubble `workspaceProjectDir` |
| Time range | `composerData` `createdAt`/`lastUpdatedAt`, header timestamps, bubble `createdAt` |
| Models | `composerData.modelConfig.modelName` (except the `default` placeholder), bubble `modelInfo.modelName` |
| Counts | bubbles of type 1 and 2, bubbles with `toolFormerData`; the inline `conversation` when there are no bubble rows |

A tool bubble is also a type-2 bubble, so it counts as an assistant message and a tool call.

## Shape privacy

Cursor documents use data as object keys (file URIs in `codeBlockData`, bubble and content
IDs). Both probes record such a key as `*`: only a key that looks like an identifier, with no
long hex or digit run, is kept. No value is recorded. `--include-metadata` keeps titles,
project paths, identifiers, and timestamps; the default report removes them.

## Gaps

- Neither layout is ingested. Promoting either follows the bestiary and schema ceremony, and
  the `cursor` harness adapter needs an indexer version bump to use them.
- The CLI protobuf turn graph is counted, not decoded; message order in `store.db` is only
  the blob `rowid` order.
- The IDE bubble order in `fullConversationHeadersOnly` is recorded as a shape, not used.
- Pre-`cursorDiskKV` chats in `workspaceStorage/*/state.vscdb`
  (`workbench.panel.aichat.view.aichat.chatdata`) are not read.
- `~/.cursor/ai-tracking/ai-code-tracking.db` and the Agents-window project index
  (`glass.localAgentProjects.v1` in `ItemTable`) are not declared.
- A `composerHeaders` table with other columns than the ones listed is shaped but not read.

## Sources

- Cursor CLI overview: <https://cursor.com/docs/cli/overview>
- txcript, Cursor CLI format: <https://docs.rs/crate/txcript/latest/source/docs/formats/cursor.md>
- txcript, Cursor desktop format: <https://docs.rs/crate/txcript/latest/source/docs/formats/cursor-desktop.md>
- deja-vu, Cursor registry: <https://vshulcz.github.io/deja-vu/registry/cursor.html>
- sidecar, Cursor CLI adapter: <https://github.com/marcus/sidecar/blob/main/docs/implemented/cursor-cli-adapter.md>
- agentcmd, cursor-agent notes: <https://github.com/jnarowski/agentcmd/blob/main/.agent/docs/cursor-agent.md>
- clustervision-core Cursor adapter: <https://docs.rs/clustervision-core/latest/cv_core/harness/cursor/index.html>
- tokenuse, Cursor sources: <https://tokenuse.app/docs/development/tools/cursor/>
- Export Cursor chat history from `state.vscdb`: <https://stackcone.com/blog/posts/export-cursor-chat-history-vscdb/>
- cursor-chat-recovery: <https://github.com/markwroberts0/cursor-chat-recovery/blob/main/README.md>
- Cursor forum, transcript location: <https://forum.cursor.com/t/cant-access-chat-history-since-latest-update/158688>
- Cursor forum, workspace state: <https://forum.cursor.com/t/how-to-retrieve-records-from-a-previously-closed-workspace/158695>
- `cursor-agent` 2026.09.26-dd393fe bundle: `src/state/index.ts` (chats root and MD5 bucket),
  `run-store/sqlite-blob-store.js` (tables and hex meta), `cursor-config/dist/paths.js`
  (config and data dirs), `src/transcript/cli-transcript-writer.ts` (agent transcripts)
