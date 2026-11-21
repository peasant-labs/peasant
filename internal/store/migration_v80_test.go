package store

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

func TestMigrationV80AddsCaptureOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	buildBehindStore(t, path, 79, func(conn *sqlite.Conn) {
		if err := sqlitex.ExecuteScript(conn, `
INSERT INTO ingest_log(id, started_at) VALUES (1, 10);
INSERT INTO ingest_run_outcomes(id, run_id, session_id, reason_id, created_at)
VALUES (1, 1, '10000000-0000-4000-8000-000000000001', 301, 11);`, nil); err != nil {
			t.Fatalf("seed the pre-upgrade audit: %v", err)
		}
	})
	plan := mustPlan(t, path)
	if plan.From != 79 || plan.To != 82 {
		t.Fatalf("upgrade plan = %+v; want an upgrade from 79 to 82", plan)
	}
	want := []UpgradeStep{schemaStep(80), emptySourceDiagnosticStep(), schemaStep(mappingSetSchemaVersion)}
	if !slices.Equal(plan.Steps, want) {
		t.Fatalf("upgrade plan = %+v; want the outcome step, the empty-ledger rebuild and the mapping-set column", plan)
	}
	if _, err := Upgrade(t.Context(), path, UpgradeOptions{}); err != nil {
		t.Fatalf("upgrade the existing store: %v", err)
	}
	db, err := Open(path, WithPoolSize(1))
	if err != nil {
		t.Fatalf("open the upgraded store: %v", err)
	}
	defer func() { _ = db.Close() }()
	conn, err := db.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO ingest_run_outcomes(run_id, session_id, reason_id, created_at) VALUES (1, ?, ?, 12)`,
		&sqlitex.ExecOptions{Args: []any{"10000000-0000-4000-8000-000000000002", int64(ingest.OutcomeReasonRefreshPublicationCaptureMissing)}}); err != nil {
		db.pool.Put(conn)
		t.Fatalf("insert outcome against the migrated foreign key: %v", err)
	}
	db.pool.Put(conn)
	rows, err := db.ListIngestRunOutcomes(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Reason != ingest.OutcomeReasonRefreshSourceChanged || rows[1].Reason != ingest.OutcomeReasonRefreshPublicationCaptureMissing {
		t.Fatalf("outcomes after upgrade = %+v; want the old row retained and the new reason readable", rows)
	}
}
