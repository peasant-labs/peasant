package store

// Legacy rows deliberately remain unproven; no lossy metadata backfill runs.
// The session-update trigger invalidates old snapshots even for legacy writers.
// A source-proven capture restores provenance inside the same transaction.
const migrationV50 = `
ALTER TABLE sessions ADD COLUMN session_cwd TEXT;
ALTER TABLE sessions ADD COLUMN cwd_provenance_kind TEXT NOT NULL DEFAULT 'not_recovered'
 CHECK (cwd_provenance_kind IN ('source_exact','source_workspace','source_worktree','source_absent','not_recovered'));
ALTER TABLE sessions ADD COLUMN publication_capture_revision INTEGER NOT NULL DEFAULT 0 CHECK (publication_capture_revision >= 0);
ALTER TABLE sessions ADD COLUMN indexed_publication_capture_revision INTEGER NOT NULL DEFAULT 0 CHECK (indexed_publication_capture_revision >= 0);
CREATE TABLE session_publication_metadata (
 session_id TEXT PRIMARY KEY REFERENCES sessions(session_id) ON DELETE CASCADE,
 capture_revision INTEGER NOT NULL CHECK (capture_revision > 0),
 schema_version INTEGER NOT NULL CHECK (schema_version > 0),
 metadata_json TEXT NOT NULL CHECK (json_valid(metadata_json)),
 metadata_hash TEXT NOT NULL CHECK (length(metadata_hash)=64 AND metadata_hash NOT GLOB '*[^0-9a-f]*'),
 content_hash TEXT NOT NULL CHECK (length(content_hash)=64 AND content_hash NOT GLOB '*[^0-9a-f]*'),
 captured_at INTEGER NOT NULL CHECK (captured_at > 0)
) STRICT;
CREATE TRIGGER sessions_publication_metadata_changed
AFTER UPDATE OF parent_id,model_harness,model_id,opaque_host_id,project_hash,
 start_ms,end_ms,ingested_ms,source_path,source_format,schema_version,
 git_branch,git_worktree,git_tracking,tool_version,session_origin ON sessions
BEGIN
 UPDATE sessions SET indexed_publication_capture_revision=0, cwd_provenance_kind='not_recovered' WHERE session_id=NEW.session_id;
END;`
