package harnesslayout

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// writeSQLiteFixture creates a synthetic SQLite database read-write and runs
// each statement. Probes never open a database this way; it only builds test
// stores.
func writeSQLiteFixture(t *testing.T, path string, statements []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite|sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// One transaction without fsync: the database is a throwaway test file.
	statements = append(append([]string{"PRAGMA synchronous = OFF", "BEGIN"}, statements...), "COMMIT")
	for _, statement := range statements {
		if err := sqlitex.Execute(conn, statement, nil); err != nil {
			t.Fatalf("build %s: %s: %v", filepath.Base(path), statement, err)
		}
	}
}
