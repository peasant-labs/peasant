package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
)

// TestExportTableRowsCoversEveryOrdinaryTable pins the dump the parity harness
// depends on: every ordinary table is present, its declared columns are in
// declaration order, and a seeded row appears once with storage-class-tagged
// cells. A table silently missing from the export would hide a difference
// between two runs.
func TestExportTableRowsCoversEveryOrdinaryTable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "export.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	entry := makeStoreEntry(t, "40000000-0000-4000-8000-000000000001",
		strings.Repeat("a", 64), "github.com--peasant-labs--peasant",
		defaults.HarnessClaudeCode, 1000, 0, 0)
	if err := db.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	exported, err := ExportTableRows(ctx, db)
	if err != nil {
		t.Fatalf("ExportTableRows: %v", err)
	}
	byName := make(map[string]ExportedTable, len(exported))
	for _, table := range exported {
		byName[table.Name] = table
	}
	for _, required := range []string{"sessions", "session_entries", "projects", "ingest_log", "session_entries_fts_data"} {
		if _, ok := byName[required]; !ok {
			t.Fatalf("export has no %q table; exported %d tables", required, len(exported))
		}
	}
	sessions := byName["sessions"]
	if len(sessions.Rows) != 1 {
		t.Fatalf("sessions has %d exported rows, want the one seeded row", len(sessions.Rows))
	}
	if sessions.Columns[0] != "session_id" {
		t.Fatalf("sessions first column = %q, want session_id in declaration order (columns: %v)", sessions.Columns[0], sessions.Columns)
	}
	if !strings.Contains(strings.Join(sessions.Rows[0], ","), "text:\"40000000-0000-4000-8000-000000000001\"") {
		t.Fatalf("seeded session row does not carry its id token: %v", sessions.Rows[0])
	}

	refused, err := ExportTableRows(ctx, nil)
	if err == nil || refused != nil {
		t.Fatalf("ExportTableRows(nil) = %v, %v; want a refusal", refused, err)
	}
}

// TestExportedTableNormalizedRowsDropsNamedVolatileColumns proves the
// normalization is multiset-based and refuses a volatile column the table does
// not declare: a stale exemption must fail loudly rather than normalize
// nothing.
func TestExportedTableNormalizedRowsDropsNamedVolatileColumns(t *testing.T) {
	t.Parallel()
	table := ExportedTable{
		Name:    "sessions",
		Columns: []string{"session_id", "indexed_at", "n"},
		Rows: [][]string{
			{"text:\"b\"", "int:2", "int:1"},
			{"text:\"a\"", "int:1", "int:1"},
		},
	}
	rows, err := table.NormalizedRows([]string{"indexed_at"})
	if err != nil {
		t.Fatalf("NormalizedRows: %v", err)
	}
	if len(rows) != 2 || rows[0] != "text:\"a\"\x1fint:1" || rows[1] != "text:\"b\"\x1fint:1" {
		t.Fatalf("NormalizedRows = %q, want the two id-and-count rows sorted without indexed_at", rows)
	}
	if _, err := table.NormalizedRows([]string{"renamed_column"}); err == nil {
		t.Fatal("NormalizedRows accepted a volatile column the table does not declare")
	}
}

// TestExportedTableNormalizedRowsKeepsMultiplicity pins that the comparison is
// a multiset, not a set: two identical rows stay two rows. A duplicate-blind
// diff would pass a store that lost one of them.
func TestExportedTableNormalizedRowsKeepsMultiplicity(t *testing.T) {
	t.Parallel()
	table := ExportedTable{
		Name:    "push_log",
		Columns: []string{"session_id", "id"},
		Rows: [][]string{
			{"text:\"x\"", "int:1"},
			{"text:\"x\"", "int:2"},
		},
	}
	rows, err := table.NormalizedRows([]string{"id"})
	if err != nil {
		t.Fatalf("NormalizedRows: %v", err)
	}
	if len(rows) != 2 || rows[0] != rows[1] || rows[0] != "text:\"x\"" {
		t.Fatalf("NormalizedRows = %q, want two identical rows", rows)
	}
}
