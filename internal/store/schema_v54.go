package store

// migrationV54 records the actual index representation separately from the
// historic index_version columns, which continue to name the producing parser.
// Existing canonical rows prove V1. An empty successful index needs both its
// producer revision and completion time; other absent evidence stays unknown.
const migrationV54 = `
ALTER TABLE sessions ADD COLUMN index_format_version INTEGER CHECK (index_format_version > 0);
ALTER TABLE index_log ADD COLUMN index_format_version INTEGER CHECK (index_format_version > 0);

UPDATE sessions SET index_format_version = 1
WHERE EXISTS (SELECT 1 FROM session_entries WHERE session_entries.session_id = sessions.session_id)
   OR (index_version > 0 AND indexed_at IS NOT NULL);
`
