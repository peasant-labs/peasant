package store

import (
	"fmt"
	"reflect"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func (s *Store) applyCapturedClassifierBatch(conn *sqlite.Conn, batch ingest.SessionAnnotationBatch, stats *ingest.AnnotationProfileStats) (_ []ingest.ClassifierAnnotationWriteResult, retErr error) {
	end := sqlitex.Save(conn)
	defer end(&retErr)
	if err := s.validateClassifierInput(conn, batch); err != nil {
		return nil, err
	}
	results, err := s.applyClassifierAnnotationWritesOnConn(conn, sessionAnnotationBatchStoreWrites(batch.Writes), stats)
	if err != nil {
		return results, err
	}
	for _, result := range results {
		if result.Err != nil {
			return results, result.Err
		}
	}
	if err := retireClassifierOutputs(conn, batch, results); err != nil {
		return results, err
	}
	return results, saveAnnotationRunStateOnConn(conn, *batch.RunState)
}

func (s *Store) validateClassifierInput(conn *sqlite.Conn, batch ingest.SessionAnnotationBatch) error {
	stale := func(reason string) error {
		return fmt.Errorf("save classifiers for session %s: %s; no annotation changes committed; refresh metrics and retry classification", batch.SessionID, reason)
	}
	expected, state := batch.Input, batch.RunState
	if expected == nil || state == nil || expected.SessionID != batch.SessionID || state.SessionID != batch.SessionID || len(batch.Owners) == 0 {
		return stale("missing captured input, completion state or configured ownership")
	}
	hash, err := expected.Hash()
	if err != nil || hash != expected.DatabaseHash {
		return stale("captured inputs were modified")
	}
	current, err := s.readMetricInputOnConn(conn, batch.SessionID, expected.StoredModels)
	if err != nil {
		return err
	}
	if current.DatabaseHash != expected.DatabaseHash || !reflect.DeepEqual(current.Existing, expected.Existing) {
		return stale("entries or consumed metrics inputs changed during classification")
	}
	metrics := current.Existing
	if metrics == nil || metrics.InputHash == nil || metrics.OutputHash == nil || metrics.ComputeVersion == nil || *metrics.ComputeVersion != state.ComputeVersion || current.IndexState.SessionEntriesHash == nil || *current.IndexState.SessionEntriesHash != state.SessionEntriesHash {
		return stale("completion evidence does not match captured entries and metrics")
	}
	outputHash, err := ingest.MetricOutputHash(metrics)
	if err != nil || state.MetricsOutputHash == "" || outputHash != state.MetricsOutputHash || outputHash != *metrics.OutputHash {
		return stale("actual metrics output differs from completion evidence")
	}
	var newer bool
	if err := sqlitex.ExecuteTransient(conn, `SELECT classifier_version FROM annotation_run_state WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(batch.SessionID)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			newer = stmt.ColumnInt(0) > state.ClassifierVersion
			return nil
		},
	}); err != nil {
		return err
	}
	if newer {
		return stale("stored classifier version is newer")
	}
	for _, write := range batch.Writes {
		owned := false
		for _, owner := range batch.Owners {
			if owner.AnnotatorID == write.Write.Create.AnnotatorID && owner.AnnotationTypeID == write.Write.Create.AnnotationTypeID && owner.AnnotatorID == write.Write.Find.AnnotatorID && owner.AnnotationTypeID == write.Write.Find.AnnotationTypeID {
				owned = true
				break
			}
		}
		p := write.Write.Create
		if !owned || write.Write.Find.SessionID == nil || *write.Write.Find.SessionID != string(batch.SessionID) || (p.SessionID == nil && p.EntryTarget == nil) || (p.SessionID != nil && *p.SessionID != string(batch.SessionID)) || (p.EntryTarget != nil && p.EntryTarget.SessionID != string(batch.SessionID)) {
			return stale("prepared write is outside configured ownership or session")
		}
	}
	return nil
}

func retireClassifierOutputs(conn *sqlite.Conn, batch ingest.SessionAnnotationBatch, results []ingest.ClassifierAnnotationWriteResult) error {
	keep := make(map[string]bool, len(results))
	for _, result := range results {
		keep[result.AnnotationID] = true
	}
	for _, owner := range batch.Owners {
		var obsolete []string
		if err := sqlitex.ExecuteTransient(conn, `SELECT a.id FROM annotations a
WHERE a.annotator_id = ? AND a.annotation_type_id = ?
AND a.superseded_by IS NULL AND a.retired_at IS NULL
AND (a.id IN (SELECT annotation_id FROM annotation_target_sessions WHERE session_id = ?)
 OR a.id IN (SELECT annotation_id FROM annotation_target_entries WHERE session_id = ?)
 OR a.id IN (SELECT annotation_id FROM annotation_target_anchors WHERE session_id = ?))`, &sqlitex.ExecOptions{
			Args: []any{owner.AnnotatorID, owner.AnnotationTypeID, string(batch.SessionID), string(batch.SessionID), string(batch.SessionID)},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				if id := stmt.ColumnText(0); !keep[id] {
					obsolete = append(obsolete, id)
				}
				return nil
			},
		}); err != nil {
			return err
		}
		for _, id := range obsolete {
			if err := sqlitex.ExecuteTransient(conn, `UPDATE annotations SET retired_at = ?, updated_at = ? WHERE id = ?`, &sqlitex.ExecOptions{Args: []any{time.Now().UnixMilli(), time.Now().UnixMilli(), id}}); err != nil {
				return err
			}
		}
	}
	return nil
}
