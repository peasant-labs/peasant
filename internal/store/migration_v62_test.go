package store

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/migration_v62.yaml
var migrationV62YAML []byte

//go:embed testdata/migration_v62.manifest.yaml
var migrationV62ManifestYAML []byte

type v62GenerationSeed struct {
	ID            string `yaml:"id"`
	MetadataJSON  string `yaml:"metadataJson"`
	InstalledAtMs int64  `yaml:"installedAtMs"`
	ActivatedAtMs *int64 `yaml:"activatedAtMs"`
}

type v62ExpectedStats struct {
	TurnCount            *int    `yaml:"turnCount"`
	InputSubmissionCount *int64  `yaml:"inputSubmissionCount"`
	ToolCallCount        *int    `yaml:"toolCallCount"`
	SubagentCount        *int    `yaml:"subagentCount"`
	DurationMs           *int64  `yaml:"durationMs"`
	TokensIn             *int    `yaml:"tokensIn"`
	TokensOut            *int    `yaml:"tokensOut"`
	ThoughtTokens        *int    `yaml:"thoughtTokens"`
	CachedReadTokens     *int    `yaml:"cachedReadTokens"`
	CachedWriteTokens    *int    `yaml:"cachedWriteTokens"`
	SeedJSON             *string `yaml:"seedJson"`
	Source               string  `yaml:"source"`
	UpdatedAtMs          int64   `yaml:"updatedAtMs"`
}

type v62StatsCase struct {
	Name             string              `yaml:"name"`
	SessionID        string              `yaml:"sessionId"`
	ActiveGeneration *string             `yaml:"activeGeneration"`
	Generations      []v62GenerationSeed `yaml:"generations"`
	Expected         *v62ExpectedStats   `yaml:"expected"`
	ExpectedFlag     int                 `yaml:"expectedFlag"`
}

type v62AnnotationRow struct {
	Name         string `yaml:"name"`
	AnnotationID string `yaml:"annotationId"`
	SessionID    string `yaml:"sessionId"`
	EntryIndex   int    `yaml:"entryIndex"`
	EndIndex     int    `yaml:"endIndex"`
}

type v62Fixtures struct {
	StatsCases     []v62StatsCase     `yaml:"statsCases"`
	AnnotationRows []v62AnnotationRow `yaml:"annotationRows"`
}

