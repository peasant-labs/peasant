# Devin Desktop

Tool identifier: `devin-desktop`. Declared in
[`internal/harnesslayout/devin_desktop.go`](../../internal/harnesslayout/devin_desktop.go).

## What the product is

Devin Desktop is Cognition's desktop IDE and agent manager. It is the Windsurf editor renamed.
The rename shipped on 2 June 2026 as an over-the-air update to existing Windsurf installs. It
keeps the Windsurf IDE, which is a VS Code fork, and makes the Agent Command Center the default
view. That view is a Kanban board of local and cloud agent sessions, grouped into "Spaces".

Devin Desktop runs agents through the Agent Client Protocol (ACP). These include Devin Local,
Cognition's Rust rewrite of Cascade, and third-party agents such as Codex, Claude Agent, and
OpenCode. The legacy Cascade agent was supported in parallel until 1 July 2026. Devin CLI and
Devin Cloud are separate products. Their stores are not part of this layout.

## Roots

| Root | Linux | macOS | Windows |
| --- | --- | --- | --- |
| `{config}/Devin` | `~/.config/Devin` | `~/Library/Application Support/Devin` | `%APPDATA%\Devin` |
| `{home}/.codeium/windsurf` | `~/.codeium/windsurf` | `~/.codeium/windsurf` | `%USERPROFILE%\.codeium\windsurf` |

The first root is the Electron user-data directory of the app. The second is the legacy
Windsurf/Codeium data directory. Devin Desktop still writes its Cascade trajectories there.

```text
{config}/Devin/
└── User/
    ├── acp-events/
    │   └── <uuid>.ndjson          transcript: ACP session updates, one file per session
    ├── globalStorage/
    │   └── state.vscdb            index: VS Code ItemTable (SQLite)
    └── workspaceStorage/          not declared (see gaps)
{home}/.codeium/windsurf/
├── cascade/
│   └── <cascade-id>.pb            transcript: opaque, obfuscated trajectory
└── memories/                      not declared: memories and global_rules.md, not sessions
```

## Artifacts

### `acp-events` — `User/acp-events/*.ndjson` (JSONL, transcript)

Each file is one agent session. Its name stem is a UUID, and the probe uses that stem as the
session identifier. According to tokscale, the stem is independent of the Devin CLI session
identifiers. Each line is one JSON object:

```json
{"providerId":"<agent>","notification":{"sessionUpdate":"<variant>", ...}}
```

`notification` is an ACP `SessionUpdate`. The probe counts records by
`notification.sessionUpdate`. The ACP variants are `user_message_chunk`,
`agent_message_chunk`, `agent_thought_chunk`, `tool_call`, `tool_call_update`, `plan`,
`available_commands_update`, `current_mode_update`, `config_option_update`,
`session_info_update`, and `usage_update`. Cognition extends `usage_update` with `_meta` keys
such as `cognition.ai/inputTokens`, `cognition.ai/outputTokens`,
`cognition.ai/cachedReadTokens`, `cognition.ai/cachedWriteTokens`, and `cognition.ai/model`.
Tokscale also parses an older shape, which carries token metrics under `notification.metadata`
or `notification.content.metadata`.

### `cascade-trajectory` — `cascade/*.pb` (opaque, transcript)

Each file is one Cascade conversation. Its name stem is the Cascade identifier. Two tokscale
contributors report that the files are encrypted or obfuscated at rest, and that `strings(1)`
finds nothing readable in them. Only the bundled language server decodes them, through its
`GetCascadeTrajectory` RPC. The probe never opens these files. It records the file
modification time as `updatedAt`, and a kind that places the file in a size bucket, such as
`opaque-under-64KiB`. The artifact is declared as `text` because the package has no binary
format. No content is parsed.

### `global-state` — `User/globalStorage/state.vscdb` (SQLite, index)

This file is the VS Code key-value store. The probe opens it read-only. It records every table
and column, typed by SQLite affinity: TEXT, BLOB, and untyped columns are recorded as strings.
In Windsurf, the `codeium.windsurf` ItemTable entry is a JSON object with keys such as
`windsurf.state.cachedActiveTrajectory:<workspace-id>` and
`windsurf.state.cachedTrajectorySummaries:<workspace-id>`. The values of these keys are base64
protobuf. The probe lets SQLite enumerate the top-level keys of that entry with `json_each`,
so no value reaches Peasant. It replaces each workspace identifier with `*` and counts the
keys by their normalized name, for example `windsurf.state.cachedActiveTrajectory:*`.

