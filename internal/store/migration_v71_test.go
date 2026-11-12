package store

// V70 fixture-driven tests: the fresh schema, the V69 upgrade and its spawns
// backfill, the duplicate (team, name) a transcript may already carry, the
// maintenance writer behind SaveClaudeEvidence, the cascade from a pruned
// evidence row, and the rebuild that replaces both derived tables.
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

// v70MaintenanceStep is one save driven through the production writer. It
// carries the exact projection that save must leave, so a wrong row or a
// missing delete fails the step.
type v70MaintenanceStep struct {
	Name           string        `yaml:"name"`
	SourcePath     string        `yaml:"source_path"`
	Scope          string        `yaml:"scope"`
	Spawns         []v70SpawnRef `yaml:"spawns"`
	ExpectedSpawns []v70Spawn    `yaml:"expected_spawns"`
}

type v70Fixture struct {
	RequiredNames            []string             `yaml:"requiredNames"`
	Expectations             []v70Expectation     `yaml:"expectations"`
	Evidence                 []v70Evidence        `yaml:"evidence"`
	ExpectedSpawns           []v70Spawn           `yaml:"expected_spawns"`
	Walk                     []v70Rollout         `yaml:"walk"`
	ExpectedCatalog          []v70Rollout         `yaml:"expected_catalog"`
	ExpectedCatalogOnUpgrade []v70Rollout         `yaml:"expected_catalog_on_upgrade"`
	InvalidWalkEntry         v70Rollout           `yaml:"invalid_walk_entry"`
	CorruptSpawns            []v70Spawn           `yaml:"corrupt_spawns"`
	CorruptCatalog           []v70Rollout         `yaml:"corrupt_catalog"`
	DuplicateProbe           v70Spawn             `yaml:"duplicate_probe"`
	RequiredMaintenanceNames []string             `yaml:"required_maintenance_names"`
	MaintenanceSteps         []v70MaintenanceStep `yaml:"maintenance_steps"`
	PruneProbe               struct {
		SpawnerPath string `yaml:"spawner_path"`
	} `yaml:"prune_probe"`
}

// v70ExpectationKinds is the closed set of assertions the fixture may name.
var v70ExpectationKinds = map[string]struct{}{
	"spawns-backfill":                 {},
	"duplicate-dedupe":                {},
	"catalog-empty":                   {},
	"rebuild-refuses-incomplete-walk": {},
	"rebuild-spawns":                  {},
	"rebuild-catalog":                 {},
	"prune-cascade":                   {},
}

