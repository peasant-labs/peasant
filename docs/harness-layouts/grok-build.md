# Grok Build

Grok Build is the terminal coding agent from xAI (branded SpaceXAI on x.ai). It runs as a
full-screen TUI, headlessly with `grok -p`, or behind the Agent Client Protocol (ACP) in other
apps. The official installer ships one Rust binary as `grok`; the binary is built from the
`xai-grok-pager` crate in the public [`xai-org/grok-build`](https://github.com/xai-org/grok-build)
repository. Install it with `curl -fsSL https://x.ai/cli/install.sh | bash` on macOS and Linux,
or `irm https://x.ai/cli/install.ps1 | iex` on Windows.

The npm package `@vibe-kit/grok-cli` is a different, community tool. It also keeps files under
`~/.grok` (`user-settings.json`), but it is not Grok Build, and this layout does not describe it.

Tool name: `grok-build`.

## Roots

Grok Build keeps every piece of state under one home directory. `$GROK_HOME` replaces it when
set and non-empty; otherwise it is `<home>/.grok` on every operating system.

| OS      | Default root                | Override      |
| ------- | --------------------------- | ------------- |
| Linux   | `~/.grok`                   | `$GROK_HOME`  |
| macOS   | `~/.grok`                   | `$GROK_HOME`  |
| Windows | `%USERPROFILE%\.grok`       | `%GROK_HOME%` |

The layout declares `{home}/.grok`. For a relocated home, run
`peasant layout capture grok-build --path "$GROK_HOME"`.

## Tree

```
~/.grok/
  config.toml, auth.json, logs/, skills/, ...      not session data
  sessions/
    session_search.sqlite                          FTS5 index over titles and prompts
    <URL-encoded cwd>/                             one group per working directory
      .cwd                                         original cwd, slug-hash groups only
      prompt_history.jsonl                         prompt recall for the group
      <session id>/                                UUIDv7 unless the client supplied one
        summary.json                               session index entry
        updates.jsonl                              ACP update stream (authoritative)
        chat_history.jsonl                         conversation items sent to the model
        signals.json                               counters and latency signals
        usage.json                                 usage totals
        plan.json                                  TODO list state
        rewind_points.jsonl                        file snapshots for /rewind
        feedback.jsonl                             ratings
        system_prompt.txt, prompt_context.json,
        tool_definitions.json                      rendered prompt inputs
        compaction_checkpoints/                    compaction state
        subagents/<subagent id>/meta.json          subagent link; child is its own session
```

The group name is the URL-encoded working directory, for example `%2Fwork%2Fdemo-app`. When the
encoded name exceeds 255 bytes, Grok Build uses `<slug>-<first 16 hex of blake3(cwd)>` and
writes the original path to `.cwd` in the group.

## Artifacts and record shapes

| Artifact         | Pattern                               | Format | Role       |
| ---------------- | ------------------------------------- | ------ | ---------- |
| `summary`        | `sessions/*/*/summary.json`           | JSON   | metadata   |
| `updates`        | `sessions/*/*/updates.jsonl`          | JSONL  | transcript |
| `chat-history`   | `sessions/*/*/chat_history.jsonl`     | JSONL  | transcript |
| `signals`        | `sessions/*/*/signals.json`           | JSON   | auxiliary  |
| `usage`          | `sessions/*/*/usage.json`             | JSON   | auxiliary  |
| `plan`           | `sessions/*/*/plan.json`              | JSON   | auxiliary  |
| `rewind-points`  | `sessions/*/*/rewind_points.jsonl`    | JSONL  | auxiliary  |
| `feedback`       | `sessions/*/*/feedback.jsonl`         | JSONL  | auxiliary  |
| `subagent-meta`  | `sessions/*/*/subagents/*/meta.json`  | JSON   | metadata   |
| `cwd-marker`     | `sessions/*/.cwd`                     | text   | index      |
| `prompt-history` | `sessions/*/prompt_history.jsonl`     | JSONL  | auxiliary  |
| `search-index`   | `sessions/session_search.sqlite`      | SQLite | index      |

`summary.json` is one snake_case object. The fields the probe reads are `info.id`, `info.cwd`,
`generated_title`, `session_summary`, `created_at`, `updated_at`, `last_active_at` (RFC 3339),
and `current_model_id`. It also carries `num_messages` (update lines), `num_chat_messages`,
`chat_format_version`, fork fields (`parent_session_id`, `forked_at`, `session_kind`), Git
context (`git_root_dir`, `git_remotes`, `head_commit`, `head_branch`), `agent_name`,
`sandbox_profile`, `reasoning_effort`, and `last_turn_summary`/`last_recap`.

Each `updates.jsonl` line is an envelope:

```json
{"timestamp": 1777629605, "method": "session/update",
 "params": {"sessionId": "<id>", "update": {"sessionUpdate": "user_message_chunk",
            "content": {"type": "text", "text": "..."}}, "_meta": {"eventId": "..."}}}
```

`timestamp` is Unix seconds (it can be `0`). `method` is `session/update` for standard ACP
updates and `_x.ai/session/update` for xAI extensions. The probe counts records by
`params.update.sessionUpdate`. Kinds seen in the upstream source include `user_message_chunk`,
`agent_message_chunk`, `agent_thought_chunk`, `tool_call`, `tool_call_update`,
`available_commands_update`, `rewind_marker`, and the extensions `subagent_spawned`,
`subagent_finished`, `hook_annotation`, `diff_review`, and `git_branch_update`. Messages are
streamed, so one user prompt or one assistant reply can span several chunk lines.

Each `chat_history.jsonl` line is a `ConversationItem` tagged by `type`: `system`, `user`
(`content[]` of `text` or `image` parts, optional `synthetic_reason` and `prompt_index`),
`assistant` (`content`, `tool_calls[]` of `{id, name, arguments}`, `model_id`), `tool_result`
(`tool_call_id`, `content`), `backend_tool_call` (server-side web or X search and code
interpreter, under `kind.tool_type`), and `reasoning`. Sessions with `chat_format_version` 0
use an older message format; the probe falls back to a `role` field for those lines.

`subagents/*/meta.json` holds `subagent_id`, `parent_session_id`, `child_session_id`,
`subagent_type`, `description`, and `status`; the probe counts it by `status`. `signals.json`
uses camelCase counters such as `turnCount`, `userMessageCount`, and `assistantMessageCount`.

## Metadata sources

| Metadata          | Source                                                                  |
| ----------------- | ----------------------------------------------------------------------- |
| Session ID        | `summary.json` `info.id`, else the session directory name               |
| Title             | `generated_title`, else `session_summary`                               |
| Project path      | `info.cwd`, else the decoded group name, else the group's `.cwd`        |
| Time range        | `created_at`, `updated_at`, `last_active_at`, and update `timestamp`s   |
| Models            | `current_model_id` and every assistant `model_id` in the chat history   |
| User turns        | chat-history `user` items whose `synthetic_reason` is absent or `human` |
| Assistant messages| chat-history `assistant` items                                          |
| Tool calls        | assistant `tool_calls` plus `backend_tool_call` items                   |

When a session has no readable chat history, the probe counts from `updates.jsonl` instead:
each run of consecutive `user_message_chunk` or `agent_message_chunk` lines is one turn or one
message, and each `tool_call` line is one tool call.

The probe discovers a session from either `summary.json` or `updates.jsonl`. A malformed
`summary.json` and a partial final JSONL line are counted as malformed and skipped.

## Gaps

- The probe does not open `session_search.sqlite`, `prompt_history.jsonl`, `.cwd` (except to
  recover the project path), `compaction_checkpoints/`, or the rendered prompt files. The
  search index is derived from the JSONL files; its schema is not captured.
- `usage.json` and `feedback.jsonl` are declared from the upstream file-name constants; their
  record shapes are not documented upstream, and the fixtures do not include them.
- `session_summary` can hold the first prompt text on older sessions. It is used as the title
  only when `generated_title` is absent, and it is removed from a shape-only report.
- `updates.jsonl` timestamps have one-second resolution; chat-history items carry none.
- A session relocated to another working directory can appear under a new group; the probe
  reports each directory it finds.
- The headless-mode guide lists `sessions/` as "Session transcripts (SQLite)". The session
  guide and the storage source both show JSONL files as the source of truth with SQLite only
  for the search index; this layout follows the source.
- The layout was derived from the public source and documentation; it has not been checked
  against a capture from a real installation.

## Confidence

High for the root, the directory tree, `summary.json`, the `updates.jsonl` envelope, and the
`chat_history.jsonl` item types: each is read from the storage code and its tests at
`xai-org/grok-build` commit `f0e3be11` (2026-09-23), and matches the user guide. Medium for
the auxiliary files, which the probe shapes without interpreting.

## Sources

- [Grok Build overview](https://docs.x.ai/build/overview) and
  [settings reference](https://docs.x.ai/build/settings/reference) (`GROK_HOME`)
- [`xai-org/grok-build` README](https://github.com/xai-org/grok-build)
- [User guide: Session Management](https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-pager/docs/user-guide/17-sessions.md)
- [User guide: Headless Mode, File Locations](https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-pager/docs/user-guide/14-headless-mode.md)
- [`session/storage/mod.rs`](https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-shell/src/session/storage/mod.rs):
  file names and the `updates.jsonl` envelope
- [`session/persistence.rs`](https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-shell/src/session/persistence.rs):
  the `summary.json` structure
- [`xai-grok-sampling-types/src/conversation.rs`](https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-sampling-types/src/conversation.rs):
  `chat_history.jsonl` items
- [`xai-grok-config/src/paths.rs`](https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-grok-config/src/paths.rs)
  and [`xai-dirs/src/lib.rs`](https://github.com/xai-org/grok-build/blob/main/crates/codegen/xai-dirs/src/lib.rs):
  the home directory and the working-directory encoding
- [deja-vu: where Grok Build stores its history](https://vshulcz.github.io/deja-vu/registry/grok.html),
  an independent reader of the same layout
