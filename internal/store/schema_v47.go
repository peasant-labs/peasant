package store

// migrationV47 stores the digest of the parsed session_entries projection.
// The value is nullable so existing sessions fail safe until a successful index
// writes the hash. A missing hash means the fast skip cannot be trusted.
const migrationV47 = `
ALTER TABLE sessions ADD COLUMN session_entries_hash TEXT CHECK (
    session_entries_hash IS NULL OR (
        length(session_entries_hash) = 64 AND session_entries_hash NOT GLOB '*[^0-9a-f]*'
    )
);
`
