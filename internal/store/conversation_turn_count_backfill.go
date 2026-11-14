package store

import (
	"fmt"
	"log/slog"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// conversationTurnCountProgressEvery is how many folded sessions pass between
// two progress reports of the backfill.
const conversationTurnCountProgressEvery = 100

// ConversationTurnCountBackfillReport is the report of an upgrade fold over
// the stored sessions: the one-time V74 backfill and the V77 turn count
// recount. A session with no stored entries cannot be re-folded, so it is
// named here; re-indexing that session replaces its stored counts with the
// folded ones. The V74 backfill copies such a session's legacy count; the
// recount keeps both stored values.
type ConversationTurnCountBackfillReport struct {
	SessionsUpdated   int
	EntryLessSessions []ingest.SessionID
	// UnreadableSessions were skipped by the recount because their stored
	// entries could not be read. Their stored counts are kept.
	UnreadableSessions []ingest.SessionID
}

// backfillConversationTurnCountsOnConn fills
// session_captured_stats.conversation_turn_count for every session on a store
// that predates V74, including legacy sessions that never had a captured
// stats row. It folds the stored entries through the same ingest definition
// every reader uses, so the copied number is the conversation count and not
// the retired raw record count. An entry-less session has nothing to fold: it
// keeps its legacy value and is reported for re-indexing.
//
// It runs once, inside the transaction of the schema step that adds the
// column: the upgrade registers it as that step's data fill, so the column
// and its values commit together and a process that dies mid-fold leaves the
// store one version below, with the fold still owed. It opens no transaction
// of its own. report receives the running session count.
func backfillConversationTurnCountsOnConn(conn *sqlite.Conn, report func(done, of int64)) (ConversationTurnCountBackfillReport, error) {
	var filled ConversationTurnCountBackfillReport
	err := foldStoredSessions(conn, "the conversation turn count backfill", report, func(_, _ int, session storedSessionEntries, readErr error) error {
		if readErr != nil {
			if session.managed {
				return fmt.Errorf("store: read the generation snapshot for session %s during the conversation turn count backfill: %w; no count was changed", session.id, readErr)
			}
			return fmt.Errorf("store: read entries for session %s during the conversation turn count backfill: %w; no count was changed", session.id, readErr)
		}
		count := 0
		if len(session.entries) == 0 {
			filled.EntryLessSessions = append(filled.EntryLessSessions, session.id)
			legacy, err := legacySessionTurnCountOnConn(conn, session.id)
			if err != nil {
				return err
			}
			count = legacy
		} else {
			count = ingest.ConversationTurnCount(ingest.EntriesToTurns(session.entries))
		}
		if err := writeConversationTurnCountOnConn(conn, session.id, count); err != nil {
			return err
		}
		filled.SessionsUpdated++
		return nil
	})
	return filled, err
}

// storedSessionEntries is one stored session as an upgrade fold reads it.
type storedSessionEntries struct {
	id ingest.SessionID
	// managed is true when the entries are the main entries of the
	// session's active generation, false when they are its mirror rows.
	managed bool
	entries []schema.SessionEntry
}

// foldStoredSessions is the one walk over the stored sessions that both
// upgrade folds share: the conversation turn count fill of schema step 74 and
// the turn count recount that migration 77 marks as owed. It lists every
// stored session in id order and reads the entries a reader serves for it: a
// managed session's mirror rows clear their source refs and may omit content,
// so its active generation's emitted main entries are read instead; any other
// session's mirror rows are its only stored entries. visit receives each
// session, its 1-based position and the number of sessions, with the error of
// that read, and decides what the fold does with
// it; an error from visit stops the walk. progress receives the number of
// sessions visited every conversationTurnCountProgressEvery sessions and at
// the last one. It opens no transaction: the caller's step owns it. purpose
// names the fold in its errors.
func foldStoredSessions(conn *sqlite.Conn, purpose string, progress func(done, of int64), visit func(done, total int, session storedSessionEntries, readErr error) error) error {
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
		return fmt.Errorf("store: list the stored sessions for %s: %w; no count was changed", purpose, err)
	}

	for i, stored := range sessions {
		session := storedSessionEntries{id: stored.id, managed: stored.active != ""}
		var readErr error
		if session.managed {
			session.entries, readErr = nativeGenerationEntriesOnConn(conn, stored.id, stored.active)
		} else {
			session.entries, readErr = listEntriesOnConn(conn, stored.id)
		}
		done := i + 1
		if err := visit(done, len(sessions), session, readErr); err != nil {
			return err
		}
		if progress != nil && (done%conversationTurnCountProgressEvery == 0 || done == len(sessions)) {
			progress(int64(done), int64(len(sessions)))
		}
	}
	return nil
}

