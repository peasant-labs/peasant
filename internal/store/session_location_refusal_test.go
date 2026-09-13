package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// TestBulkSessionLocationCarriesRefusalState proves the DIFF location query
// returns the fields the discovery hint needs to recognize a settled refusal
// without opening the pair: the stored capture failure code, the producing
// indexer revision, and the input proof. It also proves an unknown failure
// code fails the lookup closed rather than reading as "nothing refused", which
// would put the session back into pending work on every harvest forever.
func TestBulkSessionLocationCarriesRefusalState(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := publicationEntry(t, "11111111-1111-4111-8111-111111111111")
	id := e.Metadata.SessionID
	revision := capturePublication(t, s, e)
	if r := indexPublication(t, s, e, revision, batchTestEntries(id, "entry", 1)); r.Err != nil {
		t.Fatal(r.Err)
	}
	proof := strings.Repeat("b", 64)
	execContentSQL(t, s, "UPDATE session_content_captures SET status='incomplete', capture_format='preview_only', failure_code='strict_capture_refused' WHERE session_id='"+string(id)+"'")
	execContentSQL(t, s, "UPDATE sessions SET indexed_input_hash='"+proof+"' WHERE session_id='"+string(id)+"'")

	locations, err := s.BulkLookupSessionLocations(context.Background(), []ingest.SessionID{id})
	if err != nil {
		t.Fatal(err)
	}
	loc := locations[id]
	target := ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode]
	if loc.ContentFailureCode != ingest.ContentCaptureStrictRefused {
		t.Fatalf("location failure code = %q, want %q", loc.ContentFailureCode, ingest.ContentCaptureStrictRefused)
	}
	if loc.IndexerVersion != target.IndexerVersion {
		t.Fatalf("location indexer version = %d, want %d", loc.IndexerVersion, target.IndexerVersion)
	}
	if loc.IndexedInputHash == nil || *loc.IndexedInputHash != proof {
		t.Fatalf("location input proof = %v, want %q", loc.IndexedInputHash, proof)
	}

	execContentSQL(t, s, "UPDATE session_content_captures SET failure_code='not_a_code' WHERE session_id='"+string(id)+"'")
	if _, err := s.BulkLookupSessionLocations(context.Background(), []ingest.SessionID{id}); err == nil {
		t.Fatal("an unknown failure code must fail the location lookup closed")
	}
}
