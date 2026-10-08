package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// SweepResult is the per-session sweep report (design §4.5): the bounded,
// idempotent pass that deletes superseded catalog rows, removes orphan
// directories, deletes unreferenced entry rows and blobs, and rebuilds the
// search index when a delete could not trust its values.
type SweepResult struct {
	RowsDeleted        int64
	DirectoriesRemoved int64
	BodiesDeleted      int64
	BlobsDeleted       int64
	Rebuilt            bool
}

// SweepSessionReport is one flagged session's sweep outcome for the
// harvest-start recovery pass.
type SweepSessionReport struct {
	SessionID schema.SessionID
	Result    SweepResult
}

// SweepFlaggedReport is the harvest-start recovery outcome: every flagged
// session the pass swept, plus the per-session failures it skipped past.
// A skipped session keeps its flag, so the next pass retries it.
type SweepFlaggedReport struct {
	Sessions []SweepSessionReport
	Warnings []error
}

// SweepSession runs the per-session sweep (design §4.5): bounded, resumable,
// idempotent, and session-qualified. It takes the exclusive per-session lock
// for the whole pass, so a concurrent activation commit serializes against
// it; lock-free staging that lands mid-pass is fail-closed (the commit's
// mapping foreign key refuses a body the sweep deleted, and the next harvest
// retries the session).
//
// The pass, in order:
//
//  1. superseded catalog rows in both catalog tables (and every
//     generation-scoped table, through the shared reclaim list);
//  2. leftover generation directories (Release N only);
//  3. unreferenced entry rows, verified first (a digest mismatch flags the
//     search index for rebuild instead of trusting the delete values);
//  4. unreferenced blobs (chunks cascade);
//  5. the whole-index rebuild when flagged, then content_sweep_pending = 0.
//
// Every row delete runs in bounded batches (the configured write.sweep_rows);
// a crash between batches leaves a partial, safe deletion with the flag set,
// and the next pass completes it. A crash before the flag clears leaves the
// flag set with the new generation active, and the next harvest recovers by
// re-harvest: an input change re-stages over the reusable orphans, an
// unchanged input takes the skip path, and the harvest-start flagged sweep
// clears whatever either path left behind.
func (s *Store) SweepSession(ctx context.Context, sessionID schema.SessionID) (SweepResult, error) {
	if err := s.requireGenerationSupport(); err != nil {
		return SweepResult{}, err
	}
	release, err := s.sessionLocker.LockExclusive(ctx, sessionID)
	if err != nil {
		return SweepResult{}, err
	}
	defer func() { _ = release() }()
	return s.sweepSessionLocked(ctx, sessionID)
}

// sweepSessionLocked runs the five sweep steps with the exclusive
// per-session lock held by the caller (SweepSession, or reclaim's orphan
// pass). It never takes the lock itself, so the two callers cannot deadlock
// against a non-reentrant file lock.
func (s *Store) sweepSessionLocked(ctx context.Context, sessionID schema.SessionID) (SweepResult, error) {
	var result SweepResult
	active, err := s.activeGenerationID(ctx, sessionID)
	if err != nil {
		return result, err
	}
	// A session with no active generation keeps nothing: the non-active
	// predicate below is NULL-safe, so every catalog row counts as
	// superseded.
	if err := reportContentSweepSeam(contentSweepSeamAfterCommit); err != nil {
		return result, fmt.Errorf("store: sweep session %s interrupted before any delete: %w; no row was deleted and the sweep flag is unchanged", sessionID, err)
	}
	limit := s.writeConfigForLane().SweepRows
	if limit < 1 {
		limit = 1
	}

	rows, err := s.deleteSupersededGenerationRows(ctx, sessionID, active)
	if err != nil {
		return result, err
	}
	result.RowsDeleted = rows.Total()

	dirs, err := s.removeLeftoverGenerationDirs(ctx, sessionID, active)
	if err != nil {
		return result, err
	}
	result.DirectoriesRemoved = dirs

	bodies, blobs, rebuilt, err := s.sweepUnreferencedObjects(ctx, sessionID, limit)
	if err != nil {
		return result, err
	}
	result.BodiesDeleted = bodies
	result.BlobsDeleted = blobs
	result.Rebuilt = rebuilt

	if err := s.clearSweepFlag(ctx, sessionID); err != nil {
		return result, err
	}
	return result, nil
}

