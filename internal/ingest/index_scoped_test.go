package ingest_test

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testkit/scoperecorder"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/index_scoped.yaml
var indexScopedYAML []byte

type indexScopedFixture struct {
	RequiredNames []string          `yaml:"requiredNames"`
	Cases         []indexScopedCase `yaml:"cases"`
}

type indexScopedCase struct {
	Name           string             `yaml:"name"`
	Pairs          []indexPairSeed    `yaml:"pairs"`
	Stored         []indexStoredSeed  `yaml:"stored"`
	CodexSources   []indexCodexSource `yaml:"codexSources"`
	RealCodex      bool               `yaml:"realCodex"`
	CodexRoots     []string           `yaml:"codexRoots"`
	UnreadableDirs []string           `yaml:"unreadableDirs"`
	// CodexEnumeration is the coverage the codex adapter reports for the walk
	// it ran: empty (or "complete") is the whole tree; "partial" is a walk
	// that swallowed a directory-read failure.
	CodexEnumeration string `yaml:"codexEnumeration"`
	// CodexEnumerationOnly names rollout files the walk enumerated that never
	// became discovered sessions (a refused copy, a rollout no harvest
	// stores). They belong in the rebuild input and not in the session list,
	// which is the difference the enumeration exists to carry.
	CodexEnumerationOnly []indexCodexSource `yaml:"codexEnumerationOnly"`
	// SeedCatalog is the catalog the store already holds before the run, as
	// path/id pairs; it is written through the complete-enumeration rebuild.
	SeedCatalog []indexCodexSource `yaml:"seedCatalog"`
	// CodexDisabled registers a Codex adapter but leaves its source disabled,
	// which a `--all` run must treat as out of scope rather than as a source it
	// failed to walk.
	CodexDisabled bool `yaml:"codexDisabled"`
	// CodexWithoutAdapter enables the source without registering an adapter for
	// it, which a `--all` run cannot enumerate.
	CodexWithoutAdapter bool `yaml:"codexWithoutAdapter"`
	// WantRunErrorContains is the refusal a case expects; it replaces the
	// post-run assertions.
	WantRunErrorContains string `yaml:"wantRunErrorContains"`
	// WantCatalogAfter is the exact catalog the store holds after the run.
	WantCatalogAfter   []string          `yaml:"wantCatalogAfter"`
	Selected           []string          `yaml:"selected"`
	Related            string            `yaml:"related"`
	Force              bool              `yaml:"force"`
	RebuildAll         bool              `yaml:"rebuildAll"`
	SinceMs            int64             `yaml:"sinceMs"`
	WantNoRecompute    bool              `yaml:"wantNoRecompute"`
	WantMembers        []indexWantMember `yaml:"wantMembers"`
	WantMisses         []indexWantMiss   `yaml:"wantMisses"`
	WantProcessed      []string          `yaml:"wantProcessed"`
	WantRebuildCatalog []string          `yaml:"wantRebuildCatalog"`
}

type indexPairSeed struct {
	Session string `yaml:"session"`
	Host    string `yaml:"host"`
	Parent  string `yaml:"parent"`
}

type indexStoredSeed struct {
	Session       string `yaml:"session"`
	Host          string `yaml:"host"`
	Parent        string `yaml:"parent"`
	Harness       string `yaml:"harness"`
	NativeSource  bool   `yaml:"nativeSource"`
	LogicalParent string `yaml:"logicalParent"`
}

type indexCodexSource struct {
	Session string `yaml:"session"`
	Path    string `yaml:"path"`
}

type indexWantMember struct {
	Session string `yaml:"session"`
	Outcome string `yaml:"outcome"`
	Role    string `yaml:"role"`
	Reason  string `yaml:"reason"`
}

type indexWantMiss struct {
	Session string `yaml:"session"`
	Reason  string `yaml:"reason"`
}

// indexScopedStore is the one store a scoped index case reads through. It
// records the scope P7/P8 receive and the catalog `--all` rebuilds from, then
// delegates to the real store.
type indexScopedStore struct {
	*store.Store
	calls scoperecorder.Recorder

	// logicalParents fabricates the durable logical parent the store records
	// for a row whose parent cache is nil. It stands in for a child stored
	// before its parent, which is the shape a repeated harvest would have
	// recorded through relationship evidence.
	logicalParents map[ingest.SessionID]ingest.SessionID

	mu       sync.Mutex
	rebuilds []ingest.RolloutEnumeration
}

