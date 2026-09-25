package ingest

import "github.com/peasant-labs/peasant/internal/indexformat"

var strikeVocabulary = recordKindAdapterVocabulary{
	Harness: HarnessStrike,
	Inventories: []recordKindInventoryDeclaration{
		{
			Context:   RecordKindRetained,
			Namespace: "record",
			Sources: []RecordKindSource{
				recordKindSource("strike.go", "knownStrikeEventKinds"),
				recordKindSwitchSource("strike_indexer.go", "StrikeIndexer.parseWithCompletion", "envelope.Type"),
				recordKindSwitchSource("content_capture.go", "StrikeIndexer.IndexTranscriptBytesForCapture", "env.Type"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("session.started", indexformat.OutcomeIgnored),
				recordKindLiteral("session.titled", indexformat.OutcomeIgnored),
				recordKindLiteral("model.selected", indexformat.OutcomeIgnored),
				recordKindLiteral("user.message", indexformat.OutcomeText),
				recordKindLiteral("turn.started", indexformat.OutcomeIgnored),
				recordKindLiteral("turn.completed", indexformat.OutcomeIgnored),
				recordKindLiteral("assistant.text", indexformat.OutcomeText),
				recordKindLiteral("assistant.text.delta", indexformat.OutcomeText),
				recordKindLiteral("assistant.message.delta", indexformat.OutcomeText),
				recordKindLiteral("text.delta", indexformat.OutcomeText),
				recordKindLiteral("assistant.reasoning", indexformat.OutcomeText),
				recordKindLiteral("assistant.reasoning.delta", indexformat.OutcomeText),
				recordKindLiteral("reasoning.delta", indexformat.OutcomeText),
				recordKindLiteral("assistant.thinking.delta", indexformat.OutcomeText),
				recordKindLiteral("tool.begin", indexformat.OutcomeToolCall),
				recordKindLiteral("tool.output", indexformat.OutcomeToolResult),
				recordKindLiteral("tool.end", indexformat.OutcomeToolResult),
				recordKindLiteral("process.started", indexformat.OutcomeToolCall),
				recordKindLiteral("process.output", indexformat.OutcomeToolResult),
				recordKindLiteral("process.exited", indexformat.OutcomeToolResult),
				recordKindLiteral("usage.reported", indexformat.OutcomeIgnored),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(knownStrikeEventKinds)
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
				return recordKindProductionLiterals(captureContentBlockKinds(HarnessStrike))
			},
		},
	},
}
