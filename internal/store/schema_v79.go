package store

// migrationV79 adds the permanent local owner and the append-only claim log.
//
// local_owner holds exactly one row: singleton is UNIQUE and CHECKed to 1, and
// BEFORE INSERT/UPDATE/DELETE triggers refuse a second row, a replacement, and
// a deletion. owner_claims is the history; BEFORE INSERT ... WHEN EXISTS,
// BEFORE UPDATE and BEFORE DELETE triggers refuse id reuse, every rewrite, and
// every deletion, including the delete half of INSERT OR REPLACE (which fires
// no delete trigger while recursive triggers are off).
//
// sessions.owner_claim_id and annotations.owner_claim_id are nullable current
// pointers. NULL means unclaimed. They deliberately have no foreign key: a
// claim entry must survive the artifact it covered, and PruneSessions must keep
// deleting sessions without cascading into the history. Pulled rows stay
// foreign and never carry an owner pointer.
//
// The claim account scope is the V68 collective-observation scope: a normalized
// full Village base URL plus the stable Village user UUID. The composite foreign
// key reuses that grammar instead of defining a second account identity.
// owner_claims references collective_observation_scopes, which V68 introduces
// as an observation cache. A claim is permanent, so a scope row referenced by a
// claim is permanent too: the claim foreign key keeps that scope row from being
// deleted for the life of the store.
const migrationV79 = `
CREATE TABLE local_owner (
    id          TEXT PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 64),
    singleton   INTEGER NOT NULL DEFAULT 1 CHECK (singleton = 1) UNIQUE,
    created_at  INTEGER NOT NULL CHECK (created_at > 0)
) STRICT;

CREATE TABLE owner_claims (
    id                TEXT PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 64),
    local_owner_id    TEXT NOT NULL REFERENCES local_owner(id),
    village_url       TEXT NOT NULL,
    user_id           TEXT NOT NULL,
    username_snapshot TEXT NOT NULL,
    claimed_at        INTEGER NOT NULL CHECK (claimed_at > 0),
    source            TEXT NOT NULL CHECK (source IN ('sign_in_prompt','automatic','manual')),
    FOREIGN KEY (village_url, user_id)
        REFERENCES collective_observation_scopes(village_url, user_id)
) STRICT;

CREATE TRIGGER owner_claims_no_replace
BEFORE INSERT ON owner_claims
WHEN EXISTS (SELECT 1 FROM owner_claims WHERE id = NEW.id)
BEGIN
    SELECT RAISE(ABORT, 'owner_claims is append-only; a claim id is never reused');
END;

CREATE TRIGGER owner_claims_no_update
BEFORE UPDATE ON owner_claims BEGIN
    SELECT RAISE(ABORT, 'owner_claims is append-only; append a new claim');
END;

CREATE TRIGGER owner_claims_no_delete
BEFORE DELETE ON owner_claims BEGIN
    SELECT RAISE(ABORT, 'owner_claims is append-only; claims are retained');
END;

CREATE TRIGGER local_owner_no_second
BEFORE INSERT ON local_owner
WHEN EXISTS (SELECT 1 FROM local_owner)
BEGIN
    SELECT RAISE(ABORT, 'local_owner holds exactly one row for the life of the store');
END;

CREATE TRIGGER local_owner_no_update
BEFORE UPDATE ON local_owner BEGIN
    SELECT RAISE(ABORT, 'local_owner holds exactly one row for the life of the store');
END;

CREATE TRIGGER local_owner_no_delete
BEFORE DELETE ON local_owner BEGIN
    SELECT RAISE(ABORT, 'local_owner holds exactly one row for the life of the store');
END;

CREATE INDEX idx_owner_claims_owner_order
    ON owner_claims(local_owner_id, claimed_at, id);

CREATE INDEX idx_owner_claims_scope_order
    ON owner_claims(village_url, user_id, claimed_at, id);

ALTER TABLE sessions ADD COLUMN owner_claim_id TEXT;
ALTER TABLE annotations ADD COLUMN owner_claim_id TEXT;

CREATE INDEX idx_sessions_owner_claim ON sessions(owner_claim_id);
CREATE INDEX idx_annotations_owner_claim ON annotations(owner_claim_id);

-- V79 adds sessions.owner_claim_id, so the exhaustive parent-state
-- update trigger is redefined to fire on it. The trigger body is the
-- shipped V73 text plus the new column; the parent rule is unchanged.
DROP TRIGGER sessions_parent_state_au;
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
END;
`
