# Cline

Cline is a coding agent that runs as a VS Code extension (`saoudrizwan.claude-dev`), a
JetBrains plugin, and a CLI. `peasant layout capture cline` reads its session stores
read-only. Peasant does not ingest Cline.

Cline has two on-disk session formats, and both are in use:

- **Legacy tasks.** The pre-SDK extension writes one directory per task, `tasks/<taskId>/`,
  under the extension's global storage directory. The pre-SDK standalone host (JetBrains, the
  earlier CLI) uses `~/.cline/data` as that directory.
- **SDK sessions.** The SDK-based extension bundle, the current CLI, and the Cline SDK write
  one directory per session, `sessions/<sessionId>/`, under `~/.cline/data`. Since 4.1.0 the
  stable VSIX contains both bundles and a loader activates one per window, so one machine can
  hold both formats. The SDK bundle still reads legacy tasks from the extension directory.

## Roots

| Root | OS | Holds |
|------|----|-------|
| `~/.cline/data` | all | SDK sessions; legacy tasks from the standalone host |
| `{config}/Code/User/globalStorage/saoudrizwan.claude-dev` | all | legacy tasks (VS Code) |
| `{config}/Code - Insiders/User/globalStorage/saoudrizwan.claude-dev` | all | legacy tasks |
| `{config}/VSCodium/User/globalStorage/saoudrizwan.claude-dev` | all | legacy tasks |
| `{config}/Cursor/User/globalStorage/saoudrizwan.claude-dev` | all | legacy tasks |
| `{config}/Windsurf/User/globalStorage/saoudrizwan.claude-dev` | all | legacy tasks |
| `{config}/Antigravity/User/globalStorage/saoudrizwan.claude-dev` | all | legacy tasks |
| `~/.vscode-server/data/User/globalStorage/saoudrizwan.claude-dev` | linux | legacy tasks (remote VS Code) |

`{config}` is `~/.config` (or `$XDG_CONFIG_HOME`) on Linux, `~/Library/Application Support`
on macOS, and `%APPDATA%` on Windows. This is the VS Code user-data convention. `CLINE_DIR`,
`CLINE_DATA_DIR`, and `CLINE_SESSION_DATA_DIR` move the `~/.cline` roots. For a moved root,
pass it with `--path`.

## Directory tree

```
<extension globalStorage> or ~/.cline/data
  state/
    taskHistory.json                 legacy: one history item per task
  tasks/
    <taskId>/                        legacy: one task (taskId = epoch ms, e.g. 1767225600000)
      api_conversation_history.json
      ui_messages.json
      task_metadata.json
      context_history.json
      settings.json
  checkpoints/                       not captured

~/.cline/data
  sessions/
    <sessionId>/                     SDK: one session (sessionId = <epoch ms>_<5 chars>)
      <sessionId>.json               manifest
      <sessionId>.messages.json      lead-agent messages
      <agentId>.messages.json        one per sub-agent
      <agentId>__<teamTaskId>.messages.json   one per teammate task
      <sessionId>.compaction.json    compacted context, when present
  db/
    sessions.db                      SQLite index, not captured
```

## Artifacts and record shapes

All artifacts are JSON. A capture shapes every artifact that it finds and names the kind of
each record in `kinds`.

**`api_conversation_history.json`** (transcript). An array of Anthropic `MessageParam`
objects: `role` (`user` | `assistant`) and `content`, a string or an array of blocks. The block
`type` values are `text`, `image`, `document`, `tool_use` (`id`, `name`, `input`),
`tool_result` (`tool_use_id`, `content`), `thinking`, and `redacted_thinking`. Newer versions
add `id`, `ts` (epoch ms), `modelInfo` (`modelId`, `providerId`, `mode`), and `metrics`
(`tokens.prompt`, `tokens.completion`, `tokens.cached`, `cost`). Earlier versions called tools
with XML inside `text` blocks and sent the results back as `user` text. Kind:
`<role>:<sorted block types>`, for example `assistant:text+tool_use`.

**`ui_messages.json`** (transcript). An array of the rows that the extension renders: `ts`
(epoch ms, also the row identity), `type` (`say` | `ask`), `say` or `ask`, `text`, and optional
`reasoning`, `images`, `files`, `partial`, `conversationHistoryIndex`, `lastCheckpointHash`,
and `modelInfo`. `say: "task"` holds the first prompt. `say: "user_feedback"` holds a later
user message. The `text` of `say: "api_req_started"` is a JSON string with `tokensIn`,
`tokensOut`, `cacheWrites`, `cacheReads`, and `cost`. Kind: `say:<say>` or `ask:<ask>`.

**`task_metadata.json`** (metadata). `files_in_context[]` (`path`, `record_state`,
`record_source`, dates), `model_usage[]` (`ts`, `model_id`, `model_provider_id`, `mode`), and
`environment_history[]` (`ts`, OS, host, `cline_version`).

**`context_history.json`**, **`settings.json`** (auxiliary). The context-window edits and
the per-task settings overrides. They are captured as shape only.

**`state/taskHistory.json`** (index). An array of `HistoryItem`: `id`, `ulid`, `ts`, `task`
(the prompt), `tokensIn`, `tokensOut`, `cacheWrites`, `cacheReads`, `totalCost`, `size`,
`cwdOnTaskInitialization`, `isFavorited`, `modelId`, `apiProvider`. The record count is the
entry of the captured task (0 or 1). The field paths cover the whole file.

**`<sessionId>.json`** (metadata). The SDK manifest: `version`, `session_id`, `source`
(`vscode`, `cli`, `jetbrains`, ...), `pid`, `started_at`, `ended_at` (ISO 8601), `exit_code`,
`status`, `interactive`, `provider`, `model`, `cwd`, `workspace_root`, `enable_*`, `prompt`,
`metadata` (`title`, `tokensIn`, `tokensOut`, `cacheReads`, `cacheWrites`, `totalCost`, ...),
`messages_path`, `compaction_path`. Kind: `source:<source>`.

