package store_test

import (
	"context"
	_ "embed"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

//go:embed testdata/store_scope_contract.yaml
var storeScopeContractYAML []byte

//go:embed testdata/store_scope_contract.manifest.yaml
var storeScopeContractManifestYAML []byte

//go:embed testdata/scoped_pass_scope.yaml
var scopedPassScopeYAML []byte

//go:embed testdata/scoped_pass_scope.manifest.yaml
var scopedPassScopeManifestYAML []byte

type storeScopeContractFixture struct {
	Cohort        string `yaml:"cohort"`
	Decoy         string `yaml:"decoy"`
	CohortProject string `yaml:"cohortProject"`
	DecoyProject  string `yaml:"decoyProject"`
	Cases         []struct {
		Name    string   `yaml:"name"`
		IDs     []string `yaml:"ids"`
		WantIDs []string `yaml:"wantIDs"`
	} `yaml:"cases"`
}

type scopedPassCase struct {
	Name             string   `yaml:"name"`
	Narrowed         []string `yaml:"narrowed"`
	All              []string `yaml:"all"`
	NarrowedProjects []string `yaml:"narrowedProjects"`
	AllProjects      []string `yaml:"allProjects"`
}

type scopedPassFixture struct {
	Cohort              string           `yaml:"cohort"`
	Decoy               string           `yaml:"decoy"`
	CohortProject       string           `yaml:"cohortProject"`
	DecoyProject        string           `yaml:"decoyProject"`
	OrphanCohortProject string           `yaml:"orphanCohortProject"`
	OrphanDecoyProject  string           `yaml:"orphanDecoyProject"`
	Cases               []scopedPassCase `yaml:"cases"`
}

func loadStoreScopeContractFixture(t *testing.T) storeScopeContractFixture {
	t.Helper()
	var fixture storeScopeContractFixture
	if err := testutil.DecodeFixtureYAML(storeScopeContractYAML, &fixture); err != nil {
		t.Fatalf("decode store scope contract fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(storeScopeContractManifestYAML, "store scope contract")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		names = append(names, testCase.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "store scope contract"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func loadScopedPassFixture(t *testing.T) scopedPassFixture {
	t.Helper()
	var fixture scopedPassFixture
	if err := testutil.DecodeFixtureYAML(scopedPassScopeYAML, &fixture); err != nil {
		t.Fatalf("decode scoped pass fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(scopedPassScopeManifestYAML, "scoped pass scope")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		names = append(names, testCase.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "scoped pass scope"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestStoreScopeContract pins the two scope values: the zero value is refused,
// a nil narrowed scope names nothing, and the all-store value is the full run.
// The refusal witnesses assert both the error and that no row moved.
func TestStoreScopeContract(t *testing.T) {
	t.Parallel()
	fixture := loadStoreScopeContractFixture(t)
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			switch testCase.Name {
			case "zero_store_scope_refused":
				assertZeroStoreScopeRefused(t, fixture)
			case "zero_project_scope_refused":
				assertZeroProjectScopeRefused(t, fixture)
			case "nil_store_scope_selects_nothing":
				assertNilStoreScopeSelectsNothing(t, fixture)
			case "nil_project_scope_selects_nothing":
				assertNilProjectScopeSelectsNothing(t, fixture)
			case "narrowed_store_scope_sorts_and_deduplicates":
				ids := make([]ingest.SessionID, 0, len(testCase.IDs))
				for _, raw := range testCase.IDs {
					ids = append(ids, ingest.SessionID(raw))
				}
				narrowed, ok := ingest.StoredSessions(ids).Narrowed()
				if !ok {
					t.Fatal("StoredSessions reported the full-store scope")
				}
				if len(narrowed) != len(testCase.WantIDs) {
					t.Fatalf("narrowed ids = %v, want %v", narrowed, testCase.WantIDs)
				}
				for index, want := range testCase.WantIDs {
					if string(narrowed[index]) != want {
						t.Fatalf("narrowed ids = %v, want %v", narrowed, testCase.WantIDs)
					}
				}
			case "all_store_scope_is_the_full_run":
				if !ingest.AllStoredSessions().IsAll() {
					t.Fatal("AllStoredSessions is not the full-store scope")
				}
				if _, narrowed := ingest.AllStoredSessions().Narrowed(); narrowed {
					t.Fatal("AllStoredSessions reports a narrowed scope")
				}
			case "all_project_scope_is_the_full_run":
				if !ingest.AllProjects().IsAll() {
					t.Fatal("AllProjects is not the full-store scope")
				}
				if _, narrowed := ingest.AllProjects().Narrowed(); narrowed {
					t.Fatal("AllProjects reports a narrowed scope")
				}
			default:
				t.Fatalf("fixture case %q has no assertion", testCase.Name)
			}
		})
	}
}

func assertZeroStoreScopeRefused(t *testing.T, fixture storeScopeContractFixture) {
	t.Helper()
	dbPath := storetest.CopyGoldenDB(t)
	db, err := store.Open(dbPath, store.WithSkipMigrations(), store.WithWALAutocheckpointDisabled(), store.WithPoolSize(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	seedPassSessions(t, db, fixture.Cohort, fixture.Decoy, fixture.CohortProject, fixture.DecoyProject, ingest.HarnessCodex)
	seedStaleParentOrigins(t, db, fixture.Cohort, fixture.Decoy)

	before := countWALFrames(t, dbPath)
	_, err = db.InvalidateStaleParentOrigins(ctx, ingest.StoreScope{})
	if err == nil {
		t.Fatal("InvalidateStaleParentOrigins accepted the zero StoreScope")
	}
	if !strings.Contains(err.Error(), "zero StoreScope") {
		t.Fatalf("zero-scope refusal = %q, want an actionable zero StoreScope error", err)
	}
	if after := countWALFrames(t, dbPath); after != before {
		t.Fatalf("zero-scope refusal committed %d WAL frames, want 0 (before %d, after %d)", after-before, before, after)
	}
	for _, id := range []ingest.SessionID{ingest.SessionID(fixture.Cohort), ingest.SessionID(fixture.Decoy)} {
		if got := storedOriginVersion(t, db, id); got != ingest.OriginRuleVersion {
			t.Fatalf("session %s origin_version = %d after refusal, want %d; the refusal did work", id, got, ingest.OriginRuleVersion)
		}
	}
}

func assertZeroProjectScopeRefused(t *testing.T, fixture storeScopeContractFixture) {
	t.Helper()
	dbPath := storetest.CopyGoldenDB(t)
	db, err := store.Open(dbPath, store.WithSkipMigrations(), store.WithWALAutocheckpointDisabled(), store.WithPoolSize(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	seedOrphanProject(t, db, fixture.CohortProject)

	before := countWALFrames(t, dbPath)
	err = db.CleanupOrphanProjects(ctx, ingest.ProjectScope{})
	if err == nil {
		t.Fatal("CleanupOrphanProjects accepted the zero ProjectScope")
	}
	if !strings.Contains(err.Error(), "zero ProjectScope") {
		t.Fatalf("zero-project-scope refusal = %q, want an actionable zero ProjectScope error", err)
	}
	if after := countWALFrames(t, dbPath); after != before {
		t.Fatalf("zero-project-scope refusal committed %d WAL frames, want 0", after-before)
	}
	if !projectExists(t, db, fixture.CohortProject) {
		t.Fatal("the refused project cleanup deleted the orphan project")
	}
}

func assertNilStoreScopeSelectsNothing(t *testing.T, fixture storeScopeContractFixture) {
	t.Helper()
	db := storetest.Open(t)
	ctx := context.Background()
	seedPassSessions(t, db, fixture.Cohort, fixture.Decoy, fixture.CohortProject, fixture.DecoyProject, ingest.HarnessCodex)
	targets := map[ingest.Harness]ingest.HarvesterVersions{
		ingest.HarnessCodex: {IndexerVersion: 1, AdapterVersion: 2},
	}
	for _, scope := range []ingest.StoreScope{ingest.StoredSessions(nil), ingest.StoredSessions([]ingest.SessionID{})} {
		ids, err := db.ListStaleIndexSessions(ctx, targets, scope)
		if err != nil {
			t.Fatalf("ListStaleIndexSessions: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("StoredSessions(nil) selected %v, want no session", ids)
		}
		rows, err := db.ListStaleOriginSessions(ctx, ingest.OriginRuleVersion, scope)
		if err != nil {
			t.Fatalf("ListStaleOriginSessions: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("StoredSessions(nil) origin selected %v, want no session", rows)
		}
		page, err := db.ListMetricSessions(ctx, "", 256, scope)
		if err != nil {
			t.Fatalf("ListMetricSessions: %v", err)
		}
		if len(page) != 0 {
			t.Fatalf("StoredSessions(nil) metric selected %v, want no session", page)
		}
		requeued, err := db.InvalidateStaleParentOrigins(ctx, scope)
		if err != nil {
			t.Fatalf("InvalidateStaleParentOrigins: %v", err)
		}
		if requeued != 0 {
			t.Fatalf("StoredSessions(nil) requeued %d origin verdicts, want 0", requeued)
		}
	}
}

func assertNilProjectScopeSelectsNothing(t *testing.T, fixture storeScopeContractFixture) {
	t.Helper()
	db := storetest.Open(t)
	ctx := context.Background()
	seedOrphanProject(t, db, fixture.CohortProject)
	for _, scope := range []ingest.ProjectScope{ingest.Projects(nil), ingest.Projects([]ingest.ProjectHash{})} {
		if err := db.CleanupOrphanProjects(ctx, scope); err != nil {
			t.Fatalf("CleanupOrphanProjects: %v", err)
		}
		if !projectExists(t, db, fixture.CohortProject) {
			t.Fatal("Projects(nil) deleted the orphan project; an empty selection must name nothing")
		}
	}
}

// TestStoreScopeNarrowsEveryPass drives each scoped pass over two identical
// stores: one narrowed to the cohort and one at the full-store value. The
// fixture states the expected ids for each. A pass that ignores its scope
// returns the decoy from the narrowed store and fails.
func TestStoreScopeNarrowsEveryPass(t *testing.T) {
	t.Parallel()
	fixture := loadScopedPassFixture(t)
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			narrowed := runScopedPass(t, fixture, testCase.Name, false)
			wide := runScopedPass(t, fixture, testCase.Name, true)
			if testCase.Name == "orphan_projects" {
				assertIDsEqual(t, testCase.Name+" narrowed", narrowed, testCase.NarrowedProjects)
				assertIDsEqual(t, testCase.Name+" all", wide, testCase.AllProjects)
				return
			}
			assertIDsEqual(t, testCase.Name+" narrowed", narrowed, testCase.Narrowed)
			assertIDsEqual(t, testCase.Name+" all", wide, testCase.All)
		})
	}
}

// runScopedPass seeds a fresh store and runs one named pass either narrowed to
// the cohort or at the full-store value. It returns the ids the pass acted on
// (selected, or mutated for a write pass).
func runScopedPass(t *testing.T, fixture scopedPassFixture, name string, full bool) []string {
	t.Helper()
	ctx := context.Background()
	if name == "flagged_sweep" {
		return runFlaggedSweepPass(t, fixture, full)
	}
	db := storetest.Open(t)
	cohort := ingest.SessionID(fixture.Cohort)
	decoy := ingest.SessionID(fixture.Decoy)
	seedPassSessions(t, db, fixture.Cohort, fixture.Decoy, fixture.CohortProject, fixture.DecoyProject, ingest.HarnessCodex)

	scope := ingest.StoredSessions([]ingest.SessionID{cohort})
	if full {
		scope = ingest.AllStoredSessions()
	}
	targets := map[ingest.Harness]ingest.HarvesterVersions{
		ingest.HarnessCodex: {IndexerVersion: 1, AdapterVersion: 2},
	}

	switch name {
	case "stale_origin":
		rows, err := db.ListStaleOriginSessions(ctx, ingest.OriginRuleVersion, scope)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]ingest.SessionID, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.SessionID)
		}
		return sessionIDStrings(ids)
	case "invalidate_stale_parent_origins":
		seedStaleParentOrigins(t, db, fixture.Cohort, fixture.Decoy)
		if _, err := db.InvalidateStaleParentOrigins(ctx, scope); err != nil {
			t.Fatal(err)
		}
		var reset []string
		for _, id := range []ingest.SessionID{cohort, decoy} {
			if storedOriginVersion(t, db, id) == 0 {
				reset = append(reset, string(id))
			}
		}
		sort.Strings(reset)
		return reset
	case "stale_adapter":
		ids, err := db.ListStaleAdapterSessions(ctx, targets, nil, scope)
		if err != nil {
			t.Fatal(err)
		}
		return sessionIDStrings(ids)
	case "stale_index":
		ids, err := db.ListStaleIndexSessions(ctx, targets, scope)
		if err != nil {
			t.Fatal(err)
		}
		return sessionIDStrings(ids)
	case "repair":
		ids, err := db.ListSessionsNeedingRepair(ctx, targets, scope)
		if err != nil {
			t.Fatal(err)
		}
		return sessionIDStrings(ids)
	case "content_incomplete":
		page, err := db.ListContentCaptureIncompleteSessionsAfter(ctx, "", 256, scope)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]ingest.SessionID, 0, len(page))
		for _, target := range page {
			ids = append(ids, target.SessionID)
		}
		return sessionIDStrings(ids)
	case "metric_sessions":
		page, err := db.ListMetricSessions(ctx, "", 256, scope)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]ingest.SessionID, 0, len(page))
		for _, target := range page {
			ids = append(ids, target.SessionID)
		}
		return sessionIDStrings(ids)
	case "orphan_projects":
		seedOrphanProject(t, db, fixture.OrphanCohortProject)
		seedOrphanProject(t, db, fixture.OrphanDecoyProject)
		projectScope := ingest.Projects([]ingest.ProjectHash{ingest.ProjectHash(fixture.OrphanCohortProject)})
		if full {
			projectScope = ingest.AllProjects()
		}
		if err := db.CleanupOrphanProjects(ctx, projectScope); err != nil {
			t.Fatal(err)
		}
		var deleted []string
		for _, hash := range []string{fixture.OrphanCohortProject, fixture.OrphanDecoyProject} {
			if !projectExists(t, db, hash) {
				deleted = append(deleted, hash)
			}
		}
		sort.Strings(deleted)
		return deleted
	default:
		t.Fatalf("fixture pass %q has no driver", name)
		return nil
	}
}

