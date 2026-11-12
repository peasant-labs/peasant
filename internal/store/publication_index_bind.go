package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

var _ ingest.PublicationIndexBinder = (*Store)(nil)

// BindPublicationIndex is the store half of the rule "bind without a
// re-parse". The rule is stated once, at
// ingest.Pipeline.capturedInputBindsWithoutReparse; this method re-decides none
// of it. It is a compare-and-swap:
//
//   - compare: in one immediate transaction, the row still reads as the caller
//     captured it (bind.ExpectedState), its input proof names the bytes the
//     caller read (bind.IndexedInputHash), and the current metadata capture is
//     still the revision the caller binds to;
//   - swap: advance indexed_at and, when a capture revision is named, bind the
//     index and the stored content capture to it. At most two rows change and
//     no entry is written, whatever the size of the session.
//
// Any failed compare changes nothing and leaves the session unbound, so the
// next harvest selects it again.
func (s *Store) BindPublicationIndex(ctx context.Context, bind ingest.PublicationIndexBind) (err error) {
	captured := bind.ExpectedState != nil && bind.ExpectedState.SessionID == bind.SessionID
	timed := bind.CaptureRevision >= 0 && bind.IndexedAtMs > 0
	if !captured || !timed || !validIndexInputHash(bind.IndexedInputHash) {
		return fmt.Errorf("store: bind the index of session %s to its metadata capture: the request carries no captured SQL state for this session, no valid input digest, a negative capture revision or no index time, so the store cannot prove which bytes the index describes; where: store.BindPublicationIndex, before any row changed; impact: the session stays unbound and is selected again; fix: capture the index input through the ordinary index path and retry harvest", bind.SessionID)
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: bind the index of session %s to its metadata capture: take database connection: %w; where: store.BindPublicationIndex, before any row changed; impact: the session stays unbound and is selected again; fix: restore database access and retry harvest", bind.SessionID, err)
	}
	defer s.pool.Put(conn)
	end, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return fmt.Errorf("store: bind the index of session %s to its metadata capture: begin transaction: %w; where: store.BindPublicationIndex, before any row changed; impact: the session stays unbound and is selected again; fix: restore database access and retry harvest", bind.SessionID, err)
	}
	defer end(&err)
	current, err := readIndexStateOnConn(conn, bind.SessionID)
	if err != nil {
		return err
	}
	proven := current != nil && current.IndexedInputHash != nil && *current.IndexedInputHash == bind.IndexedInputHash
	if !proven || !sameIndexState(bind.ExpectedState, current) {
		return &ingest.StaleIndexWorkError{SessionID: bind.SessionID}
	}
	if err = checkPublicationIndexRevision(conn, bind.SessionID, bind.CaptureRevision); err != nil {
		return err
	}
	if err = sqlitex.ExecuteTransient(conn, `UPDATE sessions SET indexed_at = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{bind.IndexedAtMs, string(bind.SessionID)}}); err != nil {
		return fmt.Errorf("store: advance the index time of session %s after its metadata refresh: %w; where: store.BindPublicationIndex; impact: the transaction rolled back and the session is selected again; fix: restore database access and retry harvest", bind.SessionID, err)
	}
	if bind.CaptureRevision == 0 {
		return nil
	}
	// Under the rule, a parse would rewrite the stored content capture
	// unchanged but for the capture revision it belongs to. Move that revision
	// with the binding; publication readiness requires the two to agree.
	if err = sqlitex.ExecuteTransient(conn, `UPDATE session_content_captures SET publication_capture_revision = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{bind.CaptureRevision, string(bind.SessionID)}}); err != nil {
		return fmt.Errorf("store: move the content capture of session %s to metadata capture revision %d: %w; where: store.BindPublicationIndex; impact: the transaction rolled back and the session is selected again; fix: restore database access and retry harvest", bind.SessionID, bind.CaptureRevision, err)
	}
	return stampPublicationIndex(conn, bind.SessionID, bind.CaptureRevision)
}
