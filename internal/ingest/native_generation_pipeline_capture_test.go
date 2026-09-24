package ingest

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/schema"
)

// captureTestSessionID is the fixed identity the capture constructor cases
// below share, so a mismatch case differs by exactly one field.
const captureTestSessionID = "8f2b4c1a-3d5e-4f6a-8b9c-0d1e2f3a4b5c"

// validCaptureMetadata builds a recorded managed metadata snapshot the store's
// own-validity rule certifies: current schema, valid session and project
// identities, a valid content digest, an exact CWD agreement, and a matching
// integrity digest.
func validCaptureMetadata(t *testing.T, sessionID string) *UnifiedMetadata {
	t.Helper()
	sid, err := NewSessionID(sessionID)
	if err != nil {
		t.Fatalf("NewSessionID(%q): %v", sessionID, err)
	}
	meta := NewUnifiedMetadata()
	meta.SessionID = sid
	meta.ModelHarness = HarnessClaudeCode
	meta.SchemaVersion = CurrentSchemaVersion
	meta.CWD = "/home/test/testrepo"
	projectHash, _, err := DeriveProjectIdentifiers(salt.Salt{}, "github.com/test/testrepo", "/home/test/testrepo")
	if err != nil {
		t.Fatalf("derive project identity: %v", err)
	}
	meta.Project.Hash = projectHash
	meta.ContentHash = schema.ComputeTranscriptHash([]byte("recorded transcript bytes"))
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	if err := ValidatePublicationCaptureSnapshot(&meta, CWDSourceExact); err != nil {
		t.Fatalf("valid capture snapshot refused: %v", err)
	}
	return &meta
}

func captureTestSession(t *testing.T, sessionID string) DiscoveredSession {
	t.Helper()
	sid, err := NewSessionID(sessionID)
	if err != nil {
		t.Fatalf("NewSessionID(%q): %v", sessionID, err)
	}
	return DiscoveredSession{SessionID: sid, Harness: HarnessClaudeCode}
}

// TestManagedActivationCaptureCertifiesCompleteOnly drives the production
// capture constructor the native activation records: a nil input or nil
// metadata records nothing, an incomplete_new candidate records nothing even
// with a certifiable snapshot, a complete candidate with matched identity
// records the certified agreement, and a complete candidate whose snapshot
// names another session or harness records nothing.
func TestManagedActivationCaptureCertifiesCompleteOnly(t *testing.T) {
	t.Parallel()
	session := captureTestSession(t, captureTestSessionID)
	meta := validCaptureMetadata(t, captureTestSessionID)
	input := &CapturedIndexInput{session: session, metadata: meta}

	if got := managedActivationCapture(nil, session, indexformat.GenerationCompletenessComplete); got != nil {
		t.Fatalf("nil input captured %+v, want nil", got)
	}
	if got := managedActivationCapture(&CapturedIndexInput{session: session}, session, indexformat.GenerationCompletenessComplete); got != nil {
		t.Fatalf("nil metadata captured %+v, want nil", got)
	}
	if got := managedActivationCapture(input, session, indexformat.GenerationCompletenessIncompleteNew); got != nil {
		t.Fatalf("incomplete_new captured %+v, want nil: incomplete means nil capture", got)
	}
	complete := managedActivationCapture(input, session, indexformat.GenerationCompletenessComplete)
	if complete == nil {
		t.Fatal("complete candidate with matched identity captured nil, want the certified agreement")
	}
	if complete.Metadata.SessionID != session.SessionID {
		t.Fatalf("captured session = %q, want %q", complete.Metadata.SessionID, session.SessionID)
	}
	if complete.CWDProvenance != CWDSourceExact {
		t.Fatalf("captured provenance = %q, want %q", complete.CWDProvenance, CWDSourceExact)
	}
	if err := ValidatePublicationCaptureSnapshot(&complete.Metadata, complete.CWDProvenance); err != nil {
		t.Fatalf("captured agreement fails the store rule: %v", err)
	}

	otherSession := captureTestSession(t, "7a1b3c2d-4e5f-4a6b-8c9d-0e1f2a3b4c5d")
	if got := managedActivationCapture(input, otherSession, indexformat.GenerationCompletenessComplete); got != nil {
		t.Fatalf("mismatched session captured %+v, want nil", got)
	}
	otherHarness := session
	otherHarness.Harness = HarnessCodex
	if got := managedActivationCapture(input, otherHarness, indexformat.GenerationCompletenessComplete); got != nil {
		t.Fatalf("mismatched harness captured %+v, want nil", got)
	}
	broken := *meta
	broken.ContentHash = "not-a-valid-content-digest"
	broken.MetadataHash = schema.ComputeMetadataHash(&broken)
	if got := managedActivationCapture(&CapturedIndexInput{session: session, metadata: &broken}, session, indexformat.GenerationCompletenessComplete); got != nil {
		t.Fatalf("uncertifiable snapshot captured %+v, want nil", got)
	}
}
