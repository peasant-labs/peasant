package store_test

import (
	_ "embed"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/schema_fresh_vs_migrated.yaml
var freshVsMigratedFamilyYAML []byte

// assertGoldenCaseInFamily confirms golden-template-body-insert is still in
// the schema_fresh_vs_migrated inventory with this test as its documented
// runner, so removing either side fails a gate: removing the name trips the
// family's required-name manifest, and removing this test leaves the family
// entry pointing at a runner that no longer exists.
func assertGoldenCaseInFamily(t *testing.T) {
	t.Helper()
	var family struct {
		Cases []struct {
			Name    string `yaml:"name"`
			Kind    string `yaml:"kind"`
			OwnedBy string `yaml:"ownedBy,omitempty"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(freshVsMigratedFamilyYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&family); err != nil {
		t.Fatalf("decode schema_fresh_vs_migrated.yaml: %v", err)
	}
	for _, c := range family.Cases {
		if c.Name != "golden-template-body-insert" {
			continue
		}
		if c.OwnedBy != "TestGoldenTemplateBodyInsert" {
			t.Fatalf("golden-template-body-insert is owned by %q, want this test %q", c.OwnedBy, "TestGoldenTemplateBodyInsert")
		}
		return
	}
	t.Fatal("schema_fresh_vs_migrated.yaml carries no golden-template-body-insert case")
}

// TestGoldenTemplateBodyInsert proves the storetest golden template — the
// migrated database every storetest.Open copy derives from — allocates its
// first entry body at BodyRowIDBase under the writer contract, exactly like a
// fresh install and a chain-migrated store. The copy's user_version pins the
// template to the build schema, so a template built from the wrong baseline
// fails here instead of passing on a stale schema. This is the golden leg of
// the fresh/migrated/golden trio; the fresh and migrated legs live in
// TestSchemaFreshVsMigrated.
func TestGoldenTemplateBodyInsert(t *testing.T) {
	t.Parallel()
	assertGoldenCaseInFamily(t)
	s := storetest.Open(t)
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	var userVersion int64
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA user_version`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			userVersion = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("PRAGMA user_version on golden copy: %v", err)
	}
	if int(userVersion) != store.CurrentSchemaVersion() {
		t.Fatalf("golden copy user_version = %d, want the build schema %d", userVersion, store.CurrentSchemaVersion())
	}

	const sessionID = "s-golden-first-body"
	storetest.SeedSession(t, s, sessionID)
	digest := strings.Repeat("ab", 32)
	if err := sqlitex.ExecuteTransient(conn, fmt.Sprintf(`
INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role, has_tool_use, has_thinking, is_error, depth)
VALUES ((SELECT coalesce(max(body_id), %d) + 1 FROM session_entry_bodies), ?, ?, 0, 'opencode', 'text', 'user', 0, 0, 0, 0)`, store.BodyRowIDBase-1), &sqlitex.ExecOptions{
		Args: []any{sessionID, digest},
	}); err != nil {
		t.Fatalf("allocate first body on golden copy: %v", err)
	}
	var bodyID int64
	if err := sqlitex.ExecuteTransient(conn, `SELECT body_id FROM session_entry_bodies WHERE session_id=?`, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			bodyID = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("read back first body on golden copy: %v", err)
	}
	if bodyID != store.BodyRowIDBase {
		t.Errorf("golden-template-body-insert: first body_id = %d, want BodyRowIDBase %d", bodyID, store.BodyRowIDBase)
	}
}
