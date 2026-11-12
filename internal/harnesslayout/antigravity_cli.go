package harnesslayout

import "github.com/peasant-labs/schema"

// ToolAntigravityCLI is the Antigravity command-line agent (agy).
const ToolAntigravityCLI Tool = "antigravity-cli"

func init() {
	Register(antigravityLayout(antigravityProduct{
		tool:        ToolAntigravityCLI,
		harness:     schema.HarnessAntigravityCLI,
		displayName: "Antigravity CLI",
		root:        "{home}/.gemini/antigravity-cli",
		rootEntries: []antigravityArtifact{
			antigravityDeclare("prompt-history", "history.jsonl", FormatJSONL, RoleAuxiliary, antigravityReadJSONL,
				"the prompts entered at the agy prompt, across conversations"),
			antigravityDeclare("implicit-context", "implicit/*.pb", FormatText, RoleAuxiliary, antigravityReadSize,
				"protobuf context per conversation; counted and sized, never read"),
			antigravityDeclare("knowledge", "knowledge/*", FormatText, RoleAuxiliary, antigravityReadSize,
				"knowledge items and their lock; counted and sized, never read"),
			antigravityDeclare("cli-logs", "log/*", FormatText, RoleAuxiliary, antigravityReadSize,
				"one log per agy run; counted and sized, never read"),
			antigravityDeclare("persistent-terminals", "persistent_terminals/*", FormatText, RoleAuxiliary, antigravityReadSize,
				"terminal state kept between steps; counted and sized, never read"),
			antigravityDeclare("cli-settings", "settings.json", FormatJSON, RoleAuxiliary, antigravityReadJSON,
				"agy settings"),
			antigravityDeclare("cli-state", "jetski_state.pbtxt", FormatText, RoleAuxiliary, antigravityReadFieldNames,
				"protobuf text with the product state; field names only"),
			antigravityDeclare("cli-summaries", "jetbox_summaries_proto.pb", FormatText, RoleIndex, antigravityReadSize,
				"protobuf summaries of conversations; counted and sized, never read"),
		},
	}))
}
