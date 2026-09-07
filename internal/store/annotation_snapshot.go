package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite/sqlitex"
)

// ReadAnnotationPushSnapshot applies the caller's existing metadata selection
// and checks only selected active entry-coordinate records. All reads use one
// connection and snapshot, released before a caller sends anything to Village.
func (s *Store) ReadAnnotationPushSnapshot(ctx context.Context, selection ingest.AnnotationReadSelection, includeRetractions bool) (snapshot ingest.AnnotationPushSnapshot, retErr error) {
	if selection == nil {
		return snapshot, fmt.Errorf("store: annotation snapshot has no explicit selection; no annotations were read for publication; supply the current annotation selection")
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return snapshot, fmt.Errorf("store: take annotation snapshot connection: %w", err)
	}
	defer s.pool.Put(conn)
	endSnapshot := sqlitex.Save(conn)
	defer endSnapshot(&retErr)
	rows, err := listAnnotationPushRowsOnConn(conn, sqlListSystemAnnotations, "selected active annotations")
	if err != nil {
		return snapshot, err
	}
	coordinateSessions := make([]string, 0, len(rows))
	for _, row := range rows {
		if !selection.IncludesAnnotation(row) {
			continue
		}
		snapshot.Annotations = append(snapshot.Annotations, row)
		if row.TargetKind == schema.TargetEntry && row.SessionID != nil {
			coordinateSessions = append(coordinateSessions, *row.SessionID)
		}
	}
	if err := s.validateIndexFormatIDsOnConn(conn, coordinateSessions); err != nil {
		return snapshot, err
	}
	unresolved, err := listUnresolvedAnnotationTargetAnchorsOnConn(conn, "")
	if err != nil {
		return snapshot, err
	}
	for _, row := range unresolved {
		if selection.IncludesUnresolvedAnchor(row) {
			snapshot.Unresolved = append(snapshot.Unresolved, row)
		}
	}
	if len(snapshot.Unresolved) > 0 || !includeRetractions {
		return snapshot, nil
	}
	retired, err := listAnnotationPushRowsOnConn(conn, sqlListSupersededAnnotations, "selected annotation retractions")
	if err != nil {
		return snapshot, err
	}
	for _, row := range retired {
		if selection.IncludesRetraction(row) {
			snapshot.Retractions = append(snapshot.Retractions, row)
		}
	}
	return snapshot, nil
}