// reportSweepBatch reports one inter-batch boundary to the crash-injection
// seam. Production runs with a nil hook and never observes it; a
// crash-recovery test fails it to prove a partial, safe deletion with the
// flag left set.
func (s *Store) reportSweepBatch(ctx context.Context, sessionID schema.SessionID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := reportContentSweepSeam(contentSweepSeamMidBatch); err != nil {
		return fmt.Errorf("store: sweep session %s interrupted mid-pass: %w; the committed batches stay deleted, the sweep flag is set, and the next pass completes the sweep", sessionID, err)
	}
	return nil
}

// sweepUnreferencedObjects runs the sweep's object steps (design §4.5 steps
// 3-5) with the caller's lock held: verified body deletes, blob deletes
// with cascading chunks, and the whole-index rebuild when the delete values
// could not be trusted. Every delete runs in bounded batches; the seam
// fires between batches so a crash test observes the partial state.
func (s *Store) sweepUnreferencedObjects(ctx context.Context, sessionID schema.SessionID, limit int) (bodies, blobs int64, rebuilt bool, err error) {
	for {
		if err := ctx.Err(); err != nil {
			return bodies, blobs, rebuilt, err
		}
		digests, err := s.unreferencedBodyDigests(ctx, sessionID, limit)
		if err != nil {
			return bodies, blobs, rebuilt, err
		}
		if len(digests) == 0 {
			break
		}
		deleted, err := s.deleteVerifiedBodies(ctx, sessionID, digests)
		if err != nil {
			return bodies, blobs, rebuilt, err
		}
		bodies += deleted
		if err := s.reportSweepBatch(ctx, sessionID); err != nil {
			return bodies, blobs, rebuilt, err
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return bodies, blobs, rebuilt, err
		}
		digests, err := s.unreferencedBlobDigests(ctx, sessionID, limit)
		if err != nil {
			return bodies, blobs, rebuilt, err
		}
		if len(digests) == 0 {
			break
		}
		deleted, err := s.deleteUnreferencedBlobs(ctx, sessionID, digests)
		if err != nil {
			return bodies, blobs, rebuilt, err
		}
		blobs += deleted
		if err := s.reportSweepBatch(ctx, sessionID); err != nil {
			return bodies, blobs, rebuilt, err
		}
	}
	rebuilt, err = s.rebuildSearchIndexWhenFlagged(ctx, sessionID)
	if err != nil {
		return bodies, blobs, rebuilt, err
	}
	return bodies, blobs, rebuilt, nil
}

