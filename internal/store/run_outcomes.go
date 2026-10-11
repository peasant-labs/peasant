package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// IngestOutcomeRetainedRuns bounds detailed outcomes to the latest 100 audit
// runs, including runs with no failures. Existing aggregate audit rows are
// never pruned. The bound is a retention contract, not a throughput estimate.
const IngestOutcomeRetainedRuns = 100

// Small transactions prevent a large failed harvest from monopolizing SQLite.
const ingestOutcomeBatchSize = 128

func (s *Store) writeIngestRunOutcomes(ctx context.Context, conn *sqlite.Conn, runID int64, outcomes []ingest.RunOutcome) error {
	for start := 0; start < len(outcomes); start += ingestOutcomeBatchSize {
		end := min(start+ingestOutcomeBatchSize, len(outcomes))
		if err := writeOutcomeBatch(conn, runID, outcomes[start:end]); err != nil {
			return fmt.Errorf("store: audit run %d exists but its session outcomes could not be fully written: %w; restore database write access and retry harvest", runID, err)
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sqlitex.ExecuteTransient(conn, `DELETE FROM ingest_run_outcomes WHERE id IN (
SELECT id FROM ingest_run_outcomes WHERE run_id < (
SELECT id FROM ingest_log ORDER BY id DESC LIMIT 1 OFFSET ?) LIMIT ?)`, &sqlitex.ExecOptions{Args: []any{IngestOutcomeRetainedRuns - 1, ingestOutcomeBatchSize}}); err != nil {
			return fmt.Errorf("store: prune old detailed ingest outcomes after audit run %d: %w; aggregate history is unchanged; retry harvest to resume retention", runID, err)
		}
		if conn.Changes() == 0 {
			return nil
		}
	}
}

func writeOutcomeBatch(conn *sqlite.Conn, runID int64, outcomes []ingest.RunOutcome) (err error) {
	defer sqlitex.Transaction(conn)(&err)
	for _, outcome := range outcomes {
		kind, kindErr := ingest.NewRunOutcomeKind(string(outcome.Kind))
		if kindErr != nil {
			return kindErr
		}
		// Diagnostic reason codes are intentionally open: the producer's
		// ErrorType is the vocabulary, not a second store-owned enum.
		if strings.TrimSpace(outcome.ReasonCode) == "" {
			return fmt.Errorf("record outcome for session %s: missing producer reason code; provide the diagnostic ErrorType or worker error reason", outcome.SessionID)
		}
		var sid any
		if outcome.SessionID != "" {
			if _, err := ingest.NewSessionID(string(outcome.SessionID)); err != nil {
				return err
			}
			sid = string(outcome.SessionID)
		} else if kind == ingest.RunOutcomeWorkerError {
			return fmt.Errorf("record worker failure: missing session identity; provide the failed session ID before writing the audit outcome")
		}
		if err := sqlitex.Execute(conn, `INSERT INTO ingest_run_outcomes(run_id, session_id, kind, reason_code, created_at) VALUES(?,?,?,?,?)`, &sqlitex.ExecOptions{Args: []any{runID, sid, string(kind), outcome.ReasonCode, outcome.CreatedAt}}); err != nil {
			return err
		}
	}
	return nil
}

// ListIngestRunOutcomes reads retained per-session failures and diagnostics for
// one ingest_log ID. Empty means no details remain or none were recorded; the
// aggregate row remains available even after detailed retention expires.
func (s *Store) ListIngestRunOutcomes(ctx context.Context, runID int64) ([]ingest.RunOutcome, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: read outcomes for audit run %d: %w; check database access and retry", runID, err)
	}
	defer s.pool.Put(conn)
	var outcomes []ingest.RunOutcome
	err = sqlitex.ExecuteTransient(conn, `SELECT run_id, session_id, kind, reason_code, created_at FROM ingest_run_outcomes WHERE run_id=? ORDER BY id`, &sqlitex.ExecOptions{Args: []any{runID}, ResultFunc: func(stmt *sqlite.Stmt) error {
		kind, err := ingest.NewRunOutcomeKind(stmt.ColumnText(2))
		if err != nil {
			return err
		}
		var sid ingest.SessionID
		if stmt.ColumnType(1) != sqlite.TypeNull {
			sid, err = ingest.NewSessionID(stmt.ColumnText(1))
			if err != nil {
				return err
			}
		}
		outcomes = append(outcomes, ingest.RunOutcome{RunID: stmt.ColumnInt64(0), SessionID: sid, Kind: kind, ReasonCode: stmt.ColumnText(3), CreatedAt: stmt.ColumnInt64(4)})
		return nil
	}})
	if err != nil {
		return nil, fmt.Errorf("store: read outcomes for audit run %d: %w; check the stored audit data and retry", runID, err)
	}
	return outcomes, nil
}
