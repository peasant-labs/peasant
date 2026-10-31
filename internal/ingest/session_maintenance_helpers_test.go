package ingest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// seedStoredPair records one stored session row and mirrors the pair its
// metadata names, so maintenance selection sees a row that identifies a saved
// copy. It returns the managed artifact the row's identity was recorded from;
// the caller decides whether that pair is left intact, damaged, or replaced.
func seedStoredPair(t *testing.T, database *store.Store, meta *ingest.UnifiedMetadata, transcript []byte) *ingest.ManagedArtifact {
	t.Helper()
	ctx := t.Context()
	meta.ContentHash = schema.ComputeTranscriptHash(transcript)
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
		t.Fatal(err)
	}
	artifact, err := ingest.NewManagedArtifact(encoded, transcript)
	if err != nil {
		t.Fatal(err)
	}
	mirrored := database.MirrorArtifacts(ctx, []ingest.ArtifactMirrorRequest{{Artifact: artifact}})
	if len(mirrored) != 1 || mirrored[0].Err != nil || !mirrored[0].Mirrored {
		t.Fatalf("seed stored pair identity: %+v", mirrored)
	}
	return artifact
}

// sessionDiagnostics returns every diagnostic the run attached to one session,
// matched by the session's location or by a message that names it.
func sessionDiagnostics(result *ingest.PipelineResult, id ingest.SessionID) []ingest.DiagnosticEntry {
	var found []ingest.DiagnosticEntry
	for _, diagnostic := range result.Diagnostics {
		if strings.Contains(diagnostic.Location, id.String()) || strings.Contains(diagnostic.Message, id.String()) {
			found = append(found, diagnostic)
		}
	}
	return found
}

// sourceUnavailableDiagnostics returns the diagnostics the two lanes that can
// name an unusable source attached to one session: pair repair and stored-content
// recovery.
func sourceUnavailableDiagnostics(result *ingest.PipelineResult, id ingest.SessionID) []ingest.DiagnosticEntry {
	var found []ingest.DiagnosticEntry
	for _, diagnostic := range sessionDiagnostics(result, id) {
		switch diagnostic.ErrorType {
		case "pair_repair_unavailable", "content_recovery_unavailable":
			found = append(found, diagnostic)
		}
	}
	return found
}

// typedSessionDiagnostics narrows sessionDiagnostics to one diagnostic type.
func typedSessionDiagnostics(result *ingest.PipelineResult, id ingest.SessionID, errorType string) []ingest.DiagnosticEntry {
	var found []ingest.DiagnosticEntry
	for _, diagnostic := range sessionDiagnostics(result, id) {
		if diagnostic.ErrorType == errorType {
			found = append(found, diagnostic)
		}
	}
	return found
}

// failedSessionCount counts the failed results one session carries.
func failedSessionCount(result *ingest.PipelineResult, id ingest.SessionID) int {
	count := 0
	for _, session := range result.Sessions {
		if session.SessionID == id && session.Error != nil {
			count++
		}
	}
	return count
}

// diagnosticText is the message plus its remediation, the two fields a reader
// acts on.
func diagnosticText(diagnostic ingest.DiagnosticEntry) string {
	return diagnostic.Message + "\n" + diagnostic.Remediation
}
