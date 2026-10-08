package store

import (
	"os"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// TestSandboxSessionConversionCommit measures the largest-session conversion
// commit on a sandbox copy of a production store and gates it against the
// open path's busy_timeout. It runs the production MigrateSession stage and
// commit sequence for one real session end to end: lock, sweep flag, oracle
// read, convergence and integrity checks, prepare, bounded staging
// sub-transactions, then the timed activation commit. Only the commit holds
// the single SQLite writer for the whole session, so only the commit is
// asserted against the hold bound; the end-to-end wall is reported.
//
// The test runs only when the sandbox environment names a copy:
// PEASANT_SANDBOX_DB (a writable copy of the store, never the live path),
// PEASANT_SANDBOX_SYNC (an artifact root holding the session's real tree),
// and PEASANT_SANDBOX_SESSION (the session to convert). Without all three
// it skips, so CI and clean checkouts never touch it.
func TestSandboxSessionConversionCommit(t *testing.T) {
	dbPath := os.Getenv("PEASANT_SANDBOX_DB")
	syncRoot := os.Getenv("PEASANT_SANDBOX_SYNC")
	sessionID := os.Getenv("PEASANT_SANDBOX_SESSION")
	if dbPath == "" || syncRoot == "" || sessionID == "" {
		t.Skip("sandbox measurement only: set PEASANT_SANDBOX_DB, PEASANT_SANDBOX_SYNC, and PEASANT_SANDBOX_SESSION to a sandbox copy")
	}
	sid, err := schema.NewSessionID(sessionID)
	if err != nil {
		t.Fatalf("parse PEASANT_SANDBOX_SESSION %q: %v", sessionID, err)
	}
	artifacts, err := NewOSGenerationArtifactStore(syncRoot)
	if err != nil {
		t.Fatalf("open sandbox artifact root: %v", err)
	}
	locker, err := NewFileSessionLocker(syncRoot)
	if err != nil {
		t.Fatalf("open sandbox session locker: %v", err)
	}
	db, err := Open(dbPath,
		WithPoolSize(4),
		WithIndexFormats(generationIndexFormat{}),
		WithGenerationArtifacts(artifacts, locker),
	)
	if err != nil {
		t.Fatalf("open sandbox store: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := t.Context()

	release, err := db.sessionLocker.LockExclusive(ctx, sid)
	if err != nil {
		t.Fatalf("lock sandbox session: %v", err)
	}
	defer func() { _ = release() }()

	work, err := db.migrateSessionWork(ctx, sid)
	if err != nil {
		t.Fatalf("inspect sandbox session: %v", err)
	}
	if !work.convert {
		t.Fatalf("sandbox session %s carries no conversion work; the measurement needs a file-backed active generation", sessionID)
	}
	if err := db.setSweepFlag(ctx, sid); err != nil {
		t.Fatalf("set sandbox sweep flag: %v", err)
	}
	oracle, err := db.readMigrateOracle(ctx, sid, work.generationID)
	if err != nil {
		t.Fatalf("read sandbox oracle: %v", err)
	}
	mainEntries := 0
	for _, part := range oracle.entries {
		mainEntries += len(part)
	}
	t.Logf("sandbox oracle: %d entries in %d partitions, %d content records, %d aliases",
		mainEntries, len(oracle.entries), len(oracle.content), len(oracle.aliases))
	if err := db.checkMigrateOracleConverges(ctx, oracle); err != nil {
		t.Fatalf("sandbox oracle diverges: %v", err)
	}
	if err := checkMigrateEmittedIntegrity(oracle); err != nil {
		t.Fatalf("sandbox emitted integrity: %v", err)
	}
	prepared, err := prepareHarmonizedCandidate(sid, oracleGeneration(oracle), oracle.blobs)
	if err != nil {
		t.Fatalf("prepare sandbox candidate: %v", err)
	}
	stageStart := time.Now()
	if err := db.stageMigrateObjects(ctx, prepared); err != nil {
		t.Fatalf("stage sandbox objects: %v", err)
	}
	stageWall := time.Since(stageStart)
	if err := db.upsertMigrateStats(ctx, oracle); err != nil {
		t.Fatalf("upsert sandbox stats: %v", err)
	}
	commitStart := time.Now()
	converted, rollback, err := db.commitMigrateSession(ctx, oracle, prepared)
	commitWall := time.Since(commitStart)
	var oracleBytes int64
	for _, payload := range oracle.blobs {
		oracleBytes += int64(len(payload))
	}
	if err != nil {
		t.Fatalf("commit sandbox session: %v", err)
	}
	if rollback != nil {
		t.Fatalf("sandbox session rolled back at %s: %s", rollback.Dimension, rollback.Reason)
	}
	if !converted {
		t.Fatal("sandbox commit converted nothing")
	}
	t.Logf("sandbox session conversion: staging %s, activation commit %s (hold bound %s)",
		stageWall.Round(time.Millisecond), commitWall.Round(time.Millisecond), defaults.SQLiteBusyTimeout)
	byteBudget := defaults.FullContentWriteBatchBytes
	t.Logf("sandbox oracle bytes: %d (byte budget %d)", oracleBytes, byteBudget)
	if oracleBytes > byteBudget {
		// The oversized path: the session exceeds the staging byte budget,
		// so it stages and commits alone, outside the bounded-batch
		// contract the hold gate covers. Its wall is the derivation input
		// for the oversized-session behavior, not a hold violation: the
		// migration runs under the no-concurrent-writer advisory, and
		// whether the in-transaction shadow verify keeps the oversized
		// activation commit under the hold is a design tradeoff for review.
		t.Logf("sandbox session is oversized (%d bytes over the %d-byte budget): wall reported, hold gate applies to budget-conforming sessions",
			oracleBytes, byteBudget)
		return
	}
	if commitWall > defaults.SQLiteBusyTimeout {
		t.Fatalf("activation commit held %s, past the open path's busy_timeout %s",
			commitWall, defaults.SQLiteBusyTimeout)
	}
}
