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
	"gopkg.in/yaml.v3"
)

//go:embed testdata/migrations/v71_harness_check_widening.yaml
var v71Fixture []byte

//go:embed testdata/migrations/v71_harness_check_widening.manifest.yaml
var v71Manifest []byte

func TestMigrationV71WidensHarnessChecks(t *testing.T) {
	var f struct {
		Scenarios []struct {
			Name string `yaml:"name"`
		} `yaml:"scenarios"`
		RequiredObjects []string `yaml:"requiredObjects"`
	}
	var m struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(v71Fixture, &f); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(v71Manifest, &m); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, v := range f.Scenarios {
		if v.Name == "" || names[v.Name] {
			t.Fatalf("invalid scenario name %q", v.Name)
		}
		names[v.Name] = true
	}
	for _, n := range m.RequiredNames {
		if !names[n] {
			t.Fatalf("missing scenario %q", n)
		}
	}
	path := filepath.Join(t.TempDir(), "upgrade.db")
	c, err := sqlite.OpenConn(path, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = migrateFrozenSchema(t, c, 70); err != nil {
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
	if err = sqlitemigration.Migrate(context.Background(), c, dbSchema); err != nil {
		t.Fatal(err)
	}
	if got := query(stateSQL); !reflect.DeepEqual(got, state) {
		t.Fatalf("rows or parent lane changed: before %v after %v", state, got)
	}
	fk := query(`SELECT 'foreign_keys',CAST(foreign_keys AS TEXT) FROM pragma_foreign_keys`)
	if fk["foreign_keys"] != "1" {
		t.Fatal("foreign keys not enabled")
	}
	old := "'claude-code','opencode','codex','gemini-cli','cursor','antigravity','strike','pi'"
	widened := old + ",'grok-build','devin-desktop','cline','kiro','kiro-cli','kiro-crew'"
	for _, n := range []string{"sessions", "daily_summary_harness"} {
		if !strings.Contains(before[n], old) {
			t.Fatalf("missing predecessor check %s", n)
		}
		before[n] = strings.Replace(before[n], old, widened, 1)
	}
	if got := query(objectsSQL); !reflect.DeepEqual(got, before) {
		t.Fatalf("schema objects changed beyond harness check")
	}
	for name := range before {
		if strings.HasPrefix(name, "session_relationship_evidence_parent_state_") || strings.HasPrefix(name, "session_publication_metadata_parent_state_") {
			if strings.Contains(migrationV71, "CREATE TRIGGER "+name) {
				t.Fatalf("off-table trigger %s recreated", name)
			}
		}
	}
	freshPath := filepath.Join(t.TempDir(), "fresh.db")
	fresh := openFreshStore(t, freshPath)
	_ = fresh
	freshConn, err := sqlite.OpenConn(freshPath, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer freshConn.Close()
	freshObjects := map[string]string{}
	if err := sqlitex.ExecuteTransient(freshConn, objectsSQL, &sqlitex.ExecOptions{ResultFunc: func(s *sqlite.Stmt) error { freshObjects[s.ColumnText(0)] = s.ColumnText(1); return nil }}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(freshObjects, query(objectsSQL)) {
		t.Fatal("fresh and upgraded schema objects differ")
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
				t.Errorf("%s rejected %s: %v", tbl, h, err)
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
