package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// GroupingEvidenceRow is the durable per-session evidence a grouped helper list
// needs. It is a read projection over the EXISTING generation tables:
// sessions.session_purpose is the durable purpose mirror, and
// session_relationship_evidence carries the active generation's started_by
// target. It deliberately derives no identity from content or titles.
type GroupingEvidenceRow struct {
	SessionID string
	// Purpose is the durable session purpose. Empty means the active generation
	// expressed none, which is a legitimate value and never a persistence error.
	Purpose schema.SessionPurpose
	// InputSubmissionCount is the durable measured input count for the active
	// generation. Nil means unknown, which is distinct from a measured zero.
	InputSubmissionCount *int64
	// OwnerState is the started_by relationship target state on the active
	// generation. Empty means no started_by evidence exists at all, which the
	// caller treats as an unresolved owner rather than a missing one.
	OwnerState schema.RelationshipTargetState
	// OwnerTargetLocalID is the immediate started_by target local identifier.
	// It is present only for a known or retained target; it is nil otherwise,
	// including a known-missing owner that never named a local identifier.
	OwnerTargetLocalID *string
}

// GroupingEvidenceForSessions reads the active-generation grouping evidence for
// exactly the named sessions, in bounded IN(...) batches. Sessions that name no
// stored row are omitted; the caller already holds the identifiers and decides
// how to report an unresolved row. A started_by target that names no stored
// session is still reported to the caller: a known-missing owner keeps a stable
// group identity, so this reader never resolves owners through an inner join.
func (s *Store) GroupingEvidenceForSessions(ctx context.Context, sessionIDs []string) (map[string]GroupingEvidenceRow, error) {
	if len(sessionIDs) == 0 {
		return map[string]GroupingEvidenceRow{}, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection for grouping evidence: %w", err)
	}
	defer s.pool.Put(conn)

	evidence := make(map[string]GroupingEvidenceRow, len(sessionIDs))
	for start := 0; start < len(sessionIDs); start += indexFormatReadBatchSize {
		end := min(start+indexFormatReadBatchSize, len(sessionIDs))
		selected := sessionIDs[start:end]
		placeholders := make([]string, len(selected))
		args := make([]any, len(selected))
		for i, id := range selected {
			placeholders[i] = "?"
			args[i] = id
		}
		query := `SELECT s.session_id, s.session_purpose, s.input_submission_count, r.target_state, r.target_local_id
FROM sessions s
LEFT JOIN session_relationship_evidence r
  ON r.session_id = s.session_id
 AND r.kind = 'started_by'
 AND r.generation_id = s.active_generation_id
WHERE s.session_id IN (` + strings.Join(placeholders, ", ") + `)`
		if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
			Args: args,
			ResultFunc: func(stmt *sqlite.Stmt) error {
				row := GroupingEvidenceRow{SessionID: stmt.ColumnText(0)}
				if stmt.ColumnType(1) != sqlite.TypeNull {
					purpose, purposeErr := schema.NewSessionPurpose(stmt.ColumnText(1))
					if purposeErr != nil {
						return fmt.Errorf("stored session purpose for %q is not a published value: %w; run `peasant ingest verify` and repair the session before reading it in a grouped list", row.SessionID, purposeErr)
					}
					row.Purpose = purpose
				}
				if stmt.ColumnType(2) != sqlite.TypeNull {
					count := stmt.ColumnInt64(2)
					row.InputSubmissionCount = &count
				}
				if stmt.ColumnType(3) != sqlite.TypeNull {
					state, stateErr := schema.NewRelationshipTargetState(stmt.ColumnText(3))
					if stateErr != nil {
						return fmt.Errorf("stored started_by target state for %q is not a published value: %w; run `peasant ingest verify` and repair the session before reading it in a grouped list", row.SessionID, stateErr)
					}
					row.OwnerState = state
				}
				if stmt.ColumnType(4) != sqlite.TypeNull && stmt.ColumnText(4) != "" {
					target := stmt.ColumnText(4)
					row.OwnerTargetLocalID = &target
				}
				evidence[row.SessionID] = row
				return nil
			},
		}); err != nil {
			return nil, fmt.Errorf("store: grouping evidence query: %w", err)
		}
	}
	return evidence, nil
}
