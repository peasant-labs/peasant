package store

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// ContributionHistoryError carries a schema-owned selected-category conflict,
// not another account's rows. Status is 400 or 409 for caller classification.
type ContributionHistoryError struct {
	Status   int
	Conflict *schema.LocalContributionTraversalConflict
}

const observationOrderedTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func (e *ContributionHistoryError) Error() string {
	if e.Status == 409 {
		return "contribution history changed for the selected category; restart its pager using currentTraversalRevision"
	}
	return "contribution history query is malformed; use a known kind, page >= 1, limit 1..200 and a valid revision after page one"
}

func evidenceColumns(kind schema.LocalContributionEvidenceKind) (generation, coverage, table, order string) {
	switch kind {
	case schema.LocalContributionEvidenceRemoteAttempts:
		return "attempt_generation", "attempt_coverage", "remote_contribution_attempts", "collective_id,transcript_id,event_num"
	case schema.LocalContributionEvidenceReceipts:
		return "receipt_generation", "receipt_coverage", "local_contribution_receipts", "confirmed_at,receipt_id"
	case schema.LocalContributionEvidenceOperationDiagnostics:
		return "diagnostic_generation", "diagnostic_coverage", "local_operation_diagnostics", "attempted_at,operation_id"
	}
	return "", "", "", ""
}

func (s *Store) ensureObservationScope(ctx context.Context, snapshot auth.CredentialSnapshot) error {
	base, user, err := observationCredentialScope(snapshot)
	if err != nil {
		return err
	}
	found := false
	err = s.withObservationTransaction(ctx, snapshot, false, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn, `SELECT 1 FROM collective_observation_scopes WHERE village_url=? AND user_id=?`, &sqlitex.ExecOptions{Args: []any{base, user}, ResultFunc: func(*sqlite.Stmt) error { found = true; return nil }})
	})
	if err != nil || found {
		return err
	}
	_, err = s.BeginCollectiveObservation(ctx, snapshot)
	return err
}

func bumpEvidence(conn *sqlite.Conn, base, user string, kind schema.LocalContributionEvidenceKind) error {
	generation, _, _, _ := evidenceColumns(kind)
	if err := sqlitex.Execute(conn, `UPDATE collective_observation_scopes SET `+generation+`=`+generation+`+1,cache_revision=cache_revision+1 WHERE village_url=? AND user_id=? AND `+generation+`<9223372036854775807 AND cache_revision<9223372036854775807`, &sqlitex.ExecOptions{Args: []any{base, user}}); err != nil {
		return err
	}
	if conn.Changes() != 1 {
		return fmt.Errorf("advance contribution category: scope disappeared or durable generation exhausted; transaction was not installed; recreate the selected cache before retrying")
	}
	return nil
}

// RecordContributionReceipt records an actual confirmed effect supplied by the
// action adapter. It does not infer approval, allocate a remote ordinal, or
// turn a failed/uncertain operation into a successful contribution.
func (s *Store) RecordContributionReceipt(ctx context.Context, snapshot auth.CredentialSnapshot, r schema.LocalConfirmedContributionReceipt) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if err := s.ensureObservationScope(ctx, snapshot); err != nil {
		return err
	}
	base, user, err := observationCredentialScope(snapshot)
	if err != nil {
		return err
	}
	var collective, source any
	if r.CollectiveID != nil {
		collective = r.CollectiveID.String()
	}
	if r.SourceReceiptID != nil {
		source = *r.SourceReceiptID
	}
	return s.withObservationTransaction(ctx, snapshot, true, func(conn *sqlite.Conn) error {
		err := sqlitex.Execute(conn, `INSERT INTO local_contribution_receipts(village_url,user_id,receipt_id,operation,transcript_id,collective_id,confirmed_at,outcome,source_receipt_id) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(village_url,user_id,receipt_id) DO UPDATE SET operation=excluded.operation,transcript_id=excluded.transcript_id,collective_id=excluded.collective_id,confirmed_at=excluded.confirmed_at,outcome=excluded.outcome,source_receipt_id=excluded.source_receipt_id WHERE operation IS NOT excluded.operation OR transcript_id IS NOT excluded.transcript_id OR collective_id IS NOT excluded.collective_id OR confirmed_at IS NOT excluded.confirmed_at OR outcome IS NOT excluded.outcome OR source_receipt_id IS NOT excluded.source_receipt_id`, &sqlitex.ExecOptions{Args: []any{base, user, r.ReceiptID, string(r.Operation), r.TranscriptID.String(), collective, r.ConfirmedAt.UTC().Format(observationOrderedTimeLayout), string(r.Outcome), source}})
		if err != nil {
			return err
		}
		if conn.Changes() == 0 {
			return nil
		}
		return bumpEvidence(conn, base, user, schema.LocalContributionEvidenceReceipts)
	})
}

