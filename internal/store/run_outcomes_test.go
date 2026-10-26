package store

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/run_outcomes.yaml
var runOutcomeFixtureData []byte

//go:embed testdata/run_outcomes_hardening.yaml
var runOutcomeHardeningFixtureData []byte

type runOutcomeHardeningFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Reasons       []struct {
		Name   string `yaml:"name"`
		Reason string `yaml:"reason"`
		Valid  bool   `yaml:"valid"`
	} `yaml:"reasons"`
	BatchFailure struct {
		Name          string `yaml:"name"`
		Rows          int    `yaml:"rows"`
		FailAt        int    `yaml:"failAt"`
		CommittedRows int    `yaml:"committedRows"`
	} `yaml:"batchFailure"`
}

func LoadRunOutcomeHardeningFixtures(t *testing.T) runOutcomeHardeningFixture {
	t.Helper()
	var fixture runOutcomeHardeningFixture
	if err := yaml.Unmarshal(runOutcomeHardeningFixtureData, &fixture); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, row := range fixture.Reasons {
		names = append(names, row.Name)
	}
	if err := validateRecoveryRequiredNames(fixture.RequiredNames, names, "audit reasons"); err != nil {
		t.Fatal(err)
	}
	if fixture.BatchFailure.Name != "second-batch-storage-failure" || fixture.BatchFailure.FailAt <= ingestOutcomeBatchSize || fixture.BatchFailure.Rows < fixture.BatchFailure.FailAt {
		t.Fatal("missing second-batch failure fixture")
	}
	return fixture
}

