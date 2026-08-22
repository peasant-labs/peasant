package ingest_test

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestSessionRecordsReportOneDropPerBadRowAcrossPages proves that a single
// undecodable session row is reported once, not once per page. The bad row is
// the last row of the first page, so the earlier cursor design re-fetched it on
// the next page and counted the same drop twice.
func TestSessionRecordsReportOneDropPerBadRowAcrossPages(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "session-records-degrade")
	// The second session row by identifier order carries a non-integer clock, so
	// its identifier stays valid but the row is dropped. With page size one it is
	// the sentinel row of the first page.
	const corrupt = "ses_3cd91f52effeXd3QAJ54jOyzB2"
	withCanonicalConnection(t, materialized.Path, func(connection *sqlite.Conn) error {
		return sqlitex.Execute(connection, `UPDATE session SET time_updated = 'not-an-integer' WHERE id = ?1`, &sqlitex.ExecOptions{Args: []any{corrupt}})
	})

	path, err := ingest.NewOpenCodeSQLiteSourcePath(materialized.Path)
	if err != nil {
		t.Fatalf("validate synthetic source path: %v", err)
	}
	source, err := ingest.OpenOpenCodeSQLiteSource(t.Context(), path, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("open synthetic source: %v", err)
	}
	defer func() { _ = source.Close(t.Context()) }()

	pageSize, err := ingest.NewOpenCodeCurrentPageSize(1)
	if err != nil {
		t.Fatalf("build bounded page size: %v", err)
	}

	drops := 0
	var cursor *ingest.OpenCodeSessionRecordCursor
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("session-record pagination did not terminate")
		}
		page, readErr := source.SessionRecords(t.Context(), ingest.OpenCodeSessionRecordPageRequest{PageSize: pageSize, After: cursor})
		if readErr != nil {
			t.Fatalf("read session-record page %d: %v", pages, readErr)
		}
		drops += len(page.Skipped)
		if page.Next == nil {
			break
		}
		cursor = page.Next
	}

	if drops != 1 {
		t.Fatalf("one corrupt session row reported %d drops across pages, want exactly one", drops)
	}
}
