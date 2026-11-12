package store

// migrationV77 marks every stored session's two turn counts for one recount.
//
// The folded turn count returns to session_captured_stats.turn_count and the
// conversation turn count changes its rule, so every stored row has to be
// folded again. That fold is Go work, not SQL, so the migration only records
// that it is owed: a one-row marker in pending_recounts. Open runs the recount
// whenever the marker exists and the recount deletes the marker in the same
// transaction as its writes. A first open that fails or is killed therefore
// leaves the marker, and the next open runs the recount again. The schema
// version alone cannot say that: it reads 77 as soon as this statement
// commits, before any session is recounted.
//
// The table is keyed by name so a later recount can add its own row without a
// new table. No column is added to any existing table. A database with no
// sessions has nothing to recount, so it gets the table and no marker; that
// keeps a store created at head identical to the migrated chain.
const migrationV77 = `
CREATE TABLE pending_recounts (
    name TEXT PRIMARY KEY
);
INSERT INTO pending_recounts(name)
SELECT 'turn_counts' WHERE EXISTS (SELECT 1 FROM sessions);
`

// pendingRecountTurnCounts names the marker row the V77 migration writes and
// the turn count recount clears.
const pendingRecountTurnCounts = "turn_counts"
