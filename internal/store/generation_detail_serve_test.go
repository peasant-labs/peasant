package store

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// buildDetailServeGeneration builds a section-4-shaped V2 candidate: one
// uncertain earlier block plus five main records from one qualifying
// submission, with a folded tool whose call and result refs stay distinct.
// Provenance rides on the input entries; the tool result body exceeds the
// preview limit so the test proves immutable long bytes survive activation.
func buildDetailServeGeneration(t *testing.T, sid schema.SessionID, genID string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	submission, err := schema.NewSubmissionRef("s_u1")
	if err != nil {
		t.Fatalf("submission ref: %v", err)
	}
	inputProvenance := &schema.ContentProvenance{
		Origin:        schema.ContentOriginSubmittedInput,
		Actor:         schema.ActorOriginUnknown,
		Delivery:      schema.DeliveryOriginSessionAdmission,
		Ownership:     schema.ContentOwnershipLocal,
		Evidence:      schema.EvidenceNativeTyped,
		InputModality: schema.InputModalityText,
		SubmissionRef: submission,
	}
	strptr := func(s string) *string { return &s }
	callID := "call-detail-serve"
	longResult := "parser.go:12 " + strings.Repeat("z", defaults.ContentPreviewLimit+1024)
	text := func(index int, role schema.Role, entryType schema.EntryType, ref, body string) schema.SessionEntry {
		preview := body
		return schema.SessionEntry{
			SessionID: sid, EntryIndex: index, Harness: defaults.HarnessClaudeCode,
			EntryType: entryType, Role: role, ContentPreview: &preview,
			SourceEntryRef: schema.SourceEntryRef(ref),
		}
	}
	e0 := text(0, ingest.RoleUser, ingest.EntryTypeText, "e_u1", "fix parser")
	e0.Provenance = inputProvenance
	e1 := text(1, ingest.RoleSystem, ingest.EntryTypeText, "e_ctx1", "injected AGENTS content")
	e2 := text(2, ingest.RoleUser, ingest.EntryTypeText, "e_media1", "media display")
	e2.Provenance = inputProvenance
	e3 := text(3, ingest.RoleAssistant, ingest.EntryTypeText, "e_a1", "I will inspect.")
	e4 := text(4, ingest.RoleAssistant, ingest.EntryTypeThinking, "e_reason1", "I will inspect.")
	parent := 3
	e5 := text(5, ingest.RoleAssistant, ingest.EntryTypeToolUse, "e_call1", "rg parser")
	e5.Depth = 1
	e5.ParentIndex = &parent
	e5.ToolCallID = &callID
	e6 := text(6, ingest.RoleTool, ingest.EntryTypeToolResult, "e_result1", longResult)
	e6.Depth = 1
	e6.ParentIndex = &parent
	e6.ToolCallID = &callID
	_ = strptr
	old := text(0, ingest.RoleUser, ingest.EntryTypeText, "e_old", "unique old child question")

	inputCount := int64(1)
	generation := indexformat.Generation{
		ID:           genID,
		Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  defaults.HarnessClaudeCode,
			Stats:         schema.SessionStats{TurnCount: 5, InputSubmissionCount: &inputCount},
			Purpose:       schema.SessionPurposeInteraction,
		},
		Main: indexformat.Partition{Entries: []schema.SessionEntry{e0, e1, e2, e3, e4, e5, e6}},
		Earlier: []indexformat.EarlierPartition{{
			State:   schema.EarlierHistoryUncertainMigrated,
			Content: indexformat.Partition{Entries: []schema.SessionEntry{old}},
		}},
		Content: []indexformat.ContentRecord{
			{Ref: "e_u1"}, {Ref: "e_ctx1"}, {Ref: "e_media1"}, {Ref: "e_a1"},
			{Ref: "e_reason1"}, {Ref: "e_call1"}, {Ref: "e_result1"}, {Ref: "e_old"},
		},
		SourceEvidenceDigest: strings.Repeat("b", 64),
		TitleRefs:            []schema.SourceEntryRef{"e_u1"},
	}
	blobs := map[schema.SourceEntryRef][]byte{
		"e_u1":      []byte("fix parser"),
		"e_ctx1":    []byte("injected AGENTS content"),
		"e_media1":  []byte("media display"),
		"e_a1":      []byte("I will inspect."),
		"e_reason1": []byte("I will inspect."),
		"e_call1":   []byte("rg parser"),
		"e_result1": []byte(longResult),
		"e_old":     []byte("unique old child question"),
	}
	return indexformat.V2{Generation: generation}, blobs
}

