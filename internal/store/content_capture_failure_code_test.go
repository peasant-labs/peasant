package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestStoredFailureCodeThisBuildCannotNameIsRefused holds both reads that
// carry a stored capture's failure code across the store boundary.
//
// The selector acts on this value: a code read as "no failure recorded" when
// it is really a refusal puts the session back into pending work on every
// harvest, forever, which is the exact condition the steady state removes. So
// an unknown code must refuse the read and return nothing, not fall back to
// the absent code. A row carrying one is what an installation has after
// running a newer Peasant and then an older one.
func TestStoredFailureCodeThisBuildCannotNameIsRefused(t *testing.T) {
	t.Parallel()
	database := openTestStore(t)
	ctx := context.Background()
	const sessionID = "dddd4444-dddd-4ddd-8ddd-dddddddddddd"
	id, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	entry := makeStoreEntry(t, sessionID, hash, "github.com-failurecode-repo", defaults.HarnessClaudeCode, 1700000000000, 100, 50)
	if err := database.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatal(err)
	}
	// A stored row no build in this tree could have written through the writer,
	// which now validates the code. Only a different build could leave it.
	conn := takeConn(t, database.PoolForTest())
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_content_captures
(session_id,status,source_authority,transcript_origin,capture_format,entry_count,content_row_count,full_capture_sha256,captured_at_ms,failure_code,failure_message,publication_capture_revision)
VALUES(?,'incomplete','peasant_snapshot',0,'preview_only',1,0,NULL,1700000000000,'quarantined_pending_review','held by a newer build',0)`,
		&sqlitex.ExecOptions{Args: []any{sessionID}}); err != nil {
		database.PoolForTest().Put(conn)
		t.Fatal(err)
	}
	database.PoolForTest().Put(conn)

	state, err := database.ReadIndexState(ctx, id)
	if err == nil {
		t.Fatalf("the index state was read with a failure code this build cannot name: %+v", state)
	}
	if state != nil {
		t.Fatalf("a refused index-state read still returned state: %+v", state)
	}
	if !strings.Contains(err.Error(), "quarantined_pending_review") {
		t.Fatalf("the refusal does not name the value that caused it: %v", err)
	}

	capture, found, err := database.GetSessionContentCapture(ctx, id)
	if err == nil {
		t.Fatalf("the capture was read with a failure code this build cannot name: found=%v %+v", found, capture)
	}
	if found {
		t.Fatalf("a refused capture read reported the capture as present: %+v", capture)
	}
	if !strings.Contains(err.Error(), "quarantined_pending_review") {
		t.Fatalf("the refusal does not name the value that caused it: %v", err)
	}
}

// TestIndexWriteRefusesAFailureCodeThisBuildCannotName holds the write side of
// the same closed set.
//
// Reads fail closed, but a value that never reaches the row cannot be misread
// later. The two sibling enums are validated on the way in; this one is too,
// so a raw conversion at any caller is refused here rather than stored and met
// again by a future reader.
func TestIndexWriteRefusesAFailureCodeThisBuildCannotName(t *testing.T) {
	t.Parallel()
	database := openTestStore(t)
	ctx := context.Background()
	const sessionID = "eeee5555-eeee-4eee-8eee-eeeeeeeeeeee"
	id, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	entry := makeStoreEntry(t, sessionID, hash, "github.com-failurewrite-repo", defaults.HarnessClaudeCode, 1700000000000, 100, 50)
	if err := database.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatal(err)
	}
	results := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: id, Result: indexformat.V1{}, IndexVersion: 1, IndexedAtMs: 1700000000000,
		IndexerVersion: 1,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status: ingest.ContentCaptureIncomplete, SourceAuthority: ingest.ContentSourcePeasantSnapshot,
			CaptureFormat: ingest.ContentCaptureFormatPreviewOnly, CapturedAtMs: 1700000000000,
			FailureCode:    ingest.ContentCaptureFailureCode("quarantined_pending_review"),
			FailureMessage: "held by a build that is not this one",
		},
	}})
	if len(results) != 1 {
		t.Fatalf("one write returned %d results", len(results))
	}
	if results[0].Err == nil || results[0].Written {
		t.Fatalf("a capture carrying a failure code this build cannot name was written: %+v", results[0])
	}
	if !strings.Contains(results[0].Err.Error(), "quarantined_pending_review") {
		t.Fatalf("the refusal does not name the value that caused it: %v", results[0].Err)
	}
	if _, found, err := database.GetSessionContentCapture(ctx, id); err != nil || found {
		t.Fatalf("a refused write left a capture row behind: found=%v %v", found, err)
	}
}