func loadMigrationV62Fixtures(t *testing.T) v62Fixtures {
	t.Helper()
	var fixtures v62Fixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(migrationV62YAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode migration_v62.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(migrationV62ManifestYAML)
	if err != nil {
		t.Fatalf("decode migration_v62 manifest: %v", err)
	}
	actual := make([]string, 0)
	for _, c := range fixtures.StatsCases {
		actual = append(actual, c.Name)
	}
	for _, r := range fixtures.AnnotationRows {
		actual = append(actual, r.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "v62 migration"); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// TestMigrationV62Backfills proves the v62 migration carries its backfills and
// the annotation-target rebuild over a populated predecessor: stats rows and
// seed documents from the active generation metadata, the sweep flag, and the
// annotation targets carried over without their foreign key.
//
// A V61 database is seeded from the typed fixture, then migrated to V62. The
// migration must preserve every seeded row, extract the stats the live store
// shape carries, and leave the JSON columns of the three generation-keyed
// tables byte-identical: their reshape lands with the harmonized writer, not
// with this migration or the conversion command.
func TestMigrationV62Backfills(t *testing.T) {
	t.Parallel()
	fixtures := loadMigrationV62Fixtures(t)
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "v62-backfill.db")
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite, sqlite.OpenCreate)
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

	seedV62Predecessor(t, conn, fixtures)

	if err := sqlitemigration.Migrate(ctx, conn, dbSchema); err != nil {
		t.Fatalf("migrate to V62: %v", err)
	}

	assertV62StatsBackfill(t, conn, fixtures)
	assertV62AnnotationRebuild(t, conn, fixtures)
	assertV62SearchStore(t, conn)
	assertV62Immutability(t, conn)

	// The synthetic annotation rows have no parents by construction (seeded
	// with foreign keys off); remove them before the integrity gate so the
	// gate proves the migration's own references, not the test setup.
	execV62SQL(t, conn, `DELETE FROM annotation_target_entries WHERE annotation_id LIKE 'ann-v62-%'`)
	assertV62NoFKViolations(t, conn)

	var userVersion int
	queryV62Row(t, conn, "PRAGMA user_version", nil, func(stmt *sqlite.Stmt) {
		userVersion = int(stmt.ColumnInt64(0))
	})
	if userVersion != CurrentSchemaVersion() {
		t.Fatalf("user_version after migration = %d, want the build schema %d", userVersion, CurrentSchemaVersion())
	}
	if got := v62ObjectSQL(t, conn, "index", "idx_session_projection_entries_partition"); got != "" {
		t.Fatalf("duplicate partition index survived v62: %s", got)
	}
}

// seedV62Predecessor populates a V61 database with the fixture sessions,
// generations, and annotation targets. Foreign
// keys stay off during seeding: the annotation rows deliberately carry no
// parents, and the migration itself runs with foreign keys off.
func seedV62Predecessor(t *testing.T, conn *sqlite.Conn, fixtures v62Fixtures) {
	t.Helper()
	execV62SQL(t, conn, `PRAGMA foreign_keys=OFF`)
	execV62SQL(t, conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('v62-host','v62-host');
INSERT INTO projects(project_hash, canonical_cwd) VALUES('v62-project','/synthetic/v62');`)
	seen := map[string]bool{}
	declareSession := func(sessionID string, active *string) {
		if seen[sessionID] {
			return
		}
		seen[sessionID] = true
		activeValue := "NULL"
		if active != nil {
			activeValue = "'" + *active + "'"
		}
		execV62SQL(t, conn, fmt.Sprintf(`
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version, active_generation_id)
VALUES('%s','opencode','v62-model','v62-host','v62-project',1,2,3,'/synthetic/v62.jsonl','jsonl',11,%s);`, sessionID, activeValue))
	}
	digest := strings.Repeat("0", 64)
	for _, c := range fixtures.StatsCases {
		declareSession(c.SessionID, c.ActiveGeneration)
		for _, g := range c.Generations {
			activated := "NULL"
			if g.ActivatedAtMs != nil {
				activated = fmt.Sprintf("%d", *g.ActivatedAtMs)
			}
			execV62SQL(t, conn, fmt.Sprintf(`
INSERT INTO session_projection_generations(session_id, generation_id, metadata_json, source_evidence_digest, completeness, index_format_version, installed_at_ms, activated_at_ms)
VALUES('%s','%s','%s','%s','complete',2,%d,%s);`,
				c.SessionID, g.ID, escapeV62Literal(t, g.MetadataJSON), digest, g.InstalledAtMs, activated))
		}
	}
	for _, r := range fixtures.AnnotationRows {
		execV62SQL(t, conn, fmt.Sprintf(`
INSERT INTO annotation_target_entries(annotation_id, session_id, entry_index, end_index)
VALUES('%s','%s',%d,%d);`, r.AnnotationID, r.SessionID, r.EntryIndex, r.EndIndex))
	}
}

func escapeV62Literal(t *testing.T, s string) string {
	t.Helper()
	if strings.Contains(s, "'") {
		t.Fatalf("fixture literal carries a single quote; keep fixture JSON free of quotes that need escaping: %.40q", s)
	}
	return s
}

func execV62SQL(t *testing.T, conn *sqlite.Conn, script string) {
	t.Helper()
	if err := sqlitex.ExecuteScript(conn, script, nil); err != nil {
		t.Fatalf("exec SQL: %v\nscript: %.200s", err, script)
	}
}

func queryV62Row(t *testing.T, conn *sqlite.Conn, query string, args []any, fn func(*sqlite.Stmt)) {
	t.Helper()
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		fn(stmt)
		return nil
	}}); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
}

// v62ObjectSQL returns the sqlite_master SQL for a schema object, or "" when
// the object is absent. indexDefinition fails on absence, so presence checks
// for triggers and dropped indexes need this absence-tolerant form.
func v62ObjectSQL(t *testing.T, conn *sqlite.Conn, objType, name string) string {
	t.Helper()
	sql := ""
	queryV62Row(t, conn, `SELECT sql FROM sqlite_master WHERE type=? AND name=?`, []any{objType, name}, func(stmt *sqlite.Stmt) {
		sql = stmt.ColumnText(0)
	})
	return sql
}

func assertV62StatsBackfill(t *testing.T, conn *sqlite.Conn, fixtures v62Fixtures) {
	t.Helper()
	for _, c := range fixtures.StatsCases {
		var flag int
		queryV62Row(t, conn, `SELECT content_sweep_pending FROM sessions WHERE session_id=?`, []any{c.SessionID}, func(stmt *sqlite.Stmt) {
			flag = int(stmt.ColumnInt64(0))
		})
		if flag != c.ExpectedFlag {
			t.Errorf("%s: content_sweep_pending = %d, want %d", c.Name, flag, c.ExpectedFlag)
		}
		if c.Expected == nil {
			rows := 0
			queryV62Row(t, conn, `SELECT COUNT(*) FROM session_captured_stats WHERE session_id=?`, []any{c.SessionID}, func(stmt *sqlite.Stmt) {
				rows = int(stmt.ColumnInt64(0))
			})
			if rows != 0 {
				t.Errorf("%s: session_captured_stats holds %d rows, want none", c.Name, rows)
			}
			continue
		}
		got := map[string]any{}
		cols := []string{"turn_count", "input_submission_count", "tool_call_count", "subagent_count", "duration_ms", "tokens_in", "tokens_out", "thought_tokens", "cached_read_tokens", "cached_write_tokens", "seed_json", "source", "updated_at_ms", "overflow"}
		queryV62Row(t, conn, `SELECT turn_count, input_submission_count, tool_call_count, subagent_count, duration_ms, tokens_in, tokens_out, thought_tokens, cached_read_tokens, cached_write_tokens, seed_json, source, updated_at_ms, overflow FROM session_captured_stats WHERE session_id=?`, []any{c.SessionID}, func(stmt *sqlite.Stmt) {
			for i, col := range cols {
				if stmt.ColumnType(i) == sqlite.TypeNull {
					got[col] = nil
				} else {
					got[col] = stmt.ColumnText(i)
				}
			}
		})
		if len(got) == 0 {
			t.Fatalf("%s: no session_captured_stats row for %s", c.Name, c.SessionID)
		}
		checkInt := func(col string, v *int) {
			t.Helper()
			if v == nil {
				if got[col] != nil {
					t.Errorf("%s: %s = %v, want NULL", c.Name, col, got[col])
				}
				return
			}
			if got[col] != fmt.Sprintf("%d", *v) {
				t.Errorf("%s: %s = %v, want %d", c.Name, col, got[col], *v)
			}
		}
		checkInt64 := func(col string, v *int64) {
			t.Helper()
			if v == nil {
				if got[col] != nil {
					t.Errorf("%s: %s = %v, want NULL", c.Name, col, got[col])
				}
				return
			}
			if got[col] != fmt.Sprintf("%d", *v) {
				t.Errorf("%s: %s = %v, want %d", c.Name, col, got[col], *v)
			}
		}
		checkInt("turn_count", c.Expected.TurnCount)
		checkInt64("input_submission_count", c.Expected.InputSubmissionCount)
		checkInt("tool_call_count", c.Expected.ToolCallCount)
		checkInt("subagent_count", c.Expected.SubagentCount)
		checkInt64("duration_ms", c.Expected.DurationMs)
		checkInt("tokens_in", c.Expected.TokensIn)
		checkInt("tokens_out", c.Expected.TokensOut)
		checkInt("thought_tokens", c.Expected.ThoughtTokens)
		checkInt("cached_read_tokens", c.Expected.CachedReadTokens)
		checkInt("cached_write_tokens", c.Expected.CachedWriteTokens)
		if c.Expected.SeedJSON == nil {
			if got["seed_json"] != nil {
				t.Errorf("%s: seed_json = %v, want NULL", c.Name, got["seed_json"])
			}
		} else if got["seed_json"] != *c.Expected.SeedJSON {
			t.Errorf("%s: seed_json = %.80v, want %.80s", c.Name, got["seed_json"], *c.Expected.SeedJSON)
		} else if valid := 0; true {
			queryV62Row(t, conn, `SELECT json_valid(seed_json) FROM session_captured_stats WHERE session_id=?`, []any{c.SessionID}, func(stmt *sqlite.Stmt) {
				valid = int(stmt.ColumnInt64(0))
			})
			if valid != 1 {
				t.Errorf("%s: seed_json fails json_valid", c.Name)
			}
		}
		if got["source"] != c.Expected.Source {
			t.Errorf("%s: source = %v, want %s", c.Name, got["source"], c.Expected.Source)
		}
		if got["updated_at_ms"] != fmt.Sprintf("%d", c.Expected.UpdatedAtMs) {
			t.Errorf("%s: updated_at_ms = %v, want %d", c.Name, got["updated_at_ms"], c.Expected.UpdatedAtMs)
		}
		if got["overflow"] != nil {
			t.Errorf("%s: overflow = %v, want NULL", c.Name, got["overflow"])
		}
	}
}

func assertV62AnnotationRebuild(t *testing.T, conn *sqlite.Conn, fixtures v62Fixtures) {
	t.Helper()
	for _, r := range fixtures.AnnotationRows {
		found := false
		queryV62Row(t, conn, `SELECT session_id, entry_index, end_index FROM annotation_target_entries WHERE annotation_id=?`, []any{r.AnnotationID}, func(stmt *sqlite.Stmt) {
			found = true
			if stmt.ColumnText(0) != r.SessionID || int(stmt.ColumnInt64(1)) != r.EntryIndex || int(stmt.ColumnInt64(2)) != r.EndIndex {
				t.Errorf("%s: rebuilt row = (%s,%d,%d), want (%s,%d,%d)", r.Name,
					stmt.ColumnText(0), stmt.ColumnInt64(1), stmt.ColumnInt64(2),
					r.SessionID, r.EntryIndex, r.EndIndex)
			}
		})
		if !found {
			t.Errorf("%s: annotation target row %s missing after rebuild", r.Name, r.AnnotationID)
		}
	}
	definition := ""
	queryV62Row(t, conn, `SELECT sql FROM sqlite_master WHERE type='table' AND name='annotation_target_entries'`, nil, func(stmt *sqlite.Stmt) {
		definition = stmt.ColumnText(0)
	})
	if strings.Contains(definition, "session_entries") {
		t.Errorf("rebuilt annotation_target_entries still references session_entries: %s", definition)
	}
	if !strings.Contains(definition, "REFERENCES annotations(id)") {
		t.Errorf("rebuilt annotation_target_entries lost its annotations cascade: %s", definition)
	}
	if sql := v62ObjectSQL(t, conn, "index", "idx_ann_target_entry"); !strings.Contains(sql, "annotation_target_entries") {
		t.Errorf("idx_ann_target_entry missing or misplaced after rebuild: %s", sql)
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO annotation_target_entries(annotation_id, session_id, entry_index, end_index) VALUES('ann-v62-check','s-v62-full',5,5)`, nil); err == nil {
		t.Errorf("rebuilt annotation_target_entries admits end_index <= entry_index; the CHECK must hold")
	} else {
		_ = sqlitex.ExecuteTransient(conn, `DELETE FROM annotation_target_entries WHERE annotation_id='ann-v62-check'`, nil)
	}
}

