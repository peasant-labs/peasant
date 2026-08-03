package store

// migrationV42 rebuilds the two tables that mirror the closed harness set.
// SQLite cannot alter a CHECK constraint in place, so the migration stages all
// current rows, recreates the exact current schemas with Strike admitted, then
// restores the rows and session indexes.
const migrationV42 = `
CREATE TABLE sessions_v42 AS SELECT * FROM sessions;
DROP TABLE sessions;

CREATE TABLE sessions (
    session_id     TEXT PRIMARY KEY,
    parent_id      TEXT REFERENCES sessions(session_id),
    model_harness  TEXT NOT NULL CHECK (model_harness IN ('claude-code','opencode','codex','gemini-cli','cursor','antigravity','strike')),
    model_id       TEXT NOT NULL,
    opaque_host_id TEXT NOT NULL REFERENCES host_slugs(opaque_id),
    project_hash   TEXT NOT NULL REFERENCES projects(project_hash),
    start_ms       INTEGER NOT NULL,
    end_ms         INTEGER NOT NULL,
    ingested_ms    INTEGER NOT NULL,
    source_path    TEXT NOT NULL,
    source_format  TEXT NOT NULL CHECK (source_format IN ('jsonl','json')),
    schema_version INTEGER NOT NULL DEFAULT 1,
    git_branch     TEXT,
    git_worktree   TEXT,
    git_tracking   TEXT,
    tool_version   TEXT,
    pushed_at      INTEGER,
    tags           TEXT,
    index_version  INTEGER NOT NULL DEFAULT 0,
    indexed_at     INTEGER,
    license_id     TEXT CHECK (license_id IN ('CC0-1.0', 'CC-BY-4.0', 'CC-BY-SA-4.0'))
) STRICT;

INSERT INTO sessions SELECT * FROM sessions_v42;
DROP TABLE sessions_v42;

CREATE INDEX idx_sessions_start   ON sessions(start_ms);
CREATE INDEX idx_sessions_harness ON sessions(model_harness);
CREATE INDEX idx_sessions_project ON sessions(project_hash);
CREATE INDEX idx_sessions_host    ON sessions(opaque_host_id);
CREATE INDEX idx_sessions_parent  ON sessions(parent_id) WHERE parent_id IS NOT NULL;

CREATE TABLE daily_summary_harness_v42 AS SELECT * FROM daily_summary_harness;
DROP TABLE daily_summary_harness;

CREATE TABLE daily_summary_harness (
    date_utc        TEXT    NOT NULL,
    model_harness   TEXT    NOT NULL CHECK (model_harness IN ('claude-code','opencode','codex','gemini-cli','cursor','antigravity','strike')),
    session_count   INTEGER NOT NULL DEFAULT 0,
    tokens_in       INTEGER NOT NULL DEFAULT 0,
    tokens_out      INTEGER NOT NULL DEFAULT 0,
    tokens_total    INTEGER NOT NULL DEFAULT 0,
    avg_duration_ms REAL    NOT NULL DEFAULT 0,
    avg_turns       REAL    NOT NULL DEFAULT 0,
    tool_call_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (date_utc, model_harness)
) STRICT, WITHOUT ROWID;

INSERT INTO daily_summary_harness SELECT * FROM daily_summary_harness_v42;
DROP TABLE daily_summary_harness_v42;
`
