package store_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/migration_v82.yaml
var migrationV82YAML []byte

// The drivers of the migration 82 cases.
const (
	v82KindSeededPredecessor = "seeded-predecessor"
	v82KindCorpusPredecessor = "corpus-predecessor"
	v82KindUpdateRefused     = "update-refused"
	v82KindInsert            = "insert"
	v82KindEmptyPredecessor  = "empty-predecessor"
	v82KindPlan              = "plan"
	v82KindFreshVsMigrated   = "fresh-vs-migrated"
	v82KindOlderBuild        = "older-build"
)

// The generation roles a seeded predecessor holds.
const (
	v82RoleActive        = "active"
	v82RoleLatestAttempt = "latest-attempt"
	v82RoleSuperseded    = "superseded"
)

// The mapping_set_id an insert case writes.
const (
	v82MappingSelf  = "self"
	v82MappingEmpty = "empty"
	v82MappingOwner = "owner"
)

const v82Session = "abc00000-0000-4000-8000-0000000000e2"

type migrationV82Case struct {
	Name        string `yaml:"name"`
	Kind        string `yaml:"kind"`
	Given       string `yaml:"given"`
	When        string `yaml:"when"`
	Then        string `yaml:"then"`
	Generations []struct {
		ID   string `yaml:"id"`
		Role string `yaml:"role"`
	} `yaml:"generations"`
	MappingSetID string `yaml:"mappingSetID"`
	WantError    string `yaml:"wantError"`
}

