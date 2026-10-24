package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/schema"
)

func claudeIdentityTestSID(t *testing.T, raw string) schema.SessionID {
	t.Helper()
	sid, err := NewSessionID(raw)
	if err != nil {
		t.Fatalf("session id %q: %v", raw, err)
	}
	return schema.SessionID(sid)
}

func claudeIdentityTestOwnerFile(t *testing.T, dir, owner string) {
	t.Helper()
	line := `{"type":"user","uuid":"owner-root","sessionId":"` + owner + `","message":{"role":"user","content":"owner"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, owner+".jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedClaudeIdentityConflictingStartedBy(t *testing.T) {
	const owner = "11111111-1111-4111-8111-111111111111"
	dir := t.TempDir()
	claudeIdentityTestOwnerFile(t, dir, owner)
	data := []byte(`{"type":"assistant","uuid":"a1","sessionId":"` + owner + `","agentId":"abc123","isSidechain":true,"message":{"role":"assistant","content":"agent reply"}}` + "\n")
	target := claudeIdentityTestSID(t, "22222222-2222-4222-8222-222222222222")
	original := &UnifiedMetadata{
		SchemaVersion: CurrentSchemaVersion,
		SessionID:     claudeIdentityTestSID(t, "agent-abc123"),
		ModelHarness:  HarnessClaudeCode,
		Source:        SourceInfo{FilePath: filepath.Join(dir, "agent-abc123.jsonl"), Format: SourceFormatJSONL},
		ContentHash:   schema.ComputeTranscriptHash(data),
		Relationships: []schema.SessionRelationship{{
			Kind:          schema.SessionRelationshipStartedBy,
			TargetState:   schema.RelationshipTargetKnown,
			TargetLocalID: &target,
			Evidence:      schema.EvidenceNativeTyped,
		}},
	}
	_, _, err := verifyRetainedClaudeIdentityWith(data, original, original.ContentHash, claudeIdentitySnapshotFrom(&OSFileSystem{}))
	if err == nil || !strings.Contains(err.Error(), "disagrees with the transcript owner") {
		t.Fatalf("error = %v, want the recorded-owner conflict", err)
	}
}

func TestRetainedClaudeIdentityRetainedLastGoodOwner(t *testing.T) {
	const owner = "11111111-1111-4111-8111-111111111111"
	data := []byte(`{"type":"assistant","uuid":"a1","sessionId":"` + owner + `","agentId":"abc123","isSidechain":true,"message":{"role":"assistant","content":"agent reply"}}` + "\n")
	target := claudeIdentityTestSID(t, owner)
	original := &UnifiedMetadata{
		SchemaVersion: CurrentSchemaVersion,
		SessionID:     claudeIdentityTestSID(t, "agent-abc123"),
		ModelHarness:  HarnessClaudeCode,
		Source:        SourceInfo{FilePath: "/native/absent-owner.jsonl", Format: SourceFormatJSONL},
		ContentHash:   schema.ComputeTranscriptHash(data),
		Relationships: []schema.SessionRelationship{{
			Kind:          schema.SessionRelationshipStartedBy,
			TargetState:   schema.RelationshipTargetKnownRetained,
			TargetLocalID: &target,
			Evidence:      schema.EvidenceRetainedLastGood,
		}},
	}
	allowed, relationships, err := verifyRetainedClaudeIdentityWith(data, original, original.ContentHash, claudeIdentitySnapshotFrom(&OSFileSystem{}))
	if err != nil {
		t.Fatalf("retained last-good owner refused: %v", err)
	}
	if !allowed[owner] {
		t.Fatalf("allowed = %v, want %s", allowed, owner)
	}
	if len(relationships) != 1 || relationships[0].TargetLocalID == nil || relationships[0].TargetLocalID.String() != owner {
		t.Fatalf("relationships = %+v, want one started_by to %s", relationships, owner)
	}
}

func TestRetainedClaudeIdentityNilTargetStartedBy(t *testing.T) {
	const owner = "11111111-1111-4111-8111-111111111111"
	dir := t.TempDir()
	claudeIdentityTestOwnerFile(t, dir, owner)
	data := []byte(`{"type":"assistant","uuid":"a1","sessionId":"` + owner + `","agentId":"abc123","isSidechain":true,"message":{"role":"assistant","content":"agent reply"}}` + "\n")
	original := &UnifiedMetadata{
		SchemaVersion: CurrentSchemaVersion,
		SessionID:     claudeIdentityTestSID(t, "agent-abc123"),
		ModelHarness:  HarnessClaudeCode,
		Source:        SourceInfo{FilePath: filepath.Join(dir, "agent-abc123.jsonl"), Format: SourceFormatJSONL},
		ContentHash:   schema.ComputeTranscriptHash(data),
		Relationships: []schema.SessionRelationship{{
			Kind:        schema.SessionRelationshipStartedBy,
			TargetState: schema.RelationshipTargetConflictingCurrentNativeEvidence,
			Evidence:    schema.EvidenceConflict,
		}},
	}
	_, _, err := verifyRetainedClaudeIdentityWith(data, original, original.ContentHash, claudeIdentitySnapshotFrom(&OSFileSystem{}))
	if err == nil || !strings.Contains(err.Error(), "recorded started_by owner is unavailable") {
		t.Fatalf("error = %v, want the unavailable-owner refusal", err)
	}
}