// TestGenerationServesDetailBoundaryReads proves the committed store serves
// exactly what the durable detail boundary consumes: one snapshot with the
// section-4 partition layout, exact refs, mirrored counts, and immutable
// managed blobs readable through the production ContentResolver. Detail,
// export and publication hydrate from this read; nothing here reparses a
// native source.
func TestGenerationServesDetailBoundaryReads(t *testing.T) {
	sid, err := schema.NewSessionID("45454545-4545-4545-4545-454545454548")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := openGenerationStore(t)
	seedGenerationSession(t, s, string(sid))
	g1, g1Blobs := buildDetailServeGeneration(t, sid, "g-detail-serve")
	if err := activateTestGeneration(t, s, g1, g1Blobs); err != nil {
		t.Fatalf("activate section-4 generation: %v", err)
	}

	wantRefs := []schema.SourceEntryRef{"e_u1", "e_ctx1", "e_media1", "e_a1", "e_reason1", "e_call1", "e_result1"}
	err = s.WithSessionSnapshot(context.Background(), sid, func(snapshot indexformat.ReadSnapshot) error {
		if snapshot.IndexVersion != 2 || snapshot.GenerationID != "g-detail-serve" {
			return errTestDetailServe("snapshot is not the activated G1")
		}
		if len(snapshot.Main.Entries) != len(wantRefs) {
			return errTestDetailServe("main entries mismatch")
		}
		for i, want := range wantRefs {
			if snapshot.Main.Entries[i].SourceEntryRef != want {
				return errTestDetailServe("main ref order mismatch")
			}
		}
		if len(snapshot.Earlier) != 1 || snapshot.Earlier[0].State != schema.EarlierHistoryUncertainMigrated {
			return errTestDetailServe("earlier section mismatch")
		}
		if len(snapshot.Earlier[0].Content.Entries) != 1 || snapshot.Earlier[0].Content.Entries[0].SourceEntryRef != "e_old" {
			return errTestDetailServe("earlier entries mismatch")
		}
		if snapshot.Session.TurnCount != 5 || snapshot.Metadata.Stats.TurnCount != 5 {
			return errTestDetailServe("turn count mirror mismatch")
		}
		if snapshot.Session.InputSubmissionCount == nil || *snapshot.Session.InputSubmissionCount != 1 {
			return errTestDetailServe("input count mirror mismatch")
		}
		if snapshot.Session.Purpose != schema.SessionPurposeInteraction {
			return errTestDetailServe("purpose mirror mismatch")
		}
		provenance := snapshot.Main.Entries[0].Provenance
		if provenance == nil || string(provenance.SubmissionRef) != "s_u1" {
			return errTestDetailServe("input provenance did not survive the snapshot")
		}
		result, err := s.ReadFullContent(context.Background(), sid, snapshot.GenerationID, contentRecordFor(t, snapshot, "e_result1"))
		if err != nil {
			return err
		}
		if len(result) <= defaults.ContentPreviewLimit+1024 {
			return errTestDetailServe("managed tool result is not the full long body")
		}
		if string(result) != string(g1Blobs["e_result1"]) {
			return errTestDetailServe("managed tool result bytes changed across activation")
		}
		old, err := s.ReadFullContent(context.Background(), sid, snapshot.GenerationID, contentRecordFor(t, snapshot, "e_old"))
		if err != nil {
			return err
		}
		if string(old) != "unique old child question" {
			return errTestDetailServe("earlier managed bytes changed across activation")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func contentRecordFor(t *testing.T, snapshot indexformat.ReadSnapshot, ref schema.SourceEntryRef) indexformat.ContentRecord {
	t.Helper()
	for _, record := range snapshot.Content {
		if record.Ref == ref {
			return record
		}
	}
	t.Fatalf("snapshot content map has no record for %q", ref)
	return indexformat.ContentRecord{}
}

func errTestDetailServe(reason string) error {
	return &detailServeError{reason: reason}
}

type detailServeError struct{ reason string }

func (e *detailServeError) Error() string { return "detail-serve snapshot mismatch: " + e.reason }
