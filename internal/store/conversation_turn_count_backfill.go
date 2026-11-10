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

// ConversationTurnCountBackfillReport is the one-time V74 upgrade report. A
// session with no stored entries cannot be re-folded, so the backfill copies
// its legacy count and names it here; re-indexing that session replaces the
// copy with the conversation count.
type ConversationTurnCountBackfillReport struct {
	SessionsUpdated   int
	EntryLessSessions []ingest.SessionID
}

// backfillConversationTurnCounts fills session_captured_stats.conversation_turn_count
// for every session on a store that predates V74, including legacy sessions
// that never had a captured stats row. It folds the stored entries through the
// same ingest definition every reader uses, so the copied number is the
// conversation count and not the retired raw record count. An entry-less
// session has nothing to fold: it keeps its legacy value and is reported for
// re-indexing.
//
// It runs once, from Open, immediately after the V74 migration. The caller
// decides that from the database's pre-migration user_version, so a later open
// at the head schema never re-folds.
func backfillConversationTurnCounts(pool *sqlitex.Pool) (report ConversationTurnCountBackfillReport, err error) {
	conn, err := pool.Take(context.Background())
	if err != nil {
		return report, fmt.Errorf("store: take connection for the conversation turn count backfill: %w; no count was changed", err)
	}
	defer pool.Put(conn)

	end := sqlitex.Transaction(conn)
	defer end(&err)

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
		return report, fmt.Errorf("store: list managed sessions for the conversation turn count backfill: %w; no count was changed", err)
	}

	for _, session := range sessions {
		sessionID := session.id
		var entries []schema.SessionEntry
		if session.active != "" {
			// A managed session's mirror rows clear their source refs and may
			// omit content, so folding them undercounts what a reader serves.
			// Fold the generation's emitted main entries instead.
			generationEntries, err := nativeGenerationEntriesOnConn(conn, sessionID, session.active)
			if err != nil {
				return report, fmt.Errorf("store: read the generation snapshot for session %s during the conversation turn count backfill: %w; no count was changed", sessionID, err)
			}
			entries = generationEntries
			if len(entries) == 0 {
				report.EntryLessSessions = append(report.EntryLessSessions, sessionID)
			}
		} else {
			var err error
			entries, err = listEntriesOnConn(conn, sessionID)
			if err != nil {
				return report, fmt.Errorf("store: read entries for session %s during the conversation turn count backfill: %w; no count was changed", sessionID, err)
			}
		}
		count := 0
		if len(entries) == 0 {
			if session.active == "" {
				report.EntryLessSessions = append(report.EntryLessSessions, sessionID)
			}
			count, err = legacySessionTurnCountOnConn(conn, sessionID)
			if err != nil {
				return report, err
			}
		} else {
			count = ingest.ConversationTurnCount(ingest.EntriesToTurns(entries))
		}
		if err := writeConversationTurnCountOnConn(conn, sessionID, count); err != nil {
			return report, err
		}
		report.SessionsUpdated++
	}
	return report, nil
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
	slog.Info("store: conversation turn count backfill complete",
		"sessions", report.SessionsUpdated,
		"entry_less", len(report.EntryLessSessions))
	for _, sessionID := range report.EntryLessSessions {
		slog.Warn("store: a session with no foldable entries kept its legacy turn count as the conversation turn count; re-index it to replace the copy with the folded count",
			"session_id", sessionID)
	}
}
