package store

// migrationV48 stores the classifier annotation inputs that produced the last
// complete annotation pass for a session. The hash mirrors
// sessions.session_entries_hash and stays strict so incomplete or stale state
// fails safe.
const migrationV48 = `
CREATE TABLE annotation_run_state (
    session_id TEXT PRIMARY KEY,
    session_entries_hash TEXT NOT NULL CHECK (
        length(session_entries_hash) = 64 AND session_entries_hash NOT GLOB '*[^0-9a-f]*'
    ),
    compute_version INTEGER NOT NULL,
    classifier_version INTEGER NOT NULL,
    annotated_at INTEGER NOT NULL,
    FOREIGN KEY(session_id) REFERENCES sessions(session_id) ON DELETE CASCADE
);
`
