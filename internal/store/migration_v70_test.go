package store

// V70 fixture-driven tests: the fresh schema, the V69 upgrade and its spawns
// backfill, the duplicate (team, name) a transcript may already carry, the
// cascade from a pruned evidence row, the rebuild that replaces both derived
// tables, and the transaction that owns the maintenance write.
//
// The file is package store so it can swap the spawns seam and reuse the
// stored spawn layout. The strict fixture decode lives here directly because
// internal/testutil imports this package.

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/migrations/v70_derived_lookups.yaml
var v70DerivedLookupsFixtureBytes []byte

type v70Expectation struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind"`
}

type v70SpawnRef struct {
	Team string `yaml:"team"`
	Name string `yaml:"name"`
}

type v70Evidence struct {
	SourcePath   string        `yaml:"source_path"`
	Scope        string        `yaml:"scope"`
	IdentityTeam *string       `yaml:"identity_team"`
	IdentityName *string       `yaml:"identity_name"`
	Spawns       []v70SpawnRef `yaml:"spawns"`
}

type v70Spawn struct {
	SpawnerPath string `yaml:"spawner_path"`
	Team        string `yaml:"team"`
	Name        string `yaml:"name"`
}

type v70Rollout struct {
	Path      string `yaml:"path"`
	SessionID string `yaml:"session_id"`
}

type v70Fixture struct {
	RequiredNames   []string         `yaml:"requiredNames"`
	Expectations    []v70Expectation `yaml:"expectations"`
	Evidence        []v70Evidence    `yaml:"evidence"`
	ExpectedSpawns  []v70Spawn       `yaml:"expected_spawns"`
	Walk            []v70Rollout     `yaml:"walk"`
	ExpectedCatalog []v70Rollout     `yaml:"expected_catalog"`
	CorruptSpawns   []v70Spawn       `yaml:"corrupt_spawns"`
	CorruptCatalog  []v70Rollout     `yaml:"corrupt_catalog"`
	DuplicateProbe  v70Spawn         `yaml:"duplicate_probe"`
	PruneProbe      struct {
		SpawnerPath string `yaml:"spawner_path"`
	} `yaml:"prune_probe"`
}

// v70ExpectationKinds is the closed set of assertions the fixture may name.
var v70ExpectationKinds = map[string]struct{}{
	"spawns-backfill":  {},
	"duplicate-dedupe": {},
	"catalog-empty":    {},
	"rebuild-spawns":   {},
	"rebuild-catalog":  {},
	"prune-cascade":    {},
}

