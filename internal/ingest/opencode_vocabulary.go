package ingest

import "github.com/peasant-labs/peasant/internal/indexformat"

var openCodeVocabulary = recordKindAdapterVocabulary{
	Harness: HarnessOpenCode,
	Inventories: []recordKindInventoryDeclaration{
		{
			Context:   RecordKindRetained,
			Namespace: "part",
			Sources: []RecordKindSource{
				recordKindSource("opencode_capture.go", "openCodeCaptureControlKinds"),
				recordKindSource("opencode_indexer.go", "knownOpenCodeSemanticPartKinds"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("step-start", indexformat.OutcomeIgnored),
				recordKindLiteral("step-finish", indexformat.OutcomeIgnored),
				recordKindLiteral("snapshot", indexformat.OutcomeIgnored),
				recordKindLiteral("patch", indexformat.OutcomeIgnored),
				recordKindLiteral("text", indexformat.OutcomeText),
				recordKindLiteral("reasoning", indexformat.OutcomeText),
				recordKindLiteral("tool", indexformat.OutcomeToolCall),
				recordKindLiteral("tool_use", indexformat.OutcomeToolCall),
				recordKindLiteral("tool_result", indexformat.OutcomeToolResult),
				recordKindLiteral("compaction", indexformat.OutcomeText),
				recordKindLiteral("subtask", indexformat.OutcomeText),
				recordKindLiteral("agent", indexformat.OutcomeText),
			},
			Production: func() recordKindProductionSet {
				return mergeRecordKindProduction(
					recordKindProductionLiterals(openCodeCaptureControlKinds()),
					recordKindProductionLiterals(knownOpenCodeSemanticPartKinds()),
				)
			},
		},
		{
			Context:   RecordKindRetained,
			Namespace: "row",
			Sources: []RecordKindSource{
				recordKindCompleteSource("opencode_current_projection.go", "normalizeOpenCodeCurrentRowAt", "row.Type.String()"),
				recordKindCompleteSource("opencode_unknown.go", "knownOpenCodeCurrentRow", "kind"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("user", indexformat.OutcomeText),
				recordKindLiteral("assistant", indexformat.OutcomeText),
				recordKindLiteral("shell", indexformat.OutcomeText),
				recordKindLiteral("synthetic", indexformat.OutcomeText),
				recordKindLiteral("system", indexformat.OutcomeText),
				recordKindLiteral("skill", indexformat.OutcomeText),
				recordKindLiteral("compaction", indexformat.OutcomeText),
				recordKindLiteral("agent-switched", indexformat.OutcomeText),
				recordKindLiteral("model-switched", indexformat.OutcomeText),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(knownOpenCodeCurrentRowKinds())
			},
		},
		{
			Context:   RecordKindRetained,
			Namespace: "assistant_content",
			Sources: []RecordKindSource{
				recordKindSwitchSource("opencode_current_projection.go", "appendOpenCodeCurrentAssistantContent", "discriminator.Type"),
				recordKindCompleteOperandSource("opencode_unknown.go", "prepareOpenCodeCurrent", "header.Type"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("text", indexformat.OutcomeText),
				recordKindLiteral("reasoning", indexformat.OutcomeText),
				recordKindLiteral("tool", indexformat.OutcomeToolCall),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(knownOpenCodeAssistantContentKinds())
			},
		},
		{
			Context:   RecordKindNative,
			Namespace: "row",
			Sources: []RecordKindSource{
				recordKindCompleteSource("opencode_provenance_decode.go", "DecodeOpenCodeProvenanceRow", "row.Type"),
				recordKindCompleteSource("opencode_unknown.go", "knownOpenCodeCurrentRow", "kind"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("user", indexformat.OutcomeText),
				recordKindLiteral("assistant", indexformat.OutcomeText),
				recordKindLiteral("shell", indexformat.OutcomeText),
				recordKindLiteral("synthetic", indexformat.OutcomeText),
				recordKindLiteral("system", indexformat.OutcomeText),
				recordKindLiteral("skill", indexformat.OutcomeText),
				recordKindLiteral("compaction", indexformat.OutcomeText),
				recordKindLiteral("agent-switched", indexformat.OutcomeText),
				recordKindLiteral("model-switched", indexformat.OutcomeText),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(knownOpenCodeCurrentRowKinds())
			},
		},
		{
			Context:   RecordKindNative,
			Namespace: "assistant_content",
			Sources: []RecordKindSource{
				recordKindSwitchSource("opencode_current_projection.go", "appendOpenCodeCurrentAssistantContent", "discriminator.Type"),
				recordKindCompleteOperandSource("opencode_unknown.go", "prepareOpenCodeCurrent", "header.Type"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("text", indexformat.OutcomeText),
				recordKindLiteral("reasoning", indexformat.OutcomeText),
				recordKindLiteral("tool", indexformat.OutcomeToolCall),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(knownOpenCodeAssistantContentKinds())
			},
		},
		{
			Context:   RecordKindRetained,
			Namespace: "content_block",
			Sources: []RecordKindSource{
				recordKindOperandSource("opencode_unknown.go", "retainOpenCodeSemantic", "discriminator.Type"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("text", indexformat.OutcomeText),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(knownOpenCodeInlineContentKinds())
			},
		},
		{
			Context:   RecordKindRetained,
			Namespace: "tool_content",
			Sources: []RecordKindSource{
				recordKindSwitchSource("opencode_v2_projection.go", "decodeOpenCodeV2ToolState", "value.Type"),
				recordKindCompleteOperandSource("opencode_unknown.go", "prepareOpenCodeToolContent", "header.Type"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("text", indexformat.OutcomeToolResult),
				recordKindLiteral("file", indexformat.OutcomeToolResult),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(knownOpenCodeToolContentKinds())
			},
		},
		{
			Context:   RecordKindNative,
			Namespace: "tool_content",
			Sources: []RecordKindSource{
				recordKindSwitchSource("opencode_v2_projection.go", "decodeOpenCodeV2ToolState", "value.Type"),
				recordKindCompleteOperandSource("opencode_unknown.go", "prepareOpenCodeToolContent", "header.Type"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("text", indexformat.OutcomeToolResult),
				recordKindLiteral("file", indexformat.OutcomeToolResult),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(knownOpenCodeToolContentKinds())
			},
		},
	},
}
