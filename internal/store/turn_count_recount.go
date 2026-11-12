package store

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// turnCountRecountProgressInterval is how many sessions the recount folds
// between two progress lines.
const turnCountRecountProgressInterval = 500

// turnCountRecountHook is called after each session the recount handles, with
// the number of sessions handled so far. Production passes nil. A test returns
// an error from it to stop the recount part way, the way a killed first open
// would.
type turnCountRecountHook func(done int, sessionID ingest.SessionID) error

// recountTurnCounts folds every stored session once and writes both turn
// counts: session_captured_stats.turn_count takes the number of folded turns
// and conversation_turn_count takes the folded turns on the contract's allowed
// list. It runs only while the pending_recounts marker exists and deletes the
// marker in the same transaction as its writes, so an interrupted run changes
// nothing and is repeated by the next open. ran reports whether the marker was
// found.
//
// A managed session folds its generation's main entries; any other session
// folds its mirror rows. A session with no stored entries has nothing to fold:
// it keeps both stored values and is reported. A session whose entries cannot
// be read is logged, reported, keeps both stored values and does not stop the
// store from opening. Running it twice gives the same values.
func recountTurnCounts(pool *sqlitex.Pool, hook turnCountRecountHook, logger *slog.Logger) (report ConversationTurnCountBackfillReport, ran bool, err error) {
	conn, err := pool.Take(context.Background())
	if err != nil {
		return report, false, fmt.Errorf("store: take connection for the turn count recount: %w; no count was changed and the recount stays pending; retry when database access is available", err)
	}
	defer pool.Put(conn)

	pending, err := turnCountRecountPendingOnConn(conn)
	if err != nil {
		return report, false, err
	}
	if !pending {
		return report, false, nil
	}

	end, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return report, false, fmt.Errorf("store: begin the turn count recount transaction: %w; no count was changed and the recount stays pending; close other writers of this database and reopen it", err)
	}
	defer end(&err)

	// Another process may have finished the recount between the probe and the
	// write lock.
	pending, err = turnCountRecountPendingOnConn(conn)
	if err != nil {
		return report, false, err
	}
	if !pending {
		return report, false, nil
	}

	type storedSession struct {
		id     ingest.SessionID
		active string
	}
	var sessions []storedSession
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT session_id, COALESCE(active_generation_id, '') FROM sessions ORDER BY session_id`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
			sessions = append(sessions, storedSession{ingest.SessionID(stmt.ColumnText(0)), stmt.ColumnText(1)})
			return nil
		}}); err != nil {
		return report, true, fmt.Errorf("store: list sessions for the turn count recount: %w; no count was changed and the recount stays pending; repair the sessions table and reopen the store", err)
	}

	total := len(sessions)
	if total > 0 {
		logger.Info("store: recounting turn counts for every stored session; this runs once after the upgrade", "sessions", total)
	}
	for i, session := range sessions {
		entries, readErr := recountSessionEntriesOnConn(conn, session.id, session.active)
		switch {
		case readErr != nil:
			report.UnreadableSessions = append(report.UnreadableSessions, session.id)
			logger.Warn("store: a session's stored entries could not be read during the turn count recount; it keeps its stored turn counts; re-index the session to replace them",
				"session_id", session.id, "error", readErr.Error())
		case len(entries) == 0:
			report.EntryLessSessions = append(report.EntryLessSessions, session.id)
		default:
			turns := ingest.EntriesToTurns(entries)
			if err := writeTurnCountsOnConn(conn, session.id, len(turns), ingest.ConversationTurnCount(turns)); err != nil {
				return report, true, err
			}
			report.SessionsUpdated++
		}
		done := i + 1
		if hook != nil {
			if err := hook(done, session.id); err != nil {
				return report, true, fmt.Errorf("store: the turn count recount stopped after %d of %d sessions at session %s: %w; no count was changed and the recount stays pending; reopen the store to run it again", done, total, session.id, err)
			}
		}
		if done%turnCountRecountProgressInterval == 0 && done < total {
			logger.Info("store: turn count recount in progress", "sessions_done", done, "sessions_total", total)
		}
	}

	if err := sqlitex.ExecuteTransient(conn, `DELETE FROM pending_recounts WHERE name = ?`,
		&sqlitex.ExecOptions{Args: []any{pendingRecountTurnCounts}}); err != nil {
		return report, true, fmt.Errorf("store: clear the turn count recount marker: %w; no count was changed and the recount stays pending; reopen the store to run it again", err)
	}
	return report, true, nil
}

// turnCountRecountPendingOnConn reports whether the recount marker exists.
func turnCountRecountPendingOnConn(conn *sqlite.Conn) (bool, error) {
	pending := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM pending_recounts WHERE name = ?`, &sqlitex.ExecOptions{
		Args:       []any{pendingRecountTurnCounts},
		ResultFunc: func(*sqlite.Stmt) error { pending = true; return nil },
	}); err != nil {
		return false, fmt.Errorf("store: read the turn count recount marker from pending_recounts: %w; no count was changed; the store cannot tell whether its turn counts are current; repair the database and reopen it", err)
	}
	return pending, nil
}

// recountSessionEntriesOnConn reads the entries the recount folds: the
// generation's emitted main entries for a managed session, the mirror rows
// otherwise.
func recountSessionEntriesOnConn(conn *sqlite.Conn, sessionID ingest.SessionID, activeGeneration string) ([]schema.SessionEntry, error) {
	if activeGeneration != "" {
		return nativeGenerationEntriesOnConn(conn, sessionID, activeGeneration)
	}
	return listEntriesOnConn(conn, sessionID)
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