func loadV70DerivedLookupsFixture(t *testing.T) v70Fixture {
	t.Helper()
	var fixture v70Fixture
	decoder := yaml.NewDecoder(bytes.NewReader(v70DerivedLookupsFixtureBytes))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode v70 derived-lookups fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("v70 derived-lookups fixture must contain exactly one YAML document: %v", err)
	}
	if len(fixture.RequiredNames) == 0 {
		t.Fatal("v70 derived-lookups fixture declares no required expectation names")
	}
	required := make(map[string]struct{}, len(fixture.RequiredNames))
	for _, name := range fixture.RequiredNames {
		if strings.TrimSpace(name) == "" {
			t.Fatal("v70 derived-lookups fixture declares a blank required name")
		}
		if _, duplicate := required[name]; duplicate {
			t.Fatalf("v70 derived-lookups fixture repeats required name %q", name)
		}
		required[name] = struct{}{}
	}
	actual := make(map[string]struct{}, len(fixture.Expectations))
	for _, expectation := range fixture.Expectations {
		if strings.TrimSpace(expectation.Name) == "" {
			t.Fatal("v70 derived-lookups fixture declares an expectation with a blank name")
		}
		if _, duplicate := actual[expectation.Name]; duplicate {
			t.Fatalf("v70 derived-lookups fixture repeats expectation %q", expectation.Name)
		}
		actual[expectation.Name] = struct{}{}
		if _, ok := v70ExpectationKinds[expectation.Kind]; !ok {
			t.Fatalf("v70 derived-lookups fixture expectation %q names unknown kind %q; use one of the closed kinds", expectation.Name, expectation.Kind)
		}
	}
	for _, name := range fixture.RequiredNames {
		if _, ok := actual[name]; !ok {
			t.Fatalf("v70 derived-lookups fixture requires expectation %q but declares no such row", name)
		}
	}
	for name := range actual {
		if _, ok := required[name]; !ok {
			t.Fatalf("v70 derived-lookups fixture carries undeclared expectation %q; add it to requiredNames", name)
		}
	}
	for _, evidence := range fixture.Evidence {
		if strings.TrimSpace(evidence.SourcePath) == "" {
			t.Fatal("v70 derived-lookups fixture carries evidence with a blank source_path")
		}
		if !ingest.ClaudeEvidenceScope(evidence.Scope).IsValid() {
			t.Fatalf("v70 derived-lookups fixture evidence %q names unknown scope %q", evidence.SourcePath, evidence.Scope)
		}
		if (evidence.IdentityTeam == nil) != (evidence.IdentityName == nil) {
			t.Fatalf("v70 derived-lookups fixture evidence %q sets one half of an identity; set both or neither", evidence.SourcePath)
		}
	}
	if len(fixture.ExpectedSpawns) == 0 {
		t.Fatal("v70 derived-lookups fixture declares no expected spawn rows")
	}
	if len(fixture.Walk) == 0 {
		t.Fatal("v70 derived-lookups fixture declares no enumerated walk")
	}
	if len(fixture.ExpectedCatalog) == 0 {
		t.Fatal("v70 derived-lookups fixture declares no expected catalog rows")
	}
	if len(fixture.CorruptSpawns) == 0 {
		t.Fatal("v70 derived-lookups fixture declares no spawns corruption to repair")
	}
	if len(fixture.CorruptCatalog) == 0 {
		t.Fatal("v70 derived-lookups fixture declares no catalog corruption to repair")
	}
	if fixture.DuplicateProbe.SpawnerPath == "" || fixture.DuplicateProbe.Team == "" || fixture.DuplicateProbe.Name == "" {
		t.Fatal("v70 derived-lookups fixture declares an incomplete duplicate probe")
	}
	if fixture.PruneProbe.SpawnerPath == "" {
		t.Fatal("v70 derived-lookups fixture declares no prune probe")
	}
	return fixture
}

