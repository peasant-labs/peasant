package export_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

// reclaimPayloadSessionID is the sandbox session the payload-identity test
// reclaims around.
const reclaimPayloadSessionID = schema.SessionID("bbbb2222-2222-4222-8222-222222222222")

// openReclaimPayloadStore opens a real generation-capable store with the
// production artifact and lock implementations and returns the store and its
// owned-artifact root.
func openReclaimPayloadStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	return openBarrierGenerationStore(t, make(chan struct{}, 8), make(chan time.Time, 8))
}

// reclaimPayloadGeneration builds a valid managed candidate with literal bodies.
func reclaimPayloadGeneration(t *testing.T, id string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	return buildBarrierGeneration(t, reclaimPayloadSessionID, barrierGenerationSpec{
		ID:            id,
		Text:          generationBlobSpec{Literal: "payload user text"},
		ToolInput:     generationBlobSpec{Literal: "payload tool input"},
		ToolOutput:    generationBlobSpec{Literal: "payload tool output"},
		IncludeResult: true,
	})
}

// TestSupersededGenerationReclaimPayloadsAreByteIdentical proves the three
// consumers that read a session — detail, export and publication packaging —
// produce byte-identical payloads before and after the superseded-generation
// reclaim. All three derive from the durable snapshot of the active
// generation, which the reclaim never touches.
func TestSupersededGenerationReclaimPayloadsAreByteIdentical(t *testing.T) {
	ctx := context.Background()
	db, _ := openReclaimPayloadStore(t)
	storetest.SeedSession(t, db, string(reclaimPayloadSessionID))

	superseded, supersededBlobs := reclaimPayloadGeneration(t, "g_reclaim_payload_old")
	if err := activateBarrierGeneration(t, db, superseded, supersededBlobs); err != nil {
		t.Fatalf("activate superseded generation: %v", err)
	}
	active, activeBlobs := reclaimPayloadGeneration(t, "g_reclaim_payload_active")
	if err := activateBarrierGeneration(t, db, active, activeBlobs); err != nil {
		t.Fatalf("activate active generation: %v", err)
	}

	detailBefore, detailPayloadBefore := reclaimPayloadBytes(t, db)
	exportBefore := reclaimExportBytes(t, db)
	publishBefore := reclaimPublishBytes(t, db, detailPayloadBefore)

	result, err := db.ReclaimSupersededGenerations(ctx, 0)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if result.Sessions != 1 || result.DirectoriesRemoved != 1 {
		t.Fatalf("reclaim = %d sessions, %d directories, want 1 and 1", result.Sessions, result.DirectoriesRemoved)
	}

	detailAfter, detailPayloadAfter := reclaimPayloadBytes(t, db)
	exportAfter := reclaimExportBytes(t, db)
	publishAfter := reclaimPublishBytes(t, db, detailPayloadAfter)

	if !bytes.Equal(detailBefore, detailAfter) {
		t.Fatal("detail payload changed across the reclaim")
	}
	if !bytes.Equal(exportBefore, exportAfter) {
		t.Fatal("export payload changed across the reclaim")
	}
	if !bytes.Equal(publishBefore, publishAfter) {
		t.Fatal("publication payload changed across the reclaim")
	}
}

// reclaimPayloadBytes builds the shared detail bytes and payload.
func reclaimPayloadBytes(t *testing.T, db *store.Store) ([]byte, *schema.SessionDetailPayload) {
	t.Helper()
	raw, payload, err := transcript.BuildSnapshotDetailBytes(context.Background(), db, db, reclaimPayloadSessionID)
	if err != nil {
		t.Fatalf("build detail bytes: %v", err)
	}
	return raw, payload
}

// reclaimExportBytes builds the export payload and marshals it.
func reclaimExportBytes(t *testing.T, db *store.Store) []byte {
	t.Helper()
	payload, err := export.ExportSnapshotPayload(context.Background(), db, db, reclaimPayloadSessionID)
	if err != nil {
		t.Fatalf("build export payload: %v", err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal export payload: %v", err)
	}
	return encoded
}

// reclaimPublishBytes builds the publication transcript content through the
// production publish builder and marshals it. The builder's managed-detail arm
// does not read the capture metadata; a nil metadata is its documented
// no-capture shape, so the assertion isolates the detail payload the reclaim
// must not disturb.
func reclaimPublishBytes(t *testing.T, db *store.Store, detail *schema.SessionDetailPayload) []byte {
	t.Helper()
	content, err := push.BuildPublishTranscriptContent(detail, nil, nil, schema.PushContractVersion("test-contract"), config.PushFieldVisibility{}, sessionorigin.User)
	if err != nil {
		t.Fatalf("build publication payload: %v", err)
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("marshal publication payload: %v", err)
	}
	return encoded
}
