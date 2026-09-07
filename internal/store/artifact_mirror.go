package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

var _ ingest.ArtifactMirrorStore = (*Store)(nil)

// MirrorArtifacts reconciles bounded committed snapshots. Parent rows are
// committed before dependent children, but the returned results retain request
// order. A failed session rolls back all its metadata/evidence, not its peers.
func (s *Store) MirrorArtifacts(ctx context.Context, requests []ingest.ArtifactMirrorRequest) (results []ingest.ArtifactMirrorResult) {
	results = make([]ingest.ArtifactMirrorResult, len(requests))
	for i, request := range requests {
		if request.Artifact != nil {
			results[i].SessionID = request.Artifact.Metadata.SessionID
		}
	}
	failAll := func(err error) {
		for i := range results {
			results[i].Mirrored = false
			results[i].Err = err
		}
	}
	if len(requests) == 0 {
		return results
	}
	if len(requests) > 256 {
		failAll(fmt.Errorf("mirror managed artifacts before database reconciliation: batch exceeds 256 sessions; no data was changed; submit bounded parent-first pages"))
		return results
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		failAll(fmt.Errorf("mirror managed artifacts after file commit: take database connection: %w; committed files remain pending reconciliation; restore database access and run harvest", err))
		return results
	}
	defer s.pool.Put(conn)
	if err = sqlitex.ExecuteTransient(conn, "BEGIN DEFERRED", nil); err != nil {
		failAll(fmt.Errorf("begin managed artifact mirror transaction: %w; no session was reconciled; restore database access and retry harvest", err))
		return results
	}
	defer func() {
		if err == nil && conn.AutocommitEnabled() {
			err = fmt.Errorf("managed artifact mirror transaction ended before batch commit")
		}
		if err == nil {
			err = sqlitex.ExecuteTransient(conn, "COMMIT", nil)
		}
		if err != nil && !conn.AutocommitEnabled() {
			// Cancellation must not prevent cleanup of a still-open transaction.
			interrupted := conn.SetInterrupt(nil)
			err = errors.Join(err, sqlitex.ExecuteTransient(conn, "ROLLBACK", nil))
			conn.SetInterrupt(interrupted)
		}
		if err != nil {
			failAll(fmt.Errorf("commit managed artifact mirror transaction: %w; no session in this batch was reconciled; keep committed files and retry harvest", err))
		}
	}()
	pending := make(map[ingest.SessionID]int)
	for index, request := range requests {
		if validationErr := request.Artifact.Validate(); validationErr != nil {
			results[index].Err = validationErr
			continue
		}
		sid := request.Artifact.Metadata.SessionID
		if _, duplicate := pending[sid]; duplicate {
			results[index].Err = fmt.Errorf("mirror session %s: duplicate snapshot in one batch; duplicate was refused; reconcile each session once per page", sid)
			continue
		}
		pending[sid] = index
	}
	for len(pending) > 0 {
		advanced := false
		for index, request := range requests {
			if request.Artifact == nil {
				continue
			}
			sid := request.Artifact.Metadata.SessionID
			if next, waiting := pending[sid]; !waiting || next != index {
				continue
			}
			if parent := request.Artifact.Metadata.ParentUUID; parent != nil {
				if _, waiting := pending[*parent]; waiting {
					continue
				}
			}
			var fatal bool
			results[index].Err, fatal = s.mirrorArtifactSavepoint(conn, request)
			if fatal {
				err = results[index].Err
				return results
			}
			results[index].Mirrored = results[index].Err == nil
			delete(pending, sid)
			advanced = true
		}
		if !advanced {
			for sid, index := range pending {
				results[index].Err = fmt.Errorf("mirror managed session %s: cyclic parent dependency in committed metadata; affected sessions were not reconciled; correct the parent relationships before retrying", sid)
			}
			break
		}
	}
	return results
}

