# Kiro and Kiro CLI

Kiro is the AWS agentic IDE (a VS Code fork). Kiro CLI is the successor of the Amazon Q
Developer CLI. Peasant does not ingest either. Two layouts describe their session stores:

- `kiro`: the unified session directories that Kiro IDE 1.0 and Kiro CLI 3.0 write, and the
  older IDE formats in the extension's `globalStorage`.
- `kiro-cli`: the CLI 2.x file sessions and the SQLite conversation store.

Kiro Crew drives `kiro-cli`, so its sessions also appear in the `kiro-cli` store. Its own
transcripts and event log are the `kirocrew` layout in [`kirocrew.md`](kirocrew.md).

```console
peasant layout capture kiro
peasant layout capture kiro-cli
```

Kiro changed its storage format several times. A machine can hold every generation at once,
and the 1.0 update did not delete the older files. Each generation is a separate session in a
capture. The probes do not merge a session across generations.

## Roots

| Layout     | OS      | Root                                                             |
|------------|---------|------------------------------------------------------------------|
| `kiro`     | all     | `~/.kiro/sessions`                                               |
| `kiro`     | Linux   | `~/.config/Kiro/User/globalStorage/kiro.kiroagent`               |
| `kiro`     | macOS   | `~/Library/Application Support/Kiro/User/globalStorage/kiro.kiroagent` |
| `kiro`     | Windows | `%APPDATA%\Kiro\User\globalStorage\kiro.kiroagent`               |
| `kiro`     | Linux   | `~/.kiro-server/data/User/globalStorage/kiro.kiroagent` (remote server) |
| `kiro-cli` | all     | `~/.kiro/sessions/cli`                                           |
| `kiro-cli` | Linux   | `~/.local/share/kiro-cli` (holds `data.sqlite3`)                 |
| `kiro-cli` | macOS   | `~/Library/Application Support/kiro-cli`                         |
| `kiro-cli` | Windows | `%LOCALAPPDATA%\kiro-cli` and `%APPDATA%\kiro-cli`               |
| `kiro-cli` | Linux   | `~/.local/share/amazon-q` (Amazon Q Developer CLI)               |
| `kiro-cli` | macOS   | `~/Library/Application Support/amazon-q` (Amazon Q Developer CLI) |
| `kiro-cli` | Windows | `%LOCALAPPDATA%\amazon-q` and `%APPDATA%\amazon-q` (Amazon Q Developer CLI) |

`KIRO_HOME` moves `~/.kiro`, and `XDG_DATA_HOME` moves `~/.local/share`. The default roots do
not follow either variable. Pass the moved directory with `--path`.

## Directory tree

```text
~/.kiro/sessions/
├── <workspace-hash>/                  16 hex characters
│   └── sess_<uuid>/
│       ├── session.json               metadata
│       ├── messages.jsonl             transcript (event log)
│       ├── publish.cursor             not captured
│       └── snapshots/                 not captured
└── cli/                               kiro-cli layout
    ├── <uuid>.json                    header
    └── <uuid>.jsonl                   transcript

<config>/Kiro/User/globalStorage/kiro.kiroagent/
├── workspace-sessions/
│   └── <base64url(workspace path)>/
│       ├── sessions.json              index
│       ├── <uuid>.json                session
│       └── .migrated-<uuid>.json      1.0 migration marker
├── <workspace-hash>/                  32 hex characters, or "default"
│   ├── <execution-id>.chat            legacy session
│   ├── <32-hex>                       execution index
│   └── <32-hex>/
│       └── <32-hex>                   execution log
└── dev_data/                          not captured

<data>/kiro-cli/data.sqlite3           conversations_v2 and conversations tables
```

## `kiro` artifacts

### `session.json` and `messages.jsonl`

Kiro IDE 1.0 and Kiro CLI 3.0 write these files. The two products share one agent harness.
Kiro-Ception states that the parent directory name is the first 16 hex characters of the
SHA-256 of the workspace path. The session directory has a `sess_` prefix.

`session.json` is one object: `schemaVersion`, `dataModelVersion`, `id`, `title`, `agentMode`,
`workspacePaths[]`, `modelId`, `status`, `createdAt`, `lastModifiedAt`. The kind census counts
`status`.

