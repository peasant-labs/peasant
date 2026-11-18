-- sessions_parent_state_ai
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
END

-- sessions_parent_state_au
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
  latest_attempt_captured_at_ms, latest_attempt_stats_json, owner_claim_id ON sessions
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
END

-- sessions_parent_state_ad
CREATE TRIGGER sessions_parent_state_ad AFTER DELETE ON sessions
BEGIN
  UPDATE store_state SET revision = revision + 1 WHERE id = 1 AND revision < 9223372036854775807;
  SELECT RAISE(ABORT, 'store: store_state.revision cannot advance past its integer ceiling; export the store and start a new one')
   WHERE changes() = 0;
END

-- session_relationship_evidence_parent_state_ai
CREATE TRIGGER session_relationship_evidence_parent_state_ai
AFTER INSERT ON session_relationship_evidence
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
END

-- session_relationship_evidence_parent_state_au
CREATE TRIGGER session_relationship_evidence_parent_state_au
AFTER UPDATE ON session_relationship_evidence
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
END

-- session_relationship_evidence_parent_state_ad
CREATE TRIGGER session_relationship_evidence_parent_state_ad
AFTER DELETE ON session_relationship_evidence
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
 WHERE sessions.session_id = OLD.session_id;
  UPDATE store_state SET revision = revision + 1 WHERE id = 1 AND revision < 9223372036854775807;
  SELECT RAISE(ABORT, 'store: store_state.revision cannot advance past its integer ceiling; export the store and start a new one')
   WHERE changes() = 0;
END

-- session_publication_metadata_parent_state_ai
CREATE TRIGGER session_publication_metadata_parent_state_ai
AFTER INSERT ON session_publication_metadata
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
END

-- session_publication_metadata_parent_state_au
CREATE TRIGGER session_publication_metadata_parent_state_au
AFTER UPDATE OF metadata_json ON session_publication_metadata
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
END

-- session_publication_metadata_parent_state_ad
CREATE TRIGGER session_publication_metadata_parent_state_ad
AFTER DELETE ON session_publication_metadata
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
 WHERE sessions.session_id = OLD.session_id;
  UPDATE store_state SET revision = revision + 1 WHERE id = 1 AND revision < 9223372036854775807;
  SELECT RAISE(ABORT, 'store: store_state.revision cannot advance past its integer ceiling; export the store and start a new one')
   WHERE changes() = 0;
END

