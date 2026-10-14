package store

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/schema_fresh_vs_migrated.yaml
var schemaFreshVsMigratedYAML []byte

//go:embed testdata/schema_fresh_vs_migrated.manifest.yaml
var schemaFreshVsMigratedManifestYAML []byte

type freshVsMigratedCase struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind"`
	// OwnedBy names the external runner for cases this white-box family
	// cannot execute: importing storetest here would cycle (storetest
	// imports store), so the golden-template leg runs in
	// TestGoldenTemplateBodyInsert and asserts nothing here. This follows
	// the content_migration ownedBy convention; the manifest check still
	// protects the name.
	OwnedBy string `yaml:"ownedBy,omitempty"`
}

type freshVsMigratedFixtures struct {
	Cases []freshVsMigratedCase `yaml:"cases"`
}

func loadFreshVsMigratedFixtures(t *testing.T) freshVsMigratedFixtures {
	t.Helper()
	var fixtures freshVsMigratedFixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(schemaFreshVsMigratedYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode schema_fresh_vs_migrated.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(schemaFreshVsMigratedManifestYAML)
	if err != nil {
		t.Fatalf("decode schema_fresh_vs_migrated manifest: %v", err)
	}
	actual := make([]string, 0, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		actual = append(actual, c.Name)
		if strings.TrimSpace(c.Kind) == "" {
			t.Fatalf("schema_fresh_vs_migrated.yaml: case %q has a blank kind; name the build under test", c.Name)
		}
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "fresh-vs-migrated"); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// TestSchemaFreshVsMigrated proves a fresh baseline install and a
// chain-migrated store agree on harmonized behavior: the first allocated
// body_id is BodyRowIDBase on each, and the fresh and migrated schemas dump
// identically. The golden-template leg of the trio lives in
// TestGoldenTemplateBodyInsert, which allocates on a real storetest golden
// copy (this white-box package cannot import storetest without a cycle). The
// explicit allocation subquery is the writer contract (no AUTOINCREMENT, no
// sqlite_sequence), spelled here from the same constant the writers use; the
// migration's literal CHECK holds it equal. Cases carrying ownedBy run under
// that external runner and assert nothing here; the loader's manifest check
// still protects their names.
func TestSchemaFreshVsMigrated(t *testing.T) {
	t.Parallel()
	fixtures := loadFreshVsMigratedFixtures(t)
	for _, c := range fixtures.Cases {
		if c.OwnedBy != "" {
			t.Logf("%s: owned by %s; the golden template needs storetest and asserts nothing here", c.Name, c.OwnedBy)
			continue
		}
		switch c.Kind {
		case "fresh-body-id":
			assertFirstBodyID(t, c.Name, freshBodyStore(t))
		case "migrated-body-id":
			assertFirstBodyID(t, c.Name, migratedBodyStore(t))
		case "schema-equality":
			assertFreshEqualsMigrated(t, c.Name)
		default:
			t.Fatalf("unknown fresh-vs-migrated kind %q; add it to the fixture and this runner", c.Kind)
		}
	}
}

func seedBodySession(t *testing.T, conn *sqlite.Conn, sid string) {
	t.Helper()
	if err := sqlitex.ExecuteScript(conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('fresh-host','fresh-host');
INSERT INTO projects(project_hash, canonical_cwd) VALUES('fresh-project','/synthetic/fresh');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version)
VALUES('`+sid+`','opencode','fresh-model','fresh-host','fresh-project',1,2,3,'/synthetic/fresh.jsonl','jsonl',11);`, nil); err != nil {
		t.Fatalf("seed body session: %v", err)
	}
}

func firstAllocatedBodyID(t *testing.T, conn *sqlite.Conn, sid string) int64 {
	t.Helper()
	seedBodySession(t, conn, sid)
	digest := strings.Repeat("ab", 32)
	if err := sqlitex.ExecuteTransient(conn, fmt.Sprintf(`
INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role, has_tool_use, has_thinking, is_error, depth)
VALUES ((SELECT coalesce(max(body_id), %d) + 1 FROM session_entry_bodies), ?, ?, 0, 'opencode', 'text', 'user', 0, 0, 0, 0)`, BodyRowIDBase-1), &sqlitex.ExecOptions{
		Args: []any{sid, digest},
	}); err != nil {
		t.Fatalf("allocate first body: %v", err)
	}
	var bodyID int64
	if err := sqlitex.ExecuteTransient(conn, `SELECT body_id FROM session_entry_bodies WHERE session_id=?`, &sqlitex.ExecOptions{
		Args: []any{sid},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			bodyID = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return bodyID
}

func assertFirstBodyID(t *testing.T, name string, s *Store) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	if got := firstAllocatedBodyID(t, conn, "s-first-body"); got != BodyRowIDBase {
		t.Errorf("%s: first body_id = %d, want BodyRowIDBase %d", name, got, BodyRowIDBase)
	}
}

func freshBodyStore(t *testing.T) *Store {
	t.Helper()
	return openFreshStore(t, filepath.Join(t.TempDir(), "fresh-body.db"))
}

func migratedBodyStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "migrated-body.db")
	buildChainReference(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close migrated store: %v", err)
		}
	})
	return s
}

func assertFreshEqualsMigrated(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	freshPath := filepath.Join(dir, "fresh.db")
	openFreshStore(t, freshPath)
	chainPath := filepath.Join(dir, "chain.db")
	buildChainReference(t, chainPath)
	freshDump, chainDump := dumpDatabase(t, freshPath), dumpDatabase(t, chainPath)
	if !bytes.Equal(freshDump, chainDump) {
		t.Errorf("%s: fresh and migrated schemas dump differently; see TestBaselineSchemaMatchesMigratedChain for the line-level diff", name)
	}
}