`messages.jsonl` holds one `{id, timestamp, payload}` record per line. `payload.type` is one of
`session_start`, `user`, `turn_start`, `assistant`, `tool_call`, `tool_result`,
`session_metadata`, `usage_summary`, `turn_end`, `session_event`, or `pending_interaction`. An
`assistant` payload carries `operationType` (`Say` or `Reasoning`). The census counts
`payload.type`, and adds `:operationType` when it is present.

| Metadata      | Source                                                      |
|---------------|-------------------------------------------------------------|
| SessionID     | `session.json` `id`, else the directory name                 |
| Title         | `session.json` `title`                                       |
| ProjectPath   | `session.json` `workspacePaths[0]`                           |
| Time range    | `createdAt`, `lastModifiedAt`, and every record `timestamp`  |
| Models        | `session.json` `modelId`                                     |
| User turns    | `payload.type == "user"`                                     |
| Assistant     | `payload.type == "assistant"`, including `Reasoning`          |
| Tool calls    | `payload.type == "tool_call"`                                |

### `workspace-sessions/<encoded>/<uuid>.json`

This is the IDE session format before 1.0. It is one object: `sessionId`, `title`,
`selectedModel`, `workspaceDirectory`, and `history[]` of `{message: {role, content},
executionId?}`. `content` is a string or an array of `{type, text}` parts. An assistant message
that has an `executionId` is often the stub "On it.". The answer is in the execution log. The
census counts `message.role`.

The directory name is the workspace path in URL-safe base64. The probe reads
`workspaceDirectory` first, and decodes the directory name only when that field is absent. The
capture also shapes the sibling `sessions.json` index and the `.migrated-<uuid>.json` marker,
`{migratedAt, v2SessionId}`, when they exist.

Metadata: SessionID from `sessionId`, Title from `title`, ProjectPath as described above,
Models from `selectedModel`, and user turns and assistant messages from `message.role`. The
file records no timestamps and no tool calls.

### `<workspace-hash>/<execution-id>.chat`

This is the earliest IDE format, one execution per file: `executionId`, `actionId`,
`context[]`, `validations`, `chat[]` of `{role, content}`, and `metadata` with `modelId`,
`modelProvider`, `workflow`, `workflowId`, `startTime`, and `endTime` (epoch milliseconds).
`role` is `human`, `bot`, or `tool`. The census counts `role`.

Metadata: SessionID from `executionId`, the time range from `metadata.startTime` and
`metadata.endTime`, and Models from `metadata.modelId`. User turns count `human` messages,
except the system prompt that starts with `<identity>`. Assistant messages count `bot`
messages. Tool calls count the `<tool_use>` envelopes in `bot` content. The file records no
workspace path.

### `<workspace-hash>/<32-hex>/<32-hex>` execution logs

These are extensionless JSON files: `executionId`, `chatSessionId`, `workflowType`, `status`,
`startTime`, `endTime`, `input.data.userPrompt`, `actions[]`, and `usageSummary[]`. Each
action has `actionId`, `actionType`, `actionState`, `emittedAt`, and an optional `output`. The
census counts `actionType`.

Metadata: SessionID from `chatSessionId`. This value links the log to a workspace session. The
time range comes from `startTime`, `endTime`, and `actions[].emittedAt`. One user turn is
counted when `input.data.userPrompt` is set. Assistant messages count `say` actions. Tool calls
count every other action type.

### `<workspace-hash>/<32-hex>` execution index

This is an extensionless `{executions[], version}` document. Each entry has `executionId`,
`type`, `status`, and `startTime`. The census counts `type`. The time range is the only
metadata.

## `kiro-cli` artifacts

### `~/.kiro/sessions/cli/<uuid>.json` and `<uuid>.jsonl`

CLI 2.x writes these files. The header is one object: `session_id`, `cwd`, `created_at`,
`updated_at` (RFC 3339), `title`, and `session_state`. `session_state` holds
`rts_model_state.model_info.model_id` and
`conversation_metadata.user_turn_metadatas[]`, with `end_timestamp`, `builtin_tool_uses`,
`total_request_count`, and `metering_usage[]`.

The transcript holds one `{version, kind, data}` record per line. `kind` is `Prompt`,
`AssistantMessage`, `ToolResults`, or `Clear`. `data.content[]` holds `{kind, data}` blocks.
The block kind is `text`, `thinking`, `toolUse` (`{toolUseId, name, input}`), or
`toolResult` (`{toolUseId, content, status}`). `Prompt` records carry `data.meta.timestamp` in
epoch seconds. Kiro Crew's usage reader also looks for a top-level `timestamp`, so the probe
reads both. The census counts `kind`.

