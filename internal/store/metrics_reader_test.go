package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
)

// ---------------------------------------------------------------------------
// C2: ListEntriesRange + MaxEntryIndex
// ---------------------------------------------------------------------------

// seedRangeEntries seeds entryCount assistant entries (index 0..entryCount-1) into
// the given session. Returns the session ID typed as schema.SessionID.
func seedRangeEntries(t *testing.T, s *store.Store, sessionID string, entryCount int) schema.SessionID {
	t.Helper()
	sid := schema.SessionID(sessionID)
	entries := make([]schema.SessionEntry, entryCount)
	for i := range entries {
		entries[i] = schema.SessionEntry{
			SessionID:  sid,
			EntryIndex: i,
			Harness:    defaults.HarnessClaudeCode,
			EntryType:  schema.EntryTypeText,
			Role:       schema.RoleAssistant,
		}
	}
	if err := s.IndexSessionEntries(context.Background(), sid, entries); err != nil {
		t.Fatalf("seedRangeEntries(%q, %d): %v", sessionID, entryCount, err)
	}
	return sid
}

// TestListEntriesRange_InclusiveBothEnds verifies [from, to] inclusive semantics:
// entries at exactly from and to are included.
func TestListEntriesRange_InclusiveBothEnds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)

	const sid = "55555555-5555-5555-5555-111111111111"
	storetest.SeedSession(t, s, sid)
	sessionID := seedRangeEntries(t, s, sid, 5) // indices 0,1,2,3,4

	got, err := s.ListEntriesRange(ctx, sessionID, 1, 3)
	if err != nil {
		t.Fatalf("ListEntriesRange: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries [1,2,3], got %d", len(got))
	}
	for i, e := range got {
		want := 1 + i
		if e.EntryIndex != want {
			t.Errorf("entry[%d].EntryIndex = %d, want %d", i, e.EntryIndex, want)
		}
	}
}

// TestListEntriesRange_OrderedByEntryIndex verifies entries are returned in
// ascending entry_index order.
func TestListEntriesRange_OrderedByEntryIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)

	const sid = "55555555-5555-5555-5555-222222222222"
	storetest.SeedSession(t, s, sid)
	sessionID := seedRangeEntries(t, s, sid, 6) // indices 0..5

	got, err := s.ListEntriesRange(ctx, sessionID, 0, 5)
	if err != nil {
		t.Fatalf("ListEntriesRange: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("expected 6 entries, got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].EntryIndex <= got[i-1].EntryIndex {
			t.Errorf("not ordered: got[%d].EntryIndex=%d <= got[%d].EntryIndex=%d",
				i, got[i].EntryIndex, i-1, got[i-1].EntryIndex)
		}
	}
}

// TestListEntriesRange_EmptyWhenFromGreaterThanMax verifies that when from > max
// entry_index the result is an empty slice (not an error).
func TestListEntriesRange_EmptyWhenFromGreaterThanMax(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)

	const sid = "55555555-5555-5555-5555-333333333333"
	storetest.SeedSession(t, s, sid)
	sessionID := seedRangeEntries(t, s, sid, 3) // max index = 2

	got, err := s.ListEntriesRange(ctx, sessionID, 10, 20) // from > max
	if err != nil {
		t.Fatalf("ListEntriesRange(10,20): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice when from>max, got %d entries", len(got))
	}
}

// TestListEntriesRange_ClampedAtBoundaries verifies that [0, 100] on a 4-entry
// session returns all 4 entries (to is clamped to actual max, not an error).
func TestListEntriesRange_ClampedAtBoundaries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)

	const sid = "55555555-5555-5555-5555-444444444444"
	storetest.SeedSession(t, s, sid)
	sessionID := seedRangeEntries(t, s, sid, 4) // indices 0..3

	got, err := s.ListEntriesRange(ctx, sessionID, 0, 100) // to way beyond max
	if err != nil {
		t.Fatalf("ListEntriesRange(0,100): %v", err)
	}
	if len(got) != 4 {
		t.Errorf("expected 4 entries (clamped), got %d", len(got))
	}
}

