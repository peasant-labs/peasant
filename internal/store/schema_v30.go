package store

// migrationV30 creates the memory_injection_log table for tracking when
// lesson injection was enabled/disabled in eval projects. Used by
// `memory eval` to split sessions by injection status instead of date.
const migrationV30 = `
CREATE TABLE memory_injection_log (
    id           TEXT PRIMARY KEY,
    project_path TEXT NOT NULL,
    event        TEXT NOT NULL CHECK (event IN ('on', 'off')),
    created_at   INTEGER NOT NULL
) STRICT;

CREATE INDEX idx_injection_log_project ON memory_injection_log(project_path, created_at);
`
