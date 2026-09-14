package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// openGenerationCapableTestStore opens a real, fully migrated store whose
// snapshot reader is the production generation-capable reader.
func openGenerationCapableTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "artifacts")
	artifacts, err := store.NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(
		filepath.Join(dir, "generations.db"),
		store.WithPoolSize(2),
		store.WithIndexFormats(store.V2IndexFormat()),
		store.WithGenerationArtifacts(artifacts, locker),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// activateTargetDetailGeneration commits one minimal complete generation for a
// seeded session, carrying the supplied durable relationships so the local read
// can resolve relationship navigation.
func activateTargetDetailGeneration(t *testing.T, s *store.Store, sid schema.SessionID, generationID string, relationships []schema.SessionRelationship) {
	t.Helper()
	ref := schema.SourceEntryRef("e_target")
	text := "target session body"
	inputCount := int64(1)
	generation := indexformat.Generation{
		ID:           generationID,
		Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  defaults.HarnessClaudeCode,
			Stats:         schema.SessionStats{TurnCount: 1, InputSubmissionCount: &inputCount},
			Relationships: relationships,
		},
		Main: indexformat.Partition{Entries: []schema.SessionEntry{{
			SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode,
			EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &text, SourceEntryRef: ref,
		}}},
		Content:              []indexformat.ContentRecord{{Ref: ref}},
		SourceEvidenceDigest: strings.Repeat("a", 64),
		TitleRefs:            []schema.SourceEntryRef{ref},
	}
	if err := s.ActivateGeneration(context.Background(), store.GenerationActivation{
		Generation:     indexformat.V2{Generation: generation},
		Blobs:          map[schema.SourceEntryRef][]byte{ref: []byte(text)},
		IndexerVersion: 1,
		IndexedAtMs:    1,
	}); err != nil {
		t.Fatalf("activate target generation %s: %v", generationID, err)
	}
}

// largeUnrelatedLibraryEntries seeds a library large enough that a whole-library
// scan would be obvious, under one deterministic project.
func largeUnrelatedLibraryEntries(t *testing.T, count int) []ingest.StoreEntry {
	t.Helper()
	const projectHash = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	entries := make([]ingest.StoreEntry, 0, count)
	for i := 0; i < count; i++ {
		sessionID := fmt.Sprintf("30000000-0000-4000-8000-%012d", i)
		entries = append(entries, makeStoreEntry(
			t, sessionID, projectHash, "github.com-example", defaults.HarnessClaudeCode,
			1705276800000+int64(i)*1000, 1000, 500, "unrelated-library", 10, 5, 60000,
		))
	}
	return entries
}