func runFlaggedSweepPass(t *testing.T, fixture scopedPassFixture, full bool) []string {
	t.Helper()
	root := t.TempDir()
	artifacts, err := store.NewOSGenerationArtifactStore(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	locker, err := store.NewFileSessionLocker(filepath.Join(root, "locks"))
	if err != nil {
		t.Fatal(err)
	}
	db := storetest.OpenWith(t, store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locker))
	ctx := context.Background()
	cohort := ingest.SessionID(fixture.Cohort)
	decoy := ingest.SessionID(fixture.Decoy)
	seedPassSessions(t, db, fixture.Cohort, fixture.Decoy, fixture.CohortProject, fixture.DecoyProject, ingest.HarnessCodex)
	setSweepPending(t, db, cohort)
	setSweepPending(t, db, decoy)
	scope := ingest.StoredSessions([]ingest.SessionID{cohort})
	if full {
		scope = ingest.AllStoredSessions()
	}
	if _, _, err := db.SweepFlaggedSessionsForHarvest(ctx, scope); err != nil {
		t.Fatal(err)
	}
	var cleared []string
	for _, id := range []ingest.SessionID{cohort, decoy} {
		if !sweepPending(t, db, id) {
			cleared = append(cleared, string(id))
		}
	}
	sort.Strings(cleared)
	return cleared
}

