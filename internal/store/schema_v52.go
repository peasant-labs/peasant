package store

// migrationV52 separates durable prose from bounded entry previews.
const migrationV52 = `
CREATE TABLE session_content_captures (
 session_id TEXT PRIMARY KEY REFERENCES sessions(session_id) ON DELETE CASCADE,
 status TEXT NOT NULL CHECK(status IN ('complete','incomplete','failed')),
 source_authority TEXT NOT NULL CHECK(source_authority IN ('new_ingest','peasant_snapshot','provider_source','none')),
 transcript_origin INTEGER NOT NULL DEFAULT 0,
 capture_revision TEXT NOT NULL,
 entry_count INTEGER NOT NULL CHECK(entry_count >= 0),
 content_row_count INTEGER NOT NULL CHECK(content_row_count >= 0),
 full_capture_sha256 TEXT CHECK(full_capture_sha256 IS NULL OR length(full_capture_sha256)=64),
 captured_at_ms INTEGER NOT NULL,
 failure_code TEXT,
 failure_message TEXT,
	publication_capture_revision INTEGER NOT NULL DEFAULT 0 CHECK(publication_capture_revision >= 0),
 CHECK(status != 'complete' OR (full_capture_sha256 IS NOT NULL AND failure_code IS NULL AND failure_message IS NULL))
) STRICT;
CREATE TABLE session_entry_full_content (
 session_id TEXT NOT NULL,
 entry_index INTEGER NOT NULL,
 full_byte_length INTEGER NOT NULL CHECK(full_byte_length >= 0),
 full_sha256 TEXT NOT NULL CHECK(length(full_sha256)=64),
 preview_byte_length INTEGER NOT NULL CHECK(preview_byte_length >= 0),
 preview_sha256 TEXT NOT NULL CHECK(length(preview_sha256)=64),
 preview_is_full INTEGER NOT NULL CHECK(preview_is_full IN (0,1)),
 chunk_count INTEGER NOT NULL CHECK(chunk_count >= 0),
 captured_at_ms INTEGER NOT NULL,
 PRIMARY KEY(session_id,entry_index),
 FOREIGN KEY(session_id,entry_index) REFERENCES session_entries(session_id,entry_index) ON DELETE CASCADE
) STRICT, WITHOUT ROWID;
CREATE TABLE session_entry_full_content_chunks (
 session_id TEXT NOT NULL,
 entry_index INTEGER NOT NULL,
 chunk_index INTEGER NOT NULL,
 byte_offset INTEGER NOT NULL CHECK(byte_offset >= 0),
 byte_length INTEGER NOT NULL CHECK(byte_length >= 0),
 chunk_sha256 TEXT NOT NULL CHECK(length(chunk_sha256)=64),
 data BLOB NOT NULL,
 PRIMARY KEY(session_id,entry_index,chunk_index),
 FOREIGN KEY(session_id,entry_index) REFERENCES session_entry_full_content(session_id,entry_index) ON DELETE CASCADE
) STRICT, WITHOUT ROWID;
CREATE INDEX idx_session_content_captures_status ON session_content_captures(status);
INSERT INTO session_content_captures
 SELECT session_id,'incomplete','none',0,'legacy-preview-only-v52',
 (SELECT count(*) FROM session_entries e WHERE e.session_id=s.session_id),0,NULL,0,
 'legacy_preview_only','Full content has not been captured; run harvest index --force with retained artifacts.',0 FROM sessions s;
`
