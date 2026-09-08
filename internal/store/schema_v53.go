package store

// migrationV53 widens the two local harness CHECK mirrors to admit Pi. The
// ordinary rebuild preserves the complete current session schema and indices;
// disabling foreign keys during this transaction prevents cascading child loss.
const migrationV53 = `
CREATE TABLE sessions_v50 AS SELECT * FROM sessions;
DROP TABLE sessions;

CREATE TABLE sessions (
    session_id TEXT PRIMARY KEY,
    parent_id TEXT REFERENCES sessions(session_id),
    model_harness TEXT NOT NULL CHECK (model_harness IN ('claude-code','opencode','codex','gemini-cli','cursor','antigravity','strike','pi')),
    model_id TEXT NOT NULL,
    opaque_host_id TEXT NOT NULL REFERENCES host_slugs(opaque_id),
    project_hash TEXT NOT NULL REFERENCES projects(project_hash),
    start_ms INTEGER NOT NULL,
    end_ms INTEGER NOT NULL,
    ingested_ms INTEGER NOT NULL,
    source_path TEXT NOT NULL,
    source_format TEXT NOT NULL CHECK (source_format IN ('jsonl','json')),
    schema_version INTEGER NOT NULL DEFAULT 1,
    git_branch TEXT,
    git_worktree TEXT,
    git_tracking TEXT,
    tool_version TEXT,
    pushed_at INTEGER,
    tags TEXT,
    index_version INTEGER NOT NULL DEFAULT 0,
    indexed_at INTEGER,
    license_id TEXT CHECK (license_id IN ('CC0-1.0', 'CC-BY-4.0', 'CC-BY-SA-4.0')),
    session_origin TEXT NOT NULL DEFAULT 'unknown' CHECK (session_origin IN ('user','agent','unknown')),
    origin_version INTEGER NOT NULL DEFAULT 0,
    session_entries_hash TEXT CHECK (session_entries_hash IS NULL OR (length(session_entries_hash) = 64 AND session_entries_hash NOT GLOB '*[^0-9a-f]*')),
    source_fingerprint BLOB,
    session_cwd TEXT,
    cwd_provenance_kind TEXT NOT NULL DEFAULT 'not_recovered' CHECK(cwd_provenance_kind IN ('source_exact','source_workspace','source_worktree','source_absent','not_recovered')),
    publication_capture_revision INTEGER NOT NULL DEFAULT 0 CHECK(publication_capture_revision >= 0),
    indexed_publication_capture_revision INTEGER NOT NULL DEFAULT 0 CHECK(indexed_publication_capture_revision >= 0)
) STRICT;

INSERT INTO sessions SELECT * FROM sessions_v50;
DROP TABLE sessions_v50;

CREATE INDEX idx_sessions_start ON sessions(start_ms);
CREATE INDEX idx_sessions_harness ON sessions(model_harness);
CREATE INDEX idx_sessions_project ON sessions(project_hash);
CREATE INDEX idx_sessions_host ON sessions(opaque_host_id);
CREATE INDEX idx_sessions_parent ON sessions(parent_id) WHERE parent_id IS NOT NULL;

CREATE TRIGGER sessions_publication_metadata_changed
AFTER UPDATE OF parent_id,model_harness,model_id,opaque_host_id,project_hash,
 start_ms,end_ms,ingested_ms,source_path,source_format,schema_version,
 git_branch,git_worktree,git_tracking,tool_version,session_origin ON sessions
BEGIN
 UPDATE sessions SET indexed_publication_capture_revision=0, cwd_provenance_kind='not_recovered' WHERE session_id=NEW.session_id;
END;

CREATE TABLE daily_summary_harness_v50 AS SELECT * FROM daily_summary_harness;
DROP TABLE daily_summary_harness;
CREATE TABLE daily_summary_harness (
    date_utc TEXT NOT NULL,
    model_harness TEXT NOT NULL CHECK (model_harness IN ('claude-code','opencode','codex','gemini-cli','cursor','antigravity','strike','pi')),
    session_count INTEGER NOT NULL DEFAULT 0,
    tokens_in INTEGER NOT NULL DEFAULT 0,
    tokens_out INTEGER NOT NULL DEFAULT 0,
    tokens_total INTEGER NOT NULL DEFAULT 0,
    avg_duration_ms REAL NOT NULL DEFAULT 0,
    avg_turns REAL NOT NULL DEFAULT 0,
    tool_call_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (date_utc, model_harness)
) STRICT, WITHOUT ROWID;

INSERT INTO daily_summary_harness SELECT * FROM daily_summary_harness_v50;
DROP TABLE daily_summary_harness_v50;
`
