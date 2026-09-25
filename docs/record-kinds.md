# Record-kind registry

Generated from the co-located adapter vocabulary declarations. Do not hand-edit this document.
Regenerate the registry and document with: go generate ./internal/ingest/

## Reading the registry

The registry describes parser behavior; it is not a wire schema or a native-input
allowlist. A previously unseen valid kind needs no named row or release to use
the retained-unknown fallback. Adding a specialized handler does require a row.
Invalid JSON, invalid known fields, corrupt managed data and failed retention
remain errors, not successful unknown-kind captures.

- **Context** separates retained format-1 indexing from native-generation parsing.
  Pi uses format 1 even though its parser follows a native active graph.
- **Namespace** separates discriminator domains. Equal record and block names
  are not the same key. **Match** is literal unless explicitly marked prefix.
- **Status** is represented (interpreted entries or owning-entry state),
  tracked-only (stored non-conversation evidence), ignored-control (no row),
  retained-unknown (complete redacted evidence without interpretation), or refused
  (a known unsupported shape; never the default for arbitrary valid new names).
- **Preview** means the mapping can populate a human-readable content preview,
  not that every instance has nonempty text. Tool arguments alone are not preview
  text.
- **Payload** describes local retained shape, not a new public schema. Generic
  unknown payloads are complete JSON with source coordinates; known control limits
  do not license truncating unknown evidence.
- **Rendering is a consumer concern and is intentionally outside this registry.**
  The registry does not classify, track or report whether a kind is rendered.
- **Source** names first-party production code as generated reporting metadata.
  Each adapter vocabulary is compared exactly with its runtime dispatch/census;
  a source pointer is never parsed as Go syntax. Census observations are not an
  accept-list.
- **Versions** are verification targets: baseline and native overrides match
  their actual producer registries. They are not session producer stamps.

## Reporting and verification

Retained-unknown run rows count **occurrences** separately from affected
**sessions** for each harness/namespace/kind after successful writes. Multiple
unknown blocks in one session are multiple occurrences and one affected session.
The legacy refusal API counts per-session refusal inputs; it does not enumerate
all unknown occurrences and must not be used for retained-unknown accounting.

Vocabulary completeness compares declarations and production censuses in both
directions without assuming a Go syntax shape. Behavioral fixtures separately
check classification and previews. These checks are not proof of all source,
storage, export and receiver paths: those require the harness and publication
integration suites. Publication requires validated retained evidence, partial
signaling and the receiver's preservation capability; failed or oversized
transfer must not silently discard evidence. Schema release/tag precedes pins.

## claude-code (adapter 1, indexer 18)

Baseline index format: 1.

Unseen valid kinds: **retained-unknown**, preview **no**. Retain uninterpreted evidence and mark partial interpretation. Payload: complete raw JSON and source coordinates in retainedUnknown. Source: `internal/ingest/retained_unknown.go NewRetainedUnknown`.

| Context | Namespace | Kind | Match | Status | Preview | Payload | Detail | Source |
|---|---|---|---|---|---|---|---|---|
| retained-format-1 | record | `user` | literal | represented | yes | session entries |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `assistant` | literal | represented | yes | session entries |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `human` | literal | represented | yes | session entries |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `system` | literal | represented | yes | session entries |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `summary` | literal | represented | yes | session entries |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `result` | literal | represented | yes | session entries |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `progress` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `queue-operation` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `file-history-snapshot` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `last-prompt` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `permission-mode` | literal | represented | yes | bounded control extra (oversized known controls retain identity only) |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `mode` | literal | represented | yes | bounded control extra (oversized known controls retain identity only) |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `agent-setting` | literal | represented | yes | bounded control extra (oversized known controls retain identity only) |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `agent-name` | literal | represented | yes | bounded control extra (oversized known controls retain identity only) |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `ai-title` | literal | represented | yes | bounded control extra (oversized known controls retain identity only) |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `atis-latch` | literal | represented | yes | bounded control extra (oversized known controls retain identity only) |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `attachment` | literal | tracked-only | no | bounded control extra |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `bridge-session` | literal | represented | yes | bounded control extra (oversized known controls retain identity only) |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `started` | literal | represented | yes | bounded control extra (oversized known controls retain identity only) |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `cost-state` | literal | represented | yes | bounded control extra (oversized known controls retain identity only) |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `custom-title` | literal | represented | yes | bounded control extra (oversized known controls retain identity only) |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `file-history-delta` | literal | represented | yes | bounded control extra (oversized known controls retain identity only) |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `frame-link` | literal | represented | yes | bounded control extra (oversized known controls retain identity only) |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `fork-context-ref` | literal | represented | yes | bounded control extra (oversized known controls retain identity only) |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `worktree-state` | literal | represented | yes | bounded control extra (oversized known controls retain identity only) |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `launched` | literal | represented | yes | bounded control extra (oversized known controls retain identity only) |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `pr-link` | literal | represented | yes | bounded control extra (oversized known controls retain identity only) |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | record | `artifact-` | prefix | represented | yes | bounded control extra (oversized known controls retain identity only) |  | `content_capture.go claudeStrictRecordKinds; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture; claude_control_records.go claudeControlRecordTypes; claude_control_records.go isClaudeControlRecordType` |
| retained-format-1 | system_subtype | `turn_duration` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `content_capture.go claudeStrictSystemSubtypes; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | system_subtype | `compact_boundary` | literal | represented | yes | compactMetadata |  | `content_capture.go claudeStrictSystemSubtypes; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | system_subtype | `stop_hook_summary` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `content_capture.go claudeStrictSystemSubtypes; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | system_subtype | `api_error` | literal | represented | yes | session entries |  | `content_capture.go claudeStrictSystemSubtypes; content_capture.go ClaudeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | content_block | `text` | literal | represented | yes | session entries |  | `content_capture.go captureContentBlockKinds; content_capture.go validateCaptureContent` |
| retained-format-1 | content_block | `thinking` | literal | represented | yes | session entries |  | `content_capture.go captureContentBlockKinds; content_capture.go validateCaptureContent` |
| retained-format-1 | content_block | `tool_use` | literal | represented | no | tool name and arguments |  | `content_capture.go captureContentBlockKinds; content_capture.go validateCaptureContent` |
| retained-format-1 | content_block | `tool_result` | literal | represented | yes | tool output |  | `content_capture.go captureContentBlockKinds; content_capture.go validateCaptureContent` |
| retained-format-1 | content_block | `tool_reference` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `content_capture.go captureContentBlockKinds; content_capture.go validateCaptureContent` |

