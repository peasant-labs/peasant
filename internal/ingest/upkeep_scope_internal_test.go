package ingest

import (
	"context"
	_ "embed"
	"sort"
	"testing"

	"github.com/peasant-labs/peasant/internal/testkit/scoperecorder"
)

//go:embed testdata/upkeep_scope.yaml
var upkeepScopeFixtureData []byte

type upkeepScopeFixture struct {
	Required []string `yaml:"requiredNames"`
	Cases    []struct {
		Name       string   `yaml:"name"`
		Operations []string `yaml:"operations"`
	} `yaml:"cases"`
}

// upkeepScopeWorld is the store and analyzer one pass sees while it records the
// scope it was given. Every listing returns nothing after recording, so a case
// observes the pass's argument without needing a seeded store. The embedded
// interfaces supply the methods the case does not exercise; a pass that calls
// one of those panics, which is a louder failure than a silent zero value.
type upkeepScopeWorld struct {
	SessionStore
	MetricsStore
	SessionAnalyzer

	calls scoperecorder.Recorder

	// candidates are the uncached children the parent-heal listing returns.
	candidates []ParentCacheReconcile
	reconciled []ParentCacheReconcile
	parents    []SessionID
	harnesses  []Harness
	// rows is the stale-origin listing the resolver judges.
	rows []StoredOriginRow
	// evidencePaths records every path set the by-path evidence read asked for.
	evidencePaths []ResolvedPath
	// metricPages is served page by page on the caller's cursor.
	metricPages [][]MetricSession
	// changed names the sessions whose metrics refresh reports a change.
	changed map[SessionID]bool
	// locations answers the primary-key location read.
	locations map[SessionID]SessionLocation
}

func (w *upkeepScopeWorld) recordStore(operation string, scope StoreScope) {
	ids, _ := scope.Narrowed()
	narrowed := make([]string, 0, len(ids))
	for _, id := range ids {
		narrowed = append(narrowed, string(id))
	}
	w.calls.RecordStore(operation, narrowed, scope.IsAll())
}

func (w *upkeepScopeWorld) recordProjects(operation string, scope ProjectScope) {
	hashes, _ := scope.Narrowed()
	narrowed := make([]string, 0, len(hashes))
	for _, hash := range hashes {
		narrowed = append(narrowed, string(hash))
	}
	w.calls.RecordProjects(operation, narrowed, scope.IsAll())
}

func (w *upkeepScopeWorld) LookupSessionLocation(context.Context, SessionID) (string, string, error) {
	return "", "", nil
}

func (w *upkeepScopeWorld) BulkLookupSessionLocations(_ context.Context, ids []SessionID) (map[SessionID]SessionLocation, error) {
	locations := make(map[SessionID]SessionLocation, len(ids))
	for _, id := range ids {
		if location, ok := w.locations[id]; ok {
			locations[id] = location
		}
	}
	return locations, nil
}

func (w *upkeepScopeWorld) LookupSourceInfo(context.Context, SessionID) (string, SourceFormat, string, error) {
	return "", "", "", nil
}

func (w *upkeepScopeWorld) IndexSessionEntryBatch(context.Context, []SessionEntryWrite) []SessionEntryWriteResult {
	return nil
}

func (w *upkeepScopeWorld) ReadIndexState(context.Context, SessionID) (*SessionIndexState, error) {
	return nil, nil
}

func (w *upkeepScopeWorld) CleanupOrphanProjects(_ context.Context, scope ProjectScope) error {
	w.recordProjects("CleanupOrphanProjects", scope)
	return nil
}

func (w *upkeepScopeWorld) SweepSessionForHarvest(context.Context, SessionID) error { return nil }

func (w *upkeepScopeWorld) SweepFlaggedSessionsForHarvest(_ context.Context, scope StoreScope) (int, []error, error) {
	w.recordStore("SweepFlaggedSessionsForHarvest", scope)
	return 0, nil, nil
}

func (w *upkeepScopeWorld) ListStaleOriginSessions(_ context.Context, _ int, scope StoreScope) ([]StoredOriginRow, error) {
	w.recordStore("ListStaleOriginSessions", scope)
	return w.rows, nil
}

func (w *upkeepScopeWorld) UpdateOriginState(context.Context, SessionID, string, int) error {
	return nil
}

