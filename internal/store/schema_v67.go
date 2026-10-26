package store

// migrationV67 adds the latest-attempt pointer to sessions. A managed
// V2 write whose assessed capture is an unaccounted source-records omission
// preview does not replace the active authority; the preview is retained as a
// separate attempt and this pointer records it while the last-good full
// generation stays the read authority.
//
// The seven columns stay nullable with no backfill: a session that never
// retained an attempt keeps every column NULL, which is also the settled state
// the latest-attempt commit compares against. latest_attempt_stats_json carries
// the latest attempt's own producer stats so count-carrying reads can resolve
// them without touching the session's active-authority stats. Plain
// ALTER TABLE ADD COLUMN, no table rebuild, STRICT-compatible.
const migrationV67 = `
ALTER TABLE sessions ADD COLUMN latest_attempt_generation_id TEXT;
ALTER TABLE sessions ADD COLUMN latest_attempt_artifact_hash TEXT CHECK (
    latest_attempt_artifact_hash IS NULL OR (
        length(latest_attempt_artifact_hash) = 64 AND latest_attempt_artifact_hash NOT GLOB '*[^0-9a-f]*'
    )
);
ALTER TABLE sessions ADD COLUMN latest_attempt_indexer_version INTEGER CHECK (
    latest_attempt_indexer_version IS NULL OR latest_attempt_indexer_version > 0
);
ALTER TABLE sessions ADD COLUMN latest_attempt_failure_code TEXT;
ALTER TABLE sessions ADD COLUMN latest_attempt_failure_message TEXT;
ALTER TABLE sessions ADD COLUMN latest_attempt_captured_at_ms INTEGER CHECK (
    latest_attempt_captured_at_ms IS NULL OR latest_attempt_captured_at_ms >= 0
);
ALTER TABLE sessions ADD COLUMN latest_attempt_stats_json TEXT CHECK (
    latest_attempt_stats_json IS NULL OR json_valid(latest_attempt_stats_json)
);
`