Metadata: SessionID from `session_id`, Title from `title`, ProjectPath from `cwd`, and Models
from `rts_model_state.model_info.model_id`. The time range comes from `created_at`,
`updated_at`, `end_timestamp`, and `Prompt` timestamps. User turns count `Prompt` records.
Assistant messages count `AssistantMessage` records. Tool calls count `toolUse` blocks.

### `data.sqlite3`

The probe opens the database read-only with `mode=ro`. Each row of a known table is one
session. The `conversation-row` artifact records every column as `$.<table>.<column>` with the
SQLite storage type that was seen. The `conversation-value` artifact shapes the `value` JSON.
Its records are the user and assistant message of each `history[]` entry. The census names
each record by side and by serde variant: `user:Prompt`, `user:ToolUseResults`,
`user:CancelledToolUses`, `assistant:Response`, and `assistant:ToolUse`.

- `conversations_v2` (Kiro CLI): `key` (working directory), `conversation_id`, `created_at`,
  `updated_at` (epoch milliseconds), and `value`.
- `conversations` (Amazon Q Developer CLI migration 007): `key TEXT PRIMARY KEY` (working
  directory) and `value`.

`value` is a serialized `ConversationState`: `conversation_id`, `next_message`, `history[]` of
`{user, assistant, request_metadata}`, `valid_history_range`, `transcript[]`, `tools`,
`context_manager`, `latest_summary`, `model`, `model_info{model_name, model_id,
context_window_tokens}`, `file_line_tracker`, and `checkpoint_manager`. A user message has
`additional_context`, `env_context.env_state.current_working_directory`, `content`,
`timestamp` (RFC 3339 with an offset), and `images`. `assistant.ToolUse.tool_uses[]` has `id`,
`name`, `orig_name`, `args`, and `orig_args`. `request_metadata` has `model_id`,
`request_start_timestamp_ms`, and `stream_end_timestamp_ms`.

Metadata: SessionID from `conversation_id` (the column, else the value), ProjectPath from
`key`, else from the first `current_working_directory`. The time range comes from the
`created_at` and `updated_at` columns, the user `timestamp` values, and the request
timestamps. Models come from `model_info.model_id`, `model`, and `request_metadata.model_id`.
User turns count the `Prompt` variant, or plain text content. Assistant messages count the
`assistant` entries. Tool calls count `ToolUse.tool_uses[]`. The row records no title.

## Known gaps and uncertainties

- Kiro and Kiro CLI are closed source. Two open-source projects are primary sources:
  - The Amazon Q Developer CLI defines the `conversations` table, the `ConversationState`
    JSON, and `dirs::data_local_dir()/amazon-q/data.sqlite3`.
  - Kiro Crew, the Kiro team's Apache-2.0 workspace that drives `kiro-cli`, confirms these
    Kiro CLI facts in its production code:
    - The transcript store is `<KIRO_HOME or ~/.kiro>/sessions/cli`, with a `<sid>.json`
      header and a `<sid>.jsonl` transcript (`config/paths.py`, `session_map.py`).
    - The `data.sqlite3` roots for `kiro-cli` and `amazon-q` on each OS are the ones listed
      above (`identity_stores.py`).
    - The transcript records are `{kind, data: {content: [{kind, data}]}}`, with the kinds
      `Prompt`, `AssistantMessage`, and `ToolResults`, and `toolResult` blocks that carry
      `toolUseId` (`acp/client.py`, `dashboard/handlers/usage.py`).
  - Still from third-party readers only: the `conversations_v2` table, the header fields,
    the `Clear` kind, and every IDE format.
- The Kiro documentation confirms only that sessions live under `~/.kiro` (`KIRO_HOME`), that
  session IDs are UUIDs, that sessions are stored per directory, and that CLI 3.0 changed the
  session format.
- Kiro does not publish the Windows `data.sqlite3` location. Kiro Crew probes
  `%LOCALAPPDATA%\kiro-cli` first, as the location the current generation writes, and keeps
  `%USERPROFILE%\AppData\Roaming\kiro-cli` as a legacy fallback. Both are declared in that
  order. The default roots use the home-relative `AppData\Local` and do not follow a moved
  `LOCALAPPDATA`.
