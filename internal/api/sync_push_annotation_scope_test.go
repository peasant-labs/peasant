package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestHandleSyncPush_PublishesOnlyTheAnnotationsOfThePublishedSessions drives
// the Share door with an annotation on the session it publishes, one on another
// stored session, and one on the project, and requires the Village to receive
// the first alone.
//
// The door used to follow every publish with an annotation push over the whole
// store, so sharing one session also published the labels of every session on
// the machine, including ones the user never chose to share.
func TestHandleSyncPush_PublishesOnlyTheAnnotationsOfThePublishedSessions(t *testing.T) {
	t.Parallel()
	const (
		publishedID = "aaaa1111-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		otherID     = "bbbb2222-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	)
	hs := newTestXDGHomes(t)
	base := filepath.Join(hs.Data, "peasant-sync")
	db := seedSyncDoorSession(t, hs.dbPath(), publishedID, base)
	t.Cleanup(func() { _ = db.Close() })
	// The other stored session: the same project, recorded separately, and
	// not part of this push.
	input, err := db.LoadPublicationInput(t.Context(), publishedID)
	if err != nil {
		t.Fatal(err)
	}
	otherMeta := input.Metadata
	otherMeta.SessionID = schema.SessionID(otherID)
	otherMeta.Source.FilePath = "/test/path/" + otherID + ".jsonl"
	otherPreview := "a session the user did not choose"
	testutil.SeedReadyPublication(t, db, &otherMeta, []schema.SessionEntry{{
		SessionID:      schema.SessionID(otherID),
		EntryIndex:     1,
		Role:           schema.RoleUser,
		Harness:        schema.Harness(defaults.HarnessClaudeCode),
		EntryType:      schema.EntryTypeText,
		ContentPreview: &otherPreview,
	}})

	annotator, err := db.GetAnnotatorIDByName(t.Context(), "outcome-classifier")
	if err != nil {
		t.Fatal(err)
	}
	annotationType, err := db.GetAnnotationTypeID(t.Context(), testutil.TestTypeIDSessionOutcome)
	if err != nil {
		t.Fatal(err)
	}
	published, other, project := publishedID, otherID, testutil.TestProjectHash.String()
	for _, target := range []store.CreateAnnotationParams{{SessionID: &published}, {SessionID: &other}, {ProjectHash: &project}} {
		target.AnnotatorID, target.AnnotationTypeID, target.Value = annotator, annotationType, "resolved"
		if _, err := db.CreateAnnotation(t.Context(), target); err != nil {
			t.Fatal(err)
		}
	}

	var (
		mu       sync.Mutex
		received []schema.AnnotationPushItem
	)
	village := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/transcripts/publish"):
			captured := &syncCapturedPublish{parts: map[string]string{}}
			captured.record(r)
			receipt, err := testutil.AuthoritativePublishReceipt([]byte(captured.snapshot()["metadata"]), true)
			if err != nil {
				t.Errorf("build authoritative receipt: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(receipt)
		case r.URL.Path == "/api/v1/annotations":
			var request schema.AnnotationPushRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode annotation push: %v", err)
				return
			}
			mu.Lock()
			received = append(received, request.Annotations...)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(schema.AnnotationPushResponse{Created: len(request.Annotations)})
		case strings.Contains(r.URL.Path, "/annotations/manifest"):
			_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{})
		default:
			_ = json.NewEncoder(w).Encode(schema.SchemaVersionResponse{ContentCapabilities: []schema.ContentCapability{schema.ContentCapabilityObservedModelV1}})
		}
	}))
	t.Cleanup(village.Close)
	writeSyncDoorCredentials(t, hs.Config, village.URL)

	cfg := config.BaseConfig()
	cfg.Output.BasePath = base
	response := httptest.NewRecorder()
	hs.handler(db, cfg).handleSyncPush(response, httptest.NewRequest(http.MethodPost, "/api/v1/sync/push",
		strings.NewReader(`{"sessionIds":["`+publishedID+`"]}`)))
	var result schema.SyncPushResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != http.StatusOK || result.New != 1 {
		t.Fatalf("Share push: status=%d result=%+v err=%v body=%s", response.Code, result, err, response.Body)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || received[0].SessionID == nil || *received[0].SessionID != publishedID {
		targets := make([]string, 0, len(received))
		for _, item := range received {
			target := string(item.TargetKind)
			if item.SessionID != nil {
				target += ":" + *item.SessionID
			}
			targets = append(targets, target)
		}
		t.Fatalf("the Village received annotations on %v; want exactly the one on the published session %s", targets, publishedID)
	}
}