func (w *upkeepScopeWorld) LeadingUserMessagesBulk(context.Context, []string, int) (map[string][]string, error) {
	return nil, nil
}

func (w *upkeepScopeWorld) LoadClaudeEvidence(context.Context) (map[ResolvedPath]ClaudeTranscriptEvidence, error) {
	w.calls.RecordStore("LoadClaudeEvidence", nil, false)
	return nil, nil
}

func (w *upkeepScopeWorld) SaveClaudeEvidence(context.Context, []ClaudeTranscriptEvidence, []ResolvedPath) error {
	return nil
}

func (w *upkeepScopeWorld) LoadClaudeEvidenceFor(_ context.Context, paths []ResolvedPath) (map[ResolvedPath]ClaudeTranscriptEvidence, error) {
	w.calls.RecordStore("LoadClaudeEvidenceFor", nil, false)
	w.evidencePaths = append(w.evidencePaths, paths...)
	return nil, nil
}

func (w *upkeepScopeWorld) InvalidateStaleParentOrigins(_ context.Context, scope StoreScope) (int64, error) {
	w.recordStore("InvalidateStaleParentOrigins", scope)
	return 0, nil
}

func (w *upkeepScopeWorld) ListUncachedChildrenOfParents(_ context.Context, parents []SessionID, harnesses []Harness) ([]ParentCacheReconcile, error) {
	w.parents = append([]SessionID(nil), parents...)
	w.harnesses = append([]Harness(nil), harnesses...)
	return w.candidates, nil
}

func (w *upkeepScopeWorld) ReconcileParentCache(_ context.Context, updates []ParentCacheReconcile) error {
	w.reconciled = append([]ParentCacheReconcile(nil), updates...)
	return nil
}

func (w *upkeepScopeWorld) ListStaleAdapterSessions(_ context.Context, _ map[Harness]HarvesterVersions, _ []int, scope StoreScope) ([]SessionID, error) {
	w.recordStore("ListStaleAdapterSessions", scope)
	return nil, nil
}

func (w *upkeepScopeWorld) ListStaleIndexSessions(_ context.Context, _ map[Harness]HarvesterVersions, scope StoreScope) ([]SessionID, error) {
	w.recordStore("ListStaleIndexSessions", scope)
	return nil, nil
}

func (w *upkeepScopeWorld) ListSessionsNeedingRepair(_ context.Context, _ map[Harness]HarvesterVersions, scope StoreScope) ([]SessionID, error) {
	w.recordStore("ListSessionsNeedingRepair", scope)
	return nil, nil
}

func (w *upkeepScopeWorld) ListContentCaptureIncompleteSessionsAfter(_ context.Context, _ SessionID, _ int, scope StoreScope) ([]ContentCaptureIncompleteSession, error) {
	w.recordStore("ListContentCaptureIncompleteSessionsAfter", scope)
	return nil, nil
}

func (w *upkeepScopeWorld) ListMetricSessions(_ context.Context, after SessionID, _ int, scope StoreScope) ([]MetricSession, error) {
	w.recordStore("ListMetricSessions", scope)
	if after != "" || len(w.metricPages) == 0 {
		return nil, nil
	}
	page := w.metricPages[0]
	w.metricPages = w.metricPages[1:]
	return page, nil
}

func (w *upkeepScopeWorld) EnsureSessionMetrics(_ context.Context, id SessionID) (bool, bool, error) {
	return w.changed[id], true, nil
}

// scopedUpkeepPipeline is a run configuration whose two narrowings agree, as
// LOCATE leaves them: the scope names the sessions the user asked for, and the
// allowlist is the upkeep set the SQL passes may touch.
func scopedUpkeepPipeline(t *testing.T, selected, upkeep []SessionID) (*Pipeline, *upkeepScopeWorld) {
	t.Helper()
	scope, err := NewSessionScope(selected, RelatedInclude)
	if err != nil {
		t.Fatalf("build the session scope: %v", err)
	}
	allowed := make(map[SessionID]bool, len(upkeep))
	for _, id := range upkeep {
		allowed[id] = true
	}
	world := &upkeepScopeWorld{}
	pipeline := &Pipeline{
		config: PipelineConfig{Scope: &scope, AllowedSessionIDs: allowed},
		store:  world,
	}
	pipeline.metricsStore = world
	pipeline.analyzer = world
	return pipeline, world
}

