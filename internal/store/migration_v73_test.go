package store

import (
	"context"
	_ "embed"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/migrations/v73_harness_check_widening.yaml
var v73Fixture []byte

//go:embed testdata/migrations/v73_harness_check_widening.manifest.yaml
var v73Manifest []byte

func TestMigrationV73WidensHarnessChecks(t *testing.T) {
	var f struct {
		RequiredObjects []string `yaml:"requiredObjects"`
	}
	var m struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := decodeMigrationSlotYAML(v73Fixture, &f, "v73_harness_check_widening.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := decodeMigrationSlotYAML(v73Manifest, &m, "v73_harness_check_widening.manifest.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := validateMigrationNames(m.RequiredNames, f.RequiredObjects, "V71 schema objects"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "upgrade.db")
	c, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = migrateFrozenSchema(t, c, 72); err != nil {
		t.Fatal(err)
	}
	run := func(sql string) {
		t.Helper()
		if err := sqlitex.ExecuteScript(c, sql, nil); err != nil {
			t.Fatalf("execute SQL: %v", err)
		}
	}
	if err := sqlitex.ExecuteTransient(c, `PRAGMA foreign_keys=ON`, nil); err != nil {
		t.Fatal(err)
	}
	run(`
 INSERT INTO host_slugs(opaque_id,host_slug) VALUES('h','h');
 INSERT INTO projects(project_hash,canonical_cwd) VALUES('p','/synthetic');
 INSERT INTO sessions(session_id,model_harness,model_id,opaque_host_id,project_hash,start_ms,end_ms,ingested_ms,source_path,source_format) VALUES('parent','gemini-cli','m','h','p',1,2,3,'/parent','jsonl');
 INSERT INTO sessions(session_id,parent_id,model_harness,model_id,opaque_host_id,project_hash,start_ms,end_ms,ingested_ms,source_path,source_format) VALUES('child','parent','gemini-cli','m','h','p',1,2,3,'/child','jsonl');
 INSERT INTO daily_summary_harness(date_utc,model_harness,session_count) VALUES('2020-01-01','gemini-cli',1);`)
	query := func(sql string) map[string]string {
		t.Helper()
		out := map[string]string{}
		err := sqlitex.ExecuteTransient(c, sql, &sqlitex.ExecOptions{ResultFunc: func(s *sqlite.Stmt) error { out[s.ColumnText(0)] = s.ColumnText(1); return nil }})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	objectsSQL := `SELECT name,sql FROM sqlite_master WHERE (name IN ('sessions','daily_summary_harness') OR (tbl_name='sessions' AND type IN ('index','trigger') AND sql IS NOT NULL) OR (type='trigger' AND name LIKE '%parent_state%')) ORDER BY name`
	before := query(objectsSQL)
	for _, name := range f.RequiredObjects {
		if _, ok := before[name]; !ok {
			t.Fatalf("missing predecessor schema object %q", name)
		}
	}
	stateSQL := `SELECT 'revision',CAST(revision AS TEXT) FROM store_state UNION ALL SELECT session_id,coalesce(parent_id,'') || ':' || parent_state || ':' || coalesce(resolved_parent_id,'') || ':' || model_harness FROM sessions UNION ALL SELECT 'summary',model_harness || ':' || session_count FROM daily_summary_harness`
	state := query(stateSQL)
	// Encode every column by name and SQLite literal representation, including NULLs
	// and blobs, so a changed value or a missing row cannot hide in a sample.
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
		return query(`SELECT ` + key + `, ` + strings.Join(columns, ` || ':' || `) + ` FROM ` + table + ` ORDER BY ` + key)
	}
	sessionsBefore := dumpRows("sessions", "session_id")
	summaryBefore := dumpRows("daily_summary_harness", "date_utc || ':' || model_harness")
	if err = sqlitemigration.Migrate(context.Background(), c, frozenSchema(73)); err != nil {
		t.Fatal(err)
	}
	if got := query(stateSQL); !reflect.DeepEqual(got, state) {
		t.Fatalf("rows or parent lane changed: before %v after %v", state, got)
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
	fk := query(`SELECT 'foreign_keys',CAST(foreign_keys AS TEXT) FROM pragma_foreign_keys`)
	if fk["foreign_keys"] != "1" {
		t.Fatal("foreign keys not enabled")
	}
	old := "'claude-code','opencode','codex','gemini-cli','cursor','antigravity','strike','pi'"
	widened := old + ",'grok-build','devin-desktop','cline','kiro','kiro-cli','kiro-crew'"
	after := query(objectsSQL)
	for _, n := range []string{"sessions", "daily_summary_harness"} {
		if strings.Count(before[n], old) != 1 || strings.Count(after[n], widened) != 1 {
			t.Fatalf("%s harness CHECK does not match the frozen predecessor or V71 set", n)
		}
		before[n] = strings.Replace(before[n], old, "<harness-set>", 1)
		after[n] = strings.Replace(after[n], widened, "<harness-set>", 1)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("schema objects changed beyond harness check")
	}
	for name := range before {
		if strings.HasPrefix(name, "session_relationship_evidence_parent_state_") || strings.HasPrefix(name, "session_publication_metadata_parent_state_") {
			if strings.Contains(migrationV73, "CREATE TRIGGER "+name) {
				t.Fatalf("off-table trigger %s recreated", name)
			}
		}
	}
	freshPath := filepath.Join(t.TempDir(), "fresh.db")
	freshConn, err := sqlite.OpenConn(freshPath, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer freshConn.Close()
	if err := sqlitemigration.Migrate(context.Background(), freshConn, frozenSchema(73)); err != nil {
		t.Fatal(err)
	}
	freshObjects := map[string]string{}
	if err := sqlitex.ExecuteTransient(freshConn, objectsSQL, &sqlitex.ExecOptions{ResultFunc: func(s *sqlite.Stmt) error { freshObjects[s.ColumnText(0)] = s.ColumnText(1); return nil }}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(freshObjects, query(objectsSQL)) {
		t.Fatal("fresh and upgraded V71 schema objects differ")
	}
	for _, h := range schema.Harnesses() {
		for _, tbl := range []string{"sessions", "daily_summary_harness"} {
			var sql string
			var args []any
			if tbl == "sessions" {
				sql = `INSERT INTO sessions(session_id,model_harness,model_id,opaque_host_id,project_hash,start_ms,end_ms,ingested_ms,source_path,source_format) VALUES(?,?,'m','h','p',1,2,3,'/x','jsonl')`
				args = []any{"check-" + string(h), string(h)}
			} else {
				sql = `INSERT INTO daily_summary_harness(date_utc,model_harness) VALUES('2021-01-01',?)`
				args = []any{string(h)}
			}
			if err := sqlitex.ExecuteTransient(c, sql, &sqlitex.ExecOptions{Args: args}); err != nil {
				t.Errorf("%s rejected %s: %v; add a new widening migration; never edit V71", tbl, h, err)
			}
		}
	}
	for _, tbl := range []string{"sessions", "daily_summary_harness"} {
		var sql string
		if tbl == "sessions" {
			sql = `INSERT INTO sessions(session_id,model_harness,model_id,opaque_host_id,project_hash,start_ms,end_ms,ingested_ms,source_path,source_format) VALUES('bad','not-a-harness','m','h','p',1,2,3,'/x','jsonl')`
		} else {
			sql = `INSERT INTO daily_summary_harness(date_utc,model_harness) VALUES('2021-01-01','not-a-harness')`
		}
		if err := sqlitex.ExecuteTransient(c, sql, nil); err == nil {
			t.Errorf("%s accepted unknown harness", tbl)
		}
	}
}
