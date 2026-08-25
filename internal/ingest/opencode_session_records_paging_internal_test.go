package ingest

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
)

// TestSessionRecordsPaginateThroughSharedBoundedPage proves that the
// session-record read paginates through the shared bounded page and its cursor,
// returning every session row across bounded pages with no duplicates.
func TestSessionRecordsPaginateThroughSharedBoundedPage(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "legacy-reader-pages")
	path, err := NewOpenCodeSQLiteSourcePath(materialized.Path)
	if err != nil {
		t.Fatalf("validate synthetic source path: %v", err)
	}
	opened, err := OpenOpenCodeSQLiteSource(t.Context(), path, DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("open synthetic source: %v", err)
	}
	defer func() { _ = opened.Close(t.Context()) }()

	pageSize, err := NewOpenCodeCurrentPageSize(1)
	if err != nil {
		t.Fatalf("build bounded page size: %v", err)
	}
	seen := make(map[string]int)
	var cursor *OpenCodeSessionRecordCursor
	pages := 0
	for {
		page, readErr := opened.SessionRecords(t.Context(), OpenCodeSessionRecordPageRequest{PageSize: pageSize, After: cursor})
		if readErr != nil {
			t.Fatalf("read session-record page %d: %v", pages, readErr)
		}
		pages++
		for _, record := range page.Records {
			seen[record.SessionID.String()]++
		}
		if len(page.Records) > 1 {
			t.Fatalf("bounded page returned %d rows above the page size of 1", len(page.Records))
		}
		if page.Next == nil {
			break
		}
		cursor = page.Next
		if pages > 10 {
			t.Fatal("session-record pagination did not terminate")
		}
	}
	if len(seen) != 2 || seen["ses_reader_a"] != 1 || seen["ses_reader_z"] != 1 {
		t.Fatalf("paginated session rows = %v, want each of the two sessions exactly once", seen)
	}
	if pages < 2 {
		t.Fatalf("pagination produced %d pages, want at least 2 for two sessions at page size 1", pages)
	}
}
