package ingest

import (
	"errors"
	"fmt"
	"strings"
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
	// Every type on the compatibility list is reported as the cause, not just
	// the schema one: the branch that used to special-case two of them would
	// have wrapped this third one back up, and reintroduced the duplicate for
	// any type added later.
	const header = SessionID("ses_headererr00")
	pipeline.reportPendingRecoveryFailure(errors.Join(
		&artifactSessionError{SessionID: header, Err: fmt.Errorf("mirror committed session %s: %w", header, &MetadataHeaderError{
			Path: string(header) + " (stored metadata)", Cause: errors.New("compatibility header is unreadable"),
		})},
		&artifactSessionError{SessionID: refused, Err: &UnsupportedMetadataVersionError{
			Path: string(refused) + " (stored metadata)", Version: CurrentSchemaVersion + 1,
		}},
		&artifactSessionError{SessionID: "ses_broken0000", Err: errors.New("temporary evidence could not be read")},
		errors.New("the recovery walk itself failed"),
	))
	got := pipeline.snapshotDiagnostics()
	if len(got) != 4 {
		t.Fatalf("four independent failures produced %d diagnostics: %+v", len(got), got)
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
	headerRefusal, ok := byLocation[string(header)]
	if !ok || headerRefusal.ErrorType != "metadata_refused" {
		t.Fatalf("an unreadable compatibility header was not reported as the refusal it is: found=%v %+v", ok, got)
	}
	// The recovery path wraps each failure in the operation that met it, the
	// way the mirror does on the production path. What the user must read is
	// the refusal, so that it is the same entry the selection builds; the
	// wrapper is one more spelling of one cause.
	if strings.Contains(headerRefusal.Message, "mirror committed session") {
		t.Fatalf("the refusal was reported wrapped in the operation that met it, so it cannot collapse with the same refusal elsewhere: %+v", headerRefusal)
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
