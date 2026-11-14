package store

import (
	_ "embed"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/migration_v79_outcome_reasons.yaml
var migrationV79YAML []byte

//go:embed testdata/migration_v79_outcome_reasons.manifest.yaml
var migrationV79ManifestYAML []byte

// migrationV79Row is one schema-78 outcome row and the reason id migration 79
// must give it.
type migrationV79Row struct {
	Name         string  `yaml:"name"`
	ID           int64   `yaml:"id"`
	Run          int64   `yaml:"run"`
	Session      *string `yaml:"session"`
	Kind         string  `yaml:"kind"`
	Code         string  `yaml:"code"`
	CreatedAt    int64   `yaml:"createdAt"`
	WantReasonID int64   `yaml:"wantReasonId"`
}

type migrationV79Fixtures struct {
	Runs  int               `yaml:"runs"`
	Rows  []migrationV79Row `yaml:"rows"`
	Cases []struct {
		Name string `yaml:"name"`
	} `yaml:"cases"`
}

func LoadMigrationV79Fixtures(t *testing.T) migrationV79Fixtures {
	t.Helper()
	var fixtures migrationV79Fixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(migrationV79YAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode migration_v79_outcome_reasons.yaml: %v", err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
		RequiredRows  []string `yaml:"requiredRows"`
	}
	manifestDecoder := yaml.NewDecoder(strings.NewReader(string(migrationV79ManifestYAML)))
	manifestDecoder.KnownFields(true)
	if err := manifestDecoder.Decode(&manifest); err != nil {
		t.Fatalf("decode migration_v79_outcome_reasons manifest: %v", err)
	}
	if len(manifest.RequiredNames) == 0 || len(manifest.RequiredRows) == 0 {
		t.Fatal("migration_v79_outcome_reasons manifest declares no required names")
	}
	caseNames := make([]string, 0, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		caseNames = append(caseNames, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest.RequiredNames, caseNames, "v79 outcome reason cases"); err != nil {
		t.Fatal(err)
	}
	rowNames := make([]string, 0, len(fixtures.Rows))
	for _, row := range fixtures.Rows {
		rowNames = append(rowNames, row.Name)
	}
	if err := validateRecoveryRequiredNames(manifest.RequiredRows, rowNames, "v79 outcome reason rows"); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// openMigrationV79Store builds a schema-78 store on the frozen chain with the
// given number of audit runs.
func openMigrationV79Store(t *testing.T, runs int) *sqlite.Conn {
	t.Helper()
	conn, err := sqlite.OpenConn(filepath.Join(t.TempDir(), "audit.db"), sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(78)); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA foreign_keys=ON;`, nil); err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= runs; run++ {
		if err := sqlitex.Execute(conn, `INSERT INTO ingest_log(id, started_at) VALUES(?, ?)`, &sqlitex.ExecOptions{Args: []any{run, run}}); err != nil {
			t.Fatal(err)
		}
	}
	return conn
}

func insertMigrationV79Row(t *testing.T, conn *sqlite.Conn, row migrationV79Row) {
	t.Helper()
	var session any
	if row.Session != nil {
		session = *row.Session
	}
	if err := sqlitex.Execute(conn, `INSERT INTO ingest_run_outcomes(id, run_id, session_id, kind, reason_code, created_at) VALUES(?,?,?,?,?,?)`, &sqlitex.ExecOptions{Args: []any{row.ID, row.Run, session, row.Kind, row.Code, row.CreatedAt}}); err != nil {
		t.Fatalf("seed row %s: %v", row.Name, err)
	}
}

type migratedOutcomeRow struct {
	run       int64
	session   *string
	reasonID  int64
	createdAt int64
}

func readMigratedOutcomeRows(t *testing.T, conn *sqlite.Conn) map[int64]migratedOutcomeRow {
	t.Helper()
	rows := make(map[int64]migratedOutcomeRow)
	queryV62Row(t, conn, `SELECT id, run_id, session_id, reason_id, created_at FROM ingest_run_outcomes`, nil, func(stmt *sqlite.Stmt) {
		row := migratedOutcomeRow{run: stmt.ColumnInt64(1), reasonID: stmt.ColumnInt64(3), createdAt: stmt.ColumnInt64(4)}
		if stmt.ColumnType(2) != sqlite.TypeNull {
			session := stmt.ColumnText(2)
			row.session = &session
		}
		rows[stmt.ColumnInt64(0)] = row
	})
	return rows
}

func TestMigrationV79OutcomeReasons(t *testing.T) {
	fixtures := LoadMigrationV79Fixtures(t)
	byName := make(map[string]migrationV79Row, len(fixtures.Rows))
	for _, row := range fixtures.Rows {
		byName[row.Name] = row
	}
	// The fixture must hold one row for every (kind, code) pair a build before
	// the migration could write: every reason that is not a refresh, with the
	// worker-error capture failure under its old spelling.
	covered := make(map[int64]bool, len(fixtures.Rows))
	for _, row := range fixtures.Rows {
		covered[row.WantReasonID] = true
	}
	for _, reason := range ingest.AllOutcomeReasons {
		if reason.Kind() != ingest.OutcomeKindRefresh && !covered[int64(reason)] {
			t.Fatalf("migration_v79_outcome_reasons.yaml has no row that lands on reason %d (%s); add one named row for its (kind, code) pair", reason, reason.Code())
		}
	}

	conn := openMigrationV79Store(t, fixtures.Runs)
	for _, row := range fixtures.Rows {
		insertMigrationV79Row(t, conn, row)
	}
	if err := sqlitex.ExecuteScript(conn, `INSERT INTO index_log(session_id, provider, outcome, index_version, entries_count, started_at) VALUES('77000000-0000-4000-8000-000000000001', 'claude-code', 'indexed', 1, 3, 5);`, nil); err != nil {
		t.Fatalf("seed a schema-78 index_log row: %v", err)
	}
	var sequenceBefore int64
	queryV62Row(t, conn, `SELECT seq FROM sqlite_sequence WHERE name='ingest_run_outcomes'`, nil, func(stmt *sqlite.Stmt) { sequenceBefore = stmt.ColumnInt64(0) })
	// Push the sequence past the highest surviving id, as pruning of a newer
	// row would leave it.
	if err := sqlitex.ExecuteScript(conn, `UPDATE sqlite_sequence SET seq = seq + 40 WHERE name='ingest_run_outcomes';`, nil); err != nil {
		t.Fatal(err)
	}
	sequenceBefore += 40

	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(79)); err != nil {
		t.Fatalf("migration 79: %v", err)
	}
	migrated := readMigratedOutcomeRows(t, conn)

	runners := map[string]func(t *testing.T){
		"every-existing-row-lands-on-its-reason-id": func(t *testing.T) {
			for _, row := range fixtures.Rows {
				got, ok := migrated[row.ID]
				if !ok {
					t.Fatalf("row %s (id %d) was not copied", row.Name, row.ID)
				}
				if got.reasonID != row.WantReasonID {
					t.Errorf("row %s: reason id = %d, want %d", row.Name, got.reasonID, row.WantReasonID)
				}
				sameSession := (got.session == nil) == (row.Session == nil) && (got.session == nil || *got.session == *row.Session)
				if got.run != row.Run || got.createdAt != row.CreatedAt || !sameSession {
					t.Errorf("row %s: run, session or created_at changed: %+v", row.Name, got)
				}
			}
		},
		"worker-error-refresh-unavailable-rows-become-adapter-capture-failed": func(t *testing.T) {
			for _, name := range []string{"worker-error-adapter-refresh-unavailable", "both-rows-worker-error"} {
				if got := migrated[byName[name].ID].reasonID; got != int64(ingest.OutcomeReasonAdapterCaptureFailed) {
					t.Errorf("row %s: reason id = %d, want adapter_capture_failed (%d)", name, got, ingest.OutcomeReasonAdapterCaptureFailed)
				}
			}
		},
		"diagnostic-refresh-unavailable-rows-keep-their-code": func(t *testing.T) {
			for _, name := range []string{"diagnostic-adapter-refresh-unavailable", "both-rows-diagnostic"} {
				if got := migrated[byName[name].ID].reasonID; got != int64(ingest.OutcomeReasonAdapterRefreshUnavailable) {
					t.Errorf("row %s: reason id = %d, want adapter_refresh_unavailable (%d)", name, got, ingest.OutcomeReasonAdapterRefreshUnavailable)
				}
			}
		},
		"one-session-with-both-rows-gets-two-different-ids": func(t *testing.T) {
			worker, diagnostic := byName["both-rows-worker-error"], byName["both-rows-diagnostic"]
			if *worker.Session != *diagnostic.Session {
				t.Fatal("fixture rows both-rows-* must share one session")
			}
			if migrated[worker.ID].reasonID == migrated[diagnostic.ID].reasonID {
				t.Fatal("one session's worker error and diagnostic collapsed onto one reason id")
			}
		},
		"run-wide-diagnostic-keeps-its-null-session": func(t *testing.T) {
			row := byName["run-wide-diagnostic"]
			if row.Session != nil {
				t.Fatal("fixture row run-wide-diagnostic must have a null session")
			}
			if got, ok := migrated[row.ID]; !ok || got.session != nil {
				t.Fatalf("run-wide diagnostic = %+v, want a NULL session", got)
			}
		},
		"unlisted-stored-code-is-seeded-once-as-retired-above-the-fixed-range": func(t *testing.T) {
			first, second := byName["unlisted-code-first"], byName["unlisted-code-second"]
			seeded := 0
			queryV62Row(t, conn, `SELECT r.id, k.name, r.retired FROM ingest_outcome_reasons r JOIN ingest_outcome_kinds k ON k.id=r.kind_id WHERE r.code=?`, []any{first.Code}, func(stmt *sqlite.Stmt) {
				seeded++
				if stmt.ColumnInt64(0) < ingest.OutcomeReasonLocalIDBase || stmt.ColumnText(1) != first.Kind || stmt.ColumnInt(2) != 1 {
					t.Errorf("unlisted code seeded as id %d kind %s retired %d; want an id at or above %d, the kind of its lowest row (%s), retired", stmt.ColumnInt64(0), stmt.ColumnText(1), stmt.ColumnInt(2), ingest.OutcomeReasonLocalIDBase, first.Kind)
				}
			})
			if seeded != 1 {
				t.Fatalf("unlisted code seeded %d times, want once", seeded)
			}
			if migrated[first.ID].reasonID != migrated[second.ID].reasonID || migrated[first.ID].reasonID < ingest.OutcomeReasonLocalIDBase {
				t.Fatalf("unlisted rows landed on %d and %d", migrated[first.ID].reasonID, migrated[second.ID].reasonID)
			}
			queryV62Row(t, conn, `SELECT count(*) FROM ingest_outcome_reasons WHERE id >= ?`, []any{ingest.OutcomeReasonLocalIDBase}, func(stmt *sqlite.Stmt) {
				if stmt.ColumnInt(0) != 1 {
					t.Errorf("%d store-local reasons seeded, want only the one unlisted code", stmt.ColumnInt(0))
				}
			})
		},
		"row-count-is-the-same-before-and-after": func(t *testing.T) {
			if len(migrated) != len(fixtures.Rows) {
				t.Fatalf("%d rows after the migration, %d before", len(migrated), len(fixtures.Rows))
			}
		},
		"index-log-rows-read-as-not-measured": func(t *testing.T) {
			queryV62Row(t, conn, `SELECT count(*) FROM index_log WHERE parse_ms IS NULL AND write_ms IS NULL AND bytes IS NULL`, nil, func(stmt *sqlite.Stmt) {
				if stmt.ColumnInt(0) != 1 {
					t.Error("a row written before the migration does not read as not measured")
				}
			})
			if err := sqlitex.ExecuteScript(conn, `INSERT INTO index_log(session_id, provider, outcome, index_version, entries_count, started_at, parse_ms) VALUES('x', 'claude-code', 'indexed', 1, 0, 5, -1);`, nil); err == nil {
				t.Fatal("index_log admitted a negative parse time")
			}
		},
		"autoincrement-sequence-survives-the-rebuild": func(t *testing.T) {
			queryV62Row(t, conn, `SELECT seq FROM sqlite_sequence WHERE name='ingest_run_outcomes'`, nil, func(stmt *sqlite.Stmt) {
				if stmt.ColumnInt64(0) != sequenceBefore {
					t.Errorf("sequence = %d after the rebuild, want %d: a pruned id could be reused", stmt.ColumnInt64(0), sequenceBefore)
				}
			})
			queryV62Row(t, conn, `SELECT count(*) FROM sqlite_sequence WHERE name='ingest_run_outcomes_v2'`, nil, func(stmt *sqlite.Stmt) {
				if stmt.ColumnInt(0) != 0 {
					t.Error("the rebuild left a sequence row under the temporary table name")
				}
			})
		},
		"rows-of-runs-older-than-the-retained-window-are-not-copied": func(t *testing.T) {
			old := openMigrationV79Store(t, IngestOutcomeRetainedRuns+2)
			session := "77000000-0000-4000-8000-000000000001"
			insertMigrationV79Row(t, old, migrationV79Row{Name: "outside-window", ID: 1, Run: 2, Session: &session, Kind: "worker_error", Code: "error", CreatedAt: 1})
			insertMigrationV79Row(t, old, migrationV79Row{Name: "oldest-retained", ID: 2, Run: 3, Session: &session, Kind: "worker_error", Code: "error", CreatedAt: 2})
			insertMigrationV79Row(t, old, migrationV79Row{Name: "newest", ID: 3, Run: int64(IngestOutcomeRetainedRuns + 2), Session: &session, Kind: "worker_error", Code: "error", CreatedAt: 3})
			if err := sqlitemigration.Migrate(t.Context(), old, frozenSchema(79)); err != nil {
				t.Fatalf("migration 79: %v", err)
			}
			copied := readMigratedOutcomeRows(t, old)
			if _, kept := copied[1]; kept || len(copied) != 2 {
				t.Fatalf("copied rows = %+v, want only the rows of the newest %d runs", copied, IngestOutcomeRetainedRuns)
			}
			queryV62Row(t, old, `SELECT count(*) FROM ingest_log`, nil, func(stmt *sqlite.Stmt) {
				if stmt.ColumnInt(0) != IngestOutcomeRetainedRuns+2 {
					t.Error("the migration deleted an audit run row")
				}
			})
		},
		// Last: it deletes the audit runs.
		"rebuilt-table-keeps-the-cascade-and-the-run-index": func(t *testing.T) {
			if sql := v62ObjectSQL(t, conn, "index", "idx_ingest_run_outcomes_run"); !strings.Contains(sql, "ingest_run_outcomes(run_id, id)") {
				t.Fatalf("run index after the rebuild = %q", sql)
			}
			if sql := v62ObjectSQL(t, conn, "table", "ingest_run_outcomes_v2"); sql != "" {
				t.Fatal("the temporary table survived the rebuild")
			}
			if err := sqlitex.ExecuteScript(conn, `INSERT INTO ingest_run_stages(run_id, stage, duration_ms) VALUES(1, 'INDEX', 4); DELETE FROM ingest_log;`, nil); err != nil {
				t.Fatal(err)
			}
			queryV62Row(t, conn, `SELECT (SELECT count(*) FROM ingest_run_outcomes) + (SELECT count(*) FROM ingest_run_stages)`, nil, func(stmt *sqlite.Stmt) {
				if stmt.ColumnInt(0) != 0 {
					t.Error("outcome or stage rows survived the deletion of their audit run")
				}
			})
		},
	}
	for _, c := range fixtures.Cases {
		run, ok := runners[c.Name]
		if !ok {
			t.Fatalf("migration_v79_outcome_reasons.yaml: case %q has no runner; add it to the runner", c.Name)
		}
		t.Run(c.Name, run)
	}
}