// nativeGenerationEntriesOnConn returns the emitted main entries of a managed
// generation in the reader's shape: source refs and body previews preserved,
// harmonized or file-backed. An empty result means the generation holds no
// emitted entries, which the caller treats as entry-less.
func nativeGenerationEntriesOnConn(conn *sqlite.Conn, sessionID ingest.SessionID, generationID string) ([]schema.SessionEntry, error) {
	harmonized := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM session_generations WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args:       []any{string(sessionID), generationID},
		ResultFunc: func(*sqlite.Stmt) error { harmonized = true; return nil },
	}); err != nil {
		return nil, err
	}
	if harmonized {
		partitions, _, err := harmonizedPartitionsModeOnConn(conn, sessionID, generationID, false)
		if err != nil {
			return nil, err
		}
		return partitions.main.Entries, nil
	}
	partitions, err := readGenerationPartitionsOnConn(conn, sessionID, generationID)
	if err != nil {
		return nil, err
	}
	return partitions.main.Entries, nil
}

// legacySessionTurnCountOnConn reads the count a pre-V74 store already kept:
// the captured raw turn count for a managed session, falling back to the
// legacy analysis column, and zero when neither exists. It is only used for a
// session whose entries are gone, where no fold is possible.
func legacySessionTurnCountOnConn(conn *sqlite.Conn, sessionID ingest.SessionID) (int, error) {
	value := 0
	if err := sqlitex.ExecuteTransient(conn, `SELECT COALESCE(COALESCE(c.turn_count, m.turn_count), 0)
FROM sessions s
LEFT JOIN session_captured_stats c ON c.session_id = s.session_id
LEFT JOIN session_metrics m ON m.session_id = s.session_id
WHERE s.session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			value = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		return 0, fmt.Errorf("store: read the legacy turn count for entry-less session %s during the conversation turn count backfill: %w; no count was changed", sessionID, err)
	}
	return value, nil
}

// writeConversationTurnCountOnConn stores the count, creating the stats row
// when a session has none. A missing row is not invented as fully measured:
// only the count and the row identity are written. An inserted placeholder
// carries updated_at 0 so every later capture or computed update wins the
// stats merge; a count-only insert must never outrank real measurements.
func writeConversationTurnCountOnConn(conn *sqlite.Conn, sessionID ingest.SessionID, count int) error {
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_captured_stats SET conversation_turn_count = ? WHERE session_id = ?`,
		&sqlitex.ExecOptions{Args: []any{count, string(sessionID)}}); err != nil {
		return fmt.Errorf("store: write the conversation turn count for session %s: %w; no count was changed", sessionID, err)
	}
	if conn.Changes() > 0 {
		return nil
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_captured_stats (session_id, conversation_turn_count, source, updated_at_ms)
VALUES (?, ?, 'harness', 0)`, &sqlitex.ExecOptions{Args: []any{string(sessionID), count}}); err != nil {
		return fmt.Errorf("store: create the captured stats row for session %s: %w; no count was changed", sessionID, err)
	}
	return nil
}

// logConversationTurnCountBackfill publishes the one-time upgrade report. An
// entry-less session's copied count is a placeholder fact, so every one names
// its repair rather than passing the copy on silently.
func logConversationTurnCountBackfill(report ConversationTurnCountBackfillReport) {
	if report.SessionsUpdated == 0 {
		return
	}
	slog.Info("store: conversation turn count backfill complete",
		"sessions", report.SessionsUpdated,
		"entry_less", len(report.EntryLessSessions))
	for _, sessionID := range report.EntryLessSessions {
		slog.Warn("store: a session with no foldable entries kept its legacy turn count as the conversation turn count; re-index it to replace the copy with the folded count",
			"session_id", sessionID)
	}
}
