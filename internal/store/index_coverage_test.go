package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// coverageSessionID returns a deterministic valid session UUID for index i.
// Generated, not fixture-listed: the chunk-boundary case needs more sessions
// than any readable manifest should carry, and every ID here is shaped only by
// its position (requested, seeded, duplicated), never by an individual value.
func coverageSessionID(i int) ingest.SessionID {
	return ingest.SessionID(fmt.Sprintf("11111111-1111-4111-8111-%012x", i))
}

// seedCoverageEntries stores one entry row for the session so it counts as
// retained. The sessions row must exist first: the entry write refuses a
// session whose metadata was never stored.
func seedCoverageEntries(t *testing.T, s *store.Store, id ingest.SessionID) {
	t.Helper()
	seedSession(t, s, string(id))
	preview := "retained entry content"
	writes := []ingest.SessionEntryWrite{{
		SessionID:    id,
		Result:       indexformat.V1{Entries: []schema.SessionEntry{{SessionID: id, Harness: schema.HarnessClaudeCode, EntryIndex: 0, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &preview}}},
		IndexVersion: 1,
	}}
	if result := s.IndexSessionEntryBatch(context.Background(), writes); result[0].Err != nil {
		t.Fatalf("seed entries for %s: %v", id, result[0].Err)
	}
}

// TestStore_SessionsWithoutEntries_ChunkBoundary drives the real store past one
// variable-limit chunk and pins the exact membership contract: every requested
// session is reported, duplicates collapse, unrequested sessions never appear,
// and seeded entries flip exactly their own sessions.
func TestStore_SessionsWithoutEntries_ChunkBoundary(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	const requested = 1200
	ids := make([]ingest.SessionID, 0, requested+10)
	want := make(map[ingest.SessionID]bool, requested)
	for i := 0; i < requested; i++ {
		id := coverageSessionID(i)
		ids = append(ids, id)
		// Every third session keeps its entries; the rest are empty.
		hasEntries := i%3 == 0
		if hasEntries {
			seedCoverageEntries(t, s, id)
		}
		want[id] = !hasEntries
	}
	// Duplicates collapse: the first ten sessions are asked for twice.
	ids = append(ids, ids[:10]...)
	// Exclusion: sessions with entries that were never requested must not
	// appear in the answer.
	for i := requested; i < requested+5; i++ {
		seedCoverageEntries(t, s, coverageSessionID(i))
	}

	got, err := s.SessionsWithoutEntries(ctx, ids)
	if err != nil {
		t.Fatalf("SessionsWithoutEntries: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("SessionsWithoutEntries reported %d session(s), want %d; duplicates must collapse and unrequested sessions must stay out", len(got), len(want))
	}
	for id, wantWithout := range want {
		gotWithout, ok := got[id]
		if !ok {
			t.Errorf("SessionsWithoutEntries omits requested session %s", id)
			continue
		}
		if gotWithout != wantWithout {
			t.Errorf("SessionsWithoutEntries(%s) = %v, want %v", id, gotWithout, wantWithout)
		}
	}
	for i := requested; i < requested+5; i++ {
		if _, ok := got[coverageSessionID(i)]; ok {
			t.Errorf("SessionsWithoutEntries names unrequested session %s", coverageSessionID(i))
		}
	}
}

// TestStore_SessionsWithoutEntries_Empty asks nothing and expects an empty
// answer with no error and no query behind it.
func TestStore_SessionsWithoutEntries_Empty(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	for _, ids := range [][]ingest.SessionID{nil, {}} {
		got, err := s.SessionsWithoutEntries(context.Background(), ids)
		if err != nil {
			t.Fatalf("SessionsWithoutEntries(%v): %v", ids, err)
		}
		if len(got) != 0 {
			t.Fatalf("SessionsWithoutEntries(%v) = %v, want empty", ids, got)
		}
	}
}

// TestStore_SessionsWithoutEntries_UnknownSessions counts sessions the store
// never saw as empty: no row anywhere means no entries rows.
func TestStore_SessionsWithoutEntries_UnknownSessions(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ids := []ingest.SessionID{coverageSessionID(9001), coverageSessionID(9002)}
	got, err := s.SessionsWithoutEntries(context.Background(), ids)
	if err != nil {
		t.Fatalf("SessionsWithoutEntries: %v", err)
	}
	for _, id := range ids {
		if !got[id] {
			t.Errorf("SessionsWithoutEntries(%s) = false, want true: a session with no rows holds no entries", id)
		}
	}
}
