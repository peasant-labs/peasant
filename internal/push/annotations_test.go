package push_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// testAPIKey is the shared API key fixture for push_test client construction. It
// formerly lived in client_test.go before the client moved to internal/village;
// the annotation push tests still build a village client against an
// httptest server, so it is retained here for the push_test package.
const testAPIKey = "test-api-key-abc123"

// stubAnnotationStore is a minimal in-memory implementation of ingest.AnnotationQueryStore
// for testing PushAnnotations without a real SQLite database.
type stubAnnotationStore struct {
	rows          []ingest.AnnotationPushRow
	err           error
	superseded    []ingest.AnnotationPushRow // returned by ListSupersededAnnotations
	supersededErr error
}

// Compile-time guard: stubAnnotationStore must satisfy ingest.AnnotationQueryStore.
var _ ingest.AnnotationQueryStore = (*stubAnnotationStore)(nil)

func (s *stubAnnotationStore) ListSystemAnnotations(_ context.Context) ([]ingest.AnnotationPushRow, error) {
	return s.rows, s.err
}

func (s *stubAnnotationStore) ListSupersededAnnotations(_ context.Context) ([]ingest.AnnotationPushRow, error) {
	return s.superseded, s.supersededErr
}

// newSessionAnnotationRow returns a session-level AnnotationPushRow for testing.
func newSessionAnnotationRow(sessionID, typeID, value string) ingest.AnnotationPushRow {
	sid := sessionID
	return ingest.AnnotationPushRow{
		ID:            "annotation-" + typeID,
		TargetKind:    schema.TargetSession,
		SessionID:     &sid,
		TypeID:        typeID,
		Value:         value,
		IsPrimary:     true,
		AnnotatorName: "system",
	}
}

// TestPushAnnotations_DryRun verifies that dry-run mode returns the correct
// annotation count without making any HTTP calls.
func TestPushAnnotations_DryRun(t *testing.T) {
	t.Parallel()
	store := &stubAnnotationStore{
		rows: []ingest.AnnotationPushRow{
			newSessionAnnotationRow("session-1", "retry_loops", "2"),
			newSessionAnnotationRow("session-2", "outcome", "success"),
			newSessionAnnotationRow("session-3", "retry_loops", "0"),
		},
	}

	// Use a mock server that should NOT be called during dry run.
	httpCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/manifest" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{})
			return
		}
		httpCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	summary, err := push.PushAnnotations(context.Background(), client, store, true /* dryRun */)
	if err != nil {
		t.Fatalf("PushAnnotations dry-run: unexpected error: %v", err)
	}
	if summary == nil {
		t.Fatal("PushAnnotations dry-run: expected non-nil summary")
	}
	if summary.Total != 3 {
		t.Errorf("dry-run Total: want 3, got %d", summary.Total)
	}
	if httpCalled {
		t.Error("dry-run should not make HTTP calls, but the server was contacted")
	}
}

