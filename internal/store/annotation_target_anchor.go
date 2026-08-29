package store

import (
	"context"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

type AnnotationTargetAnchorState string

const (
	AnnotationTargetAnchorResolved   AnnotationTargetAnchorState = "resolved"
	AnnotationTargetAnchorUnresolved AnnotationTargetAnchorState = "unresolved"
	AnnotationTargetAnchorSuperseded AnnotationTargetAnchorState = "superseded"
)

const (
	sqlUpsertAnnotationTargetAnchor = `INSERT INTO annotation_target_anchors (
    annotation_id, session_id, entry_index, end_index, state,
    entry_id, tool_call_id, entry_type, role, part_type, content_fingerprint, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(annotation_id) DO UPDATE SET
    session_id = excluded.session_id,
    entry_index = excluded.entry_index,
    end_index = excluded.end_index,
    state = excluded.state,
    entry_id = excluded.entry_id,
    tool_call_id = excluded.tool_call_id,
    entry_type = excluded.entry_type,
    role = excluded.role,
    part_type = excluded.part_type,
    content_fingerprint = excluded.content_fingerprint,
    updated_at = excluded.updated_at`

	sqlSelectAnchorEntryForSpan = `SELECT entry_index, entry_id, tool_call_id, entry_type, role, part_type, content_preview
FROM session_entries
WHERE session_id = ? AND entry_index >= ? AND entry_index < ?
ORDER BY entry_index
LIMIT 1`

	sqlListUnresolvedAnnotationTargetAnchors = `SELECT
	    ata.annotation_id, ata.session_id, ata.entry_index, ata.end_index, ata.state,
	    ann.name, ak.name, t.type_id, a.content_hash
FROM annotation_target_anchors ata
JOIN annotations a ON a.id = ata.annotation_id
JOIN annotators ann ON ann.id = a.annotator_id
JOIN annotator_kinds ak ON ak.id = ann.kind_id
JOIN annotation_types t ON t.id = a.annotation_type_id
WHERE ata.state = 'unresolved'
  AND a.superseded_by IS NULL
  AND (? = '' OR ata.session_id = ?)
ORDER BY ata.session_id, ata.annotation_id`
)

func upsertAnnotationTargetAnchorOnConn(conn *sqlite.Conn, annotationID, sessionID string, start, end int, state AnnotationTargetAnchorState) error {
	var anchor entryTargetAnchor
	if state == AnnotationTargetAnchorResolved {
		found := false
		if err := sqlitex.ExecuteTransient(conn, sqlSelectAnchorEntryForSpan, &sqlitex.ExecOptions{
			Args: []any{sessionID, start, end},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				anchor = scanEntryTargetAnchor(stmt)
				found = true
				return nil
			},
		}); err != nil {
			return fmt.Errorf("store: read anchor source entry for annotation %s during target repair: %w", annotationID, err)
		}
		if !found {
			return fmt.Errorf("store: persist resolved annotation target anchor for %s: no session entry exists at %s[%d,%d); what: Peasant tried to mark an annotation target resolved without a durable entry anchor; why: the target row and session_entries projection are inconsistent; where: annotation_target_anchors write; when: annotation creation or re-index repair; user impact: push/export is refused rather than publishing a guessed target; how to fix: re-run ingest for the session, then recreate the annotation if it is still missing", annotationID, sessionID, start, end)
		}
	}
	var startArg, endArg any
	if end > start {
		startArg = start
		endArg = end
	}
	if err := sqlitex.ExecuteTransient(conn, sqlUpsertAnnotationTargetAnchor, &sqlitex.ExecOptions{Args: []any{
		annotationID, sessionID, startArg, endArg, string(state),
		nullableStringArg(anchor.entryID), nullableStringArg(anchor.toolCallID), nullableStringArg(anchor.entryType), nullableStringArg(anchor.role), nullableStringArg(anchor.partType), nullableStringArg(anchor.contentPreview), time.Now().UnixMilli(),
	}}); err != nil {
		return fmt.Errorf("store: upsert annotation_target_anchors for %s: %w", annotationID, err)
	}
	return nil
}

func scanEntryTargetAnchor(stmt *sqlite.Stmt) entryTargetAnchor {
	return entryTargetAnchor{
		entryIndex:     stmt.ColumnInt(0),
		entryID:        nullableColumnTextValue(stmt, 1),
		toolCallID:     nullableColumnTextValue(stmt, 2),
		entryType:      nullableColumnTextValue(stmt, 3),
		role:           nullableColumnTextValue(stmt, 4),
		partType:       nullableColumnTextValue(stmt, 5),
		contentPreview: nullableColumnTextValue(stmt, 6),
	}
}

func nullableStringArg(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableColumnTextValue(stmt *sqlite.Stmt, col int) string {
	if stmt.ColumnType(col) == sqlite.TypeNull {
		return ""
	}
	return stmt.ColumnText(col)
}

func (s *Store) ListUnresolvedAnnotationTargetAnchors(ctx context.Context, sessionID string) ([]ingest.AnnotationTargetAnchorRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection for unresolved annotation target anchor lookup: %w", err)
	}
	defer s.pool.Put(conn)

	var rows []ingest.AnnotationTargetAnchorRow
	if err := sqlitex.ExecuteTransient(conn, sqlListUnresolvedAnnotationTargetAnchors, &sqlitex.ExecOptions{
		Args: []any{sessionID, sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row := ingest.AnnotationTargetAnchorRow{
				AnnotationID:  stmt.ColumnText(0),
				SessionID:     stmt.ColumnText(1),
				AnnotatorName: stmt.ColumnText(5),
				AnnotatorKind: stmt.ColumnText(6),
				TypeID:        stmt.ColumnText(7),
			}
			if stmt.ColumnType(8) != sqlite.TypeNull {
				v := stmt.ColumnText(8)
				row.ContentHash = &v
			}
			rows = append(rows, row)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: list unresolved annotation_target_anchors for session %q: %w", sessionID, err)
	}
	return rows, nil
}

func (s *Store) HasUnresolvedAnnotationTargetAnchors(ctx context.Context, sessionID ingest.SessionID) (bool, error) {
	rows, err := s.ListUnresolvedAnnotationTargetAnchors(ctx, string(sessionID))
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}
