package ingest

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
)

// readAllSessionRecords drains every bounded session-record page of one source
// and returns the records keyed by session identifier.
func readAllSessionRecords(t *testing.T, fixtureName string) map[string]OpenCodeSessionRecord {
	t.Helper()
	materialized := testfixture.MaterializeByName(t, fixtureName)
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
	records := make(map[string]OpenCodeSessionRecord)
	var cursor *OpenCodeSessionRecordCursor
	pages := 0
	for {
		page, readErr := opened.SessionRecords(t.Context(), OpenCodeSessionRecordPageRequest{PageSize: pageSize, After: cursor})
		if readErr != nil {
			t.Fatalf("read session-record page %d: %v", pages, readErr)
		}
		if !page.Supported || !page.HasParent || !page.HasClock {
			t.Fatalf("session-record page %d support = supported %t parent %t clock %t, want all true", pages, page.Supported, page.HasParent, page.HasClock)
		}
		for _, record := range page.Records {
			records[record.SessionID.String()] = record
		}
		pages++
		if page.Next == nil {
			break
		}
		cursor = page.Next
		if pages > 20 {
			t.Fatal("session-record pagination did not terminate")
		}
	}
	return records
}

// TestSessionRecordsReadAttributionColumns proves the bounded session-record
// read attributes each session to its working directory, title, and creation
// time when the session table carries those columns.
func TestSessionRecordsReadAttributionColumns(t *testing.T) {
	records := readAllSessionRecords(t, "hybrid-attribution")

	legacyWinner, ok := records["ses_3cd91f52effeXd3QAJ54jOyzL1"]
	if !ok {
		t.Fatalf("session records = %v, want the legacy-only winner", records)
	}
	if legacyWinner.Directory != "/home/dev/peasant-labs/garden" || legacyWinner.Title != "legacy winner attribution" || legacyWinner.TimeCreated != 3000 {
		t.Fatalf("legacy winner attribution = directory %q title %q created %d, want the fixture attribution", legacyWinner.Directory, legacyWinner.Title, legacyWinner.TimeCreated)
	}

	currentWinner, ok := records["ses_3cd91f52effeXd3QAJ54jOyzL2"]
	if !ok {
		t.Fatalf("session records = %v, want the current winner", records)
	}
	if currentWinner.Directory != "/home/dev/peasant-labs/tool" || currentWinner.Title != "current winner attribution" || currentWinner.TimeCreated != 3010 {
		t.Fatalf("current winner attribution = directory %q title %q created %d, want the fixture attribution", currentWinner.Directory, currentWinner.Title, currentWinner.TimeCreated)
	}
}

// TestSessionRecordsWithoutAttributionColumnsYieldEmptyFields proves that a
// session table that lacks the attribution columns reports empty directory and
// title and a zero creation time rather than failing the read.
func TestSessionRecordsWithoutAttributionColumnsYieldEmptyFields(t *testing.T) {
	records := readAllSessionRecords(t, "hybrid-catalog")
	if len(records) == 0 {
		t.Fatal("hybrid catalog produced no session records")
	}
	for id, record := range records {
		if record.Directory != "" || record.Title != "" || record.TimeCreated != 0 {
			t.Fatalf("session %q without attribution columns = directory %q title %q created %d, want empty attribution", id, record.Directory, record.Title, record.TimeCreated)
		}
	}
}