// fullUpkeepPipeline is the same composition without a scope: the full-run
// control for every pass.
func fullUpkeepPipeline() (*Pipeline, *upkeepScopeWorld) {
	world := &upkeepScopeWorld{}
	pipeline := &Pipeline{store: world}
	pipeline.metricsStore = world
	pipeline.analyzer = world
	return pipeline, world
}

func upkeepScopeIDs(t *testing.T, pipeline *Pipeline) []string {
	t.Helper()
	ids, narrowed := pipeline.runStoreScope().Narrowed()
	if !narrowed {
		t.Fatal("the scoped pipeline's store scope is not narrowed")
	}
	narrowedIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		narrowedIDs = append(narrowedIDs, string(id))
	}
	sort.Strings(narrowedIDs)
	return narrowedIDs
}

// TestEveryStoreWidePassTakesTheRunScope proves the argument each pass
// receives: a scoped run hands it the upkeep set, and a full run hands it the
// all-store value. The upkeep set includes a stored-but-unreadable member,
// because a full run still maintains that row's stored state.
func TestEveryStoreWidePassTakesTheRunScope(t *testing.T) {
	var fixtures upkeepScopeFixture
	decodeOneFixture(t, upkeepScopeFixtureData, &fixtures, "upkeep scope fixture")
	present := make(map[string]bool, len(fixtures.Cases))
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || len(fixture.Operations) == 0 {
			t.Fatalf("upkeep scope fixture %q names no operations", fixture.Name)
		}
		if present[fixture.Name] {
			t.Fatalf("upkeep scope fixture repeats %q", fixture.Name)
		}
		present[fixture.Name] = true
	}
	requireNamedCases(t, "upkeep scope fixture", fixtures.Required, present)

	selected := SessionID("11111111-1111-4111-8111-111111111111")
	unreadable := SessionID("22222222-2222-4222-8222-222222222222")
	upkeep := []SessionID{selected, unreadable}

	runners := map[string]func(*Pipeline){
		"flagged-sweep": func(p *Pipeline) {
			if err := p.sweepFlaggedSessionsAtStart(context.Background()); err != nil {
				t.Fatalf("sweep flagged sessions: %v", err)
			}
		},
		"stale-origin-listing": func(p *Pipeline) {
			if _, err := p.resolveStoredOrigins(context.Background()); err != nil {
				t.Fatalf("resolve stored origins: %v", err)
			}
		},
		"parent-origin-requeue": func(p *Pipeline) {
			p.reconcileOrphanParentCaches(context.Background(), nil, nil)
		},
		"stale-adapter-listing": func(p *Pipeline) {
			p.appendStoredAdapterWork(context.Background(), nil, nil)
		},
		"stale-index-listing": func(p *Pipeline) {
			if _, err := p.pairRepairCandidateIDs(context.Background()); err != nil {
				t.Fatalf("pair repair candidates: %v", err)
			}
		},
		"repair-listing": func(p *Pipeline) {
			p.repairSessions(context.Background())
		},
		"incomplete-content-listing": func(p *Pipeline) {
			if _, _, _, err := p.backfillIncompleteContent(context.Background(), 0); err != nil {
				t.Fatalf("backfill incomplete content: %v", err)
			}
		},
		"metric-session-listing": func(p *Pipeline) {
			p.refreshStoredMetrics(context.Background())
		},
	}

	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			runner, ok := runners[fixture.Name]
			if !ok {
				t.Fatalf("upkeep scope fixture %q has no runner", fixture.Name)
			}

			scoped, scopedWorld := scopedUpkeepPipeline(t, []SessionID{selected}, upkeep)
			runner(scoped)
			assertRecordedScopes(t, scopedWorld.calls.Calls(), fixture.Operations, upkeepScopeIDs(t, scoped))

			full, fullWorld := fullUpkeepPipeline()
			runner(full)
			assertRecordedScopes(t, fullWorld.calls.Calls(), fixture.Operations, nil)
		})
	}
}