var _ ingest.DerivedLookupRebuilder = (*indexScopedStore)(nil)

func (s *indexScopedStore) StoredSessionSources(ctx context.Context, ids []ingest.SessionID) (map[ingest.SessionID]ingest.StoredSessionSource, error) {
	result, err := s.Store.StoredSessionSources(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, parent := range s.logicalParents {
		row, ok := result[id]
		if !ok {
			continue
		}
		parent := parent
		row.Parent = &parent
		result[id] = row
	}
	return result, nil
}

func (s *indexScopedStore) ListStaleIndexSessions(ctx context.Context, targets map[ingest.Harness]ingest.HarvesterVersions, scope ingest.StoreScope) ([]ingest.SessionID, error) {
	s.recordScope("ListStaleIndexSessions", scope)
	return s.Store.ListStaleIndexSessions(ctx, targets, scope)
}

func (s *indexScopedStore) ListSessionsNeedingRepair(ctx context.Context, targets map[ingest.Harness]ingest.HarvesterVersions, scope ingest.StoreScope) ([]ingest.SessionID, error) {
	s.recordScope("ListSessionsNeedingRepair", scope)
	return s.Store.ListSessionsNeedingRepair(ctx, targets, scope)
}

func (s *indexScopedStore) RebuildDerivedLookups(ctx context.Context, enumeration ingest.RolloutEnumeration) error {
	s.mu.Lock()
	s.rebuilds = append(s.rebuilds, ingest.RolloutEnumeration{
		Status: enumeration.Status,
		Files:  append([]ingest.RolloutFile(nil), enumeration.Files...),
	})
	s.mu.Unlock()
	return s.Store.RebuildDerivedLookups(ctx, enumeration)
}

func (s *indexScopedStore) recordScope(operation string, scope ingest.StoreScope) {
	ids, _ := scope.Narrowed()
	narrowed := make([]string, 0, len(ids))
	for _, id := range ids {
		narrowed = append(narrowed, string(id))
	}
	s.calls.RecordStore(operation, narrowed, scope.IsAll())
}

func (s *indexScopedStore) rebuildCatalogs() []ingest.RolloutEnumeration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ingest.RolloutEnumeration, len(s.rebuilds))
	for i, enumeration := range s.rebuilds {
		out[i] = ingest.RolloutEnumeration{
			Status: enumeration.Status,
			Files:  append([]ingest.RolloutFile(nil), enumeration.Files...),
		}
	}
	return out
}

// indexScopedCodexAdapter is the stub Codex adapter of a scoped index case. It
// reports the rollout enumeration the case describes, which is the input the
// derived lookup rebuild consumes.
type indexScopedCodexAdapter struct {
	*testutil.StubAdapter
	enumeration ingest.RolloutEnumeration
}

var _ ingest.RolloutEnumerator = (*indexScopedCodexAdapter)(nil)

func (a *indexScopedCodexAdapter) EnumeratedRollouts() ingest.RolloutEnumeration {
	return ingest.RolloutEnumeration{
		Status: a.enumeration.Status,
		Files:  append([]ingest.RolloutFile(nil), a.enumeration.Files...),
	}
}

func makeIndexScopedCodexAdapter(sessions []ingest.DiscoveredSession, enumeration ingest.RolloutEnumeration) ingest.AdapterFactory {
	return func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter {
		return &indexScopedCodexAdapter{
			StubAdapter: &testutil.StubAdapter{
				ProviderValue: ingest.HarnessCodex,
				Sessions:      sessions,
			},
			enumeration: enumeration,
		}
	}
}

func loadIndexScopedFixture(t *testing.T) indexScopedFixture {
	t.Helper()
	var fixture indexScopedFixture
	if err := testutil.DecodeNamedFixtureYAML(indexScopedYAML, &fixture); err != nil {
		t.Fatalf("decode the session-scoped index fixture: %v", err)
	}
	for _, testCase := range fixture.Cases {
		noCatalog := testCase.WantRunErrorContains != "" || testCase.CodexEnumeration == "none" || testCase.CodexWithoutAdapter
		if testCase.RebuildAll && len(testCase.WantRebuildCatalog) == 0 && !noCatalog {
			t.Fatalf("case %q rebuilds the derived lookups but names no expected catalog", testCase.Name)
		}
		if !testCase.RebuildAll && len(testCase.Selected) == 0 {
			t.Fatalf("case %q is scoped but names no selected session", testCase.Name)
		}
	}
	return fixture
}