// TestPushAnnotations_HappyPath verifies that PushAnnotations sends the correct
// request to the village and returns an accurate summary on success.
func TestPushAnnotations_HappyPath(t *testing.T) {
	t.Parallel()
	sessionID := "session-abc"
	store := &stubAnnotationStore{
		rows: []ingest.AnnotationPushRow{
			newSessionAnnotationRow(sessionID, "retry_loops", "1"),
			newSessionAnnotationRow(sessionID, "outcome", "success"),
		},
	}

	// Track whether the server received the correct request.
	var receivedReq schema.AnnotationPushRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/manifest" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{})
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("method: got %q, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1/annotations" {
			t.Errorf("path: got %q, want /api/v1/annotations", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&receivedReq); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		resp := schema.AnnotationPushResponse{
			Created: 2,
			Updated: 0,
			Skipped: 0,
			Errors:  0,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	summary, err := push.PushAnnotations(context.Background(), client, store, false /* dryRun */)
	if err != nil {
		t.Fatalf("PushAnnotations: unexpected error: %v", err)
	}
	if summary == nil {
		t.Fatal("PushAnnotations: expected non-nil summary")
	}

	// Verify summary reflects server response.
	if summary.Total != 2 {
		t.Errorf("Total: want 2, got %d", summary.Total)
	}
	if summary.Created != 2 {
		t.Errorf("Created: want 2, got %d", summary.Created)
	}
	if summary.Errors != 0 {
		t.Errorf("Errors: want 0, got %d", summary.Errors)
	}

	// Verify the request sent to the server.
	if len(receivedReq.Annotations) != 2 {
		t.Errorf("request annotations count: want 2, got %d", len(receivedReq.Annotations))
	}

	// Verify content hashes were set (non-empty).
	for i, item := range receivedReq.Annotations {
		if item.ContentHash == "" {
			t.Errorf("annotations[%d].ContentHash is empty — should be computed before send", i)
		}
	}
}

// TestPushAnnotationsSelected_FiltersByID verifies that when a non-empty
// selection is supplied, only the named annotations reach the village. This is
// the backend counterpart of the share wizard's label-selection step.
func TestPushAnnotationsSelected_FiltersByID(t *testing.T) {
	t.Parallel()
	sessionID := "session-sel"
	store := &stubAnnotationStore{
		rows: []ingest.AnnotationPushRow{
			newSessionAnnotationRow(sessionID, "retry_loops", "1"),   // ID: annotation-retry_loops
			newSessionAnnotationRow(sessionID, "outcome", "success"), // ID: annotation-outcome (excluded)
			newSessionAnnotationRow(sessionID, "tool_calls", "5"),    // ID: annotation-tool_calls
		},
	}

	var receivedReq schema.AnnotationPushRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/manifest" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{})
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&receivedReq); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(schema.AnnotationPushResponse{Created: 2})
	}))
	defer srv.Close()

	selection := push.AnnotationSelection{
		IDs: map[string]bool{
			"annotation-retry_loops": true,
			"annotation-tool_calls":  true,
		},
	}

	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	summary, err := push.PushAnnotationsSelected(context.Background(), client, store, selection, false, push.DefaultConcurrency)
	if err != nil {
		t.Fatalf("PushAnnotationsSelected: unexpected error: %v", err)
	}
	// Only the two selected annotations should be considered for push.
	if summary.Total != 2 {
		t.Errorf("Total: want 2 (the selected subset), got %d", summary.Total)
	}
	if len(receivedReq.Annotations) != 2 {
		t.Fatalf("request annotations count: want 2, got %d", len(receivedReq.Annotations))
	}
	// The excluded "outcome" annotation must not be on the wire.
	for _, item := range receivedReq.Annotations {
		if item.TypeID == "outcome" {
			t.Errorf("excluded annotation (typeId=outcome) was published")
		}
	}
}

// TestPushAnnotationsSelected_FiltersBySession verifies the session-ID gate:
// when SessionIDs is set, only annotations whose target session is selected are
// pushed; annotations not tied to a session (nil SessionID) are unaffected.
func TestPushAnnotationsSelected_FiltersBySession(t *testing.T) {
	t.Parallel()
	ph := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	projRow := ingest.AnnotationPushRow{
		ID:            "annotation-project",
		TargetKind:    schema.TargetProject,
		SessionID:     nil, // not session-scoped → passes the session gate
		ProjectHash:   &ph,
		TypeID:        "metadata.session_scope",
		Value:         "x",
		IsPrimary:     true,
		AnnotatorName: "system",
	}
	store := &stubAnnotationStore{
		rows: []ingest.AnnotationPushRow{
			newSessionAnnotationRow("session-A", "outcome", "ok"),    // selected session
			newSessionAnnotationRow("session-A", "retry_loops", "1"), // selected session
			newSessionAnnotationRow("session-B", "outcome", "no"),    // NOT selected → excluded
			projRow, // nil session → kept
		},
	}

	selection := push.AnnotationSelection{
		SessionIDs: map[string]bool{"session-A": true},
	}

	client := village.NewVillageClient("http://unused.invalid", testAPIKey, nil)
	// dry-run: no HTTP; summary.Total reflects the filtered set.
	summary, err := push.PushAnnotationsSelected(context.Background(), client, store, selection, true, push.DefaultConcurrency)
	if err != nil {
		t.Fatalf("PushAnnotationsSelected: unexpected error: %v", err)
	}
	// 2 from session-A + 1 project-level (nil session) = 3; session-B excluded.
	if summary.Total != 3 {
		t.Errorf("Total: want 3 (2 session-A + 1 non-session), got %d", summary.Total)
	}
}

