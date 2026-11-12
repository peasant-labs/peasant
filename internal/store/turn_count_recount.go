package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// recountTurnCountsOnConn folds every stored session once and writes both
// turn counts: session_captured_stats.turn_count takes the number of folded
// turns and conversation_turn_count takes the folded turns on the contract's
// allowed list. It is the upgrade step that migration 77 marks as owed. It
// runs only while the pending_recounts marker exists and deletes the marker
// in the same transaction as its writes, so a run that fails, is cancelled or
// is killed changes nothing and the next upgrade runs it again. ran reports
// whether the marker was found.
//
// It walks the sessions with foldStoredSessions, the walk the step 74 fill
// also uses. A session with no stored entries has nothing to fold: it keeps
// both stored values and is reported. A session whose entries cannot be read
// is logged, reported and keeps both stored values; it does not stop the
// recount. Running it twice gives the same values. progress receives the
// running session count; hook, nil in production, is called after each
// session.
func recountTurnCountsOnConn(ctx context.Context, conn *sqlite.Conn, progress func(done, of int64), hook recountSessionHook, logger *slog.Logger) (report ConversationTurnCountBackfillReport, ran bool, err error) {
	end, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return report, false, fmt.Errorf("store: begin the turn count recount transaction: %w; no count was changed and the recount stays owed; close other writers of this database and run `peasant migrate --schema-only` again", err)
	}
	defer end(&err)

	// The marker is read under the write lock: another upgrade may have
	// finished the recount since this one was planned.
	pending, err := turnCountRecountPendingOnConn(conn)
	if err != nil || !pending {
		return report, false, err
	}

	err = foldStoredSessions(conn, "the turn count recount", progress, func(done, total int, session storedSessionEntries, readErr error) error {
		switch {
		case readErr != nil:
			report.UnreadableSessions = append(report.UnreadableSessions, session.id)
			logger.Warn("store: a session's stored entries could not be read during the turn count recount; it keeps its stored turn counts; re-index the session to replace them",
				"session_id", session.id, "error", readErr.Error())
		case len(session.entries) == 0:
			report.EntryLessSessions = append(report.EntryLessSessions, session.id)
		default:
			turns := ingest.EntriesToTurns(session.entries)
			if err := writeTurnCountsOnConn(conn, session.id, len(turns), ingest.ConversationTurnCount(turns)); err != nil {
				return err
			}
			report.SessionsUpdated++
		}
		if hook != nil {
			if err := hook(done, session.id); err != nil {
				return fmt.Errorf("store: the turn count recount stopped after %d of %d sessions at session %s: %w; no count was changed and the recount stays owed; run `peasant migrate --schema-only` to run it again", done, total, session.id, err)
			}
		}
		// A cancellation stops between sessions too: the fold of one session
		// is Go work the connection's interrupt does not reach.
		return ctx.Err()
	})
	if err != nil {
		return report, true, err
	}

	if err := sqlitex.ExecuteTransient(conn, `DELETE FROM pending_recounts WHERE name = ?`,
		&sqlitex.ExecOptions{Args: []any{pendingRecountTurnCounts}}); err != nil {
		return report, true, fmt.Errorf("store: clear the turn count recount marker: %w; no count was changed and the recount stays owed; run `peasant migrate --schema-only` to run it again", err)
	}
	// A cancellation that arrived after the last session must not commit.
	return report, true, ctx.Err()
}

// turnCountRecountPendingOnConn reports whether the recount marker exists.
func turnCountRecountPendingOnConn(conn *sqlite.Conn) (bool, error) {
	pending := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM pending_recounts WHERE name = ?`, &sqlitex.ExecOptions{
		Args:       []any{pendingRecountTurnCounts},
		ResultFunc: func(*sqlite.Stmt) error { pending = true; return nil },
	}); err != nil {
		return false, fmt.Errorf("store: read the turn count recount marker from pending_recounts: %w; no count was changed; the store cannot tell whether its turn counts are current; restore the database from a backup, or move it aside and run `peasant harvest` to rebuild it", err)
	}
	return pending, nil
}

// writeTurnCountsOnConn stores both turn counts from one fold, creating the
// stats row when a session has none. A missing row is not invented as fully
// measured: only the two counts and the row identity are written. An inserted
// placeholder carries updated_at 0 so every later capture or computed update
// wins the stats merge; a count-only insert must never outrank real
// measurements.
func writeTurnCountsOnConn(conn *sqlite.Conn, sessionID ingest.SessionID, turnCount, conversationTurnCount int) error {
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_captured_stats SET turn_count = ?, conversation_turn_count = ? WHERE session_id = ?`,
		&sqlitex.ExecOptions{Args: []any{turnCount, conversationTurnCount, string(sessionID)}}); err != nil {
		return fmt.Errorf("store: write the turn counts for session %s: %w; no count was changed; repair the session_captured_stats table and retry", sessionID, err)
	}
	if conn.Changes() > 0 {
		return nil
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_captured_stats (session_id, turn_count, conversation_turn_count, source, updated_at_ms)
VALUES (?, ?, ?, 'harness', 0)`, &sqlitex.ExecOptions{Args: []any{string(sessionID), turnCount, conversationTurnCount}}); err != nil {
		return fmt.Errorf("store: create the captured stats row for session %s while writing its turn counts: %w; no count was changed; repair the session_captured_stats table and retry", sessionID, err)
	}
	return nil
}

// logTurnCountRecount publishes the recount summary. An entry-less or
// unreadable session keeps counts the recount could not confirm, so every one
// names its repair.
func logTurnCountRecount(logger *slog.Logger, report ConversationTurnCountBackfillReport) {
	logger.Info("store: turn count recount complete",
		"sessions_recounted", report.SessionsUpdated,
		"entry_less", len(report.EntryLessSessions),
		"unreadable", len(report.UnreadableSessions))
	for _, sessionID := range report.EntryLessSessions {
		logger.Warn("store: a session with no stored entries kept its stored turn counts; re-index it to replace them with the folded counts",
			"session_id", sessionID)
	}
}