func TestIndexScoped(t *testing.T) {
	t.Parallel()
	fixture := loadIndexScopedFixture(t)
	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			runIndexScopedCase(t, testCase)
		})
	}
}

func runIndexScopedCase(t *testing.T, testCase indexScopedCase) {
	t.Helper()
	ctx := context.Background()
	mfs := testutil.NewMemFS()
	faults := map[string]struct{}{}
	for _, dir := range testCase.UnreadableDirs {
		faults[dir] = struct{}{}
	}
	walkFS := &codexCatalogWalkFS{MemFS: mfs, unreadable: faults}
	counting := testutil.NewCountingFS(mfs)
	db := storetest.Open(t)
	if len(testCase.SeedCatalog) > 0 {
		seed := make([]ingest.RolloutFile, 0, len(testCase.SeedCatalog))
		for _, row := range testCase.SeedCatalog {
			seed = append(seed, ingest.RolloutFile{
				Path:      ingest.ResolvedPath(row.Path),
				SessionID: ingest.SessionID(row.Session),
			})
		}
		if err := db.RebuildDerivedLookups(ctx, ingest.RolloutEnumeration{
			Status: ingest.RolloutEnumerationComplete,
			Files:  seed,
		}); err != nil {
			t.Fatalf("case %q: seed the rollout catalog: %v", testCase.Name, err)
		}
	}
	recording := &indexScopedStore{Store: db}

	for _, pair := range testCase.Pairs {
		writeIndexScopedPair(t, mfs, pair)
	}
	for _, row := range testCase.Stored {
		seedIndexScopedRow(t, ctx, mfs, db, row)
		if row.LogicalParent != "" {
			if recording.logicalParents == nil {
				recording.logicalParents = make(map[ingest.SessionID]ingest.SessionID)
			}
			recording.logicalParents[ingest.SessionID(row.Session)] = ingest.SessionID(row.LogicalParent)
		}
	}

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}
	var codexSessions []ingest.DiscoveredSession
	for _, source := range testCase.CodexSources {
		session := makeDiscoveredSession(t, source.Session, source.Path, time.UnixMilli(1708300800000))
		session.Harness = ingest.HarnessCodex
		codexSessions = append(codexSessions, session)
		if testCase.RealCodex {
			body := `{"type":"session_meta","payload":{"id":"` + source.Session + `","cwd":"/work/project"}}` + "\n"
			if err := mfs.WriteFile(source.Path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The rollout enumeration a real Codex walk reports: what it enumerated,
	// which is NOT the discovered session list (a rollout no harvest stores is
	// still a row) and is marked partial when the walk could not read the whole
	// tree.
	var codexEnumeration ingest.RolloutEnumeration
	if testCase.CodexEnumeration == "partial" {
		codexEnumeration.Status = ingest.RolloutEnumerationPartial
	} else {
		codexEnumeration.Status = ingest.RolloutEnumerationComplete
	}
	for _, source := range testCase.CodexSources {
		codexEnumeration.Files = append(codexEnumeration.Files, ingest.RolloutFile{
			Path:      ingest.ResolvedPath(source.Path),
			SessionID: ingest.SessionID(source.Session),
		})
	}
	for _, source := range testCase.CodexEnumerationOnly {
		codexEnumeration.Files = append(codexEnumeration.Files, ingest.RolloutFile{
			Path:      ingest.ResolvedPath(source.Path),
			SessionID: ingest.SessionID(source.Session),
		})
	}

	cfg := makePipelineConfig(testOutputDir, func(cfg *ingest.PipelineConfig) {
		cfg.Reindex = true
		cfg.Force = testCase.Force
		cfg.RebuildAll = testCase.RebuildAll
		cfg.Command = "harvest index"
		if testCase.RebuildAll {
			// `--all` clears the session filters: these two would exclude the
			// only saved pair if they survived.
			cfg.AllowedSessionIDs = map[ingest.SessionID]bool{"99999999-9999-4999-8999-999999999999": true}
			future := time.UnixMilli(4102444800000)
			cfg.Since = &future
		}
		if testCase.SinceMs != 0 {
			since := time.UnixMilli(testCase.SinceMs)
			cfg.Since = &since
		}
		if len(codexSessions) > 0 {
			cfg.Sources[ingest.HarnessCodex] = ingest.SourceConfig{
				Paths:   []ingest.ResolvedPath{ingest.ResolvedPath("/codex")},
				Enabled: true,
			}
			adapters[ingest.HarnessCodex] = makeIndexScopedCodexAdapter(codexSessions, codexEnumeration)
		}
		if testCase.RealCodex {
			var roots []ingest.ResolvedPath
			for _, root := range testCase.CodexRoots {
				roots = append(roots, ingest.ResolvedPath(root))
			}
			cfg.Sources[ingest.HarnessCodex] = ingest.SourceConfig{Paths: roots, Enabled: true}
			adapters[ingest.HarnessCodex] = func(fsys ingest.FileSystem, git ingest.GitResolver, sal salt.Salt) ingest.SourceAdapter {
				return ingest.NewCodexAdapter(walkFS, git, sal)
			}
		}
		if testCase.CodexWithoutAdapter {
			cfg.Sources[ingest.HarnessCodex] = ingest.SourceConfig{
				Paths:   []ingest.ResolvedPath{ingest.ResolvedPath("/codex")},
				Enabled: true,
			}
		}
		if testCase.CodexDisabled {
			cfg.Sources[ingest.HarnessCodex] = ingest.SourceConfig{
				Paths:   []ingest.ResolvedPath{ingest.ResolvedPath("/codex")},
				Enabled: false,
			}
			adapters[ingest.HarnessCodex] = makeIndexScopedCodexAdapter(codexSessions, codexEnumeration)
		}
		if len(testCase.Selected) > 0 {
			ids := make([]ingest.SessionID, 0, len(testCase.Selected))
			for _, raw := range testCase.Selected {
				id, err := ingest.NewSessionID(raw)
				if err != nil {
					t.Fatalf("case %q: session id %q: %v", testCase.Name, raw, err)
				}
				ids = append(ids, id)
			}
			policy := ingest.RelatedInclude
			if testCase.Related == "exclude" {
				policy = ingest.RelatedExclude
			}
			scope, err := ingest.NewSessionScope(ids, policy)
			if err != nil {
				t.Fatalf("case %q: NewSessionScope: %v", testCase.Name, err)
			}
			cfg.Scope = &scope
		}
	})

	pipeline, err := newTestPipeline(counting, testutil.DefaultGitResolver(), adapters, cfg,
		ingest.WithIndexers(ingest.NewIndexerRegistry(counting, ingest.IndexerRegistryOptions{})),
		ingest.WithHarvesterVersions(maps.Clone(ingest.HarvesterVersionRegistry)),
		ingest.WithStore(recording),
		ingest.WithMetricsStore(recording),
		ingest.WithIndexLogger(recording),
	)
	if err != nil {
		t.Fatalf("case %q: NewPipeline: %v", testCase.Name, err)
	}

	result, runErr := pipeline.Run(ctx)

	// A refused rebuild writes nothing: the run fails loudly and the catalog
	// the store already held is exactly what it still holds.
	if testCase.WantRunErrorContains != "" {
		if runErr == nil {
			t.Fatalf("case %q: Run succeeded, want the refusal naming %q", testCase.Name, testCase.WantRunErrorContains)
		}
		if !strings.Contains(runErr.Error(), testCase.WantRunErrorContains) {
			t.Fatalf("case %q: Run error = %v, want it to name %q", testCase.Name, runErr, testCase.WantRunErrorContains)
		}
		assertIndexScopedCatalog(t, testCase, ctx, db, testCase.WantCatalogAfter)
		return
	}

	// The point of the scoped path: the retained tree is never walked. A
	// WalkDir under the output root is forbidden outright, and no directory
	// below the root is read either: the only enumeration is the root's host
	// list every probe is bounded by.
	if testCase.RebuildAll {
		if below := counting.CountUnder(testutil.FSOpReadDir, testOutputDir) - counting.Count(testutil.FSOpReadDir, testOutputDir); below == 0 {
			t.Fatalf("case %q: `--all` must run the whole-tree reader; directory reads below %s = 0 (run error: %v)", testCase.Name, testOutputDir, runErr)
		}
	} else {
		if walks := counting.CountUnder(testutil.FSOpWalkDir, testOutputDir); walks != 0 {
			t.Fatalf("case %q: a session-scoped index run must not walk the output root; WalkDir under %s = %d", testCase.Name, testOutputDir, walks)
		}
		if below := counting.CountUnder(testutil.FSOpReadDir, testOutputDir) - counting.Count(testutil.FSOpReadDir, testOutputDir); below != 0 {
			t.Fatalf("case %q: a session-scoped index run must read only the output root's host list; directory reads below %s = %d", testCase.Name, testOutputDir, below)
		}
	}

	if len(testCase.WantMisses) > 0 {
		assertIndexScopedMiss(t, testCase, runErr, result)
		return
	}
	if runErr != nil {
		t.Fatalf("case %q: Run: %v", testCase.Name, runErr)
	}
	if result == nil {
		t.Fatalf("case %q: Run returned no result", testCase.Name)
	}

	if testCase.RebuildAll {
		assertIndexScopedRebuild(t, testCase, ctx, result, recording, db)
		return
	}

	report := result.ScopeReport
	if report == nil {
		t.Fatalf("case %q: a session-scoped run must report its cohort", testCase.Name)
	}
	assertIndexScopedMembers(t, testCase, *report)
	scopedIndexAssertIDs(t, "Processed()", report.Processed(), testCase.WantProcessed)
	assertIndexScopedUpkeepScopes(t, testCase, report.Upkeep(), recording)
	if testCase.WantNoRecompute {
		assertIndexScopedNoRecompute(t, testCase, counting)
	}
}

// assertIndexScopedNoRecompute proves a stored session whose saved pair is gone
// keeps its stored copy: no pair is written or installed. The source may still
// be read by the store-side upkeep a full run does for the row.
func assertIndexScopedNoRecompute(t *testing.T, testCase indexScopedCase, counting *testutil.CountingFS) {
	t.Helper()
	if writes := counting.CountUnder(testutil.FSOpWriteFile, testOutputDir); writes != 0 {
		t.Errorf("case %q: an unreadable member must not have its pair written; WriteFile under %s = %d", testCase.Name, testOutputDir, writes)
	}
	if renames := counting.CountUnder(testutil.FSOpRename, testOutputDir); renames != 0 {
		t.Errorf("case %q: an unreadable member must not have its pair installed; Rename under %s = %d", testCase.Name, testOutputDir, renames)
	}
}

func assertIndexScopedMembers(t *testing.T, testCase indexScopedCase, report ingest.ScopeReport) {
	t.Helper()
	if len(report.Members) != len(testCase.WantMembers) {
		t.Fatalf("case %q: report members = %+v, want %+v", testCase.Name, report.Members, testCase.WantMembers)
	}
	for i, want := range testCase.WantMembers {
		member := report.Members[i]
		if string(member.ID) != want.Session {
			t.Errorf("case %q: member %d id = %s, want %s", testCase.Name, i, member.ID, want.Session)
		}
		if got := scopedIndexIndexOutcomeName(member.Outcome); got != want.Outcome {
			t.Errorf("case %q: member %s outcome = %s, want %s", testCase.Name, member.ID, got, want.Outcome)
		}
		if got := scopedIndexIndexRoleName(member.Role); got != want.Role {
			t.Errorf("case %q: member %s role = %s, want %s", testCase.Name, member.ID, got, want.Role)
		}
		if want.Reason != "" && member.Unreadable.String() != scopedIndexIndexReasonName(want.Reason) {
			t.Errorf("case %q: member %s unreadable = %s, want %s", testCase.Name, member.ID, member.Unreadable, want.Reason)
		}
	}
}

func assertIndexScopedMiss(t *testing.T, testCase indexScopedCase, runErr error, result *ingest.PipelineResult) {
	t.Helper()
	if result != nil {
		t.Errorf("case %q: a refused selection recorded a result; nothing may be written", testCase.Name)
	}
	var scopeErr *ingest.SessionScopeError
	if !errors.As(runErr, &scopeErr) {
		t.Fatalf("case %q: error = %v, want *SessionScopeError", testCase.Name, runErr)
	}
	if scopeErr.Command != "harvest index" {
		t.Errorf("case %q: miss command = %q, want harvest index", testCase.Name, scopeErr.Command)
	}
	for _, want := range testCase.WantMisses {
		if want.Reason != "not-found" {
			continue
		}
		if fix := scopeErr.FixLine(); !strings.Contains(fix, "harvest index` once without --session") {
			t.Errorf("case %q: fix %q must name `harvest index` without --session", testCase.Name, fix)
		}
	}
	if len(scopeErr.Misses) != len(testCase.WantMisses) {
		t.Fatalf("case %q: misses = %+v, want %+v", testCase.Name, scopeErr.Misses, testCase.WantMisses)
	}
	for i, want := range testCase.WantMisses {
		miss := scopeErr.Misses[i]
		if string(miss.ID) != want.Session {
			t.Errorf("case %q: miss %d id = %s, want %s", testCase.Name, i, miss.ID, want.Session)
		}
		if got := scopedIndexIndexMissReasonName(miss.Reason); got != want.Reason {
			t.Errorf("case %q: miss %s reason = %s, want %s", testCase.Name, miss.ID, got, want.Reason)
		}
	}
}

func assertIndexScopedRebuild(t *testing.T, testCase indexScopedCase, ctx context.Context, result *ingest.PipelineResult, recording *indexScopedStore, db *store.Store) {
	t.Helper()
	if result.ScopeReport != nil {
		t.Errorf("case %q: `--all` is never scoped; ScopeReport = %+v", testCase.Name, result.ScopeReport)
	}
	if result.Summary.RebuiltFromFiles != len(testCase.Pairs) {
		t.Errorf("case %q: `--all` must record every saved pair; RebuiltFromFiles = %d, want %d", testCase.Name, result.Summary.RebuiltFromFiles, len(testCase.Pairs))
	}
	enumerations := recording.rebuildCatalogs()
	if len(enumerations) != 1 {
		t.Fatalf("case %q: RebuildDerivedLookups calls = %d, want 1", testCase.Name, len(enumerations))
	}
	wantStatus := ingest.RolloutEnumerationComplete
	if testCase.CodexEnumeration == "none" {
		wantStatus = ingest.RolloutEnumerationNotWalked
	} else if testCase.CodexEnumeration == "partial" || testCase.CodexWithoutAdapter {
		wantStatus = ingest.RolloutEnumerationPartial
	}
	if enumerations[0].Status != wantStatus {
		t.Fatalf("case %q: rebuild input status = %v, want %v", testCase.Name, enumerations[0].Status, wantStatus)
	}
	got := make([]string, 0, len(enumerations[0].Files))
	for _, file := range enumerations[0].Files {
		got = append(got, file.Path.String()+"="+string(file.SessionID))
	}
	sort.Strings(got)
	want := append([]string(nil), testCase.WantRebuildCatalog...)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("case %q: rebuild input catalog = %v, want %v", testCase.Name, got, want)
	}
	// The catalog reflects the expected reconciliation.
	assertIndexScopedCatalog(t, testCase, ctx, db, testCase.WantCatalogAfter)
}

// assertIndexScopedCatalog compares the catalog the store holds with the
// fixture's exact path=id set.
func assertIndexScopedCatalog(t *testing.T, testCase indexScopedCase, ctx context.Context, db *store.Store, want []string) {
	t.Helper()
	rows, err := db.LoadCodexRolloutCatalog(ctx)
	if err != nil {
		t.Fatalf("case %q: load the rollout catalog: %v", testCase.Name, err)
	}
	got := make([]string, 0, len(rows))
	for _, row := range rows {
		got = append(got, row.Path.String()+"="+row.SessionID.String())
	}
	sort.Strings(got)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if strings.Join(got, "\n") != strings.Join(sorted, "\n") {
		t.Errorf("case %q: stored catalog = %v, want %v", testCase.Name, got, sorted)
	}
}

// assertIndexScopedUpkeepScopes proves P7 and P8 received the narrowed upkeep
// set on the scoped path.
func assertIndexScopedUpkeepScopes(t *testing.T, testCase indexScopedCase, upkeep []ingest.SessionID, recording *indexScopedStore) {
	t.Helper()
	want := make([]string, 0, len(upkeep))
	for _, id := range upkeep {
		want = append(want, string(id))
	}
	sort.Strings(want)

	seen := make(map[string]bool, 2)
	for _, call := range recording.calls.Calls() {
		if call.Kind != scoperecorder.KindStore {
			continue
		}
		if call.All {
			t.Errorf("case %q: %s received the all-store scope on a scoped run", testCase.Name, call.Operation)
			continue
		}
		got := append([]string(nil), call.IDs...)
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("case %q: %s scope ids = %v, want the upkeep set %v", testCase.Name, call.Operation, got, want)
		}
		seen[call.Operation] = true
	}
	for _, operation := range []string{"ListStaleIndexSessions", "ListSessionsNeedingRepair"} {
		if !seen[operation] {
			t.Errorf("case %q: %s was not called on the scoped path", testCase.Name, operation)
		}
	}
}

