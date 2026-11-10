package store

// migrationV71 adds the derived lookup tables. None of the three objects holds
// authoritative state:
//
//   - idx_claude_evidence_identity_root is a partial index over the root
//     evidence rows that declare a teammate identity, so a store-wide
//     uniqueness question is answered by an indexed read.
//   - claude_teammate_spawns projects the spawns recorded in
//     claude_transcript_evidence.spawns_json, keyed by the transcript that
//     recorded them. The composite primary key means a pair repeated inside
//     one transcript cannot inflate the spawner count.
//   - codex_rollout_catalog records enumerated rollout paths that discovery
//     saw but did not store, so a later scoped run can still find the path or
//     refuse a duplicate id. The session_id index is deliberately non-unique:
//     two paths may carry one id, which is exactly the duplicate a refusal
//     must be able to see.
//
// The migration is create-only and additive: no existing table or index is
// rebuilt and foreign keys stay enforced (the migration options are nil).
//
// The spawns projection can be backfilled from data this migration can read,
// so an upgraded store gets a useful table immediately. INSERT OR IGNORE is
// required, not stylistic: mining appends spawn records without deduplicating
// them, and a transcript may already carry a repeated (team, name). The
// catalog cannot be backfilled — a path that was never stored is absent from
// the database. No shipped path writes it yet, so an upgraded store keeps an
// empty catalog until a discovery walk that enumerates rollouts fills it.
const migrationV71 = `
CREATE INDEX idx_claude_evidence_identity_root
  ON claude_transcript_evidence(identity_team, identity_name)
  WHERE scope = 'root' AND identity_team IS NOT NULL;

CREATE TABLE claude_teammate_spawns (
  spawner_path TEXT NOT NULL REFERENCES claude_transcript_evidence(source_path) ON DELETE CASCADE,
  team         TEXT NOT NULL,
  name         TEXT NOT NULL,
  PRIMARY KEY (spawner_path, team, name)
) STRICT;
CREATE INDEX idx_claude_teammate_spawns_identity ON claude_teammate_spawns(team, name);

CREATE TABLE codex_rollout_catalog (
  source_path TEXT PRIMARY KEY,
  session_id  TEXT NOT NULL
) STRICT;
CREATE INDEX idx_codex_rollout_catalog_session ON codex_rollout_catalog(session_id);

INSERT OR IGNORE INTO claude_teammate_spawns (spawner_path, team, name)
SELECT e.source_path,
       json_extract(j.value, '$.team'),
       json_extract(j.value, '$.name')
FROM claude_transcript_evidence e, json_each(e.spawns_json) AS j
WHERE e.scope = 'root'
  AND json_extract(j.value, '$.team') IS NOT NULL
  AND json_extract(j.value, '$.name') IS NOT NULL;
`
