package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

var _ ingest.StoredMetadataReader = (*Store)(nil)

// ReadStoredMetadata reads recorded context for retained-envelope recovery.
// One statement captures the session, dimensions, retained seed and commits.
// Missing original fields (including redaction and project name) stay absent;
// no computed metric, historical checksum or current adapter target is used.
func (s *Store) ReadStoredMetadata(ctx context.Context, sid ingest.SessionID) ([]byte, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: recover recorded metadata for session %s: %w; no retained files were changed; restore database access and retry harvest", sid, err)
	}
	defer s.pool.Put(conn)
	var data []byte
	err = sqlitex.ExecuteTransient(conn, `SELECT json_patch(json_object(
    'schemaVersion', s.schema_version,
    'sessionId', s.session_id, 'parentUuid', s.parent_id,
    'harness', s.model_harness, 'model', s.model_id, 'version', s.tool_version,
    'hostSlug', h.host_slug,
    'timestamp', json_object('start', s.start_ms, 'end', s.end_ms, 'ingested', s.ingested_ms),
    'source', json_object('filePath', s.source_path, 'format', s.source_format),
    'project', json_object('hash', s.project_hash, 'filePath', p.canonical_cwd),
    'git', json_object('branch', s.git_branch, 'remote', h.git_remote,
        'worktree', s.git_worktree, 'tracking', s.git_tracking,
        'commits', (SELECT json_group_array(json_object(
            'hash', c.commit_hash, 'message', c.message, 'authorName', c.author_name,
            'authorEmail', c.author_email, 'commitTime', c.commit_time, 'authorTime', c.author_time
        )) FROM (SELECT * FROM session_commits WHERE session_id = s.session_id ORDER BY commit_hash) c))
), json_object('adapterVersion', s.adapter_version, 'stats', json(s.metric_seed_json))),
json_type(s.metric_seed_json)
FROM sessions s
JOIN host_slugs h ON h.opaque_id = s.opaque_host_id
LEFT JOIN projects p ON p.project_hash = s.project_hash
WHERE s.session_id = ?1`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(1) != sqlite.TypeNull && stmt.ColumnText(1) != "object" {
				return fmt.Errorf("retained metric seed must be a JSON object; SQL NULL alone represents unknown input")
			}
			data = []byte(stmt.ColumnText(0))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: read recorded metadata for session %s before retained recovery: %w; no retained files were changed; restore valid stored metadata and retry harvest", sid, err)
	}
	return data, nil
}