func writeIndexScopedPair(t *testing.T, mfs *testutil.MemFS, pair indexPairSeed) {
	t.Helper()
	meta := makeMinimalMeta(t, pair.Session)
	meta.HostSlug = ingest.HostSlug(pair.Host)
	if pair.Parent != "" {
		parent, err := ingest.NewSessionID(pair.Parent)
		if err != nil {
			t.Fatalf("pair %q parent %q: %v", pair.Session, pair.Parent, err)
		}
		meta.ParentUUID = &parent
	}
	layoutHost := pair.Host
	if pair.Parent != "" {
		layoutHost = pair.Host + "/" + pair.Parent + "/subagents"
	}
	setupPeasantSyncSession(t, mfs, testOutputDir, layoutHost, pair.Session, meta)
}

func seedIndexScopedRow(t *testing.T, ctx context.Context, mfs *testutil.MemFS, db *store.Store, row indexStoredSeed) {
	t.Helper()
	meta := makeMinimalMeta(t, row.Session)
	meta.HostSlug = ingest.HostSlug(row.Host)
	if row.Harness != "" {
		meta.ModelHarness = ingest.Harness(row.Harness)
	}
	if row.Parent != "" {
		parent, err := ingest.NewSessionID(row.Parent)
		if err != nil {
			t.Fatalf("stored %q parent %q: %v", row.Session, row.Parent, err)
		}
		meta.ParentUUID = &parent
	}
	nativeSource := "/native/" + row.Session + ".jsonl"
	meta.Source.FilePath = nativeSource
	if row.NativeSource {
		if err := mfs.WriteFile(nativeSource, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("write native source for %q: %v", row.Session, err)
		}
	}
	if err := db.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
		t.Fatalf("seed stored row for %q: %v", row.Session, err)
	}
}