func assertV62SearchStore(t *testing.T, conn *sqlite.Conn) {
	t.Helper()
	for _, name := range []string{"session_search_source", "session_search_fts", "session_search_state"} {
		found := false
		queryV62Row(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE name=?`, []any{name}, func(stmt *sqlite.Stmt) {
			found = stmt.ColumnInt64(0) == 1
		})
		if !found {
			t.Errorf("v62 search object %s is missing", name)
		}
	}
	needsRebuild := -1
	queryV62Row(t, conn, `SELECT needs_rebuild FROM session_search_state WHERE id=1`, nil, func(stmt *sqlite.Stmt) {
		needsRebuild = int(stmt.ColumnInt64(0))
	})
	if needsRebuild != 0 {
		t.Errorf("session_search_state.needs_rebuild = %d, want 0", needsRebuild)
	}
	for _, trigger := range []string{"session_entry_bodies_fts_ai", "session_entry_bodies_fts_bd", "session_entry_bodies_immutable", "session_generations_immutable"} {
		if sql := v62ObjectSQL(t, conn, "trigger", trigger); sql == "" {
			t.Errorf("v62 trigger %s is missing", trigger)
		}
	}
}

func assertV62Immutability(t *testing.T, conn *sqlite.Conn) {
	t.Helper()
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role, has_tool_use, has_thinking, is_error, depth) VALUES(1125899906842624,'s-v62-full','`+strings.Repeat("a", 64)+`',0,'opencode','text','user',0,0,0,0)`, nil); err != nil {
		t.Fatalf("seed body row: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_entry_bodies SET depth=1 WHERE body_id=1125899906842624`, nil); err == nil {
		t.Errorf("session_entry_bodies accepts UPDATE; the immutability trigger must refuse")
	}
	_ = sqlitex.ExecuteTransient(conn, `DELETE FROM session_entry_bodies WHERE body_id=1125899906842624`, nil)
}

func assertV62NoFKViolations(t *testing.T, conn *sqlite.Conn) {
	t.Helper()
	violations := []string{}
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA foreign_key_check`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			violations = append(violations, fmt.Sprintf("%s rowid %d -> %s", stmt.ColumnText(0), stmt.ColumnInt64(1), stmt.ColumnText(2)))
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Errorf("foreign_key_check reports %d violations after v62: %v", len(violations), violations)
	}
}
