package ingest

import (
	"strings"
	"testing"
)

const orphanSlotSessionID = "ses_3cd91f52effeXd3QAJ54jOyzO7"

// TestOpenCodeCycleBrokenParentDiagnostic proves that a parent link that would
// close a cycle produces a diagnostic, not only an absent parent.
func TestOpenCodeCycleBrokenParentDiagnostic(t *testing.T) {
	messages := []openCodeSemanticMessage{
		{EntryID: "msg_a", Data: openCodeIndexMsg{ID: "msg_a", ParentID: "msg_b", Role: RoleUser.String()}},
		{EntryID: "msg_b", Data: openCodeIndexMsg{ID: "msg_b", ParentID: "msg_a", Role: RoleUser.String()}},
	}
	session := DiscoveredSession{SessionID: SessionID(orphanSlotSessionID), SourcePath: ResolvedPath("/synthetic/orphan.db")}
	diagnostics := missingOpenCodeParentDiagnostics(session, messages)
	cycleNamed := false
	for _, warning := range diagnostics {
		if warning.ErrorType == string(OpenCodeGraphMissingParent) && strings.Contains(warning.Message, "cycle") {
			cycleNamed = true
		}
	}
	if !cycleNamed {
		t.Fatalf("a cycle-broken parent link produced no cycle diagnostic: %+v", diagnostics)
	}
}