## codex (adapter 1, indexer 17)

Baseline index format: 1.

Native generation: adapter 2, indexer 18, index format 2.

Unseen valid kinds: **retained-unknown**, preview **no**. Retain uninterpreted evidence and mark partial interpretation. Payload: complete raw JSON and source coordinates in retainedUnknown. Source: `internal/ingest/retained_unknown.go NewRetainedUnknown`.

| Context | Namespace | Kind | Match | Status | Preview | Payload | Detail | Source |
|---|---|---|---|---|---|---|---|---|
| retained-format-1 | envelope | `session_meta` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `content_capture.go codexStrictEnvelopeKinds; content_capture.go CodexIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | envelope | `turn_context` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `content_capture.go codexStrictEnvelopeKinds; content_capture.go CodexIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | envelope | `event_msg` | literal | represented | no | state on owning entry; no independent row |  | `content_capture.go codexStrictEnvelopeKinds; content_capture.go CodexIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | envelope | `response_item` | literal | represented | no | state on owning entry; no independent row |  | `content_capture.go codexStrictEnvelopeKinds; content_capture.go CodexIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | event | `token_count` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `content_capture.go codexStrictEventMsgKinds; content_capture.go CodexIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | event | `task_started` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `content_capture.go codexStrictEventMsgKinds; content_capture.go CodexIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | event | `task_complete` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `content_capture.go codexStrictEventMsgKinds; content_capture.go CodexIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | event | `turn_aborted` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `content_capture.go codexStrictEventMsgKinds; content_capture.go CodexIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | event | `user_message` | literal | ignored-control | no | none | Mirrored content is represented by response items. | `content_capture.go codexStrictEventMsgKinds; content_capture.go CodexIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | event | `agent_message` | literal | ignored-control | no | none | Mirrored content is represented by response items. | `content_capture.go codexStrictEventMsgKinds; content_capture.go CodexIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | event | `agent_reasoning` | literal | ignored-control | no | none | Mirrored content is represented by response items. | `content_capture.go codexStrictEventMsgKinds; content_capture.go CodexIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | response_item | `message` | literal | represented | yes | session entries |  | `content_capture.go codexStrictResponsePayloadKinds; content_capture.go CodexIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | response_item | `reasoning` | literal | represented | yes | session entries |  | `content_capture.go codexStrictResponsePayloadKinds; content_capture.go CodexIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | response_item | `function_call` | literal | represented | no | tool name and arguments |  | `content_capture.go codexStrictResponsePayloadKinds; content_capture.go CodexIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | response_item | `custom_tool_call` | literal | represented | no | tool name and arguments |  | `content_capture.go codexStrictResponsePayloadKinds; content_capture.go CodexIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | response_item | `function_call_output` | literal | represented | yes | tool output |  | `content_capture.go codexStrictResponsePayloadKinds; content_capture.go CodexIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | response_item | `custom_tool_call_output` | literal | represented | yes | tool output |  | `content_capture.go codexStrictResponsePayloadKinds; content_capture.go CodexIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | message_block | `input_text` | literal | represented | yes | session entries |  | `content_capture.go codexStrictMessageBlockKinds` |
| retained-format-1 | message_block | `output_text` | literal | represented | yes | session entries |  | `content_capture.go codexStrictMessageBlockKinds` |
| retained-format-1 | reasoning_summary | `summary_text` | literal | represented | yes | session entries |  | `content_capture.go codexStrictReasoningSummaryKinds` |
| retained-format-1 | reasoning_content | `reasoning_text` | literal | represented | yes | session entries |  | `content_capture.go codexStrictReasoningContentKinds` |
| retained-format-1 | reasoning_content | `text` | literal | represented | yes | session entries |  | `content_capture.go codexStrictReasoningContentKinds` |
| native-generation | envelope | `session_meta` | literal | represented | no | state on owning entry; no independent row |  | `codex_history_replay.go recognizedCodexEnvelopeType; codex_history_replay.go codexReplayState.replayRecord` |
| native-generation | envelope | `turn_context` | literal | represented | no | state on owning entry; no independent row |  | `codex_history_replay.go recognizedCodexEnvelopeType; codex_history_replay.go codexReplayState.replayRecord` |
| native-generation | envelope | `event_msg` | literal | represented | no | state on owning entry; no independent row |  | `codex_history_replay.go recognizedCodexEnvelopeType; codex_history_replay.go codexReplayState.replayRecord` |
| native-generation | envelope | `response_item` | literal | represented | no | state on owning entry; no independent row |  | `codex_history_replay.go recognizedCodexEnvelopeType; codex_history_replay.go codexReplayState.replayRecord` |
| native-generation | envelope | `compacted` | literal | represented | no | state on owning entry; no independent row |  | `codex_history_replay.go recognizedCodexEnvelopeType; codex_history_replay.go codexReplayState.replayRecord` |
| native-generation | event | `token_count` | literal | ignored-control | no | none | Metadata event; no conversation row. | `codex_history_replay.go codexReplayState.replayEventMessage; codex_unknown.go prepareCodexRecord; content_capture.go codexStrictEventMsgKinds` |
| native-generation | event | `user_message` | literal | ignored-control | no | none | Mirrored content is represented by response items. | `codex_history_replay.go codexReplayState.replayEventMessage; codex_unknown.go prepareCodexRecord; content_capture.go codexStrictEventMsgKinds` |
| native-generation | event | `agent_message` | literal | ignored-control | no | none | Mirrored content is represented by response items. | `codex_history_replay.go codexReplayState.replayEventMessage; codex_unknown.go prepareCodexRecord; content_capture.go codexStrictEventMsgKinds` |
| native-generation | event | `agent_reasoning` | literal | ignored-control | no | none | Mirrored content is represented by response items. | `codex_history_replay.go codexReplayState.replayEventMessage; codex_unknown.go prepareCodexRecord; content_capture.go codexStrictEventMsgKinds` |
| native-generation | event | `item_started` | literal | represented | no | state on owning entry; no independent row |  | `codex_history_replay.go codexReplayState.replayEventMessage; codex_unknown.go prepareCodexRecord; content_capture.go codexStrictEventMsgKinds` |
| native-generation | event | `item_completed` | literal | represented | no | state on owning entry; no independent row |  | `codex_history_replay.go codexReplayState.replayEventMessage; codex_unknown.go prepareCodexRecord; content_capture.go codexStrictEventMsgKinds` |
| native-generation | event | `turn_started` | literal | represented | no | state on owning entry; no independent row |  | `codex_history_replay.go codexReplayState.replayEventMessage; codex_unknown.go prepareCodexRecord; content_capture.go codexStrictEventMsgKinds` |
| native-generation | event | `task_started` | literal | represented | no | state on owning entry; no independent row |  | `codex_history_replay.go codexReplayState.replayEventMessage; codex_unknown.go prepareCodexRecord; content_capture.go codexStrictEventMsgKinds` |
| native-generation | event | `turn_complete` | literal | represented | no | state on owning entry; no independent row |  | `codex_history_replay.go codexReplayState.replayEventMessage; codex_unknown.go prepareCodexRecord; content_capture.go codexStrictEventMsgKinds` |
| native-generation | event | `task_complete` | literal | represented | no | state on owning entry; no independent row |  | `codex_history_replay.go codexReplayState.replayEventMessage; codex_unknown.go prepareCodexRecord; content_capture.go codexStrictEventMsgKinds` |
| native-generation | event | `thread_rolled_back` | literal | represented | no | state on owning entry; no independent row |  | `codex_history_replay.go codexReplayState.replayEventMessage; codex_unknown.go prepareCodexRecord; content_capture.go codexStrictEventMsgKinds` |
| native-generation | event | `turn_aborted` | literal | represented | no | state on owning entry; no independent row |  | `codex_history_replay.go codexReplayState.replayEventMessage; codex_unknown.go prepareCodexRecord; content_capture.go codexStrictEventMsgKinds` |
| native-generation | response_item | `message` | literal | represented | yes | session entries |  | `codex_history_replay.go codexResponseNativeType` |
| native-generation | response_item | `agent_message` | literal | represented | yes | session entries |  | `codex_history_replay.go codexResponseNativeType` |
| native-generation | response_item | `reasoning` | literal | represented | yes | session entries |  | `codex_history_replay.go codexResponseNativeType` |
| native-generation | response_item | `function_call` | literal | represented | no | tool name and arguments |  | `codex_history_replay.go codexResponseNativeType` |
| native-generation | response_item | `custom_tool_call` | literal | represented | no | tool name and arguments |  | `codex_history_replay.go codexResponseNativeType` |
| native-generation | response_item | `function_call_output` | literal | represented | yes | tool output |  | `codex_history_replay.go codexResponseNativeType` |
| native-generation | response_item | `custom_tool_call_output` | literal | represented | yes | tool output |  | `codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `message` | literal | represented | yes | session entries |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `agent_message` | literal | represented | yes | session entries |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `reasoning` | literal | represented | yes | session entries |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `function_call` | literal | represented | no | tool name and arguments |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `custom_tool_call` | literal | represented | no | tool name and arguments |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `function_call_output` | literal | represented | yes | tool output |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `custom_tool_call_output` | literal | represented | yes | tool output |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `UserMessage` | literal | represented | yes | session entries |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `AgentMessage` | literal | represented | yes | session entries |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `Reasoning` | literal | represented | yes | session entries |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `FunctionCallOutput` | literal | represented | yes | tool output |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `CommandExecution` | literal | represented | yes | classified native item and provenance |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `FileChange` | literal | represented | yes | classified native item and provenance |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `SubAgentActivity` | literal | represented | yes | classified native item and provenance |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `CollabAgentToolCall` | literal | represented | yes | classified native item and provenance |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `ContextCompaction` | literal | represented | yes | classified native item and provenance |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `Extension` | literal | represented | yes | classified native item and provenance |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `Plan` | literal | represented | yes | classified native item and provenance |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `HookPrompt` | literal | represented | yes | classified native item and provenance |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `WebSearch` | literal | represented | yes | classified native item and provenance |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `ImageView` | literal | represented | yes | classified native item and provenance |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `ImageGeneration` | literal | represented | yes | classified native item and provenance |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `McpToolCall` | literal | represented | yes | classified native item and provenance |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `DynamicToolCall` | literal | represented | yes | classified native item and provenance |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `EnteredReviewMode` | literal | represented | yes | classified native item and provenance |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | item_body | `ExitedReviewMode` | literal | represented | yes | classified native item and provenance |  | `codex_history_replay.go codexItemNativeType; codex_history_replay.go codexResponseNativeType` |
| native-generation | message_block | `input_text` | literal | represented | yes | session entries |  | `codex_provenance.go codexMediaContentTypes; codex_provenance.go codexBlockModality; content_capture.go codexStrictMessageBlockKinds` |
| native-generation | message_block | `output_text` | literal | represented | yes | session entries |  | `codex_provenance.go codexMediaContentTypes; codex_provenance.go codexBlockModality; content_capture.go codexStrictMessageBlockKinds` |
| native-generation | message_block | `input_image` | literal | represented | yes | media modality and textual marker |  | `codex_provenance.go codexMediaContentTypes; codex_provenance.go codexBlockModality; content_capture.go codexStrictMessageBlockKinds` |
| native-generation | message_block | `image` | literal | represented | yes | media modality and textual marker |  | `codex_provenance.go codexMediaContentTypes; codex_provenance.go codexBlockModality; content_capture.go codexStrictMessageBlockKinds` |
| native-generation | message_block | `image_url` | literal | represented | yes | media modality and textual marker |  | `codex_provenance.go codexMediaContentTypes; codex_provenance.go codexBlockModality; content_capture.go codexStrictMessageBlockKinds` |
| native-generation | message_block | `input_audio` | literal | represented | yes | media modality and textual marker |  | `codex_provenance.go codexMediaContentTypes; codex_provenance.go codexBlockModality; content_capture.go codexStrictMessageBlockKinds` |
| native-generation | message_block | `audio` | literal | represented | yes | media modality and textual marker |  | `codex_provenance.go codexMediaContentTypes; codex_provenance.go codexBlockModality; content_capture.go codexStrictMessageBlockKinds` |
| native-generation | reasoning_summary | `summary_text` | literal | represented | yes | session entries |  | `content_capture.go codexStrictReasoningSummaryKinds` |
| native-generation | reasoning_content | `reasoning_text` | literal | represented | yes | session entries |  | `content_capture.go codexStrictReasoningContentKinds` |
| native-generation | reasoning_content | `text` | literal | represented | yes | session entries |  | `content_capture.go codexStrictReasoningContentKinds` |
| native-generation | content_kind | `user.text` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `user.image` | literal | represented | yes | media modality and textual marker |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `user.audio` | literal | represented | yes | media modality and textual marker |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `user.answered_question` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `multi_agent.inter_agent_message` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `multi_agent.inter_agent_completion_message` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `compaction.summary` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `shell.user_command` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `images.resize_notice` | literal | represented | yes | diagnostic text and provenance; no standalone submitted-input count without a media sibling |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `images.preparation_error` | literal | represented | yes | diagnostic text and provenance; no standalone submitted-input count without a media sibling |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `images.unsupported` | literal | represented | yes | diagnostic text and provenance; no standalone submitted-input count without a media sibling |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `audio.unsupported` | literal | represented | yes | diagnostic text and provenance; no standalone submitted-input count without a media sibling |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `agents_md.instructions` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `environments.environment_context` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `environments.instructions` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `model.base_instructions` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `generic.developer_instructions` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `managed_config.developer_instructions` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `permissions.instructions` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `persistent_mode.instructions` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `personality.spec_instructions` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `collaboration_mode.instructions` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `model_switch.instructions` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `hooks.additional_context` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `skills.catalog` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `skills.selected_skill_instructions` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `memories.instructions` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `notes.thread_hint` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `apps.instructions` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `plugins.instructions` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `plugins.usage_instructions` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `plugins.recommendations` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `tools.deferred_namespaces` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `multi_agent.subagent_notification` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `multi_agent.role_instructions` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `multi_agent.mode_instructions` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `multi_agent.usage_hint` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `guardian.policy` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `guardian.node_repl_policy` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `guardian.review_evidence` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `guardian.node_repl_review_evidence` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `guardian.trusted_tool` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `guardian.trusted_skills` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `guardian.approved_action` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `guardian.followup_review_reminder` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `generic.turn_aborted` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `current_time.reminder` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `token_budget.context_window` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `token_budget.context_window_guidance` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `token_budget.remaining_tokens` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `token_budget.reminder` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `rollout_budget.remaining_tokens` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `compaction.auto_fallback_prompt` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `permissions.approved_command_prefix_saved` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `network_proxy.rule_saved` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |
| native-generation | content_kind | `user_verification.notice` | literal | represented | yes | session entries |  | `codex_provenance.go codexContentKindRegistry` |