// requireV70NameSet fails on a blank, duplicate, missing, or undeclared name,
// so each fixture section is protected by an exact name set rather than a
// count.
func requireV70NameSet(t *testing.T, label string, required, actual []string) {
	t.Helper()
	if len(required) == 0 {
		t.Fatalf("v70 derived-lookups fixture declares no required %s names", label)
	}
	requiredSet := make(map[string]struct{}, len(required))
	for _, name := range required {
		if strings.TrimSpace(name) == "" {
			t.Fatalf("v70 derived-lookups fixture declares a blank required %s name", label)
		}
		if _, duplicate := requiredSet[name]; duplicate {
			t.Fatalf("v70 derived-lookups fixture repeats required %s name %q", label, name)
		}
		requiredSet[name] = struct{}{}
	}
	actualSet := make(map[string]struct{}, len(actual))
	for _, name := range actual {
		if strings.TrimSpace(name) == "" {
			t.Fatalf("v70 derived-lookups fixture declares a %s row with a blank name", label)
		}
		if _, duplicate := actualSet[name]; duplicate {
			t.Fatalf("v70 derived-lookups fixture repeats %s %q", label, name)
		}
		actualSet[name] = struct{}{}
	}
	for _, name := range required {
		if _, ok := actualSet[name]; !ok {
			t.Fatalf("v70 derived-lookups fixture requires %s %q but declares no such row", label, name)
		}
	}
	for name := range actualSet {
		if _, ok := requiredSet[name]; !ok {
			t.Fatalf("v70 derived-lookups fixture carries undeclared %s %q; add it to its required name list", label, name)
		}
	}
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

	expectationNames := make([]string, 0, len(fixture.Expectations))
	for _, expectation := range fixture.Expectations {
		expectationNames = append(expectationNames, expectation.Name)
		if _, ok := v70ExpectationKinds[expectation.Kind]; !ok {
			t.Fatalf("v70 derived-lookups fixture expectation %q names unknown kind %q; use one of the closed kinds", expectation.Name, expectation.Kind)
		}
	}
	requireV70NameSet(t, "expectation", fixture.RequiredNames, expectationNames)

	maintenanceNames := make([]string, 0, len(fixture.MaintenanceSteps))
	for _, step := range fixture.MaintenanceSteps {
		maintenanceNames = append(maintenanceNames, step.Name)
		if strings.TrimSpace(step.SourcePath) == "" {
			t.Fatalf("v70 derived-lookups maintenance step %q carries a blank source_path", step.Name)
		}
		if !ingest.ClaudeEvidenceScope(step.Scope).IsValid() {
			t.Fatalf("v70 derived-lookups maintenance step %q names unknown scope %q", step.Name, step.Scope)
		}
		// An empty expected projection is meaningful: the scope-flip step ends
		// with no rows, so only the row fields are validated here.
		for _, row := range step.ExpectedSpawns {
			if strings.TrimSpace(row.SpawnerPath) == "" {
				t.Fatalf("v70 derived-lookups maintenance step %q expects a row with a blank spawner_path", step.Name)
			}
		}
	}
	requireV70NameSet(t, "maintenance", fixture.RequiredMaintenanceNames, maintenanceNames)

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
	if strings.TrimSpace(fixture.InvalidWalkEntry.Path) == "" {
		t.Fatal("v70 derived-lookups fixture declares no incomplete walk entry")
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
		if err := sqlitex.Execute(conn,
			`INSERT INTO claude_transcript_evidence (
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

// withV70Conn scopes one connection to one assertion, so no test puts a
// connection back twice and the pool size never depends on the test body.
func withV70Conn(t *testing.T, db *Store, fn func(*sqlite.Conn)) {
	t.Helper()
	conn, err := db.pool.Take(t.Context())
	if err != nil {
		t.Fatalf("take a store connection: %v", err)
	}
	defer db.pool.Put(conn)
	fn(conn)
}

func readSpawnRows(t *testing.T, db *Store) []v70Spawn {
	t.Helper()
	var rows []v70Spawn
	withV70Conn(t, db, func(conn *sqlite.Conn) {
		if err := sqlitex.ExecuteTransient(conn, `SELECT spawner_path, team, name FROM claude_teammate_spawns`, &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				rows = append(rows, v70Spawn{SpawnerPath: stmt.ColumnText(0), Team: stmt.ColumnText(1), Name: stmt.ColumnText(2)})
				return nil
			},
		}); err != nil {
			t.Fatalf("read the teammate spawns projection: %v", err)
		}
	})
	return rows
}

func readCatalogRows(t *testing.T, db *Store) []v70Rollout {
	t.Helper()
	var rows []v70Rollout
	withV70Conn(t, db, func(conn *sqlite.Conn) {
		if err := sqlitex.ExecuteTransient(conn, `SELECT source_path, session_id FROM codex_rollout_catalog`, &sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				rows = append(rows, v70Rollout{Path: stmt.ColumnText(0), SessionID: stmt.ColumnText(1)})
				return nil
			},
		}); err != nil {
			t.Fatalf("read the Codex rollout catalog: %v", err)
		}
	})
	return rows
}

func requireSpawnRowsEqual(t *testing.T, db *Store, want []v70Spawn) {
	t.Helper()
	wantRows := sortedSpawnRows(want)
	gotRows := sortedSpawnRows(readSpawnRows(t, db))
	if strings.Join(wantRows, "\n") != strings.Join(gotRows, "\n") {
		t.Fatalf("claude_teammate_spawns = %q, want %q", gotRows, wantRows)
	}
}

func requireCatalogRowsEqual(t *testing.T, db *Store, want []v70Rollout) {
	t.Helper()
	wantRows := sortedRolloutRows(want)
	gotRows := sortedRolloutRows(readCatalogRows(t, db))
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

func countOnStore(t *testing.T, db *Store, query string, args ...any) int {
	t.Helper()
	count := 0
	withV70Conn(t, db, func(conn *sqlite.Conn) {
		count = countOnConn(t, conn, query, args...)
	})
	return count
}

// plantCorruptDerivedState writes rows no source derives. INSERT OR IGNORE
// keeps the planting idempotent so two expectations can share it.
func plantCorruptDerivedState(t *testing.T, db *Store, fixture v70Fixture) {
	t.Helper()
	withV70Conn(t, db, func(conn *sqlite.Conn) {
		for _, row := range fixture.CorruptSpawns {
			if err := sqlitex.Execute(conn,
				`INSERT OR IGNORE INTO claude_teammate_spawns (spawner_path, team, name) VALUES (?, ?, ?)`,
				&sqlitex.ExecOptions{Args: []any{row.SpawnerPath, row.Team, row.Name}}); err != nil {
				t.Fatalf("plant corrupt spawn row %v: %v", row, err)
			}
		}
		for _, row := range fixture.CorruptCatalog {
			if err := sqlitex.Execute(conn,
				`INSERT OR IGNORE INTO codex_rollout_catalog (source_path, session_id) VALUES (?, ?)`,
				&sqlitex.ExecOptions{Args: []any{row.Path, row.SessionID}}); err != nil {
				t.Fatalf("plant corrupt catalog row %v: %v", row, err)
			}
		}
	})
}

func fixtureRolloutFiles(fixture v70Fixture) []ingest.RolloutFile {
	rollouts := make([]ingest.RolloutFile, 0, len(fixture.Walk))
	for _, row := range fixture.Walk {
		rollouts = append(rollouts, ingest.RolloutFile{
			Path:      ingest.ResolvedPath(row.Path),
			SessionID: ingest.SessionID(row.SessionID),
		})
	}
	return rollouts
}

// TestMigrationV71DerivedLookups upgrades a seeded V69 store through the real
// Open path and asserts every fixture expectation in order.
func TestMigrationV71DerivedLookups(t *testing.T) {
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

	rollouts := fixtureRolloutFiles(fixture)
	incomplete := append(append([]ingest.RolloutFile{}, rollouts...), ingest.RolloutFile{
		Path: ingest.ResolvedPath(fixture.InvalidWalkEntry.Path),
		// SessionID is deliberately empty: the fixture names the incomplete entry.
	})

	for _, expectation := range fixture.Expectations {
		switch expectation.Kind {
		case "spawns-backfill":
			requireSpawnRowsEqual(t, db, fixture.ExpectedSpawns)
		case "duplicate-dedupe":
			probe := fixture.DuplicateProbe
			got := countOnStore(t, db,
				`SELECT COUNT(*) FROM claude_teammate_spawns WHERE spawner_path = ? AND team = ? AND name = ?`,
				probe.SpawnerPath, probe.Team, probe.Name)
			if got != 1 {
				t.Fatalf("the repeated (team, name) in %q produced %d rows, want 1", probe.SpawnerPath, got)
			}
		case "catalog-empty":
			requireCatalogRowsEqual(t, db, fixture.ExpectedCatalogOnUpgrade)
		case "rebuild-refuses-incomplete-walk":
			// The planting is what makes the rollback observable: a rebuild that
			// cleared the tables and did not roll back would replace these rows.
			plantCorruptDerivedState(t, db, fixture)
			beforeSpawns := readSpawnRows(t, db)
			beforeCatalog := readCatalogRows(t, db)
			rebuildErr := db.RebuildDerivedLookups(t.Context(), ingest.RolloutEnumeration{Status: ingest.RolloutEnumerationComplete, Files: incomplete})
			if rebuildErr == nil {
				t.Fatal("RebuildDerivedLookups accepted a walk entry with an empty session id")
			}
			if !strings.Contains(rebuildErr.Error(), "empty session id") {
				t.Fatalf("RebuildDerivedLookups error = %q, want it to name the empty session id", rebuildErr)
			}
			requireSpawnRowsEqual(t, db, beforeSpawns)
			requireCatalogRowsEqual(t, db, beforeCatalog)
		case "rebuild-spawns":
			plantCorruptDerivedState(t, db, fixture)
			beforeSpawns := readSpawnRows(t, db)
			beforeCatalog := readCatalogRows(t, db)
			if err := db.RebuildDerivedLookups(t.Context(), ingest.RolloutEnumeration{}); !errors.Is(err, ingest.ErrIncompleteRolloutEnumeration) {
				t.Fatalf("unset enumeration error = %v", err)
			}
			requireSpawnRowsEqual(t, db, beforeSpawns)
			requireCatalogRowsEqual(t, db, beforeCatalog)
			if err := db.RebuildDerivedLookups(t.Context(), ingest.RolloutEnumeration{Status: ingest.RolloutEnumerationNotWalked}); err != nil {
				t.Fatalf("rebuild without Codex: %v", err)
			}
			requireSpawnRowsEqual(t, db, fixture.ExpectedSpawns)
			requireCatalogRowsEqual(t, db, beforeCatalog)
			var deletes []ingest.ResolvedPath
			for _, row := range fixture.CorruptCatalog {
				deletes = append(deletes, ingest.ResolvedPath(row.Path))
			}
			if err := db.RebuildDerivedLookups(t.Context(), ingest.RolloutEnumeration{Status: ingest.RolloutEnumerationComplete, Files: rollouts, Deletes: deletes}); err != nil {
				t.Fatalf("rebuild the derived lookups: %v", err)
			}
			requireSpawnRowsEqual(t, db, fixture.ExpectedSpawns)
		case "rebuild-catalog":
			// Explicitly deleted paths are removed alongside the upserts.
			requireCatalogRowsEqual(t, db, fixture.ExpectedCatalog)
		case "prune-cascade":
			pruned := fixture.PruneProbe.SpawnerPath
			if err := db.SaveClaudeEvidence(t.Context(), nil, []ingest.ResolvedPath{ingest.ResolvedPath(pruned)}); err != nil {
				t.Fatalf("prune the evidence row for %q through the evidence cache: %v", pruned, err)
			}
			if got := countOnStore(t, db, `SELECT COUNT(*) FROM claude_transcript_evidence WHERE source_path = ?`, pruned); got != 0 {
				t.Fatalf("the evidence cache left %d evidence rows for the pruned path %q, want 0", got, pruned)
			}
			if got := countOnStore(t, db, `SELECT COUNT(*) FROM claude_teammate_spawns WHERE spawner_path = ?`, pruned); got != 0 {
				t.Fatalf("the spawn rows for pruned path %q survived its evidence row's deletion: %d rows remain, want 0; the derived table was not cleared with its parent", pruned, got)
			}
		default:
			t.Fatalf("fixture expectation %q names kind %q with no runner", expectation.Name, expectation.Kind)
		}
	}

	// The scope filter behind the projection: the subagent transcript records
	// spawns, and only a root record may project them.
	for _, evidence := range fixture.Evidence {
		if evidence.Scope != string(ingest.ClaudeEvidenceScopeSubagent) {
			continue
		}
		if got := countOnStore(t, db,
			`SELECT COUNT(*) FROM claude_teammate_spawns WHERE spawner_path = ?`, evidence.SourcePath); got != 0 {
			t.Fatalf("the subagent transcript %q holds %d spawn rows, want none", evidence.SourcePath, got)
		}
	}
}

// TestSaveClaudeEvidenceWritesTeammateSpawns drives the production writer and
// compares the whole projection after each save against the fixture's
// expectations: a root save, a re-save that drops a pair, a subagent save
// whose non-empty spawns must not be projected, and a scope flip that ends
// with no rows. A recorder delegates to the production writer, so the row
// assertions still observe it, and it also proves the maintenance runs for a
// non-root upsert: skipping it leaves the recorder short one call.
func TestSaveClaudeEvidenceWritesTeammateSpawns(t *testing.T) {
	fixture := loadV70DerivedLookupsFixture(t)
	dbPath := filepath.Join(t.TempDir(), "v70-maintenance.db")
	// The maintenance write is this test's subject, so the store opens through
	// the shipped migration chain and then writes through SaveClaudeEvidence.
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open the maintenance store: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the maintenance store: %v", err)
		}
	}()

	type spawnCall struct {
		Path   string
		Spawns []ingest.ClaudeTeammateIdentity
	}
	var calls []spawnCall
	previous := swapSaveClaudeTeammateSpawns(func(conn *sqlite.Conn, sourcePath string, spawns []ingest.ClaudeTeammateIdentity) error {
		calls = append(calls, spawnCall{Path: sourcePath, Spawns: append([]ingest.ClaudeTeammateIdentity(nil), spawns...)})
		return writeClaudeTeammateSpawns(conn, sourcePath, spawns)
	})
	defer swapSaveClaudeTeammateSpawns(previous)

	for _, step := range fixture.MaintenanceSteps {
		path, err := ingest.NewResolvedPath(step.SourcePath)
		if err != nil {
			t.Fatalf("maintenance step %q names an invalid source path %q: %v", step.Name, step.SourcePath, err)
		}
		record := ingest.ClaudeTranscriptEvidence{
			SourcePath:      path,
			Scope:           ingest.ClaudeEvidenceScope(step.Scope),
			ModTimeUnixNano: 1,
			SizeBytes:       2,
			Origin:          sessionorigin.User,
			ProofVersion:    ingest.ClaudeIdentityProofVersion,
		}
		for _, spawn := range step.Spawns {
			record.Spawns = append(record.Spawns, ingest.ClaudeTeammateIdentity{Team: spawn.Team, Name: spawn.Name})
		}
		if err := db.SaveClaudeEvidence(t.Context(), []ingest.ClaudeTranscriptEvidence{record}, nil); err != nil {
			t.Fatalf("maintenance step %q: SaveClaudeEvidence: %v", step.Name, err)
		}
		requireSpawnRowsEqual(t, db, step.ExpectedSpawns)
	}

	// The scope flip: a path saved as a root and then re-saved as a subagent.
	// The fixture derives it so no test hardcodes the step name.
	flipIndex := -1
	rootPaths := make(map[string]struct{}, len(fixture.MaintenanceSteps))
	for index := range fixture.MaintenanceSteps {
		step := &fixture.MaintenanceSteps[index]
		if step.Scope == string(ingest.ClaudeEvidenceScopeRoot) {
			rootPaths[step.SourcePath] = struct{}{}
			continue
		}
		if _, wasRoot := rootPaths[step.SourcePath]; wasRoot {
			flipIndex = index
		}
	}
	if flipIndex < 0 {
		t.Fatal("v70 derived-lookups fixture re-saves no root path as a subagent; the scope flip is untested")
	}
	if len(calls) != len(fixture.MaintenanceSteps) {
		t.Fatalf("SaveClaudeEvidence made %d spawn maintenance calls for %d saves; the maintenance must run for every upserted path, including a non-root one", len(calls), len(fixture.MaintenanceSteps))
	}
	flip := &fixture.MaintenanceSteps[flipIndex]
	if calls[flipIndex].Path != flip.SourcePath {
		t.Fatalf("spawn maintenance call %d is for %q, want the scope-flip path %q", flipIndex, calls[flipIndex].Path, flip.SourcePath)
	}
	if len(calls[flipIndex].Spawns) != 0 {
		t.Fatalf("the scope flip for %q passed %d spawns to the projection, want none: a non-root record contributes no rows", flip.SourcePath, len(calls[flipIndex].Spawns))
	}
}

// TestMigrationV71FreshSchema proves a brand-new store carries the V70 objects
// with the ratified shapes and that the schema version advanced by one.
func TestMigrationV71FreshSchema(t *testing.T) {
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

	if got, want := CurrentSchemaVersion(), 78; got != want {
		t.Fatalf("CurrentSchemaVersion() = %d, want %d", got, want)
	}
	var version int
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA user_version`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { version = int(stmt.ColumnInt64(0)); return nil },
	}); err != nil {
		t.Fatalf("read the fresh store's user_version: %v", err)
	}
	if version != 78 {
		t.Fatalf("fresh user_version = %d, want 78", version)
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
