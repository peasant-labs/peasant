package ingest_test

// This file is the pipeline parity harness: one runner proves that a
// session-scoped run leaves the store in the same normalized state as a full
// run over the same sources, reads exactly the source paths a scoped run is
// allowed to read, and never falls back to a whole-table evidence load.
//
// The two fixtures are the case data and the deletion-protection manifest. The
// manifest asserts its inventory against the adapter registry crossed with the
// closed case-kind list, so a new harness or a new kind appears here instead of
// being silently skipped.

import (
	"context"
	_ "embed"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testkit/scoperecorder"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/session_scope.yaml
var sessionScopeYAML []byte

//go:embed testdata/session_scope.manifest.yaml
var sessionScopeManifestYAML []byte

//go:embed testdata/session_scope_upkeep.yaml
var sessionScopeUpkeepYAML []byte

// scopeCaseKinds is the closed set of pipeline case kinds the manifest must
// cover. A kind added here without a fixture case fails the manifest guard.
var scopeCaseKinds = []string{
	"control",
	"stored_unchanged",
	"stored_changed",
	"new_direct_lookup",
	"related_exclude",
	"project_changed",
	"stored_source_gone",
	"derived_rebuild",
	"stored_child_source_gone",
}

type scopeFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Root          string   `yaml:"root"`
	// VolatileColumns names, per table, the columns whose value is a property
	// of when the run happened rather than what it stored. They are dropped
	// before the multiset comparison.
	VolatileColumns map[string][]string `yaml:"volatileColumns"`
	// ExemptTables names each table excluded from the multiset comparison, with
	// its reason. Exemption is always by name; there is no count.
	ExemptTables map[string]string `yaml:"exemptTables"`
	// ExemptColumns names the AUTOINCREMENT surrogate ids the row-identity
	// discussion exempts, each with its reason.
	ExemptColumns map[string]string `yaml:"exemptColumns"`
	Cases         []scopeCase       `yaml:"cases"`
}

type scopeCase struct {
	Name     string        `yaml:"name"`
	Harness  string        `yaml:"harness"`
	Kind     string        `yaml:"kind"`
	Files    []scopeFile   `yaml:"files"`
	Mutation scopeMutation `yaml:"mutation"`

	// SeedExcludes names fixture files that are absent while the seed runs, so
	// a case can start from a store that does not record the selected session.
	SeedExcludes []string `yaml:"seedExcludes"`
	// NonCohortSessions seeds stored rows that have no source file, which is
	// how the size-independence case grows the store without growing the
	// source tree.
	NonCohortSessions int `yaml:"nonCohortSessions"`

	// Execution is "scoped" (A full, B scoped; the default) or "full_both"
	// (both stores run a full harvest). The derived tables need full_both: the
	// scoped path deliberately does not walk Codex, so a scoped store cannot
	// derive the catalogue and the two stores would differ for a reason that is
	// not a defect.
	Execution string `yaml:"execution"`
	// ExpectedDerived is what the derived tables must hold after a full_both
	// run. It is computed from the fixture sources and asserted per store, so a
	// no-op catalog writer or a broken spawns projection leaves them empty and
	// fails.
	ExpectedDerived *scopeExpectedDerived `yaml:"expectedDerived"`

	Selected []string `yaml:"selected"`
	Related  string   `yaml:"related"`
	SinceMs  int64    `yaml:"sinceMs"`

	// ExpectedOps is the exact source-root operation set per configured
	// harness, keyed by harness name. Every configured root must have an entry
	// on a scoped case: the assertion derives the root list from the same
	// declaration the pipeline uses, so a new source with no entry fails
	// instead of passing silently. A full_both case runs no scoped discovery
	// and does not assert source ops.
	ExpectedOps  map[string]scopeRootOps `yaml:"expectedOps"`
	ContextPaths []string                `yaml:"contextPaths"`
	// EvidencePaths is the exact union the evidence recorder may request. When
	// empty it defaults to the read set plus context paths.
	EvidencePaths []string `yaml:"evidencePaths"`

	WantProcessed []string `yaml:"wantProcessed"`
	WantUpkeep    []string `yaml:"wantUpkeep"`
	WantOutcome   string   `yaml:"wantOutcome"`
	WantReason    string   `yaml:"wantReason"`
	WantRunError  string   `yaml:"wantRunError"`

	// ExpectParentTargets, when non-empty, is the exact parent list the run's
	// uncached-child heal must receive: the processed members of the
	// independent-admission harnesses. It is the uncached-child-heal witness.
	ExpectParentTargets   []string `yaml:"expectParentTargets"`
	ExpectParentHarnesses []string `yaml:"expectParentHarnesses"`

	// WantIndexLogDelta requires the run to add at least one index_log entry for
	// a cohort id, so the cohort-filtered delta compares real work rather than
	// two empty sets.
	WantIndexLogDelta bool `yaml:"wantIndexLogDelta"`

	// AssertOldProjectGone asks the runner to prove the project identity the
	// selected session held before the mutation has no project row in either
	// store, not merely that the two agree.
	AssertOldProjectGone bool `yaml:"assertOldProjectGone"`

	// AssertTeammateLinks names session→parent pairs that must be recorded in
	// BOTH stores. The table diff proves the two runs agree; this proves the
	// agreed answer is the link the case is about, so a case that silently
	// forms no link in either store cannot pass.
	AssertTeammateLinks []scopeAssertTeammateLink `yaml:"assertTeammateLinks"`
}

// scopeAssertTeammateLink is one recorded teammate parent the case requires.
type scopeAssertTeammateLink struct {
	Session string `yaml:"session"`
	Parent  string `yaml:"parent"`
}

type scopeFile struct {
	Path string `yaml:"path"`
	// Root overrides the fixture root for this file (a case that also needs a
	// Codex rollout tree names /codex here).
	Root string `yaml:"root"`
	Body string `yaml:"body"`
	// Lines is an alternative body spelling for JSONL fixtures.
	Lines []string `yaml:"lines"`
}

// scopeExpectedDerived is the exact derived state a derived case must produce.
// Paths are relative to the Claude root (spawns) or the Codex root (catalogue
// entries).
type scopeExpectedDerived struct {
	Spawns  []scopeExpectedSpawn   `yaml:"spawns"`
	Catalog []scopeExpectedRollout `yaml:"catalog"`
}

type scopeExpectedSpawn struct {
	Spawner string `yaml:"spawner"`
	Team    string `yaml:"team"`
	Name    string `yaml:"name"`
}

type scopeExpectedRollout struct {
	Path      string `yaml:"path"`
	SessionID string `yaml:"session"` // "session" avoids the YAML "id" ambiguity
}

// scopeRootOps is one configured source root's exact operation sets, relative
// to that root.
type scopeRootOps struct {
	Reads    []string `yaml:"reads"`
	Stats    []string `yaml:"stats"`
	Readdirs []string `yaml:"readdirs"`
}

type scopeMutation struct {
	// Action is none, replace, append or delete.
	Action string   `yaml:"action"`
	Path   string   `yaml:"path"`
	Body   string   `yaml:"body"`
	Lines  []string `yaml:"lines"`
}

type scopeManifest struct {
	RequiredNames []string `yaml:"requiredNames"`
	// NotApplicable names groups of (harness, kind) pairs that have no pipeline
	// case, with the reason for each group. The runner expands each named group
	// to the full cross product, so a pair is covered by name, never by a
	// count.
	NotApplicable []scopeNotApplicable `yaml:"notApplicable"`
	// Passes is the closed pass inventory: every scope-bearing store call the
	// scoped pipeline makes must be named here, and every store_scope pass must
	// record a call on every scoped run.
	Passes []scopePass `yaml:"passes"`
}

