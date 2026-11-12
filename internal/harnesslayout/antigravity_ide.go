package harnesslayout

import "github.com/peasant-labs/schema"

// ToolAntigravityIDE is the Antigravity desktop application.
const ToolAntigravityIDE Tool = "antigravity-ide"

func init() {
	Register(antigravityLayout(antigravityProduct{
		tool:        ToolAntigravityIDE,
		harness:     schema.HarnessAntigravityIDE,
		displayName: "Antigravity IDE",
		root:        "{home}/.gemini/antigravity",
		sessionExtras: []antigravityArtifact{
			antigravityDeclare("agent-messages", antigravitySystemDir+"/messages/*.json", FormatJSON, RoleAuxiliary, antigravityReadJSON,
				"messages between agents of the conversation, and read.json, the set of messages already read"),
			antigravityDeclare("undelivered-messages", antigravitySystemDir+"/messages/undelivered/*", FormatJSON, RoleAuxiliary, antigravityReadSize,
				"messages not yet delivered; counted and sized, never read"),
		},
		rootEntries: []antigravityArtifact{
			antigravityDeclare("daemon-hub", "daemon/hub_*.json", FormatJSON, RoleAuxiliary, antigravityReadJSON,
				"one file per running hub process"),
			antigravityDeclare("ide-state", "antigravity_state.pbtxt", FormatText, RoleAuxiliary, antigravityReadFieldNames,
				"protobuf text with the product state and its migrations; field names only"),
			antigravityDeclare("ide-summaries", "agyhub_summaries_proto.pb", FormatText, RoleIndex, antigravityReadSize,
				"protobuf summaries of conversations; counted and sized, never read"),
		},
	}))
}
