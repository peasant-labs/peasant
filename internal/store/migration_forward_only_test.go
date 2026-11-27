package store

import (
	"bytes"
	_ "embed"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/migration_forward_only_cases.yaml
var migrationForwardOnlyCasesYAML []byte

type migrationForwardOnlyFixture struct {
	RequiredNames []string                   `yaml:"requiredNames"`
	Cases         []migrationForwardOnlyCase `yaml:"cases"`
}

type migrationForwardOnlyCase struct {
	Name string `yaml:"name"`
}

// TestMigrationForwardOnlyNamedCase proves a database stamped by a newer build
// is refused loudly before any migration or write instead of being served.
func TestMigrationForwardOnlyNamedCase(t *testing.T) {
	t.Parallel()
	var fixture migrationForwardOnlyFixture
	decoder := yaml.NewDecoder(bytes.NewReader(migrationForwardOnlyCasesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode migration_forward_only_cases.yaml: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		names = append(names, testCase.Name)
	}
	if err := validateMigrationNames(fixture.RequiredNames, names, "migration_forward_only_cases.yaml"); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) != 1 || fixture.Cases[0].Name != "migration-forward-only" {
		t.Fatalf("cases = %+v, want the migration-forward-only case", fixture.Cases)
	}
	path := filepath.Join(t.TempDir(), "forward-only.db")
	created, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA user_version = `+strconv.Itoa(CurrentSchemaVersion()+1), nil); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("opening a database from a newer schema version succeeded")
	} else if !strings.Contains(err.Error(), "newer Peasant") {
		t.Fatalf("opening a newer-schema database error = %v, want a forward-only refusal", err)
	}
}
