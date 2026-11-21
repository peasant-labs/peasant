package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// DanglingMappingSetDisposition says what a dangling mapping-set reference
// means for the store.
type DanglingMappingSetDisposition string

const (
	// DanglingMappingSetCorruption is a reference no sweep will remove: a root
	// (the active generation or the latest attempt) reads parsed rows that are
	// not there, or a superseded reference sits in a session whose sweep flag
	// is clear.
	DanglingMappingSetCorruption DanglingMappingSetDisposition = "corruption"
	// DanglingMappingSetPendingSweep is a superseded reference in a flagged
	// session: an interrupted sweep collected its owner first, nothing reads
	// it, and the next pass deletes it.
	DanglingMappingSetPendingSweep DanglingMappingSetDisposition = "pending-sweep"
)

// AllDanglingMappingSetDispositions lists every disposition, in severity order.
func AllDanglingMappingSetDispositions() []DanglingMappingSetDisposition {
	return []DanglingMappingSetDisposition{DanglingMappingSetCorruption, DanglingMappingSetPendingSweep}
}

// DanglingMappingSet is one dangling-mapping-set finding: a generation whose
// mapping_set_id names no generation of its session that owns its rows.
type DanglingMappingSet struct {
	SessionID    schema.SessionID
	GenerationID string
	MappingSetID string
	Disposition  DanglingMappingSetDisposition
}

// collectDanglingMappingSets finds every generation whose mapping_set_id
// names a generation that is missing from its session or is itself a
// reference. A root reading such a reference, or a superseded one in an
// unflagged session, is corruption; a superseded one in a flagged session is
// a pending sweep. Verify reads every reference: the column has no index, and
// verify is a full pass.
func collectDanglingMappingSets(conn *sqlite.Conn, report *ContentVerifyReport) error {
	query := `SELECT g.session_id, g.generation_id, g.mapping_set_id,
 COALESCE(g.generation_id = s.active_generation_id, 0) OR COALESCE(g.generation_id = s.latest_attempt_generation_id, 0),
 s.content_sweep_pending
FROM session_generations g JOIN sessions s ON s.session_id = g.session_id
WHERE g.mapping_set_id IS NOT NULL
 AND NOT EXISTS (SELECT 1 FROM session_generations o WHERE o.session_id = g.session_id AND o.generation_id = g.mapping_set_id AND o.mapping_set_id IS NULL)
ORDER BY g.session_id, g.generation_id`
	err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		id, err := schema.NewSessionID(stmt.ColumnText(0))
		if err != nil {
			return err
		}
		disposition := DanglingMappingSetCorruption
		if stmt.ColumnInt64(3) == 0 && stmt.ColumnInt64(4) == 1 {
			disposition = DanglingMappingSetPendingSweep
		}
		report.DanglingMappingSets = append(report.DanglingMappingSets, DanglingMappingSet{
			SessionID: id, GenerationID: stmt.ColumnText(1), MappingSetID: stmt.ColumnText(2), Disposition: disposition,
		})
		return nil
	}})
	if err != nil {
		return fmt.Errorf("store: check the mapping-set references during verification: %w; nothing was changed; retry verification", err)
	}
	return nil
}

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
FROM session_entry_bodies b WHERE NOT EXISTS (SELECT 1 FROM session_generation_entries m WHERE m.session_id=b.session_id AND m.body_digest=b.body_digest) OR NOT EXISTS (SELECT 1 FROM sessions s WHERE s.session_id=b.session_id)
UNION ALL
SELECT b.session_id, 0, 1, b.byte_length FROM session_content b WHERE NOT EXISTS (SELECT 1 FROM session_generation_content m WHERE m.session_id=b.session_id AND m.digest=b.digest) OR NOT EXISTS (SELECT 1 FROM sessions s WHERE s.session_id=b.session_id)
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
	if err := collectDanglingMappingSets(conn, report); err != nil {
		return err
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
		var flagged, found bool
		if err := sqlitex.Execute(conn, `SELECT content_sweep_pending FROM sessions WHERE session_id=?`, &sqlitex.ExecOptions{Args: []any{string(id)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			flagged = stmt.ColumnInt(0) != 0
			return nil
		}}); err != nil {
			return err
		}
		if !found {
			// Content whose sessions row is gone: no sweep can reach it, and
			// the session reads as absent while its bodies remain.
			report.OwnerlessSessions = append(report.OwnerlessSessions, id)
		} else if !flagged {
			report.UnflaggedOrphanSessions = append(report.UnflaggedOrphanSessions, id)
		}
	}
	sort.Slice(report.OwnerlessSessions, func(i, j int) bool {
		return report.OwnerlessSessions[i] < report.OwnerlessSessions[j]
	})
	sort.Slice(report.UnflaggedOrphanSessions, func(i, j int) bool {
		return report.UnflaggedOrphanSessions[i] < report.UnflaggedOrphanSessions[j]
	})
	return nil
}
