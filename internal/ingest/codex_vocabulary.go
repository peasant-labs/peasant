package ingest

import "github.com/peasant-labs/peasant/internal/indexformat"

var codexVocabulary = recordKindAdapterVocabulary{
	Harness: HarnessCodex,
	Inventories: []recordKindInventoryDeclaration{
		{
			Context:   RecordKindRetained,
			Namespace: "envelope",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "codexStrictEnvelopeKinds"),
				recordKindSwitchSource("content_capture.go", "CodexIndexer.IndexTranscriptBytesForCapture", "env.Type"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("session_meta", indexformat.OutcomeIgnored),
				recordKindLiteral("turn_context", indexformat.OutcomeIgnored),
				recordKindLiteral("event_msg", indexformat.OutcomeIgnored),
				recordKindLiteral("response_item", indexformat.OutcomeIgnored),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(codexStrictEnvelopeKinds())
			},
		},
		{
			Context:   RecordKindRetained,
			Namespace: "event",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "codexStrictEventMsgKinds"),
				recordKindSwitchSource("content_capture.go", "CodexIndexer.IndexTranscriptBytesForCapture", "event.Type"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("token_count", indexformat.OutcomeIgnored),
				recordKindLiteral("task_started", indexformat.OutcomeIgnored),
				recordKindLiteral("task_complete", indexformat.OutcomeIgnored),
				recordKindLiteral("turn_aborted", indexformat.OutcomeIgnored),
				recordKindLiteral("user_message", indexformat.OutcomeIgnored),
				recordKindLiteral("agent_message", indexformat.OutcomeIgnored),
				recordKindLiteral("agent_reasoning", indexformat.OutcomeIgnored),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(codexStrictEventMsgKinds())
			},
		},
		{
			Context:   RecordKindRetained,
			Namespace: "response_item",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "codexStrictResponsePayloadKinds"),
				recordKindSwitchSource("content_capture.go", "CodexIndexer.IndexTranscriptBytesForCapture", "payload.Type"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("message", indexformat.OutcomeText),
				recordKindLiteral("reasoning", indexformat.OutcomeText),
				recordKindLiteral("function_call", indexformat.OutcomeToolCall),
				recordKindLiteral("custom_tool_call", indexformat.OutcomeToolCall),
				recordKindLiteral("function_call_output", indexformat.OutcomeToolResult),
				recordKindLiteral("custom_tool_call_output", indexformat.OutcomeToolResult),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(codexStrictResponsePayloadKinds())
			},
		},
		{
			Context:   RecordKindRetained,
			Namespace: "message_block",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "codexStrictMessageBlockKinds"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("input_text", indexformat.OutcomeText),
				recordKindLiteral("output_text", indexformat.OutcomeText),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(codexStrictMessageBlockKinds())
			},
		},
		{
			Context:   RecordKindRetained,
			Namespace: "reasoning_summary",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "codexStrictReasoningSummaryKinds"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("summary_text", indexformat.OutcomeText),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(codexStrictReasoningSummaryKinds())
			},
		},
		{
			Context:   RecordKindRetained,
			Namespace: "reasoning_content",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "codexStrictReasoningContentKinds"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("reasoning_text", indexformat.OutcomeText),
				recordKindLiteral("text", indexformat.OutcomeText),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(codexStrictReasoningContentKinds())
			},
		},
		{
			Context:   RecordKindNative,
			Namespace: "envelope",
			Sources: []RecordKindSource{
				recordKindSwitchSource("codex_history_replay.go", "recognizedCodexEnvelopeType", "envelopeType"),
				recordKindSwitchSource("codex_history_replay.go", "codexReplayState.replayRecord", "record.EnvelopeType"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("session_meta", indexformat.OutcomeIgnored),
				recordKindLiteral("turn_context", indexformat.OutcomeIgnored),
				recordKindLiteral("event_msg", indexformat.OutcomeIgnored),
				recordKindLiteral("response_item", indexformat.OutcomeIgnored),
				recordKindLiteral("compacted", indexformat.OutcomeIgnored),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(codexNativeEnvelopeKinds())
			},
		},
		{
			Context:   RecordKindNative,
			Namespace: "event",
			Sources: []RecordKindSource{
				recordKindCompleteSource("codex_history_replay.go", "codexReplayState.replayEventMessage", "payload.Type"),
				recordKindSwitchSource("codex_unknown.go", "prepareCodexRecord", "variant"),
				recordKindSource("content_capture.go", "codexStrictEventMsgKinds"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("token_count", indexformat.OutcomeIgnored),
				recordKindLiteral("user_message", indexformat.OutcomeIgnored),
				recordKindLiteral("agent_message", indexformat.OutcomeIgnored),
				recordKindLiteral("agent_reasoning", indexformat.OutcomeIgnored),
				recordKindLiteral("item_started", indexformat.OutcomeIgnored),
				recordKindLiteral("item_completed", indexformat.OutcomeIgnored),
				recordKindLiteral("turn_started", indexformat.OutcomeIgnored),
				recordKindLiteral("task_started", indexformat.OutcomeIgnored),
				recordKindLiteral("turn_complete", indexformat.OutcomeIgnored),
				recordKindLiteral("task_complete", indexformat.OutcomeIgnored),
				recordKindLiteral("thread_rolled_back", indexformat.OutcomeIgnored),
				recordKindLiteral("turn_aborted", indexformat.OutcomeIgnored),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(codexNativeEventMsgKinds())
			},
		},
		{
			Context:   RecordKindNative,
			Namespace: "response_item",
			Sources: []RecordKindSource{
				recordKindSwitchSource("codex_history_replay.go", "codexResponseNativeType", "payloadType"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("message", indexformat.OutcomeText),
				recordKindLiteral("agent_message", indexformat.OutcomeText),
				recordKindLiteral("reasoning", indexformat.OutcomeText),
				recordKindLiteral("function_call", indexformat.OutcomeToolCall),
				recordKindLiteral("custom_tool_call", indexformat.OutcomeToolCall),
				recordKindLiteral("function_call_output", indexformat.OutcomeToolResult),
				recordKindLiteral("custom_tool_call_output", indexformat.OutcomeToolResult),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(codexNativeResponsePayloadKinds())
			},
		},
		{
			Context:   RecordKindNative,
			Namespace: "item_body",
			Sources: []RecordKindSource{
				recordKindSwitchSource("codex_history_replay.go", "codexItemNativeType", "body.Type"),
				recordKindSwitchSource("codex_history_replay.go", "codexResponseNativeType", "payloadType"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("message", indexformat.OutcomeText),
				recordKindLiteral("agent_message", indexformat.OutcomeText),
				recordKindLiteral("reasoning", indexformat.OutcomeText),
				recordKindLiteral("function_call", indexformat.OutcomeToolCall),
				recordKindLiteral("custom_tool_call", indexformat.OutcomeToolCall),
				recordKindLiteral("function_call_output", indexformat.OutcomeToolResult),
				recordKindLiteral("custom_tool_call_output", indexformat.OutcomeToolResult),
				recordKindLiteral("UserMessage", indexformat.OutcomeText),
				recordKindLiteral("AgentMessage", indexformat.OutcomeText),
				recordKindLiteral("Reasoning", indexformat.OutcomeText),
				recordKindLiteral("FunctionCallOutput", indexformat.OutcomeToolResult),
				recordKindLiteral("CommandExecution", indexformat.OutcomeText),
				recordKindLiteral("FileChange", indexformat.OutcomeText),
				recordKindLiteral("SubAgentActivity", indexformat.OutcomeText),
				recordKindLiteral("CollabAgentToolCall", indexformat.OutcomeText),
				recordKindLiteral("ContextCompaction", indexformat.OutcomeText),
				recordKindLiteral("Extension", indexformat.OutcomeText),
				recordKindLiteral("Plan", indexformat.OutcomeText),
				recordKindLiteral("HookPrompt", indexformat.OutcomeText),
				recordKindLiteral("WebSearch", indexformat.OutcomeText),
				recordKindLiteral("ImageView", indexformat.OutcomeText),
				recordKindLiteral("ImageGeneration", indexformat.OutcomeText),
				recordKindLiteral("McpToolCall", indexformat.OutcomeText),
				recordKindLiteral("DynamicToolCall", indexformat.OutcomeText),
				recordKindLiteral("EnteredReviewMode", indexformat.OutcomeText),
				recordKindLiteral("ExitedReviewMode", indexformat.OutcomeText),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(codexNativeItemBodyKinds())
			},
		},
		{
			Context:   RecordKindNative,
			Namespace: "message_block",
			Sources: []RecordKindSource{
				recordKindSource("codex_provenance.go", "codexMediaContentTypes"),
				recordKindSwitchSource("codex_provenance.go", "codexBlockModality", "contentType"),
				recordKindSource("content_capture.go", "codexStrictMessageBlockKinds"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("input_text", indexformat.OutcomeText),
				recordKindLiteral("output_text", indexformat.OutcomeText),
				recordKindLiteral("input_image", indexformat.OutcomeText),
				recordKindLiteral("image", indexformat.OutcomeText),
				recordKindLiteral("image_url", indexformat.OutcomeText),
				recordKindLiteral("input_audio", indexformat.OutcomeText),
				recordKindLiteral("audio", indexformat.OutcomeText),
			},
			Production: func() recordKindProductionSet {
				return mergeRecordKindProduction(
					recordKindProductionLiterals(codexStrictMessageBlockKinds()),
					recordKindProductionLiterals(recordKindProductionMapKeys(codexMediaContentTypes())),
				)
			},
		},
		{
			Context:   RecordKindNative,
			Namespace: "reasoning_summary",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "codexStrictReasoningSummaryKinds"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("summary_text", indexformat.OutcomeText),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(codexStrictReasoningSummaryKinds())
			},
		},
		{
			Context:   RecordKindNative,
			Namespace: "reasoning_content",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "codexStrictReasoningContentKinds"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("reasoning_text", indexformat.OutcomeText),
				recordKindLiteral("text", indexformat.OutcomeText),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(codexStrictReasoningContentKinds())
			},
		},
		{
			Context:   RecordKindNative,
			Namespace: "content_kind",
			Sources: []RecordKindSource{
				recordKindSource("codex_provenance.go", "codexContentKindRegistry"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("user.text", indexformat.OutcomeText),
				recordKindLiteral("user.image", indexformat.OutcomeText),
				recordKindLiteral("user.audio", indexformat.OutcomeText),
				recordKindLiteral("user.answered_question", indexformat.OutcomeText),
				recordKindLiteral("multi_agent.inter_agent_message", indexformat.OutcomeText),
				recordKindLiteral("multi_agent.inter_agent_completion_message", indexformat.OutcomeText),
				recordKindLiteral("compaction.summary", indexformat.OutcomeText),
				recordKindLiteral("shell.user_command", indexformat.OutcomeText),
				recordKindLiteral("images.resize_notice", indexformat.OutcomeText),
				recordKindLiteral("images.preparation_error", indexformat.OutcomeText),
				recordKindLiteral("images.unsupported", indexformat.OutcomeText),
				recordKindLiteral("audio.unsupported", indexformat.OutcomeText),
				recordKindLiteral("agents_md.instructions", indexformat.OutcomeText),
				recordKindLiteral("environments.environment_context", indexformat.OutcomeText),
				recordKindLiteral("environments.instructions", indexformat.OutcomeText),
				recordKindLiteral("model.base_instructions", indexformat.OutcomeText),
				recordKindLiteral("generic.developer_instructions", indexformat.OutcomeText),
				recordKindLiteral("managed_config.developer_instructions", indexformat.OutcomeText),
				recordKindLiteral("permissions.instructions", indexformat.OutcomeText),
				recordKindLiteral("persistent_mode.instructions", indexformat.OutcomeText),
				recordKindLiteral("personality.spec_instructions", indexformat.OutcomeText),
				recordKindLiteral("collaboration_mode.instructions", indexformat.OutcomeText),
				recordKindLiteral("model_switch.instructions", indexformat.OutcomeText),
				recordKindLiteral("hooks.additional_context", indexformat.OutcomeText),
				recordKindLiteral("skills.catalog", indexformat.OutcomeText),
				recordKindLiteral("skills.selected_skill_instructions", indexformat.OutcomeText),
				recordKindLiteral("memories.instructions", indexformat.OutcomeText),
				recordKindLiteral("notes.thread_hint", indexformat.OutcomeText),
				recordKindLiteral("apps.instructions", indexformat.OutcomeText),
				recordKindLiteral("plugins.instructions", indexformat.OutcomeText),
				recordKindLiteral("plugins.usage_instructions", indexformat.OutcomeText),
				recordKindLiteral("plugins.recommendations", indexformat.OutcomeText),
				recordKindLiteral("tools.deferred_namespaces", indexformat.OutcomeText),
				recordKindLiteral("multi_agent.subagent_notification", indexformat.OutcomeText),
				recordKindLiteral("multi_agent.role_instructions", indexformat.OutcomeText),
				recordKindLiteral("multi_agent.mode_instructions", indexformat.OutcomeText),
				recordKindLiteral("multi_agent.usage_hint", indexformat.OutcomeText),
				recordKindLiteral("guardian.policy", indexformat.OutcomeText),
				recordKindLiteral("guardian.node_repl_policy", indexformat.OutcomeText),
				recordKindLiteral("guardian.review_evidence", indexformat.OutcomeText),
				recordKindLiteral("guardian.node_repl_review_evidence", indexformat.OutcomeText),
				recordKindLiteral("guardian.trusted_tool", indexformat.OutcomeText),
				recordKindLiteral("guardian.trusted_skills", indexformat.OutcomeText),
				recordKindLiteral("guardian.approved_action", indexformat.OutcomeText),
				recordKindLiteral("guardian.followup_review_reminder", indexformat.OutcomeText),
				recordKindLiteral("generic.turn_aborted", indexformat.OutcomeText),
				recordKindLiteral("current_time.reminder", indexformat.OutcomeText),
				recordKindLiteral("token_budget.context_window", indexformat.OutcomeText),
				recordKindLiteral("token_budget.context_window_guidance", indexformat.OutcomeText),
				recordKindLiteral("token_budget.remaining_tokens", indexformat.OutcomeText),
				recordKindLiteral("token_budget.reminder", indexformat.OutcomeText),
				recordKindLiteral("rollout_budget.remaining_tokens", indexformat.OutcomeText),
				recordKindLiteral("compaction.auto_fallback_prompt", indexformat.OutcomeText),
				recordKindLiteral("permissions.approved_command_prefix_saved", indexformat.OutcomeText),
				recordKindLiteral("network_proxy.rule_saved", indexformat.OutcomeText),
				recordKindLiteral("user_verification.notice", indexformat.OutcomeText),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(recordKindProductionMapKeys(codexContentKindRegistry()))
			},
		},
	},
}