// scopePass is one pass of the closed pass inventory. Guard selects how the
// pass is witnessed: a store-scope call, the parent-target list, or a by-path
// evidence read.
type scopePass struct {
	ID        string `yaml:"id"`
	Operation string `yaml:"operation"`
	Guard     string `yaml:"guard"`
}

var scopePassGuards = []string{"store_scope", "parent_targets", "by_path_read"}

type scopeNotApplicable struct {
	Harnesses []string `yaml:"harnesses"`
	Kinds     []string `yaml:"kinds"`
	Reason    string   `yaml:"reason"`
}

func loadScopeFixture(t *testing.T) scopeFixture {
	t.Helper()
	var fixture scopeFixture
	if err := testutil.DecodeNamedFixtureYAML(sessionScopeYAML, &fixture); err != nil {
		t.Fatalf("decode the session-scope parity fixture: %v", err)
	}
	names := make(map[string]bool, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" {
			t.Fatalf("session-scope fixture has a case with no name")
		}
		if names[testCase.Name] {
			t.Fatalf("session-scope fixture repeats case %q", testCase.Name)
		}
		names[testCase.Name] = true
		if testCase.Harness == "" || testCase.Kind == "" {
			t.Fatalf("case %q names no harness or kind", testCase.Name)
		}
		if len(testCase.Selected) == 0 && testCase.Kind != "control" && testCase.Execution != "full_both" {
			t.Fatalf("case %q selects no session", testCase.Name)
		}
		if testCase.Execution != "" && testCase.Execution != "scoped" && testCase.Execution != "full_both" {
			t.Fatalf("case %q names unknown execution %q", testCase.Name, testCase.Execution)
		}
		if testCase.Execution == "full_both" && testCase.ExpectedDerived == nil {
			t.Fatalf("case %q runs full_both but names no expectedDerived rows", testCase.Name)
		}
	}
	if err := testutil.RequireFixtureNames("session-scope parity fixture", "case", fixture.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func loadScopeManifest(t *testing.T) scopeManifest {
	t.Helper()
	var manifest scopeManifest
	if err := yaml.Unmarshal(sessionScopeManifestYAML, &manifest); err != nil {
		t.Fatalf("decode the session-scope manifest: %v", err)
	}
	return manifest
}

// TestSessionScopeManifestCoversTheRegistryCross is the static derivation: the
// adapter registry crossed with the closed case kinds, every pair either a real
// case or a named not_applicable group. A new harness, a new kind, or a case
// that was deleted without its manifest entry fails here.
func TestSessionScopeManifestCoversTheRegistryCross(t *testing.T) {
	t.Parallel()
	fixture := loadScopeFixture(t)
	manifest := loadScopeManifest(t)
	if err := testutil.RequireFixtureNames("session-scope manifest", "case", manifest.RequiredNames, caseNameSet(fixture.Cases)); err != nil {
		t.Fatal(err)
	}
	if err := validateScopePassInventory(manifest); err != nil {
		t.Fatal(err)
	}

	have := make(map[string]bool, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		have[testCase.Harness+"\x00"+testCase.Kind] = true
	}
	notApplicable := make(map[string]string)
	harnesses := make(map[string]bool)
	for _, group := range manifest.NotApplicable {
		if group.Reason == "" || len(group.Harnesses) == 0 || len(group.Kinds) == 0 {
			t.Fatalf("not_applicable group %v names no harnesses/kinds or gives no reason", group)
		}
		for _, harness := range group.Harnesses {
			for _, kind := range group.Kinds {
				key := harness + "\x00" + kind
				if _, duplicate := notApplicable[key]; duplicate {
					t.Fatalf("the manifest names the pair %s/%s twice", harness, kind)
				}
				notApplicable[key] = group.Reason
				harnesses[harness] = true
			}
		}
	}
	for harness := range ingest.HarvesterVersionRegistry {
		harnesses[string(harness)] = true
	}
	for harness := range harnesses {
		if _, known := ingest.HarvesterVersionRegistry[ingest.Harness(harness)]; !known {
			t.Fatalf("the manifest names unknown harness %q; the registry has %v", harness, registryHarnesses())
		}
		for _, kind := range scopeCaseKinds {
			key := harness + "\x00" + kind
			if have[key] {
				continue
			}
			if _, named := notApplicable[key]; named {
				continue
			}
			t.Fatalf("the registry cross %s/%s is neither a case nor a named not_applicable pair", harness, kind)
		}
	}
	for _, testCase := range fixture.Cases {
		if !containsString(scopeCaseKinds, testCase.Kind) {
			t.Fatalf("case %q names kind %q outside the closed list %v", testCase.Name, testCase.Kind, scopeCaseKinds)
		}
	}
}

func TestSessionScopeParity(t *testing.T) {
	t.Parallel()
	fixture := loadScopeFixture(t)
	manifest := loadScopeManifest(t)
	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			runScopeParityCase(t, fixture, manifest, testCase)
		})
	}
}

func caseNameSet(cases []scopeCase) map[string]bool {
	out := make(map[string]bool, len(cases))
	for _, testCase := range cases {
		out[testCase.Name] = true
	}
	return out
}

func registryHarnesses() []string {
	out := make([]string, 0, len(ingest.HarvesterVersionRegistry))
	for harness := range ingest.HarvesterVersionRegistry {
		out = append(out, string(harness))
	}
	sort.Strings(out)
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// scopeWorld is one run's filesystem and store. The recorder is on the same
// filesystem the pipeline sees, so the assertion covers the run, not a copy of
// its inputs.
type scopeWorld struct {
	mfs  *testutil.MemFS
	fs   *testutil.OpRecorderFS
	raw  *store.Store
	pipe *scopeStore
	path string
}

func (w *scopeWorld) close(t *testing.T) {
	t.Helper()
	if w.raw == nil {
		return
	}
	if err := w.raw.Close(); err != nil {
		t.Fatalf("close the store at %s: %v", w.path, err)
	}
	w.raw = nil
}

// scopeAdapters builds the production adapters the fixture can drive. The
// Codex window clock is pinned so a rollout's "own day folder" is a property of
// the fixture, not of the day the test runs.
func scopeAdapters() map[ingest.Harness]ingest.AdapterFactory {
	anchor := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	return map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: func(fs ingest.FileSystem, git ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
			return ingest.NewClaudeAdapter(fs, git, s)
		},
		ingest.HarnessCodex: func(fs ingest.FileSystem, git ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
			return ingest.NewCodexAdapter(fs, git, s, ingest.WithCodexWindowClock(func() time.Time { return anchor }))
		},
	}
}

// scopeStore records every scope-bearing store call and the Claude evidence
// reads. It is attached to the scoped store only: the full run on A is allowed
// to load the whole evidence table, so recording it would make the zero-load
// assertion false and invite someone to weaken it.
type scopeStore struct {
	*store.Store
	calls scoperecorder.Recorder

	mu        sync.Mutex
	wholeLoad int
	requested []string
	saves     int
	parents   []ingest.SessionID
	harnesses []ingest.Harness
}

func (s *scopeStore) recordStore(operation string, scope ingest.StoreScope) {
	ids, narrowed := scope.Narrowed()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	s.calls.RecordStore(operation, out, scope.IsAll() || !narrowed)
}

func (s *scopeStore) recordProjects(operation string, scope ingest.ProjectScope) {
	hashes, narrowed := scope.Narrowed()
	out := make([]string, 0, len(hashes))
	for _, hash := range hashes {
		out = append(out, string(hash))
	}
	s.calls.RecordProjects(operation, out, scope.IsAll() || !narrowed)
}

func (s *scopeStore) SweepFlaggedSessionsForHarvest(ctx context.Context, scope ingest.StoreScope) (int, []error, error) {
	s.recordStore("SweepFlaggedSessionsForHarvest", scope)
	return s.Store.SweepFlaggedSessionsForHarvest(ctx, scope)
}

