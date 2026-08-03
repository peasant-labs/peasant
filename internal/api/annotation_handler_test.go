package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// ---------------------------------------------------------------------------
// Batch test helpers.
// ---------------------------------------------------------------------------

// batchTestProvider is a minimal DataProvider for annotation batch tests.
// It satisfies the api.DataProvider interface with no-op methods since the
// batch endpoint reads directly from the store, not the DataProvider.
type batchTestProvider struct{}

func (batchTestProvider) Sessions(_ context.Context) ([]ingest.Session, error) { return nil, nil }
func (batchTestProvider) SessionSummaries(_ context.Context) ([]api.SessionSummary, error) {
	return nil, nil
}
func (batchTestProvider) SessionByID(_ context.Context, _ string) (*ingest.Session, error) {
	return nil, nil
}
func (batchTestProvider) DashboardMetrics(_ context.Context) (*api.DashboardPayload, error) {
	return nil, nil
}
func (batchTestProvider) TrendsData(_ context.Context) (*api.TrendsPayload, error) {
	return nil, nil
}
func (batchTestProvider) QualitySessions(_ context.Context, _ api.QualityFilter) ([]api.QualitySession, error) {
	return nil, nil
}
func (batchTestProvider) AnnotationsForSession(_ context.Context, _ string) ([]schema.AnnotationSummary, error) {
	return nil, nil
}
func (batchTestProvider) ProjectFamiliarity(_ context.Context, _ schema.ProjectHash) (*schema.FamiliarityPayload, error) {
	return nil, nil
}
func (batchTestProvider) ChildSessionsForParent(_ context.Context, _ string) ([]schema.ChildSessionRef, error) {
	return nil, nil
}
func (batchTestProvider) ProjectSummaries(_ context.Context) (*codemap.ProjectSummariesResult, error) {
	return nil, nil
}
func (batchTestProvider) ResolveProject(_ context.Context, _ string) (*schema.ProjectResolutionPayload, error) {
	return nil, errors.New("not implemented")
}
func (batchTestProvider) MapGraph(_ context.Context, _ schema.ProjectHash, _ string) (*schema.MapGraphPayload, error) {
	return nil, nil
}
func (batchTestProvider) MapNodeDetail(_ context.Context, _ schema.ProjectHash, _ string) (*schema.MapNodeDetailPayload, error) {
	return nil, nil
}
func (batchTestProvider) ProjectTasks(_ context.Context, _ schema.ProjectHash, _ string) (*schema.ProjectTasksPayload, error) {
	return nil, nil
}
func (batchTestProvider) ReviewChanges(_ context.Context, _ schema.ProjectHash) (*schema.ReviewListPayload, error) {
	return nil, nil
}
func (batchTestProvider) ChangeDetail(_ context.Context, _ schema.ProjectHash, _ string) (*schema.ChangeDetailPayload, error) {
	return nil, nil
}

func (batchTestProvider) ChangeDiff(_ context.Context, _ schema.ProjectHash, _, _ string) (*schema.ChangeDiffPayload, error) {
	return nil, nil
}

func (batchTestProvider) Search(_ context.Context, _ string, _ int) (*schema.SearchPayload, error) {
	return nil, nil
}

var _ api.DataProvider = batchTestProvider{}

// newAnnotationTestServer creates a real HTTP server with real store + Hub wired up.
// Returns the base URL and the store (for seeding test data). Cleanup is registered
// via t.Cleanup.
func newAnnotationTestServer(t *testing.T) (string, *store.Store) {
	t.Helper()
	s := storetest.Open(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	hub := api.NewHub(batchTestProvider{})
	go hub.Run(ctx)

	srv := api.NewServer(api.ServerConfig{
		Port:  0,
		Store: s,
		Hub:   hub,
	})
	if err := srv.Listen(ctx); err != nil {
		t.Fatalf("newAnnotationTestServer: Listen: %v", err)
	}
	go srv.Serve(ctx) //nolint:errcheck

	return "http://" + srv.Addr().String(), s
}

// postBatch sends a POST /api/v1/annotations/batch and returns the response.
func postBatch(t *testing.T, baseURL string, req schema.BatchCreateAnnotationsRequest) *http.Response {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("postBatch: marshal: %v", err)
	}
	resp, err := http.Post(
		baseURL+defaults.RouteAnnotationsBatch.String(),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("postBatch: POST: %v", err)
	}
	return resp
}