// RecordOperationDiagnostic uses the caller's stable local operation UUID.
// Replaying equivalent diagnostics is idempotent and cannot starve other kinds.
func (s *Store) RecordOperationDiagnostic(ctx context.Context, snapshot auth.CredentialSnapshot, d schema.LocalOperationDiagnostic) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if err := s.ensureObservationScope(ctx, snapshot); err != nil {
		return err
	}
	base, user, err := observationCredentialScope(snapshot)
	if err != nil {
		return err
	}
	var transcript, collective any
	if d.TranscriptID != nil {
		transcript = d.TranscriptID.String()
	}
	if d.CollectiveID != nil {
		collective = d.CollectiveID.String()
	}
	return s.withObservationTransaction(ctx, snapshot, true, func(conn *sqlite.Conn) error {
		err := sqlitex.Execute(conn, `INSERT INTO local_operation_diagnostics(village_url,user_id,operation_id,operation,transcript_id,collective_id,attempted_at,outcome,issue_code,issue_message,issue_retry) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(village_url,user_id,operation_id) DO UPDATE SET operation=excluded.operation,transcript_id=excluded.transcript_id,collective_id=excluded.collective_id,attempted_at=excluded.attempted_at,outcome=excluded.outcome,issue_code=excluded.issue_code,issue_message=excluded.issue_message,issue_retry=excluded.issue_retry WHERE operation IS NOT excluded.operation OR transcript_id IS NOT excluded.transcript_id OR collective_id IS NOT excluded.collective_id OR attempted_at IS NOT excluded.attempted_at OR outcome IS NOT excluded.outcome OR issue_code IS NOT excluded.issue_code OR issue_message IS NOT excluded.issue_message OR issue_retry IS NOT excluded.issue_retry`, &sqlitex.ExecOptions{Args: []any{base, user, d.OperationID.String(), string(d.Operation), transcript, collective, d.AttemptedAt.UTC().Format(observationOrderedTimeLayout), string(d.Outcome), string(d.Issue.Code), d.Issue.Message, d.Issue.Retry}})
		if err != nil {
			return err
		}
		if conn.Changes() == 0 {
			return nil
		}
		return bumpEvidence(conn, base, user, schema.LocalContributionEvidenceOperationDiagnostics)
	})
}

// SetContributionCoverage is a complete-source coverage observation, not a
// transient fetch-error marker. Failed reads must retain previous coverage.
func (s *Store) SetContributionCoverage(ctx context.Context, snapshot auth.CredentialSnapshot, kind schema.LocalContributionEvidenceKind, coverage schema.CollectiveEvidenceCoverage) error {
	if !kind.IsValid() || !coverage.IsValid() {
		return fmt.Errorf("set contribution coverage: unknown category or coverage; no state changed; use the shared schema menus")
	}
	if err := s.ensureObservationScope(ctx, snapshot); err != nil {
		return err
	}
	base, user, err := observationCredentialScope(snapshot)
	if err != nil {
		return err
	}
	_, column, _, _ := evidenceColumns(kind)
	return s.withObservationTransaction(ctx, snapshot, true, func(conn *sqlite.Conn) error {
		if err := sqlitex.Execute(conn, `UPDATE collective_observation_scopes SET `+column+`=? WHERE village_url=? AND user_id=? AND `+column+`<>?`, &sqlitex.ExecOptions{Args: []any{string(coverage), base, user, string(coverage)}}); err != nil {
			return err
		}
		if conn.Changes() == 0 {
			return nil
		}
		return bumpEvidence(conn, base, user, kind)
	})
}