func (s *scopeStore) ListStaleOriginSessions(ctx context.Context, currentVersion int, scope ingest.StoreScope) ([]ingest.StoredOriginRow, error) {
	s.recordStore("ListStaleOriginSessions", scope)
	return s.Store.ListStaleOriginSessions(ctx, currentVersion, scope)
}

func (s *scopeStore) InvalidateStaleParentOrigins(ctx context.Context, scope ingest.StoreScope) (int64, error) {
	s.recordStore("InvalidateStaleParentOrigins", scope)
	return s.Store.InvalidateStaleParentOrigins(ctx, scope)
}

func (s *scopeStore) ListStaleAdapterSessions(ctx context.Context, targets map[ingest.Harness]ingest.HarvesterVersions, nativeRefreshVersions []int, scope ingest.StoreScope) ([]ingest.SessionID, error) {
	s.recordStore("ListStaleAdapterSessions", scope)
	return s.Store.ListStaleAdapterSessions(ctx, targets, nativeRefreshVersions, scope)
}

func (s *scopeStore) ListStaleIndexSessions(ctx context.Context, targets map[ingest.Harness]ingest.HarvesterVersions, scope ingest.StoreScope) ([]ingest.SessionID, error) {
	s.recordStore("ListStaleIndexSessions", scope)
	return s.Store.ListStaleIndexSessions(ctx, targets, scope)
}

func (s *scopeStore) ListSessionsNeedingRepair(ctx context.Context, targets map[ingest.Harness]ingest.HarvesterVersions, scope ingest.StoreScope) ([]ingest.SessionID, error) {
	s.recordStore("ListSessionsNeedingRepair", scope)
	return s.Store.ListSessionsNeedingRepair(ctx, targets, scope)
}

func (s *scopeStore) ListContentCaptureIncompleteSessionsAfter(ctx context.Context, after ingest.SessionID, limit int, scope ingest.StoreScope) ([]ingest.ContentCaptureIncompleteSession, error) {
	s.recordStore("ListContentCaptureIncompleteSessionsAfter", scope)
	return s.Store.ListContentCaptureIncompleteSessionsAfter(ctx, after, limit, scope)
}

func (s *scopeStore) ListMetricSessions(ctx context.Context, after ingest.SessionID, limit int, scope ingest.StoreScope) ([]ingest.MetricSession, error) {
	s.recordStore("ListMetricSessions", scope)
	return s.Store.ListMetricSessions(ctx, after, limit, scope)
}

func (s *scopeStore) CleanupOrphanProjects(ctx context.Context, scope ingest.ProjectScope) error {
	s.recordProjects("CleanupOrphanProjects", scope)
	return s.Store.CleanupOrphanProjects(ctx, scope)
}

func (s *scopeStore) ListUncachedChildrenOfParents(ctx context.Context, parents []ingest.SessionID, harnesses []ingest.Harness) ([]ingest.ParentCacheReconcile, error) {
	s.mu.Lock()
	s.parents = append([]ingest.SessionID(nil), parents...)
	s.harnesses = append([]ingest.Harness(nil), harnesses...)
	s.mu.Unlock()
	return s.Store.ListUncachedChildrenOfParents(ctx, parents, harnesses)
}

func (s *scopeStore) LoadClaudeEvidence(ctx context.Context) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, error) {
	s.mu.Lock()
	s.wholeLoad++
	s.mu.Unlock()
	return s.Store.LoadClaudeEvidence(ctx)
}

func (s *scopeStore) LoadClaudeEvidenceCompletely(ctx context.Context) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, bool, error) {
	s.mu.Lock()
	s.wholeLoad++
	s.mu.Unlock()
	return s.Store.LoadClaudeEvidenceCompletely(ctx)
}

func (s *scopeStore) LoadClaudeEvidenceFor(ctx context.Context, paths []ingest.ResolvedPath) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, error) {
	s.mu.Lock()
	for _, path := range paths {
		s.requested = append(s.requested, path.String())
	}
	s.mu.Unlock()
	return s.Store.LoadClaudeEvidenceFor(ctx, paths)
}

func (s *scopeStore) LoadClaudeEvidenceForCompletely(ctx context.Context, paths []ingest.ResolvedPath) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, bool, error) {
	s.mu.Lock()
	for _, path := range paths {
		s.requested = append(s.requested, path.String())
	}
	s.mu.Unlock()
	return s.Store.LoadClaudeEvidenceForCompletely(ctx, paths)
}

func (s *scopeStore) SaveClaudeEvidence(ctx context.Context, upserts []ingest.ClaudeTranscriptEvidence, deletes []ingest.ResolvedPath) error {
	s.mu.Lock()
	s.saves++
	s.mu.Unlock()
	return s.Store.SaveClaudeEvidence(ctx, upserts, deletes)
}

func (s *scopeStore) evidenceSnapshot() (wholeLoad int, requested []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wholeLoad, append([]string(nil), s.requested...)
}

func (s *scopeStore) parentSnapshot() (parents []ingest.SessionID, harnesses []ingest.Harness) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ingest.SessionID(nil), s.parents...), append([]ingest.Harness(nil), s.harnesses...)
}

// openScopeWorld builds a world from the fixture files, then applies one
// mutation. Each phase gets its own world, so one run's output files can never
// be read by another run and a source mutation is the only difference between
// them. record wraps the store for the evidence/scope recorder.
func openScopeWorld(t *testing.T, root string, files []scopeFile, excludes []string, mutation scopeMutation, dbPath string, record bool) *scopeWorld {
	t.Helper()
	mfs := testutil.NewMemFS()
	excluded := make(map[string]bool, len(excludes))
	for _, path := range excludes {
		excluded[path] = true
	}
	for _, file := range files {
		if excluded[file.Path] {
			continue
		}
		fileRoot := root
		if file.Root != "" {
			fileRoot = file.Root
		}
		writeScopeFile(t, mfs, fileRoot, file)
	}
	applyScopeMutation(t, mfs, root, mutation)

	fs := testutil.NewOpRecorderFS(mfs)
	db, err := store.Open(dbPath, store.WithSkipMigrations(), store.WithPoolSize(1), store.WithWALAutocheckpointDisabled())
	if err != nil {
		t.Fatalf("open the store at %s: %v", dbPath, err)
	}
	world := &scopeWorld{mfs: mfs, fs: fs, raw: db, path: dbPath}
	if record {
		world.pipe = &scopeStore{Store: db}
	}
	return world
}

func newScopeSeedPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seed.db")
	storetest.CopyGoldenTo(t, path)
	return path
}

func writeScopeFile(t *testing.T, mfs *testutil.MemFS, root string, file scopeFile) {
	t.Helper()
	body := file.Body
	if len(file.Lines) > 0 {
		body = strings.Join(file.Lines, "\n") + "\n"
	}
	if err := mfs.WriteFile(filepath.Join(root, file.Path), []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture file %q: %v", file.Path, err)
	}
}

func applyScopeMutation(t *testing.T, mfs *testutil.MemFS, root string, mutation scopeMutation) {
	t.Helper()
	if mutation.Action == "" || mutation.Action == "none" {
		return
	}
	path := filepath.Join(root, mutation.Path)
	body := mutation.Body
	if len(mutation.Lines) > 0 {
		body = strings.Join(mutation.Lines, "\n") + "\n"
	}
	switch mutation.Action {
	case "delete":
		if err := mfs.Remove(path); err != nil {
			t.Fatalf("apply the delete mutation on %q: %v", path, err)
		}
	case "append":
		existing, err := mfs.ReadFile(path)
		if err != nil {
			t.Fatalf("read %q for the append mutation: %v", path, err)
		}
		if err := mfs.WriteFile(path, append(existing, []byte(body)...), 0o600); err != nil {
			t.Fatalf("apply the append mutation on %q: %v", path, err)
		}
	case "replace":
		if err := mfs.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("apply the replace mutation on %q: %v", path, err)
		}
	default:
		t.Fatalf("unknown mutation action %q", mutation.Action)
	}
}

