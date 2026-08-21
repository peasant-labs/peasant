package store

// migrationV44 creates the Claude discovery evidence cache. Claude does not
// record which child session belongs to which parent, so discovery mines that
// link from the transcripts. Mining reads a whole transcript and parses every
// line, and it used to run again on every discovery. The table holds the mined
// result against the size and the modification time of the file that produced
// it, so an unchanged transcript is never read again.
const migrationV44 = `
CREATE TABLE claude_transcript_evidence (
  source_path TEXT PRIMARY KEY,
  scope TEXT NOT NULL CHECK(scope IN ('root', 'subagent')),
  mod_time_unix_nano INTEGER NOT NULL,
  size_bytes INTEGER NOT NULL CHECK(size_bytes >= 0),
  has_conversation INTEGER NOT NULL CHECK(has_conversation IN (0, 1)),
  identity_team TEXT,
  identity_name TEXT,
  spawns_json TEXT NOT NULL CHECK(json_valid(spawns_json)),
  title TEXT NOT NULL,
  branch TEXT NOT NULL,
  cwd TEXT NOT NULL,
  CHECK((identity_team IS NULL) = (identity_name IS NULL))
) STRICT;
`