// unreferencedBodyDigests lists up to limit entry bodies of the session that
// no generation entry row references, through the body-digest index. Every
// predicate is qualified by session_id, so a sweep never sees another
// session's rows.
func (s *Store) unreferencedBodyDigests(ctx context.Context, sessionID schema.SessionID, limit int) ([]string, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection to list unreferenced bodies for session %s: %w; no row was deleted", sessionID, err)
	}
	defer s.pool.Put(conn)
	var digests []string
	err = sqlitex.ExecuteTransient(conn, `SELECT b.body_digest FROM session_entry_bodies b WHERE b.session_id = ? AND NOT EXISTS (SELECT 1 FROM session_generation_entries m WHERE m.session_id = b.session_id AND m.body_digest = b.body_digest) LIMIT ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), int64(limit)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			digests = append(digests, stmt.ColumnText(0))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list unreferenced bodies for session %s: %w; no row was deleted", sessionID, err)
	}
	return digests, nil
}

// deleteVerifiedBodies deletes one bounded batch of unreferenced bodies in
// one transaction. Each row is verified first: the recomputed digest must
// equal the stored one, or the delete values cannot be trusted for the
// BEFORE DELETE un-indexing and the pass flags the search index for rebuild
// before deleting the row anyway. A referenced body aborts the batch through
// its foreign key: the transaction rolls back and the error names the
// session, so a stray delete is never silent.
func (s *Store) deleteVerifiedBodies(ctx context.Context, sessionID schema.SessionID, digests []string) (int64, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: take connection to delete %d bodies for session %s: %w; no row was deleted", len(digests), sessionID, err)
	}
	defer s.pool.Put(conn)
	var deleted int64
	txnErr := error(nil)
	endFn := sqlitex.Transaction(conn)
	defer endFn(&txnErr)
	for _, digest := range digests {
		if err := ctx.Err(); err != nil {
			txnErr = err
			return deleted, txnErr
		}
		trusted, err := sweepVerifyBodyForDelete(conn, sessionID, digest)
		if err != nil {
			txnErr = err
			return deleted, txnErr
		}
		if !trusted {
			if err := sweepFlagSearchRebuild(conn, sessionID); err != nil {
				txnErr = err
				return deleted, txnErr
			}
		}
		if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_entry_bodies WHERE session_id = ? AND body_digest = ?`, &sqlitex.ExecOptions{
			Args: []any{string(sessionID), digest},
		}); err != nil {
			txnErr = fmt.Errorf("store: delete body %s for session %s: %w; the batch rolled back and every body row is unchanged (a referenced body refuses through its foreign key: the sweep never deletes live content)", digest, sessionID, err)
			return deleted, txnErr
		}
		deleted += int64(conn.Changes())
	}
	return deleted, nil
}

// sweepVerifyBodyForDelete is the sweep's delete-time check: the row's
// recomputed digest must equal its stored anchor, or the FTS delete values
// read from its columns cannot be trusted. A missing row reads as
// untrusted: concurrent work removed it first, and the rebuild resolves any
// doubt about its postings.
//
// The search change owns the canonical delete-time check and the rebuild
// gates; this sweep-local helper carries the same contract until that change
// consolidates the two call sites.
func sweepVerifyBodyForDelete(conn *sqlite.Conn, sessionID schema.SessionID, digest string) (bool, error) {
	found := false
	trusted := false
	err := sqlitex.ExecuteTransient(conn, `SELECT `+sqlSelectBodyColumns+` FROM session_entry_bodies WHERE session_id = ? AND body_digest = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), digest},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			record := scanEntryRecord(stmt)
			trusted = string(bodyDigestForRecord(record)) == digest
			return nil
		},
	})
	if err != nil {
		return false, fmt.Errorf("store: verify body %s for session %s before delete: %w; no row was deleted", digest, sessionID, err)
	}
	if !found {
		return false, nil
	}
	return trusted, nil
}

// sweepFlagSearchRebuild marks the store-global search index for a
// whole-index rebuild on the caller's connection: a deleting transaction
// calls it when the deleted row failed the digest check, so its indexed
// values cannot be trusted for the BEFORE DELETE un-indexing.
func sweepFlagSearchRebuild(conn *sqlite.Conn, sessionID schema.SessionID) error {
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_search_state SET needs_rebuild = 1 WHERE id = 1`, nil); err != nil {
		return fmt.Errorf("store: flag the search index for rebuild while sweeping session %s: %w; no row was deleted", sessionID, err)
	}
	return nil
}

