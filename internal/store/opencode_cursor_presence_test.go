package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
)

// TestInsertSessionsWritesOnlyAnAcquiredOpenCodeCursor holds when the ordinary
// session write may move the OpenCode event cursor.
//
// The cursor is a monotonic per-session sequence, so zero is a claim - "no
// events ever" - and not an absence. A write that carries no cursor has not
// observed one, and must leave whatever an earlier harvest acquired exactly
// where it is. A source fingerprint says the source was READ; it does not say
// a cursor was among what was read, so it cannot authorize the write.
func TestInsertSessionsWritesOnlyAnAcquiredOpenCodeCursor(t *testing.T) {
	t.Parallel()
	database := openTestStore(t)
	ctx := context.Background()
	const sessionID = "ses_3cd91f52effeXd3QAJ54jOyzv5"
	id, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	entry := makeStoreEntry(t, sessionID, hash, "github.com-opencode-repo", defaults.HarnessOpenCode, 1700000000000, 100, 50)
	entry.SourceFingerprint = []byte("the source was read")

	stored := func() (int64, bool) {
		t.Helper()
		cursors, err := database.BulkLookupOpenCodeSeqCursors(ctx, []ingest.SessionID{id})
		if err != nil {
			t.Fatal(err)
		}
		value, found := cursors[id]
		return value, found
	}

	// Nothing observed a cursor yet, so nothing may be recorded: a session with
	// no cursor is a first sighting, and an acquired zero would hide that.
	if err := database.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatal(err)
	}
	if value, found := stored(); found {
		t.Fatalf("a write that observed no cursor recorded one as %d; a session with no cursor must stay without one", value)
	}

	// An observed cursor is acquired evidence and is written.
	acquired := int64(77)
	entry.EventSeq = &acquired
	if err := database.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatal(err)
	}
	if value, found := stored(); !found || value != acquired {
		t.Fatalf("an observed cursor was not recorded: stored=%d found=%v want %d", value, found, acquired)
	}

	// A later write that observed nothing preserves it. This is the regression:
	// the fingerprint alone used to authorize the write, so progress a previous
	// harvest proved was overwritten with zero.
	entry.EventSeq = nil
	if err := database.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatal(err)
	}
	if value, found := stored(); !found || value != acquired {
		t.Fatalf("a write that observed no cursor moved the stored one to %d (found=%v); an unknown cursor must preserve proven progress, not reset it", value, found)
	}

	// Zero is a real acquired value when it was observed, and it is written.
	zero := int64(0)
	entry.EventSeq = &zero
	if err := database.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatal(err)
	}
	if value, found := stored(); !found || value != 0 {
		t.Fatalf("an observed zero cursor was not recorded: stored=%d found=%v", value, found)
	}
}
