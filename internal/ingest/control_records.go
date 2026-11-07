package ingest

import "github.com/peasant-labs/schema"

// ControlRecordKindFor reports the control kind a stored provider part type
// names for one harness, or "" when the part type is not a rendered control
// record. The fold sets it on a depth-0 turn only.
//
// It returns partType whenever the harness's writer would have stored it as a
// control record: isClaudeControlRecordType for Claude Code (the closed control
// map plus the artifact- prefix family) and isCodexNativeControlKind for Codex.
// No other harness has a control vocabulary, so every kind it stores is
// conversation or private evidence and classifies as "".
//
// The returned set IS controlRowKinds — the render authorisation vocabulary —
// because it excludes the compaction notices a writer still stores as system
// rows: ClaudeCompactBoundaryLabel ("compact-boundary") and codexTypeCompacted
// ("compacted"). The record-kind registry's OutcomeControl storage set includes
// those notices and is deliberately NOT the render set, so adding a kind there
// can never widen what a consumer paints as a control row.
//
// The writer tables are the single definition. This function delegates to the
// same predicates the writers consult rather than restating a list, so the
// stored vocabulary and the classified vocabulary cannot drift; the parity test
// enumerates the writer's census through this function.
func ControlRecordKindFor(harness schema.Harness, partType string) string {
	if partType == "" {
		return ""
	}
	switch harness {
	case schema.HarnessClaudeCode:
		if isClaudeControlRecordType(partType) {
			return partType
		}
	case schema.HarnessCodex:
		if isCodexNativeControlKind(partType) {
			return partType
		}
	}
	return ""
}