// ReadContributionHistory reads token, coverage, filtered total and bounded
// selected rows in ONE SQLite snapshot. No catalog labels or poll dates join
// the evidence payload, and no overall cache revision participates in paging.
func (s *Store) ReadContributionHistory(ctx context.Context, snapshot auth.CredentialSnapshot, q schema.LocalContributionHistoryQuery) (schema.LocalContributionHistoryResponse, error) {
	var result schema.LocalContributionHistoryResponse
	if !q.Kind.IsValid() {
		return result, &ContributionHistoryError{Status: 400}
	}
	if err := s.ensureObservationScope(ctx, snapshot); err != nil {
		return result, err
	}
	base, user, err := observationCredentialScope(snapshot)
	if err != nil {
		return result, err
	}
	result.Scope, err = schema.NormalizeCollectiveObservationScope(base, schema.VillageUUID(user))
	if err != nil {
		return result, err
	}
	result.Kind, result.Page, result.Limit = q.Kind, q.Page, q.Limit
	generation, coverage, table, order := evidenceColumns(q.Kind)
	err = s.withObservationTransaction(ctx, snapshot, false, func(conn *sqlite.Conn) error {
		var incarnation string
		var rev int64
		var coverageRaw string
		if err := sqlitex.Execute(conn, `SELECT incarnation,`+generation+`,`+coverage+` FROM collective_observation_scopes WHERE village_url=? AND user_id=?`, &sqlitex.ExecOptions{Args: []any{base, user}, ResultFunc: func(stmt *sqlite.Stmt) error {
			incarnation, rev, coverageRaw = stmt.ColumnText(0), stmt.ColumnInt64(1), stmt.ColumnText(2)
			return nil
		}}); err != nil {
			return err
		}
		for _, known := range schema.AllCollectiveEvidenceCoverages {
			if string(known) == coverageRaw {
				result.Coverage = known
			}
		}
		var err error
		result.TraversalRevision, err = schema.EncodeContributionTraversalRevision(schema.ContributionTraversalIdentity{Scope: result.Scope, Incarnation: schema.VillageUUID(incarnation), Kind: q.Kind, Generation: strconv.FormatInt(rev, 10)})
		if err != nil {
			return err
		}
		if status := schema.ValidateContributionHistoryQuery(q, result.TraversalRevision); status != 200 {
			failure := &ContributionHistoryError{Status: status}
			if status == 409 {
				failure.Conflict = &schema.LocalContributionTraversalConflict{CurrentTraversalRevision: result.TraversalRevision, Issue: schema.OperationIssue{Code: schema.OperationIssueSuperseded, Operation: schema.CollectiveOperationFetch, Message: "The selected evidence payload, coverage or scope changed during paging.", Retry: "Restart this category at page one using currentTraversalRevision."}}
			}
			return failure
		}
		where := ` WHERE village_url=? AND user_id=?`
		args := []any{base, user}
		if q.CollectiveID != nil {
			where += ` AND collective_id=?`
			args = append(args, q.CollectiveID.String())
		}
		if err := sqlitex.Execute(conn, `SELECT count(*) FROM `+table+where, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error { result.Total = stmt.ColumnInt64(0); return nil }}); err != nil {
			return err
		}
		// Avoid offset multiplication overflow for valid JSON-safe huge pages.
		offset := result.Total
		if int64(q.Page-1) <= result.Total/int64(q.Limit) {
			offset = int64(q.Page-1) * int64(q.Limit)
		}
		args = append(args, q.Limit, offset)
		selectRows := ` FROM ` + table + where + ` ORDER BY ` + order + ` LIMIT ? OFFSET ?`
		switch q.Kind {
		case schema.LocalContributionEvidenceRemoteAttempts:
			result.RemoteAttempts = []schema.LocalRemoteContributionAttempt{}
			return sqlitex.Execute(conn, `SELECT collective_id,transcript_id,event_num,status,recorded_at,decided_at,decided_by_actor,first_seen_at`+selectRows, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
				row := schema.LocalRemoteContributionAttempt{CollectiveID: schema.VillageUUID(stmt.ColumnText(0)), TranscriptID: schema.TranscriptID(stmt.ColumnText(1)), Attempt: schema.VillageShareEvent{EventNum: int32(stmt.ColumnInt64(2))}}
				for _, known := range schema.AllVillageShareStatuses {
					if string(known) == stmt.ColumnText(3) {
						row.Attempt.Status = known
					}
				}
				var err error
				row.Attempt.RecordedAt, err = time.Parse(time.RFC3339Nano, stmt.ColumnText(4))
				if err != nil {
					return err
				}
				if stmt.ColumnType(5) != sqlite.TypeNull {
					at, err := time.Parse(time.RFC3339Nano, stmt.ColumnText(5))
					if err != nil {
						return err
					}
					row.Attempt.DecidedAt = &at
				}
				for _, known := range schema.AllVillageShareEventActors {
					if string(known) == stmt.ColumnText(6) {
						row.Attempt.DecidedByActor = known
					}
				}
				row.FirstSeenAt, err = time.Parse(time.RFC3339Nano, stmt.ColumnText(7))
				if err != nil {
					return err
				}
				if err := row.Validate(); err != nil {
					return err
				}
				result.RemoteAttempts = append(result.RemoteAttempts, row)
				return nil
			}})
		case schema.LocalContributionEvidenceReceipts:
			result.Receipts = []schema.LocalConfirmedContributionReceipt{}
			return sqlitex.Execute(conn, `SELECT receipt_id,operation,transcript_id,collective_id,confirmed_at,outcome,source_receipt_id`+selectRows, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
				row := schema.LocalConfirmedContributionReceipt{ReceiptID: stmt.ColumnText(0), TranscriptID: schema.TranscriptID(stmt.ColumnText(2))}
				for _, known := range schema.AllCollectiveOperations {
					if string(known) == stmt.ColumnText(1) {
						row.Operation = known
					}
				}
				if stmt.ColumnType(3) != sqlite.TypeNull {
					id := schema.VillageUUID(stmt.ColumnText(3))
					row.CollectiveID = &id
				}
				var err error
				row.ConfirmedAt, err = time.Parse(time.RFC3339Nano, stmt.ColumnText(4))
				if err != nil {
					return err
				}
				for _, known := range schema.AllLocalConfirmedContributionOutcomes {
					if string(known) == stmt.ColumnText(5) {
						row.Outcome = known
					}
				}
				if stmt.ColumnType(6) != sqlite.TypeNull {
					id := stmt.ColumnText(6)
					row.SourceReceiptID = &id
				}
				if err := row.Validate(); err != nil {
					return err
				}
				result.Receipts = append(result.Receipts, row)
				return nil
			}})
		case schema.LocalContributionEvidenceOperationDiagnostics:
			result.OperationDiagnostics = []schema.LocalOperationDiagnostic{}
			return sqlitex.Execute(conn, `SELECT operation_id,operation,transcript_id,collective_id,attempted_at,outcome,issue_code,issue_message,issue_retry`+selectRows, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
				row := schema.LocalOperationDiagnostic{OperationID: schema.VillageUUID(stmt.ColumnText(0))}
				for _, known := range schema.AllCollectiveOperations {
					if string(known) == stmt.ColumnText(1) {
						row.Operation = known
					}
				}
				if stmt.ColumnType(2) != sqlite.TypeNull {
					id := schema.TranscriptID(stmt.ColumnText(2))
					row.TranscriptID = &id
				}
				if stmt.ColumnType(3) != sqlite.TypeNull {
					id := schema.VillageUUID(stmt.ColumnText(3))
					row.CollectiveID = &id
				}
				var err error
				row.AttemptedAt, err = time.Parse(time.RFC3339Nano, stmt.ColumnText(4))
				if err != nil {
					return err
				}
				for _, known := range schema.AllLocalOperationDiagnosticOutcomes {
					if string(known) == stmt.ColumnText(5) {
						row.Outcome = known
					}
				}
				row.Issue = schema.OperationIssue{Operation: row.Operation, Message: stmt.ColumnText(7), Retry: stmt.ColumnText(8)}
				for _, known := range schema.AllOperationIssueCodes {
					if string(known) == stmt.ColumnText(6) {
						row.Issue.Code = known
					}
				}
				if err := row.Validate(); err != nil {
					return err
				}
				result.OperationDiagnostics = append(result.OperationDiagnostics, row)
				return nil
			}})
		}
		return nil
	})
	if err != nil {
		return schema.LocalContributionHistoryResponse{}, err
	}
	if err := result.Validate(); err != nil {
		return schema.LocalContributionHistoryResponse{}, err
	}
	return result, nil
}
