package store

// migrationV71 replaces the per-line structural skeleton with the facts the
// readers ask. identity_records_json is cleared to an empty array after the
// facts are derived, so an upgraded store does not keep both copies. A row
// whose skeleton cannot be parsed is marked for re-mining (proof_version 0)
// and keeps the default facts; the migration does not fail and does not invent
// a coordinate.
//
// A root row keeps identity_chain_json. A stored lineage proof passes a root
// transcript as the owner snapshot or the layout-parent artifact, and never
// passes a subagent row in either role. Subagent rows keep the session-id set,
// the spawn-agent set, and the two booleans, and an empty chain.
//
// The chain is not a bare (session, uuid, parent) triple. The lineage walk
// also compares type and subtype, and it accepts a foreign compact-boundary
// record that has no parent. Those two fields are part of each link. The list
// keeps the first and last occurrence of each structural key, in file order:
// the last copy is the parent-transition witness a first-only list would drop,
// and a repeated foreign link cannot newly fail a walk the first copy passed.
//
// proof_version is left as stored on a row the backfill could read. This
// build's proof version is newer, so discovery re-mines that row once and
// rewrites the facts from the transcript. The backfill is what a reader uses
// until that happens, and what a row keeps if its file is not visited.
const migrationV71 = `
ALTER TABLE claude_transcript_evidence
  ADD COLUMN identity_session_ids_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(identity_session_ids_json));
ALTER TABLE claude_transcript_evidence
  ADD COLUMN identity_spawn_agent_ids_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(identity_spawn_agent_ids_json));
ALTER TABLE claude_transcript_evidence
  ADD COLUMN identity_legacy_agent_proof INTEGER NOT NULL DEFAULT 0 CHECK(identity_legacy_agent_proof IN (0, 1));
ALTER TABLE claude_transcript_evidence
  ADD COLUMN identity_contradictory_sidechain INTEGER NOT NULL DEFAULT 0 CHECK(identity_contradictory_sidechain IN (0, 1));
ALTER TABLE claude_transcript_evidence
  ADD COLUMN identity_chain_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(identity_chain_json));

UPDATE claude_transcript_evidence
SET proof_version = 0
WHERE json_valid(identity_records_json) = 0
   OR json_type(identity_records_json) != 'array';

UPDATE claude_transcript_evidence
SET proof_version = 0
WHERE json_valid(identity_records_json)
  AND json_type(identity_records_json) = 'array'
  AND EXISTS (
    SELECT 1 FROM json_each(identity_records_json) AS j
    WHERE json_type(j.value) != 'object'
       OR IFNULL(json_type(j.value, '$.sessionId'), '') != 'text'
       OR json_extract(j.value, '$.sessionId') = ''
       OR (json_type(j.value, '$.uuid') IS NOT NULL AND json_type(j.value, '$.uuid') != 'text')
       OR (json_type(j.value, '$.parentUuid') IS NOT NULL AND json_type(j.value, '$.parentUuid') != 'text')
       OR (json_type(j.value, '$.type') IS NOT NULL AND json_type(j.value, '$.type') != 'text')
       OR (json_type(j.value, '$.subtype') IS NOT NULL AND json_type(j.value, '$.subtype') != 'text')
       OR (json_type(j.value, '$.spawnAgentId') IS NOT NULL AND json_type(j.value, '$.spawnAgentId') != 'text')
       OR (json_type(j.value, '$.agentId') IS NOT NULL AND json_type(j.value, '$.agentId') != 'text')
       OR (json_type(j.value, '$.isSidechain') IS NOT NULL AND json_type(j.value, '$.isSidechain') NOT IN ('true', 'false'))
  );

UPDATE claude_transcript_evidence
SET identity_session_ids_json = COALESCE((
      SELECT json_group_array(session_id ORDER BY session_id)
      FROM (
        SELECT DISTINCT json_extract(j.value, '$.sessionId') AS session_id
        FROM json_each(claude_transcript_evidence.identity_records_json) AS j
      )
    ), '[]'),
    identity_spawn_agent_ids_json = COALESCE((
      SELECT json_group_array(spawn_id ORDER BY spawn_id)
      FROM (
        SELECT DISTINCT json_extract(j.value, '$.spawnAgentId') AS spawn_id
        FROM json_each(claude_transcript_evidence.identity_records_json) AS j
        WHERE json_type(j.value, '$.spawnAgentId') = 'text'
          AND json_extract(j.value, '$.spawnAgentId') != ''
      )
    ), '[]'),
    identity_legacy_agent_proof = CASE WHEN json_array_length(identity_records_json) > 0
      AND NOT EXISTS (
        SELECT 1 FROM json_each(identity_records_json) AS j
        WHERE IFNULL(json_extract(j.value, '$.agentId'), '') != native_agent_id
           OR json_type(j.value, '$.isSidechain') IS NOT NULL
      ) THEN 1 ELSE 0 END,
    identity_contradictory_sidechain = CASE WHEN EXISTS (
        SELECT 1 FROM json_each(identity_records_json) AS j
        WHERE json_type(j.value, '$.isSidechain') = 'false'
      ) THEN 1 ELSE 0 END
WHERE json_valid(identity_records_json)
  AND json_type(identity_records_json) = 'array'
  AND NOT EXISTS (
    SELECT 1 FROM json_each(identity_records_json) AS j
    WHERE json_type(j.value) != 'object'
       OR IFNULL(json_type(j.value, '$.sessionId'), '') != 'text'
       OR json_extract(j.value, '$.sessionId') = ''
       OR (json_type(j.value, '$.uuid') IS NOT NULL AND json_type(j.value, '$.uuid') != 'text')
       OR (json_type(j.value, '$.parentUuid') IS NOT NULL AND json_type(j.value, '$.parentUuid') != 'text')
       OR (json_type(j.value, '$.type') IS NOT NULL AND json_type(j.value, '$.type') != 'text')
       OR (json_type(j.value, '$.subtype') IS NOT NULL AND json_type(j.value, '$.subtype') != 'text')
       OR (json_type(j.value, '$.spawnAgentId') IS NOT NULL AND json_type(j.value, '$.spawnAgentId') != 'text')
       OR (json_type(j.value, '$.agentId') IS NOT NULL AND json_type(j.value, '$.agentId') != 'text')
       OR (json_type(j.value, '$.isSidechain') IS NOT NULL AND json_type(j.value, '$.isSidechain') NOT IN ('true', 'false'))
  );

UPDATE claude_transcript_evidence
SET identity_chain_json = COALESCE((
  WITH records AS (
    SELECT j.key AS idx,
           json_extract(j.value, '$.sessionId') AS session_id,
           IFNULL(json_extract(j.value, '$.uuid'), '') AS uuid,
           IFNULL(json_extract(j.value, '$.parentUuid'), '') AS parent_uuid,
           IFNULL(json_extract(j.value, '$.type'), '') AS type,
           IFNULL(json_extract(j.value, '$.subtype'), '') AS subtype
    FROM json_each(claude_transcript_evidence.identity_records_json) AS j
  ),
  bounds AS (
    SELECT session_id, uuid, parent_uuid, type, subtype,
           MIN(idx) AS first_idx,
           MAX(idx) AS last_idx
    FROM records
    GROUP BY session_id, uuid, parent_uuid, type, subtype
  ),
  kept AS (
    SELECT first_idx AS idx, session_id, uuid, parent_uuid, type, subtype FROM bounds
    UNION ALL
    SELECT last_idx, session_id, uuid, parent_uuid, type, subtype FROM bounds WHERE last_idx != first_idx
  )
  SELECT json_group_array(json_object(
    'sessionId', session_id,
    'uuid', uuid,
    'parentUuid', parent_uuid,
    'type', type,
    'subtype', subtype
  ) ORDER BY idx)
  FROM kept
), '[]')
WHERE scope = 'root'
  AND json_valid(identity_records_json)
  AND json_type(identity_records_json) = 'array'
  AND NOT EXISTS (
    SELECT 1 FROM json_each(identity_records_json) AS j
    WHERE json_type(j.value) != 'object'
       OR IFNULL(json_type(j.value, '$.sessionId'), '') != 'text'
       OR json_extract(j.value, '$.sessionId') = ''
       OR (json_type(j.value, '$.uuid') IS NOT NULL AND json_type(j.value, '$.uuid') != 'text')
       OR (json_type(j.value, '$.parentUuid') IS NOT NULL AND json_type(j.value, '$.parentUuid') != 'text')
       OR (json_type(j.value, '$.type') IS NOT NULL AND json_type(j.value, '$.type') != 'text')
       OR (json_type(j.value, '$.subtype') IS NOT NULL AND json_type(j.value, '$.subtype') != 'text')
       OR (json_type(j.value, '$.spawnAgentId') IS NOT NULL AND json_type(j.value, '$.spawnAgentId') != 'text')
       OR (json_type(j.value, '$.agentId') IS NOT NULL AND json_type(j.value, '$.agentId') != 'text')
       OR (json_type(j.value, '$.isSidechain') IS NOT NULL AND json_type(j.value, '$.isSidechain') NOT IN ('true', 'false'))
  );

UPDATE claude_transcript_evidence
SET identity_records_json = '[]';
`
