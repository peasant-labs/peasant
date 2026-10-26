package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// IngestOutcomeRetainedRuns bounds detailed outcomes to the latest 100 audit
// runs, including runs with no failures. Existing aggregate audit rows are
// never pruned. The bound is a retention contract, not a throughput estimate.
const IngestOutcomeRetainedRuns = 100

// Small transactions prevent a large failed harvest from monopolizing SQLite.
// Detail volume has no per-run cap; atomicity is per batch, not per run.
const ingestOutcomeBatchSize = 128

func (s *Store) writeIngestRunOutcomes(ctx context.Context, conn *sqlite.Conn, runID int64, outcomes []ingest.RunOutcome) error {
	var writeErr error
	for start := 0; start < len(outcomes); start += ingestOutcomeBatchSize {
		end := min(start+ingestOutcomeBatchSize, len(outcomes))
		if err := writeOutcomeBatch(conn, runID, outcomes[start:end]); err != nil {
			writeErr = fmt.Errorf("store: audit run %d exists but its session outcomes could not be fully written: %w; restore database write access and retry harvest", runID, err)
			break
		}
	}
	return errors.Join(writeErr, pruneIngestRunOutcomes(ctx, conn, runID))
}

func pruneIngestRunOutcomes(ctx context.Context, conn *sqlite.Conn, runID int64) error {
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
		reason, reasonErr := ingest.NewAuditReason(string(outcome.ReasonCode))
		if reasonErr != nil {
			return reasonErr
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
		if err := sqlitex.Execute(conn, `INSERT INTO ingest_run_outcomes(run_id, session_id, kind, reason_code, created_at) VALUES(?,?,?,?,?)`, &sqlitex.ExecOptions{Args: []any{runID, sid, string(kind), string(reason), outcome.CreatedAt}}); err != nil {
			return err
		}
	}
	return nil
}

// ListIngestRunOutcomes reads retained per-session failures and diagnostics for
// one ingest_log ID. Empty means no details remain or none were recorded; the
// aggregate row remains available even after detailed retention expires.
// Writes are best-effort and atomic per batch only. A nonempty set may be
// partial; neither an empty nor a populated set certifies run completeness.
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
		reason, err := ingest.NewAuditReason(stmt.ColumnText(3))
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
		outcomes = append(outcomes, ingest.RunOutcome{RunID: stmt.ColumnInt64(0), SessionID: sid, Kind: kind, ReasonCode: reason, CreatedAt: stmt.ColumnInt64(4)})
		return nil
	}})
	if err != nil {
		return nil, fmt.Errorf("store: read outcomes for audit run %d: %w; check the stored audit data and retry", runID, err)
	}
	return outcomes, nil
}