// TestPushAnnotationsSelected_AssociationTarget verifies the actual annotation
// push path preserves the opaque association target and applies session
// selection through the association ledger's local session context.
func TestPushAnnotationsSelected_AssociationTarget(t *testing.T) {
	t.Parallel()
	associationID, err := schema.NewAssociationID("assoc-00000000-0000-0000-0000-000000000003")
	if err != nil {
		t.Fatalf("NewAssociationID: %v", err)
	}
	sessionID := "association-session"
	associationSessionID := ingest.SessionID(sessionID)
	store := &stubAnnotationStore{rows: []ingest.AnnotationPushRow{{
		ID:                   "annotation-association",
		TargetKind:           schema.TargetAssociation,
		TargetAssociationID:  &associationID,
		AssociationSessionID: &associationSessionID,
		TypeID:               "quality.session_outcome",
		Value:                "resolved",
		IsPrimary:            true,
		AnnotatorName:        "system",
	}}}

	var received schema.AnnotationPushRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/manifest" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{})
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode annotation push request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(schema.AnnotationPushResponse{Created: 1})
	}))
	defer server.Close()

	client := village.NewVillageClient(server.URL, testAPIKey, nil)
	summary, err := push.PushAnnotationsSelected(context.Background(), client, store, push.AnnotationSelection{
		SessionIDs: map[string]bool{sessionID: true},
	}, false, push.DefaultConcurrency)
	if err != nil {
		t.Fatalf("PushAnnotationsSelected: %v", err)
	}
	if summary.Total != 1 || len(received.Annotations) != 1 {
		t.Fatalf("association annotation summary total=%d wire rows=%d, want 1", summary.Total, len(received.Annotations))
	}
	item := received.Annotations[0]
	if item.TargetKind != schema.TargetAssociation {
		t.Errorf("association annotation target kind = %q, want %q", item.TargetKind, schema.TargetAssociation)
	}
	if item.TargetAssociationID == nil || *item.TargetAssociationID != associationID {
		t.Errorf("association annotation target ID = %v, want %q", item.TargetAssociationID, associationID)
	}
	if item.SessionID != nil || item.EntryTarget != nil || item.AnnotationID != nil || item.ProjectHash != nil {
		t.Errorf("association annotation populated another wire target arm: %+v", item)
	}
}

// TestPushAnnotationsSelected_InvalidAssociationTargetStopsBeforeNetwork proves
// a malformed association target is stopped at the local schema boundary, before
// a manifest or annotation request can mutate remote state — and is reported
// with a recovery rather than aborting the whole run.
//
// Aborting is not an option here even though the row is genuinely broken. Its
// state is permanent: the same row fails identically on every attempt, so a
// fatal error walls the user out of publishing ANY annotation, in ANY
// repository, on every push, until they find and delete it unaided. The
// annotation is left behind, named, and given the steps that clear it.
func TestPushAnnotationsSelected_InvalidAssociationTargetStopsBeforeNetwork(t *testing.T) {
	t.Parallel()
	serverCalls := 0
	associationSession := ingest.SessionID("session-associated")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverCalls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	store := &stubAnnotationStore{rows: []ingest.AnnotationPushRow{{
		ID:            "annotation-invalid-association",
		TargetKind:    schema.TargetAssociation,
		TypeID:        "quality.session_outcome",
		Value:         "resolved",
		IsPrimary:     true,
		AnnotatorName: "system",
		// The association still names its local session even though its wire ID is
		// missing, so recovery can remain session-scoped instead of deleting every
		// annotation by this annotator.
		AssociationSessionID: &associationSession,
	}}}
	client := village.NewVillageClient(server.URL, testAPIKey, nil)
	summary, err := push.PushAnnotationsSelected(context.Background(), client, store, push.AnnotationSelection{}, false, push.DefaultConcurrency)
	if err != nil {
		t.Fatalf("PushAnnotationsSelected returned a fatal error for one unpublishable row: %v", err)
	}
	if serverCalls != 0 {
		t.Errorf("invalid association target made %d network calls, want 0", serverCalls)
	}
	if summary.Total != 0 {
		t.Errorf("summary.Total = %d, want 0: an unpublishable row is never a push candidate", summary.Total)
	}
	if len(summary.Unpublishable) != 1 {
		t.Fatalf("summary.Unpublishable = %d entries, want 1: the row must be reported, not silently dropped", len(summary.Unpublishable))
	}
	reported := summary.Unpublishable[0]
	if reported.ID != "annotation-invalid-association" {
		t.Errorf("reported annotation ID = %q, want the store ID so a user can find it", reported.ID)
	}
	if reported.AnnotatorName != "system" {
		t.Errorf("reported annotator = %q, want the annotator name: it is the only key 'peasant annotate prune' takes", reported.AnnotatorName)
	}
	if reported.Reason == "" {
		t.Error("reported annotation carries no reason, so the report does not say what is wrong with it")
	}
	if reported.SessionID != string(associationSession) {
		t.Errorf("reported session = %q, want association session %q so cleanup stays scoped", reported.SessionID, associationSession)
	}
	if recovery := reported.Recovery(); !strings.Contains(recovery, "peasant annotate prune") {
		t.Errorf("recovery = %q, want the command that actually clears the annotation", recovery)
	} else if strings.Contains(recovery, "peasant ingest --force --session") {
		t.Errorf("recovery = %q, but re-ingest cannot reconstruct a target row that no longer records its session or entry", recovery)
	} else if !strings.Contains(recovery, "--dry-run") {
		t.Errorf("recovery = %q, want a preview before the annotator-wide cleanup", recovery)
	} else if !strings.Contains(recovery, "--session") || !strings.Contains(recovery, "peasant annotate list") {
		t.Errorf("recovery = %q, want session-scoped inspection and cleanup", recovery)
	} else if strings.Contains(recovery, "every listed annotation") {
		t.Errorf("recovery = %q, but annotate prune --dry-run prints a count, not a list", recovery)
	}
}