// unreferencedBlobDigests lists up to limit content blobs of the session
// that no generation content descriptor references, through the descriptor
// digest index. The deferred non-native move will add a second referrer;
// until then the descriptor table is the only one.
func (s *Store) unreferencedBlobDigests(ctx context.Context, sessionID schema.SessionID, limit int) ([]string, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection to list unreferenced blobs for session %s: %w; no row was deleted", sessionID, err)
	}
	defer s.pool.Put(conn)
	var digests []string
	err = sqlitex.ExecuteTransient(conn, `SELECT c.digest FROM session_content c WHERE c.session_id = ? AND NOT EXISTS (SELECT 1 FROM session_generation_content d WHERE d.session_id = c.session_id AND d.digest = c.digest) LIMIT ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), int64(limit)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			digests = append(digests, stmt.ColumnText(0))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: list unreferenced blobs for session %s: %w; no row was deleted", sessionID, err)
	}
	return digests, nil
}

// deleteUnreferencedBlobs deletes one bounded batch of unreferenced blob
// objects in one transaction. The chunks cascade from the header, so one
// statement per object removes the whole object. A referenced blob aborts
// the batch through its foreign key, like a referenced body.
func (s *Store) deleteUnreferencedBlobs(ctx context.Context, sessionID schema.SessionID, digests []string) (int64, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: take connection to delete %d blobs for session %s: %w; no row was deleted", len(digests), sessionID, err)
	}
	defer s.pool.Put(conn)
	var deleted int64
	txnErr := error(nil)
	endFn := sqlitex.Transaction(conn)
	defer endFn(&txnErr)
	for _, digest := range digests {
		if err := ctx.Err(); err != nil {
			txnErr = err
			return deleted, txnErr
		}
		if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_content WHERE session_id = ? AND digest = ?`, &sqlitex.ExecOptions{
			Args: []any{string(sessionID), digest},
		}); err != nil {
			txnErr = fmt.Errorf("store: delete blob %s for session %s: %w; the batch rolled back and every blob is unchanged (a referenced blob refuses through its foreign key: the sweep never deletes live content)", digest, sessionID, err)
			return deleted, txnErr
		}
		deleted += int64(conn.Changes())
	}
	return deleted, nil
}