## cursor (adapter 1, indexer 17)

Baseline index format: 1.

Unseen valid kinds: **retained-unknown**, preview **no**. Retain uninterpreted evidence and mark partial interpretation. Payload: complete raw JSON and source coordinates in retainedUnknown. Source: `internal/ingest/retained_unknown.go NewRetainedUnknown`.

| Context | Namespace | Kind | Match | Status | Preview | Payload | Detail | Source |
|---|---|---|---|---|---|---|---|---|
| retained-format-1 | role | `user` | literal | represented | yes | session entries |  | `content_capture.go captureRoleKinds; unknown_jsonl.go cursorCaptureRoleKinds` |
| retained-format-1 | role | `assistant` | literal | represented | yes | session entries |  | `content_capture.go captureRoleKinds; unknown_jsonl.go cursorCaptureRoleKinds` |
| retained-format-1 | role | `system` | literal | represented | yes | session entries |  | `content_capture.go captureRoleKinds; unknown_jsonl.go cursorCaptureRoleKinds` |
| retained-format-1 | role | `tool` | literal | represented | yes | session entries |  | `content_capture.go captureRoleKinds; unknown_jsonl.go cursorCaptureRoleKinds` |
| retained-format-1 | role | `human` | literal | represented | yes | session entries |  | `content_capture.go captureRoleKinds; unknown_jsonl.go cursorCaptureRoleKinds` |
| retained-format-1 | record | `turn_ended` | literal | represented | yes | session entries |  | `content_capture.go cursorStrictRecordKinds` |
| retained-format-1 | content_block | `text` | literal | represented | yes | session entries |  | `content_capture.go captureContentBlockKinds` |
| retained-format-1 | content_block | `thinking` | literal | represented | yes | session entries |  | `content_capture.go captureContentBlockKinds` |
| retained-format-1 | content_block | `tool_use` | literal | represented | no | tool name and arguments |  | `content_capture.go captureContentBlockKinds` |
| retained-format-1 | content_block | `tool_result` | literal | represented | yes | tool output |  | `content_capture.go captureContentBlockKinds` |