// runScopePhase runs one harvest against a world. scope == nil is the full run.
// scopeSourceRoots is the one declaration of every configured source root,
// used by the runner and by the op assertion so they cannot drift.
func scopeSourceRoots() map[ingest.Harness]string {
	return map[ingest.Harness]string{
		ingest.HarnessClaudeCode: "/claude",
		ingest.HarnessCodex:      "/codex/sessions",
	}
}

func runScopePhase(t *testing.T, ctx context.Context, world *scopeWorld, scope *ingest.SessionScope) *ingest.PipelineResult {
	t.Helper()
	cfg := makePipelineConfig(testOutputDir, func(cfg *ingest.PipelineConfig) {
		sources := make(map[ingest.Harness]ingest.SourceConfig, len(scopeSourceRoots()))
		for harness, root := range scopeSourceRoots() {
			sources[harness] = ingest.SourceConfig{Paths: []ingest.ResolvedPath{ingest.ResolvedPath(root)}, Enabled: true}
		}
		cfg.Sources = sources
		cfg.Command = "harvest"
		cfg.Scope = scope
	})
	storeArg := ingest.SessionStore(world.raw)
	metricsArg := ingest.MetricsStore(world.raw)
	logArg := ingest.IngestLogger(world.raw)
	indexLogArg := ingest.IndexLogger(world.raw)
	if world.pipe != nil {
		storeArg = world.pipe
		metricsArg = world.pipe
		logArg = world.pipe
		indexLogArg = world.pipe
	}
	p, err := newTestPipeline(world.fs, testutil.NoGitResolver(), scopeAdapters(), cfg,
		ingest.WithIndexers(ingest.NewIndexerRegistry(world.fs, ingest.IndexerRegistryOptions{})),
		ingest.WithStore(storeArg),
		ingest.WithMetricsStore(metricsArg),
		ingest.WithIndexLogger(indexLogArg),
		ingest.WithAnalyzer(metrics.NewEngine(world.raw)),
		ingest.WithLogger(logArg),
		ingest.WithHarvesterVersions(maps.Clone(ingest.HarvesterVersionRegistry)),
	)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, err := p.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return result
}

func runScopeParityCase(t *testing.T, fixture scopeFixture, manifest scopeManifest, testCase scopeCase) {
	t.Helper()
	ctx := context.Background()
	root := fixture.Root

	// Seed steps 1-3: baseline tree, full harvest, watermark settlement, then
	// the idempotence control on a byte copy. The control is what makes the
	// before/after comparison sound: a full run on the settled store must
	// change nothing by itself.
	seedPath := newScopeSeedPath(t)
	seed := openScopeWorld(t, root, testCase.Files, testCase.SeedExcludes, scopeMutation{}, seedPath, false)
	if testCase.NonCohortSessions > 0 {
		seedScopeStore(t, ctx, seed.raw, testCase.NonCohortSessions)
	}
	runScopePhase(t, ctx, seed, nil)
	runScopePhase(t, ctx, seed, nil)
	seed.close(t)

	controlPath := filepath.Join(t.TempDir(), "control.db")
	storetest.ForkSeeded(t, seedPath, controlPath)
	control := openScopeWorld(t, root, testCase.Files, testCase.SeedExcludes, scopeMutation{}, controlPath, false)
	controlBefore := exportScopeTables(t, ctx, control.raw)
	runScopePhase(t, ctx, control, nil)
	controlAfter := exportScopeTables(t, ctx, control.raw)
	if differing := compareScopeTables(t, fixture, controlBefore, controlAfter); len(differing) != 0 {
		t.Fatalf("control (mutation: none): tables %v differ, want an empty normalized diff", differing)
	}
	control.close(t)
	if testCase.Kind == "control" {
		return
	}

	// Steps 5-6: the case mutation and the fork to A and B. The seed exclusion
	// applies to the seed only: A and B must see the full tree the case names.
	aPath := filepath.Join(t.TempDir(), "a.db")
	bPath := filepath.Join(t.TempDir(), "b.db")
	storetest.ForkSeeded(t, seedPath, aPath, bPath)
	worldA := openScopeWorld(t, root, testCase.Files, nil, testCase.Mutation, aPath, false)
	worldB := openScopeWorld(t, root, testCase.Files, nil, testCase.Mutation, bPath, true)
	defer worldA.close(t)
	defer worldB.close(t)

	aBefore := exportScopeTables(t, ctx, worldA.raw)
	bBefore := exportScopeTables(t, ctx, worldB.raw)
	bIngestBefore := tableRowCount(bBefore, "ingest_log")

	if testCase.Execution == "full_both" {
		// The derived tables are the subject: both stores run the same full
		// harvest from a seed that has no derived rows, so a no-op catalog
		// writer or a broken spawns projection leaves both empty.
		runScopePhase(t, ctx, worldA, nil)
		runScopePhase(t, ctx, worldB, nil)
		aAfter := exportScopeTables(t, ctx, worldA.raw)
		bAfter := exportScopeTables(t, ctx, worldB.raw)
		if differing := compareScopeTables(t, fixture, aAfter, bAfter); len(differing) != 0 {
			t.Fatalf("full-both stores differ: %v", differing)
		}
		assertScopeLogs(t, fixture, aBefore, aAfter, bBefore, bAfter, bIngestBefore, testCase)
		assertDerivedRebuild(t, ctx, testCase, worldA.raw, worldB.raw)
		return
	}

	scope := scopeForCase(t, testCase)
	runScopePhase(t, ctx, worldA, nil)
	result := runScopePhase(t, ctx, worldB, scope)

	aAfter := exportScopeTables(t, ctx, worldA.raw)
	bAfter := exportScopeTables(t, ctx, worldB.raw)

	// The parity claim: the full run on A and the scoped run on B leave the
	// same normalized multiset per non-exempt table. The logs are asserted
	// directly below, not waved through.
	if differing := compareScopeTables(t, fixture, aAfter, bAfter); len(differing) != 0 {
		t.Fatalf("scoped run B vs full run A: tables %v differ by multiset", differing)
	}
	assertScopeLogs(t, fixture, aBefore, aAfter, bBefore, bAfter, bIngestBefore, testCase)
	assertScopeOps(t, fixture, testCase, worldB)
	assertScopeEvidence(t, testCase, worldB.pipe)
	assertScopeReport(t, testCase, result)
	assertScopeCalls(t, testCase, manifest, worldB.pipe)
	if testCase.ExpectedDerived != nil {
		assertDerivedRebuild(t, ctx, testCase, worldA.raw, worldB.raw)
	}
	if testCase.AssertOldProjectGone {
		assertOldProjectGone(t, ctx, worldA.raw, worldB.raw, bBefore, bAfter)
	}
	if len(testCase.AssertTeammateLinks) > 0 {
		assertTeammateLinks(t, testCase, aAfter, bAfter)
	}
}

