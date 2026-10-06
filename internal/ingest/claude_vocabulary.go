package ingest

import "github.com/peasant-labs/peasant/internal/indexformat"

var claudeVocabulary = recordKindAdapterVocabulary{
	Harness: HarnessClaudeCode,
	Inventories: []recordKindInventoryDeclaration{
		{
			Context:   RecordKindRetained,
			Namespace: "record",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "claudeStrictRecordKinds"),
				recordKindSwitchSource("content_capture.go", "ClaudeIndexer.IndexTranscriptBytesForCapture", "line.Type"),
				recordKindSource("claude_control_records.go", "claudeControlRecordTypes"),
				recordKindPrefixSource("claude_control_records.go", "isClaudeControlRecordType", "recordType"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("user", indexformat.OutcomeText),
				recordKindLiteral("assistant", indexformat.OutcomeText),
				recordKindLiteral("human", indexformat.OutcomeText),
				recordKindLiteral("system", indexformat.OutcomeText),
				recordKindLiteral("summary", indexformat.OutcomeText),
				recordKindLiteral("result", indexformat.OutcomeText),
				recordKindLiteral("progress", indexformat.OutcomeIgnored),
				recordKindLiteral("queue-operation", indexformat.OutcomeIgnored),
				recordKindLiteral("file-history-snapshot", indexformat.OutcomeIgnored),
				recordKindLiteral("last-prompt", indexformat.OutcomeIgnored),
				recordKindLiteral("permission-mode", indexformat.OutcomeControl),
				recordKindLiteral("mode", indexformat.OutcomeControl),
				recordKindLiteral("agent-setting", indexformat.OutcomeControl),
				recordKindLiteral("agent-name", indexformat.OutcomeControl),
				recordKindLiteral("ai-title", indexformat.OutcomeControl),
				recordKindLiteral("atis-latch", indexformat.OutcomeControl),
				recordKindLiteral("attachment", indexformat.OutcomeControl),
				recordKindLiteral("bridge-session", indexformat.OutcomeControl),
				recordKindLiteral("started", indexformat.OutcomeControl),
				recordKindLiteral("cost-state", indexformat.OutcomeControl),
				recordKindLiteral("custom-title", indexformat.OutcomeControl),
				recordKindLiteral("file-history-delta", indexformat.OutcomeControl),
				recordKindLiteral("frame-link", indexformat.OutcomeControl),
				recordKindLiteral("fork-context-ref", indexformat.OutcomeControl),
				recordKindLiteral("worktree-state", indexformat.OutcomeControl),
				recordKindLiteral("launched", indexformat.OutcomeControl),
				recordKindLiteral("pr-link", indexformat.OutcomeControl),
				recordKindPrefix("artifact-", indexformat.OutcomeControl),
			},
			Production: func() recordKindProductionSet {
				return addRecordKindPrefixes(
					mergeRecordKindProduction(
						recordKindProductionLiterals(claudeStrictRecordKinds()),
						recordKindProductionLiterals(recordKindProductionMapKeys(claudeControlRecordTypes)),
					),
					claudeArtifactControlPrefix,
				)
			},
		},
		{
			Context:   RecordKindRetained,
			Namespace: "system_subtype",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "claudeStrictSystemSubtypes"),
				recordKindSwitchSource("content_capture.go", "ClaudeIndexer.IndexTranscriptBytesForCapture", "line.Subtype"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("turn_duration", indexformat.OutcomeIgnored),
				recordKindLiteral("compact_boundary", indexformat.OutcomeControl),
				recordKindLiteral("stop_hook_summary", indexformat.OutcomeIgnored),
				recordKindLiteral("api_error", indexformat.OutcomeText),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(claudeStrictSystemSubtypes())
			},
		},
		{
			Context:   RecordKindRetained,
			Namespace: "content_block",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "captureContentBlockKinds"),
				recordKindSwitchSource("content_capture.go", "validateCaptureContent", "block.Type"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("text", indexformat.OutcomeText),
				recordKindLiteral("thinking", indexformat.OutcomeText),
				recordKindLiteral("tool_use", indexformat.OutcomeToolCall),
				recordKindLiteral("tool_result", indexformat.OutcomeToolResult),
				recordKindLiteral("tool_reference", indexformat.OutcomeIgnored),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(captureContentBlockKinds(HarnessClaudeCode))
			},
		},
	},
}