// TestPushAnnotationsSelected_UnpublishableRowDoesNotBlockValidRows proves one
// permanently malformed local row cannot wall every healthy annotation off from
// the village. The invalid row is reported and left untouched; the valid row
// still follows the normal manifest and upload path.
func TestPushAnnotationsSelected_UnpublishableRowDoesNotBlockValidRows(t *testing.T) {
	t.Parallel()
	fv := &fakeVillage{}
	server := httptest.NewServer(fv.handler())
	defer server.Close()

	store := &stubAnnotationStore{rows: []ingest.AnnotationPushRow{
		{
			ID:            "annotation-invalid-association",
			TargetKind:    schema.TargetAssociation,
			TypeID:        "quality.session_outcome",
			Value:         "resolved",
			IsPrimary:     true,
			AnnotatorName: "system",
		},
		newSessionAnnotationRow("session-valid", "outcome", "success"),
	}}
	client := village.NewVillageClient(server.URL, testAPIKey, nil)
	summary, err := push.PushAnnotationsSelected(context.Background(), client, store, push.AnnotationSelection{}, false, 1)
	if err != nil {
		t.Fatalf("push with one valid and one unpublishable annotation: %v", err)
	}
	if summary.Total != 1 || summary.Created != 1 || len(summary.Unpublishable) != 1 {
		t.Fatalf("summary = %+v, want one created annotation and one separately reported unpublishable annotation", summary)
	}
	_, posted, _ := fv.snapshot()
	if len(posted) != 1 {
		t.Fatalf("posted annotations = %d, want the healthy row to reach the village", len(posted))
	}
}

// TestPushAnnotationsSelected_RepositoryScopeWithholdsUnattributableMalformedRow
// keeps failure reporting inside the same repository boundary as publishing. A
// malformed row with no target cannot be attributed to this repository and must
// not leak into a per-repository hook's output merely because validation runs
// before content hashing. The repository identity alone must fail closed even if
// a caller omitted the selected-session map.
func TestPushAnnotationsSelected_RepositoryScopeWithholdsUnattributableMalformedRow(t *testing.T) {
	t.Parallel()
	store := &stubAnnotationStore{rows: []ingest.AnnotationPushRow{{
		ID:            "annotation-outside-scope",
		TargetKind:    schema.TargetAssociation,
		TypeID:        "quality.session_outcome",
		Value:         "resolved",
		AnnotatorName: "system",
	}}}
	selection := push.AnnotationSelection{
		RepositoryProjectHashes: map[string]bool{"project-in-scope": true},
	}
	summary, err := push.PushAnnotationsSelected(context.Background(), nil, store, selection, true, 1)
	if err != nil {
		t.Fatalf("repository-scoped dry run: %v", err)
	}
	if summary.Total != 0 || len(summary.Unpublishable) != 0 {
		t.Fatalf("out-of-scope malformed row leaked into the scoped result: %+v", summary)
	}
}

