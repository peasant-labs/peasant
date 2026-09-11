package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

var _ ingest.PublicationCaptureDigestReader = (*Store)(nil)

// ReadPublicationCaptureDigest reports the identity of the capture the session
// currently carries: its harness and the metadata and transcript digests of
// the mirrored pair. A session with no row, or whose snapshot no longer
// matches its capture revision, returns nil: a superseded snapshot is not
// evidence of what the database holds.
//
// It reads three small columns and opens no retained file, which is what lets
// a harvest decide that a settled session needs no work before it locks the
// session or reads its transcript.
func (s *Store) ReadPublicationCaptureDigest(ctx context.Context, sessionID ingest.SessionID) (_ *ingest.PublicationCaptureDigest, err error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: read the mirrored capture identity for session %s before reconciling its retained files: take connection: %w; no retained file or database row was changed; restore database access and retry harvest", sessionID, err)
	}
	defer s.pool.Put(conn)
	var digest *ingest.PublicationCaptureDigest
	err = sqlitex.ExecuteTransient(conn, `SELECT s.model_harness,p.metadata_hash,p.content_hash
 FROM sessions s JOIN session_publication_metadata p ON p.session_id=s.session_id
 AND p.capture_revision=s.publication_capture_revision
 WHERE s.session_id=? AND s.publication_capture_revision>0`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			var harness schema.Harness
			if harnessErr := harness.UnmarshalText([]byte(stmt.ColumnText(0))); harnessErr != nil || !harness.IsKnown() {
				return fmt.Errorf("stored harness %q is not recognized; no retained file or database row was changed; restore valid session metadata before harvest", stmt.ColumnText(0))
			}
			digest = &ingest.PublicationCaptureDigest{
				SessionID: sessionID, Harness: harness,
				MetadataHash: stmt.ColumnText(1), ContentHash: stmt.ColumnText(2),
			}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: read the mirrored capture identity for session %s before reconciling its retained files: %w; no retained file or database row was changed; restore database access and retry harvest", sessionID, err)
	}
	return digest, nil
}