// seedV69WithEvidence installs the predecessor schema and the fixture's
// evidence rows through raw SQL, the way a store installed before V70 exists.
func seedV69WithEvidence(t *testing.T, dbPath string, fixture v70Fixture) {
	t.Helper()
	ctx := t.Context()
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatalf("open the V69 seed database: %v", err)
	}
	if err := sqlitemigration.Migrate(ctx, conn, frozenSchema(69)); err != nil {
		_ = conn.Close()
		t.Fatalf("migrate the seed database to V69: %v", err)
	}
	for _, evidence := range fixture.Evidence {
		spawns := make([]claudeSpawnRow, 0, len(evidence.Spawns))
		for _, spawn := range evidence.Spawns {
			spawns = append(spawns, claudeSpawnRow{Team: spawn.Team, Name: spawn.Name})
		}
		spawnsJSON, err := json.Marshal(spawns)
		if err != nil {
			_ = conn.Close()
			t.Fatalf("encode spawns for %q: %v", evidence.SourcePath, err)
		}
		var team, name any
		if evidence.IdentityTeam != nil {
			team = *evidence.IdentityTeam
			name = *evidence.IdentityName
		}
		if err := sqlitex.ExecuteTransient(conn, `
INSERT INTO claude_transcript_evidence (
    source_path, scope, mod_time_unix_nano, size_bytes, has_conversation,
    identity_team, identity_name, spawns_json, title, branch, cwd
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, &sqlitex.ExecOptions{
			Args: []any{
				evidence.SourcePath, evidence.Scope, int64(1), int64(2), int64(0),
				team, name, string(spawnsJSON), "", "", "",
			},
		}); err != nil {
			_ = conn.Close()
			t.Fatalf("seed evidence %q: %v", evidence.SourcePath, err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close the V69 seed database: %v", err)
	}
}

func formatSpawn(row v70Spawn) string {
	return row.SpawnerPath + "\x00" + row.Team + "\x00" + row.Name
}

func formatRollout(row v70Rollout) string {
	return row.Path + "\x00" + row.SessionID
}

func sortedSpawnRows(rows []v70Spawn) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, formatSpawn(row))
	}
	sort.Strings(out)
	return out
}

func sortedRolloutRows(rows []v70Rollout) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, formatRollout(row))
	}
	sort.Strings(out)
	return out
}

func requireSpawnRowsEqual(t *testing.T, conn *sqlite.Conn, want []v70Spawn) {
	t.Helper()
	var got []v70Spawn
	if err := sqlitex.ExecuteTransient(conn, `SELECT spawner_path, team, name FROM claude_teammate_spawns`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			got = append(got, v70Spawn{SpawnerPath: stmt.ColumnText(0), Team: stmt.ColumnText(1), Name: stmt.ColumnText(2)})
			return nil
		},
	}); err != nil {
		t.Fatalf("read the teammate spawns projection: %v", err)
	}
	wantRows := sortedSpawnRows(want)
	gotRows := sortedSpawnRows(got)
	if strings.Join(wantRows, "\n") != strings.Join(gotRows, "\n") {
		t.Fatalf("claude_teammate_spawns = %q, want %q", gotRows, wantRows)
	}
}

func requireCatalogRowsEqual(t *testing.T, conn *sqlite.Conn, want []v70Rollout) {
	t.Helper()
	var got []v70Rollout
	if err := sqlitex.ExecuteTransient(conn, `SELECT source_path, session_id FROM codex_rollout_catalog`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			got = append(got, v70Rollout{Path: stmt.ColumnText(0), SessionID: stmt.ColumnText(1)})
			return nil
		},
	}); err != nil {
		t.Fatalf("read the Codex rollout catalog: %v", err)
	}
	wantRows := sortedRolloutRows(want)
	gotRows := sortedRolloutRows(got)
	if strings.Join(wantRows, "\n") != strings.Join(gotRows, "\n") {
		t.Fatalf("codex_rollout_catalog = %q, want %q", gotRows, wantRows)
	}
}

func countOnConn(t *testing.T, conn *sqlite.Conn, query string, args ...any) int {
	t.Helper()
	count := 0
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = int(stmt.ColumnInt64(0))
			return nil
		},
	}); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return count
}

// TestMigrationV70DerivedLookups upgrades a seeded V69 store through the real
// Open path and asserts every fixture expectation in order.
func TestMigrationV70DerivedLookups(t *testing.T) {
	fixture := loadV70DerivedLookupsFixture(t)
	dbPath := filepath.Join(t.TempDir(), "v70-upgrade.db")
	seedV69WithEvidence(t, dbPath, fixture)

	// The migration test runs the real upgrade path (and its pragmas), which
	// a skip-less Open performs on the seeded predecessor.
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open the seeded V69 store: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the upgraded store: %v", err)
		}
	}()

	conn, err := db.pool.Take(t.Context())
	if err != nil {
		t.Fatalf("take the upgraded store's connection: %v", err)
	}
	defer db.pool.Put(conn)

	if got := countOnConn(t, conn, `SELECT COUNT(*) FROM claude_teammate_spawns`); got != len(fixture.ExpectedSpawns) {
		t.Fatalf("V70 backfilled %d spawn rows, want %d", got, len(fixture.ExpectedSpawns))
	}

	rollouts := make([]ingest.RolloutFile, 0, len(fixture.Walk))
	for _, row := range fixture.Walk {
		rollouts = append(rollouts, ingest.RolloutFile{
			Path:      ingest.ResolvedPath(row.Path),
			SessionID: ingest.SessionID(row.SessionID),
		})
	}

	for _, expectation := range fixture.Expectations {
		switch expectation.Kind {
		case "spawns-backfill":
			requireSpawnRowsEqual(t, conn, fixture.ExpectedSpawns)
		case "duplicate-dedupe":
			probe := fixture.DuplicateProbe
			got := countOnConn(t, conn,
				`SELECT COUNT(*) FROM claude_teammate_spawns WHERE spawner_path = ? AND team = ? AND name = ?`,
				probe.SpawnerPath, probe.Team, probe.Name)
			if got != 1 {
				t.Fatalf("the repeated (team, name) in %q produced %d rows, want 1", probe.SpawnerPath, got)
			}
		case "catalog-empty":
			if got := countOnConn(t, conn, `SELECT COUNT(*) FROM codex_rollout_catalog`); got != 0 {
				t.Fatalf("V70 backfilled %d Codex catalog rows, want 0: a path the database never stored cannot be recovered", got)
			}
		case "rebuild-spawns":
			for _, row := range fixture.CorruptSpawns {
				if err := sqlitex.Execute(conn,
					`INSERT INTO claude_teammate_spawns (spawner_path, team, name) VALUES (?, ?, ?)`,
					&sqlitex.ExecOptions{Args: []any{row.SpawnerPath, row.Team, row.Name}}); err != nil {
					t.Fatalf("plant corrupt spawn row %v: %v", row, err)
				}
			}
			for _, row := range fixture.CorruptCatalog {
				if err := sqlitex.Execute(conn,
					`INSERT INTO codex_rollout_catalog (source_path, session_id) VALUES (?, ?)`,
					&sqlitex.ExecOptions{Args: []any{row.Path, row.SessionID}}); err != nil {
					t.Fatalf("plant corrupt catalog row %v: %v", row, err)
				}
			}
			db.pool.Put(conn)
			if err := db.RebuildDerivedLookups(t.Context(), rollouts); err != nil {
				t.Fatalf("rebuild the derived lookups: %v", err)
			}
			conn, err = db.pool.Take(t.Context())
			if err != nil {
				t.Fatalf("retake the store connection after the rebuild: %v", err)
			}
			requireSpawnRowsEqual(t, conn, fixture.ExpectedSpawns)
		case "rebuild-catalog":
			requireCatalogRowsEqual(t, conn, fixture.ExpectedCatalog)
		case "prune-cascade":
			pruned := fixture.PruneProbe.SpawnerPath
			if err := sqlitex.ExecuteTransient(conn,
				`DELETE FROM claude_transcript_evidence WHERE source_path = ?`,
				&sqlitex.ExecOptions{Args: []any{pruned}}); err != nil {
				t.Fatalf("prune the evidence row for %q: %v", pruned, err)
			}
			if got := countOnConn(t, conn,
				`SELECT COUNT(*) FROM claude_teammate_spawns WHERE spawner_path = ?`, pruned); got != 0 {
				t.Fatalf("pruning the evidence row for %q left %d spawn rows, want 0 through ON DELETE CASCADE", pruned, got)
			}
		default:
			t.Fatalf("fixture expectation %q names kind %q with no runner", expectation.Name, expectation.Kind)
		}
	}

	// The scope flip behind the projection: a subagent transcript contributes
	// no rows, because only root records record projectable spawns.
	for _, evidence := range fixture.Evidence {
		if evidence.Scope != string(ingest.ClaudeEvidenceScopeSubagent) {
			continue
		}
		if got := countOnConn(t, conn,
			`SELECT COUNT(*) FROM claude_teammate_spawns WHERE spawner_path = ?`, evidence.SourcePath); got != 0 {
			t.Fatalf("the subagent transcript %q holds %d spawn rows, want none", evidence.SourcePath, got)
		}
	}
}

// TestMigrationV70FreshSchema proves a brand-new store carries the V70 objects
// with the ratified shapes and that the schema version advanced by one.
func TestMigrationV70FreshSchema(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "v70-fresh.db")
	// The fresh install is this test's subject, so it opens without skipping
	// migrations and asserts the schema they produce.
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open a fresh store: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the fresh store: %v", err)
		}
	}()

	conn, err := db.pool.Take(t.Context())
	if err != nil {
		t.Fatalf("take the fresh store's connection: %v", err)
	}
	defer db.pool.Put(conn)

	if got, want := CurrentSchemaVersion(), 70; got != want {
		t.Fatalf("CurrentSchemaVersion() = %d, want %d: V70 advances the shipped version by one", got, want)
	}
	var version int
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA user_version`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { version = int(stmt.ColumnInt64(0)); return nil },
	}); err != nil {
		t.Fatalf("read the fresh store's user_version: %v", err)
	}
	if version != 70 {
		t.Fatalf("fresh user_version = %d, want 70", version)
	}

	for _, name := range []string{"claude_teammate_spawns", "codex_rollout_catalog"} {
		strict := 0
		if err := sqlitex.ExecuteTransient(conn,
			`SELECT strict FROM pragma_table_list WHERE name = ? AND schema = 'main'`,
			&sqlitex.ExecOptions{Args: []any{name}, ResultFunc: func(stmt *sqlite.Stmt) error {
				strict = int(stmt.ColumnInt64(0))
				return nil
			}}); err != nil {
			t.Fatalf("read the table classification for %q: %v", name, err)
		}
		if strict != 1 {
			t.Fatalf("table %q is not STRICT", name)
		}
	}
	for _, name := range []string{
		"idx_claude_evidence_identity_root",
		"idx_claude_teammate_spawns_identity",
		"idx_codex_rollout_catalog_session",
	} {
		if got := countOnConn(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name); got != 1 {
			t.Fatalf("index %q is missing from a fresh store", name)
		}
	}

	// The ratified shape: the identity index is partial over root rows, and the
	// session_id index is deliberately non-unique so two paths may share one id.
	partial := ""
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_claude_evidence_identity_root'`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error { partial = stmt.ColumnText(0); return nil }}); err != nil {
		t.Fatalf("read the partial identity index SQL: %v", err)
	}
	if !strings.Contains(partial, "WHERE scope = 'root'") || !strings.Contains(partial, "identity_team IS NOT NULL") {
		t.Fatalf("idx_claude_evidence_identity_root is not the ratified partial index: %s", partial)
	}
	nonUnique := ""
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_codex_rollout_catalog_session'`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error { nonUnique = stmt.ColumnText(0); return nil }}); err != nil {
		t.Fatalf("read the rollout session index SQL: %v", err)
	}
	if strings.Contains(strings.ToUpper(nonUnique), "UNIQUE") {
		t.Fatalf("idx_codex_rollout_catalog_session must stay non-unique so a duplicate id is visible: %s", nonUnique)
	}
}