// TestPushAnnotationsSelected_LabelAndSessionAreANDed verifies that the label
// gate (IDs/hashes) and the session gate (SessionIDs) compose with AND: an
// annotation must satisfy BOTH. Covers the disagreement cases (pass-label /
// fail-session and fail-label / pass-session must both be excluded), including
// malformed active rows and superseded retractions.
func TestPushAnnotationsSelected_LabelAndSessionAreANDed(t *testing.T) {
	t.Parallel()
	sessionA := "session-A"
	sessionB := "session-B"
	associationSessionA := ingest.SessionID(sessionA)
	const (
		supersededHashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		supersededHashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	hashA := supersededHashA
	hashB := supersededHashB
	store := &stubAnnotationStore{
		rows: []ingest.AnnotationPushRow{
			newSessionAnnotationRow("session-A", "outcome", "ok"),    // label✅ session✅ → kept
			newSessionAnnotationRow("session-A", "retry_loops", "1"), // label❌ session✅ → dropped
			newSessionAnnotationRow("session-B", "tool_calls", "5"),  // label✅ session❌ → dropped
			{
				ID:                   "annotation-malformed-but-unselected",
				TargetKind:           schema.TargetAssociation,
				AssociationSessionID: &associationSessionA,
				TypeID:               "outcome",
				Value:                "broken",
				AnnotatorName:        "system",
			},
		},
		superseded: []ingest.AnnotationPushRow{
			{ID: "superseded-A", TargetKind: schema.TargetEntry, SessionID: &sessionA, TypeID: "outcome", Value: "old", AnnotatorName: "system", ContentHash: &hashA},
			{ID: "superseded-B", TargetKind: schema.TargetEntry, SessionID: &sessionB, TypeID: "outcome", Value: "old", AnnotatorName: "system", ContentHash: &hashB},
		},
	}

	var receivedReq schema.AnnotationPushRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/manifest" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{Hashes: []string{supersededHashA, supersededHashB}})
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&receivedReq); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(schema.AnnotationPushResponse{Created: 1})
	}))
	defer srv.Close()

	selection := push.AnnotationSelection{
		// Label selects outcome + tool_calls (NOT retry_loops)...
		IDs: map[string]bool{"annotation-outcome": true, "annotation-tool_calls": true},
		// ...but session selects only session-A (NOT session-B).
		SessionIDs: map[string]bool{"session-A": true},
	}

	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	summary, err := push.PushAnnotationsSelected(context.Background(), client, store, selection, false, push.DefaultConcurrency)
	if err != nil {
		t.Fatalf("PushAnnotationsSelected: unexpected error: %v", err)
	}
	// Only "outcome" satisfies BOTH gates.
	if summary.Total != 1 {
		t.Errorf("Total: want 1 (only label∧session), got %d", summary.Total)
	}
	if len(summary.Unpublishable) != 0 {
		t.Errorf("an explicitly unselected malformed row leaked into the result: %+v", summary.Unpublishable)
	}
	if len(receivedReq.Annotations) != 1 {
		t.Fatalf("request annotations count: want 1, got %d", len(receivedReq.Annotations))
	}
	if got := receivedReq.Annotations[0].TypeID; got != "outcome" {
		t.Errorf("survivor TypeID: want outcome, got %q", got)
	}
	if len(receivedReq.Retractions) != 1 || receivedReq.Retractions[0] != supersededHashA || summary.Retracted != 1 {
		t.Errorf("repository/session-scoped retractions = %v (summary=%d), want only %s; retracting %s would mutate another scope",
			receivedReq.Retractions, summary.Retracted, supersededHashA, supersededHashB)
	}
}

// TestPushAnnotationsSelected_FiltersByContentHash verifies the selection also
// matches on the freshly computed content hash, not just the store ID.
func TestPushAnnotationsSelected_FiltersByContentHash(t *testing.T) {
	t.Parallel()
	sessionID := "session-hash"
	rows := []ingest.AnnotationPushRow{
		newSessionAnnotationRow(sessionID, "retry_loops", "1"),
		newSessionAnnotationRow(sessionID, "outcome", "success"),
	}
	store := &stubAnnotationStore{rows: rows}

	// Compute the content hash of the first row the same way PushAnnotations does,
	// so we can select it by hash.
	wantHash := annotationPushItemHash(rows[0])

	var receivedReq schema.AnnotationPushRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/manifest" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{})
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&receivedReq); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(schema.AnnotationPushResponse{Created: 1})
	}))
	defer srv.Close()

	selection := push.AnnotationSelection{
		ContentHashes: map[string]bool{wantHash: true},
	}

	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	summary, err := push.PushAnnotationsSelected(context.Background(), client, store, selection, false, push.DefaultConcurrency)
	if err != nil {
		t.Fatalf("PushAnnotationsSelected: unexpected error: %v", err)
	}
	if summary.Total != 1 {
		t.Errorf("Total: want 1 (matched by content hash), got %d", summary.Total)
	}
	if len(receivedReq.Annotations) != 1 {
		t.Fatalf("request annotations count: want 1, got %d", len(receivedReq.Annotations))
	}
	if receivedReq.Annotations[0].TypeID != "retry_loops" {
		t.Errorf("published wrong annotation: got typeId=%q, want retry_loops", receivedReq.Annotations[0].TypeID)
	}
}