func loadMigrationV82Cases(t *testing.T) []migrationV82Case {
	t.Helper()
	var fixture struct {
		RequiredNames []string           `yaml:"requiredNames"`
		Cases         []migrationV82Case `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(migrationV82YAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode the migration 82 fixture: %v", err)
	}
	var names []string
	for _, c := range fixture.Cases {
		named := c.Name != "" && !slices.Contains(names, c.Name)
		stated := c.Given != "" && c.When != "" && c.Then != ""
		if !named || !stated {
			t.Fatalf("migration 82 case %q is unnamed, duplicated, or misses its given, when or then", c.Name)
		}
		names = append(names, c.Name)
	}
	if got, want := slices.Sorted(slices.Values(names)), slices.Sorted(slices.Values(fixture.RequiredNames)); !slices.Equal(got, want) {
		t.Fatalf("migration 82 cases %v differ from the required names %v", got, want)
	}
	return fixture.Cases
}

func TestMigrationV82AddsTheMappingSetColumn(t *testing.T) {
	if store.MappingSetSchemaVersionForTest != 82 || store.CurrentSchemaVersion() != 83 {
		t.Fatalf("the mapping-set migration is %d and the head is %d; this test pins them at 82 and 83", store.MappingSetSchemaVersionForTest, store.CurrentSchemaVersion())
	}
	for _, c := range loadMigrationV82Cases(t) {
		t.Run(c.Name, func(t *testing.T) {
			switch c.Kind {
			case v82KindSeededPredecessor:
				runV82SeededPredecessor(t, c)
			case v82KindCorpusPredecessor:
				runV82CorpusPredecessor(t)
			case v82KindUpdateRefused, v82KindInsert:
				runV82Constraint(t, c)
			case v82KindEmptyPredecessor:
				path := filepath.Join(t.TempDir(), "empty.db")
				store.BuildBehindStoreForTest(t, path, 81, nil)
				upgradeV82(t, path)
			case v82KindPlan:
				path := filepath.Join(t.TempDir(), "plan.db")
				store.BuildBehindStoreForTest(t, path, 81, nil)
				plan, err := store.PlanUpgrade(path)
				if err != nil {
					t.Fatal(err)
				}
				if plan.From != 81 || plan.To != 83 || len(plan.Steps) != 2 {
					t.Fatalf("plan = %+v, want the mapping-set step then the owner step from 81 to 83", plan)
				}
				step := plan.Steps[0]
				quick := step.Cost == store.UpgradeCostQuick && !plan.HasLongStep()
				schemaStep := step.Version == 82 && step.Kind == store.UpgradeStepSchema && step.StampEffect == store.StampEffectNone
				if !quick || !schemaStep {
					t.Fatalf("step = %+v, want the quick schema step 82 with no session effect", step)
				}
			case v82KindFreshVsMigrated:
				runV82FreshVsMigrated(t)
			case v82KindOlderBuild:
				runV82OlderBuild(t)
			default:
				t.Fatalf("migration 82 case %s has unknown kind %q", c.Name, c.Kind)
			}
		})
	}
}

// upgradeV82 upgrades a schema-81 store through mapping-set step 82 to the head.
func upgradeV82(t *testing.T, path string) {
	t.Helper()
	report, err := store.Upgrade(t.Context(), path, store.UpgradeOptions{})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if report.From != 81 || report.To != store.CurrentSchemaVersion() {
		t.Fatalf("upgrade report = %d -> %d, want 81 -> %d", report.From, report.To, store.CurrentSchemaVersion())
	}
	if version, err := store.SchemaVersionAt(path); err != nil || version != store.CurrentSchemaVersion() {
		t.Fatalf("store is at schema %d (%v), want %d", version, err, store.CurrentSchemaVersion())
	}
}

func v82Exec(t *testing.T, conn *sqlite.Conn, query string, args ...any) {
	t.Helper()
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// v82SeedSession writes one session row and the given generation rows by SQL,
// with every NOT NULL column set to a valid synthetic value.
func v82SeedSession(t *testing.T, conn *sqlite.Conn, generations ...string) {
	t.Helper()
	v82Exec(t, conn, `INSERT INTO host_slugs(opaque_id, host_slug) VALUES('v82-host','v82-host')`)
	v82Exec(t, conn, `INSERT INTO projects(project_hash) VALUES('v82-project')`)
	v82Exec(t, conn, `INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES(?,'claude-code','fixture-model','v82-host','v82-project',1,2,3,'/synthetic/v82.jsonl','jsonl')`, v82Session)
	for i, id := range generations {
		v82InsertGeneration(t, conn, id, nil, int64(i))
	}
}

// v82InsertGeneration inserts one generation row; mappingSetID is written
// only when the column exists (non-nil).
func v82InsertGeneration(t *testing.T, conn *sqlite.Conn, id string, mappingSetID *string, installed int64) error {
	t.Helper()
	digest := strings.Repeat("a", 64)
	columns := `session_id, generation_id, schema_version, harness, model, version, ts_start, ts_end, source_format, project_hash, project_name, host_slug, content_hash, metadata_hash, redaction_applied, completeness, source_evidence_digest, index_format_version, candidate_digest, installed_at_ms`
	values := `?, ?, 12, 'claude-code', 'fixture-model', '1', 1, 2, 'jsonl', 'v82-project', 'v82', 'v82-host', ?, ?, 0, 'complete', ?, 2, ?, ?`
	args := []any{v82Session, id, digest, digest, digest, digest, installed}
	if mappingSetID != nil {
		columns += `, mapping_set_id`
		values += `, ?`
		args = append(args, *mappingSetID)
	}
	return sqlitex.ExecuteTransient(conn, `INSERT INTO session_generations(`+columns+`) VALUES(`+values+`)`, &sqlitex.ExecOptions{Args: args})
}

// v82GenerationRows renders every generation row as column=value cells, in
// generation order, leaving out the named columns.
func v82GenerationRows(t *testing.T, path string, without ...string) []string {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var rows []string
	if err := sqlitex.ExecuteTransient(conn, `SELECT * FROM session_generations ORDER BY session_id, generation_id`, &sqlitex.ExecOptions{ResultFunc: func(st *sqlite.Stmt) error {
		var cells []string
		for i := range st.ColumnCount() {
			if slices.Contains(without, st.ColumnName(i)) {
				continue
			}
			cells = append(cells, fmt.Sprintf("%s=%d:%s", st.ColumnName(i), st.ColumnType(i), st.ColumnText(i)))
		}
		rows = append(rows, strings.Join(cells, " "))
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	return rows
}

func runV82SeededPredecessor(t *testing.T, c migrationV82Case) {
	path := filepath.Join(t.TempDir(), "seeded.db")
	store.BuildBehindStoreForTest(t, path, 81, func(conn *sqlite.Conn) {
		var ids []string
		for _, g := range c.Generations {
			ids = append(ids, g.ID)
		}
		v82SeedSession(t, conn, ids...)
		for _, g := range c.Generations {
			switch g.Role {
			case v82RoleActive:
				v82Exec(t, conn, `UPDATE sessions SET active_generation_id = ? WHERE session_id = ?`, g.ID, v82Session)
			case v82RoleLatestAttempt:
				v82Exec(t, conn, `UPDATE sessions SET latest_attempt_generation_id = ?, latest_attempt_artifact_hash = ? WHERE session_id = ?`, g.ID, strings.Repeat("b", 64), v82Session)
			case v82RoleSuperseded:
			default:
				t.Fatalf("unknown generation role %q", g.Role)
			}
		}
	})
	before := v82GenerationRows(t, path)
	if len(before) != len(c.Generations) {
		t.Fatalf("seeded %d generation rows, want %d", len(before), len(c.Generations))
	}
	upgradeV82(t, path)
	if after := v82GenerationRows(t, path, "mapping_set_id"); !slices.Equal(after, before) {
		t.Fatalf("the upgrade changed generation rows:\nbefore %v\nafter  %v", before, after)
	}
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var total, null int
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*), COUNT(*) FILTER (WHERE mapping_set_id IS NULL) FROM session_generations`, &sqlitex.ExecOptions{ResultFunc: func(st *sqlite.Stmt) error {
		total, null = int(st.ColumnInt64(0)), int(st.ColumnInt64(1))
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if total != len(c.Generations) || null != total {
		t.Fatalf("%d of %d generation rows read NULL after the upgrade, want all %d", null, total, len(c.Generations))
	}
}

// runV82CorpusPredecessor writes each harness's corpus session with the
// production writers, puts session_generations back to its schema-81
// definition, upgrades, and compares every harvest-path reader with its
// committed golden.
func runV82CorpusPredecessor(t *testing.T) {
	corpus, manifest := loadReaderEntrypoints(t)
	predecessor := filepath.Join(t.TempDir(), "predecessor.db")
	store.BuildBehindStoreForTest(t, predecessor, 81, nil)
	v81Table := v82TableSQL(t, predecessor)
	for _, h := range corpus.Harnesses {
		t.Run(h.Harness, func(t *testing.T) {
			seeded := seedReaderEntrypointStore(t, corpus, h)
			if err := seeded.db.Close(); err != nil {
				t.Fatal(err)
			}
			conn, err := sqlite.OpenConn(seeded.path(), sqlite.OpenReadWrite)
			if err != nil {
				t.Fatal(err)
			}
			v82Exec(t, conn, `ALTER TABLE session_generations DROP COLUMN mapping_set_id`)
			// The corpus store is built at the current head, which also carries
			// the later local-owner migration. Remove those objects so the
			// predecessor really is a schema-81 store.
			v82Exec(t, conn, `DROP TRIGGER IF EXISTS sessions_parent_state_au`)
			v82Exec(t, conn, `DROP INDEX IF EXISTS idx_sessions_owner_claim`)
			v82Exec(t, conn, `DROP INDEX IF EXISTS idx_annotations_owner_claim`)
			v82Exec(t, conn, `DROP TABLE IF EXISTS owner_claims`)
			v82Exec(t, conn, `DROP TABLE IF EXISTS local_owner`)
			v82Exec(t, conn, `ALTER TABLE sessions DROP COLUMN owner_claim_id`)
			v82Exec(t, conn, `ALTER TABLE annotations DROP COLUMN owner_claim_id`)
			v82Exec(t, conn, `PRAGMA user_version = 81`)
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			if got := v82TableSQL(t, seeded.path()); got != v81Table {
				t.Fatalf("the predecessor table differs from the schema-81 definition:\n got %s\nwant %s", got, v81Table)
			}
			upgradeV82(t, seeded.path())
			seeded.db = openReaderEntrypointStore(t, seeded.root)
			observed, _ := readReaderEntrypoints(t, corpus, seeded)
			for _, entry := range manifest.Entries {
				name := h.Harness + "/" + entry
				if !slices.Contains(manifest.RequiredNames, name) {
					continue
				}
				want, err := os.ReadFile(filepath.Join(readerEntrypointGoldensDir, name+".golden"))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(observed[entry], want) {
					t.Errorf("%s changed across the upgrade:\nwant %s\n got %s", name, want, observed[entry])
				}
			}
		})
	}
}

// v82TableSQL returns the stored definition of session_generations.
func v82TableSQL(t *testing.T, path string) string {
	t.Helper()
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var definition string
	if err := sqlitex.ExecuteTransient(conn, `SELECT sql FROM sqlite_master WHERE type='table' AND name='session_generations'`, &sqlitex.ExecOptions{ResultFunc: func(st *sqlite.Stmt) error {
		definition = st.ColumnText(0)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	return definition
}

func runV82Constraint(t *testing.T, c migrationV82Case) {
	path := filepath.Join(t.TempDir(), "constraint.db")
	store.BuildBehindStoreForTest(t, path, 81, func(conn *sqlite.Conn) {
		v82SeedSession(t, conn, "g_owner")
	})
	upgradeV82(t, path)
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var got error
	if c.Kind == v82KindUpdateRefused {
		got = sqlitex.ExecuteTransient(conn, `UPDATE session_generations SET mapping_set_id = 'g_other' WHERE generation_id = 'g_owner'`, nil)
	} else {
		id := "g_reference"
		value := map[string]string{v82MappingSelf: id, v82MappingEmpty: "", v82MappingOwner: "g_owner"}
		mapping, ok := value[c.MappingSetID]
		if !ok {
			t.Fatalf("unknown mappingSetID %q", c.MappingSetID)
		}
		got = v82InsertGeneration(t, conn, id, &mapping, 9)
	}
	if c.WantError == "" {
		if got != nil {
			t.Fatalf("%s: %v", c.Then, got)
		}
		return
	}
	if got == nil || !strings.Contains(got.Error(), c.WantError) {
		t.Fatalf("got %v, want an error containing %q", got, c.WantError)
	}
}

func runV82FreshVsMigrated(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "fresh.db")
	db, err := store.Open(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	migrated := filepath.Join(dir, "migrated.db")
	store.BuildChainReferenceForTest(t, migrated)
	if !bytes.Equal(store.DumpDatabaseForTest(t, fresh), store.DumpDatabaseForTest(t, migrated)) {
		t.Fatal("the fresh and migrated stores dump differently; TestBaselineSchemaMatchesMigratedChain shows the line-level diff")
	}
	for _, path := range []string{fresh, migrated} {
		if !strings.Contains(v82TableSQL(t, path), "mapping_set_id TEXT CHECK") {
			t.Fatalf("%s carries no mapping_set_id column: %s", path, v82TableSQL(t, path))
		}
	}
}

func runV82OlderBuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "newer.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	newer := store.CurrentSchemaVersion() + 1
	conn, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	v82Exec(t, conn, `PRAGMA journal_mode = DELETE`)
	v82Exec(t, conn, fmt.Sprintf(`PRAGMA user_version = %d`, newer))
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := store.Open(path); err == nil {
		_ = opened.Close()
		t.Fatal("an open of a newer store succeeded")
	} else if want := store.NewerSchemaErrorForTest(path, newer); err.Error() != want.Error() {
		t.Fatalf("open refused with %v, want %v", err, want)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the refused open changed the store file")
	}
	if _, err := os.Stat(path + "-wal"); err == nil {
		t.Fatal("the refused open created a write-ahead log beside the store")
	}
}
