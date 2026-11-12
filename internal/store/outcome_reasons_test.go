package store

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/ingest_outcome_reasons.yaml
var ingestOutcomeReasonsYAML []byte

//go:embed testdata/ingest_outcome_reasons.manifest.yaml
var ingestOutcomeReasonsManifestYAML []byte

type outcomeReasonRow struct {
	ID      int64  `yaml:"id"`
	Kind    string `yaml:"kind"`
	Code    string `yaml:"code"`
	Retired bool   `yaml:"retired"`
}

type outcomeReasonFixtures struct {
	Kinds []struct {
		ID   int64  `yaml:"id"`
		Name string `yaml:"name"`
	} `yaml:"kinds"`
	Reasons []outcomeReasonRow `yaml:"reasons"`
	Cases   []struct {
		Name string `yaml:"name"`
	} `yaml:"cases"`
}

func LoadOutcomeReasonFixtures(t *testing.T) outcomeReasonFixtures {
	t.Helper()
	var fixtures outcomeReasonFixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(ingestOutcomeReasonsYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode ingest_outcome_reasons.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(ingestOutcomeReasonsManifestYAML)
	if err != nil {
		t.Fatalf("decode ingest_outcome_reasons manifest: %v", err)
	}
	names := make([]string, 0, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "ingest outcome reasons"); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// seededOutcomeReasons reads the fixed-range reason rows of a store.
func seededOutcomeReasons(t *testing.T, conn *sqlite.Conn) []outcomeReasonRow {
	t.Helper()
	var rows []outcomeReasonRow
	queryV62Row(t, conn, `SELECT r.id, k.name, r.code, r.retired FROM ingest_outcome_reasons r JOIN ingest_outcome_kinds k ON k.id = r.kind_id WHERE r.id < ? ORDER BY r.id`, []any{ingest.OutcomeReasonLocalIDBase}, func(stmt *sqlite.Stmt) {
		rows = append(rows, outcomeReasonRow{ID: stmt.ColumnInt64(0), Kind: stmt.ColumnText(1), Code: stmt.ColumnText(2), Retired: stmt.ColumnInt(3) == 1})
	})
	return rows
}

func goOutcomeReasons() []outcomeReasonRow {
	rows := make([]outcomeReasonRow, 0, len(ingest.AllOutcomeReasons))
	for _, reason := range ingest.AllOutcomeReasons {
		rows = append(rows, outcomeReasonRow{ID: int64(reason), Kind: reason.Kind().String(), Code: reason.Code(), Retired: reason.Retired()})
	}
	return rows
}

// diffOutcomeReasonSets reports, in both directions, the rows one set holds
// and the other does not.
func diffOutcomeReasonSets(want, got []outcomeReasonRow) (missing, extra []outcomeReasonRow) {
	index := func(rows []outcomeReasonRow) map[outcomeReasonRow]bool {
		set := make(map[outcomeReasonRow]bool, len(rows))
		for _, row := range rows {
			set[row] = true
		}
		return set
	}
	wantSet, gotSet := index(want), index(got)
	for _, row := range want {
		if !gotSet[row] {
			missing = append(missing, row)
		}
	}
	for _, row := range got {
		if !wantSet[row] {
			extra = append(extra, row)
		}
	}
	return missing, extra
}

func TestIngestOutcomeReasons(t *testing.T) {
	fixtures := LoadOutcomeReasonFixtures(t)
	s, _ := openGenerationStore(t)
	conn, err := s.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// One case returns the connection to read through the store, so the
	// release reads the variable when the test ends.
	defer func() { s.pool.Put(conn) }()
	if err := sqlitex.ExecuteScript(conn, `INSERT INTO ingest_log(started_at) VALUES(1);`, nil); err != nil {
		t.Fatal(err)
	}

	runners := map[string]func(t *testing.T){
		"go-constants-and-seeded-rows-match-exactly": func(t *testing.T) {
			seeded := seededOutcomeReasons(t, conn)
			// Three sets must agree exactly: the Go constants, the seeded
			// rows, and the fixture that names the id contract.
			if missing, extra := diffOutcomeReasonSets(goOutcomeReasons(), seeded); len(missing)+len(extra) != 0 {
				t.Fatalf("seeded rows differ from ingest.AllOutcomeReasons: not seeded %+v, seeded without a constant %+v", missing, extra)
			}
			if missing, extra := diffOutcomeReasonSets(fixtures.Reasons, seeded); len(missing)+len(extra) != 0 {
				t.Fatalf("seeded rows differ from the fixture: not seeded %+v, seeded but not in the fixture %+v", missing, extra)
			}
		},
		"kinds-match-exactly": func(t *testing.T) {
			type kindRow struct {
				id   int64
				name string
			}
			var got []kindRow
			queryV62Row(t, conn, `SELECT id, name FROM ingest_outcome_kinds ORDER BY id`, nil, func(stmt *sqlite.Stmt) {
				got = append(got, kindRow{stmt.ColumnInt64(0), stmt.ColumnText(1)})
			})
			if len(got) != len(fixtures.Kinds) || len(got) != len(ingest.AllOutcomeKinds) {
				t.Fatalf("seeded kinds = %+v, fixture %+v, Go %v", got, fixtures.Kinds, ingest.AllOutcomeKinds)
			}
			for i, kind := range ingest.AllOutcomeKinds {
				if got[i].id != int64(kind) ||
					got[i].name != kind.String() ||
					fixtures.Kinds[i].ID != got[i].id ||
					fixtures.Kinds[i].Name != got[i].name {
					t.Fatalf("kind %d: seeded %+v, fixture %+v, Go %d %s", i, got[i], fixtures.Kinds[i], kind, kind)
				}
			}
		},
		"constructor-refuses-an-unknown-code": func(t *testing.T) {
			if _, err := ingest.NewOutcomeReason("code_no_build_lists"); err == nil {
				t.Fatal("constructor admitted a code this build does not define")
			}
			for _, row := range fixtures.Reasons {
				reason, err := ingest.NewOutcomeReason(row.Code)
				if err != nil || int64(reason) != row.ID {
					t.Fatalf("constructor(%s) = %d, %v; want id %d", row.Code, reason, err, row.ID)
				}
			}
		},
		"every-code-is-unique-across-kinds": func(t *testing.T) {
			seen := make(map[string]int64)
			for _, reason := range ingest.AllOutcomeReasons {
				if other, dup := seen[reason.Code()]; dup {
					t.Fatalf("code %s names both reason %d and reason %d", reason.Code(), other, reason)
				}
				seen[reason.Code()] = int64(reason)
			}
			queryV62Row(t, conn, `SELECT count(*) - count(DISTINCT code) FROM ingest_outcome_reasons`, nil, func(stmt *sqlite.Stmt) {
				if stmt.ColumnInt(0) != 0 {
					t.Error("two seeded reasons share a code")
				}
			})
			// The table itself refuses a second kind for a taken code.
			err := sqlitex.Execute(conn, `INSERT INTO ingest_outcome_reasons(id, kind_id, code) VALUES(999998, 1, 'adapter_refresh_unavailable')`, nil)
			if err == nil {
				t.Fatal("a code was admitted under a second kind")
			}
		},
		"capture-failure-is-a-worker-error-named-adapter-capture-failed": func(t *testing.T) {
			reason := ingest.OutcomeReasonAdapterCaptureFailed
			if reason.Kind() != ingest.OutcomeKindWorkerError || reason.Code() != "adapter_capture_failed" {
				t.Fatalf("capture failure = %s %s", reason.Kind(), reason.Code())
			}
		},
		"refresh-unavailable-is-a-diagnostic-and-keeps-its-code": func(t *testing.T) {
			reason := ingest.OutcomeReasonAdapterRefreshUnavailable
			if reason.Kind() != ingest.OutcomeKindDiagnostic || reason.Code() != "adapter_refresh_unavailable" {
				t.Fatalf("refresh unavailable = %s %s", reason.Kind(), reason.Code())
			}
		},
		"retired-code-keeps-its-row-and-its-id": func(t *testing.T) {
			// Every reason the build marks retired still has its seeded row.
			for _, reason := range ingest.AllOutcomeReasons {
				if !reason.Retired() {
					continue
				}
				queryV62Row(t, conn, `SELECT count(*) FROM ingest_outcome_reasons WHERE id=? AND code=? AND retired=1`, []any{int64(reason), reason.Code()}, func(stmt *sqlite.Stmt) {
					if stmt.ColumnInt(0) != 1 {
						t.Errorf("retired reason %s lost its row or its id", reason.Code())
					}
				})
			}
			// Retiring a code is a flag on its row: the id and the outcome
			// rows that name it are untouched.
			reason := ingest.OutcomeReasonMissingModel
			if err := sqlitex.Execute(conn, `INSERT INTO ingest_run_outcomes(run_id, reason_id, created_at) VALUES(1,?,5)`, &sqlitex.ExecOptions{Args: []any{int64(reason)}}); err != nil {
				t.Fatal(err)
			}
			if err := sqlitex.Execute(conn, `UPDATE ingest_outcome_reasons SET retired=1 WHERE code=?`, &sqlitex.ExecOptions{Args: []any{reason.Code()}}); err != nil {
				t.Fatal(err)
			}
			queryV62Row(t, conn, `SELECT r.id FROM ingest_run_outcomes o JOIN ingest_outcome_reasons r ON r.id=o.reason_id WHERE o.created_at=5 AND r.retired=1`, nil, func(stmt *sqlite.Stmt) {
				if stmt.ColumnInt64(0) != int64(reason) {
					t.Errorf("retired reason id = %d, want %d", stmt.ColumnInt64(0), reason)
				}
			})
			if err := sqlitex.ExecuteScript(conn, `DELETE FROM ingest_run_outcomes WHERE created_at=5; UPDATE ingest_outcome_reasons SET retired=0;`, nil); err != nil {
				t.Fatal(err)
			}
		},
		"outcome-row-cannot-name-a-missing-reason": func(t *testing.T) {
			err := sqlitex.Execute(conn, `INSERT INTO ingest_run_outcomes(run_id, reason_id, created_at) VALUES(1,?,6)`, &sqlitex.ExecOptions{Args: []any{undefinedOutcomeReasonID}})
			if err == nil {
				t.Fatal("the foreign key admitted an outcome row for a reason with no row")
			}
		},
		"reader-refuses-an-id-this-build-does-not-define": func(t *testing.T) {
			if _, err := ingest.OutcomeReasonFromID(undefinedOutcomeReasonID); err == nil {
				t.Fatal("read boundary admitted an undefined id")
			}
			if _, err := ingest.OutcomeReasonFromID(0); err == nil {
				t.Fatal("read boundary admitted the zero id")
			}
			for _, row := range fixtures.Reasons {
				if reason, err := ingest.OutcomeReasonFromID(row.ID); err != nil || reason.Code() != row.Code {
					t.Fatalf("OutcomeReasonFromID(%d) = %s, %v", row.ID, reason.Code(), err)
				}
			}
		},
		"reader-skips-and-counts-a-store-local-reason-id": func(t *testing.T) {
			local := ingest.OutcomeReasonLocalIDBase
			if err := sqlitex.Execute(conn, `INSERT INTO ingest_outcome_reasons(id, kind_id, code, retired) VALUES(?, 2, 'code_no_build_lists', 1)`, &sqlitex.ExecOptions{Args: []any{local}}); err != nil {
				t.Fatal(err)
			}
			if err := sqlitex.Execute(conn, `INSERT INTO ingest_run_outcomes(run_id, reason_id, created_at) VALUES(1,?,7),(1,?,8)`, &sqlitex.ExecOptions{Args: []any{local, int64(ingest.OutcomeReasonMissingModel)}}); err != nil {
				t.Fatal(err)
			}
			s.pool.Put(conn)
			got, skipped, err := s.ListIngestRunOutcomesWithSkipped(t.Context(), 1)
			var takeErr error
			if conn, takeErr = s.pool.Take(t.Context()); takeErr != nil {
				t.Fatal(takeErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			if skipped != 1 || len(got) != 1 || got[0].Reason != ingest.OutcomeReasonMissingModel {
				t.Fatalf("read = %+v, skipped %d; want the one defined row and 1 skipped", got, skipped)
			}
		},
	}
	for _, c := range fixtures.Cases {
		run, ok := runners[c.Name]
		if !ok {
			t.Fatalf("ingest_outcome_reasons.yaml: case %q has no runner; add it to the runner", c.Name)
		}
		t.Run(c.Name, run)
	}
}
