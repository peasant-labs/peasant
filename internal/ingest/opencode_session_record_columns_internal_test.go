package ingest

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/opencode_session_record_columns.yaml
var openCodeSessionRecordColumnsYAML []byte

type openCodeSessionRecordColumnsFixture struct {
	RequiredCases []string                          `yaml:"required_cases"`
	Cases         []openCodeSessionRecordColumnCase `yaml:"cases"`
}

type openCodeSessionRecordColumnCase struct {
	Name     string                           `yaml:"name"`
	Extended bool                             `yaml:"extended"`
	Rows     []openCodeSessionRecordColumnRow `yaml:"rows"`
}

type openCodeSessionRecordColumnRow struct {
	ID               string  `yaml:"id"`
	ParentID         string  `yaml:"parent_id"`
	TimeCreated      int64   `yaml:"time_created"`
	TimeUpdated      int64   `yaml:"time_updated"`
	Directory        string  `yaml:"directory"`
	Title            string  `yaml:"title"`
	Agent            string  `yaml:"agent"`
	TokensInput      int64   `yaml:"tokens_input"`
	TokensOutput     int64   `yaml:"tokens_output"`
	TokensReasoning  int64   `yaml:"tokens_reasoning"`
	TokensCacheRead  int64   `yaml:"tokens_cache_read"`
	TokensCacheWrite int64   `yaml:"tokens_cache_write"`
	Cost             float64 `yaml:"cost"`
	Version          string  `yaml:"version"`
	Slug             string  `yaml:"slug"`
	Revert           string  `yaml:"revert"`
}

func loadOpenCodeSessionRecordColumnsFixture(t *testing.T) openCodeSessionRecordColumnsFixture {
	t.Helper()
	var fixture openCodeSessionRecordColumnsFixture
	if err := yaml.Unmarshal(openCodeSessionRecordColumnsYAML, &fixture); err != nil {
		t.Fatalf("decode session-record columns fixture: %v", err)
	}
	presentRec := make(map[string]struct{}, len(fixture.Cases))
	for _, c := range fixture.Cases {
		presentRec[c.Name] = struct{}{}
	}
	if len(fixture.RequiredCases) == 0 {
		t.Fatal("session-record columns fixture declares no required cases")
	}
	for _, name := range fixture.RequiredCases {
		if _, ok := presentRec[name]; !ok {
			t.Fatalf("session-record columns fixture is missing required case %q", name)
		}
	}
	return fixture
}

// materializeExtendedSessionDatabase writes a synthetic OpenCode database whose
// session table carries either the extended column set or only the base
// attribution columns, then inserts one row per fixture entry. It exercises the
// production source against both real layouts.
func materializeExtendedSessionDatabase(t *testing.T, testCase openCodeSessionRecordColumnCase) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "opencode.db")
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite|sqlite.OpenCreate)
	if err != nil {
		t.Fatalf("open synthetic database: %v", err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			t.Fatalf("close synthetic database: %v", closeErr)
		}
	}()

	schema := `CREATE TABLE session (
  id TEXT PRIMARY KEY,
  parent_id TEXT,
  time_created INTEGER NOT NULL DEFAULT 0,
  time_updated INTEGER NOT NULL DEFAULT 0,
  directory TEXT,
  title TEXT
);`
	if testCase.Extended {
		schema = `CREATE TABLE session (
  id TEXT PRIMARY KEY,
  parent_id TEXT,
  time_created INTEGER NOT NULL DEFAULT 0,
  time_updated INTEGER NOT NULL DEFAULT 0,
  directory TEXT,
  title TEXT,
  agent TEXT,
  tokens_input INTEGER,
  tokens_output INTEGER,
  tokens_reasoning INTEGER,
  tokens_cache_read INTEGER,
  tokens_cache_write INTEGER,
  cost REAL,
  version TEXT,
  slug TEXT,
  revert TEXT
);`
	}
	if err := sqlitex.ExecuteScript(conn, schema, nil); err != nil {
		t.Fatalf("create synthetic session schema: %v", err)
	}
	for _, row := range testCase.Rows {
		parent := nullableSessionText(row.ParentID)
		if testCase.Extended {
			if err := sqlitex.Execute(conn, `INSERT INTO session (id, parent_id, time_created, time_updated, directory, title, agent, tokens_input, tokens_output, tokens_reasoning, tokens_cache_read, tokens_cache_write, cost, version, slug, revert) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16);`, &sqlitex.ExecOptions{Args: []any{row.ID, parent, row.TimeCreated, row.TimeUpdated, row.Directory, row.Title, row.Agent, row.TokensInput, row.TokensOutput, row.TokensReasoning, row.TokensCacheRead, row.TokensCacheWrite, row.Cost, row.Version, row.Slug, row.Revert}}); err != nil {
				t.Fatalf("insert extended session row %q: %v", row.ID, err)
			}
			continue
		}
		if err := sqlitex.Execute(conn, `INSERT INTO session (id, parent_id, time_created, time_updated, directory, title) VALUES (?1, ?2, ?3, ?4, ?5, ?6);`, &sqlitex.ExecOptions{Args: []any{row.ID, parent, row.TimeCreated, row.TimeUpdated, row.Directory, row.Title}}); err != nil {
			t.Fatalf("insert base session row %q: %v", row.ID, err)
		}
	}
	return dbPath
}

