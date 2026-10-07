package store

import (
	"context"

	"github.com/peasant-labs/schema"
)

// MigratePlan is the peasant migrate Phase 0 preflight report (design §7.2):
// the sessions with conversion work plus the drain counts (pending intents
// and superseded generations to discard). Read-only; --dry-run stops here.
type MigratePlan struct {
	Sessions   []schema.SessionID
	Intents    int
	Superseded int
}

// MigrateResult is the peasant migrate Phase 4 final report: per-session
// dispositions accumulated across the run, plus the freed bytes.
type MigrateResult struct {
	Converted  int64
	RolledBack int64
	Marked     int64
	Skipped    int64
	BytesFreed int64
}

// PlanMigration computes the Phase 0 preflight: counts per phase, pending
// intents and superseded generations to discard, field/blob mismatches
// (sampled), and the free-disk and no-other-writer advisories. Stub: returns
// ErrHarmonizedNotImplemented until the migration slice lands it.
func PlanMigration(ctx context.Context) (MigratePlan, error) {
	return MigratePlan{}, ErrHarmonizedNotImplemented
}

// MigrateSession converts one file-backed native session under its exclusive
// lock (design §7.2 Phase 2): legacy-oracle read, structured row writes,
// stats backfill, catalog transaction with the in-transaction shadow verify,
// old-row deletes, directory removal, sweep, and flag clear. Resumable: the
// per-session DB state is the progress record. Stub: returns
// ErrHarmonizedNotImplemented until the migration slice lands it.
func MigrateSession(ctx context.Context, sessionID schema.SessionID) (MigrateOutcome, error) {
	return "", ErrHarmonizedNotImplemented
}