// assertTeammateLinks proves each named session recorded the named parent in
// both stores. The multiset comparison above cannot: two stores that both
// failed to link would agree.
//
// ExportTableRows renders every cell as a storage-class-tagged canonical token
// (text:"...", int:..., null), so the comparison is made on the same token the
// table diff uses.
func assertTeammateLinks(t *testing.T, testCase scopeCase, stores ...map[string]store.ExportedTable) {
	t.Helper()
	textToken := func(value string) string { return "text:" + strconv.Quote(value) }
	for _, tables := range stores {
		sessions, ok := tables["sessions"]
		if !ok {
			t.Fatalf("case %q: the export has no sessions table", testCase.Name)
		}
		sessionColumn := columnIndex(sessions.Columns, "session_id")
		parentColumn := columnIndex(sessions.Columns, "parent_id")
		if sessionColumn < 0 || parentColumn < 0 {
			t.Fatalf("case %q: the sessions export lacks session_id or parent_id: %v", testCase.Name, sessions.Columns)
		}
		parents := make(map[string]string, len(sessions.Rows))
		for _, row := range sessions.Rows {
			parents[row[sessionColumn]] = row[parentColumn]
		}
		for _, link := range testCase.AssertTeammateLinks {
			if got := parents[textToken(link.Session)]; got != textToken(link.Parent) {
				t.Errorf("case %q: session %s parent = %q, want %q", testCase.Name, link.Session, got, textToken(link.Parent))
			}
		}
	}
}

// columnIndex returns the position of name, or -1.
func columnIndex(columns []string, name string) int {
	for index, column := range columns {
		if column == name {
			return index
		}
	}
	return -1
}

func scopeForCase(t *testing.T, testCase scopeCase) *ingest.SessionScope {
	t.Helper()
	if len(testCase.Selected) == 0 {
		return nil
	}
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
	return &scope
}

// exportScopeTables returns the exported tables keyed by name.
func exportScopeTables(t *testing.T, ctx context.Context, db *store.Store) map[string]store.ExportedTable {
	t.Helper()
	tables, err := store.ExportTableRows(ctx, db)
	if err != nil {
		t.Fatalf("export the store's tables: %v", err)
	}
	out := make(map[string]store.ExportedTable, len(tables))
	for _, table := range tables {
		out[table.Name] = table
	}
	return out
}

func tableRowCount(tables map[string]store.ExportedTable, name string) int {
	return len(tables[name].Rows)
}

// compareScopeTables compares two stores as a multiset of normalized rows per
// table and returns the names that differ. Exempt tables are skipped by name;
// volatile columns are dropped; every other table is compared with no primary
// key and no row count. A structural problem (a missing exempt table, a
// volatile column a table does not declare) is fatal: it means the fixture has
// drifted from the schema, not that a run differs.
func compareScopeTables(t *testing.T, fixture scopeFixture, before, after map[string]store.ExportedTable) []string {
	t.Helper()
	for name := range fixture.ExemptTables {
		if _, ok := after[name]; !ok {
			t.Fatalf("exempt table %q is missing from the export; the exemption names a table the schema no longer declares", name)
		}
	}
	for table, columns := range fixture.VolatileColumns {
		if _, ok := after[table]; !ok {
			t.Fatalf("volatile_columns names table %q, which the export does not contain", table)
		}
		for _, column := range columns {
			if !containsString(after[table].Columns, column) {
				t.Fatalf("volatile_columns names %s.%s, which the table does not declare", table, column)
			}
		}
	}
	for qualified := range fixture.ExemptColumns {
		table, column, ok := scopeSplitColumn(qualified)
		if !ok {
			t.Fatalf("exempt_columns names %q; the key must be table.column", qualified)
		}
		exported, exists := after[table]
		if !exists {
			t.Fatalf("exempt_columns names %s.%s, but the export has no table %q", table, column, table)
		}
		if !containsString(exported.Columns, column) {
			t.Fatalf("exempt_columns names %s.%s, which the table does not declare", table, column)
		}
	}
	names := make([]string, 0, len(before))
	for name := range before {
		names = append(names, name)
	}
	sort.Strings(names)
	var differing []string
	for _, name := range names {
		if _, exempt := fixture.ExemptTables[name]; exempt {
			continue
		}
		want, ok := after[name]
		if !ok {
			t.Fatalf("table %q exists before the run and not after", name)
		}
		gotRows, err := before[name].NormalizedRows(fixture.VolatileColumns[name])
		if err != nil {
			t.Fatalf("normalize %q before: %v", name, err)
		}
		wantRows, err := want.NormalizedRows(fixture.VolatileColumns[name])
		if err != nil {
			t.Fatalf("normalize %q after: %v", name, err)
		}
		if strings.Join(gotRows, "\x1e") != strings.Join(wantRows, "\x1e") {
			t.Logf("table %q differs by multiset\n full (%d rows): %v\n scoped (%d rows): %v", name, len(gotRows), firstScopeRows(gotRows, 6), len(wantRows), firstScopeRows(wantRows, 6))
			differing = append(differing, name)
		}
	}
	return differing
}

func firstScopeRows(rows []string, limit int) []string {
	if len(rows) <= limit {
		return rows
	}
	return rows[:limit]
}

// assertScopeLogs asserts the run logs directly: each store gains exactly one
// ingest_log row, and index_log entries naming the cohort are equal. The tables
// are exempt from the multiset diff only because their ids and timestamps are a
// property of the run.
func assertScopeLogs(t *testing.T, fixture scopeFixture, aBefore, aAfter, bBefore, bAfter map[string]store.ExportedTable, bIngestBefore int, testCase scopeCase) {
	t.Helper()
	if got := tableRowCount(bAfter, "ingest_log") - bIngestBefore; got != 1 {
		t.Fatalf("case %q: the run on B added %d ingest_log rows, want exactly 1", testCase.Name, got)
	}
	if got := tableRowCount(aAfter, "ingest_log") - tableRowCount(aBefore, "ingest_log"); got != 1 {
		t.Fatalf("case %q: the run on A added %d ingest_log rows, want exactly 1", testCase.Name, got)
	}
	cohort := make(map[string]bool)
	for _, id := range testCase.WantUpkeep {
		cohort[id] = true
	}
	for _, id := range testCase.WantProcessed {
		cohort[id] = true
	}
	indexRows := func(tables map[string]store.ExportedTable) []string {
		rows, err := tables["index_log"].NormalizedRows(fixture.VolatileColumns["index_log"])
		if err != nil {
			t.Fatalf("normalize index_log: %v", err)
		}
		return rows
	}
	if len(cohort) == 0 {
		return
	}
	// index_log is exempt from the whole-table diff; this is the index-side
	// guard: the entries this run added for its cohort ids must agree. Delta
	// from before the run, so a seed row is not counted as this run's work.
	aAdded := cohortLogDelta(indexRows(aBefore), indexRows(aAfter), cohort)
	bAdded := cohortLogDelta(indexRows(bBefore), indexRows(bAfter), cohort)
	if testCase.WantIndexLogDelta && (len(aAdded) == 0 || len(bAdded) == 0) {
		t.Fatalf("case %q: index_log cohort delta is empty (full=%d scoped=%d); the index-side guard would compare nothing", testCase.Name, len(aAdded), len(bAdded))
	}
	if strings.Join(aAdded, "\x1e") != strings.Join(bAdded, "\x1e") {
		t.Fatalf("case %q: index_log entries added for the cohort differ: full=%v scoped=%v", testCase.Name, aAdded, bAdded)
	}
}