func (s *Store) mirrorArtifactSavepoint(conn *sqlite.Conn, request ingest.ArtifactMirrorRequest) (error, bool) {
	const savepoint = "artifact_mirror_item"
	if conn.AutocommitEnabled() {
		return fmt.Errorf("mirror session %s: outer transaction was lost; remaining sessions were refused; retry the batch after restoring database access", request.Artifact.Metadata.SessionID), true
	}
	if err := sqlitex.ExecuteTransient(conn, "SAVEPOINT "+savepoint, nil); err != nil {
		return err, true
	}
	itemErr := s.mirrorArtifactOnConn(conn, request)
	if conn.AutocommitEnabled() {
		return fmt.Errorf("mirror session %s: outer transaction rolled back during reconciliation: %w; earlier tentative successes and remaining work were refused", request.Artifact.Metadata.SessionID, itemErr), true
	}
	if itemErr == nil {
		if err := sqlitex.ExecuteTransient(conn, "RELEASE SAVEPOINT "+savepoint, nil); err != nil {
			return err, true
		}
		return nil, false
	}
	interrupted := conn.SetInterrupt(nil)
	rollbackErr := sqlitex.ExecuteTransient(conn, "ROLLBACK TO SAVEPOINT "+savepoint, nil)
	releaseErr := sqlitex.ExecuteTransient(conn, "RELEASE SAVEPOINT "+savepoint, nil)
	conn.SetInterrupt(interrupted)
	if rollbackErr != nil || releaseErr != nil || conn.AutocommitEnabled() {
		return errors.Join(itemErr, rollbackErr, releaseErr), true
	}
	return itemErr, false
}

func (s *Store) mirrorArtifactOnConn(conn *sqlite.Conn, request ingest.ArtifactMirrorRequest) (err error) {
	meta := request.Artifact.Metadata
	origin := sessionorigin.Unknown
	if err := sqlitex.ExecuteTransient(conn, "SELECT schema_version, adapter_version, session_origin FROM sessions WHERE session_id = ?", &sqlitex.ExecOptions{
		Args: []any{string(meta.SessionID)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnInt(0) > ingest.CurrentSchemaVersion {
				return &ingest.UnsupportedMetadataVersionError{Path: string(meta.SessionID) + " (stored metadata)", Version: stmt.ColumnInt(0)}
			}
			if stmt.ColumnType(1) != sqlite.TypeNull {
				target := 0
				if meta.AdapterVersion != nil {
					target = *meta.AdapterVersion
				}
				if stmt.ColumnInt(1) > target {
					return fmt.Errorf("mirror session %s: stored adapter revision %d exceeds captured revision %d; producer evidence was preserved; recover the matching committed artifact before retrying", meta.SessionID, stmt.ColumnInt(1), target)
				}
			}
			var parseErr error
			origin, parseErr = sessionorigin.Parse(stmt.ColumnText(2))
			return parseErr
		},
	}); err != nil {
		return err
	}
	if request.Origin != nil {
		if err := request.Origin.Validate(); err != nil {
			return err
		}
		origin = *request.Origin
	}
	if request.EventSeq != nil && (*request.EventSeq < 0 || meta.ModelHarness != ingest.HarnessOpenCode) {
		return fmt.Errorf("mirror session %s: acquired event sequence is invalid for harness %s; all session changes were refused; supply a nonnegative cursor only when acquired from OpenCode", meta.SessionID, meta.ModelHarness)
	}
	if meta.ParentUUID != nil {
		parentExists := false
		if err := sqlitex.ExecuteTransient(conn, sqlSessionExists, &sqlitex.ExecOptions{
			Args: []any{string(*meta.ParentUUID)}, ResultFunc: func(*sqlite.Stmt) error { parentExists = true; return nil },
		}); err != nil {
			return err
		}
		if !parentExists {
			return fmt.Errorf("mirror managed session %s after file commit: parent %s is not stored; this child was not reconciled; retain its committed files and reconcile the parent before retrying harvest", meta.SessionID, *meta.ParentUUID)
		}
	}
	entry := ingest.StoreEntry{Metadata: &meta, Session: ingest.DiscoveredSession{SessionID: meta.SessionID, Harness: meta.ModelHarness, Origin: origin}}
	if err := s.insertSessionsOnConn(conn, []ingest.StoreEntry{entry}, request.Artifact.MetricSeed() != nil); err != nil {
		return err
	}
	if err := upsertSessionCommitsOnConn(conn, meta.SessionID, meta.Git.Commits); err != nil {
		return err
	}
	if request.EventSeq != nil {
		if err := upsertOpenCodeSeqCursorOnConn(conn, meta.SessionID, *request.EventSeq); err != nil {
			return err
		}
	}
	return sqlitex.ExecuteTransient(conn, "UPDATE sessions SET artifact_hash = ? WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{request.Artifact.ArtifactHash, string(meta.SessionID)}})
}