// assertRecordedScopes checks the exact per-pass call set and, for each call,
// whether it carried the all-store value or the expected narrowed ids.
func assertRecordedScopes(t *testing.T, calls []scoperecorder.Call, wantOperations []string, wantIDs []string) {
	t.Helper()
	recorded := make(map[string]scoperecorder.Call, len(calls))
	for _, call := range calls {
		if _, duplicate := recorded[call.Operation]; duplicate {
			t.Fatalf("%s recorded twice in one pass run", call.Operation)
		}
		recorded[call.Operation] = call
	}
	if len(recorded) != len(wantOperations) {
		t.Fatalf("recorded operations %v, want %v", operationNames(recorded), wantOperations)
	}
	for _, operation := range wantOperations {
		call, ok := recorded[operation]
		if !ok {
			t.Fatalf("operation %s did not record; recorded %v", operation, operationNames(recorded))
		}
		if call.Kind != scoperecorder.KindStore {
			t.Fatalf("%s recorded kind %v, want a session scope", operation, call.Kind)
		}
		if wantIDs == nil {
			if !call.All {
				t.Fatalf("%s carried a narrowed scope on a full run", operation)
			}
			continue
		}
		if call.All {
			t.Fatalf("%s carried the all-store scope on a scoped run", operation)
		}
		if len(call.IDs) != len(wantIDs) {
			t.Fatalf("%s ids %v, want the upkeep set %v", operation, call.IDs, wantIDs)
		}
		for i := range wantIDs {
			if call.IDs[i] != wantIDs[i] {
				t.Fatalf("%s ids %v, want the upkeep set %v", operation, call.IDs, wantIDs)
			}
		}
	}
}

func operationNames(recorded map[string]scoperecorder.Call) []string {
	names := make([]string, 0, len(recorded))
	for operation := range recorded {
		names = append(names, operation)
	}
	sort.Strings(names)
	return names
}

// TestScopedParentHealTouchesOnlyTheUpkeepSet pins the one pass whose intended
// target set differs from its healed set. The targets are the cohort's
// processed independently admitted members; the healed children are only those
// in the upkeep set, and a child rewritten by this run is never healed.
func TestScopedParentHealTouchesOnlyTheUpkeepSet(t *testing.T) {
	parent := SessionID("33333333-3333-4333-8333-333333333333")
	upkeepChild := SessionID("44444444-4444-4444-8444-444444444444")
	writtenChild := SessionID("55555555-5555-4555-8555-555555555555")
	sibling := SessionID("66666666-6666-4666-8666-666666666666")

	pipeline, world := scopedUpkeepPipeline(t,
		[]SessionID{parent},
		[]SessionID{parent, upkeepChild, writtenChild},
	)
	world.candidates = []ParentCacheReconcile{
		{Child: upkeepChild, Parent: parent},
		{Child: writtenChild, Parent: parent},
		{Child: sibling, Parent: parent},
	}
	discovered := []DiffEntry{
		{Session: DiscoveredSession{SessionID: parent, Harness: HarnessCodex}},
		{Session: DiscoveredSession{SessionID: SessionID("77777777-7777-4777-8777-777777777777"), Harness: HarnessClaudeCode}},
	}
	written := []DiffEntry{{Session: DiscoveredSession{SessionID: writtenChild, Harness: HarnessCodex}}}

	pipeline.reconcileOrphanParentCaches(context.Background(), discovered, written)

	if len(world.parents) != 1 || world.parents[0] != parent {
		t.Fatalf("heal targets %v, want only the processed independent-admission member %s", world.parents, parent)
	}
	wantHarnesses := []Harness{HarnessCodex, HarnessOpenCode}
	if len(world.harnesses) != len(wantHarnesses) {
		t.Fatalf("heal harnesses %v, want %v", world.harnesses, wantHarnesses)
	}
	for i := range wantHarnesses {
		if world.harnesses[i] != wantHarnesses[i] {
			t.Fatalf("heal harnesses %v, want %v", world.harnesses, wantHarnesses)
		}
	}
	if len(world.reconciled) != 1 || world.reconciled[0].Child != upkeepChild {
		t.Fatalf("healed children %v, want only the upkeep child %s", world.reconciled, upkeepChild)
	}
	// The requeue pass is narrowed with the same upkeep set the heal uses.
	calls := world.calls.Calls()
	if len(calls) != 1 || calls[0].Operation != "InvalidateStaleParentOrigins" || calls[0].All {
		t.Fatalf("parent-origin requeue recorded %v, want one narrowed call", calls)
	}
}