// assertScopeOps asserts the exact source-root operation sets for EVERY
// configured harness root, the fixture's context_paths in the Claude read and
// stat sets, and the absence of a directory read or walk on any ancestor of a
// configured root. The root list is derived from the same declaration the
// pipeline uses, and a configured harness with no fixture entry fails, so a new
// source cannot escape the assertion.
func assertScopeOps(t *testing.T, fixture scopeFixture, testCase scopeCase, world *scopeWorld) {
	t.Helper()
	roots := scopeSourceRoots()
	readSets := make(map[string]map[string]bool, len(roots))
	statSets := make(map[string]map[string]bool, len(roots))
	for harness, root := range roots {
		if root == "" {
			t.Fatalf("case %q: configured harness %s has no source root", testCase.Name, harness)
		}
		want, declared := testCase.ExpectedOps[string(harness)]
		if !declared {
			t.Fatalf("case %q: configured harness %s has no expectedOps entry; every configured root must state its exact operation set", testCase.Name, harness)
		}
		relPaths := func(paths []string) []string {
			out := make([]string, 0, len(paths))
			for _, sourcePath := range paths {
				if sourcePath == root || strings.HasPrefix(sourcePath, root+"/") {
					rel := strings.TrimPrefix(strings.TrimPrefix(sourcePath, root), "/")
					if rel == "" {
						rel = "."
					}
					out = append(out, rel)
				}
			}
			sort.Strings(out)
			return out
		}
		assertSet := func(operation testutil.FSOp, expected []string) {
			got := relPaths(world.fs.Paths(operation))
			sortedWant := append([]string(nil), expected...)
			sort.Strings(sortedWant)
			if strings.Join(got, "\x00") != strings.Join(sortedWant, "\x00") {
				t.Fatalf("case %q: %s set on the %s root %s = %v, want exactly %v", testCase.Name, operation, harness, root, got, sortedWant)
			}
		}
		assertSet(testutil.FSOpReadFile, want.Reads)
		assertSet(testutil.FSOpStat, want.Stats)
		assertSet(testutil.FSOpReadDir, want.Readdirs)
		if walks := relPaths(world.fs.Paths(testutil.FSOpWalkDir)); len(walks) != 0 {
			t.Fatalf("case %q: a scoped run walked the %s source root %s: %v", testCase.Name, harness, root, walks)
		}
		for _, ancestor := range scopeSourceAncestors(root) {
			for _, operation := range []testutil.FSOp{testutil.FSOpReadDir, testutil.FSOpWalkDir} {
				for _, touched := range world.fs.Paths(operation) {
					if touched == ancestor {
						t.Fatalf("case %q: %s on %s, an ancestor of the %s source root %s; an ancestor read or walk can descend into a root the scoped run must not enumerate", testCase.Name, operation, ancestor, harness, root)
					}
				}
			}
		}
		readSets[string(harness)] = make(map[string]bool)
		for _, sourcePath := range relPaths(world.fs.Paths(testutil.FSOpReadFile)) {
			readSets[string(harness)][sourcePath] = true
		}
		statSets[string(harness)] = make(map[string]bool)
		for _, sourcePath := range relPaths(world.fs.Paths(testutil.FSOpStat)) {
			statSets[string(harness)][sourcePath] = true
		}
	}
	claude := string(ingest.HarnessClaudeCode)
	for _, sourcePath := range testCase.ContextPaths {
		if !readSets[claude][sourcePath] {
			t.Fatalf("case %q: context path %q is absent from the Claude read set %v", testCase.Name, sourcePath, scopeKeysOf(readSets[claude]))
		}
		if !statSets[claude][sourcePath] {
			t.Fatalf("case %q: context path %q is absent from the Claude stat set %v", testCase.Name, sourcePath, scopeKeysOf(statSets[claude]))
		}
	}
}

// scopeSourceAncestors returns every ancestor directory of root, nearest first,
// ending at the filesystem root. They are the directories a walk or listing
// could use to reach a configured source without naming it.
func scopeSourceAncestors(root string) []string {
	var ancestors []string
	for dir := path.Dir(root); dir != "." && dir != root; dir = path.Dir(dir) {
		ancestors = append(ancestors, dir)
		if dir == "/" {
			break
		}
	}
	return ancestors
}

func scopeKeysOf(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// assertScopeEvidence proves the evidence cache was read by path only, and only
// for paths this run is allowed to ask about. The expected union golden is the
// case's read set plus its declared context paths: a requested path outside it
// is a widening the run did not need, and a whole-table load is refused
// outright.
func assertScopeEvidence(t *testing.T, testCase scopeCase, recorder *scopeStore) {
	t.Helper()
	if recorder == nil {
		t.Fatal("the scoped world has no recording store")
	}
	wholeLoad, requested := recorder.evidenceSnapshot()
	if wholeLoad != 0 {
		t.Fatalf("case %q: the scoped run called the whole-table LoadClaudeEvidence %d times, want 0", testCase.Name, wholeLoad)
	}
	allowed := make(map[string]bool)
	for _, sourcePath := range testCase.ExpectedOps[string(ingest.HarnessClaudeCode)].Reads {
		allowed[sourcePath] = true
	}
	for _, sourcePath := range testCase.ContextPaths {
		allowed[sourcePath] = true
	}
	for _, path := range testCase.EvidencePaths {
		allowed[path] = true
	}
	for _, path := range requested {
		rel := strings.TrimPrefix(path, "/claude/")
		rel = strings.TrimPrefix(rel, "/codex/")
		if !allowed[rel] {
			t.Fatalf("case %q: the evidence recorder requested %q, which is neither a cohort source nor a context path (allowed %v)", testCase.Name, path, scopeKeysOf(allowed))
		}
	}
}

// assertScopeReport pins the LOCATE outcome for the unreadable and new cases.
func assertScopeReport(t *testing.T, testCase scopeCase, result *ingest.PipelineResult) {
	t.Helper()
	if len(testCase.Selected) == 0 {
		return
	}
	if result.ScopeReport == nil {
		t.Fatalf("case %q: a scoped run returned no ScopeReport", testCase.Name)
	}
	scopeAssertIDSet(t, testCase.Name+" processed", idsToStrings(result.ScopeReport.Processed()), testCase.WantProcessed)
	scopeAssertIDSet(t, testCase.Name+" upkeep", idsToStrings(result.ScopeReport.Upkeep()), testCase.WantUpkeep)
	if testCase.WantOutcome == "" {
		return
	}
	for _, member := range result.ScopeReport.Members {
		if !containsString(testCase.Selected, string(member.ID)) {
			continue
		}
		if got := scopeOutcomeName(member.Outcome); got != testCase.WantOutcome {
			t.Fatalf("case %q: selected member %s outcome = %q, want %q", testCase.Name, member.ID, got, testCase.WantOutcome)
		}
		if testCase.WantReason != "" {
			if got := scopeUnreadableReasonName(member.Unreadable); got != testCase.WantReason {
				t.Fatalf("case %q: selected member %s reason = %q, want %q", testCase.Name, member.ID, got, testCase.WantReason)
			}
		}
	}
}

func idsToStrings(ids []ingest.SessionID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}

func scopeAssertIDSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	gotSorted := append([]string(nil), got...)
	sort.Strings(gotSorted)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if strings.Join(gotSorted, ",") != strings.Join(wantSorted, ",") {
		t.Fatalf("%s = %v, want exactly %v", label, gotSorted, wantSorted)
	}
}

