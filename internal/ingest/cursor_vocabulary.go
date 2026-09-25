package ingest

import "github.com/peasant-labs/peasant/internal/indexformat"

var cursorVocabulary = recordKindAdapterVocabulary{
	Harness: HarnessCursor,
	Inventories: []recordKindInventoryDeclaration{
		{
			Context:   RecordKindRetained,
			Namespace: "role",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "captureRoleKinds"),
				recordKindSource("unknown_jsonl.go", "cursorCaptureRoleKinds"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("user", indexformat.OutcomeText),
				recordKindLiteral("assistant", indexformat.OutcomeText),
				recordKindLiteral("system", indexformat.OutcomeText),
				recordKindLiteral("tool", indexformat.OutcomeText),
				recordKindLiteral("human", indexformat.OutcomeText),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(cursorCaptureRoleKinds())
			},
		},
		{
			Context:   RecordKindRetained,
			Namespace: "record",
			Sources: []RecordKindSource{
				recordKindSource("content_capture.go", "cursorStrictRecordKinds"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("turn_ended", indexformat.OutcomeText),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(cursorStrictRecordKinds())
			},
		},
		{
			Context:   RecordKindRetained,
			Namespace: "content_block",
			Sources: []RecordKindSource{
				recordKindListSource("content_capture.go", "captureContentBlockKinds"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("text", indexformat.OutcomeText),
				recordKindLiteral("thinking", indexformat.OutcomeText),
				recordKindLiteral("tool_use", indexformat.OutcomeToolCall),
				recordKindLiteral("tool_result", indexformat.OutcomeToolResult),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(captureContentBlockKinds(HarnessCursor))
			},
		},
	},
}
