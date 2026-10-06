package store_test

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// An unchanged-session replacement is skipped, not rewritten. The skip is a
// completed write: it restores the indexed input proof, advances the index
// revision and time, records the entries digest, and stamps the publication
// capture revision, exactly as a replacement does. A skip that left any of
// these stale would leave the row selected by the repair and readiness
// predicates on the next harvest, so a warm store would never converge.
func TestUnchangedIndexSkipCompletesWriteBookkeeping(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		clearHash bool
		wantStat  string
	}{
		{name: "hash match", clearHash: false, wantStat: "SkippedByHash"},
		{name: "compare fallback", clearHash: true, wantStat: "SkippedByCompare"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := openTestStore(t)
			sid := schema.SessionID(testutil.TestSessionUUID)
			entry := makeStoreEntry(t, string(sid), string(testutil.TestProjectHash), "fixture-host", defaults.HarnessClaudeCode, 1700000000000, 100, 50)
			if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{entry}); err != nil {
				t.Fatal(err)
			}
			entries := batchTestEntries(sid, "same", 2)
			seed := db.IndexSessionEntryBatch(t.Context(), []ingest.SessionEntryWrite{{
				SessionID: sid, Result: indexformat.V1{Entries: entries}, IndexVersion: 1,
				IndexerVersion: 15, IndexedAtMs: 100,
			}})[0]
			if seed.Err != nil {
				t.Fatal(seed.Err)
			}
			proof := "abcdef1234abcdef1234abcdef1234abcdef1234abcdef1234abcdef12345679"
			inputSQL(t, db, sid, "UPDATE sessions SET artifact_hash = ? WHERE session_id = ?", string(testutil.TestProjectHash))
			inputSQL(t, db, sid, "UPDATE sessions SET indexed_input_hash = ? WHERE session_id = ?", proof)
			inputSQL(t, db, sid, "UPDATE sessions SET publication_capture_revision = 3, cwd_provenance_kind = 'source_exact', indexed_publication_capture_revision = 0 WHERE session_id = ?")

			// The artifact installer clears the input proof before it replaces a
			// pair. Clearing the digest as well selects the compare fallback.
			if err := db.PrepareArtifactInstall(t.Context(), sid); err != nil {
				t.Fatal(err)
			}
			if tc.clearHash {
				inputSQL(t, db, sid, "UPDATE sessions SET session_entries_hash = NULL WHERE session_id = ?")
			}
			cleared := readInputState(t, db, sid)
			if cleared.IndexedInputHash != nil {
				t.Fatalf("preparation must clear the input proof; got %q", *cleared.IndexedInputHash)
			}

			// Re-run the identical entries carrying the captured proof and the
			// current publication capture revision, as the harvest does.
			res := db.IndexSessionEntryBatch(t.Context(), []ingest.SessionEntryWrite{{
				SessionID: sid, Result: indexformat.V1{Entries: entries}, IndexVersion: 1,
				IndexerVersion: 15, IndexedAtMs: 200, IndexedInputHash: &proof,
				ExpectedState: cleared, CaptureRevision: 3,
			}})[0]
			if res.Err != nil {
				t.Fatal(res.Err)
			}
			if !res.Skipped || !res.Written {
				t.Fatalf("identical entries must be skipped and still report completion: %+v", res)
			}
			switch tc.wantStat {
			case "SkippedByHash":
				if res.Stats.SkippedByHash != 1 {
					t.Fatalf("expected the hash-match skip; stats=%+v", res.Stats)
				}
			case "SkippedByCompare":
				if res.Stats.SkippedByCompare != 1 {
					t.Fatalf("expected the compare fallback skip; stats=%+v", res.Stats)
				}
			}

			after := readInputState(t, db, sid)
			if after.IndexedInputHash == nil || *after.IndexedInputHash != proof {
				t.Fatalf("the skip did not restore the input proof: %+v", after.IndexedInputHash)
			}
			if after.IndexedAt == nil || *after.IndexedAt != 200 {
				t.Fatalf("the skip did not advance indexed_at: %+v", after.IndexedAt)
			}
			if after.IndexerVersion != 15 {
				t.Fatalf("the skip did not record the producer revision: %d", after.IndexerVersion)
			}
			if after.SessionEntriesHash == nil || *after.SessionEntriesHash == "" {
				t.Fatalf("the skip did not record the entries digest: %+v", after.SessionEntriesHash)
			}
			if got := indexedPublicationRevision(t, db, sid); got != 3 {
				t.Fatalf("the skip did not stamp the publication capture revision: %d, want 3", got)
			}
			// The row is now settled: nothing about it selects it again.
			needing, err := db.ListSessionsNeedingRepair(t.Context(), ingest.HarvesterVersionRegistry)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range needing {
				if id == sid {
					t.Fatalf("a skipped, proven session was selected for repair: %v", needing)
				}
			}
		})
	}
}

func indexedPublicationRevision(t *testing.T, db *store.Store, sid schema.SessionID) int64 {
	t.Helper()
	conn := takeConn(t, db.Pool())
	defer db.Pool().Put(conn)
	var rev int64
	if err := sqlitex.ExecuteTransient(conn, "SELECT indexed_publication_capture_revision FROM sessions WHERE session_id = ?", &sqlitex.ExecOptions{
		Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error { rev = stmt.ColumnInt64(0); return nil },
	}); err != nil {
		t.Fatal(err)
	}
	return rev
}
