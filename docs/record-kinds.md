# Record-kind registry: human view

The registry itself lives at `internal/ingest/record_kinds.yaml`: one row per
harness record or part kind, naming the parser disposition, the stored shape,
and the reader treatment. It is the per-harness mapping deliverable of
peasant-labs/peasant#397, enforced by the drift test in
`internal/ingest/record_kinds_drift_test.go`, which fails when production code
and the registry disagree in either direction.

## Reading a row

- **Status** — `represented` (stored and shown as transcript content),
  `tracked-only` (stored for tracking and search, hidden), `ignored-control`
  (no entry; accounted with a reason, never blocks certification), `refused`
  (no entry; the typed refusal names the kind and the capture stays
  incomplete).
- **Preview** — whether the kind carries a human-readable action line.
- **Payload** — the retained `extra` shape, or `none`.
- **Visualized** — `rendered` (with its renderer), `hidden`, `planned`, or
  `not-applicable`. The `visualized` column is the shared source of truth with
  the fairtrade rendering work (peasant-labs/fairtrade-design-system#87 for the
  first renderer, #88 for the visibility rule).
- **Detail** — the refusal or ignore reason, or the renderer for rendered
  kinds.
- **Source** — the first-party code reference, or the census comment on
  peasant-labs/peasant#385 that observed the kind.

A kind absent from the registry is refused: every strict parser ends its
dispatch with an `UnrepresentedRecordError` for an unlisted kind. The run
report names refused kinds with counts and lists the tracked-not-visualized
kinds, so both stay visible decisions.

## Changing the vocabulary

Adding or removing a kind in a parser requires a registry edit in the same
change, then a regeneration of the table below:

```bash
go generate ./internal/ingest/
```

`adapter_version` and `indexer_version` name the `HarvesterVersionRegistry`
baseline the mapping was written against; a version bump forces an explicit
re-verification here. Pi has no section until it grows a strict kind
vocabulary to walk. Version history for the indexer revisions lives in
`docs/indexer-version-history.md`.

<!-- BEGIN GENERATED RECORD KINDS: do not hand-edit; run go generate ./internal/ingest/ -->

## claude-code (adapter 1, indexer 17)

| Kind | Status | Preview | Payload | Visualized | Detail |
|---|---|---|---|---|---|
| `user` | represented | yes | none | rendered | TranscriptViewer user turn (fairtrade adaptTranscript) |
| `assistant` | represented | yes | none | rendered | TranscriptViewer assistant turn (fairtrade adaptTranscript) |
| `human` | represented | yes | none | rendered | TranscriptViewer user turn (fairtrade adaptTranscript) |
| `system` | represented | yes | none | rendered | TranscriptViewer system turn (fairtrade adaptTranscript) |
| `summary` | represented | yes | none | rendered | TranscriptViewer system turn (fairtrade adaptTranscript) |
| `result` | represented | yes | none | rendered | TranscriptViewer system turn (fairtrade adaptTranscript) |
| `api_error` | represented | yes | none | rendered | TranscriptViewer system turn (fairtrade adaptTranscript) |
| `text` | represented | yes | none | rendered | TranscriptViewer turn content (fairtrade adaptTranscript) |
| `thinking` | represented | yes | none | rendered | ThinkingVM (fairtrade adaptTranscript) |
| `tool_use` | represented | yes | none | rendered | TranscriptViewer tool turn (fairtrade adaptTranscript) |
| `tool_result` | represented | yes | none | rendered | TranscriptViewer tool turn (fairtrade adaptTranscript) |
| `tool_reference` | ignored-control | no | none | not-applicable | Control block naming a deferred tool schema; no conversation content. |
| `compact_boundary` | represented | yes | full compactMetadata | planned |  |
| `permission-mode` | represented | yes | value + scope | planned |  |
| `mode` | represented | yes | value + scope | planned |  |
| `agent-setting` | represented | yes | value + scope | planned |  |
| `agent-name` | represented | yes | value + scope | planned |  |
| `ai-title` | represented | yes | value | planned |  |
| `atis-latch` | represented | yes | payload | planned |  |
| `attachment` | tracked-only | no | sub-kind + payload | hidden |  |
| `bridge-session` | represented | yes | payload | planned |  |
| `started` | represented | yes | payload + scope | planned |  |
| `cost-state` | represented | yes | snapshot | planned |  |
| `custom-title` | represented | yes | payload | planned |  |
| `file-history-delta` | represented | yes | payload | planned |  |
| `frame-link` | represented | yes | payload | planned |  |
| `fork-context-ref` | represented | yes | payload | planned |  |
| `worktree-state` | represented | yes | payload | planned |  |
| `launched` | represented | yes | payload | planned |  |
| `pr-link` | represented | yes | number/repository/url | planned |  |
| `artifact-` | represented | yes | payload | planned |  |
| `progress` | ignored-control | no | none | not-applicable | Contentless harness progress marker; a content-bearing record still refuses. |
| `queue-operation` | ignored-control | no | none | not-applicable | Contentless harness queue marker; a content-bearing record still refuses. |
| `file-history-snapshot` | ignored-control | no | none | not-applicable | Contentless history snapshot marker; a content-bearing record still refuses. |
| `turn_duration` | ignored-control | no | none | not-applicable | Contentless system timing subtype. |
| `stop_hook_summary` | ignored-control | no | none | not-applicable | Contentless system hook subtype. |
| `last-prompt` | ignored-control | no | none | not-applicable | Mirror of the prompt the user turn already represents; recorded as metadata. |
| `image` | refused | no | none | not-applicable | Content block with no representation; refusal keeps the capture incomplete until a build represents it. |
| `document` | refused | no | none | not-applicable | Content block with no representation; refusal keeps the capture incomplete until a build represents it. |
| `server_tool_use` | refused | no | none | not-applicable | Content block with no representation; refusal keeps the capture incomplete until a build represents it. |
| `advisor_tool_result` | refused | no | none | not-applicable | Content block with no representation; refusal keeps the capture incomplete until a build represents it. |

## codex (adapter 1, indexer 16)

| Kind | Status | Preview | Payload | Visualized | Detail |
|---|---|---|---|---|---|
| `session_meta` | ignored-control | no | none | not-applicable | Rollout envelope carrying session metadata, not conversation content. |
| `turn_context` | ignored-control | no | none | not-applicable | Rollout envelope carrying turn metadata, not conversation content. |
| `event_msg` | represented | no | none | not-applicable |  |
| `response_item` | represented | no | none | not-applicable |  |
| `token_count` | ignored-control | no | none | not-applicable | Control event carrying token counts, not conversation content. |
| `task_started` | ignored-control | no | none | not-applicable | Control event marking task start, not conversation content. |
| `task_complete` | ignored-control | no | none | not-applicable | Control event marking task completion, not conversation content. |
| `turn_aborted` | ignored-control | no | none | not-applicable | Control event marking an aborted turn, not conversation content. |
| `user_message` | ignored-control | no | none | not-applicable | Mirror of the conversation already represented by a response item; the mirror must match or the capture fails. |
| `agent_message` | ignored-control | no | none | not-applicable | Mirror of the conversation already represented by a response item; the mirror must match or the capture fails. |
| `agent_reasoning` | ignored-control | no | none | not-applicable | Mirror of the reasoning already represented by a response item; the mirror must match or the capture fails. |
| `message` | represented | yes | none | rendered | TranscriptViewer turn (fairtrade adaptTranscript) |
| `reasoning` | represented | yes | none | rendered | ThinkingVM (fairtrade adaptTranscript) |
| `function_call` | represented | yes | none | rendered | TranscriptViewer tool turn (fairtrade adaptTranscript) |
| `custom_tool_call` | represented | yes | none | rendered | TranscriptViewer tool turn (fairtrade adaptTranscript) |
| `function_call_output` | represented | yes | none | rendered | TranscriptViewer tool turn (fairtrade adaptTranscript) |
| `custom_tool_call_output` | represented | yes | none | rendered | TranscriptViewer tool turn (fairtrade adaptTranscript) |
| `input_text` | represented | yes | none | rendered | TranscriptViewer turn content (fairtrade adaptTranscript) |
| `output_text` | represented | yes | none | rendered | TranscriptViewer turn content (fairtrade adaptTranscript) |
| `summary_text` | represented | yes | none | rendered | ThinkingVM (fairtrade adaptTranscript) |
| `reasoning_text` | represented | yes | none | rendered | ThinkingVM (fairtrade adaptTranscript) |
| `text` | represented | yes | none | rendered | ThinkingVM (fairtrade adaptTranscript) |
| `response_item-unrepresentable` | refused | no | none | not-applicable | Response payload shapes with no representation (agent_message, web_search_call, tool_search_call, tool_search_output); the refusal names the response_item kind. |
| `input_image` | refused | no | none | not-applicable | Message block with no representation; refusal keeps the capture incomplete until a build represents it. |
| `token_usage_record` | refused | no | none | not-applicable | Rollout envelope with no representation; refusal keeps the capture incomplete until a build represents it. |
| `world_state` | refused | no | none | not-applicable | Rollout envelope with no representation; refusal keeps the capture incomplete until a build represents it. |
| `inter_agent_communication_metadata` | refused | no | none | not-applicable | Rollout envelope with no representation; refusal keeps the capture incomplete until a build represents it. |
| `compacted` | refused | no | none | not-applicable | Rollout envelope with no representation; refusal keeps the capture incomplete until a build represents it. |
| `patch_apply_end` | refused | no | none | not-applicable | Event kind with no representation; refusal keeps the capture incomplete until a build represents it. |
| `sub_agent_activity` | refused | no | none | not-applicable | Event kind with no representation; refusal keeps the capture incomplete until a build represents it. |
| `item_completed` | refused | no | none | not-applicable | Event kind with no representation; refusal keeps the capture incomplete until a build represents it. |
| `thread_settings_applied` | refused | no | none | not-applicable | Event kind with no representation; refusal keeps the capture incomplete until a build represents it. |
| `web_search_end` | refused | no | none | not-applicable | Event kind with no representation; refusal keeps the capture incomplete until a build represents it. |
| `context_compacted` | refused | no | none | not-applicable | Event kind with no representation; refusal keeps the capture incomplete until a build represents it. |
| `mcp_tool_call_end` | refused | no | none | not-applicable | Event kind with no representation; refusal keeps the capture incomplete until a build represents it. |
| `thread_rolled_back` | refused | no | none | not-applicable | Event kind with no representation; refusal keeps the capture incomplete until a build represents it. |
| `thread_goal_updated` | refused | no | none | not-applicable | Event kind with no representation; refusal keeps the capture incomplete until a build represents it. |
| `entered_review_mode` | refused | no | none | not-applicable | Event kind with no representation; refusal keeps the capture incomplete until a build represents it. |
| `exited_review_mode` | refused | no | none | not-applicable | Event kind with no representation; refusal keeps the capture incomplete until a build represents it. |

## cursor (adapter 1, indexer 16)

| Kind | Status | Preview | Payload | Visualized | Detail |
|---|---|---|---|---|---|
| `user` | represented | yes | none | rendered | TranscriptViewer user turn (fairtrade adaptTranscript) |
| `assistant` | represented | yes | none | rendered | TranscriptViewer assistant turn (fairtrade adaptTranscript) |
| `human` | represented | yes | none | rendered | TranscriptViewer user turn (fairtrade adaptTranscript) |
| `system` | represented | yes | none | rendered | TranscriptViewer system turn (fairtrade adaptTranscript) |
| `tool` | represented | yes | none | rendered | TranscriptViewer tool turn (fairtrade adaptTranscript) |
| `turn_ended` | represented | no | none | not-applicable |  |
| `text` | represented | yes | none | rendered | TranscriptViewer turn content (fairtrade adaptTranscript) |
| `thinking` | represented | yes | none | rendered | ThinkingVM (fairtrade adaptTranscript) |
| `tool_use` | represented | yes | none | rendered | TranscriptViewer tool turn (fairtrade adaptTranscript) |
| `tool_result` | represented | yes | none | rendered | TranscriptViewer tool turn (fairtrade adaptTranscript) |

## opencode (adapter 1, indexer 16)

| Kind | Status | Preview | Payload | Visualized | Detail |
|---|---|---|---|---|---|
| `step-start` | ignored-control | no | none | not-applicable | Control part marking step start; a part carrying content still fails. |
| `step-finish` | ignored-control | no | none | not-applicable | Control part marking step finish; a part carrying content still fails. |
| `snapshot` | ignored-control | no | none | not-applicable | Control part carrying snapshot state; a part carrying content still fails. |
| `patch` | ignored-control | no | none | not-applicable | Control part carrying patch state; a part carrying content still fails. |
| `text` | represented | yes | none | rendered | TranscriptViewer turn content (fairtrade adaptTranscript) |
| `reasoning` | represented | yes | none | rendered | ThinkingVM (fairtrade adaptTranscript) |
| `tool` | represented | yes | none | rendered | TranscriptViewer tool turn (fairtrade adaptTranscript) |
| `tool_use` | represented | yes | none | rendered | TranscriptViewer tool turn (fairtrade adaptTranscript) |
| `tool_result` | represented | yes | none | rendered | TranscriptViewer tool turn (fairtrade adaptTranscript) |
| `compaction` | represented | yes | none | planned |  |
| `subtask` | represented | yes | none | planned |  |
| `agent` | represented | yes | none | planned |  |
| `file` | refused | no | none | not-applicable | Part type with no representation in the strict path; the legacy SQLite path tolerates it with an OpenCodeUnknownPartType diagnostic instead of refusing. |

## strike (adapter 1, indexer 16)

| Kind | Status | Preview | Payload | Visualized | Detail |
|---|---|---|---|---|---|
| `session.started` | ignored-control | no | none | not-applicable | Session metadata event, not conversation content. |
| `session.titled` | ignored-control | no | none | not-applicable | Session metadata event, not conversation content. |
| `model.selected` | ignored-control | no | none | not-applicable | Session metadata event, not conversation content. |
| `user.message` | represented | yes | none | rendered | TranscriptViewer user turn (fairtrade adaptTranscript) |
| `turn.started` | represented | no | none | not-applicable |  |
| `turn.completed` | represented | no | none | not-applicable |  |
| `assistant.text` | represented | yes | none | rendered | TranscriptViewer assistant turn (fairtrade adaptTranscript) |
| `assistant.text.delta` | represented | yes | none | rendered | TranscriptViewer assistant turn (fairtrade adaptTranscript) |
| `assistant.message.delta` | represented | yes | none | rendered | TranscriptViewer assistant turn (fairtrade adaptTranscript) |
| `text.delta` | represented | yes | none | rendered | TranscriptViewer assistant turn (fairtrade adaptTranscript) |
| `assistant.reasoning` | represented | yes | none | rendered | ThinkingVM (fairtrade adaptTranscript) |
| `assistant.reasoning.delta` | represented | yes | none | rendered | ThinkingVM (fairtrade adaptTranscript) |
| `reasoning.delta` | represented | yes | none | rendered | ThinkingVM (fairtrade adaptTranscript) |
| `assistant.thinking.delta` | represented | yes | none | rendered | ThinkingVM (fairtrade adaptTranscript) |
| `tool.begin` | represented | yes | none | rendered | TranscriptViewer tool turn (fairtrade adaptTranscript) |
| `tool.output` | represented | yes | none | rendered | TranscriptViewer tool turn (fairtrade adaptTranscript) |
| `tool.end` | represented | yes | none | rendered | TranscriptViewer tool turn (fairtrade adaptTranscript) |
| `process.started` | represented | yes | none | rendered | TranscriptViewer tool turn (fairtrade adaptTranscript) |
| `process.output` | represented | yes | none | rendered | TranscriptViewer tool turn (fairtrade adaptTranscript) |
| `process.exited` | represented | yes | none | rendered | TranscriptViewer tool turn (fairtrade adaptTranscript) |
| `usage.reported` | represented | no | none | not-applicable |  |
| `agent.selected` | refused | no | none | not-applicable | Event type with no representation; refusal keeps the capture incomplete until a build represents it. |
| `permission.mode` | refused | no | none | not-applicable | Event type with no representation; refusal keeps the capture incomplete until a build represents it. |
| `effort.selected` | refused | no | none | not-applicable | Event type with no representation; refusal keeps the capture incomplete until a build represents it. |
| `autonomy.selected` | refused | no | none | not-applicable | Event type with no representation; refusal keeps the capture incomplete until a build represents it. |
| `phase.changed` | refused | no | none | not-applicable | Event type with no representation; refusal keeps the capture incomplete until a build represents it. |
| `child.started` | refused | no | none | not-applicable | Event type with no representation; refusal keeps the capture incomplete until a build represents it. |
| `child.completed` | refused | no | none | not-applicable | Event type with no representation; refusal keeps the capture incomplete until a build represents it. |
| `permission.asked` | refused | no | none | not-applicable | Event type with no representation; refusal keeps the capture incomplete until a build represents it. |
| `permission.resolved` | refused | no | none | not-applicable | Event type with no representation; refusal keeps the capture incomplete until a build represents it. |
| `session.meta` | refused | no | none | not-applicable | Event type with no representation; refusal keeps the capture incomplete until a build represents it. |
| `team.roster` | refused | no | none | not-applicable | Event type with no representation; refusal keeps the capture incomplete until a build represents it. |
| `engine.error` | refused | no | none | not-applicable | Event type with no representation; refusal keeps the capture incomplete until a build represents it. |
| `session.header` | refused | no | none | not-applicable | Event type with no representation; refusal keeps the capture incomplete until a build represents it. |
| `question.asked` | refused | no | none | not-applicable | Event type with no representation; refusal keeps the capture incomplete until a build represents it. |
| `question.resolved` | refused | no | none | not-applicable | Event type with no representation; refusal keeps the capture incomplete until a build represents it. |
| `files.invalidated` | refused | no | none | not-applicable | Event type with no representation; refusal keeps the capture incomplete until a build represents it. |

<!-- END GENERATED RECORD KINDS -->