func scopedIndexAssertIDs(t *testing.T, label string, got []ingest.SessionID, want []string) {
	t.Helper()
	gotIDs := make([]string, 0, len(got))
	for _, id := range got {
		gotIDs = append(gotIDs, string(id))
	}
	sort.Strings(gotIDs)
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)
	if strings.Join(gotIDs, ",") != strings.Join(sortedWant, ",") {
		t.Errorf("%s = %v, want %v", label, gotIDs, sortedWant)
	}
}

func scopedIndexIndexOutcomeName(outcome ingest.LocateOutcome) string {
	switch outcome {
	case ingest.LocateStoredPath:
		return "stored-path"
	case ingest.LocateDirectLookup:
		return "direct-lookup"
	case ingest.LocateStoredUnreadable:
		return "stored-unreadable"
	default:
		return "unset"
	}
}

func scopedIndexIndexRoleName(role ingest.CohortRole) string {
	switch role {
	case ingest.CohortSelected:
		return "selected"
	case ingest.CohortChild:
		return "child"
	case ingest.CohortAncestor:
		return "ancestor"
	case ingest.CohortContext:
		return "context"
	default:
		return "unset"
	}
}

func scopedIndexIndexReasonName(reason string) string {
	switch reason {
	case "source-gone":
		return "source gone"
	case "harness-disabled":
		return "harness disabled"
	case "source-failed":
		return "source failed"
	default:
		return reason
	}
}

func scopedIndexIndexMissReasonName(reason ingest.MissReason) string {
	switch reason {
	case ingest.MissNotFound:
		return "not-found"
	case ingest.MissHarnessFiltered:
		return "harness-filtered"
	case ingest.MissSinceFiltered:
		return "since-filtered"
	default:
		return fmt.Sprintf("unset(%d)", reason)
	}
}