// rebuildSearchIndexWhenFlagged runs the whole-index rebuild when the
// store-global health flag is set (by this pass or by the commit that
// preceded it), in one transaction, and reports whether it ran. FTS5 cannot
// remove postings by rowid without the original values, so the rebuild is
// whole-index, not per-row; it supersedes any pending per-row doubt.
func (s *Store) rebuildSearchIndexWhenFlagged(ctx context.Context, sessionID schema.SessionID) (bool, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return false, fmt.Errorf("store: take connection to check the search health for session %s: %w; the sweep flag is unchanged", sessionID, err)
	}
	defer s.pool.Put(conn)
	needsRebuild := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT needs_rebuild FROM session_search_state WHERE id = 1`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			needsRebuild = stmt.ColumnInt64(0) == 1
			return nil
		},
	}); err != nil {
		return false, fmt.Errorf("store: read the search health while sweeping session %s: %w; the sweep flag is unchanged", sessionID, err)
	}
	if !needsRebuild {
		return false, nil
	}
	txnErr := error(nil)
	endFn := sqlitex.Transaction(conn)
	defer endFn(&txnErr)
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_search_fts(session_search_fts) VALUES('rebuild')`, nil); err != nil {
		txnErr = fmt.Errorf("store: rebuild the search index while sweeping session %s: %w; the health flag stays set and the sweep flag is unchanged", sessionID, err)
		return false, txnErr
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_search_state SET needs_rebuild = 0 WHERE id = 1`, nil); err != nil {
		txnErr = fmt.Errorf("store: clear the search health while sweeping session %s: %w; the health flag stays set and the sweep flag is unchanged", sessionID, err)
		return false, txnErr
	}
	return true, nil
}

// removeLeftoverGenerationDirs removes every generation directory of the
// session that no live file-backed generation row requires (design §4.5
// step 2, Release N only). A file-backed active generation keeps its
// directory; a harmonized session keeps none, including the directory named
// after its moved generation. Removal runs through the ownership-verified
// cleanup path, so an unowned directory is refused with an actionable error
// instead of deleted. A missing directory counts as success.
//
// Unlike reclaim's standalone contract, the harvest path takes no pending
// intent guard: a leftover intent is an orphan the sweep discards (the
// migration drain owns the intent files themselves).
func (s *Store) removeLeftoverGenerationDirs(ctx context.Context, sessionID schema.SessionID, active string) (int64, error) {
	if s.generationArtifacts == nil {
		return 0, nil
	}
	dirs, err := s.generationArtifacts.ListGenerationDirectories(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("store: list generation directories while sweeping session %s: %w; no directory was removed", sessionID, err)
	}
	var removed int64
	for _, generationID := range dirs {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if generationID == active {
			continue
		}
		if err := s.removeLeftoverGenerationDir(ctx, sessionID, generationID, active); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// removeLeftoverGenerationDir removes one non-active generation directory
// with the lock already held: it re-checks the read authority, proves
// ownership (a committed file-backed row or a valid staged manifest naming
// this exact session and generation), and removes the directory through the
// root-confined no-follow path. An unowned directory refuses instead of
// deleting: the error names the session and generation, what blocked the
// removal, and that the next pass retries it.
func (s *Store) removeLeftoverGenerationDir(ctx context.Context, sessionID schema.SessionID, generationID, active string) error {
	if generationID == active {
		return fmt.Errorf("store: refuse to remove generation %s for session %s while sweeping: it is the active read authority; select an inactive generation or leave managed recovery to replace it", generationID, sessionID)
	}
	current, err := s.activeGenerationID(ctx, sessionID)
	if err != nil {
		return err
	}
	if current == generationID {
		return fmt.Errorf("store: refuse to remove generation %s for session %s while sweeping: it became the active read authority during the sweep; the directory was left in place", generationID, sessionID)
	}
	if err := s.verifyOwnedInactiveGeneration(ctx, sessionID, generationID); err != nil {
		return err
	}
	if err := s.generationArtifacts.RemoveGeneration(ctx, sessionID, generationID); err != nil {
		return fmt.Errorf("store: remove leftover generation %s for session %s while sweeping: %w; the directory was left in place and the next pass retries the removal", generationID, sessionID, err)
	}
	return nil
}

// clearSweepFlag clears the session's crash marker after steps 1-4 (and the
// step-5 rebuild) succeed. It reads before writing, so a clean session pays
// no write. A crash before this write leaves the flag set with the new
// generation active, and the next pass resumes the sweep.
func (s *Store) clearSweepFlag(ctx context.Context, sessionID schema.SessionID) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection to clear the sweep flag for session %s: %w; the flag stays set", sessionID, err)
	}
	defer s.pool.Put(conn)
	flagged := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT content_sweep_pending FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			flagged = stmt.ColumnInt64(0) == 1
			return nil
		},
	}); err != nil {
		return fmt.Errorf("store: read the sweep flag for session %s: %w; the flag stays set", sessionID, err)
	}
	if !flagged {
		return nil
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET content_sweep_pending = 0 WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
	}); err != nil {
		return fmt.Errorf("store: clear the sweep flag for session %s: %w; the flag stays set and the next pass resumes the sweep", sessionID, err)
	}
	return nil
}

// SweepFlaggedSessions sweeps every session the crash marker flags: the
// harvest-start recovery pass (design §4.5). It selects through the partial
// index instead of scanning every session, sweeps each flagged session under
// its own exclusive lock, and continues past per-session failures, reporting
// them as warnings. A warned session keeps its flag, so the next pass
// retries it. Unflagged sessions are never touched.
func (s *Store) SweepFlaggedSessions(ctx context.Context) (SweepFlaggedReport, error) {
	var report SweepFlaggedReport
	if err := s.requireGenerationSupport(); err != nil {
		return report, err
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return report, fmt.Errorf("store: take connection to list flagged sessions for the harvest-start sweep: %w; no session was swept", err)
	}
	var flagged []schema.SessionID
	listErr := sqlitex.ExecuteTransient(conn, `SELECT session_id FROM sessions WHERE content_sweep_pending = 1 ORDER BY session_id`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			flagged = append(flagged, schema.SessionID(stmt.ColumnText(0)))
			return nil
		},
	})
	s.pool.Put(conn)
	if listErr != nil {
		return report, fmt.Errorf("store: list flagged sessions for the harvest-start sweep: %w; no session was swept", listErr)
	}
	for _, sessionID := range flagged {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		result, err := s.SweepSession(ctx, sessionID)
		if err != nil {
			report.Warnings = append(report.Warnings, fmt.Errorf("store: harvest-start sweep skipped session %s: %w; its flag stays set and the next pass retries it", sessionID, err))
			continue
		}
		report.Sessions = append(report.Sessions, SweepSessionReport{SessionID: sessionID, Result: result})
	}
	return report, nil
}