// assertScopeCalls enforces the pass inventory: every store_scope pass named
// in the manifest must record at least one narrowed call carrying the run's
// upkeep set, every recorded scope-bearing call must be named in the manifest,
// and the parent-target pass must record exactly when its target set is
// non-empty.
func assertScopeCalls(t *testing.T, testCase scopeCase, manifest scopeManifest, recorder *scopeStore) {
	t.Helper()
	if len(manifest.Passes) == 0 {
		t.Fatal("the manifest names no passes; the pass inventory must be explicit")
	}
	byOperation := make(map[string][]scoperecorder.Call)
	for _, call := range recorder.calls.Calls() {
		byOperation[call.Operation] = append(byOperation[call.Operation], call)
	}
	named := make(map[string]bool, len(manifest.Passes))
	storeScopePasses := 0
	for _, pass := range manifest.Passes {
		if pass.ID == "" || pass.Operation == "" {
			t.Fatalf("the manifest pass %v names no id or operation", pass)
		}
		if !containsString(scopePassGuards, pass.Guard) {
			t.Fatalf("pass %q names unknown guard %q; the closed set is %v", pass.ID, pass.Guard, scopePassGuards)
		}
		if named[pass.Operation] && pass.Guard == "store_scope" {
			t.Fatalf("the manifest names the store-scope operation %q twice", pass.Operation)
		}
		named[pass.Operation] = true
		switch pass.Guard {
		case "store_scope":
			storeScopePasses++
			recorded := byOperation[pass.Operation]
			if len(recorded) == 0 {
				t.Fatalf("case %q: pass %q (%s) recorded no call; the manifest inventory is the fail-on-unrecorded rule", testCase.Name, pass.ID, pass.Operation)
			}
			for _, call := range recorded {
				if call.All {
					t.Fatalf("case %q: pass %q (%s) carried the all-store scope on a scoped run", testCase.Name, pass.ID, pass.Operation)
				}
				if call.Kind == scoperecorder.KindProjects {
					if len(call.ProjectHashes) == 0 {
						t.Fatalf("case %q: pass %q (%s) carried a narrowed project scope with no hashes", testCase.Name, pass.ID, pass.Operation)
					}
					continue
				}
				scopeAssertIDSet(t, testCase.Name+" "+pass.ID+" ids", call.IDs, testCase.WantUpkeep)
			}
		case "parent_targets":
			assertScopeParentTargets(t, testCase, pass, recorder)
		case "by_path_read":
			// The by-path evidence read is witnessed per case kind: for a
			// Claude cohort this run built, at least one path must be asked for.
			// The dedicated read-recorder cases pin the exact set.
			if testCase.Harness == string(ingest.HarnessClaudeCode) && len(testCase.ExpectedOps[string(ingest.HarnessClaudeCode)].Reads) > 0 {
				_, requested := recorder.evidenceSnapshot()
				if len(requested) == 0 {
					t.Fatalf("case %q: pass %q recorded no by-path evidence read, want at least one", testCase.Name, pass.ID)
				}
			}
		}
	}
	if storeScopePasses == 0 {
		t.Fatal("the manifest names no store-scope pass; the inventory would assert nothing")
	}
	for operation := range byOperation {
		if !named[operation] {
			t.Fatalf("case %q: scope-bearing operation %q recorded a call but is not named in the pass inventory; add it to the manifest so a new pass cannot go unwitnessed", testCase.Name, operation)
		}
	}
}

// assertScopeParentTargets pins the argument of the uncached-child heal: its
// targets are the processed members of the independent-admission harnesses,
// never the whole store and never the whole upkeep set. An empty target set
// records no call, which is the one documented exception to the unrecorded
// rule.
func assertScopeParentTargets(t *testing.T, testCase scopeCase, pass scopePass, recorder *scopeStore) {
	t.Helper()
	parents, harnesses := recorder.parentSnapshot()
	if len(testCase.ExpectParentTargets) == 0 {
		if len(parents) != 0 || len(harnesses) != 0 {
			t.Fatalf("case %q: pass %q recorded a target list %v/%v although the processed intersection is empty", testCase.Name, pass.ID, parents, harnesses)
		}
		return
	}
	gotIDs := make([]string, 0, len(parents))
	for _, parent := range parents {
		gotIDs = append(gotIDs, string(parent))
	}
	scopeAssertIDSet(t, testCase.Name+" "+pass.ID+" targets", gotIDs, testCase.ExpectParentTargets)
	gotHarnesses := make([]string, 0, len(harnesses))
	for _, harness := range harnesses {
		gotHarnesses = append(gotHarnesses, string(harness))
	}
	sort.Strings(gotHarnesses)
	wantHarnesses := append([]string(nil), testCase.ExpectParentHarnesses...)
	sort.Strings(wantHarnesses)
	if strings.Join(gotHarnesses, ",") != strings.Join(wantHarnesses, ",") {
		t.Fatalf("case %q: pass %q harnesses = %v, want %v", testCase.Name, pass.ID, gotHarnesses, wantHarnesses)
	}
}

// assertOldProjectGone proves a project identity the run emptied has no row in
// either store, not merely that the tables agree.
func assertOldProjectGone(t *testing.T, ctx context.Context, a, b *store.Store, before, after map[string]store.ExportedTable) {
	t.Helper()
	old := oldProjectHashes(before, after)
	if len(old) == 0 {
		t.Fatal("assertOldProjectGone is set, but the run did not change any project identity")
	}
	for _, storeName := range []string{"A", "B"} {
		db := a
		if storeName == "B" {
			db = b
		}
		exported := exportScopeTables(t, ctx, db)
		for _, hash := range old {
			token := "text:" + quoteScopeToken(hash)
			for _, row := range exported["projects"].Rows {
				if len(row) > 0 && row[0] == token {
					t.Fatalf("store %s still holds the emptied project row %s: %v", storeName, hash, row)
				}
			}
		}
	}
}

func quoteScopeToken(value string) string {
	return "\"" + strings.ReplaceAll(value, "\"", "\\\"") + "\""
}

// oldProjectHashes returns the project hashes a store held before the run and
// no longer holds after it.
func oldProjectHashes(before, after map[string]store.ExportedTable) []string {
	afterSet := make(map[string]bool)
	for _, row := range after["projects"].Rows {
		if len(row) > 0 {
			afterSet[row[0]] = true
		}
	}
	var old []string
	for _, row := range before["projects"].Rows {
		if len(row) == 0 || afterSet[row[0]] {
			continue
		}
		old = append(old, strings.Trim(strings.TrimPrefix(row[0], "text:"), "\""))
	}
	return old
}

func scopeOutcomeName(outcome ingest.LocateOutcome) string {
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

// seedScopeStore inserts stored sessions that have no source file, the
// background population the size-independence case needs. The ids are derived
// from the ordinal so a case is reproducible.
func seedScopeStore(t *testing.T, ctx context.Context, db *store.Store, count int) {
	t.Helper()
	entries := make([]ingest.StoreEntry, 0, count)
	for i := 0; i < count; i++ {
		entries = append(entries, makeScopeSeedEntry(t, fmt.Sprintf("%08d-0000-4000-8000-000000000000", i)))
	}
	if err := db.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("seed %d non-cohort sessions: %v", count, err)
	}
}

func makeScopeSeedEntry(t *testing.T, rawSessionID string) ingest.StoreEntry {
	t.Helper()
	sessionID, err := ingest.NewSessionID(rawSessionID)
	if err != nil {
		t.Fatalf("seed session id %q: %v", rawSessionID, err)
	}
	projectHash, err := ingest.NewProjectHash(strings.Repeat("c", 64))
	if err != nil {
		t.Fatalf("seed project hash: %v", err)
	}
	hostSlug, err := ingest.NewHostSlug("github.com--peasant-labs--background")
	if err != nil {
		t.Fatalf("seed host slug: %v", err)
	}
	sourcePath, err := ingest.NewResolvedPath("/background/" + rawSessionID + ".jsonl")
	if err != nil {
		t.Fatalf("seed source path: %v", err)
	}
	ingested := int64(2000)
	return ingest.StoreEntry{Metadata: &ingest.UnifiedMetadata{
		SchemaVersion: ingest.CurrentSchemaVersion,
		SessionID:     sessionID,
		ModelHarness:  ingest.HarnessClaudeCode,
		HostSlug:      hostSlug,
		Project:       ingest.ProjectInfo{Hash: projectHash, FilePath: "/background", Name: "background"},
		Source:        ingest.SourceInfo{FilePath: string(sourcePath), Format: ingest.SourceFormatJSONL},
		Timestamp:     ingest.TimestampInfo{Start: 1000, End: 2000, Ingested: &ingested},
	}}
}