The probe reads no other ItemTable entry. It never reads `secret://` entries, which hold VS
Code's encrypted extension secrets, or any other key. This database is not a session, so it
appears once in the report under its relative path.

## Metadata sources

| Field | Source |
| --- | --- |
| `sessionId` | file name stem (ACP stream or Cascade trajectory) |
| `title` | last non-empty `session_info_update.title` |
| `startedAt`/`updatedAt` | `updatedAt`, `created_at`, `timestamp`, `metadata.created_at`, `content.metadata.created_at` in ACP updates; file modification time for a Cascade trajectory |
| `models` | `_meta["cognition.ai/model"]`, `metadata.generation_model`, `content.metadata.generation_model` |
| `userTurns` | runs of consecutive `user_message_chunk` records with the same `messageId` |
| `assistantMessages` | runs of consecutive `agent_message_chunk` records with the same `messageId` |
| `toolCalls` | `tool_call` records; `tool_call_update` records are not counted |

`projectPath` is not recovered. ACP carries the working directory in the `session/new`
request, not in the updates. There is no evidence that the stream records that request.

## Opacity and gaps

- The Cascade `.pb` trajectories are opaque. Decoding them requires running Cognition's
  language server, and this probe does not do that.
- The `codeium.windsurf` values are protobuf. A reverse-engineered field map exists
  (windsurf-trajectory-extractor), but the probe does not decode them. So Cascade titles,
  and the links from trajectories to workspaces, are not recovered.
- The message counts are approximate. They count streamed chunks, and a chunk stream that
  omits `messageId` merges adjacent messages of the same kind.
- An ACP stream may describe the same conversation as a Cascade trajectory. The probe does not
  correlate them.
- `User/workspaceStorage/<hash>/state.vscdb` and `workspace.json` belong to Windsurf's older
  Chat mode and map workspace hashes to folders. They are not declared.
- Legacy pre-rename roots such as `{config}/Windsurf` and `{config}/Windsurf - Next` are not
  declared. Tokscale also scans a lowercase `~/.config/devin` on Linux; it is not declared.

## Confidence

- **High:** the product identity and the rename. Sources: Cognition's blog and FAQ.
- **High:** `~/.codeium/windsurf/cascade/*.pb` on macOS and Windows, and their opacity.
  Sources: two independent users in tokscale #885 and #894.
- **Medium:** `{config}/Devin/User/acp-events/*.ndjson` on macOS and Windows, and its record
  envelope. The source is tokscale's shipped parser, not a Cognition document.
- **Low:** the Linux path, which tokscale scans but no user has confirmed.
- **Medium:** the ACP `sessionUpdate` variants. Source: the ACP specification.
- **Low:** that Devin Desktop's `globalStorage/state.vscdb` keeps the `codeium.windsurf`
  entry. The only evidence is from Windsurf before the rename (windsurf-trajectory-extractor);
  after the rename, it is inferred from the continued Windsurf extension and data directory.
- No layout here was checked against a real Devin Desktop install.

## Sources

- Cognition, "Windsurf is now Devin Desktop": <https://devin.ai/blog/windsurf-is-now-devin-desktop>
- Devin Desktop FAQ: <https://docs.devin.ai/desktop/devin-desktop-faq>
- Devin Docs, memories and rules (`~/.codeium/windsurf/memories/`): <https://docs.devin.ai/desktop/cascade/memories>
- tokscale Devin parser: <https://github.com/junhoyeo/tokscale/blob/1d9a9395418efc6952944b794097935d7d6fa1e8/crates/tokscale-core/src/sessions/devin.rs>
- tokscale scan roots: <https://github.com/junhoyeo/tokscale/blob/1d9a9395418efc6952944b794097935d7d6fa1e8/crates/tokscale-core/src/scanner.rs>
- tokscale PR #885 (Windows `acp-events` root, `.pb` opacity on macOS): <https://github.com/junhoyeo/tokscale/pull/885>
- tokscale issue #894 (Cascade trajectory locations and decoding): <https://github.com/junhoyeo/tokscale/issues/894>
- windsurf-trajectory-extractor (`codeium.windsurf` keys): <https://github.com/jijiamoer/windsurf-trajectory-extractor/blob/e7d5c04b3708e968a353ecf32020e034b97fbaf3/src/windsurf_trajectory/extractor.py>
- ACP prompt turn and `SessionUpdate` variants: <https://agentclientprotocol.com/protocol/prompt-turn>