**`*.messages.json`** (transcript). An envelope: `version`, `updated_at`, `agent` (`lead` |
`subagent` | `teammate`), `sessionId`, `taskType`, `origin`, `messages[]`, `system_prompt`.
Each message has `role`, `content` (a string or blocks of type `text`, `image`, `file`,
`media`, `tool_use`, `tool_result`, `thinking`, `redacted_thinking`), and optional `id`,
`ts`, `modelInfo` (`id`, `provider`, `family`), `metrics` (`inputTokens`, `outputTokens`,
`cacheReadTokens`, `cacheWriteTokens`, `cost`), and `metadata`. User text is wrapped in
`<user_input mode="act|plan|yolo">`. Kind: the same as for the API history.

**`<sessionId>.compaction.json`** (auxiliary). `version`, `updated_at`,
`source_message_count`, hashes, and the compacted `messages[]`.

## Metadata sources

| Field | Legacy task | SDK session |
|-------|-------------|-------------|
| SessionID | directory name | `session_id`, else the directory name |
| Title | `taskHistory.json` `task`, else the first `say: "task"` text | manifest `metadata.title`, else `prompt` |
| ProjectPath | `taskHistory.json` `cwdOnTaskInitialization` | manifest `cwd`, else `workspace_root` |
| StartedAt / UpdatedAt | min/max of every `ts` in the API history, UI rows, `model_usage`, and the history item | `started_at`, `ended_at`, the messages `updated_at`, and every message `ts` |
| Models | `modelInfo.modelId`, `model_usage[].model_id`, history `modelId` | manifest `model`, message `modelInfo.id` |
| UserTurns | UI rows `say: task` and `say: user_feedback`; without UI rows, API `user` messages without a `tool_result` block | lead `user` messages without a `tool_result` block |
| AssistantMsgs | API `assistant` messages | lead `assistant` messages |
| ToolCalls | API `tool_use` blocks; without them, UI rows whose `say` or `ask` is `tool`, `command`, `use_mcp_server`, or `browser_action_launch` | lead `tool_use` blocks |

Titles are collapsed to one line, stripped of the `<user_input>` and `<mode_notice>` markup,
and cut at 240 characters. The token totals are recorded in the history item, in the
`api_req_started` rows, in the message `metrics`, and in the manifest `metadata`. `Metadata`
has no token fields, so a capture shows them only as field paths.

## Known gaps

- The capture does not read `db/sessions.db`. The manifests carry the same columns.
- The sub-agent and teammate message files are shaped, but their counts and models are not
  added to the session metadata.
- A legacy task records its working directory only in `taskHistory.json`. A task that is
  missing from the history has no project path. The directory also appears inside
  `<environment_details>` text, which the capture does not read.
- The UI-row fallback for tool calls in tasks from before `tool_use` blocks is a heuristic.
- The fork directory names follow the VS Code convention. They were not checked against each
  fork's source. `Antigravity` is the least certain. Portable installs, `--user-data-dir`,
  and the remote servers of forks (`~/.cursor-server`, `~/.windsurf-server`) are not declared.
- The shape of `context_history.json` (nested tuples keyed by message index) comes from
  observed code paths. The capture records it as shape only.
- Cline's own docs disagree with its code. `.clinerules/storage.md` puts `taskHistory.json`
  under `tasks/`, and the SDK hub page shows `sessions/sessions.db` with flat
  `<id>.json` files. This page follows the code: `state/taskHistory.json`, `db/sessions.db`,
  and `sessions/<id>/<id>.json`.

## Sources

Cline source at commit `46c9035` (2026-09-28) unless the link says otherwise:

- [v3.89.2 `core/storage/disk.ts`](https://github.com/cline/cline/blob/v3.89.2/apps/vscode/src/core/storage/disk.ts):
  the legacy task files and `state/taskHistory.json`
- [`sdk/legacy-state-reader.ts`](https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/apps/vscode/src/sdk/legacy-state-reader.ts)
- [`hosts/vscode/vscode-to-file-migration.ts`](https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/apps/vscode/src/hosts/vscode/vscode-to-file-migration.ts):
  VS Code keeps tasks in its own global storage
- [`shared/messages/content.ts`](https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/apps/vscode/src/shared/messages/content.ts),
  [`shared/ExtensionMessage.ts`](https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/apps/vscode/src/shared/ExtensionMessage.ts),
  [`shared/HistoryItem.ts`](https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/apps/vscode/src/shared/HistoryItem.ts),
  [`ContextTrackerTypes.ts`](https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/apps/vscode/src/core/context/context-tracking/ContextTrackerTypes.ts)
- [`sdk/packages/shared/src/storage/paths.ts`](https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/sdk/packages/shared/src/storage/paths.ts):
  `~/.cline/data`, `sessions/`, `db/`, and the environment overrides
- [`services/session-artifacts.ts`](https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/sdk/packages/core/src/services/session-artifacts.ts),
  [`session/models/session-manifest.ts`](https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/sdk/packages/core/src/session/models/session-manifest.ts),
  [`services/session-data.ts`](https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/sdk/packages/core/src/services/session-data.ts),
  [`shared/src/llms/messages.ts`](https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/sdk/packages/shared/src/llms/messages.ts)
- [`CHANGELOG.md`](https://github.com/cline/cline/blob/46c9035e4d9f62ecc81ee0673aeb41db1561239d/CHANGELOG.md):
  the combined legacy and SDK VSIX
- [Installing Cline](https://docs.cline.bot/getting-started/installing-cline): the supported
  editors
