package ingest

import (
	"strings"
	"testing"
)

const orphanSlotSessionID = "ses_3cd91f52effeXd3QAJ54jOyzO7"

// previousVersionOrphanProjection is a version 1 managed legacy projection whose
// orphan slot uses the retired in-band marker and a fabricated parent link.
const previousVersionOrphanProjection = `{"format":"peasant.opencode.legacy-sqlite","version":1,"session_id":"ses_3cd91f52effeXd3QAJ54jOyzO7","messages":[` +
	`{"id":"msg_real","session_id":"ses_3cd91f52effeXd3QAJ54jOyzO7","time_created":1000,"time_updated":1000,"data":{"id":"msg_real","sessionID":"ses_3cd91f52effeXd3QAJ54jOyzO7","role":"user","content":"hi"},"parts":[{"id":"part_real","message_id":"msg_real","session_id":"ses_3cd91f52effeXd3QAJ54jOyzO7","time_created":1000,"time_updated":1000,"data":{"id":"part_real","type":"text","text":"hi"}}]},` +
	`{"id":"orphan-parent-part_x","session_id":"ses_3cd91f52effeXd3QAJ54jOyzO7","time_created":1100,"time_updated":1100,"data":{"id":"orphan-parent-part_x","sessionID":"ses_3cd91f52effeXd3QAJ54jOyzO7","role":"system","parentID":"msg_absent_parent","_peasant_orphan_part":true},"parts":[{"id":"part_x","message_id":"orphan-parent-part_x","session_id":"ses_3cd91f52effeXd3QAJ54jOyzO7","time_created":1100,"time_updated":1100,"data":{"id":"part_x","type":"text","text":"ORPHAN_CONTENT"}}]}` +
	`]}`

// TestOpenCodeManagedProjectionReadsPreviousVersionOrphan proves that a version 1
// artifact whose orphan slot uses the retired in-band marker still decodes,
// still renders the orphan as an entry, and never emits a missing-parent
// diagnostic for the fabricated parent link.
func TestOpenCodeManagedProjectionReadsPreviousVersionOrphan(t *testing.T) {
	projection, err := decodeManagedOpenCodeProjection([]byte(previousVersionOrphanProjection), openCodeLegacyProjectionFormat, openCodeLegacyProjectionVersion, SessionID(orphanSlotSessionID))
	if err != nil {
		t.Fatalf("previous-version managed projection did not decode: %v", err)
	}
	messages, err := parseManagedOpenCodeSemanticMessages(projection, "managed legacy SQLite")
	if err != nil {
		t.Fatalf("parse previous-version semantic messages: %v", err)
	}
	indexer := NewOpenCodeIndexer(&OSFileSystem{}, WithOpenCodeFullDepth(true), WithOpenCodeFullContent(true))
	entries := indexer.indexSemanticMessages(SessionID(orphanSlotSessionID), messages)
	orphanRendered := false
	for _, entry := range entries {
		if entry.ContentPreview != nil && strings.Contains(*entry.ContentPreview, "ORPHAN_CONTENT") {
			orphanRendered = true
		}
	}
	if !orphanRendered {
		t.Fatalf("previous-version orphan part was not rendered as an entry: %+v", entries)
	}
	session := DiscoveredSession{SessionID: SessionID(orphanSlotSessionID), SourcePath: ResolvedPath("/synthetic/orphan.db")}
	for _, warning := range missingOpenCodeParentDiagnostics(session, messages) {
		if warning.ErrorType == string(OpenCodeGraphMissingParent) && strings.Contains(warning.Message, "msg_absent_parent") {
			t.Fatalf("previous-version orphan slot emitted a spurious missing-parent diagnostic: %+v", warning)
		}
	}
}

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
