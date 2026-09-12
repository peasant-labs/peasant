package store

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

var (
	_ ingest.StaleAdapterSessionLister = (*Store)(nil)
	_ ingest.RepairSessionLister       = (*Store)(nil)
)

// ListStaleAdapterSessions selects the sessions whose stored adapter revision
// is behind this build's target for their harness, or whose stored schema is
// one this build re-extracts from native input. It is the database-driven
// replacement for the retained-tree walk that used to find adapter-refresh
// work: it reads no file, and a NULL adapter_version counts as revision 1, so
// at target 1 no legacy row is selected and nothing is re-extracted every run.
//
// nativeRefreshVersions is the exact set of stored schema versions this build
// re-extracts from native input, computed once in ingest so the two predicates
// (this selection and metadataNeedsNativeRefresh) cannot drift.
func (s *Store) ListStaleAdapterSessions(ctx context.Context, targets map[ingest.Harness]ingest.HarvesterVersions, nativeRefreshVersions []int) ([]ingest.SessionID, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	refresh := make([]string, 0, len(nativeRefreshVersions))
	refreshArgs := make([]any, 0, len(nativeRefreshVersions))
	for _, version := range nativeRefreshVersions {
		refresh = append(refresh, "?")
		refreshArgs = append(refreshArgs, version)
	}
	refreshClause := "0"
	if len(refresh) > 0 {
		refreshClause = "s.schema_version IN (" + strings.Join(refresh, ",") + ")"
	}
	conditions := make([]string, 0, len(targets))
	args := make([]any, 0, len(targets)*(2+len(refreshArgs)))
	for _, harness := range slices.Sorted(maps.Keys(targets)) {
		adapter := targets[harness].AdapterVersion
		if !harness.IsKnown() || adapter < 1 {
			return nil, fmt.Errorf("store: select stale adapters: harness %q has invalid adapter target %d; supply registered positive harvester versions", harness, adapter)
		}
		conditions = append(conditions, "(s.model_harness = ? AND s.schema_version <= ? AND (COALESCE(s.adapter_version, 1) < ? OR "+refreshClause+"))")
		args = append(args, string(harness), int(ingest.CurrentSchemaVersion), adapter)
		args = append(args, refreshArgs...)
	}
	return s.selectSessionIDs(ctx, "SELECT s.session_id FROM sessions s WHERE "+strings.Join(conditions, " OR ")+" ORDER BY s.session_id", args)
}

// ListSessionsNeedingRepair selects the sessions a crash between the two write
// commits can leave: a row whose pair hash is recorded but whose index input
// proof was cleared, and a row whose current publication capture the index is
// not yet bound to. Both settle in one ordinary harvest, so the next unchanged
// harvest selects nothing. It is harness-scoped and reads no file.
func (s *Store) ListSessionsNeedingRepair(ctx context.Context, targets map[ingest.Harness]ingest.HarvesterVersions) ([]ingest.SessionID, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	harnessPlaceholders := make([]string, 0, len(targets))
	args := make([]any, 0, len(targets)+1)
	for _, harness := range slices.Sorted(maps.Keys(targets)) {
		if !harness.IsKnown() {
			return nil, fmt.Errorf("store: select repair sessions: harness %q is not registered", harness)
		}
		harnessPlaceholders = append(harnessPlaceholders, "?")
		args = append(args, string(harness))
	}
	args = append(args, int(ingest.CurrentSchemaVersion))
	query := `SELECT s.session_id FROM sessions s
WHERE s.model_harness IN (` + strings.Join(harnessPlaceholders, ",") + `)
 AND s.schema_version <= ?
 AND (
   (s.artifact_hash IS NOT NULL AND s.indexed_input_hash IS NULL)
   OR (s.publication_capture_revision > 0
       AND s.indexed_publication_capture_revision <> s.publication_capture_revision
       AND EXISTS (SELECT 1 FROM session_publication_metadata p
                   WHERE p.session_id = s.session_id
                     AND p.capture_revision = s.publication_capture_revision))
 )
ORDER BY s.session_id`
	return s.selectSessionIDs(ctx, query, args)
}

func (s *Store) selectSessionIDs(ctx context.Context, query string, args []any) ([]ingest.SessionID, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)
	var sessions []ingest.SessionID
	err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sid, err := ingest.NewSessionID(stmt.ColumnText(0))
			if err != nil {
				return nil // Skip invalid session IDs; they cannot be worked anyway.
			}
			sessions = append(sessions, sid)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: select session ids: %w", err)
	}
	return sessions, nil
}
