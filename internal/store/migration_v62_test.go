package store

// V62 fixture-driven upgrade, round-trip, and rejection tests. The file is
// package store so it can migrate a database to exactly V61 through the real
// migration list and then apply V62 on top; the strict fixture decode is done
// here directly because testutil imports store.

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/migrations/v62_thinking_level.yaml
var v62ThinkingLevelFixtureBytes []byte

type v62ThinkingLevelFixture struct {
	RequiredRecordNames    []string          `yaml:"required_record_names"`
	RequiredRejectionNames []string          `yaml:"required_rejection_names"`
	Records                []v62Record       `yaml:"records"`
	Rejections             []v62NamedRefusal `yaml:"rejections"`
}

type v62Record struct {
	Name             string  `yaml:"name"`
	SessionID        string  `yaml:"session_id"`
	ThinkingLevel    *string `yaml:"thinking_level"`
	ThinkingLevelRaw *string `yaml:"thinking_level_raw"`
}

type v62NamedRefusal struct {
	Name string `yaml:"name"`
	SQL  string `yaml:"sql"`
}

func requireV62Names(kind string, required []string, present map[string]bool) error {
	for _, name := range required {
		if !present[name] {
			return fmt.Errorf("v62 thinking-level fixture: required %s %q is missing; restore the case in testdata/migrations/v62_thinking_level.yaml", kind, name)
		}
	}
	return nil
}

func loadV62Fixture(t *testing.T) v62ThinkingLevelFixture {
	t.Helper()
	var fixture v62ThinkingLevelFixture
	d := yaml.NewDecoder(bytes.NewReader(v62ThinkingLevelFixtureBytes))
	d.KnownFields(true)
	if err := d.Decode(&fixture); err != nil {
		t.Fatalf("decode v62 thinking-level fixture: %v", err)
	}
	var trailing any
	if err := d.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("v62 thinking-level fixture must contain exactly one YAML document: %v", err)
	}
	records := map[string]bool{}
	for _, r := range fixture.Records {
		if records[r.Name] {
			t.Fatalf("v62 thinking-level fixture: duplicate record name %q", r.Name)
		}
		records[r.Name] = true
	}
	rejections := map[string]bool{}
	for _, r := range fixture.Rejections {
		if rejections[r.Name] {
			t.Fatalf("v62 thinking-level fixture: duplicate rejection name %q", r.Name)
		}
		rejections[r.Name] = true
	}
	if err := requireV62Names("record", fixture.RequiredRecordNames, records); err != nil {
		t.Fatal(err)
	}
	if err := requireV62Names("rejection", fixture.RequiredRejectionNames, rejections); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func nullableArg(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// TestMigrationV62UpgradeFromV61 migrates a populated V61 database to V62 and
// proves the pre-existing row reads back with both new columns NULL (absent,
// never 'off'), then round-trips every fixture record through the upgraded
// table and confirms the CHECK refuses every fixture rejection.
func TestMigrationV62UpgradeFromV61(t *testing.T) {
	t.Parallel()
	fixture := loadV62Fixture(t)
	ctx := t.Context()
	conn, err := sqlite.OpenConn(filepath.Join(t.TempDir(), "v62.db"), sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	predecessor := sqlitemigration.Schema{
		Migrations:       dbSchema.Migrations[:61],
		MigrationOptions: dbSchema.MigrationOptions[:61],
	}
	if err := sqlitemigration.Migrate(ctx, conn, predecessor); err != nil {
		t.Fatalf("migrate to V61: %v", err)
	}
	if err := sqlitex.ExecuteScript(conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('h','h');
INSERT INTO projects(project_hash, canonical_cwd) VALUES('p','/synthetic/v62');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES('v62-preexisting','codex','m','h','p',1,2,3,'/synthetic/v62.jsonl','jsonl');`, nil); err != nil {
		t.Fatalf("seed V61 row: %v", err)
	}
	if err := sqlitemigration.Migrate(ctx, conn, dbSchema); err != nil {
		t.Fatalf("migrate to V62: %v", err)
	}

	readBack := func(id string) (level, raw *string) {
		t.Helper()
		found := false
		if err := sqlitex.ExecuteTransient(conn, `SELECT thinking_level, thinking_level_raw FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
			Args: []any{id},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				found = true
				if stmt.ColumnType(0) != sqlite.TypeNull {
					v := stmt.ColumnText(0)
					level = &v
				}
				if stmt.ColumnType(1) != sqlite.TypeNull {
					v := stmt.ColumnText(1)
					raw = &v
				}
				return nil
			},
		}); err != nil {
			t.Fatalf("select %s: %v", id, err)
		}
		if !found {
			t.Fatalf("row %s missing after insert", id)
		}
		return level, raw
	}

	if level, raw := readBack("v62-preexisting"); level != nil || raw != nil {
		t.Fatalf("pre-existing V61 row must read back with both thinking-level columns NULL, got level=%v raw=%v", level, raw)
	}

	const insert = `INSERT INTO sessions
		(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, thinking_level, thinking_level_raw)
		VALUES (?, 'claude-code', 'm', 'h', 'p', 1, 2, 3, '/x.jsonl', 'jsonl', ?, ?)`
	for _, record := range fixture.Records {
		if err := sqlitex.ExecuteTransient(conn, insert, &sqlitex.ExecOptions{
			Args: []any{record.SessionID, nullableArg(record.ThinkingLevel), nullableArg(record.ThinkingLevelRaw)},
		}); err != nil {
			t.Fatalf("case %q: insert: %v", record.Name, err)
		}
		level, raw := readBack(record.SessionID)
		assertNullableEqual(t, record.Name, "thinking_level", level, record.ThinkingLevel)
		assertNullableEqual(t, record.Name, "thinking_level_raw", raw, record.ThinkingLevelRaw)
		if level != nil && !schema.ThinkingLevel(*level).IsValid() {
			t.Errorf("case %q: stored thinking_level %q is not a canonical schema.ThinkingLevel", record.Name, *level)
		}
	}

	for _, rejection := range fixture.Rejections {
		if err := sqlitex.ExecuteTransient(conn, rejection.SQL, nil); err == nil {
			t.Errorf("case %q: expected the sessions.thinking_level CHECK to refuse this insert, it succeeded", rejection.Name)
		}
	}
}

func assertNullableEqual(t *testing.T, name, column string, got, want *string) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Errorf("case %q: %s = %v, want %v (NULL and a value must stay distinct)", name, column, deref(got), deref(want))
	case !bytes.Equal([]byte(*got), []byte(*want)):
		t.Errorf("case %q: %s = %q, want exact bytes %q", name, column, *got, *want)
	}
}

func deref(v *string) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprintf("%q", *v)
}