- No source publishes the fields of `workspace-sessions/<encoded>/sessions.json`. The probe
  records its shape and treats a top-level array, or a `sessions[]` array, as its records.
- The meaning of the constant execution directory `414d1636299d2b9e4ce7e17fb11f63e9` is not
  known. The probe accepts any 32-hex directory and file name.
- The execution-log metadata counts every non-`say` action as a tool call. Some action types
  could be control steps.
- Legacy `.chat` files and execution logs record only a workspace hash. The path is in
  `Kiro/User/workspaceStorage/<hash>/workspace.json`, outside the capture root, so these
  sessions have no ProjectPath.
- Execution logs are not joined to their workspace session. Join them by `chatSessionId`.

## Sources

- Kiro CLI commands, session management, and `KIRO_HOME`:
  <https://kiro.dev/docs/reference/cli-commands/>
- Kiro CLI 3.0 unified harness and session format change: <https://kiro.dev/docs/cli/v3/>
- `~/.kiro/sessions/<hash>/`, `workspace-sessions/`, `sessions.json`, and the `.migrated-`
  markers: <https://github.com/kirodotdev/Kiro/issues/9472>
- `globalStorage/kiro.kiroagent` growth on macOS: <https://github.com/kirodotdev/Kiro/issues/5469>
- IDE execution records under `kiro.kiroagent`:
  <https://builder.aws.com/content/3HVFGgtEiTeFTUTzmNCWhopvLQL/how-i-recovered-a-lost-kiro-ai-brainstorming-session-and-what-i-learned-about-where-kiro-actually-stores-your-work>
- Amazon Q Developer CLI storage:
  <https://github.com/aws/amazon-q-developer-cli/blob/main/crates/chat-cli/src/database/sqlite_migrations/007_conversations_table.sql>,
  <https://github.com/aws/amazon-q-developer-cli/blob/main/crates/chat-cli/src/database/mod.rs>,
  <https://github.com/aws/amazon-q-developer-cli/blob/main/crates/chat-cli/src/cli/chat/conversation.rs>,
  <https://github.com/aws/amazon-q-developer-cli/blob/main/crates/chat-cli/src/cli/chat/message.rs>,
  <https://github.com/aws/amazon-q-developer-cli/blob/main/crates/chat-cli/src/util/paths.rs>
- codeburn Kiro provider, every generation with fixtures:
  <https://github.com/getagentseal/codeburn/blob/main/docs/providers/kiro.md>,
  <https://github.com/getagentseal/codeburn/blob/main/src/providers/kiro.ts>
- Kiro-Ception loaders: <https://github.com/DevOps-Nirvana/Kiro-Ception/blob/main/src/kiro_ception/ide_loader.py>,
  <https://github.com/DevOps-Nirvana/Kiro-Ception/blob/main/src/kiro_ception/cli_loader.py>
- `conversations_v2` columns: <https://github.com/junbaor/kiro-cli-chat-viewer/blob/main/database-schema.md>
- CLI and IDE session files: <https://vshulcz.github.io/deja-vu/guide/delete-kiro-session-history.html>,
  <https://github.com/aws-samples/sample-kiro-cli-multiagent-development/blob/main/docs/observability.md>
- Default paths per OS: <https://github.com/pajaydev/kiro-history>, <https://openusage.sh/docs/providers/kiro/>
- Kiro Crew source at `2e09b10`: <https://github.com/kirodotdev/KiroCrew/blob/2e09b10304baee7c245b10992a3be1dc4374cb1f/src/kiro_crew/config/paths.py>,
  <https://github.com/kirodotdev/KiroCrew/blob/2e09b10304baee7c245b10992a3be1dc4374cb1f/src/kiro_crew/identity_stores.py>,
  <https://github.com/kirodotdev/KiroCrew/blob/2e09b10304baee7c245b10992a3be1dc4374cb1f/src/kiro_crew/session_map.py>,
  <https://github.com/kirodotdev/KiroCrew/blob/2e09b10304baee7c245b10992a3be1dc4374cb1f/src/kiro_crew/acp/client.py>,
  <https://github.com/kirodotdev/KiroCrew/blob/2e09b10304baee7c245b10992a3be1dc4374cb1f/src/kiro_crew/dashboard/handlers/usage.py>
