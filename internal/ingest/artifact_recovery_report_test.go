package ingest

import (
	"errors"
	"testing"
)

// TestPendingRecoveryFailureReportsRefusalsPerSession holds who a pending
// recovery failure is reported against.
//
// recoverPending visits independent intents and joins what it could not
// finish, which loses which session each failure came from. A refusal this
// build cannot lift - a stored schema newer than it understands - must still
// reach the user against ITS session, with the remedy that lifts it, so it
// collapses with the same refusal from the index selection instead of arriving
// as an anonymous output-directory entry telling the user to inspect recovery
// evidence. Anything else stays an artifact-recovery diagnostic.
func TestPendingRecoveryFailureReportsRefusalsPerSession(t *testing.T) {
	const refused = SessionID("ses_refused000")
	pipeline := &Pipeline{config: PipelineConfig{OutputDir: "/output"}}
	pipeline.reportPendingRecoveryFailure(errors.Join(
		&artifactSessionError{SessionID: refused, Err: &UnsupportedMetadataVersionError{
			Path: string(refused) + " (stored metadata)", Version: CurrentSchemaVersion + 1,
		}},
		&artifactSessionError{SessionID: "ses_broken0000", Err: errors.New("temporary evidence could not be read")},
		errors.New("the recovery walk itself failed"),
	))
	got := pipeline.snapshotDiagnostics()
	if len(got) != 3 {
		t.Fatalf("three independent failures produced %d diagnostics: %+v", len(got), got)
	}
	byLocation := make(map[string]DiagnosticEntry, len(got))
	for _, diagnostic := range got {
		byLocation[diagnostic.Location] = diagnostic
	}
	refusal, ok := byLocation[string(refused)]
	if !ok {
		t.Fatalf("the refusal was not reported against its session: %+v", got)
	}
	if refusal.ErrorType != "metadata_refused" {
		t.Fatalf("the refusal was reported as %q, so it cannot collapse with the same refusal from the selection: %+v", refusal.ErrorType, refusal)
	}
	if refusal.Remediation != "Upgrade Peasant to a build compatible with the recorded producer/schema version." {
		t.Fatalf("the refusal carries a remedy that does not lift it: %q", refusal.Remediation)
	}
	// Neither of the other two lifts by upgrading, so both stay recovery
	// diagnostics against the output directory the user can inspect.
	other, ok := byLocation["/output"]
	if !ok || other.ErrorType != "artifact_recovery_incomplete" {
		t.Fatalf("a failure that is not a refusal changed reporter: found=%v %+v", ok, got)
	}
	for _, diagnostic := range got {
		if diagnostic.Location == "ses_broken0000" {
			t.Fatalf("an unreadable intent was reported as a session refusal: %+v", diagnostic)
		}
	}
}