// seedPassSessions inserts the cohort and decoy rows every scoped pass reads.
// Both carry an artifact hash, no indexed input proof, no adapter revision and
// no content capture row, so the same pair qualifies for every pass.
func seedPassSessions(t *testing.T, db *store.Store, cohort, decoy, cohortProject, decoyProject string, harness ingest.Harness) {
	t.Helper()
	for _, spec := range []struct {
		id, project string
	}{
		{cohort, cohortProject},
		{decoy, decoyProject},
	} {
		entry := scopeSessionEntry(t, spec.id, spec.project, harness)
		if err := db.InsertSessions(context.Background(), []ingest.StoreEntry{entry}); err != nil {
			t.Fatalf("seed session %s: %v", spec.id, err)
		}
	}
}

func scopeSessionEntry(t *testing.T, rawID, rawProject string, harness ingest.Harness) ingest.StoreEntry {
	t.Helper()
	id, err := ingest.NewSessionID(rawID)
	if err != nil {
		t.Fatal(err)
	}
	project, err := ingest.NewProjectHash(rawProject)
	if err != nil {
		t.Fatal(err)
	}
	host, err := ingest.NewHostSlug("github.com--scope--test")
	if err != nil {
		t.Fatal(err)
	}
	source, err := ingest.NewResolvedPath("/synthetic/sessions/" + rawID + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	artifact := strings.Repeat("e", 64)
	start := int64(1700000000000)
	ingested := start + 60000
	meta := ingest.NewUnifiedMetadata()
	meta.SchemaVersion = ingest.CurrentSchemaVersion
	meta.SessionID = id
	meta.ModelHarness = harness
	meta.Model = ingest.ModelID("claude-opus-4-6")
	meta.HostSlug = host
	meta.Timestamp = ingest.TimestampInfo{Start: start, End: start + 60000, Ingested: &ingested}
	meta.Source = ingest.SourceInfo{FilePath: string(source), Format: ingest.SourceFormatJSONL}
	meta.Project = ingest.ProjectInfo{Hash: project, Name: "scope-test", FilePath: "/scope/test"}
	return ingest.StoreEntry{
		ArtifactHash: &artifact,
		Metadata:     &meta,
		Session: ingest.DiscoveredSession{
			SessionID:    id,
			Harness:      harness,
			SourcePath:   source,
			SourceFormat: ingest.SourceFormatJSONL,
		},
	}
}

func seedStaleParentOrigins(t *testing.T, db *store.Store, ids ...string) {
	t.Helper()
	conn, err := db.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.PoolForTest().Put(conn)
	const parent = "99999999-9999-4999-8999-999999999999"
	const generation = "scope-parent-generation"
	for _, rawID := range ids {
		if _, err := ingest.NewSessionID(rawID); err != nil {
			t.Fatal(err)
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_relationship_evidence
(session_id, generation_id, ordinal, kind, target_state, target_local_id, evidence, anchor_kind, anchor_source_entry_ref, anchor_source_revision_ref)
VALUES (?, ?, 0, 'started_by', 'target_known', ?, 'native_typed', '', '', '')`, &sqlitex.ExecOptions{
			Args: []any{rawID, generation, parent},
		}); err != nil {
			t.Fatalf("seed relationship evidence for %s: %v", rawID, err)
		}
		if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET latest_attempt_generation_id = ?, origin_version = ?, session_origin = 'unknown' WHERE session_id = ?`, &sqlitex.ExecOptions{
			Args: []any{generation, ingest.OriginRuleVersion, rawID},
		}); err != nil {
			t.Fatalf("seed stale parent origin for %s: %v", rawID, err)
		}
	}
}

func storedOriginVersion(t *testing.T, db *store.Store, id ingest.SessionID) int {
	t.Helper()
	conn, err := db.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.PoolForTest().Put(conn)
	version := -1
	if err := sqlitex.ExecuteTransient(conn, `SELECT origin_version FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(id)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			version = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return version
}

func setSweepPending(t *testing.T, db *store.Store, id ingest.SessionID) {
	t.Helper()
	conn, err := db.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.PoolForTest().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET content_sweep_pending = 1 WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(id)},
	}); err != nil {
		t.Fatal(err)
	}
}

func sweepPending(t *testing.T, db *store.Store, id ingest.SessionID) bool {
	t.Helper()
	conn, err := db.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.PoolForTest().Put(conn)
	pending := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT content_sweep_pending FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(id)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			pending = stmt.ColumnInt(0) == 1
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return pending
}

func seedOrphanProject(t *testing.T, db *store.Store, hash string) {
	t.Helper()
	if _, err := ingest.NewProjectHash(hash); err != nil {
		t.Fatal(err)
	}
	conn, err := db.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.PoolForTest().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `INSERT OR IGNORE INTO projects(project_hash, canonical_cwd) VALUES (?, ?)`, &sqlitex.ExecOptions{
		Args: []any{hash, "/synthetic/orphan"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sqlitex.ExecuteTransient(conn, `INSERT OR IGNORE INTO daily_summary_by_project(date_utc, project_hash) VALUES ('2026-01-01', ?)`, &sqlitex.ExecOptions{
		Args: []any{hash},
	}); err != nil {
		t.Fatal(err)
	}
}

func projectExists(t *testing.T, db *store.Store, hash string) bool {
	t.Helper()
	conn, err := db.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.PoolForTest().Put(conn)
	exists := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT 1 FROM projects WHERE project_hash = ?`, &sqlitex.ExecOptions{
		Args: []any{hash},
		ResultFunc: func(*sqlite.Stmt) error {
			exists = true
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return exists
}

func countWALFrames(t *testing.T, dbPath string) int {
	t.Helper()
	frames, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	return frames
}

func sessionIDStrings(ids []ingest.SessionID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	sort.Strings(out)
	return out
}

func assertIDsEqual(t *testing.T, label string, got, want []string) {
	t.Helper()
	gotCopy := append([]string(nil), got...)
	wantCopy := append([]string(nil), want...)
	sort.Strings(gotCopy)
	sort.Strings(wantCopy)
	if len(gotCopy) != len(wantCopy) {
		t.Fatalf("%s ids = %v, want %v", label, gotCopy, wantCopy)
	}
	for index := range wantCopy {
		if gotCopy[index] != wantCopy[index] {
			t.Fatalf("%s ids = %v, want %v", label, gotCopy, wantCopy)
		}
	}
}
