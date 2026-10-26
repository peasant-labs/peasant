package store

// Failed sessions need not exist in sessions: acquisition can fail before the
// first mirror. Only the parent audit run is a foreign key.
const migrationV64 = `
CREATE TABLE ingest_run_outcomes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL REFERENCES ingest_log(id) ON DELETE CASCADE,
    session_id TEXT,
    kind TEXT NOT NULL CHECK(kind IN ('worker_error', 'diagnostic')),
    reason_code TEXT NOT NULL CHECK(length(reason_code) > 0),
    created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_ingest_run_outcomes_run ON ingest_run_outcomes(run_id, id);
`
