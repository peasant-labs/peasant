package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// SQL constants for familiarity data access.
const (
	sqlUpsertSessionFile = `INSERT INTO session_files (session_id, file_path, interaction, turn_count, human_turns)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(session_id, file_path) DO UPDATE SET
    interaction = CASE
        WHEN excluded.interaction > session_files.interaction THEN excluded.interaction
        ELSE session_files.interaction
    END,
    turn_count = excluded.turn_count,
    human_turns = excluded.human_turns`

	sqlRefreshFileFamiliarity = `INSERT INTO file_familiarity (project_hash, file_path, familiarity_depth, session_count, total_turns, total_human_turns, first_engaged_at, last_engaged_at)
SELECT
    s.project_hash,
    sf.file_path,
    0,
    COUNT(DISTINCT sf.session_id),
    SUM(sf.turn_count),
    SUM(sf.human_turns),
    MIN(datetime(s.start_ms / 1000, 'unixepoch')),
    MAX(datetime(s.end_ms / 1000, 'unixepoch'))
FROM session_files sf
JOIN sessions s ON s.session_id = sf.session_id
WHERE s.project_hash = ?
GROUP BY s.project_hash, sf.file_path
ON CONFLICT(project_hash, file_path) DO UPDATE SET
    session_count = excluded.session_count,
    total_turns = excluded.total_turns,
    total_human_turns = excluded.total_human_turns,
    first_engaged_at = excluded.first_engaged_at,
    last_engaged_at = excluded.last_engaged_at`

	sqlGetFileFamiliarity = `SELECT
    file_path, familiarity_depth, session_count, total_turns, total_human_turns,
    first_engaged_at, last_engaged_at
FROM file_familiarity
WHERE project_hash = ?
ORDER BY last_engaged_at DESC NULLS LAST, file_path`

	sqlGetSessionFilesForProject = `SELECT
    sf.session_id, sf.file_path, sf.interaction, sf.turn_count, sf.human_turns
FROM session_files sf
JOIN sessions s ON s.session_id = sf.session_id
WHERE s.project_hash = ?
ORDER BY s.start_ms DESC`

	sqlGetMaxInteractionForFile = `SELECT
    MAX(sf.interaction) AS max_interaction
FROM session_files sf
JOIN sessions s ON s.session_id = sf.session_id
WHERE s.project_hash = ? AND sf.file_path = ?`

	sqlUpdateFamiliarityDepth = `UPDATE file_familiarity SET familiarity_depth = ? WHERE project_hash = ? AND file_path = ?`
)

// SessionFileRow represents a row in the session_files table.
type SessionFileRow struct {
	SessionID   string
	FilePath    string
	Interaction string
	TurnCount   int
	HumanTurns  int
}

// FileFamiliarityRow represents a row in the file_familiarity table.
type FileFamiliarityRow struct {
	FilePath         string
	FamiliarityDepth int
	SessionCount     int
	TotalTurns       int
	TotalHumanTurns  int
	FirstEngagedAt   *string
	LastEngagedAt    *string
}

// UpsertSessionFiles inserts or updates session_files rows for a session.
func (s *Store) UpsertSessionFiles(ctx context.Context, sessionID string, files []SessionFileRow) (err error) {
	conn, connErr := s.pool.Take(ctx)
	if connErr != nil {
		return fmt.Errorf("store: take connection: %w", connErr)
	}
	defer s.pool.Put(conn)

	defer sqlitex.Transaction(conn)(&err)

	for _, f := range files {
		err = sqlitex.ExecuteTransient(conn, sqlUpsertSessionFile, &sqlitex.ExecOptions{
			Args: []any{sessionID, f.FilePath, f.Interaction, f.TurnCount, f.HumanTurns},
		})
		if err != nil {
			return fmt.Errorf("store: upsert session file %s/%s: %w", sessionID, f.FilePath, err)
		}
	}
	return nil
}

