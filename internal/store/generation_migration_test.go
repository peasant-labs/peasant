package store

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/generation_migration.yaml
var generationMigrationYAML []byte

//go:embed testdata/generation_migration.manifest.yaml
var generationMigrationManifestYAML []byte

type migrationFixture struct {
	Cases []struct {
		Name     string `yaml:"name"`
		Accepted []any  `yaml:"accepted"`
		Rejected []any  `yaml:"rejected"`
	} `yaml:"cases"`
	TargetStateRejects []string `yaml:"target_state_rejects"`
}

func loadMigrationFixture(t *testing.T) migrationFixture {
	t.Helper()
	var fixture migrationFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(generationMigrationYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode generation_migration.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(generationMigrationManifestYAML)
	if err != nil {
		t.Fatalf("decode generation_migration manifest: %v", err)
	}
	actual := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "generation migration"); err != nil {
		t.Fatal(err)
	}
	if len(fixture.TargetStateRejects) == 0 {
		t.Fatal("generation_migration.yaml carries no target-state rejects")
	}
	return fixture
}

// TestMigrationV60ManagedGenerationCatalog pins the closed sets and nullable
// count checks the V2 activation catalog depends on. The relationship target
// state CHECK is extracted from the live schema and compared for exact set
// equality against schema.AllRelationshipTargetStates, and every count check is
// driven from the typed named fixture (including fractional and non-numeric
// rejection), so a sampled inline list cannot pass while the SQL set drifts.
func TestMigrationV60ManagedGenerationCatalog(t *testing.T) {
	fixture := loadMigrationFixture(t)
	s, _ := openGenerationStore(t)
	ctx := context.Background()
	conn, err := s.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)

	tables := map[string]bool{}
	if err := sqlitex.ExecuteTransient(conn, `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'session_%'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { tables[stmt.ColumnText(0)] = true; return nil },
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"session_projection_generations", "session_relationship_evidence", "session_projection_sections",
		"session_projection_entries", "session_context_segments", "session_projection_content", "session_projection_aliases",
	} {
		if !tables[name] {
			t.Errorf("migration V60 did not create table %s", name)
		}
	}

	// The SQL CHECK must admit exactly the schema-owned target-state set, not a
	// sample of it.
	checkSet := relationshipTargetStateCheckSet(t, conn)
	schemaSet := map[string]struct{}{}
	for _, state := range schema.AllRelationshipTargetStates {
		schemaSet[string(state)] = struct{}{}
	}
	for state := range schemaSet {
		if _, ok := checkSet[state]; !ok {
			t.Errorf("target_state CHECK is missing schema-owned state %q", state)
		}
	}
	for state := range checkSet {
		if _, ok := schemaSet[state]; !ok {
			t.Errorf("target_state CHECK admits undeclared state %q; the SQL set must equal schema.AllRelationshipTargetStates", state)
		}
	}

	sid := "33333333-3333-4333-8333-333333333333"
	seedGenerationSession(t, s, sid)
	digest := strings.Repeat("0", 64)

	insertGeneration := func(count any) error {
		return sqlitex.ExecuteTransient(conn, `INSERT INTO session_projection_generations
 (session_id, generation_id, metadata_json, source_evidence_digest, completeness, index_format_version, installed_at_ms, input_submission_count)
 VALUES (?, 'gen-check', '{}', ?, 'complete', 2, 1, ?)`, &sqlitex.ExecOptions{Args: []any{sid, digest, count}})
	}
	updateSessionCount := func(count any) error {
		return sqlitex.ExecuteTransient(conn, `UPDATE sessions SET input_submission_count = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{count, sid}})
	}
	for _, tc := range fixture.Cases {
		apply := insertGeneration
		if tc.Name == "sessions-input-count" {
			apply = updateSessionCount
		} else if tc.Name != "generation-input-count" {
			t.Fatalf("unknown migration count scenario %q; add it to the fixture, the required-names manifest and this runner", tc.Name)
		}
		for _, accepted := range tc.Accepted {
			if err := apply(accepted); err != nil {
				t.Errorf("%s: input_submission_count %v was rejected: %v", tc.Name, accepted, err)
			}
			_ = sqlitex.ExecuteTransient(conn, `DELETE FROM session_projection_generations WHERE generation_id='gen-check'`, nil)
		}
		for _, rejected := range tc.Rejected {
			if err := apply(rejected); err == nil {
				t.Errorf("%s: input_submission_count %v was accepted; the nullable count check must reject it", tc.Name, rejected)
			}
		}
	}
	_ = sqlitex.ExecuteTransient(conn, `UPDATE sessions SET input_submission_count = NULL WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{sid}})

	for _, state := range schema.AllRelationshipTargetStates {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_relationship_evidence (session_id, generation_id, kind, target_state) VALUES (?, 'gen-check', 'started_by', ?)`, &sqlitex.ExecOptions{Args: []any{sid, string(state)}}); err != nil {
			t.Errorf("relationship target state %q was rejected: %v", state, err)
		}
		if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_relationship_evidence WHERE generation_id='gen-check'`, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, state := range fixture.TargetStateRejects {
		if _, valid := schemaSet[state]; valid {
			t.Fatalf("fixture target-state reject %q is actually schema-owned", state)
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_relationship_evidence (session_id, generation_id, kind, target_state) VALUES (?, 'gen-check', 'started_by', ?)`, &sqlitex.ExecOptions{Args: []any{sid, state}}); err == nil {
			t.Errorf("relationship target state %q was accepted; the CHECK must admit exactly the schema set", state)
		}
	}
}

// relationshipTargetStateCheckSet extracts the target_state IN list from the
// live session_relationship_evidence table definition so the test compares the
// SQL closed set itself rather than a maintained copy.
func relationshipTargetStateCheckSet(t *testing.T, conn *sqlite.Conn) map[string]struct{} {
	t.Helper()
	definition := ""
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT sql FROM sqlite_master WHERE type='table' AND name='session_relationship_evidence'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			definition = stmt.ColumnText(0)
			found = true
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("session_relationship_evidence table definition is missing")
	}
	marker := "target_state IN"
	index := strings.Index(definition, marker)
	if index < 0 {
		t.Fatalf("target_state CHECK not found in %q", definition)
	}
	rest := definition[index+len(marker):]
	open := strings.Index(rest, "(")
	if open < 0 {
		t.Fatalf("target_state CHECK has no parenthesized IN list: %q", definition)
	}
	rest = rest[open+1:]
	close := strings.Index(rest, ")")
	if close < 0 {
		t.Fatalf("target_state CHECK IN list is unterminated: %q", definition)
	}
	set := map[string]struct{}{}
	for _, part := range strings.Split(rest[:close], ",") {
		value := strings.Trim(strings.TrimSpace(part), "'")
		if value == "" {
			continue
		}
		set[value] = struct{}{}
	}
	if len(set) == 0 {
		t.Fatalf("target_state CHECK IN list parsed empty: %q", definition)
	}
	return set
}
