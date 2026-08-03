package store

// migrationV33 updates the model_harness CHECK constraints and existing data
// to use bestiary.Harness identifiers:
//   - "claude"  → "claude-code"
//   - "gemini"  → "gemini-cli"
//   - "codex" and "opencode" values remain unchanged.
//
// SQLite does not support ALTER TABLE ... ALTER CHECK, so we recreate the
// sessions and daily_summary_harness tables with the updated constraints.
//
// New harnesses "cursor" and "antigravity" are also added to the CHECK set.
const migrationV33 = `
-- Step 1: Copy sessions into a constraint-free staging table, then apply the
-- harness rename THERE. The live sessions table still carries the OLD CHECK
-- (model_harness IN ('claude',...)), so updating it in place to 'claude-code'
-- would violate that constraint. A CREATE TABLE ... AS SELECT copy does not
-- inherit CHECK constraints, so the rename is safe on the staging table.
-- Column list: 20 columns (matches schema after V23).
CREATE TABLE sessions_v33 AS SELECT * FROM sessions;
UPDATE sessions_v33 SET model_harness = 'claude-code' WHERE model_harness = 'claude';
UPDATE sessions_v33 SET model_harness = 'gemini-cli'  WHERE model_harness = 'gemini';
DROP TABLE sessions;

-- Step 2: Recreate sessions table with updated CHECK constraint.
CREATE TABLE sessions (
    session_id     TEXT PRIMARY KEY,
    parent_id      TEXT REFERENCES sessions(session_id),
    model_harness  TEXT NOT NULL CHECK (model_harness IN ('claude-code','opencode','codex','gemini-cli','cursor','antigravity')),
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
    indexed_at     INTEGER
) STRICT;

INSERT INTO sessions SELECT * FROM sessions_v33;
DROP TABLE sessions_v33;

CREATE INDEX IF NOT EXISTS idx_sessions_start    ON sessions(start_ms);
CREATE INDEX IF NOT EXISTS idx_sessions_harness  ON sessions(model_harness);
CREATE INDEX IF NOT EXISTS idx_sessions_project  ON sessions(project_hash);
CREATE INDEX IF NOT EXISTS idx_sessions_host     ON sessions(opaque_host_id);
CREATE INDEX IF NOT EXISTS idx_sessions_parent   ON sessions(parent_id) WHERE parent_id IS NOT NULL;

-- Step 3: Same staging-table approach for daily_summary_harness — its live
-- CHECK is also the OLD harness set, so rename on the constraint-free copy.
-- Column list: 9 columns (matches schema after V23).
CREATE TABLE daily_summary_harness_v33 AS SELECT * FROM daily_summary_harness;
UPDATE daily_summary_harness_v33 SET model_harness = 'claude-code' WHERE model_harness = 'claude';
UPDATE daily_summary_harness_v33 SET model_harness = 'gemini-cli'  WHERE model_harness = 'gemini';
DROP TABLE daily_summary_harness;

-- Step 4: Recreate daily_summary_harness with updated CHECK.
CREATE TABLE daily_summary_harness (
    date_utc        TEXT    NOT NULL,
    model_harness   TEXT    NOT NULL CHECK (model_harness IN ('claude-code','opencode','codex','gemini-cli','cursor','antigravity')),
    session_count   INTEGER NOT NULL DEFAULT 0,
    tokens_in       INTEGER NOT NULL DEFAULT 0,
    tokens_out      INTEGER NOT NULL DEFAULT 0,
    tokens_total    INTEGER NOT NULL DEFAULT 0,
    avg_duration_ms REAL    NOT NULL DEFAULT 0,
    avg_turns       REAL    NOT NULL DEFAULT 0,
    tool_call_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (date_utc, model_harness)
) STRICT, WITHOUT ROWID;

INSERT INTO daily_summary_harness SELECT * FROM daily_summary_harness_v33;
DROP TABLE daily_summary_harness_v33;
`
