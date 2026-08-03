package store

const migrationV43 = `
CREATE TABLE session_publications (
  village_origin TEXT NOT NULL,
  owner_user_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  remote_transcript_id TEXT NOT NULL,
  transcript_url TEXT NOT NULL,
  project_hash TEXT NOT NULL,
  operation_fingerprint TEXT NOT NULL CHECK(length(operation_fingerprint) = 64),
  content_hash TEXT NOT NULL CHECK(length(content_hash) = 64),
  visibility TEXT NOT NULL CHECK(visibility IN ('private', 'group', 'public')),
  published_at INTEGER NOT NULL CHECK(published_at > 0),
  remote_updated_at INTEGER NOT NULL CHECK(remote_updated_at >= published_at),
  receipt_json TEXT NOT NULL CHECK(json_valid(receipt_json)),
  PRIMARY KEY (village_origin, owner_user_id, project_hash, session_id),
  UNIQUE (village_origin, owner_user_id, remote_transcript_id),
  FOREIGN KEY (session_id) REFERENCES sessions(session_id) ON DELETE CASCADE
) STRICT;
CREATE INDEX idx_session_publications_project ON session_publications(project_hash);

CREATE TABLE publication_attempt_diagnostics (
  id INTEGER PRIMARY KEY,
  village_origin TEXT NOT NULL,
  owner_user_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  project_hash TEXT NOT NULL,
  attempted_at INTEGER NOT NULL CHECK(attempted_at > 0),
  stage TEXT NOT NULL CHECK(stage IN ('publish','validate','visibility','persist')),
  message TEXT NOT NULL,
  FOREIGN KEY (session_id) REFERENCES sessions(session_id) ON DELETE CASCADE
) STRICT;
CREATE INDEX idx_publication_attempts_target ON publication_attempt_diagnostics(village_origin, owner_user_id, project_hash, session_id, attempted_at DESC);
`
