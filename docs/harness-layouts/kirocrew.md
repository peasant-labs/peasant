# Kiro Crew

[Kiro Crew](https://kiro.dev/crew/) (`kirocrew`) is an open-source (Apache-2.0) persistent
development workspace from the Kiro team at AWS, published at
[kirodotdev/KiroCrew](https://github.com/kirodotdev/KiroCrew). A long-running Python process, the
Gateway, drives `kiro-cli` over the Agent Client Protocol (ACP). It serves the desktop app, the web
dashboard, the `kirocrew` CLI, and chat channels such as Slack and Discord, and it runs subagents,
cron jobs, and task runs.

Kiro Crew is not a thin wrapper. `kiro-cli` still writes its own replay log for each ACP session, but
Kiro Crew keeps its own transcript and its own event log for every session in its data home. This
page covers only the Kiro Crew data home. The `kiro-cli` store (`~/.kiro/sessions/cli/`) is the
`kiro-cli` layout in [`kiro.md`](kiro.md).

The layout was derived from the Kiro Crew source at commit `bc31c3c` (2026-09-28).

## Roots

| OS | Root |
| --- | --- |
| Linux, macOS, Windows | `{home}/.kiro/crew` |
| Linux, macOS, Windows | `{home}/.kirocrew` (legacy, deprecated, not migrated) |

`KIROCREW_HOME` overrides the data home. The desktop app, the one-line installer, and the source
build all use the default. The Docker image persists `/home/kirocrew`, so the data home in the
container is `/home/kirocrew/.kiro/crew`. Use `--path` for an override or for a container volume.

## Tree

```text
~/.kiro/crew/
├── session_map.json                      session key -> kiro-cli session id, cwd, provider, links
├── sessions/
│   ├── <stem>.jsonl                      one transcript per session key
│   ├── <stem>.attachments/               images the transcript links to (not captured)
│   ├── archive/<stem>__<YYYYMMDD-HHMMSS>[-n].jsonl   rotated rows, kept 7 days
│   ├── .threads/<stem>.json              reply threads on messages of <stem>
│   └── .index/session_index.db           FTS5 search index (not captured)
├── crew-log/
│   └── sessions/<store name>/
│       ├── log.jsonl                     first segment (seq 1 onward)
│       ├── log.<first seq>.jsonl         later segments
│       ├── .lock
│       └── .lease
└── subagents/<id>/state.json             subagent run state (not captured)
```

`<stem>` is the session key with every character outside word characters, `-`, and `.` replaced by
`_` (`history._safe_key`). A dashboard slot `3` has the key `dashboard:3` and the stem `dashboard_3`.
A Slack thread has the key `slack:<thread_ts>`. `<store name>` is a readable prefix of the unit id
plus an 8-character SHA-256 digest. The raw unit id is in the crew-log header.

## Sessions

One session is one transcript stem. It includes the transcript, its archive segments, its threads
sidecar, the session map entries whose key sanitizes to the stem, and every crew-log unit whose header
`slot` names the stem, either as the stem itself or as `dashboard_<slot>`. Each ACP session a
conversation ran under has its own crew-log unit, so one transcript can own several units.

A crew-log unit that names no transcript, such as a subagent run, is a session of its own, keyed by
the header `id`. A unit whose header does not decode cannot be attributed and is skipped.

## Artifacts and record shapes

### `transcript`: `sessions/*.jsonl`

Line 1 is a metadata object: `_type: "metadata"`, `created_at` (ISO 8601 with offset),
`last_consolidated`, and optional `title`, `agent`, `model`, `project` (a project directory), `workspace`,
`mode`, `pinned`, `closed`, `tags`, `tab_id`, `linked_session_key`, `folder_id`, and more. Every other
line is a message row: `role`, `content`, `ts`, and optional `tools` (tool names on legacy channel
rows), `cls`, `source_thread`, `source_user`, `meta.mid`, `variants`, and `variant_idx`. The roles are
`user`, `assistant`, `system`, and the dashboard tool roles `tool`, `tool_call`, and `tool_result`.

Kind census: the `_type` value, otherwise the `role`.

### `archive`: `sessions/archive/*__*.jsonl`

Line 1 is `{_type: "archive", reason, archived_at, count}`. The rows that follow are message rows
rotated out of the live transcript. The stem is everything before the last `__`, because session keys
may contain dots. Kind census: as for the transcript.

### `threads`: `sessions/.threads/*.json`

`{version: 1, threads: {<message mid>: [{id, role, content, ts}]}}`. The capture folds each message id
to the placeholder `{mid}`, so identifiers never become field paths. Each reply is one record. Kind
census: the reply `role`.

### `session-map`: `session_map.json`

One object keyed by session key. An entry is `{sid, slack_thread_ts, slack_channel_id}` plus optional
`provider`, `cwd`, `link`, and flags. A legacy entry is a bare `sid` string. The capture records only
the entries of the captured session. Kind census: `provider` (`acp` when absent) or `legacy`.

### `crew-log`: `crew-log/sessions/*/log*.jsonl`

Line 1 of every segment is a header: `{type: "session", version: 1, id, owner, agent, task, pack,
slot, thread, cwd, remote, createdAt}` (`createdAt` in epoch milliseconds). Every other line is an
entry: `{type, seq, time, src, [thread], [ref], [ignorable], data}`. `type` is `domain/action`, for
example `session/opened`, `turn/started`, `turn/completed`, `message/received`, `message/sent`,
`tool/called`, `tool/completed`, `model/selected`, `approval/requested`, and `subagent/spawned`. Tool
arguments and results are recorded only as a hash and a byte count. Message bodies are in
`data.text`. Kind census: `type`.

## Metadata sources

| Field | Source |
| --- | --- |
| `sessionId` | Transcript stem. For a session made only of a crew log, the header `id`. |
| `title` | Transcript metadata `title`. |
| `projectPath` | Transcript metadata `project`, then session map `cwd`, then crew-log `cwd`. |
| `startedAt`, `updatedAt` | Range of the transcript `created_at` and row `ts`, and the crew-log `createdAt` and entry `time`. |
| `models` | Transcript metadata `model`, then crew-log `data.model` (`session/opened`, `model/selected`, `turn/completed`), in segment order. |
| `userTurns` | Transcript and archive rows with `role: "user"`. With no transcript, `message/received` entries with `data.role: "user"`. |
| `assistantMessages` | Transcript and archive rows with `role: "assistant"`. With no transcript, `message/sent` entries. |
| `toolCalls` | `tool/called` entries when a crew log exists. Otherwise the transcript `tools` names plus rows with role `tool` or `tool_call`. |

## Gaps

- The `kiro-cli` replay log (`~/.kiro/sessions/cli/<sid>.json` and `.jsonl`) holds the full tool
  arguments and results. The session map `sid` links it to a Kiro Crew session. Capture it with
  `peasant layout capture kiro-cli`; see [`kiro.md`](kiro.md). The two captures are not joined.
- `sessions/.index/session_index.db` is derived from the transcripts and stores folded transcript
  text, so the capture does not open it.
- Attachments, `subagents/<id>/state.json` and `result.txt`, and the pre-projection
  `ledger/<store name>/state.json` are not captured.
- The transcript tool count is a heuristic. Legacy channel rows list tool names in `tools`, and
  dashboard rows use tool roles. The crew log is exact, and the capture prefers it.
- Crew logs arrived with upstream #10091. An older session has only a transcript.
- A `KIROCREW_HOME` override is not detected. Pass `--path`.

## Sources

- [kirodotdev/KiroCrew](https://github.com/kirodotdev/KiroCrew) and
  [kiro.dev/docs/crew](https://kiro.dev/docs/crew/)
- [`src/kiro_crew/config/paths.py`](https://github.com/kirodotdev/KiroCrew/blob/main/src/kiro_crew/config/paths.py):
  the data home, the legacy home, and the `kiro-cli` sessions directory
- [`src/kiro_crew/history.py`](https://github.com/kirodotdev/KiroCrew/blob/main/src/kiro_crew/history.py):
  transcript, archive, and threads sidecar writers
- [`src/kiro_crew/session_storage.py`](https://github.com/kirodotdev/KiroCrew/blob/main/src/kiro_crew/session_storage.py):
  which files make up one session, and how crew-log units join transcripts
- [`src/kiro_crew/session_map.py`](https://github.com/kirodotdev/KiroCrew/blob/main/src/kiro_crew/session_map.py)
- [`docs/architecture/overview.md#data-home`](https://github.com/kirodotdev/KiroCrew/blob/main/docs/architecture/overview.md#data-home)
- [`docs/reference/crew-log/envelope.md`](https://github.com/kirodotdev/KiroCrew/blob/main/docs/reference/crew-log/envelope.md)
  and [`session-types.md`](https://github.com/kirodotdev/KiroCrew/blob/main/docs/reference/crew-log/session-types.md)
