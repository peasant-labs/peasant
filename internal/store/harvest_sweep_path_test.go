package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// TestHarvestSweepPathSetsAndClearsFlag runs the pipeline's exact store
// calls against a real store and proves the crash-recovery contract end to
// end: staging sets the sweep flag, the commit leaves it set (the store
// never sweeps; the harvest does), the after-commit sweep clears it with
// the superseded rows, and the harvest-start sweep clears staged crash
// leftovers with no commit at all.
func TestHarvestSweepPathSetsAndClearsFlag(t *testing.T) {
	dbPath := storetest.CopyGoldenDB(t)
	db := openWriteBudgetHarmonized(t, dbPath)
	ctx := context.Background()
	sid, err := schema.NewSessionID("99999999-9999-4999-8999-999999999999")
	if err != nil {
		t.Fatal(err)
	}
	storetest.SeedSession(t, db, string(sid))

	v2, blobs := writeBudgetV2(t, sid, "gen_harvest_sweep_1", []string{"harvest sweep path one", "harvest sweep path two"})
	native := ingest.NativeGenerationActivation{Generation: v2, Blobs: blobs, IndexerVersion: 1, IndexedAtMs: 1}

	// The pipeline stages through its stager entry point: the crash flag
	// is set before the first object write.
	staged, err := db.StageNativeGeneration(ctx, native)
	if err != nil {
		t.Fatalf("stage through the harvest entry point: %v", err)
	}
	if got := harvestSweepFlag(t, dbPath, sid); got != 1 {
		t.Fatalf("sweep flag after staging = %d, want set", got)
	}

	// The pipeline activates through its prepared-activator entry point:
	// the commit is durable and the flag stays set, because the sweep is
	// the harvest's step, not the store's.
	outcome, err := db.ActivateStagedNativeGeneration(ctx, native, staged)
	if err != nil {
		t.Fatalf("activate through the harvest entry point: %v", err)
	}
	if outcome.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("activation disposition = %v, want CommittedNow", outcome.Disposition)
	}
	if got := harvestSweepFlag(t, dbPath, sid); got != 1 {
		t.Fatalf("sweep flag after commit = %d, want still set: only the harvest sweep clears it", got)
	}

	// The pipeline sweeps through its after-commit entry point: the flag
	// clears with the session clean.
	if err := db.SweepSessionForHarvest(ctx, sid); err != nil {
		t.Fatalf("after-commit sweep through the harvest entry point: %v", err)
	}
	if got := harvestSweepFlag(t, dbPath, sid); got != 0 {
		t.Fatalf("sweep flag after the harvest sweep = %d, want cleared", got)
	}
	if got := harvestSweepCount(t, dbPath, `SELECT COUNT(*) FROM session_entry_bodies WHERE session_id = ?`, sid); got != 2 {
		t.Fatalf("bodies after the harvest sweep = %d, want the committed 2", got)
	}

	// A crash between staging and commit leaves staged objects with the
	// flag set and no new generation: the harvest-start entry point
	// recovers them with no commit at all.
	v2b, blobbs := writeBudgetV2(t, sid, "gen_harvest_sweep_2", []string{"harvest sweep path three", "harvest sweep path four"})
	if _, err := db.StageNativeGeneration(ctx, ingest.NativeGenerationActivation{Generation: v2b, Blobs: blobbs, IndexerVersion: 2, IndexedAtMs: 2}); err != nil {
		t.Fatalf("stage the crashed candidate: %v", err)
	}
	if got := harvestSweepCount(t, dbPath, `SELECT COUNT(*) FROM session_entry_bodies WHERE session_id = ?`, sid); got != 4 {
		t.Fatalf("bodies with staged orphans = %d, want 2 committed + 2 staged", got)
	}
	swept, warnings, err := db.SweepFlaggedSessionsForHarvest(ctx)
	if err != nil {
		t.Fatalf("harvest-start sweep through the harvest entry point: %v", err)
	}
	if swept != 1 || len(warnings) != 0 {
		t.Fatalf("harvest-start sweep = %d sessions, warnings %v, want the 1 crashed session cleanly", swept, warnings)
	}
	if got := harvestSweepFlag(t, dbPath, sid); got != 0 {
		t.Fatalf("sweep flag after the harvest-start sweep = %d, want cleared", got)
	}
	if got := harvestSweepCount(t, dbPath, `SELECT COUNT(*) FROM session_entry_bodies WHERE session_id = ?`, sid); got != 2 {
		t.Fatalf("bodies after the harvest-start sweep = %d, want the committed 2", got)
	}
	if got := harvestSweepCount(t, dbPath, `SELECT COUNT(*) FROM session_generations WHERE session_id = ?`, sid); got != 1 {
		t.Fatalf("generations after recovery = %d, want the 1 committed", got)
	}
}

// harvestSweepFlag reads one session's crash flag through a direct
// read-only connection: the store under test stays open, as in production.
func harvestSweepFlag(t *testing.T, dbPath string, sid schema.SessionID) int64 {
	t.Helper()
	return harvestSweepCount(t, dbPath, `SELECT content_sweep_pending FROM sessions WHERE session_id = ?`, sid)
}

// harvestSweepCount runs one integer query against the harvest test's
// database file.
func harvestSweepCount(t *testing.T, dbPath, query string, sid schema.SessionID) int64 {
	t.Helper()
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatalf("open harvest database: %v", err)
	}
	defer conn.Close()
	var got int64
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			got = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return got
}
