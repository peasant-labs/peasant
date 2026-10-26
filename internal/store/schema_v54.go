package store

// completedIndexSQL is the SQL form of ingest.SessionIndexState.Completed: a
// positive producer revision AND a recorded completion time. That method is the
// one definition every Go caller uses; this constant is its only SQL copy, so a
// migration backfill and any later query cannot state the rule differently. A
// format stamp is not a substitute for either half.
const completedIndexSQL = "(index_version > 0 AND indexed_at IS NOT NULL)"

// migrationV54 records the actual index representation separately from the
// historic index_version columns, which continue to name the producing parser.
// Existing canonical rows prove V1. An empty successful index needs both its
// producer revision and completion time; other absent evidence stays unknown.
const migrationV54 = `
ALTER TABLE sessions ADD COLUMN index_format_version INTEGER CHECK (index_format_version > 0);
ALTER TABLE index_log ADD COLUMN index_format_version INTEGER CHECK (index_format_version > 0);

UPDATE sessions SET index_format_version = 1
WHERE EXISTS (SELECT 1 FROM session_entries WHERE session_entries.session_id = sessions.session_id)
   OR ` + completedIndexSQL + `;
`