// TestSaveClaudeEvidenceRollsBackTeammateSpawnsOnFailure proves the evidence
// row and its derived spawn rows share one transaction: the seam fails only
// after the row is visible on the transaction's connection, and neither row
// may survive the rollback.
func TestSaveClaudeEvidenceRollsBackTeammateSpawnsOnFailure(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v70-rollback.db")
	// The production transaction is this test's subject, so the store opens
	// through the shipped migration chain and then writes through SaveClaudeEvidence.
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open the rollback store: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the rollback store: %v", err)
		}
	}()

	path := "/synthetic/claude/rollback.jsonl"
	record := ingest.ClaudeTranscriptEvidence{
		SourcePath:      ingest.ResolvedPath(path),
		Scope:           ingest.ClaudeEvidenceScopeRoot,
		ModTimeUnixNano: 1,
		SizeBytes:       2,
		Origin:          sessionorigin.User,
		ProofVersion:    ingest.ClaudeIdentityProofVersion,
		Spawns:          []ingest.ClaudeTeammateIdentity{{Team: "team-x", Name: "worker-y"}},
	}

	sawEvidenceRow := false
	previous := swapSaveClaudeTeammateSpawns(func(conn *sqlite.Conn, sourcePath string, spawns []ingest.ClaudeTeammateIdentity) error {
		found := 0
		if err := sqlitex.ExecuteTransient(conn,
			`SELECT COUNT(*) FROM claude_transcript_evidence WHERE source_path = ?`,
			&sqlitex.ExecOptions{Args: []any{sourcePath}, ResultFunc: func(stmt *sqlite.Stmt) error {
				found = int(stmt.ColumnInt64(0))
				return nil
			}}); err != nil {
			return fmt.Errorf("inspect the transaction's evidence row: %w", err)
		}
		sawEvidenceRow = found == 1
		return errors.New("injected teammate-spawn write failure")
	})
	defer swapSaveClaudeTeammateSpawns(previous)

	err = db.SaveClaudeEvidence(context.Background(), []ingest.ClaudeTranscriptEvidence{record}, nil)
	if err == nil {
		t.Fatal("SaveClaudeEvidence returned nil despite the injected spawns failure")
	}
	wantFragment := fmt.Sprintf("store.SaveClaudeEvidence: write teammate spawns for transcript %q: injected teammate-spawn write failure", path)
	if !strings.Contains(err.Error(), wantFragment) {
		t.Fatalf("SaveClaudeEvidence error = %q, want it to contain %q", err, wantFragment)
	}
	if !sawEvidenceRow {
		t.Fatal("the spawns seam did not see the evidence row on the transaction's connection; the rollback case proves nothing")
	}

	conn, err := db.pool.Take(t.Context())
	if err != nil {
		t.Fatalf("take the rollback store's connection: %v", err)
	}
	defer db.pool.Put(conn)
	if got := countOnConn(t, conn, `SELECT COUNT(*) FROM claude_transcript_evidence WHERE source_path = ?`, path); got != 0 {
		t.Fatalf("the rolled-back transaction left %d evidence rows for %q, want 0", got, path)
	}
	if got := countOnConn(t, conn, `SELECT COUNT(*) FROM claude_teammate_spawns WHERE spawner_path = ?`, path); got != 0 {
		t.Fatalf("the rolled-back transaction left %d spawn rows for %q, want 0", got, path)
	}
}