## opencode (adapter 2, indexer 17)

Baseline index format: 1.

Native generation: adapter 3, indexer 18, index format 2.

Unseen valid kinds: **retained-unknown**, preview **no**. Retain uninterpreted evidence and mark partial interpretation. Payload: complete raw JSON and source coordinates in retainedUnknown. Source: `internal/ingest/retained_unknown.go NewRetainedUnknown`.

| Context | Namespace | Kind | Match | Status | Preview | Payload | Detail | Source |
|---|---|---|---|---|---|---|---|---|
| retained-format-1 | part | `step-start` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `opencode_capture.go openCodeCaptureControlKinds; opencode_indexer.go knownOpenCodeSemanticPartKinds` |
| retained-format-1 | part | `step-finish` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `opencode_capture.go openCodeCaptureControlKinds; opencode_indexer.go knownOpenCodeSemanticPartKinds` |
| retained-format-1 | part | `snapshot` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `opencode_capture.go openCodeCaptureControlKinds; opencode_indexer.go knownOpenCodeSemanticPartKinds` |
| retained-format-1 | part | `patch` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `opencode_capture.go openCodeCaptureControlKinds; opencode_indexer.go knownOpenCodeSemanticPartKinds` |
| retained-format-1 | part | `text` | literal | represented | yes | session entries |  | `opencode_capture.go openCodeCaptureControlKinds; opencode_indexer.go knownOpenCodeSemanticPartKinds` |
| retained-format-1 | part | `reasoning` | literal | represented | yes | session entries |  | `opencode_capture.go openCodeCaptureControlKinds; opencode_indexer.go knownOpenCodeSemanticPartKinds` |
| retained-format-1 | part | `tool` | literal | represented | no | tool name and arguments |  | `opencode_capture.go openCodeCaptureControlKinds; opencode_indexer.go knownOpenCodeSemanticPartKinds` |
| retained-format-1 | part | `tool_use` | literal | represented | no | tool name and arguments |  | `opencode_capture.go openCodeCaptureControlKinds; opencode_indexer.go knownOpenCodeSemanticPartKinds` |
| retained-format-1 | part | `tool_result` | literal | represented | yes | tool output |  | `opencode_capture.go openCodeCaptureControlKinds; opencode_indexer.go knownOpenCodeSemanticPartKinds` |
| retained-format-1 | part | `compaction` | literal | represented | yes | session entries |  | `opencode_capture.go openCodeCaptureControlKinds; opencode_indexer.go knownOpenCodeSemanticPartKinds` |
| retained-format-1 | part | `subtask` | literal | represented | yes | session entries |  | `opencode_capture.go openCodeCaptureControlKinds; opencode_indexer.go knownOpenCodeSemanticPartKinds` |
| retained-format-1 | part | `agent` | literal | represented | yes | session entries |  | `opencode_capture.go openCodeCaptureControlKinds; opencode_indexer.go knownOpenCodeSemanticPartKinds` |
| retained-format-1 | row | `user` | literal | represented | yes | session entries |  | `opencode_current_projection.go normalizeOpenCodeCurrentRowAt; opencode_unknown.go knownOpenCodeCurrentRow` |
| retained-format-1 | row | `assistant` | literal | represented | yes | session entries |  | `opencode_current_projection.go normalizeOpenCodeCurrentRowAt; opencode_unknown.go knownOpenCodeCurrentRow` |
| retained-format-1 | row | `shell` | literal | represented | yes | session entries |  | `opencode_current_projection.go normalizeOpenCodeCurrentRowAt; opencode_unknown.go knownOpenCodeCurrentRow` |
| retained-format-1 | row | `synthetic` | literal | represented | yes | session entries |  | `opencode_current_projection.go normalizeOpenCodeCurrentRowAt; opencode_unknown.go knownOpenCodeCurrentRow` |
| retained-format-1 | row | `system` | literal | represented | yes | session entries |  | `opencode_current_projection.go normalizeOpenCodeCurrentRowAt; opencode_unknown.go knownOpenCodeCurrentRow` |
| retained-format-1 | row | `skill` | literal | represented | yes | session entries |  | `opencode_current_projection.go normalizeOpenCodeCurrentRowAt; opencode_unknown.go knownOpenCodeCurrentRow` |
| retained-format-1 | row | `compaction` | literal | represented | yes | session entries |  | `opencode_current_projection.go normalizeOpenCodeCurrentRowAt; opencode_unknown.go knownOpenCodeCurrentRow` |
| retained-format-1 | row | `agent-switched` | literal | represented | yes | session entries |  | `opencode_current_projection.go normalizeOpenCodeCurrentRowAt; opencode_unknown.go knownOpenCodeCurrentRow` |
| retained-format-1 | row | `model-switched` | literal | represented | yes | session entries |  | `opencode_current_projection.go normalizeOpenCodeCurrentRowAt; opencode_unknown.go knownOpenCodeCurrentRow` |
| retained-format-1 | assistant_content | `text` | literal | represented | yes | session entries |  | `opencode_current_projection.go appendOpenCodeCurrentAssistantContent; opencode_unknown.go prepareOpenCodeCurrent` |
| retained-format-1 | assistant_content | `reasoning` | literal | represented | yes | session entries |  | `opencode_current_projection.go appendOpenCodeCurrentAssistantContent; opencode_unknown.go prepareOpenCodeCurrent` |
| retained-format-1 | assistant_content | `tool` | literal | represented | no | tool name and arguments |  | `opencode_current_projection.go appendOpenCodeCurrentAssistantContent; opencode_unknown.go prepareOpenCodeCurrent` |
| native-generation | row | `user` | literal | represented | yes | session entries |  | `opencode_provenance_decode.go DecodeOpenCodeProvenanceRow; opencode_unknown.go knownOpenCodeCurrentRow` |
| native-generation | row | `assistant` | literal | represented | yes | session entries |  | `opencode_provenance_decode.go DecodeOpenCodeProvenanceRow; opencode_unknown.go knownOpenCodeCurrentRow` |
| native-generation | row | `shell` | literal | represented | yes | session entries |  | `opencode_provenance_decode.go DecodeOpenCodeProvenanceRow; opencode_unknown.go knownOpenCodeCurrentRow` |
| native-generation | row | `synthetic` | literal | represented | yes | session entries |  | `opencode_provenance_decode.go DecodeOpenCodeProvenanceRow; opencode_unknown.go knownOpenCodeCurrentRow` |
| native-generation | row | `system` | literal | represented | yes | session entries |  | `opencode_provenance_decode.go DecodeOpenCodeProvenanceRow; opencode_unknown.go knownOpenCodeCurrentRow` |
| native-generation | row | `skill` | literal | represented | yes | session entries |  | `opencode_provenance_decode.go DecodeOpenCodeProvenanceRow; opencode_unknown.go knownOpenCodeCurrentRow` |
| native-generation | row | `compaction` | literal | represented | yes | session entries |  | `opencode_provenance_decode.go DecodeOpenCodeProvenanceRow; opencode_unknown.go knownOpenCodeCurrentRow` |
| native-generation | row | `agent-switched` | literal | represented | yes | session entries |  | `opencode_provenance_decode.go DecodeOpenCodeProvenanceRow; opencode_unknown.go knownOpenCodeCurrentRow` |
| native-generation | row | `model-switched` | literal | represented | yes | session entries |  | `opencode_provenance_decode.go DecodeOpenCodeProvenanceRow; opencode_unknown.go knownOpenCodeCurrentRow` |
| native-generation | assistant_content | `text` | literal | represented | yes | session entries |  | `opencode_current_projection.go appendOpenCodeCurrentAssistantContent; opencode_unknown.go prepareOpenCodeCurrent` |
| native-generation | assistant_content | `reasoning` | literal | represented | yes | session entries |  | `opencode_current_projection.go appendOpenCodeCurrentAssistantContent; opencode_unknown.go prepareOpenCodeCurrent` |
| native-generation | assistant_content | `tool` | literal | represented | no | tool name and arguments |  | `opencode_current_projection.go appendOpenCodeCurrentAssistantContent; opencode_unknown.go prepareOpenCodeCurrent` |
| retained-format-1 | content_block | `text` | literal | represented | yes | session entries |  | `opencode_unknown.go retainOpenCodeSemantic` |
| retained-format-1 | tool_content | `text` | literal | represented | yes | tool output |  | `opencode_v2_projection.go decodeOpenCodeV2ToolState; opencode_unknown.go prepareOpenCodeToolContent` |
| retained-format-1 | tool_content | `file` | literal | represented | no | structured tool output URI/MIME |  | `opencode_v2_projection.go decodeOpenCodeV2ToolState; opencode_unknown.go prepareOpenCodeToolContent` |
| native-generation | tool_content | `text` | literal | represented | yes | tool output |  | `opencode_v2_projection.go decodeOpenCodeV2ToolState; opencode_unknown.go prepareOpenCodeToolContent` |
| native-generation | tool_content | `file` | literal | represented | no | structured tool output URI/MIME |  | `opencode_v2_projection.go decodeOpenCodeV2ToolState; opencode_unknown.go prepareOpenCodeToolContent` |

