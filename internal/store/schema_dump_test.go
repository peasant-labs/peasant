package store

import (
	"bytes"
	"path/filepath"
	"testing"

	"zombiezen.com/go/sqlite/sqlitex"
)

// TestBaselineShadowRowSensitivity proves the canonical comparison reads FTS5
// shadow rows. Two fresh baseline databases start from identical dumps; a
// single valid mutation of one shadow row makes the mutated dump differ. If
// the dump excluded shadow rows the mutation would be invisible and this test
// would fail.
func TestBaselineShadowRowSensitivity(t *testing.T) {
	fixtures, err := LoadBaselineOpenCaseFixtures()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateBaselineOpenCaseFixtures(fixtures); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	referencePath := filepath.Join(dir, "reference.db")
	openFreshStore(t, referencePath)
	referenceDump := dumpDatabase(t, referencePath)

	for _, mutation := range fixtures.ShadowMutations.Mutations {
		mutation := mutation
		t.Run(mutation.Name, func(t *testing.T) {
			mutatedPath := filepath.Join(t.TempDir(), "mutated.db")
			store := openFreshStore(t, mutatedPath)
			if got := dumpDatabase(t, mutatedPath); !bytes.Equal(got, referenceDump) {
				t.Fatal("two fresh baseline databases differ before the mutation; the sensitivity check needs identical starting dumps")
			}

			conn, err := store.Pool().Take(t.Context())
			if err != nil {
				t.Fatalf("take a connection from the mutated store: %v", err)
			}
			meta, err := loadTableMeta(conn)
			if err != nil {
				store.Pool().Put(conn)
				t.Fatal(err)
			}
			if kind := meta[mutation.Table].Type; kind != "shadow" {
				store.Pool().Put(conn)
				t.Fatalf("fixture mutation %q names table %q, whose pragma_table_list type is %q; a shadow sensitivity case must mutate a shadow table", mutation.Name, mutation.Table, kind)
			}
			if err := sqlitex.ExecuteTransient(conn, mutation.Statement, nil); err != nil {
				store.Pool().Put(conn)
				t.Fatalf("apply fixture mutation %q: %v", mutation.Name, err)
			}
			store.Pool().Put(conn)

			if got := dumpDatabase(t, mutatedPath); bytes.Equal(got, referenceDump) {
				t.Fatalf("the canonical dump ignored shadow mutation %q on table %q; the comparison must read every shadow row", mutation.Name, mutation.Table)
			}
		})
	}
}
