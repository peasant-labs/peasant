package store

import (
	"context"
	_ "embed"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	regexp "github.com/wasilibs/go-re2"
)

//go:embed testdata/stored_harness_check.yaml
var storedHarnessCheckYAML []byte

//go:embed testdata/stored_harness_check.manifest.yaml
var storedHarnessCheckManifestYAML []byte

// storedHarnessCheckCase is the literal model_harness CHECK list of one frozen
// schema version, in the order its DDL writes it.
type storedHarnessCheckCase struct {
	Name          string   `yaml:"name"`
	SchemaVersion int      `yaml:"schemaVersion"`
	Values        []string `yaml:"values"`
}

// ddlList renders the case's values as the DDL writes them inside IN (...).
func (c storedHarnessCheckCase) ddlList() string {
	return "'" + strings.Join(c.Values, "','") + "'"
}

// harnessCheckTables are the two tables that mirror the stored harness set in
// a CHECK.
var harnessCheckTables = []string{"sessions", "daily_summary_harness"}

// unknownHarnessValue is the value every stored-harness CHECK must refuse.
const unknownHarnessValue = "not-a-harness"

// loadStoredHarnessChecks decodes the frozen CHECK fixture and holds its case
// names equal to the manifest.
func loadStoredHarnessChecks(t *testing.T) map[string]storedHarnessCheckCase {
	t.Helper()
	var fixture struct {
		Cases []storedHarnessCheckCase `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := decodeMigrationSlotYAML(storedHarnessCheckYAML, &fixture, "testdata/stored_harness_check.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := decodeMigrationSlotYAML(storedHarnessCheckManifestYAML, &manifest, "testdata/stored_harness_check.manifest.yaml"); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	byName := make(map[string]storedHarnessCheckCase, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if c.SchemaVersion < 2 || c.SchemaVersion > CurrentSchemaVersion() {
			t.Fatalf("testdata/stored_harness_check.yaml: case %q names schema version %d, outside the shipped range 2..%d; name the frozen version whose migration set the list", c.Name, c.SchemaVersion, CurrentSchemaVersion())
		}
		if len(c.Values) == 0 {
			t.Fatalf("testdata/stored_harness_check.yaml: case %q holds no values; write the literal CHECK list of schema version %d", c.Name, c.SchemaVersion)
		}
		names = append(names, c.Name)
		byName[c.Name] = c
	}
	if err := validateMigrationNames(manifest.RequiredNames, names, "stored harness CHECK"); err != nil {
		t.Fatal(err)
	}
	return byName
}

// storedHarnessCheckCaseNamed returns one required case of the frozen fixture.
func storedHarnessCheckCaseNamed(t *testing.T, name string) storedHarnessCheckCase {
	t.Helper()
	c, ok := loadStoredHarnessChecks(t)[name]
	if !ok {
		t.Fatalf("testdata/stored_harness_check.yaml has no case %q; a migration test reads its frozen CHECK list from that fixture", name)
	}
	return c
}

var harnessCheckListPattern = regexp.MustCompile(`model_harness IN \(([^)]*)\)`)

// harnessCheckValues parses the model_harness IN (...) list from the table's
// sqlite_master DDL.
func harnessCheckValues(t *testing.T, conn *sqlite.Conn, table string) []string {
	t.Helper()
	var ddl string
	err := sqlitex.Execute(conn, `SELECT sql FROM sqlite_master WHERE type='table' AND name=?`, &sqlitex.ExecOptions{
		Args: []any{table},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			ddl = stmt.ColumnText(0)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("read the %s DDL: %v", table, err)
	}
	matches := harnessCheckListPattern.FindAllStringSubmatch(ddl, -1)
	if len(matches) != 1 {
		t.Fatalf("the %s DDL holds %d model_harness IN (...) lists, want exactly one:\n%s", table, len(matches), ddl)
	}
	var values []string
	for _, quoted := range strings.Split(matches[0][1], ",") {
		values = append(values, strings.Trim(strings.TrimSpace(quoted), "'"))
	}
	return values
}

// assertStoredHarnessCheck holds both tables' CHECK lists equal to the case's
// literal list, inserts every listed value into both tables, and requires the
// unknown value to be refused by both.
func assertStoredHarnessCheck(t *testing.T, conn *sqlite.Conn, c storedHarnessCheckCase, store string) {
	t.Helper()
	for _, table := range harnessCheckTables {
		if got := harnessCheckValues(t, conn, table); !reflect.DeepEqual(got, c.Values) {
			t.Errorf("%s store at schema %d: %s CHECK lists %v, want the frozen %s list %v", store, c.SchemaVersion, table, got, c.Name, c.Values)
		}
	}
	if err := sqlitex.ExecuteScript(conn, `
INSERT OR IGNORE INTO host_slugs(opaque_id,host_slug) VALUES('check-host','check-host');
INSERT OR IGNORE INTO projects(project_hash,canonical_cwd) VALUES('check-project','/synthetic/check');`, nil); err != nil {
		t.Fatalf("%s store at schema %d: seed the session parents: %v", store, c.SchemaVersion, err)
	}
	insertSession := `INSERT INTO sessions(session_id,model_harness,model_id,opaque_host_id,project_hash,start_ms,end_ms,ingested_ms,source_path,source_format) VALUES(?,?,'m','check-host','check-project',1,2,3,'/synthetic/check','jsonl')`
	insertSummary := `INSERT INTO daily_summary_harness(date_utc,model_harness) VALUES('2021-01-01',?)`
	for _, value := range c.Values {
		if err := sqlitex.Execute(conn, insertSession, &sqlitex.ExecOptions{Args: []any{"check-" + value, value}}); err != nil {
			t.Errorf("%s store at schema %d: sessions refused listed value %q: %v", store, c.SchemaVersion, value, err)
		}
		if err := sqlitex.Execute(conn, insertSummary, &sqlitex.ExecOptions{Args: []any{value}}); err != nil {
			t.Errorf("%s store at schema %d: daily_summary_harness refused listed value %q: %v", store, c.SchemaVersion, value, err)
		}
	}
	if err := sqlitex.Execute(conn, insertSession, &sqlitex.ExecOptions{Args: []any{"check-unknown", unknownHarnessValue}}); err == nil {
		t.Errorf("%s store at schema %d: sessions accepted %q", store, c.SchemaVersion, unknownHarnessValue)
	}
	if err := sqlitex.Execute(conn, insertSummary, &sqlitex.ExecOptions{Args: []any{unknownHarnessValue}}); err == nil {
		t.Errorf("%s store at schema %d: daily_summary_harness accepted %q", store, c.SchemaVersion, unknownHarnessValue)
	}
}

// openFrozenConn opens a new database file and applies the first n shipped
// migrations to it.
func openFrozenConn(t *testing.T, name string, n int) *sqlite.Conn {
	t.Helper()
	conn, err := sqlite.OpenConn(filepath.Join(t.TempDir(), name), sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close %s: %v", name, err)
		}
	})
	if err := migrateFrozenSchema(t, conn, n); err != nil {
		t.Fatalf("migrate %s to schema %d: %v", name, n, err)
	}
	return conn
}

// TestStoredHarnessCheckPerFrozenVersion holds the stored-harness CHECK of
// every frozen version that changed it equal to its literal list, on both
// tables, in a store built at that version and in a store migrated to it from
// its predecessor. At the head it also checks the store the production Open
// creates from the baseline.
func TestStoredHarnessCheckPerFrozenVersion(t *testing.T) {
	t.Parallel()
	for name, c := range loadStoredHarnessChecks(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fresh := openFrozenConn(t, "fresh.db", c.SchemaVersion)
			assertStoredHarnessCheck(t, fresh, c, "fresh")

			migrated := openFrozenConn(t, "migrated.db", c.SchemaVersion-1)
			if err := sqlitemigration.Migrate(context.Background(), migrated, frozenSchema(c.SchemaVersion)); err != nil {
				t.Fatalf("migrate from schema %d to %d: %v", c.SchemaVersion-1, c.SchemaVersion, err)
			}
			assertStoredHarnessCheck(t, migrated, c, "migrated")

			if c.SchemaVersion != CurrentSchemaVersion() {
				return
			}
			s := openFreshStore(t, filepath.Join(t.TempDir(), "opened.db"))
			conn, err := s.Pool().Take(t.Context())
			if err != nil {
				t.Fatalf("take a connection of the opened store: %v", err)
			}
			defer s.Pool().Put(conn)
			assertStoredHarnessCheck(t, conn, c, "opened")
		})
	}
}

// harnessCheckWidening describes one migration that only widens the stored
// harness CHECK on sessions and daily_summary_harness.
type harnessCheckWidening struct {
	// slot is the migration's schema version; the store starts at slot-1.
	slot int
	// from is the frozen CHECK case in force at slot-1: the case of the last
	// migration before slot that set the list.
	from storedHarnessCheckCase
	// to is the frozen CHECK case slot sets.
	to storedHarnessCheckCase
	// migration is the migration's SQL, read to prove the off-table
	// parent-lane triggers are not recreated.
	migration string
	// requiredObjects are the schema objects the predecessor must hold.
	requiredObjects []string
}

// runHarnessCheckWidening migrates a store seeded at schema w.slot-1 with
// rows for every stored harness to schema w.slot, and requires the migration
// to change nothing but the CHECK list: every row and column, the parent lane,
// store_state.revision, foreign keys, every sessions index and trigger, and
// the DDL of a store built fresh at the new version.
func runHarnessCheckWidening(t *testing.T, w harnessCheckWidening) {
	t.Helper()
	if w.to.SchemaVersion != w.slot || w.from.SchemaVersion >= w.slot {
		t.Fatalf("widening at slot %d names %s (schema %d) before and %s (schema %d) after; the after case must be the slot's own and the before case an earlier one", w.slot, w.from.Name, w.from.SchemaVersion, w.to.Name, w.to.SchemaVersion)
	}
	predecessor := w.slot - 1
	c := openFrozenConn(t, "upgrade.db", predecessor)
	run := func(sql string, args ...any) {
		t.Helper()
		if err := sqlitex.Execute(c, sql, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatalf("execute %s: %v", sql, err)
		}
	}
	run(`PRAGMA foreign_keys=ON`)
	run(`INSERT INTO host_slugs(opaque_id,host_slug) VALUES('h','h')`)
	run(`INSERT INTO projects(project_hash,canonical_cwd) VALUES('p','/synthetic')`)
	insertSession := `INSERT INTO sessions(session_id,parent_id,model_harness,model_id,opaque_host_id,project_hash,start_ms,end_ms,ingested_ms,source_path,source_format) VALUES(?,?,?,'m','h','p',1,2,3,?,'jsonl')`
	parent := ""
	for _, value := range w.from.Values {
		id := "seed-" + value
		if parent == "" {
			run(insertSession, id, nil, value, "/"+id)
			parent = id
		} else {
			run(insertSession, id, parent, value, "/"+id)
		}
		run(`INSERT INTO daily_summary_harness(date_utc,model_harness,session_count) VALUES('2020-01-01',?,1)`, value)
	}
	query := func(conn *sqlite.Conn, sql string) map[string]string {
		t.Helper()
		out := map[string]string{}
		err := sqlitex.ExecuteTransient(conn, sql, &sqlitex.ExecOptions{ResultFunc: func(s *sqlite.Stmt) error {
			out[s.ColumnText(0)] = s.ColumnText(1)
			return nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	objectsSQL := `SELECT name,sql FROM sqlite_master WHERE (name IN ('sessions','daily_summary_harness') OR (tbl_name='sessions' AND type IN ('index','trigger') AND sql IS NOT NULL) OR (type='trigger' AND name LIKE '%parent_state%')) ORDER BY name`
	before := query(c, objectsSQL)
	for _, name := range w.requiredObjects {
		if _, ok := before[name]; !ok {
			t.Fatalf("missing predecessor schema object %q at schema %d", name, predecessor)
		}
	}
	if got, want := slices.Sorted(maps.Keys(before)), slices.Sorted(slices.Values(w.requiredObjects)); !reflect.DeepEqual(got, want) {
		t.Fatalf("predecessor schema objects at schema %d are %v, want exactly the required objects %v", predecessor, got, want)
	}
	stateSQL := `SELECT 'revision',CAST(revision AS TEXT) FROM store_state UNION ALL SELECT session_id,coalesce(parent_id,'') || ':' || parent_state || ':' || coalesce(resolved_parent_id,'') || ':' || model_harness FROM sessions UNION ALL SELECT 'summary:' || model_harness,CAST(session_count AS TEXT) FROM daily_summary_harness`
	state := query(c, stateSQL)
	// Encode every column by name and SQLite literal representation, including
	// NULLs and blobs, so a changed value or a missing row cannot hide in a
	// sample.
	dumpRows := func(table, key string) map[string]string {
		t.Helper()
		var columns []string
		err := sqlitex.ExecuteTransient(c, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, &sqlitex.ExecOptions{Args: []any{table}, ResultFunc: func(stmt *sqlite.Stmt) error {
			name := stmt.ColumnText(0)
			columns = append(columns, `hex(quote("`+strings.ReplaceAll(name, `"`, `""`)+`"))`)
			return nil
		}})
		if err != nil || len(columns) == 0 {
			t.Fatalf("read columns for %s: %v", table, err)
		}
		return query(c, `SELECT `+key+`, `+strings.Join(columns, ` || ':' || `)+` FROM `+table+` ORDER BY `+key)
	}
	sessionsBefore := dumpRows("sessions", "session_id")
	summaryBefore := dumpRows("daily_summary_harness", "date_utc || ':' || model_harness")
	if len(sessionsBefore) != len(w.from.Values) || len(summaryBefore) != len(w.from.Values) {
		t.Fatalf("seeded %d sessions and %d summaries, want one of each per stored harness value of %s", len(sessionsBefore), len(summaryBefore), w.from.Name)
	}

	if err := sqlitemigration.Migrate(context.Background(), c, frozenSchema(w.slot)); err != nil {
		t.Fatal(err)
	}

	if got := query(c, stateSQL); !reflect.DeepEqual(got, state) {
		t.Fatalf("rows, parent lane or store_state.revision changed: before %v after %v", state, got)
	}
	if got := dumpRows("sessions", "session_id"); !reflect.DeepEqual(got, sessionsBefore) {
		t.Fatalf("session rows changed: before %v after %v", sessionsBefore, got)
	}
	if got := dumpRows("daily_summary_harness", "date_utc || ':' || model_harness"); !reflect.DeepEqual(got, summaryBefore) {
		t.Fatalf("summary rows changed: before %v after %v", summaryBefore, got)
	}
	if err := sqlitex.ExecuteTransient(c, `PRAGMA foreign_key_check`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		t.Errorf("foreign key violation in %s row %d", stmt.ColumnText(0), stmt.ColumnInt64(1))
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if fk := query(c, `SELECT 'foreign_keys',CAST(foreign_keys AS TEXT) FROM pragma_foreign_keys`); fk["foreign_keys"] != "1" {
		t.Fatal("foreign keys not enabled after the migration")
	}

	after := query(c, objectsSQL)
	oldList, newList := w.from.ddlList(), w.to.ddlList()
	for _, table := range harnessCheckTables {
		if strings.Count(before[table], oldList) != 1 {
			t.Fatalf("%s DDL at schema %d does not hold the frozen %s list exactly once", table, predecessor, w.from.Name)
		}
		if strings.Count(after[table], newList) != 1 {
			t.Fatalf("%s DDL at schema %d does not hold the frozen %s list exactly once", table, w.slot, w.to.Name)
		}
		before[table] = strings.Replace(before[table], oldList, "<harness-set>", 1)
		after[table] = strings.Replace(after[table], newList, "<harness-set>", 1)
	}
	if !reflect.DeepEqual(after, before) {
		for name := range before {
			if before[name] != after[name] {
				t.Errorf("schema object %s changed beyond the harness CHECK:\nbefore: %s\nafter:  %s", name, before[name], after[name])
			}
		}
		t.Fatalf("schema objects changed beyond the harness CHECK")
	}
	for name := range before {
		offTable := strings.HasPrefix(name, "session_relationship_evidence_parent_state_") || strings.HasPrefix(name, "session_publication_metadata_parent_state_")
		if offTable && strings.Contains(w.migration, "CREATE TRIGGER "+name) {
			t.Fatalf("off-table trigger %s recreated", name)
		}
	}

	fresh := openFrozenConn(t, "fresh.db", w.slot)
	if got, want := query(fresh, objectsSQL), query(c, objectsSQL); !reflect.DeepEqual(got, want) {
		for name := range want {
			if got[name] != want[name] {
				t.Errorf("schema object %s differs:\nfresh:    %s\nmigrated: %s", name, got[name], want[name])
			}
		}
		t.Fatalf("fresh and migrated schema %d objects differ", w.slot)
	}
	assertStoredHarnessCheck(t, c, w.to, "migrated")
}

// loadWideningObjects decodes a migration's required predecessor objects and
// holds them equal to its manifest.
func loadWideningObjects(t *testing.T, fixture, manifest []byte, name string) []string {
	t.Helper()
	var f struct {
		RequiredObjects []string `yaml:"requiredObjects"`
	}
	var m struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := decodeMigrationSlotYAML(fixture, &f, name+".yaml"); err != nil {
		t.Fatal(err)
	}
	if err := decodeMigrationSlotYAML(manifest, &m, name+".manifest.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := validateMigrationNames(m.RequiredNames, f.RequiredObjects, fmt.Sprintf("%s schema objects", name)); err != nil {
		t.Fatal(err)
	}
	return f.RequiredObjects
}
