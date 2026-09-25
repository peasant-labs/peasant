package ingest

import "github.com/peasant-labs/peasant/internal/indexformat"

var piVocabulary = recordKindAdapterVocabulary{
	Harness: HarnessPi,
	Inventories: []recordKindInventoryDeclaration{
		{
			Context:   RecordKindRetained,
			Namespace: "entry",
			Sources: []RecordKindSource{
				recordKindSwitchSource("pi_source.go", "piEntryType.UnmarshalJSON", "value"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("session", indexformat.OutcomeIgnored),
				recordKindLiteral("message", indexformat.OutcomeText),
				recordKindLiteral("thinking_level_change", indexformat.OutcomeControl),
				recordKindLiteral("model_change", indexformat.OutcomeControl),
				recordKindLiteral("compaction", indexformat.OutcomeText),
				recordKindLiteral("branch_summary", indexformat.OutcomeText),
				recordKindLiteral("custom", indexformat.OutcomeControl),
				recordKindLiteral("custom_message", indexformat.OutcomeText),
				recordKindLiteral("label", indexformat.OutcomeControl),
				recordKindLiteral("session_info", indexformat.OutcomeControl),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(piEntryTypeKinds())
			},
		},
		{
			Context:   RecordKindRetained,
			Namespace: "message_role",
			Sources: []RecordKindSource{
				recordKindSwitchSource("pi_indexer.go", "piMessageRole.UnmarshalJSON", "value"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("user", indexformat.OutcomeText),
				recordKindLiteral("assistant", indexformat.OutcomeText),
				recordKindLiteral("toolResult", indexformat.OutcomeToolResult),
				recordKindLiteral("bashExecution", indexformat.OutcomeText),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(piMessageRoleKinds())
			},
		},
		{
			Context:   RecordKindRetained,
			Namespace: "content_block",
			Sources: []RecordKindSource{
				recordKindSwitchSource("pi_indexer.go", "piBlockType.UnmarshalJSON", "value"),
				recordKindSwitchSource("pi_indexer.go", "piContent", "block.Type"),
			},
			Rules: []recordKindRule{
				recordKindLiteral("text", indexformat.OutcomeText),
				recordKindLiteral("thinking", indexformat.OutcomeText),
				recordKindLiteral("image", indexformat.OutcomeText),
				recordKindLiteral("toolCall", indexformat.OutcomeToolCall),
			},
			Production: func() recordKindProductionSet {
				return recordKindProductionLiterals(piBlockTypeKinds())
			},
		},
	},
}