// TestPushAnnotationsSelected_EmptySelectionPushesAll verifies the back-compat
// contract: an empty selection behaves exactly like PushAnnotations (all).
func TestPushAnnotationsSelected_EmptySelectionPushesAll(t *testing.T) {
	t.Parallel()
	sessionID := "session-all"
	store := &stubAnnotationStore{
		rows: []ingest.AnnotationPushRow{
			newSessionAnnotationRow(sessionID, "retry_loops", "1"),
			newSessionAnnotationRow(sessionID, "outcome", "success"),
		},
	}

	var receivedReq schema.AnnotationPushRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/manifest" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&receivedReq)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(schema.AnnotationPushResponse{Created: 2})
	}))
	defer srv.Close()

	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	summary, err := push.PushAnnotationsSelected(context.Background(), client, store, push.AnnotationSelection{}, false, push.DefaultConcurrency)
	if err != nil {
		t.Fatalf("PushAnnotationsSelected: unexpected error: %v", err)
	}
	if summary.Total != 2 {
		t.Errorf("Total: want 2 (empty selection = all), got %d", summary.Total)
	}
	if len(receivedReq.Annotations) != 2 {
		t.Errorf("request annotations count: want 2, got %d", len(receivedReq.Annotations))
	}
}

// annotationPushItemHash recomputes the content hash for a session-level row the
// same way PushAnnotations does, so a test can target it via content-hash
// selection. Mirrors annotationRowToPushItem for the session target arm.
func annotationPushItemHash(row ingest.AnnotationPushRow) string {
	item := schema.AnnotationPushItem{
		TargetKind:    row.TargetKind,
		SessionID:     row.SessionID,
		TypeID:        row.TypeID,
		Value:         row.Value,
		IsPrimary:     row.IsPrimary,
		Confidence:    row.Confidence,
		Reason:        row.Reason,
		AnnotatorName: row.AnnotatorName,
		Provenance:    row.Provenance,
	}
	return item.ComputeContentHash()
}