## pi (adapter 2, indexer 17)

Baseline index format: 1.

Unseen valid kinds: **retained-unknown**, preview **no**. Retain uninterpreted evidence and mark partial interpretation. Payload: complete raw JSON and source coordinates in retainedUnknown. Source: `internal/ingest/retained_unknown.go NewRetainedUnknown`.

| Context | Namespace | Kind | Match | Status | Preview | Payload | Detail | Source |
|---|---|---|---|---|---|---|---|---|
| retained-format-1 | entry | `session` | literal | ignored-control | no | none | Required v3 document header; no conversation row. | `pi_source.go piEntryType.UnmarshalJSON` |
| retained-format-1 | entry | `message` | literal | represented | yes | PiExtra state and usage; role/block dependent content |  | `pi_source.go piEntryType.UnmarshalJSON` |
| retained-format-1 | entry | `thinking_level_change` | literal | tracked-only | no | PiExtra state carrier |  | `pi_source.go piEntryType.UnmarshalJSON` |
| retained-format-1 | entry | `model_change` | literal | tracked-only | no | PiExtra state carrier |  | `pi_source.go piEntryType.UnmarshalJSON` |
| retained-format-1 | entry | `compaction` | literal | represented | yes | summary and PiExtra usage/native metadata |  | `pi_source.go piEntryType.UnmarshalJSON` |
| retained-format-1 | entry | `branch_summary` | literal | represented | yes | summary and PiExtra usage/native metadata |  | `pi_source.go piEntryType.UnmarshalJSON` |
| retained-format-1 | entry | `custom` | literal | tracked-only | no | PiExtra native metadata |  | `pi_source.go piEntryType.UnmarshalJSON` |
| retained-format-1 | entry | `custom_message` | literal | represented | yes | text/media and PiExtra native metadata |  | `pi_source.go piEntryType.UnmarshalJSON` |
| retained-format-1 | entry | `label` | literal | tracked-only | no | PiExtra state carrier |  | `pi_source.go piEntryType.UnmarshalJSON` |
| retained-format-1 | entry | `session_info` | literal | tracked-only | no | PiExtra state carrier |  | `pi_source.go piEntryType.UnmarshalJSON` |
| retained-format-1 | message_role | `user` | literal | represented | yes | session entries |  | `pi_indexer.go piMessageRole.UnmarshalJSON` |
| retained-format-1 | message_role | `assistant` | literal | represented | yes | session entries |  | `pi_indexer.go piMessageRole.UnmarshalJSON` |
| retained-format-1 | message_role | `toolResult` | literal | represented | yes | tool output |  | `pi_indexer.go piMessageRole.UnmarshalJSON` |
| retained-format-1 | message_role | `bashExecution` | literal | represented | yes | shell parent and paired execute tool entries |  | `pi_indexer.go piMessageRole.UnmarshalJSON` |
| retained-format-1 | content_block | `text` | literal | represented | yes | session entries |  | `pi_indexer.go piBlockType.UnmarshalJSON; pi_indexer.go piContent` |
| retained-format-1 | content_block | `thinking` | literal | represented | yes | session entries |  | `pi_indexer.go piBlockType.UnmarshalJSON; pi_indexer.go piContent` |
| retained-format-1 | content_block | `image` | literal | represented | yes | textual media marker |  | `pi_indexer.go piBlockType.UnmarshalJSON; pi_indexer.go piContent` |
| retained-format-1 | content_block | `toolCall` | literal | represented | no | tool name and arguments |  | `pi_indexer.go piBlockType.UnmarshalJSON; pi_indexer.go piContent` |