// ---------------------------------------------------------------------------
// TUI test helpers.
// ---------------------------------------------------------------------------

// insertAnnotationTestSession inserts the minimum required rows (project, host_slug, session)
// so that annotation FK constraints succeed in tests. Uses a raw SQLite connection so this
// helper can be called from package api_test without exposing store internals.
func insertAnnotationTestSession(t *testing.T, dbPath, sessionID string) {
	t.Helper()

	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("insertAnnotationTestSession: OpenConn(%q): %v", dbPath, err)
	}
	defer conn.Close()

	// Enable FK enforcement for this connection.
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA foreign_keys = ON`, nil); err != nil {
		t.Fatalf("insertAnnotationTestSession: PRAGMA foreign_keys: %v", err)
	}

	const projHash = "annhandlrprojhash0000000000000000000000000000000000000000000000"
	// V23+: host_slugs uses opaque_id (64-char hex) as PK; sessions uses opaque_host_id FK.
	const hostSlugOpaqueID = "aa99bb88cc77dd66ee55ff4433221100aa99bb88cc77dd66ee55ff4433221100"
	const hostSlug = "annhandlerslug"

	if err := sqlitex.ExecuteTransient(conn,
		`INSERT OR IGNORE INTO projects (project_hash, canonical_cwd) VALUES (?, '/annhandler')`,
		&sqlitex.ExecOptions{Args: []any{projHash}}); err != nil {
		t.Fatalf("insertAnnotationTestSession: insert project: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT OR IGNORE INTO host_slugs (opaque_id, host_slug) VALUES (?, ?)`,
		&sqlitex.ExecOptions{Args: []any{hostSlugOpaqueID, hostSlug}}); err != nil {
		t.Fatalf("insertAnnotationTestSession: insert host_slug: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT OR IGNORE INTO sessions
		 (session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
		 VALUES (?, 'claude-code', 'claude-opus-4-6', ?, ?, 1, 2, 3, '/f', 'jsonl')`,
		&sqlitex.ExecOptions{Args: []any{sessionID, hostSlugOpaqueID, projHash}}); err != nil {
		t.Fatalf("insertAnnotationTestSession: insert session: %v", err)
	}
}

// openStoreWithSession creates a fully-migrated store with a pre-inserted test session.
// Returns the store and a cleanup function.
func openStoreWithSession(t *testing.T) (s *store.Store, sessionID string) {
	t.Helper()

	dbPath := storetest.CopyGoldenDB(t)
	sessionID = uuid.New().String()
	insertAnnotationTestSession(t, dbPath, sessionID)

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("openStoreWithSession: store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("openStoreWithSession: Close: %v", err)
		}
	})
	return s, sessionID
}

// startAnnotationServer starts a real api.Server with a fully-migrated SQLite store
// and returns the base URL and a cleanup function.
func startAnnotationServer(t *testing.T, s *store.Store) (baseURL string, cancel func()) {
	t.Helper()
	ctx, cancelFn := context.WithCancel(context.Background())

	hub := api.NewHub(nil)
	go hub.Run(ctx)

	srv := api.NewServer(api.ServerConfig{
		Port:  0,
		Store: s,
		Hub:   hub,
	})
	if err := srv.Listen(ctx); err != nil {
		cancelFn()
		t.Fatalf("startAnnotationServer: Listen: %v", err)
	}
	go srv.Serve(ctx) //nolint:errcheck // shutdown via cancel

	return "http://" + srv.Addr().String(), func() {
		cancelFn()
		srv.Shutdown(context.Background()) //nolint:errcheck
	}
}

// postAnnotation sends POST /api/v1/annotations with the given request and returns the response.
func postAnnotation(t *testing.T, baseURL string, req schema.CreateAnnotationRequest) *http.Response {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("postAnnotation: marshal: %v", err)
	}
	resp, err := http.Post(baseURL+"/api/v1/annotations", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("postAnnotation: POST: %v", err)
	}
	return resp
}

// TestAnnotationHandler_GetAssociationTarget verifies the mounted annotation
// endpoint exposes an association-target label for its owning session without
// recasting it as a session or entry annotation.
func TestAnnotationHandler_GetAssociationTarget(t *testing.T) {
	t.Parallel()
	baseURL, db := newAnnotationTestServer(t)
	sessionID := "association-annotation-session"
	storetest.SeedSession(t, db, sessionID)
	if err := db.UpsertSessionCommits(context.Background(), ingest.SessionID(sessionID), []ingest.CommitInfo{{
		Hash:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Message: "association annotation commit",
	}}); err != nil {
		t.Fatalf("UpsertSessionCommits: %v", err)
	}
	associations, err := db.ListCurrentSessionCommitAssociations(context.Background(), ingest.SessionID(sessionID))
	if err != nil {
		t.Fatalf("ListCurrentSessionCommitAssociations: %v", err)
	}
	if len(associations) != 1 {
		t.Fatalf("current associations = %d, want 1", len(associations))
	}
	annotationTypeID, err := db.GetAnnotationTypeID(context.Background(), testutil.TestTypeIDSessionOutcome)
	if err != nil {
		t.Fatalf("GetAnnotationTypeID: %v", err)
	}
	annotatorID, err := db.GetAnnotatorIDByName(context.Background(), "outcome-classifier")
	if err != nil {
		t.Fatalf("GetAnnotatorIDByName: %v", err)
	}
	if _, err := db.CreateAnnotation(context.Background(), store.CreateAnnotationParams{
		AssociationID:    &associations[0].ID,
		AnnotationTypeID: annotationTypeID,
		AnnotatorID:      annotatorID,
		Value:            "resolved",
	}); err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}

	response, err := http.Get(baseURL + "/api/v1/annotations?session_id=" + sessionID)
	if err != nil {
		t.Fatalf("GET annotations: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("GET annotations status = %d, want 200; body=%s", response.StatusCode, body)
	}
	var annotations []schema.AnnotationSummary
	if err := json.NewDecoder(response.Body).Decode(&annotations); err != nil {
		t.Fatalf("decode association annotation response: %v", err)
	}
	if len(annotations) != 1 {
		t.Fatalf("GET association annotations = %d, want 1", len(annotations))
	}
	annotation := annotations[0]
	if annotation.TargetKind != schema.TargetAssociation {
		t.Errorf("annotation target kind = %q, want %q", annotation.TargetKind, schema.TargetAssociation)
	}
	if annotation.TargetAssociationID == nil || *annotation.TargetAssociationID != associations[0].ID {
		t.Errorf("annotation target association ID = %v, want %q", annotation.TargetAssociationID, associations[0].ID)
	}
	if annotation.TargetSessionID != nil || annotation.TargetEntryIndex != nil || annotation.TargetAnnotID != nil || annotation.TargetProjectHash != nil {
		t.Errorf("association annotation populated another target arm: %+v", annotation)
	}
}

// ---------------------------------------------------------------------------
// Batch annotation tests.
// ---------------------------------------------------------------------------

// TestBatchCreateAnnotations_AllValid verifies that a valid batch commits atomically
// and returns 201 with IDs in request order.
func TestBatchCreateAnnotations_AllValid(t *testing.T) {
	t.Parallel()
	baseURL, s := newAnnotationTestServer(t)
	sid := "test-session-batch-001"
	storetest.SeedSession(t, s, sid)

	req := schema.BatchCreateAnnotationsRequest{
		Annotations: []schema.CreateAnnotationRequest{
			{
				SessionID:     sid,
				TypeID:        "quality.session_outcome",
				Value:         "resolved",
				IsPrimary:     true,
				AnnotatorName: "human-web",
			},
			{
				SessionID:     sid,
				TypeID:        "quality.user_frustration",
				Value:         "not_detected",
				IsPrimary:     true,
				AnnotatorName: "human-web",
			},
		},
	}

	resp := postBatch(t, baseURL, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var got schema.BatchCreateAnnotationsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.IDs) != 2 {
		t.Errorf("expected 2 IDs, got %d: %v", len(got.IDs), got.IDs)
	}
	for i, id := range got.IDs {
		if id == "" {
			t.Errorf("IDs[%d] is empty", i)
		}
	}
}

// TestBatchCreateAnnotations_EntryLevel verifies entry-level targeting
// (SessionID + TargetEntryIndex) routes to annotation_target_entry.
func TestBatchCreateAnnotations_EntryLevel(t *testing.T) {
	t.Parallel()
	baseURL, s := newAnnotationTestServer(t)
	sid := "test-session-entry-001"
	entryIdx := 3
	storetest.SeedSession(t, s, sid)
	storetest.SeedSessionEntry(t, s, sid, entryIdx)
	req := schema.BatchCreateAnnotationsRequest{
		Annotations: []schema.CreateAnnotationRequest{
			{
				SessionID:        sid,
				TypeID:           "quality.frustration_signal",
				Value:            "detected",
				IsPrimary:        false,
				AnnotatorName:    "human-web",
				TargetEntryIndex: &entryIdx,
			},
		},
	}

	resp := postBatch(t, baseURL, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for entry-level annotation, got %d", resp.StatusCode)
	}

	var got schema.BatchCreateAnnotationsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.IDs) != 1 || got.IDs[0] == "" {
		t.Errorf("expected 1 non-empty ID, got %v", got.IDs)
	}
}

// TestBatchCreateAnnotations_InvalidRollsBack verifies the all-or-nothing semantics:
// if one annotation has an invalid value, nothing is committed and 400 is returned
// with the zero-based index of the failing annotation.
func TestBatchCreateAnnotations_InvalidRollsBack(t *testing.T) {
	t.Parallel()
	baseURL, s := newAnnotationTestServer(t)
	sid := "test-session-batch-002"
	storetest.SeedSession(t, s, sid)

	req := schema.BatchCreateAnnotationsRequest{
		Annotations: []schema.CreateAnnotationRequest{
			{
				SessionID:     sid,
				TypeID:        "quality.session_outcome",
				Value:         "resolved",
				IsPrimary:     true,
				AnnotatorName: "human-web",
			},
			{
				SessionID:     sid,
				TypeID:        "quality.session_outcome",
				Value:         "NOT_A_VALID_VALUE",
				IsPrimary:     false,
				AnnotatorName: "human-web",
			},
		},
	}

	resp := postBatch(t, baseURL, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on validation failure, got %d", resp.StatusCode)
	}

	var got schema.BatchCreateAnnotationsErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if got.FailingIndex != 1 {
		t.Errorf("failingIndex = %d, want 1", got.FailingIndex)
	}
	if got.Error == "" {
		t.Error("expected non-empty error message")
	}
}

// TestBatchCreateAnnotations_EmptyBatch verifies that an empty annotation slice returns 400.
func TestBatchCreateAnnotations_EmptyBatch(t *testing.T) {
	t.Parallel()
	baseURL, _ := newAnnotationTestServer(t)

	req := schema.BatchCreateAnnotationsRequest{Annotations: []schema.CreateAnnotationRequest{}}
	resp := postBatch(t, baseURL, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty batch, got %d", resp.StatusCode)
	}
}

// TestBatchCreateAnnotations_MissingTypeID verifies missing typeId returns 400
// with failingIndex pointing to the offending annotation (index 0 here).
func TestBatchCreateAnnotations_MissingTypeID(t *testing.T) {
	t.Parallel()
	baseURL, _ := newAnnotationTestServer(t)

	req := schema.BatchCreateAnnotationsRequest{
		Annotations: []schema.CreateAnnotationRequest{
			{
				SessionID:     "test-session-batch-003",
				TypeID:        "", // missing
				Value:         "resolved",
				IsPrimary:     true,
				AnnotatorName: "human-web",
			},
		},
	}

	resp := postBatch(t, baseURL, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing typeId, got %d", resp.StatusCode)
	}

	var got schema.BatchCreateAnnotationsErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if got.FailingIndex != 0 {
		t.Errorf("failingIndex = %d, want 0", got.FailingIndex)
	}
}

// ---------------------------------------------------------------------------
// TUI-style HTTP client tests (plain HTTP, no WebSocket)
// ---------------------------------------------------------------------------

// TestAnnotationHandler_TUIClient_CreateSingle verifies a TUI HTTP client can
// create a session-level annotation via POST /api/v1/annotations (no WebSocket).
// This is the path taken by the TUI 'c' key commit for a single pending annotation.
func TestAnnotationHandler_TUIClient_CreateSingle(t *testing.T) {
	t.Parallel()

	s, sessionID := openStoreWithSession(t)
	baseURL, cancel := startAnnotationServer(t, s)
	defer cancel()

	resp := postAnnotation(t, baseURL, schema.CreateAnnotationRequest{
		SessionID:     sessionID,
		TypeID:        testutil.TestTypeIDSessionOutcome,
		Value:         "resolved",
		AnnotatorName: "human-web",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Errorf("POST /api/v1/annotations: status = %d, want %d; body: %s", resp.StatusCode, http.StatusCreated, bodyBytes)
	}

	var result schema.CreateAnnotationResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.ID == "" {
		t.Error("response ID is empty, want non-empty annotation UUID")
	}
}

// TestAnnotationHandler_TUIClient_BatchStyle verifies the TUI commit pattern:
// creating multiple annotations in sequence via individual POST calls (one per pending).
// This mirrors what the TUI 'c' key does: iterate pending list and POST each one.
func TestAnnotationHandler_TUIClient_BatchStyle(t *testing.T) {
	t.Parallel()

	s, sessionID := openStoreWithSession(t)
	baseURL, cancel := startAnnotationServer(t, s)
	defer cancel()

	// Simulate TUI committing 3 pending annotations in sequence.
	toCommit := []schema.CreateAnnotationRequest{
		{SessionID: sessionID, TypeID: testutil.TestTypeIDSessionOutcome, Value: "resolved", AnnotatorName: "human-web"},
		{SessionID: sessionID, TypeID: testutil.TestTypeIDUserFrustration, Value: "detected", AnnotatorName: "human-web"},
		{SessionID: sessionID, TypeID: testutil.TestTypeIDSessionScope, Value: "feature", AnnotatorName: "human-web"},
	}

	var createdIDs []string
	for i, req := range toCommit {
		resp := postAnnotation(t, baseURL, req)
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusCreated {
			t.Errorf("annotation %d: status = %d, want %d", i, resp.StatusCode, http.StatusCreated)
			continue
		}

		var result schema.CreateAnnotationResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("annotation %d: decode response: %v", i, err)
		}
		if result.ID == "" {
			t.Errorf("annotation %d: response ID is empty", i)
		}
		createdIDs = append(createdIDs, result.ID)
	}

	if len(createdIDs) != len(toCommit) {
		t.Errorf("created %d annotations, want %d", len(createdIDs), len(toCommit))
	}
}

// TestAnnotationHandler_TUIClient_MissingSessionID returns 400 for empty sessionId.
// Verifies the handler validates required fields the same way for TUI and web clients.
func TestAnnotationHandler_TUIClient_MissingSessionID(t *testing.T) {
	t.Parallel()

	s := storetest.Open(t)
	baseURL, cancel := startAnnotationServer(t, s)
	defer cancel()

	resp := postAnnotation(t, baseURL, schema.CreateAnnotationRequest{
		TypeID: testutil.TestTypeIDSessionOutcome,
		Value:  "resolved",
		// SessionID intentionally omitted
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (bad request for missing sessionId)", resp.StatusCode, http.StatusBadRequest)
	}
}

// TestAnnotationHandler_TUIClient_InvalidAnnotationType returns 400 for unknown type.
// Verifies the TUI gets a useful error when committing with a stale or invalid typeId.
func TestAnnotationHandler_TUIClient_InvalidAnnotationType(t *testing.T) {
	t.Parallel()

	s, sessionID := openStoreWithSession(t)
	baseURL, cancel := startAnnotationServer(t, s)
	defer cancel()

	resp := postAnnotation(t, baseURL, schema.CreateAnnotationRequest{
		SessionID: sessionID,
		TypeID:    "nonexistent.type_id",
		Value:     "somevalue",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (bad request for unknown type)", resp.StatusCode, http.StatusBadRequest)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/annotation-types — TUI uses this to populate the picker
// ---------------------------------------------------------------------------

// TestAnnotationHandler_TUIClient_GetAnnotationTypes verifies the TUI can fetch
// annotation types for the picker via plain HTTP GET (no WebSocket required).
func TestAnnotationHandler_TUIClient_GetAnnotationTypes(t *testing.T) {
	t.Parallel()

	s := storetest.Open(t)
	baseURL, cancel := startAnnotationServer(t, s)
	defer cancel()

	resp, err := http.Get(baseURL + "/api/v1/annotation-types")
	if err != nil {
		t.Fatalf("GET /api/v1/annotation-types: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var types []schema.AnnotationTypeSummary
	if err := json.NewDecoder(resp.Body).Decode(&types); err != nil {
		t.Fatalf("decode annotation types: %v", err)
	}
	// Seeded types from V13+V18 should be present.
	if len(types) == 0 {
		t.Error("expected at least one annotation type from seed data, got 0")
	}

	// Verify at least one known seeded type is present.
	var foundOutcome bool
	for _, tp := range types {
		if tp.TypeID == testutil.TestTypeIDSessionOutcome {
			foundOutcome = true
			break
		}
	}
	if !foundOutcome {
		t.Errorf("seeded type %q not found in GET /api/v1/annotation-types response", testutil.TestTypeIDSessionOutcome)
	}
}

// TestAnnotationHandler_NoStore_Returns503 verifies the handler returns 503 when
// the store is not configured (e.g., TUI running without a database).
func TestAnnotationHandler_NoStore_Returns503(t *testing.T) {
	t.Parallel()

	// Server with no store configured.
	srv := api.NewServer(api.ServerConfig{Port: 0})
	if err := srv.Listen(t.Context()); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go srv.Serve(t.Context()) //nolint:errcheck

	baseURL := "http://" + srv.Addr().String()

	resp, err := http.Get(baseURL + "/api/v1/annotations?session_id=any")
	if err != nil {
		t.Fatalf("GET /api/v1/annotations: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d (service unavailable without store)", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

// TestAnnotationHandler_GetAnnotations_MissingSessionID verifies 400 for missing session_id query param.
func TestAnnotationHandler_GetAnnotations_MissingSessionID(t *testing.T) {
	t.Parallel()

	s := storetest.Open(t)
	baseURL, cancel := startAnnotationServer(t, s)
	defer cancel()

	resp, err := http.Get(baseURL + "/api/v1/annotations")
	if err != nil {
		t.Fatalf("GET /api/v1/annotations (no session_id): %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (bad request for missing session_id)", resp.StatusCode, http.StatusBadRequest)
	}
}
