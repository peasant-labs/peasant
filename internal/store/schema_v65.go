package store

// migrationV65 extends the local Claude discovery cache with structural layout
// proof. Zero proof_version marks pre-upgrade rows for one-time re-mining; no
// transcript bytes or public session identity are changed by this migration.
const migrationV65 = `
ALTER TABLE claude_transcript_evidence
  ADD COLUMN proof_version INTEGER NOT NULL DEFAULT 0 CHECK(proof_version >= 0);
ALTER TABLE claude_transcript_evidence
  ADD COLUMN native_agent_id TEXT NOT NULL DEFAULT '';
ALTER TABLE claude_transcript_evidence
  ADD COLUMN native_agent_valid INTEGER NOT NULL DEFAULT 0 CHECK(native_agent_valid IN (0, 1));
ALTER TABLE claude_transcript_evidence
  ADD COLUMN content_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE claude_transcript_evidence
  ADD COLUMN identity_records_json TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(identity_records_json));
`
