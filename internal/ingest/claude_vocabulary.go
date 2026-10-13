package ingest

import "github.com/peasant-labs/peasant/internal/indexformat"

// claudeVocabulary declares the record, system-subtype and content-block kinds
// Claude recognizes. The retained format-1 validator and the managed-generation
// classifier read the same dispatch, so both contexts declare the same kinds
// from the same production census; only the source selectors differ.
var claudeVocabulary = recordKindAdapterVocabulary{
	Harness: HarnessClaudeCode,
	Inventories: []recordKindInventoryDeclaration{
		{
			Context:   RecordKindRetained,
			Namespace: "record",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "claudeStrictRecordKinds"),
				recordKindSwitchSource("content_capture.go", "claudeCaptureRecordValidator", "line.Type"),
				recordKindSource("claude_control_records.go", "claudeControlRecordTypes"),
				recordKindPrefixSource("claude_control_records.go", "isClaudeControlRecordType", "recordType"),
			},
			Rules:      claudeRecordKindRules(),
			Production: claudeRecordKindProduction,
		},
		{
			Context:   RecordKindRetained,
			Namespace: "system_subtype",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "claudeStrictSystemSubtypes"),
				recordKindSwitchSource("content_capture.go", "claudeCaptureRecordValidator", "line.Subtype"),
			},
			Rules:      claudeSystemSubtypeKindRules(),
			Production: claudeSystemSubtypeKindProduction,
		},
		{
			Context:   RecordKindRetained,
			Namespace: "content_block",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "captureContentBlockKinds"),
				recordKindSwitchSource("content_capture.go", "validateCaptureContent", "block.Type"),
			},
			Rules:      claudeContentBlockKindRules(),
			Production: claudeContentBlockKindProduction,
		},
		{
			Context:   RecordKindNative,
			Namespace: "record",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "claudeStrictRecordKinds"),
				recordKindSwitchSource("content_capture.go", "claudeCaptureRecordValidator", "line.Type"),
				recordKindSource("claude_control_records.go", "claudeControlRecordTypes"),
				recordKindPrefixSource("claude_control_records.go", "isClaudeControlRecordType", "recordType"),
			},
			Rules:      claudeRecordKindRules(),
			Production: claudeRecordKindProduction,
		},
		{
			Context:   RecordKindNative,
			Namespace: "system_subtype",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "claudeStrictSystemSubtypes"),
				recordKindSwitchSource("content_capture.go", "claudeCaptureRecordValidator", "line.Subtype"),
			},
			Rules:      claudeSystemSubtypeKindRules(),
			Production: claudeSystemSubtypeKindProduction,
		},
		{
			Context:   RecordKindNative,
			Namespace: "content_block",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "captureContentBlockKinds"),
				recordKindSwitchSource("claude_indexer.go", "decomposeClaudeContentBlocks", "block.Type"),
			},
			Rules:      claudeContentBlockKindRules(),
			Production: claudeContentBlockKindProduction,
		},
	},
}

// claudeRecordKindRules declares every Claude record kind the shared validator
// recognizes.
func claudeRecordKindRules() []recordKindRule {
	return []recordKindRule{
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
	}
}

func claudeRecordKindProduction() recordKindProductionSet {
	return addRecordKindPrefixes(
		mergeRecordKindProduction(
			recordKindProductionLiterals(claudeStrictRecordKinds()),
			recordKindProductionLiterals(recordKindProductionMapKeys(claudeControlRecordTypes)),
		),
		claudeArtifactControlPrefix,
	)
}

// claudeSystemSubtypeKindRules declares every Claude system subtype the shared
// validator recognizes.
func claudeSystemSubtypeKindRules() []recordKindRule {
	return []recordKindRule{
		recordKindLiteral("turn_duration", indexformat.OutcomeIgnored),
		recordKindLiteral("compact_boundary", indexformat.OutcomeControl),
		recordKindLiteral("stop_hook_summary", indexformat.OutcomeIgnored),
		recordKindLiteral("api_error", indexformat.OutcomeText),
	}
}

func claudeSystemSubtypeKindProduction() recordKindProductionSet {
	return recordKindProductionLiterals(claudeStrictSystemSubtypes())
}

// claudeContentBlockKindRules declares every Claude content-block kind the
// shared validation and the depth-1 decomposition recognize.
func claudeContentBlockKindRules() []recordKindRule {
	return []recordKindRule{
		recordKindLiteral("text", indexformat.OutcomeText),
		recordKindLiteral("thinking", indexformat.OutcomeText),
		recordKindLiteral("tool_use", indexformat.OutcomeToolCall),
		recordKindLiteral("tool_result", indexformat.OutcomeToolResult),
		recordKindLiteral("tool_reference", indexformat.OutcomeIgnored),
	}
}

func claudeContentBlockKindProduction() recordKindProductionSet {
	return recordKindProductionLiterals(captureContentBlockKinds(HarnessClaudeCode))
}
