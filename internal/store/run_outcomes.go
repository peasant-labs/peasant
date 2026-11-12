package store

import (
	"context"
	"errors"
	"fmt"
	"time"

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
		// The write boundary admits only a reason this build defines and still
		// writes. The foreign key refuses an id with no seeded row.
		reason, reasonErr := ingest.OutcomeReasonFromID(int64(outcome.Reason))
		if reasonErr != nil {
			return reasonErr
		}
		var sid any
		if outcome.SessionID != "" {
			if _, err := ingest.NewSessionID(string(outcome.SessionID)); err != nil {
				return err
			}
			sid = string(outcome.SessionID)
		} else if reason.Kind() != ingest.OutcomeKindDiagnostic {
			return fmt.Errorf("store: record %s outcome for audit run %d: missing session identity; only a diagnostic may be run-wide; nothing was written for this batch; provide the session ID before writing the audit outcome", reason.Kind(), runID)
		}
		if err := sqlitex.Execute(conn, `INSERT INTO ingest_run_outcomes(run_id, session_id, reason_id, created_at) VALUES(?,?,?,?)`, &sqlitex.ExecOptions{Args: []any{runID, sid, int64(reason), outcome.CreatedAt}}); err != nil {
			return err
		}
	}
	return nil
}

// ListIngestRunOutcomes reads retained per-session failures, diagnostics and
// refresh rows for one ingest_log ID. Empty means no details remain or none
// were recorded; the aggregate row remains available even after detailed
// retention expires. Writes are best-effort and atomic per batch only. A
// nonempty set may be partial; neither an empty nor a populated set certifies
// run completeness.
//
// A row whose reason id is store-local (at or above
// ingest.OutcomeReasonLocalIDBase) names a code no build lists; it is skipped
// and counted in skipped. A lower id this build does not define is an error.
func (s *Store) ListIngestRunOutcomes(ctx context.Context, runID int64) ([]ingest.RunOutcome, error) {
	outcomes, _, err := s.ListIngestRunOutcomesWithSkipped(ctx, runID)
	return outcomes, err
}

// ListIngestRunOutcomesWithSkipped is ListIngestRunOutcomes that also reports
// how many rows it skipped for carrying a store-local reason id.
func (s *Store) ListIngestRunOutcomesWithSkipped(ctx context.Context, runID int64) (outcomes []ingest.RunOutcome, skipped int, err error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("store: read outcomes for audit run %d: %w; check database access and retry", runID, err)
	}
	defer s.pool.Put(conn)
	err = sqlitex.ExecuteTransient(conn, `SELECT run_id, session_id, reason_id, created_at FROM ingest_run_outcomes WHERE run_id=? ORDER BY id`, &sqlitex.ExecOptions{Args: []any{runID}, ResultFunc: func(stmt *sqlite.Stmt) error {
		id := stmt.ColumnInt64(2)
		if id >= ingest.OutcomeReasonLocalIDBase {
			skipped++
			return nil
		}
		reason, err := ingest.OutcomeReasonFromID(id)
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
		outcomes = append(outcomes, ingest.RunOutcome{RunID: stmt.ColumnInt64(0), SessionID: sid, Reason: reason, CreatedAt: stmt.ColumnInt64(3)})
		return nil
	}})
	if err != nil {
		return nil, 0, fmt.Errorf("store: read outcomes for audit run %d: %w; check the stored audit data and retry", runID, err)
	}
	return outcomes, skipped, nil
}

// IngestRunStageRetainedRuns bounds stored stage rows to the newest audit
// runs. It follows the outcome retention rule: a retention contract, not a
// throughput estimate. No ingest_log or index_log row is deleted.
const IngestRunStageRetainedRuns = IngestOutcomeRetainedRuns

const sqlInsertIngestRunStage = `INSERT INTO ingest_run_stages(run_id, stage, duration_ms, done, total, bytes, lane_wait_ms) VALUES(?,?,?,?,?,?,?)`

// A run has at most one row per stage, so the rows of all runs older than the
// retained window are few: one bounded delete inside the run's transaction.
const sqlPruneIngestRunStages = `DELETE FROM ingest_run_stages WHERE run_id < (
SELECT id FROM ingest_log ORDER BY id DESC LIMIT 1 OFFSET ?)`

// writeIngestRunStages validates and inserts the stage rows of one run, then
// applies the stage retention rule. The caller owns the transaction.
func writeIngestRunStages(conn *sqlite.Conn, runID int64, stages []ingest.IndexProfileStage) error {
	seen := make(map[ingest.Stage]bool, len(stages))
	for _, row := range stages {
		stage, err := ingest.NewStage(string(row.Stage))
		if err != nil {
			return err
		}
		if seen[stage] {
			return fmt.Errorf("store: write stage rows for audit run %d: stage %s appears twice; a run stores one row per stage; nothing was written; fold the stage's segments with IndexProfiler.StageTotals before logging the run", runID, stage)
		}
		seen[stage] = true
		if err := sqlitex.Execute(conn, sqlInsertIngestRunStage, &sqlitex.ExecOptions{Args: []any{
			runID, string(stage), max(row.Duration.Milliseconds(), 0), max(row.Done, 0), max(row.Total, 0), max(row.Bytes, 0), max(row.LaneWait.Milliseconds(), 0),
		}}); err != nil {
			return fmt.Errorf("store: insert stage row %s for audit run %d: %w", stage, runID, err)
		}
	}
	if err := sqlitex.Execute(conn, sqlPruneIngestRunStages, &sqlitex.ExecOptions{Args: []any{IngestRunStageRetainedRuns - 1}}); err != nil {
		return fmt.Errorf("store: prune old stage rows after audit run %d: %w", runID, err)
	}
	return nil
}

// ListIngestRunStages reads the stored profile of one run: one row per stage,
// in stage-name order. Empty means the run had no profiler or its rows fell
// out of the retention window.
func (s *Store) ListIngestRunStages(ctx context.Context, runID int64) ([]ingest.IndexProfileStage, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: read stage rows for audit run %d: %w; check database access and retry", runID, err)
	}
	defer s.pool.Put(conn)
	var stages []ingest.IndexProfileStage
	err = sqlitex.ExecuteTransient(conn, `SELECT stage, duration_ms, done, total, bytes, lane_wait_ms FROM ingest_run_stages WHERE run_id=? ORDER BY stage`, &sqlitex.ExecOptions{Args: []any{runID}, ResultFunc: func(stmt *sqlite.Stmt) error {
		stage, err := ingest.NewStage(stmt.ColumnText(0))
		if err != nil {
			return err
		}
		stages = append(stages, ingest.IndexProfileStage{
			Stage:    stage,
			Duration: time.Duration(stmt.ColumnInt64(1)) * time.Millisecond,
			Done:     stmt.ColumnInt(2),
			Total:    stmt.ColumnInt(3),
			Bytes:    stmt.ColumnInt64(4),
			LaneWait: time.Duration(stmt.ColumnInt64(5)) * time.Millisecond,
		})
		return nil
	}})
	if err != nil {
		return nil, fmt.Errorf("store: read stage rows for audit run %d: %w; check the stored audit data and retry", runID, err)
	}
	return stages, nil
}