func TestRunOutcomesReasonBoundaries(t *testing.T) {
	for _, row := range LoadRunOutcomeHardeningFixtures(t).Reasons {
		t.Run(row.Name, func(t *testing.T) {
			reason, err := ingest.NewAuditReason(row.Reason)
			if (err == nil) != row.Valid {
				t.Fatalf("constructor acceptance = %v, want %v", err == nil, row.Valid)
			}
			if row.Valid && string(reason) != row.Reason {
				t.Fatal("constructor changed spelling")
			}
			s, _ := openGenerationStore(t)
			outcomes := LoadRunOutcomeFixtures(t)
			// Deliberately bypass the constructor to exercise the store boundary.
			outcomes[0].ReasonCode = ingest.AuditReason(row.Reason)
			err = s.LogIngestRun(t.Context(), ingest.IngestLogEntry{StartedAt: 1, SessionsError: 1, Outcomes: outcomes})
			if (err == nil) != row.Valid {
				t.Fatalf("writer acceptance = %v, want %v", err == nil, row.Valid)
			}
			if !row.Valid && row.Reason != "" && strings.Contains(err.Error(), row.Reason) {
				t.Fatal("write rejection leaked untrusted reason")
			}
			conn, err := s.pool.Take(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			// Simulate old or externally corrupted data in the immutable schema.
			if err := sqlitex.ExecuteTransient(conn, `PRAGMA ignore_check_constraints=ON`, nil); err != nil {
				t.Fatal(err)
			}
			err = sqlitex.Execute(conn, `INSERT INTO ingest_run_outcomes(run_id, kind, reason_code, created_at) VALUES(1,'diagnostic',?,2)`, &sqlitex.ExecOptions{Args: []any{row.Reason}})
			if resetErr := sqlitex.ExecuteTransient(conn, `PRAGMA ignore_check_constraints=OFF`, nil); resetErr != nil {
				t.Fatal(resetErr)
			}
			s.pool.Put(conn)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.ListIngestRunOutcomes(t.Context(), 1)
			if (err == nil) != row.Valid {
				t.Fatalf("reader acceptance = %v, want %v", err == nil, row.Valid)
			}
			if !row.Valid && got != nil {
				t.Fatal("reader returned unvalidated details")
			}
			if !row.Valid && row.Reason != "" && strings.Contains(err.Error(), row.Reason) {
				t.Fatal("read rejection leaked untrusted reason")
			}
		})
	}
}

func TestRunOutcomesSecondBatchFailureAndRetention(t *testing.T) {
	fixture := LoadRunOutcomeHardeningFixtures(t).BatchFailure
	s, _ := openGenerationStore(t)
	base := LoadRunOutcomeFixtures(t)
	for range IngestOutcomeRetainedRuns {
		if err := s.LogIngestRun(t.Context(), ingest.IngestLogEntry{StartedAt: 1, Outcomes: base}); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := s.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = sqlitex.ExecuteScript(conn, `CREATE TRIGGER fail_detail BEFORE INSERT ON ingest_run_outcomes WHEN NEW.created_at = `+fmt.Sprint(fixture.FailAt)+` BEGIN SELECT RAISE(ABORT, 'injected detail write failure'); END;`, nil)
	s.pool.Put(conn)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := make([]ingest.RunOutcome, fixture.Rows)
	for i := range outcomes {
		outcomes[i] = base[0]
		outcomes[i].CreatedAt = int64(i + 1)
	}
	if err := s.LogIngestRun(t.Context(), ingest.IngestLogEntry{StartedAt: 2, SessionsError: fixture.Rows, Outcomes: outcomes}); err == nil {
		t.Fatal("injected second-batch failure did not fail")
	}
	runID := int64(IngestOutcomeRetainedRuns + 1)
	got, err := s.ListIngestRunOutcomes(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != fixture.CommittedRows {
		t.Fatalf("committed prefix = %d, want %d", len(got), fixture.CommittedRows)
	}
	for i, outcome := range got {
		if outcome.CreatedAt != int64(i+1) || outcome.RunID != runID {
			t.Fatal("partial batch committed or prefix changed")
		}
	}
	old, err := s.ListIngestRunOutcomes(t.Context(), 1)
	if err != nil || len(old) != 0 {
		t.Fatalf("failed write skipped retention: %v, %v", old, err)
	}
	conn, err = s.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	queryV62Row(t, conn, `SELECT sessions_error FROM ingest_log WHERE id=?`, []any{runID}, func(stmt *sqlite.Stmt) {
		if stmt.ColumnInt64(0) != int64(fixture.Rows) {
			t.Error("detail failure changed aggregate")
		}
	})
}

type runOutcomeFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Outcomes      []struct {
		Name      string `yaml:"name"`
		SessionID string `yaml:"sessionId"`
		Kind      string `yaml:"kind"`
		Reason    string `yaml:"reason"`
	} `yaml:"outcomes"`
}

func LoadRunOutcomeFixtures(t *testing.T) []ingest.RunOutcome {
	t.Helper()
	var fixture runOutcomeFixture
	if err := yaml.Unmarshal(runOutcomeFixtureData, &fixture); err != nil {
		t.Fatal(err)
	}
	var names []string
	var outcomes []ingest.RunOutcome
	for _, row := range fixture.Outcomes {
		names = append(names, row.Name)
		kind, err := ingest.NewRunOutcomeKind(row.Kind)
		if err != nil {
			t.Fatal(err)
		}
		var sid ingest.SessionID
		if row.SessionID != "" {
			sid, err = ingest.NewSessionID(row.SessionID)
			if err != nil {
				t.Fatal(err)
			}
		}
		reason, err := ingest.NewAuditReason(row.Reason)
		if err != nil {
			t.Fatal(err)
		}
		outcomes = append(outcomes, ingest.RunOutcome{SessionID: sid, Kind: kind, ReasonCode: reason, CreatedAt: 2})
	}
	if err := validateRecoveryRequiredNames(fixture.RequiredNames, names, "run outcomes"); err != nil {
		t.Fatal(err)
	}
	return outcomes
}

func TestMigrationV64RunOutcomes(t *testing.T) {
	t.Parallel()
	conn, err := sqlite.OpenConn(filepath.Join(t.TempDir(), "audit.db"), sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := sqlitemigration.Migrate(t.Context(), conn, frozenSchema(63)); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteScript(conn, `INSERT INTO ingest_log(started_at, sessions_error) VALUES(1,1);`, nil); err != nil {
		t.Fatal(err)
	}
	if err := sqlitemigration.Migrate(t.Context(), conn, dbSchema); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA foreign_keys=ON;`, nil); err != nil {
		t.Fatal(err)
	}
	outcomes := LoadRunOutcomeFixtures(t)
	if err := writeOutcomeBatch(conn, 1, outcomes); err != nil {
		t.Fatal(err)
	}
	if err := writeOutcomeBatch(conn, 999, outcomes); err == nil {
		t.Fatal("outcomes admitted without their parent audit run")
	}
	var errors int64
	queryV62Row(t, conn, `SELECT sessions_error FROM ingest_log WHERE id=1`, nil, func(stmt *sqlite.Stmt) { errors = stmt.ColumnInt64(0) })
	if errors != 1 {
		t.Fatal("migration changed the preexisting aggregate")
	}
	if err := sqlitex.ExecuteScript(conn, `DELETE FROM ingest_log WHERE id=1;`, nil); err != nil {
		t.Fatal(err)
	}
	queryV62Row(t, conn, `SELECT count(*) FROM ingest_run_outcomes`, nil, func(stmt *sqlite.Stmt) {
		if stmt.ColumnInt64(0) != 0 {
			t.Error("outcomes survived parent deletion")
		}
	})
}

func TestIngestRunOutcomesWriterAndRetention(t *testing.T) {
	s, _ := openGenerationStore(t)
	outcomes := LoadRunOutcomeFixtures(t)
	entry := ingest.IngestLogEntry{StartedAt: 1, SessionsError: 1, Outcomes: outcomes}
	if err := s.LogIngestRun(t.Context(), entry); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListIngestRunOutcomes(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := range outcomes {
		outcomes[i].RunID = 1
	}
	if !reflect.DeepEqual(got, outcomes) {
		t.Fatalf("read audit outcomes = %+v, want %+v", got, outcomes)
	}
	// The last retained boundary remains readable with populated runs, and
	// empty successful runs advance retention too.
	for range IngestOutcomeRetainedRuns - 1 {
		if err := s.LogIngestRun(t.Context(), entry); err != nil {
			t.Fatal(err)
		}
	}
	got, err = s.ListIngestRunOutcomes(t.Context(), 1)
	if err != nil || !reflect.DeepEqual(got, outcomes) {
		t.Fatalf("oldest retained run changed at the boundary: %+v, %v", got, err)
	}
	if err := s.LogIngestRun(t.Context(), ingest.IngestLogEntry{StartedAt: 3}); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListIngestRunOutcomes(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatal("old detailed outcomes were not pruned")
	}
	got, err = s.ListIngestRunOutcomes(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}
	for i := range outcomes {
		outcomes[i].RunID = 2
	}
	if !reflect.DeepEqual(got, outcomes) {
		t.Fatalf("retention pruned a retained run: %+v", got)
	}
	conn, err := s.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	queryV62Row(t, conn, `SELECT sessions_error FROM ingest_log WHERE id=1`, nil, func(stmt *sqlite.Stmt) {
		if stmt.ColumnInt64(0) != 1 {
			t.Error("retention removed aggregate history")
		}
	})
}

func TestIngestRunOutcomesFailurePreservesAggregate(t *testing.T) {
	s, _ := openGenerationStore(t)
	invalid := LoadRunOutcomeFixtures(t)
	invalid[0].ReasonCode = ""
	if err := s.LogIngestRun(t.Context(), ingest.IngestLogEntry{StartedAt: 1, SessionsError: 1, Outcomes: invalid}); err == nil {
		t.Fatal("missing reason admitted")
	}
	conn, err := s.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	queryV62Row(t, conn, `SELECT count(*) FROM ingest_log WHERE sessions_error=1`, nil, func(stmt *sqlite.Stmt) {
		if stmt.ColumnInt64(0) != 1 {
			t.Error("outcome failure lost aggregate audit")
		}
	})
	queryV62Row(t, conn, `SELECT count(*) FROM ingest_run_outcomes`, nil, func(stmt *sqlite.Stmt) {
		if stmt.ColumnInt64(0) != 0 {
			t.Error("invalid outcome batch partially committed")
		}
	})
}

func TestIngestRunOutcomesBatchedWrite(t *testing.T) {
	s, _ := openGenerationStore(t)
	fixture := LoadRunOutcomeFixtures(t)
	// Exercise the batch boundary and tail against the real writer. This is
	// a batching invariant, not a fixture deletion-protection count.
	want := make([]ingest.RunOutcome, 2*ingestOutcomeBatchSize+1)
	for i := range want {
		want[i] = fixture[i%len(fixture)]
		want[i].RunID = 1
	}
	if err := s.LogIngestRun(t.Context(), ingest.IngestLogEntry{StartedAt: 1, Outcomes: want}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListIngestRunOutcomes(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batched writer lost or changed an outcome: got %d, want %d", len(got), len(want))
	}
}
