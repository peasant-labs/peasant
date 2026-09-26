package push

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/schema"
)

// TestBuildTranscriptContentFromDetail pins the publication half of the single
// durable payload-construction boundary: an already-validated detail payload
// is wrapped in the versioned envelope with the negotiated contract version
// and the stored origin declaration, preserving every durable evidence field.
// No network access and no store mutation happen here.
func TestBuildTranscriptContentFromDetail(t *testing.T) {
	inputCount := int64(1)
	detail := &schema.SessionDetailPayload{
		ID:                   "45454545-4545-4545-4545-454545454548",
		Harness:              schema.HarnessCodex,
		TurnCount:            5,
		InputSubmissionCount: &inputCount,
		Purpose:              schema.SessionPurposeInteraction,
		Turns: []schema.TurnDetail{
			{Index: 0, Role: schema.RoleUser, Content: "fix parser"},
		},
		EarlierHistory: []schema.EarlierHistorySection{
			{State: schema.EarlierHistoryUncertainMigrated, Turns: []schema.TurnDetail{}},
		},
	}

	content := BuildTranscriptContentFromDetail(detail, schema.PushContractVersion("test-contract"), sessionorigin.User)
	if content.Kind != schema.ContentKindSessionDetail {
		t.Fatalf("envelope kind = %q, want session_detail", content.Kind)
	}
	if content.SessionDetail == nil {
		t.Fatal("envelope carries no session detail")
	}
	if content.SessionDetail.TurnCount != 5 || content.SessionDetail.InputSubmissionCount == nil || *content.SessionDetail.InputSubmissionCount != 1 {
		t.Fatalf("envelope detail counts = %d/%v, want 5/1", content.SessionDetail.TurnCount, content.SessionDetail.InputSubmissionCount)
	}
	if len(content.SessionDetail.EarlierHistory) != 1 {
		t.Fatalf("envelope detail earlier sections = %d, want 1", len(content.SessionDetail.EarlierHistory))
	}
	if content.SessionDetail.SessionOrigin != schema.SessionOriginUser {
		t.Fatalf("envelope detail origin = %q, want user", content.SessionDetail.SessionOrigin)
	}

	unknown := BuildTranscriptContentFromDetail(&schema.SessionDetailPayload{}, "test-contract", sessionorigin.Origin("bogus"))
	if unknown.SessionDetail.SessionOrigin != schema.SessionOriginUnknown {
		t.Fatalf("invalid stored origin declares %q, want unknown", unknown.SessionDetail.SessionOrigin)
	}
}
