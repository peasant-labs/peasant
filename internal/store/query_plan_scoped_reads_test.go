package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestScopedStatementsQueryPlans is the query-plan guard for every statement
// the scoped reads and the derived lookups touch. Production and this guard
// share the same SQL renderers: no statement is copied into the test, so a
// production edit that re-introduces a population scan is red here.
//
// The guarded property is size independence: a scoped read must be driven by
// the ids, paths, or project hashes it was given. A SCAN of the session
// population, the evidence table, the spawns projection, the rollout catalog,
// or the relationship evidence is exactly the failure this refuses.
func TestScopedStatementsQueryPlans(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, err := Open(filepath.Join(t.TempDir(), "scoped-plan.db"), WithPoolSize(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.pool.Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.pool.Put(conn)

	const (
		idA = ingest.SessionID("11111111-1111-4111-8111-111111111111")
		idB = ingest.SessionID("22222222-2222-4222-8222-222222222222")
	)
	narrow := ingest.StoredSessions([]ingest.SessionID{idA, idB})
	harnessArgs := make([]any, 0, len(independentAdmissionHarnesses()))
	for _, harness := range independentAdmissionHarnesses() {
		harnessArgs = append(harnessArgs, string(harness))
	}
	targets := map[ingest.Harness]ingest.HarvesterVersions{
		ingest.HarnessCodex:    {IndexerVersion: 1, AdapterVersion: 1},
		ingest.HarnessOpenCode: {IndexerVersion: 1, AdapterVersion: 1},
	}
	adapterSQL, adapterArgs, err := buildListStaleAdapterSessionsQuery(targets, nil, narrow)
	if err != nil {
		t.Fatal(err)
	}
	indexSQL, indexArgs, err := buildListStaleIndexSessionsQuery(targets, narrow)
	if err != nil {
		t.Fatal(err)
	}
	repairSQL, repairArgs, err := buildListSessionsNeedingRepairQuery(targets, narrow)
	if err != nil {
		t.Fatal(err)
	}

	contentSQL, contentArgs := buildListContentCaptureIncompleteSessionsQuery("", 25, narrow)
	sweepSQL, sweepArgs := flaggedSessionsSweepSQL(narrow)
	originSQL, originArgs := listStaleOriginSessionsSQL(0, narrow)
	invalidateSQL, invalidateArgs := invalidateStaleParentOriginsSQL(narrow)

	projects := ingest.Projects([]ingest.ProjectHash{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	orphanDailySQL, orphanDailyArgs := buildDeleteOrphanDailySummaryByProjectSQL(projects)
	orphanProjectsSQL, orphanProjectsArgs := buildDeleteOrphanProjectsSQL(projects)
	metricSQL, metricArgs := buildListMetricSessionsQuery("", 25, narrow)

	targetsJSON := `["11111111-1111-4111-8111-111111111111","22222222-2222-4222-8222-222222222222"]`
	durableArgs := append([]any{targetsJSON}, harnessArgs...)

	cases := []struct {
		name      string
		sql       string
		args      []any
		wantIndex string
	}{
		{
			name:      "flagged_sweep",
			sql:       sweepSQL,
			args:      sweepArgs,
			wantIndex: "sqlite_autoindex_sessions_1",
		},
		{
			name:      "stale_origin",
			sql:       originSQL,
			args:      originArgs,
			wantIndex: "sqlite_autoindex_sessions_1",
		},
		{
			name: "invalidate_stale_parent_origins",
			sql:  invalidateSQL,
			args: invalidateArgs,
		},
		{name: "stale_adapter", sql: adapterSQL, args: adapterArgs},
		{name: "stale_index", sql: indexSQL, args: indexArgs},
		{name: "repair", sql: repairSQL, args: repairArgs},
		{name: "content_incomplete", sql: contentSQL, args: contentArgs},
		{
			name:      "metric_sessions",
			sql:       metricSQL,
			args:      metricArgs,
			wantIndex: "sqlite_autoindex_sessions_1",
		},
		{
			name:      "orphan_daily_summary",
			sql:       orphanDailySQL,
			args:      orphanDailyArgs,
			wantIndex: "idx_sessions_project",
		},
		{
			name:      "orphan_projects",
			sql:       orphanProjectsSQL,
			args:      orphanProjectsArgs,
			wantIndex: "idx_sessions_project",
		},
		{
			name:      "claude_evidence_by_path",
			sql:       sqlSelectClaudeEvidenceFor(2),
			args:      []any{"/synthetic/a.jsonl", "/synthetic/b.jsonl"},
			wantIndex: "sqlite_autoindex_claude_transcript_evidence_1",
		},
		{
			name:      "claude_identity_declarers",
			sql:       sqlCountClaudeIdentityDeclarers,
			args:      []any{"team", "name"},
			wantIndex: "idx_claude_evidence_identity_root",
		},
		{
			name:      "claude_identity_spawners",
			sql:       sqlCountClaudeIdentitySpawners,
			args:      []any{"team", "name"},
			wantIndex: "idx_claude_teammate_spawns_identity",
		},
		{
			name:      "claude_identity_spawner_paths",
			sql:       sqlClaudeIdentitySpawnerPaths,
			args:      []any{"team", "name"},
			wantIndex: "idx_claude_teammate_spawns_identity",
		},
		{
			name:      "codex_rollout_count",
			sql:       sqlCountCodexRolloutsBySession,
			args:      []any{string(idA)},
			wantIndex: "idx_codex_rollout_catalog_session",
		},
		{
			name:      "codex_rollout_paths",
			sql:       sqlCodexRolloutPathsBySession,
			args:      []any{string(idA)},
			wantIndex: "idx_codex_rollout_catalog_session",
		},
		{
			name:      "stored_session_sources",
			sql:       storedSessionSourcesSQL(2),
			args:      []any{string(idA), string(idB)},
			wantIndex: "sqlite_autoindex_sessions_1",
		},
		{
			name:      "stored_cached_children",
			sql:       storedCachedChildrenSQL(2),
			args:      []any{string(idA), string(idB)},
			wantIndex: "idx_sessions_parent",
		},
		{
			name:      "stored_relationship_children",
			sql:       storedChildrenSQL(storedRelationshipChildrenSQL, len(harnessArgs)),
			args:      durableArgs,
			wantIndex: "idx_relationship_evidence_started_by_target",
		},
		{
			name:      "stored_publication_children",
			sql:       storedChildrenSQL(storedPublicationChildrenSQL, len(harnessArgs)),
			args:      durableArgs,
			wantIndex: "idx_session_publication_parent_uuid",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			plan := explainQueryPlan(t, conn, testCase.sql, testCase.args)
			assertNoForbiddenScan(t, testCase.name, plan)
			if testCase.wantIndex != "" {
				assertPlanUsesIndex(t, testCase.name, plan, testCase.wantIndex)
			}
		})
	}
}

// forbiddenScanTargets maps every alias used by the guarded statements to the
// table it names, plus the unaliased table names. The guard refuses a SCAN of
// any of them: those are the population-wide reads R8 forbids.
var forbiddenScanTargets = map[string]string{
	"s":                             "sessions",
	"sessions":                      "sessions",
	"r":                             "session_relationship_evidence",
	"session_relationship_evidence": "session_relationship_evidence",
	"claude_transcript_evidence":    "claude_transcript_evidence",
	"claude_teammate_spawns":        "claude_teammate_spawns",
	"codex_rollout_catalog":         "codex_rollout_catalog",
}

func explainQueryPlan(t *testing.T, conn *sqlite.Conn, sql string, args []any) []string {
	t.Helper()
	var plan []string
	if err := sqlitex.ExecuteTransient(conn, "EXPLAIN QUERY PLAN "+sql, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			plan = append(plan, stmt.ColumnText(3))
			return nil
		},
	}); err != nil {
		t.Fatalf("explain %q: %v", sql, err)
	}
	if len(plan) == 0 {
		t.Fatalf("explain %q produced no plan rows", sql)
	}
	return plan
}

func assertNoForbiddenScan(t *testing.T, label string, plan []string) {
	t.Helper()
	for _, row := range plan {
		rest, scanned := strings.CutPrefix(row, "SCAN ")
		if !scanned {
			continue
		}
		name := strings.Fields(rest)
		if len(name) == 0 {
			continue
		}
		target := name[0]
		if table, forbidden := forbiddenScanTargets[target]; forbidden {
			t.Errorf("%s scans %s (%q); a scoped read must be driven by its ids, paths, or hashes; plan=%v", label, table, row, plan)
		}
	}
}

func assertPlanUsesIndex(t *testing.T, label string, plan []string, index string) {
	t.Helper()
	for _, row := range plan {
		if strings.Contains(row, "USING INDEX "+index) || strings.Contains(row, "USING COVERING INDEX "+index) {
			return
		}
	}
	t.Errorf("%s does not seek %s; plan=%v", label, index, plan)
}