// TestPushAnnotations_PartialFailure verifies that per-item failures reported
// by the server are reflected in summary.Errors without returning a fatal error.
func TestPushAnnotations_PartialFailure(t *testing.T) {
	t.Parallel()
	sessionID := "session-xyz"
	store := &stubAnnotationStore{
		rows: []ingest.AnnotationPushRow{
			newSessionAnnotationRow(sessionID, "retry_loops", "3"),
			newSessionAnnotationRow(sessionID, "outcome", "error"),
			newSessionAnnotationRow(sessionID, "tool_calls", "5"),
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/manifest" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Server reports 2 created, 1 error (per-item failure — not a fatal HTTP error).
		resp := schema.AnnotationPushResponse{
			Created: 2,
			Updated: 0,
			Skipped: 0,
			Errors:  1,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	summary, err := push.PushAnnotations(context.Background(), client, store, false)
	if err != nil {
		t.Fatalf("PushAnnotations: unexpected fatal error: %v (per-item errors should be in summary)", err)
	}
	if summary.Total != 3 {
		t.Errorf("Total: want 3, got %d", summary.Total)
	}
	if summary.Created != 2 {
		t.Errorf("Created: want 2, got %d", summary.Created)
	}
	if summary.Errors != 1 {
		t.Errorf("Errors: want 1, got %d", summary.Errors)
	}
}

// TestPushAnnotations_EmptyStore verifies that PushAnnotations returns a zero
// summary and no error when there are no annotations to push.
func TestPushAnnotations_EmptyStore(t *testing.T) {
	t.Parallel()
	store := &stubAnnotationStore{rows: nil}

	// Server should NOT be called.
	httpCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/manifest" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{})
			return
		}
		httpCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	summary, err := push.PushAnnotations(context.Background(), client, store, false)
	if err != nil {
		t.Fatalf("PushAnnotations on empty store: unexpected error: %v", err)
	}
	if summary == nil {
		t.Fatal("expected non-nil summary even for empty store")
	}
	if summary.Total != 0 {
		t.Errorf("Total: want 0, got %d", summary.Total)
	}
	if httpCalled {
		t.Error("should not contact server when there are no annotations")
	}
}

// TestPushAnnotations_NotFound_GracefulDegradation verifies that a 404 from the
// village (pre-v1.1.5 server without annotation support) causes graceful
// degradation: summary with SkipReason, nil error. The push command prints an
// informative message explaining why annotations were skipped.
func TestPushAnnotations_NotFound_GracefulDegradation(t *testing.T) {
	t.Parallel()
	store := &stubAnnotationStore{
		rows: []ingest.AnnotationPushRow{
			newSessionAnnotationRow("session-1", "retry_loops", "2"),
			newSessionAnnotationRow("session-2", "outcome", "success"),
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/manifest" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	summary, err := push.PushAnnotations(context.Background(), client, store, false)

	// 404 = village does not support annotations. Not an error.
	if err != nil {
		t.Fatalf("expected nil error for 404 (graceful degradation), got: %v", err)
	}
	if summary == nil {
		t.Fatal("expected non-nil summary with SkipReason for 404, got nil")
	}
	if summary.SkipReason == "" {
		t.Error("expected non-empty SkipReason for 404 (graceful degradation)")
	}
	if summary.Total != 2 {
		t.Errorf("Total: want 2 (annotations that would have been pushed), got %d", summary.Total)
	}
	// Per-item counters should be zero — no items were actually sent.
	if summary.Created != 0 || summary.Updated != 0 || summary.Skipped != 0 || summary.Errors != 0 {
		t.Errorf("per-item counters should be zero when skipped, got created=%d updated=%d skipped=%d errors=%d",
			summary.Created, summary.Updated, summary.Skipped, summary.Errors)
	}
}

// TestPushAnnotations_ServerError verifies that a non-2xx HTTP response from the
// village is surfaced as a fatal error (not swallowed into summary.Errors).
func TestPushAnnotations_ServerError(t *testing.T) {
	t.Parallel()
	sessionID := "session-err"
	store := &stubAnnotationStore{
		rows: []ingest.AnnotationPushRow{
			newSessionAnnotationRow(sessionID, "retry_loops", "1"),
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/manifest" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{})
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	summary, err := push.PushAnnotations(context.Background(), client, store, false)

	// A 500 from the server is a fatal error: it means the whole batch was rejected.
	if err == nil {
		t.Fatal("expected fatal error on 500 response, got nil")
	}
	// summary should still be returned (Total is set before the HTTP call).
	if summary == nil {
		t.Fatal("expected non-nil summary even on server error")
	}
	if summary.Total != 1 {
		t.Errorf("Total: want 1, got %d", summary.Total)
	}
}

// TestPushAnnotations_ContentHashesAreComputed verifies that all items sent to
// the server have their ContentHash field set (non-empty) and that different
// annotations produce different hashes.
func TestPushAnnotations_ContentHashesAreComputed(t *testing.T) {
	t.Parallel()
	s1 := "session-1"
	s2 := "session-2"
	store := &stubAnnotationStore{
		rows: []ingest.AnnotationPushRow{
			{
				ID:            "ann-1",
				TargetKind:    schema.TargetSession,
				SessionID:     &s1,
				TypeID:        "retry_loops",
				Value:         "2",
				IsPrimary:     true,
				AnnotatorName: "system",
			},
			{
				ID:            "ann-2",
				TargetKind:    schema.TargetSession,
				SessionID:     &s2,
				TypeID:        "retry_loops",
				Value:         "5",
				IsPrimary:     true,
				AnnotatorName: "system",
			},
		},
	}

	var receivedItems []schema.AnnotationPushItem
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/manifest" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{})
			return
		}
		var req schema.AnnotationPushRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		receivedItems = req.Annotations
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(schema.AnnotationPushResponse{Created: 2})
	}))
	defer srv.Close()

	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	_, err := push.PushAnnotations(context.Background(), client, store, false)
	if err != nil {
		t.Fatalf("PushAnnotations: %v", err)
	}

	if len(receivedItems) != 2 {
		t.Fatalf("expected 2 items, got %d", len(receivedItems))
	}

	hash0 := receivedItems[0].ContentHash
	hash1 := receivedItems[1].ContentHash
	if hash0 == "" || hash1 == "" {
		t.Error("ContentHash should be non-empty for all items")
	}
	if hash0 == hash1 {
		t.Error("different annotations should have different content hashes")
	}
}

// TestPushAnnotations_SystemTypesOnly verifies that PushAnnotations passes through
// only the annotations returned by ListSystemAnnotations (which filters to system
// origin at the SQL level.
func TestPushAnnotations_SystemTypesOnly(t *testing.T) {
	t.Parallel()
	// ListSystemAnnotations returns only system-origin annotations.
	// The stub models this by providing 2 system rows — PushAnnotations should
	// send exactly these 2 items to the server.
	store := &stubAnnotationStore{
		rows: []ingest.AnnotationPushRow{
			newSessionAnnotationRow("session-1", "retry_loops", "2"),
			newSessionAnnotationRow("session-2", "outcome", "success"),
		},
	}

	var receivedReq schema.AnnotationPushRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/manifest" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&receivedReq)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(schema.AnnotationPushResponse{Created: 2})
	}))
	defer srv.Close()

	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	summary, err := push.PushAnnotations(context.Background(), client, store, false)
	if err != nil {
		t.Fatalf("PushAnnotations: %v", err)
	}

	// PushAnnotations must send exactly what ListSystemAnnotations returned.
	if summary.Total != 2 {
		t.Errorf("Total: want 2, got %d", summary.Total)
	}
	if len(receivedReq.Annotations) != 2 {
		t.Fatalf("server received %d annotations, want 2", len(receivedReq.Annotations))
	}
	// Verify the type_ids match what was provided.
	typeIDs := map[string]bool{}
	for _, item := range receivedReq.Annotations {
		typeIDs[item.TypeID] = true
	}
	if !typeIDs["retry_loops"] || !typeIDs["outcome"] {
		t.Errorf("expected type_ids {retry_loops, outcome}, got %v", typeIDs)
	}
}

