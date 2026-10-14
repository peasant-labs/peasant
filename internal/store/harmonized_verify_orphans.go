package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// collectContentOrphans counts objects with no mapping in ANY generation.
// Database bytes are the stored body text payload plus orphan blob lengths;
// owned-directory bytes are separate, never mixed with SQLite page overhead.
func (s *Store) collectContentOrphans(ctx context.Context, report *ContentVerifyReport) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return err
	}
	defer s.pool.Put(conn)
	owners := make(map[schema.SessionID]bool)
	query := `SELECT session_id, sum(bodies), sum(blobs), sum(bytes) FROM (
SELECT b.session_id, 1 bodies, 0 blobs,
 coalesce(length(CAST(b.content_preview AS BLOB)),0)+coalesce(length(CAST(b.tool_input AS BLOB)),0)+coalesce(length(CAST(b.tool_output AS BLOB)),0)+coalesce(length(CAST(b.extra AS BLOB)),0)+coalesce(length(CAST(b.extra_verbatim AS BLOB)),0) bytes
FROM session_entry_bodies b WHERE NOT EXISTS (SELECT 1 FROM session_generation_entries m WHERE m.session_id=b.session_id AND m.body_digest=b.body_digest)
UNION ALL
SELECT b.session_id, 0, 1, b.byte_length FROM session_content b WHERE NOT EXISTS (SELECT 1 FROM session_generation_content m WHERE m.session_id=b.session_id AND m.digest=b.digest)
) GROUP BY session_id`
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		id, err := schema.NewSessionID(stmt.ColumnText(0))
		if err != nil {
			return err
		}
		owners[id] = true
		report.OrphanBodies += stmt.ColumnInt64(1)
		report.OrphanBlobs += stmt.ColumnInt64(2)
		report.OrphanBytes += stmt.ColumnInt64(3)
		return nil
	}}); err != nil {
		return fmt.Errorf("store: count unreferenced content during verification: %w; no content was deleted; retry verification", err)
	}
	if s.generationArtifacts != nil {
		ids, err := s.generationArtifacts.ListOwnedSessionIDs(ctx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			var keep string
			if err := sqlitex.Execute(conn, `SELECT s.active_generation_id FROM sessions s JOIN session_projection_generations g ON g.session_id=s.session_id AND g.generation_id=s.active_generation_id WHERE s.session_id=?`, &sqlitex.ExecOptions{Args: []any{string(id)}, ResultFunc: func(stmt *sqlite.Stmt) error {
				keep = stmt.ColumnText(0)
				return nil
			}}); err != nil {
				return err
			}
			dirs, footprint, err := s.generationArtifacts.OrphanGenerationFootprint(ctx, id, keep)
			if err != nil {
				return err
			}
			report.OrphanOwnedDirs += dirs
			report.OrphanOwnedBytes += footprint.Bytes
			if dirs > 0 {
				owners[id] = true
			}
		}
	}
	for id := range owners {
		var flagged bool
		if err := sqlitex.Execute(conn, `SELECT content_sweep_pending FROM sessions WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(id)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			flagged = stmt.ColumnInt(0) != 0
			return nil
		}}); err != nil {
			return err
		}
		if !flagged {
			report.UnflaggedOrphanSessions = append(report.UnflaggedOrphanSessions, id)
		}
	}
	sort.Slice(report.UnflaggedOrphanSessions, func(i, j int) bool {
		return report.UnflaggedOrphanSessions[i] < report.UnflaggedOrphanSessions[j]
	})
	return nil
}
