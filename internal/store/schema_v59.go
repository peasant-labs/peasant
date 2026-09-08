package store

import (
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

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

// captureFormatPredecessorSchemaVersion is the user_version of a database that
// still carries the unconstrained capture_revision column, immediately before
// the capture-format rebuild runs.
const captureFormatPredecessorSchemaVersion = 58

// captureFormatUpgradeMapping is one arm of the migration's CASE, restated so
// the pre-migration guard and the migration cannot disagree about which stored
// tags are recognised. TestMigrationV59 asserts every arm is present in the SQL.
type captureFormatUpgradeMapping struct {
	StoredTag string
	Format    ingest.ContentCaptureFormat
}

// captureFormatUpgradeMappings lists every capture tag a shipped build wrote.
func captureFormatUpgradeMappings() []captureFormatUpgradeMapping {
	return []captureFormatUpgradeMapping{
		{StoredTag: "full-content-v1", Format: ingest.ContentCaptureFormatFull},
		{StoredTag: "full-source-v1", Format: ingest.ContentCaptureFormatFull},
		{StoredTag: "preview-only-v50", Format: ingest.ContentCaptureFormatPreviewOnly},
		{StoredTag: "legacy-preview-only-v52", Format: ingest.ContentCaptureFormatLegacyPreviewOnly},
	}
}

// refuseUnmappableCaptureFormats reports the rows that the capture-format
// rebuild cannot map, before the rebuild runs. The migration itself already
// fails closed on them, but SQLite can only say that a NOT NULL constraint
// failed, which names neither the blocking row nor a way forward. Running the
// same predicate first turns that into an answerable refusal.
func refuseUnmappableCaptureFormats(conn *sqlite.Conn) error {
	version := -1
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA user_version`, &sqlitex.ExecOptions{ResultFunc: func(st *sqlite.Stmt) error {
		version = st.ColumnInt(0)
		return nil
	}}); err != nil {
		return fmt.Errorf("store capture-format upgrade: cannot read the database schema version before migrating; the database may be unreadable or held by another process; close other peasant processes and retry: %w", err)
	}
	if version != captureFormatPredecessorSchemaVersion {
		return nil
	}
	placeholders := make([]string, 0, len(captureFormatUpgradeMappings()))
	args := make([]any, 0, len(captureFormatUpgradeMappings()))
	for _, m := range captureFormatUpgradeMappings() {
		placeholders = append(placeholders, "?")
		args = append(args, m.StoredTag)
	}
	var blocking []string
	if err := sqlitex.ExecuteTransient(conn, `SELECT session_id,capture_revision FROM session_content_captures WHERE capture_revision NOT IN (`+strings.Join(placeholders, ",")+`)`, &sqlitex.ExecOptions{Args: args, ResultFunc: func(st *sqlite.Stmt) error {
		blocking = append(blocking, fmt.Sprintf("session %s holds %q", st.ColumnText(0), st.ColumnText(1)))
		return nil
	}}); err != nil {
		return fmt.Errorf("store capture-format upgrade: cannot inspect stored capture tags before migrating; the session_content_captures table may be damaged; restore a backup or remove the database and re-ingest: %w", err)
	}
	if len(blocking) == 0 {
		return nil
	}
	return fmt.Errorf("store capture-format upgrade: the schema upgrade to a closed capture-format set stopped at schema %d because %d stored capture row(s) carry a tag no shipped peasant build wrote, so their contents cannot be attributed to a known capture shape and were left untouched (%s); recognised tags are %v; delete those rows from session_content_captures and run peasant harvest index --force to recapture those sessions, then reopen the database",
		captureFormatPredecessorSchemaVersion, len(blocking), strings.Join(blocking, "; "), captureFormatUpgradeStoredTags())
}

// captureFormatUpgradeStoredTags names the recognised tags for an error message.
func captureFormatUpgradeStoredTags() []string {
	tags := make([]string, 0, len(captureFormatUpgradeMappings()))
	for _, m := range captureFormatUpgradeMappings() {
		tags = append(tags, m.StoredTag)
	}
	return tags
}