// TestListEntriesRange_SingleEntry verifies that [k, k] returns exactly one entry.
func TestListEntriesRange_SingleEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)

	const sid = "55555555-5555-5555-5555-555555555555"
	storetest.SeedSession(t, s, sid)
	sessionID := seedRangeEntries(t, s, sid, 5) // indices 0..4

	got, err := s.ListEntriesRange(ctx, sessionID, 2, 2) // exact single
	if err != nil {
		t.Fatalf("ListEntriesRange(2,2): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry for [2,2], got %d", len(got))
	}
	if got[0].EntryIndex != 2 {
		t.Errorf("EntryIndex = %d, want 2", got[0].EntryIndex)
	}
}

// TestListEntriesRange_NoEntriesInRange verifies that when the session has entries
// but none fall within [from, to], the result is empty (not an error).
func TestListEntriesRange_NoEntriesInRange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)

	const sid = "55555555-5555-5555-5555-666666666666"
	storetest.SeedSession(t, s, sid)
	sessionID := seedRangeEntries(t, s, sid, 3) // indices 0,1,2

	// Ask for range [5, 9] — none exist.
	got, err := s.ListEntriesRange(ctx, sessionID, 5, 9)
	if err != nil {
		t.Fatalf("ListEntriesRange(5,9): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice for out-of-range [5,9], got %d entries", len(got))
	}
}

// ---------------------------------------------------------------------------
// MaxEntryIndex
// ---------------------------------------------------------------------------

// TestMaxEntryIndex_EmptySession verifies that MaxEntryIndex returns -1 when the
// session exists but has no indexed entries.
func TestMaxEntryIndex_EmptySession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)

	const sid = "66666666-6666-6666-6666-111111111111"
	storetest.SeedSession(t, s, sid)
	// Do NOT seed entries — session has none.

	max, err := s.MaxEntryIndex(ctx, schema.SessionID(sid))
	if err != nil {
		t.Fatalf("MaxEntryIndex: %v", err)
	}
	if max != -1 {
		t.Errorf("MaxEntryIndex for empty session = %d, want -1", max)
	}
}

// TestMaxEntryIndex_UnknownSession verifies that MaxEntryIndex returns -1 for a
// session ID that does not exist in the DB at all.
func TestMaxEntryIndex_UnknownSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)

	max, err := s.MaxEntryIndex(ctx, schema.SessionID("ffffffff-ffff-ffff-ffff-ffffffffffff"))
	if err != nil {
		t.Fatalf("MaxEntryIndex: %v", err)
	}
	if max != -1 {
		t.Errorf("MaxEntryIndex for unknown session = %d, want -1", max)
	}
}

// TestMaxEntryIndex_TrueMax verifies that MaxEntryIndex returns the highest
// entry_index for a session with multiple entries.
func TestMaxEntryIndex_TrueMax(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)

	const sid = "66666666-6666-6666-6666-222222222222"
	storetest.SeedSession(t, s, sid)
	sessionID := seedRangeEntries(t, s, sid, 7) // max index = 6

	max, err := s.MaxEntryIndex(ctx, sessionID)
	if err != nil {
		t.Fatalf("MaxEntryIndex: %v", err)
	}
	if max != 6 {
		t.Errorf("MaxEntryIndex = %d, want 6", max)
	}
}

// TestMaxEntryIndex_SingleEntry verifies MaxEntryIndex == 0 when there is exactly one entry.
func TestMaxEntryIndex_SingleEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)

	const sid = "66666666-6666-6666-6666-333333333333"
	storetest.SeedSession(t, s, sid)
	sessionID := seedRangeEntries(t, s, sid, 1) // only index 0

	max, err := s.MaxEntryIndex(ctx, sessionID)
	if err != nil {
		t.Fatalf("MaxEntryIndex: %v", err)
	}
	if max != 0 {
		t.Errorf("MaxEntryIndex = %d, want 0", max)
	}
}