## strike (adapter 1, indexer 17)

Baseline index format: 1.

Unseen valid kinds: **retained-unknown**, preview **no**. Retain uninterpreted evidence and mark partial interpretation. Payload: complete raw JSON and source coordinates in retainedUnknown. Source: `internal/ingest/retained_unknown.go NewRetainedUnknown`.

| Context | Namespace | Kind | Match | Status | Preview | Payload | Detail | Source |
|---|---|---|---|---|---|---|---|---|
| retained-format-1 | record | `session.started` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `session.titled` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `model.selected` | literal | ignored-control | no | none | Entryless control; unexpected conversation content remains a validation error. | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `user.message` | literal | represented | yes | session entries |  | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `turn.started` | literal | represented | no | state on owning entry; no independent row |  | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `turn.completed` | literal | represented | no | state on owning entry; no independent row |  | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `assistant.text` | literal | represented | yes | session entries |  | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `assistant.text.delta` | literal | represented | yes | session entries |  | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `assistant.message.delta` | literal | represented | yes | session entries |  | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `text.delta` | literal | represented | yes | session entries |  | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `assistant.reasoning` | literal | represented | yes | session entries |  | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `assistant.reasoning.delta` | literal | represented | yes | session entries |  | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `reasoning.delta` | literal | represented | yes | session entries |  | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `assistant.thinking.delta` | literal | represented | yes | session entries |  | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `tool.begin` | literal | represented | no | tool name and arguments |  | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `tool.output` | literal | represented | yes | tool output |  | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `tool.end` | literal | represented | yes | tool output |  | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `process.started` | literal | represented | no | tool name and arguments |  | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `process.output` | literal | represented | yes | tool output |  | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `process.exited` | literal | represented | yes | tool output |  | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | record | `usage.reported` | literal | represented | no | state on owning entry; no independent row |  | `strike.go knownStrikeEventKinds; strike_indexer.go StrikeIndexer.parseWithCompletion; content_capture.go StrikeIndexer.IndexTranscriptBytesForCapture` |
| retained-format-1 | content_block | `text` | literal | represented | yes | session entries |  | `content_capture.go captureContentBlockKinds` |
| retained-format-1 | content_block | `thinking` | literal | represented | yes | session entries |  | `content_capture.go captureContentBlockKinds` |
| retained-format-1 | content_block | `tool_use` | literal | represented | no | tool name and arguments |  | `content_capture.go captureContentBlockKinds` |
| retained-format-1 | content_block | `tool_result` | literal | represented | yes | tool output |  | `content_capture.go captureContentBlockKinds` |
