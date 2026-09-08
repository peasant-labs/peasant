package store

// migrationV59 renames the unconstrained capture tag column to capture_format
// and closes it with a CHECK over the canonical set. The ordinary table rebuild
// preserves every other column and constraint byte-for-byte and recreates the
// status index. The CASE has no ELSE arm, so an unmapped legacy value inserts
// NULL into a NOT NULL column and fails the migration instead of coercing an
// untrusted capture into a canonical format.
const migrationV59 = `
CREATE TABLE session_content_captures_v58 AS SELECT * FROM session_content_captures;
DROP TABLE session_content_captures;

CREATE TABLE session_content_captures (
 session_id TEXT PRIMARY KEY REFERENCES sessions(session_id) ON DELETE CASCADE,
 status TEXT NOT NULL CHECK(status IN ('complete','incomplete','failed')),
 source_authority TEXT NOT NULL CHECK(source_authority IN ('new_ingest','peasant_snapshot','provider_source','none')),
 transcript_origin INTEGER NOT NULL DEFAULT 0,
 capture_format TEXT NOT NULL CHECK(capture_format IN ('full','preview_only','legacy_preview_only')),
 entry_count INTEGER NOT NULL CHECK(entry_count >= 0),
 content_row_count INTEGER NOT NULL CHECK(content_row_count >= 0),
 full_capture_sha256 TEXT CHECK(full_capture_sha256 IS NULL OR length(full_capture_sha256)=64),
 captured_at_ms INTEGER NOT NULL,
 failure_code TEXT,
 failure_message TEXT,
	publication_capture_revision INTEGER NOT NULL DEFAULT 0 CHECK(publication_capture_revision >= 0),
 CHECK(status != 'complete' OR (full_capture_sha256 IS NOT NULL AND failure_code IS NULL AND failure_message IS NULL))
) STRICT;

INSERT INTO session_content_captures
 (session_id,status,source_authority,transcript_origin,capture_format,entry_count,content_row_count,
  full_capture_sha256,captured_at_ms,failure_code,failure_message,publication_capture_revision)
 SELECT session_id,status,source_authority,transcript_origin,
  CASE capture_revision
   WHEN 'full-content-v1' THEN 'full'
   WHEN 'full-source-v1' THEN 'full'
   WHEN 'preview-only-v50' THEN 'preview_only'
   WHEN 'legacy-preview-only-v52' THEN 'legacy_preview_only'
   ELSE NULL END,
  entry_count,content_row_count,full_capture_sha256,captured_at_ms,failure_code,failure_message,
  publication_capture_revision FROM session_content_captures_v58;
DROP TABLE session_content_captures_v58;

CREATE INDEX idx_session_content_captures_status ON session_content_captures(status);
`
