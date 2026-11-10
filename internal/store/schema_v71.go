package store

// migrationV71 widens the stored harness set without changing existing rows.
// The CREATE bodies are frozen slot-70 sqlite_master text; do not hand-edit them.
// Recreate session triggers after copying rows so the copy does not advance the revision.
const migrationV71 = `
CREATE TABLE sessions_v71 AS SELECT * FROM sessions;
DROP TABLE sessions;

CREATE TABLE sessions (
    session_id TEXT PRIMARY KEY,
    parent_id TEXT REFERENCES sessions(session_id),
    model_harness TEXT NOT NULL CHECK (model_harness IN ('claude-code','opencode','codex','gemini-cli','cursor','antigravity','strike','pi','grok-build','devin-desktop','cline','kiro','kiro-cli','kiro-crew')),
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
, index_format_version INTEGER CHECK (index_format_version > 0), adapter_version INTEGER CHECK (adapter_version > 0), artifact_hash TEXT CHECK (artifact_hash IS NULL OR (length(artifact_hash) = 64 AND artifact_hash NOT GLOB '*[^0-9a-f]*')), metric_seed_json TEXT CHECK (metric_seed_json IS NULL OR json_valid(metric_seed_json)), indexed_input_hash TEXT CHECK (
    indexed_input_hash IS NULL OR (
        length(indexed_input_hash) = 64 AND indexed_input_hash NOT GLOB '*[^0-9a-f]*'
    )
), active_generation_id TEXT, root_session_id TEXT, session_purpose TEXT, input_submission_count INTEGER CHECK(input_submission_count IS NULL OR (input_submission_count BETWEEN 0 AND 9007199254740991)), content_sweep_pending INTEGER NOT NULL DEFAULT 0 CHECK(content_sweep_pending IN (0,1)), thinking_level TEXT
  CHECK (thinking_level IS NULL OR thinking_level IN
    ('off','minimal','low','medium','high','xhigh','max','ultra','ultracode')), thinking_level_raw TEXT, thinking_level_history TEXT, latest_attempt_generation_id TEXT, latest_attempt_artifact_hash TEXT CHECK (
    latest_attempt_artifact_hash IS NULL OR (
        length(latest_attempt_artifact_hash) = 64 AND latest_attempt_artifact_hash NOT GLOB '*[^0-9a-f]*'
    )
), latest_attempt_indexer_version INTEGER CHECK (
    latest_attempt_indexer_version IS NULL OR latest_attempt_indexer_version > 0
), latest_attempt_failure_code TEXT, latest_attempt_failure_message TEXT, latest_attempt_captured_at_ms INTEGER CHECK (
    latest_attempt_captured_at_ms IS NULL OR latest_attempt_captured_at_ms >= 0
), latest_attempt_stats_json TEXT CHECK (
    latest_attempt_stats_json IS NULL OR json_valid(latest_attempt_stats_json)
), parent_state TEXT NOT NULL DEFAULT 'root'
  CHECK (parent_state IN ('root','resolved','unresolved')), resolved_parent_id TEXT
  CHECK ((parent_state = 'resolved') = (resolved_parent_id IS NOT NULL))) STRICT;

INSERT INTO sessions SELECT * FROM sessions_v71;
DROP TABLE sessions_v71;

CREATE INDEX idx_sessions_child_page
  ON sessions(resolved_parent_id, start_ms DESC, session_id ASC)
  WHERE parent_state = 'resolved';

CREATE INDEX idx_sessions_harness ON sessions(model_harness);

CREATE INDEX idx_sessions_host ON sessions(opaque_host_id);

CREATE INDEX idx_sessions_parent ON sessions(parent_id) WHERE parent_id IS NOT NULL;

CREATE INDEX idx_sessions_project ON sessions(project_hash);

CREATE INDEX idx_sessions_root_page
  ON sessions(start_ms DESC, session_id ASC)
  WHERE parent_state = 'root';

CREATE INDEX idx_sessions_start ON sessions(start_ms);

CREATE INDEX idx_sessions_sweep_pending ON sessions(session_id) WHERE content_sweep_pending = 1;

CREATE TRIGGER sessions_parent_state_ad AFTER DELETE ON sessions
BEGIN
  UPDATE store_state SET revision = revision + 1 WHERE id = 1 AND revision < 9223372036854775807;
  SELECT RAISE(ABORT, 'store: store_state.revision cannot advance past its integer ceiling; export the store and start a new one')
   WHERE changes() = 0;
END;

CREATE TRIGGER sessions_parent_state_ai AFTER INSERT ON sessions
BEGIN
  UPDATE sessions
   SET parent_state = CASE
         WHEN EXISTS (
  SELECT 1 FROM session_relationship_evidence r
   WHERE r.session_id = sessions.session_id
     AND r.generation_id = COALESCE(sessions.active_generation_id, sessions.latest_attempt_generation_id)
     AND r.kind = 'started_by'
     AND r.target_state IN ('unknown','conflicting_current_native_evidence')
) THEN 'unresolved'
         WHEN CASE
  WHEN EXISTS (
  SELECT 1 FROM session_relationship_evidence e
   WHERE e.session_id = sessions.session_id
     AND e.generation_id = COALESCE(sessions.active_generation_id, sessions.latest_attempt_generation_id)
     AND e.kind IN ('started_by','context_from')
) THEN (
    SELECT r.target_local_id FROM session_relationship_evidence r
     WHERE r.session_id = sessions.session_id
       AND r.generation_id = COALESCE(sessions.active_generation_id, sessions.latest_attempt_generation_id)
       AND r.kind = 'started_by'
       AND r.target_state IN ('target_known','target_known_retained')
       AND r.target_local_id IS NOT NULL AND r.target_local_id <> ''
  )
  ELSE COALESCE(
    NULLIF((SELECT CASE WHEN json_valid(p.metadata_json) THEN json_extract(p.metadata_json, '$.parentUuid') END
              FROM session_publication_metadata p
             WHERE p.session_id = sessions.session_id), ''),
    sessions.parent_id)
END IS NOT NULL THEN 'resolved'
         ELSE 'root'
       END,
       resolved_parent_id = CASE
         WHEN EXISTS (
  SELECT 1 FROM session_relationship_evidence r
   WHERE r.session_id = sessions.session_id
     AND r.generation_id = COALESCE(sessions.active_generation_id, sessions.latest_attempt_generation_id)
     AND r.kind = 'started_by'
     AND r.target_state IN ('unknown','conflicting_current_native_evidence')
) THEN NULL
         WHEN CASE
  WHEN EXISTS (
  SELECT 1 FROM session_relationship_evidence e
   WHERE e.session_id = sessions.session_id
     AND e.generation_id = COALESCE(sessions.active_generation_id, sessions.latest_attempt_generation_id)
     AND e.kind IN ('started_by','context_from')
) THEN (
    SELECT r.target_local_id FROM session_relationship_evidence r
     WHERE r.session_id = sessions.session_id
       AND r.generation_id = COALESCE(sessions.active_generation_id, sessions.latest_attempt_generation_id)
       AND r.kind = 'started_by'
       AND r.target_state IN ('target_known','target_known_retained')
       AND r.target_local_id IS NOT NULL AND r.target_local_id <> ''
  )
  ELSE COALESCE(
    NULLIF((SELECT CASE WHEN json_valid(p.metadata_json) THEN json_extract(p.metadata_json, '$.parentUuid') END
              FROM session_publication_metadata p
             WHERE p.session_id = sessions.session_id), ''),
    sessions.parent_id)
END IS NOT NULL THEN CASE
  WHEN EXISTS (
  SELECT 1 FROM session_relationship_evidence e
   WHERE e.session_id = sessions.session_id
     AND e.generation_id = COALESCE(sessions.active_generation_id, sessions.latest_attempt_generation_id)
     AND e.kind IN ('started_by','context_from')
) THEN (
    SELECT r.target_local_id FROM session_relationship_evidence r
     WHERE r.session_id = sessions.session_id
       AND r.generation_id = COALESCE(sessions.active_generation_id, sessions.latest_attempt_generation_id)
       AND r.kind = 'started_by'
       AND r.target_state IN ('target_known','target_known_retained')
       AND r.target_local_id IS NOT NULL AND r.target_local_id <> ''
  )
  ELSE COALESCE(
    NULLIF((SELECT CASE WHEN json_valid(p.metadata_json) THEN json_extract(p.metadata_json, '$.parentUuid') END
              FROM session_publication_metadata p
             WHERE p.session_id = sessions.session_id), ''),
    sessions.parent_id)
END
         ELSE NULL
       END
 WHERE sessions.session_id = NEW.session_id;
  UPDATE store_state SET revision = revision + 1 WHERE id = 1 AND revision < 9223372036854775807;
  SELECT RAISE(ABORT, 'store: store_state.revision cannot advance past its integer ceiling; export the store and start a new one')
   WHERE changes() = 0;
END;

CREATE TRIGGER sessions_parent_state_au
AFTER UPDATE OF parent_id, model_harness, model_id, opaque_host_id,
  project_hash, start_ms, end_ms, ingested_ms, source_path, source_format,
  schema_version, git_branch, git_worktree, git_tracking, tool_version,
  pushed_at, tags, index_version, indexed_at, license_id, session_origin,
  origin_version, session_entries_hash, source_fingerprint, session_cwd,
  publication_capture_revision, index_format_version, adapter_version,
  artifact_hash, metric_seed_json, indexed_input_hash, active_generation_id,
  root_session_id, session_purpose, input_submission_count,
  content_sweep_pending, thinking_level, thinking_level_raw,
  thinking_level_history, latest_attempt_generation_id,
  latest_attempt_artifact_hash, latest_attempt_indexer_version,
  latest_attempt_failure_code, latest_attempt_failure_message,
  latest_attempt_captured_at_ms, latest_attempt_stats_json ON sessions
BEGIN
  UPDATE sessions
   SET parent_state = CASE
         WHEN EXISTS (
  SELECT 1 FROM session_relationship_evidence r
   WHERE r.session_id = sessions.session_id
     AND r.generation_id = COALESCE(sessions.active_generation_id, sessions.latest_attempt_generation_id)
     AND r.kind = 'started_by'
     AND r.target_state IN ('unknown','conflicting_current_native_evidence')
) THEN 'unresolved'
         WHEN CASE
  WHEN EXISTS (
  SELECT 1 FROM session_relationship_evidence e
   WHERE e.session_id = sessions.session_id
     AND e.generation_id = COALESCE(sessions.active_generation_id, sessions.latest_attempt_generation_id)
     AND e.kind IN ('started_by','context_from')
) THEN (
    SELECT r.target_local_id FROM session_relationship_evidence r
     WHERE r.session_id = sessions.session_id
       AND r.generation_id = COALESCE(sessions.active_generation_id, sessions.latest_attempt_generation_id)
       AND r.kind = 'started_by'
       AND r.target_state IN ('target_known','target_known_retained')
       AND r.target_local_id IS NOT NULL AND r.target_local_id <> ''
  )
  ELSE COALESCE(
    NULLIF((SELECT CASE WHEN json_valid(p.metadata_json) THEN json_extract(p.metadata_json, '$.parentUuid') END
              FROM session_publication_metadata p
             WHERE p.session_id = sessions.session_id), ''),
    sessions.parent_id)
END IS NOT NULL THEN 'resolved'
         ELSE 'root'
       END,
       resolved_parent_id = CASE
         WHEN EXISTS (
  SELECT 1 FROM session_relationship_evidence r
   WHERE r.session_id = sessions.session_id
     AND r.generation_id = COALESCE(sessions.active_generation_id, sessions.latest_attempt_generation_id)
     AND r.kind = 'started_by'
     AND r.target_state IN ('unknown','conflicting_current_native_evidence')
) THEN NULL
         WHEN CASE
  WHEN EXISTS (
  SELECT 1 FROM session_relationship_evidence e
   WHERE e.session_id = sessions.session_id
     AND e.generation_id = COALESCE(sessions.active_generation_id, sessions.latest_attempt_generation_id)
     AND e.kind IN ('started_by','context_from')
) THEN (
    SELECT r.target_local_id FROM session_relationship_evidence r
     WHERE r.session_id = sessions.session_id
       AND r.generation_id = COALESCE(sessions.active_generation_id, sessions.latest_attempt_generation_id)
       AND r.kind = 'started_by'
       AND r.target_state IN ('target_known','target_known_retained')
       AND r.target_local_id IS NOT NULL AND r.target_local_id <> ''
  )
  ELSE COALESCE(
    NULLIF((SELECT CASE WHEN json_valid(p.metadata_json) THEN json_extract(p.metadata_json, '$.parentUuid') END
              FROM session_publication_metadata p
             WHERE p.session_id = sessions.session_id), ''),
    sessions.parent_id)
END IS NOT NULL THEN CASE
  WHEN EXISTS (
  SELECT 1 FROM session_relationship_evidence e
   WHERE e.session_id = sessions.session_id
     AND e.generation_id = COALESCE(sessions.active_generation_id, sessions.latest_attempt_generation_id)
     AND e.kind IN ('started_by','context_from')
) THEN (
    SELECT r.target_local_id FROM session_relationship_evidence r
     WHERE r.session_id = sessions.session_id
       AND r.generation_id = COALESCE(sessions.active_generation_id, sessions.latest_attempt_generation_id)
       AND r.kind = 'started_by'
       AND r.target_state IN ('target_known','target_known_retained')
       AND r.target_local_id IS NOT NULL AND r.target_local_id <> ''
  )
  ELSE COALESCE(
    NULLIF((SELECT CASE WHEN json_valid(p.metadata_json) THEN json_extract(p.metadata_json, '$.parentUuid') END
              FROM session_publication_metadata p
             WHERE p.session_id = sessions.session_id), ''),
    sessions.parent_id)
END
         ELSE NULL
       END
 WHERE sessions.session_id = NEW.session_id;
  UPDATE store_state SET revision = revision + 1 WHERE id = 1 AND revision < 9223372036854775807;
  SELECT RAISE(ABORT, 'store: store_state.revision cannot advance past its integer ceiling; export the store and start a new one')
   WHERE changes() = 0;
END;

CREATE TRIGGER sessions_publication_metadata_changed
AFTER UPDATE OF parent_id,model_harness,model_id,opaque_host_id,project_hash,
 start_ms,end_ms,ingested_ms,source_path,source_format,schema_version,
 git_branch,git_worktree,git_tracking,tool_version,session_origin ON sessions
BEGIN
 UPDATE sessions SET indexed_publication_capture_revision=0, cwd_provenance_kind='not_recovered' WHERE session_id=NEW.session_id;
END;

CREATE TABLE daily_summary_harness_v71 AS SELECT * FROM daily_summary_harness;
DROP TABLE daily_summary_harness;

CREATE TABLE daily_summary_harness (
    date_utc TEXT NOT NULL,
    model_harness TEXT NOT NULL CHECK (model_harness IN ('claude-code','opencode','codex','gemini-cli','cursor','antigravity','strike','pi','grok-build','devin-desktop','cline','kiro','kiro-cli','kiro-crew')),
    session_count INTEGER NOT NULL DEFAULT 0,
    tokens_in INTEGER NOT NULL DEFAULT 0,
    tokens_out INTEGER NOT NULL DEFAULT 0,
    tokens_total INTEGER NOT NULL DEFAULT 0,
    avg_duration_ms REAL NOT NULL DEFAULT 0,
    avg_turns REAL NOT NULL DEFAULT 0,
    tool_call_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (date_utc, model_harness)
) STRICT, WITHOUT ROWID;

INSERT INTO daily_summary_harness SELECT * FROM daily_summary_harness_v71;
DROP TABLE daily_summary_harness_v71;

`
