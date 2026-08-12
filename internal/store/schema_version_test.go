package store

import (
	"path/filepath"
	"testing"
)

// TestCurrentSchemaVersionMatchesFreshDatabase pins the premise every "can I
// reuse this database" check rests on: the version this package reports is the
// version it actually stamps on a database it just created. If the two ever
// drift, a caller comparing them would reject every database it wrote itself
// and silently do the slow thing forever.
func TestCurrentSchemaVersionMatchesFreshDatabase(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "fresh.db")
	s, err := Open(dbPath, WithPoolSize(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	stamped, err := SchemaVersionAt(dbPath)
	if err != nil {
		t.Fatalf("SchemaVersionAt: %v", err)
	}
	if stamped != CurrentSchemaVersion() {
		t.Errorf("a freshly migrated database is stamped at user_version %d, but CurrentSchemaVersion reports %d", stamped, CurrentSchemaVersion())
	}
}

// TestSchemaVersionAtMissingDatabase covers the first-run case: there is no file
// to read a version from, and the caller must be told so rather than handed a
// zero it could mistake for a real version.
func TestSchemaVersionAtMissingDatabase(t *testing.T) {
	t.Parallel()
	if _, err := SchemaVersionAt(filepath.Join(t.TempDir(), "absent.db")); err == nil {
		t.Error("SchemaVersionAt on a missing database returned no error; a caller cannot tell that apart from version 0")
	}
}
