package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestIngestLog_AutoincrementGrows verifies that consecutive LogIngestRun calls
// produce rows with sequential, strictly increasing AUTOINCREMENT IDs.
func TestIngestLog_AutoincrementGrows(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// Insert two entries.
	entry1 := ingest.IngestLogEntry{
		StartedAt:   1705276800000,
		SessionsNew: 3,
	}
	entry2 := ingest.IngestLogEntry{
		StartedAt:   1705276860000,
		SessionsNew: 7,
	}

	if err := s.LogIngestRun(ctx, entry1); err != nil {
		t.Fatalf("LogIngestRun (entry1): %v", err)
	}
	if err := s.LogIngestRun(ctx, entry2); err != nil {
		t.Fatalf("LogIngestRun (entry2): %v", err)
	}

	// Query the ingest_log table directly to verify IDs.
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	var ids []int64
	err := sqlitex.ExecuteTransient(conn, `SELECT id FROM ingest_log ORDER BY id`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			ids = append(ids, stmt.ColumnInt64(0))
			return nil
		},
	})
	if err != nil {
		t.Fatalf("query ingest_log IDs: %v", err)
	}

	if len(ids) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(ids))
	}
	if ids[0] >= ids[1] {
		t.Errorf("IDs should be strictly increasing: got %d, %d", ids[0], ids[1])
	}
	// AUTOINCREMENT starts at 1 for the first row.
	if ids[0] != 1 {
		t.Errorf("first ID: expected 1, got %d", ids[0])
	}
	if ids[1] != 2 {
		t.Errorf("second ID: expected 2, got %d", ids[1])
	}
}

// TestIngestLog_PipelineIntegration verifies that Store.LogIngestRun correctly
// persists all IngestLogEntry fields, including optional SourcePath and
// ErrorMessage, and that they can be read back with correct values.
func TestIngestLog_PipelineIntegration(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	startedAt := int64(1705276800000)  // 2024-01-15T00:00:00Z
	finishedAt := int64(1705276860000) // +60s
	sourcePath := "/home/test/.claude/projects"
	errMsg := "adapter discovery failed for opencode"

	entry := ingest.IngestLogEntry{
		StartedAt:         startedAt,
		FinishedAt:        &finishedAt,
		SessionsNew:       12,
		SessionsUpdated:   3,
		SessionsUnchanged: 50,
		SessionsError:     2,
		IndexedCount:      15,
		ComputedCount:     14,
		ErrorMessage:      &errMsg,
		SourcePath:        &sourcePath,
	}

	if err := s.LogIngestRun(ctx, entry); err != nil {
		t.Fatalf("LogIngestRun: %v", err)
	}

	// Verify by querying the DB directly.
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify row count.
	count := queryInt(t, conn, `SELECT COUNT(*) FROM ingest_log`)
	if count != 1 {
		t.Fatalf("ingest_log count: expected 1, got %d", count)
	}

	// Verify integer fields.
	if got := queryInt(t, conn, `SELECT sessions_new FROM ingest_log WHERE id = 1`); got != 12 {
		t.Errorf("sessions_new: expected 12, got %d", got)
	}
	if got := queryInt(t, conn, `SELECT sessions_updated FROM ingest_log WHERE id = 1`); got != 3 {
		t.Errorf("sessions_updated: expected 3, got %d", got)
	}
	if got := queryInt(t, conn, `SELECT sessions_unchanged FROM ingest_log WHERE id = 1`); got != 50 {
		t.Errorf("sessions_unchanged: expected 50, got %d", got)
	}
	if got := queryInt(t, conn, `SELECT sessions_error FROM ingest_log WHERE id = 1`); got != 2 {
		t.Errorf("sessions_error: expected 2, got %d", got)
	}
	if got := queryInt(t, conn, `SELECT indexed_count FROM ingest_log WHERE id = 1`); got != 15 {
		t.Errorf("indexed_count: expected 15, got %d", got)
	}
	if got := queryInt(t, conn, `SELECT computed_count FROM ingest_log WHERE id = 1`); got != 14 {
		t.Errorf("computed_count: expected 14, got %d", got)
	}

	// Verify started_at and finished_at (int64 values).
	var gotStarted, gotFinished int64
	err := sqlitex.ExecuteTransient(conn, `SELECT started_at, finished_at FROM ingest_log WHERE id = 1`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			gotStarted = stmt.ColumnInt64(0)
			gotFinished = stmt.ColumnInt64(1)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("query timestamps: %v", err)
	}
	if gotStarted != startedAt {
		t.Errorf("started_at: expected %d, got %d", startedAt, gotStarted)
	}
	if gotFinished != finishedAt {
		t.Errorf("finished_at: expected %d, got %d", finishedAt, gotFinished)
	}

	// Verify nullable text fields.
	gotSource := queryText(t, conn, `SELECT source_path FROM ingest_log WHERE id = 1`)
	if gotSource != sourcePath {
		t.Errorf("source_path: expected %q, got %q", sourcePath, gotSource)
	}
	gotErr := queryText(t, conn, `SELECT error_message FROM ingest_log WHERE id = 1`)
	if gotErr != errMsg {
		t.Errorf("error_message: expected %q, got %q", errMsg, gotErr)
	}
}

// TestIngestLog_BestEffortErrorSwallow documents the IngestLogger contract:
// LogIngestRun CAN return errors, but the pipeline treats logging as best-effort
// and ignores them. This test uses the StubIngestLogger from testutil with a
// preset error to verify that errors are faithfully returned (the caller decides
// whether to propagate or swallow).
func TestIngestLog_BestEffortErrorSwallow(t *testing.T) {
	t.Parallel()

	injectedErr := errors.New("simulated DB write failure")
	logger := &testutil.StubIngestLogger{
		Err: injectedErr,
	}

	ctx := context.Background()
	entry := ingest.IngestLogEntry{
		StartedAt:   1705276800000,
		SessionsNew: 1,
	}

	// The stub should return the injected error.
	err := logger.LogIngestRun(ctx, entry)
	if err == nil {
		t.Fatal("expected LogIngestRun to return an error, got nil")
	}
	if !errors.Is(err, injectedErr) {
		t.Errorf("expected error %q, got %q", injectedErr, err)
	}

	// Verify no entries were recorded (error path short-circuits).
	if len(logger.Entries) != 0 {
		t.Errorf("expected 0 logged entries on error, got %d", len(logger.Entries))
	}

	// Document the contract: a successful call DOES record the entry.
	successLogger := &testutil.StubIngestLogger{}
	if err := successLogger.LogIngestRun(ctx, entry); err != nil {
		t.Fatalf("expected nil error on success, got %v", err)
	}
	if len(successLogger.Entries) != 1 {
		t.Errorf("expected 1 logged entry on success, got %d", len(successLogger.Entries))
	}
}