// RefreshFileFamiliarity aggregates session_files into file_familiarity for a project.
// Uses a single connection throughout to avoid pool deadlocks.
func (s *Store) RefreshFileFamiliarity(ctx context.Context, projectHash string) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	// Step 1: Aggregate session_files into file_familiarity.
	err = sqlitex.ExecuteTransient(conn, sqlRefreshFileFamiliarity, &sqlitex.ExecOptions{
		Args: []any{projectHash},
	})
	if err != nil {
		return fmt.Errorf("store: refresh file familiarity: %w", err)
	}

	// Step 2: Read back the rows using the same connection.
	rows, err := getFileFamiliarityWithConn(conn, projectHash)
	if err != nil {
		return fmt.Errorf("store: get file familiarity for depth update: %w", err)
	}

	// Step 3: Update familiarity_depth for each file based on interaction patterns.
	for _, row := range rows {
		maxInteraction := schema.InteractionMentioned
		err = sqlitex.ExecuteTransient(conn, sqlGetMaxInteractionForFile, &sqlitex.ExecOptions{
			Args: []any{projectHash, row.FilePath},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				if stmt.ColumnType(0) != sqlite.TypeNull {
					maxInteraction = schema.InteractionType(stmt.ColumnText(0))
				}
				return nil
			},
		})
		if err != nil {
			continue // best-effort
		}

		depth := computeFamiliarityDepthFromRow(row.SessionCount, row.TotalTurns, row.TotalHumanTurns, maxInteraction)
		err = sqlitex.ExecuteTransient(conn, sqlUpdateFamiliarityDepth, &sqlitex.ExecOptions{
			Args: []any{depth, projectHash, row.FilePath},
		})
		if err != nil {
			continue // best-effort
		}
	}

	return nil
}

// computeFamiliarityDepthFromRow computes the 0-3 familiarity depth for a file.
func computeFamiliarityDepthFromRow(sessionCount, totalTurns, totalHumanTurns int, maxInteraction schema.InteractionType) int {
	_ = totalTurns // reserved for future heuristics
	if sessionCount == 0 {
		return 0
	}
	if totalHumanTurns >= 5 {
		return 3
	}
	if sessionCount >= 2 && maxInteraction == schema.InteractionQuestioned {
		return 3
	}
	if totalHumanTurns >= 2 || maxInteraction == schema.InteractionDiscussed || maxInteraction == schema.InteractionQuestioned {
		return 2
	}
	return 1
}

// getFileFamiliarityWithConn reads familiarity rows using the provided connection.
// This avoids pool deadlocks when called from RefreshFileFamiliarity which already holds a connection.
func getFileFamiliarityWithConn(conn *sqlite.Conn, projectHash string) ([]FileFamiliarityRow, error) {
	var rows []FileFamiliarityRow
	err := sqlitex.ExecuteTransient(conn, sqlGetFileFamiliarity, &sqlitex.ExecOptions{
		Args: []any{projectHash},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row := FileFamiliarityRow{
				FilePath:         stmt.ColumnText(0),
				FamiliarityDepth: stmt.ColumnInt(1),
				SessionCount:     stmt.ColumnInt(2),
				TotalTurns:       stmt.ColumnInt(3),
				TotalHumanTurns:  stmt.ColumnInt(4),
			}
			if stmt.ColumnType(5) != sqlite.TypeNull {
				v := stmt.ColumnText(5)
				row.FirstEngagedAt = &v
			}
			if stmt.ColumnType(6) != sqlite.TypeNull {
				v := stmt.ColumnText(6)
				row.LastEngagedAt = &v
			}
			rows = append(rows, row)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: get file familiarity for %s: %w", projectHash, err)
	}
	return rows, nil
}

// GetFileFamiliarity reads aggregated familiarity data for a project.
func (s *Store) GetFileFamiliarity(ctx context.Context, projectHash string) ([]FileFamiliarityRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	return getFileFamiliarityWithConn(conn, projectHash)
}

// GetSessionFilesForProject returns all session_files rows for sessions in a project.
func (s *Store) GetSessionFilesForProject(ctx context.Context, projectHash string) ([]SessionFileRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var rows []SessionFileRow
	err = sqlitex.ExecuteTransient(conn, sqlGetSessionFilesForProject, &sqlitex.ExecOptions{
		Args: []any{projectHash},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, SessionFileRow{
				SessionID:   stmt.ColumnText(0),
				FilePath:    stmt.ColumnText(1),
				Interaction: stmt.ColumnText(2),
				TurnCount:   stmt.ColumnInt(3),
				HumanTurns:  stmt.ColumnInt(4),
			})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: get session files for %s: %w", projectHash, err)
	}
	return rows, nil
}