// TestPushAnnotations_SupersededExcluded verifies that superseded annotations are
// excluded from the push batch. ListSystemAnnotations filters WHERE superseded_by IS NULL,
// so annotations that have been superseded should not appear in the push payload.
//
// This test exercises the contract at the stubAnnotationStore level: the stub's
// ListSystemAnnotations returns only "active" (non-superseded) annotations.
// The real SQL filtering is tested by TestGetAnnotationsForProject_SupersededExcluded
// in the store package.
func TestPushAnnotations_SupersededExcluded(t *testing.T) {
	t.Parallel()
	// Simulate a store that has 3 annotations but ListSystemAnnotations returns only 2
	// (one has been superseded and is excluded by the SQL filter).
	store := &stubAnnotationStore{
		rows: []ingest.AnnotationPushRow{
			// Only active (non-superseded) annotations are returned.
			newSessionAnnotationRow("session-1", "retry_loops", "2"),
			newSessionAnnotationRow("session-1", "outcome", "resolved"),
			// The superseded annotation (retry_loops v1 with value "1") is NOT returned
			// by ListSystemAnnotations because superseded_by IS NOT NULL.
		},
	}

	var receivedItems []schema.AnnotationPushItem
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/annotations/manifest" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(schema.AnnotationManifestResponse{})
			return
		}
		var req schema.AnnotationPushRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		receivedItems = req.Annotations
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(schema.AnnotationPushResponse{Created: 2})
	}))
	defer srv.Close()

	client := village.NewVillageClient(srv.URL, testAPIKey, nil)
	summary, err := push.PushAnnotations(context.Background(), client, store, false)
	if err != nil {
		t.Fatalf("PushAnnotations: %v", err)
	}

	// Only 2 annotations should be pushed (the superseded one is excluded).
	if summary.Total != 2 {
		t.Errorf("Total: want 2, got %d", summary.Total)
	}
	if len(receivedItems) != 2 {
		t.Fatalf("server received %d items, want 2", len(receivedItems))
	}

	// Verify content — only "2" and "resolved" should be present, not "1".
	values := map[string]bool{}
	for _, item := range receivedItems {
		values[item.Value] = true
	}
	if !values["2"] || !values["resolved"] {
		t.Errorf("expected values {2, resolved}, got %v", values)
	}
	if values["1"] {
		t.Error("superseded annotation value '1' should not be in push batch")
	}
}