func nullableSessionText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func readExtendedSessionRecords(t *testing.T, dbPath string) map[string]OpenCodeSessionRecord {
	t.Helper()
	path, err := NewOpenCodeSQLiteSourcePath(dbPath)
	if err != nil {
		t.Fatalf("validate synthetic source path: %v", err)
	}
	opened, err := OpenOpenCodeSQLiteSource(t.Context(), path, DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("open synthetic source: %v", err)
	}
	defer func() { _ = opened.Close(t.Context()) }()
	pageSize, err := NewOpenCodeCurrentPageSize(64)
	if err != nil {
		t.Fatalf("build bounded page size: %v", err)
	}
	records := make(map[string]OpenCodeSessionRecord)
	var cursor *OpenCodeSessionRecordCursor
	for pages := 0; ; pages++ {
		page, readErr := opened.SessionRecords(t.Context(), OpenCodeSessionRecordPageRequest{PageSize: pageSize, After: cursor})
		if readErr != nil {
			t.Fatalf("read session-record page %d: %v", pages, readErr)
		}
		for _, record := range page.Records {
			records[record.SessionID.String()] = record
		}
		if page.Next == nil {
			break
		}
		cursor = page.Next
		if pages > 20 {
			t.Fatal("session-record pagination did not terminate")
		}
	}
	return records
}

// TestSessionRecordsReadExtendedColumns proves the extended session-record read
// fills agent, the token aggregates, cost, version, slug, and revert when the
// session table carries every extended column, and leaves those fields empty or
// zero for an older layout that carries only the base attribution columns while
// still resolving the base attribution.
func TestSessionRecordsReadExtendedColumns(t *testing.T) {
	fixture := loadOpenCodeSessionRecordColumnsFixture(t)
	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			dbPath := materializeExtendedSessionDatabase(t, testCase)
			records := readExtendedSessionRecords(t, dbPath)
			for _, row := range testCase.Rows {
				record, ok := records[row.ID]
				if !ok {
					t.Fatalf("session %q missing from records %v", row.ID, records)
				}
				// The base attribution resolves in both layouts.
				if record.Directory != row.Directory || record.Title != row.Title || record.TimeCreated != row.TimeCreated {
					t.Fatalf("session %q base attribution = dir %q title %q created %d, want %q %q %d", row.ID, record.Directory, record.Title, record.TimeCreated, row.Directory, row.Title, row.TimeCreated)
				}
				if testCase.Extended {
					assertExtendedFields(t, row.ID, record, row)
					continue
				}
				assertEmptyExtendedFields(t, row.ID, record)
			}
		})
	}
}

func assertExtendedFields(t *testing.T, id string, record OpenCodeSessionRecord, row openCodeSessionRecordColumnRow) {
	t.Helper()
	got := fmt.Sprintf("%s|%d|%d|%d|%d|%d|%.4f|%s|%s|%s", record.Agent, record.TokensInput, record.TokensOutput, record.TokensReasoning, record.TokensCacheRead, record.TokensCacheWrite, record.Cost, record.Version, record.Slug, record.Revert)
	want := fmt.Sprintf("%s|%d|%d|%d|%d|%d|%.4f|%s|%s|%s", row.Agent, row.TokensInput, row.TokensOutput, row.TokensReasoning, row.TokensCacheRead, row.TokensCacheWrite, row.Cost, row.Version, row.Slug, row.Revert)
	if got != want {
		t.Fatalf("session %q extended fields = %s, want %s", id, got, want)
	}
}

func assertEmptyExtendedFields(t *testing.T, id string, record OpenCodeSessionRecord) {
	t.Helper()
	if record.Agent != "" || record.TokensInput != 0 || record.TokensOutput != 0 || record.TokensReasoning != 0 || record.TokensCacheRead != 0 || record.TokensCacheWrite != 0 || record.Cost != 0 || record.Version != "" || record.Slug != "" || record.Revert != "" {
		t.Fatalf("session %q older layout leaked extended fields: %+v", id, record)
	}
}