// TestStoreDataProviderSessionSummariesByIDIsTargetScoped proves the link
// resolution path performs work proportional to the REQUESTED targets, not to
// the stored library. It seeds a large unrelated library and asserts the
// allocation volume of one resolution stays far below what materializing every
// stored row would cost, so a regression back to a whole-library scan fails.
func TestStoreDataProviderSessionSummariesByIDIsTargetScoped(t *testing.T) {
	const librarySize = 2048
	db := openGenerationCapableTestStore(t)
	const projectHash = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	targetA := "40000000-0000-4000-8000-00000000000a"
	targetB := "40000000-0000-4000-8000-00000000000b"
	entries := largeUnrelatedLibraryEntries(t, librarySize)
	entries = append(entries,
		makeStoreEntry(t, targetA, projectHash, "github.com-example", defaults.HarnessClaudeCode, 1705276999000, 1000, 500, "target-project", 10, 5, 60000),
		makeStoreEntry(t, targetB, projectHash, "github.com-example", defaults.HarnessClaudeCode, 1705276999500, 1000, 500, "target-project", 10, 5, 60000),
	)
	if err := db.InsertSessions(context.Background(), entries); err != nil {
		t.Fatalf("seed library: %v", err)
	}
	api.MarkStoredSessionsIndexed(t, db)

	provider := api.NewStoreDataProvider(db, sessionvisibility.All())
	requested := []string{targetA, targetB}
	// Warm the pool and any lazy per-store state so the measurement isolates the
	// lookup itself.
	if _, err := provider.SessionSummariesByID(context.Background(), requested); err != nil {
		t.Fatalf("warm SessionSummariesByID: %v", err)
	}

	var callErr error
	allocs := testing.AllocsPerRun(5, func() {
		_, callErr = provider.SessionSummariesByID(context.Background(), requested)
	})
	if callErr != nil {
		t.Fatalf("SessionSummariesByID: %v", callErr)
	}
	// A whole-library scan allocates on the order of one row per stored session
	// (thousands here); a target-scoped exact-ID lookup stays in the hundreds.
	// The bound is deliberately well above the scoped cost and well below the
	// library-sized cost, so it is a work-scope assertion, not a timing guess.
	if allocs > float64(librarySize)/2 {
		t.Fatalf("SessionSummariesByID allocated %.0f objects for 2 targets with a %d-row library; link resolution is not target-scoped", allocs, librarySize)
	}

	summaries, err := provider.SessionSummariesByID(context.Background(), requested)
	if err != nil {
		t.Fatalf("SessionSummariesByID: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("SessionSummariesByID returned %d summaries, want exactly the 2 named targets", len(summaries))
	}
}

// TestServerSessionDetailRouteMissingSessionIsNotFound pins the integration
// boundary between the generation-capable reader and the HTTP status contract:
// a valid identifier that names no stored session must answer 404, not 500,
// and must not echo the requested identifier. The real reader emits a typed
// not-found sentinel from its metadata lookup; the adapter maps it to the API
// not-found, so removing that mapping turns this route back into a server
// error and fails here.
func TestServerSessionDetailRouteMissingSessionIsNotFound(t *testing.T) {
	db := openGenerationCapableTestStore(t)
	provider := api.NewStoreDataProvider(db, sessionvisibility.All())
	baseURL := startDetailReadServer(t, provider)

	const missingID = "60000000-0000-4000-8000-000000000001"
	status, _, body := getJSON(t, baseURL+"/api/v1/sessions/"+missingID)
	if status != 404 {
		t.Fatalf("missing session status = %d, want 404; body=%s", status, body)
	}
	if strings.Contains(string(body), missingID) {
		t.Fatal("not-found body must not echo the requested identifier")
	}
}

// TestServerSessionDetailRouteResolvesStoredTargetAgainstLargeLibrary is the
// mounted integration evidence for the exact-ID link lookup: the registered
// HTTP detail route serves a generation-capable real store and resolves the
// durable relationship navigation to the stored parent target while a large
// unrelated library is present.
func TestServerSessionDetailRouteResolvesStoredTargetAgainstLargeLibrary(t *testing.T) {
	const librarySize = 1024
	db := openGenerationCapableTestStore(t)
	const projectHash = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	targetID := "50000000-0000-4000-8000-000000000001"
	parentID := "50000000-0000-4000-8000-000000000002"
	entries := largeUnrelatedLibraryEntries(t, librarySize)
	entries = append(entries,
		makeStoreEntry(t, targetID, projectHash, "github.com-example", defaults.HarnessClaudeCode, 1705276999000, 1000, 500, "target-project", 10, 5, 60000),
		makeStoreEntry(t, parentID, projectHash, "github.com-example", defaults.HarnessClaudeCode, 1705276999500, 1000, 500, "target-project", 10, 5, 60000),
	)
	if err := db.InsertSessions(context.Background(), entries); err != nil {
		t.Fatalf("seed library: %v", err)
	}

	target := schema.SessionID(targetID)
	parent := schema.SessionID(parentID)
	activateTargetDetailGeneration(t, db, target, "g-route-target", []schema.SessionRelationship{{
		Kind:          schema.SessionRelationshipStartedBy,
		TargetState:   schema.RelationshipTargetKnown,
		TargetLocalID: &parent,
		Evidence:      schema.EvidenceNativeTyped,
	}})

	provider := api.NewStoreDataProvider(db, sessionvisibility.All())
	baseURL := startDetailReadServer(t, provider)

	status, _, body := getJSON(t, baseURL+"/api/v1/sessions/"+targetID)
	if status != 200 {
		t.Fatalf("detail route status = %d, want 200; body=%s", status, body)
	}
	var decoded struct {
		ID                     string                                 `json:"id"`
		RelationshipNavigation []schema.SessionRelationshipNavigation `json:"relationshipNavigation"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode detail route body: %v", err)
	}
	if decoded.ID != targetID {
		t.Fatalf("detail route id = %q, want the requested session", decoded.ID)
	}
	if len(decoded.RelationshipNavigation) != 1 {
		t.Fatalf("relationshipNavigation = %+v, want exactly one resolved entry", decoded.RelationshipNavigation)
	}
	nav := decoded.RelationshipNavigation[0]
	if nav.Kind != schema.SessionRelationshipStartedBy || nav.Status != schema.RelationshipNavigationResolved {
		t.Fatalf("relationshipNavigation = %+v, want a resolved started_by entry", nav)
	}
	if nav.LocalID == nil || *nav.LocalID != parent {
		t.Fatalf("relationshipNavigation target = %v, want the stored parent %s", nav.LocalID, parentID)
	}
}