// TestScopedOrphanCleanupNamesProjectsFromBeforeAndAfter proves the project
// set a scoped cleanup may consider: the project the cohort held before the
// run, the project it holds after, and an unreadable member's project. The
// old project of a moved session is only reachable from the pre-write side.
func TestScopedOrphanCleanupNamesProjectsFromBeforeAndAfter(t *testing.T) {
	moved := SessionID("88888888-8888-4888-8888-888888888888")
	unreadable := SessionID("99999999-9999-4999-8999-999999999999")
	const (
		beforeHash     ProjectHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		afterHash      ProjectHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		unreadableHash ProjectHash = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	)

	pipeline, world := scopedUpkeepPipeline(t, []SessionID{moved}, []SessionID{moved, unreadable})
	pipeline.locationCache = map[SessionID]SessionLocation{
		moved: {ProjectHash: beforeHash},
	}
	world.locations = map[SessionID]SessionLocation{
		moved:      {ProjectHash: afterHash},
		unreadable: {ProjectHash: unreadableHash},
	}

	pipeline.cleanupOrphanProjects(context.Background(), "test")

	calls := world.calls.Calls()
	if len(calls) != 1 || calls[0].Operation != "CleanupOrphanProjects" {
		t.Fatalf("orphan cleanup recorded %v, want one project-scope call", calls)
	}
	if calls[0].Kind != scoperecorder.KindProjects || calls[0].All {
		t.Fatalf("orphan cleanup scope %+v, want a narrowed project scope", calls[0])
	}
	want := []string{string(beforeHash), string(afterHash), string(unreadableHash)}
	sort.Strings(want)
	got := append([]string(nil), calls[0].ProjectHashes...)
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("cleanup projects %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cleanup projects %v, want %v", got, want)
		}
	}

	full, fullWorld := fullUpkeepPipeline()
	full.cleanupOrphanProjects(context.Background(), "test")
	fullCalls := fullWorld.calls.Calls()
	if len(fullCalls) != 1 || !fullCalls[0].All {
		t.Fatalf("full-run orphan cleanup %v, want the all-project scope", fullCalls)
	}
}