func scopeUnreadableReasonName(reason ingest.UnreadableReason) string {
	switch reason {
	case ingest.UnreadableSourceGone:
		return "source-gone"
	case ingest.UnreadableHarnessDisabled:
		return "harness-disabled"
	case ingest.UnreadableSourceFailed:
		return "source-failed"
	default:
		return "unset"
	}
}

// assertDerivedRebuild proves the derived tables are derived, not stored
// state: both stores must hold exactly the rows this fixture implies, and a
// rebuild from the same enumeration must change neither store. It runs per
// store, so a drift the two runs share is caught even though the table
// comparison cannot see it.
func assertDerivedRebuild(t *testing.T, ctx context.Context, testCase scopeCase, a, b *store.Store) {
	t.Helper()
	if testCase.ExpectedDerived == nil {
		t.Fatalf("case %q runs full_both with no expectedDerived rows", testCase.Name)
	}
	wantSpawns := normalizedDerivedSpawns(testCase.ExpectedDerived.Spawns)
	wantCatalog := normalizedDerivedCatalog(testCase.ExpectedDerived.Catalog)
	enumeration := ingest.RolloutEnumeration{Status: ingest.RolloutEnumerationComplete}
	for _, row := range testCase.ExpectedDerived.Catalog {
		enumeration.Files = append(enumeration.Files, ingest.RolloutFile{
			Path:      ingest.ResolvedPath(filepath.Join(scopeSourceRoots()[ingest.HarnessCodex], row.Path)),
			SessionID: ingest.SessionID(row.SessionID),
		})
	}
	for name, db := range map[string]*store.Store{"A": a, "B": b} {
		before := exportScopeTables(t, ctx, db)
		assertDerivedRows(t, name, before, "claude_teammate_spawns", wantSpawns)
		assertDerivedRows(t, name, before, "codex_rollout_catalog", wantCatalog)
		if err := db.RebuildDerivedLookups(ctx, enumeration); err != nil {
			t.Fatalf("derived-rebuild store %s: RebuildDerivedLookups: %v", name, err)
		}
		after := exportScopeTables(t, ctx, db)
		for _, table := range []string{"claude_teammate_spawns", "codex_rollout_catalog"} {
			wantRows, err := before[table].NormalizedRows(nil)
			if err != nil {
				t.Fatalf("derived-rebuild store %s: normalize %s before rebuild: %v", name, table, err)
			}
			gotRows, err := after[table].NormalizedRows(nil)
			if err != nil {
				t.Fatalf("derived-rebuild store %s: normalize %s after rebuild: %v", name, table, err)
			}
			if strings.Join(wantRows, "\x1e") != strings.Join(gotRows, "\x1e") {
				t.Fatalf("derived-rebuild store %s: rebuild changed %s; the table is state, not a projection: before %v after %v", name, table, wantRows, gotRows)
			}
		}
	}
}

// assertDerivedRows compares a derived table against the exact rows the fixture
// implies. An empty expected set is a real assertion: a no-op writer leaves the
// table empty and a row that should exist is reported missing.
func assertDerivedRows(t *testing.T, storeName string, tables map[string]store.ExportedTable, table string, want []string) {
	t.Helper()
	got, err := tables[table].NormalizedRows(nil)
	if err != nil {
		t.Fatalf("derived-rebuild store %s: normalize %s: %v", storeName, table, err)
	}
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)
	if strings.Join(got, "\x1e") != strings.Join(sortedWant, "\x1e") {
		t.Fatalf("derived-rebuild store %s: %s = %v, want exactly %v", storeName, table, got, sortedWant)
	}
}

func normalizedDerivedSpawns(rows []scopeExpectedSpawn) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, derivedRowToken(
			filepath.Join(scopeSourceRoots()[ingest.HarnessClaudeCode], row.Spawner),
			row.Team,
			row.Name,
		))
	}
	sort.Strings(out)
	return out
}

func normalizedDerivedCatalog(rows []scopeExpectedRollout) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, derivedRowToken(
			filepath.Join(scopeSourceRoots()[ingest.HarnessCodex], row.Path),
			row.SessionID,
		))
	}
	sort.Strings(out)
	return out
}

// derivedRowToken renders one expected row the same way NormalizedRows renders
// a stored row: storage-class-tagged, quoted text cells joined by the normalizer
// separator.
func derivedRowToken(cells ...string) string {
	parts := make([]string, len(cells))
	for i, cell := range cells {
		parts[i] = "text:" + quoteScopeToken(cell)
	}
	return strings.Join(parts, "\x1f")
}

// scopeSplitColumn splits an exempt-column key at its last dot, so a table name
// containing a dot still resolves the column.
func scopeSplitColumn(qualified string) (table, column string, ok bool) {
	index := strings.LastIndexByte(qualified, '.')
	if index <= 0 || index == len(qualified)-1 {
		return "", "", false
	}
	return qualified[:index], qualified[index+1:], true
}

// cohortLogDelta returns the rows present after a run but not before it, for
// entries naming a cohort id. It is a multiset difference, so a repeated
// identical entry keeps its multiplicity.
func cohortLogDelta(before, after []string, cohort map[string]bool) []string {
	remaining := make(map[string]int, len(before))
	for _, row := range before {
		if cohortRowNamesCohort(row, cohort) {
			remaining[row]++
		}
	}
	var added []string
	for _, row := range after {
		if !cohortRowNamesCohort(row, cohort) {
			continue
		}
		if remaining[row] > 0 {
			remaining[row]--
			continue
		}
		added = append(added, row)
	}
	sort.Strings(added)
	return added
}

func cohortRowNamesCohort(row string, cohort map[string]bool) bool {
	// The row is normalized already, so column positions have shifted; the
	// session id is whichever text cell names a cohort id.
	for _, cell := range strings.Split(row, "\x1f") {
		id := strings.Trim(strings.TrimPrefix(cell, "text:"), "\"")
		if cohort[id] {
			return true
		}
	}
	return false
}

// assertScopeParentTargets pins the argument of the uncached-child heal: its
// targets are the processed members of the independent-admission harnesses,
// never the whole store and never the whole upkeep set.

// validateScopePassInventory makes the manifest's pass inventory explicit and
// complete: every entry states its id, operation and guard; the store-scope
// operations are unique; and the inventory contains at least one pass of each
// guard kind, so dropping a guard class cannot leave the expected pass
// assertion silently smaller.
func validateScopePassInventory(manifest scopeManifest) error {
	if len(manifest.Passes) == 0 {
		return fmt.Errorf("the session-scope manifest names no passes; list every scope-bearing pass with its id, operation and guard")
	}
	seenID := make(map[string]bool, len(manifest.Passes))
	seenStoreOp := make(map[string]bool, len(manifest.Passes))
	guards := make(map[string]int, len(scopePassGuards))
	for _, pass := range manifest.Passes {
		if pass.ID == "" || pass.Operation == "" {
			return fmt.Errorf("manifest pass %v names no id or operation; every pass must be explicit", pass)
		}
		if !containsString(scopePassGuards, pass.Guard) {
			return fmt.Errorf("manifest pass %q names unknown guard %q; the closed set is %v", pass.ID, pass.Guard, scopePassGuards)
		}
		if seenID[pass.ID] {
			return fmt.Errorf("manifest pass id %q appears twice", pass.ID)
		}
		seenID[pass.ID] = true
		if pass.Guard == "store_scope" {
			if seenStoreOp[pass.Operation] {
				return fmt.Errorf("manifest names the store-scope operation %q twice", pass.Operation)
			}
			seenStoreOp[pass.Operation] = true
		}
		guards[pass.Guard]++
	}
	for _, guard := range scopePassGuards {
		if guards[guard] == 0 {
			return fmt.Errorf("manifest has no %q pass; the inventory must cover every guard kind", guard)
		}
	}
	return nil
}
