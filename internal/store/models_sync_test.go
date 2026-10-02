package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestSyncModelsRollsBackMidBatchFailure pins the all-or-nothing contract of
// SyncModels: when one statement in the batch fails, no earlier upsert may
// survive. A BEFORE INSERT trigger aborts on one named model, so the failure is
// injected inside the real prepared-statement loop rather than mocked, and the
// assertion is on the persisted rows after the call returns.
func TestSyncModelsRollsBackMidBatchFailure(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// Reject exactly one model in the middle of the batch. The trigger is
	// committed before SyncModels runs, so it survives the batch rollback.
	conn := takeConn(t, s.PoolForTest())
	err := sqlitex.ExecuteTransient(conn, `CREATE TRIGGER reject_boom BEFORE INSERT ON models
		WHEN NEW.model_id = 'boom'
		BEGIN
			SELECT RAISE(ABORT, 'injected mid-batch failure');
		END`, nil)
	s.PoolForTest().Put(conn)
	if err != nil {
		t.Fatalf("create failure-injection trigger: %v", err)
	}

	models := []ingest.ModelInfo{
		{ModelID: "first", ProviderKey: "p", DisplayName: "First", LastSynced: "t"},
		{ModelID: "boom", ProviderKey: "p", DisplayName: "Boom", LastSynced: "t"},
		{ModelID: "third", ProviderKey: "p", DisplayName: "Third", LastSynced: "t"},
	}
	err = s.SyncModels(ctx, models)
	if err == nil {
		t.Fatal("SyncModels succeeded despite an injected statement failure; want an error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("SyncModels error %q does not name the failing model", err)
	}

	// The batch is one logical registry sync: nothing may persist.
	conn = takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	var count int
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM models`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { count = stmt.ColumnInt(0); return nil },
	}); err != nil {
		t.Fatalf("count models after failure: %v", err)
	}
	if count != 0 {
		t.Fatalf("SyncModels left %d rows after a mid-batch failure; want the batch rolled back to 0", count)
	}
}