// TestScopedOriginPassReadsEvidenceByPath proves the origin pass asks the
// evidence cache only for the rows it judges: the whole-table loader is never
// called, so nothing outside the cohort's stale rows is read.
func TestScopedOriginPassReadsEvidenceByPath(t *testing.T) {
	judged := SessionID("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	path := ResolvedPath("/synthetic/origin/judged.jsonl")
	world := &upkeepScopeWorld{}
	world.rows = []StoredOriginRow{{SessionID: judged, ParentID: "parent", SourcePath: path}}

	resolver, err := NewOriginResolver(world, world, nil)
	if err != nil {
		t.Fatalf("build origin resolver: %v", err)
	}
	if _, err := resolver.ResolveStoredOrigins(context.Background(), OriginRuleVersion, StoredSessions([]SessionID{judged})); err != nil {
		t.Fatalf("resolve stored origins: %v", err)
	}

	calls := world.calls.Calls()
	seen := make(map[string]scoperecorder.Call, len(calls))
	for _, call := range calls {
		seen[call.Operation] = call
	}
	if _, wholeTable := seen["LoadClaudeEvidence"]; wholeTable {
		t.Fatal("the scoped origin pass read the whole evidence table")
	}
	byPath, ok := seen["LoadClaudeEvidenceFor"]
	if !ok {
		t.Fatal("the scoped origin pass did not read evidence by path")
	}
	if byPath.All || len(byPath.IDs) != 0 {
		t.Fatalf("by-path read recorded %+v, want no session scope", byPath)
	}
	row, ok := seen["ListStaleOriginSessions"]
	if !ok || row.All {
		t.Fatalf("origin listing recorded %+v, want one narrowed call", row)
	}
	if len(row.IDs) != 1 || row.IDs[0] != string(judged) {
		t.Fatalf("origin listing ids %v, want [%s]", row.IDs, judged)
	}
	if len(world.evidencePaths) != 1 || world.evidencePaths[0] != path {
		t.Fatalf("evidence paths %v, want [%s]", world.evidencePaths, path)
	}
}

// TestScopedMetricsRefreshDaysCoverOnlyUpkeep proves the insights inputs
// narrow: the refreshed start day of every upkeep member is returned, and a
// session outside the upkeep set contributes no day even when a store page
// hands it back. The day-set code itself is unchanged.
func TestScopedMetricsRefreshDaysCoverOnlyUpkeep(t *testing.T) {
	selected := SessionID("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	unreadable := SessionID("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	outsider := SessionID("dddddddd-dddd-4ddd-8ddd-dddddddddddd")

	const (
		selectedDay   = int64(1767312000000) // 2026-01-02 UTC
		unreadableDay = int64(1767398400000) // 2026-01-03 UTC
		outsiderDay   = int64(1767484800000) // 2026-01-04 UTC
	)

	pipeline, world := scopedUpkeepPipeline(t, []SessionID{selected}, []SessionID{selected, unreadable})
	world.metricPages = [][]MetricSession{{
		{SessionID: selected, Harness: HarnessClaudeCode, StartMS: selectedDay},
		{SessionID: unreadable, Harness: HarnessClaudeCode, StartMS: unreadableDay},
		{SessionID: outsider, Harness: HarnessClaudeCode, StartMS: outsiderDay},
	}}
	world.changed = map[SessionID]bool{selected: true, unreadable: true, outsider: true}

	_, _, _, days := pipeline.refreshStoredMetrics(context.Background())

	if days["2026-01-02"] != true {
		t.Fatalf("refreshed days %v, want the processed member's day", days)
	}
	if days["2026-01-03"] != true {
		t.Fatalf("refreshed days %v, want the unreadable upkeep member's day", days)
	}
	if days["2026-01-04"] {
		t.Fatalf("refreshed days %v, want no day for a session outside the upkeep set", days)
	}

	calls := world.calls.Calls()
	if len(calls) == 0 {
		t.Fatal("the metrics refresh recorded no listing")
	}
	for _, call := range calls {
		if call.Operation != "ListMetricSessions" {
			t.Fatalf("metrics refresh recorded %s, want only the listing", call.Operation)
		}
		if call.All {
			t.Fatal("the metrics listing carried the all-store scope on a scoped run")
		}
	}
}

// TestRunStoreScopeNeverWidens proves the two full-run and narrowed values, and
// that a scoped run with no allowlist selects nothing rather than everything.
func TestRunStoreScopeNeverWidens(t *testing.T) {
	full := &Pipeline{}
	if !full.runStoreScope().IsAll() {
		t.Fatal("a full run's store scope is not the all-store value")
	}

	scope, err := NewSessionScope([]SessionID{"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"}, RelatedInclude)
	if err != nil {
		t.Fatalf("build the session scope: %v", err)
	}
	narrowed := &Pipeline{config: PipelineConfig{Scope: &scope}}
	ids, ok := narrowed.runStoreScope().Narrowed()
	if !ok || len(ids) != 0 {
		t.Fatalf("a scoped run with no allowlist selected %v, want no session", ids)
	}
}

// TestScopedRunDoesNotReportStaleIndexOutsideSelection pins the one diagnostic
// that changes under a scope: the count of stored sessions outside the
// selection is a full-run report, so a scoped run computes none of it, while the
// full run still reports the same entry it always did.
func TestScopedRunDoesNotReportStaleIndexOutsideSelection(t *testing.T) {
	scoped, _ := scopedUpkeepPipeline(t, []SessionID{"ffffffff-ffff-4fff-8fff-ffffffffffff"}, []SessionID{"ffffffff-ffff-4fff-8fff-ffffffffffff"})
	scoped.reportStaleIndexOutsideSelection(2)
	if diagnostics := scoped.snapshotDiagnostics(); len(diagnostics) != 0 {
		t.Fatalf("scoped run reported %v, want no outside-selection diagnostic", diagnostics)
	}

	full, _ := fullUpkeepPipeline()
	full.reportStaleIndexOutsideSelection(2)
	diagnostics := full.snapshotDiagnostics()
	if len(diagnostics) != 1 || diagnostics[0].ErrorType != "stale_index_outside_selection" {
		t.Fatalf("full run reported %v, want the outside-selection diagnostic", diagnostics)
	}

	full.reportStaleIndexOutsideSelection(0)
	if diagnostics := full.snapshotDiagnostics(); len(diagnostics) != 1 {
		t.Fatalf("a zero count reported %v, want no diagnostic", diagnostics)
	}
}
