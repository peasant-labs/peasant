package ingest_test

// Independent child admission through the real pipeline.
//
// Every case drives the real production pipeline (DISCOVER, FILTER with its
// admission decision, operational topology, root dispatch, staging, writer
// dispatch) against a real SQLite store, then reopens the database and
// verifies the admitted cohort, the FK availability cache, and the retained
// logical evidence. The adapter and indexer are narrow test doubles for the
// harness source and the transcript parser; the scheduling, staging, writer,
// and store paths under test are the production code.

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/orphan_pipeline.yaml
var orphanPipelineYAML []byte

//go:embed testdata/orphan_pipeline.manifest.yaml
var orphanPipelineManifestYAML []byte

type orphanSessionFixture struct {
	ID         string         `yaml:"id"`
	Harness    ingest.Harness `yaml:"harness"`
	Parent     *string        `yaml:"parent"`
	Transcript string         `yaml:"transcript"`
}

// orphanOriginSpec is a stored origin verdict and the rule version it was
// written at, as the origin resolve pass persists them.
type orphanOriginSpec struct {
	Verdict string `yaml:"verdict"`
	Version int    `yaml:"version"`
}

type orphanRunFixture struct {
	ExpectWarning *orphanWarningFixture `yaml:"expectWarning"`
	Discovered    []string              `yaml:"discovered"`
	Reparent      map[string]*string    `yaml:"reparent"`
	// ExpectParentCache is the FK availability cache each named session must
	// carry right after this run, read inside the run loop so a heal is
	// attributed to the harvest that performed it.
	ExpectParentCache map[string]*string `yaml:"expectParentCache"`
	// ExpectOrigin is the stored origin verdict and version each named session
	// must carry right after this run, read through the resolve pass's own
	// stale-origin listing.
	ExpectOrigin map[string]orphanOriginSpec `yaml:"expectOrigin"`
}

type orphanWarningFixture struct {
	Contains []string `yaml:"contains"`
	Excludes []string `yaml:"excludes"`
}

type orphanCaseFixture struct {
	Name              string             `yaml:"name"`
	Discovered        []string           `yaml:"discovered"`
	Runs              []orphanRunFixture `yaml:"runs"`
	Allowed           *[]string          `yaml:"allowed"`
	Excluded          []string           `yaml:"excluded"`
	Filtered          *[]string          `yaml:"filtered"`
	SelfParent        []string           `yaml:"selfParent"`
	FailParents       []string           `yaml:"failParents"`
	ExpectStored      []string           `yaml:"expectStored"`
	ExpectNotStored   []string           `yaml:"expectNotStored"`
	ExpectParentCache map[string]*string `yaml:"expectParentCache"`
	ExpectLogical     map[string]*string `yaml:"expectLogicalParent"`
	// ExpectRootOwned names admitted children whose managed pair must install at
	// the stable child-owned root location because no operational parent edge
	// was available.
	ExpectRootOwned []string `yaml:"expectRootOwned"`
	// ExpectNested names admitted children whose managed pair must install under
	// their available logical parent's subtree, because the operational edge
	// keeps the stable nested location.
	ExpectNested []string `yaml:"expectNested"`
	// UnchangedBytes names stored sessions whose managed pair (metadata and
	// transcript) must stay byte-identical across every run of the case. A
	// later parent appearing reconciles only the FK cache; it never
	// re-extracts, relocates or rewrites the child's managed bytes.
	UnchangedBytes []string `yaml:"unchangedBytes"`
	// OSControl marks a case that must also run against the real operating
	// system filesystem on a temporary directory, so the managed-path and
	// managed-byte survival claims are proven on the production filesystem and
	// not only in the in-memory double.
	OSControl bool `yaml:"osControl"`
	// FailReconcileOnce injects one transient parent-cache reconciliation
	// failure through a store decorator that otherwise delegates to the real
	// SQLite store. The case proves the next harvest retries the failed
	// reconciliation and heals the cache without another new session.
	FailReconcileOnce bool   `yaml:"failReconcileOnce"`
	InvalidateFault   string `yaml:"invalidateFault"`
	// UnrelatedRoots names sessions an earlier run stored but the final run no
	// longer discovers. The final run must not open their managed metadata
	// files: only the persistence-backed target-scoped reconciliation runs.
	UnrelatedRoots []string `yaml:"unrelatedRoots"`
	// SeedStored names sessions stored directly before the first run, in order,
	// with their logical parent retained and no publication capture, so a
	// parent listed ahead of its child lands the child's FK cache. They are
	// settled for adapter and index maintenance.
	SeedStored []string `yaml:"seedStored"`
	// SeedCapture names sessions stored directly before the first run, after
	// SeedStored, with a validated publication capture whose snapshot retains
	// the logical parent and no relationship evidence. A child whose parent is
	// not stored keeps a NULL FK cache while that durable snapshot names the
	// parent, which is the evidence the reverse cache heal reads. Its recorded
	// source is absent, so maintenance cannot rewrite it.
	SeedCapture []string `yaml:"seedCapture"`
	// SeedOrigin records an origin verdict and version on seeded sessions
	// before the first run, as an earlier resolve pass would have left them.
	SeedOrigin map[string]orphanOriginSpec `yaml:"seedOrigin"`
	// ExpectParentCacheAfterSeed is the FK availability cache each named
	// session must carry after seeding and before the first run.
	ExpectParentCacheAfterSeed map[string]*string `yaml:"expectParentCacheAfterSeed"`
}

type orphanCorpusFixture struct {
	Sessions []orphanSessionFixture `yaml:"sessions"`
	Cases    []orphanCaseFixture    `yaml:"cases"`
}

var errOrphanParentFailure = errors.New("synthetic parent extraction failure")

// errOrphanTransientReconcile is the one injected fault of a retry case: the
// reverse parent-cache reconciliation fails once, and the next harvest must
// heal it without another new session.
var errOrphanTransientReconcile = errors.New("synthetic transient parent-cache reconciliation failure")

// orphanTransientReconcileStore delegates every store method to the real
// SQLite store and fails only the first cache reconciliation or origin requeue
// when a case asks for it. The injected fault changes no stored row; the next
// harvest must retry the failed operation.
type orphanTransientReconcileStore struct {
	*store.Store
	failNext             bool
	injections           int
	invalidateFault      string
	invalidateInjections int
}

var _ ingest.OrphanParentReconciler = (*orphanTransientReconcileStore)(nil)

func (s *orphanTransientReconcileStore) InvalidateStaleParentOrigins(ctx context.Context) (int64, error) {
	if s.invalidateFault != "" {
		fault := s.invalidateFault
		s.invalidateFault = ""
		s.invalidateInjections++
		return 0, errors.New(fault)
	}
	return s.Store.InvalidateStaleParentOrigins(ctx)
}

func (s *orphanTransientReconcileStore) ReconcileParentCache(ctx context.Context, updates []ingest.ParentCacheReconcile) error {
	if s.failNext {
		s.failNext = false
		s.injections++
		return errOrphanTransientReconcile
	}
	return s.Store.ReconcileParentCache(ctx, updates)
}

// orphanMetadataReadCounter wraps a filesystem and records which managed
// metadata files were opened. The no-unrelated-read case uses it to prove the
// reconciliation never opens a stored root's metadata.
type orphanMetadataReadCounter struct {
	ingest.FileSystem
	reads map[string]int
}

func (c *orphanMetadataReadCounter) ReadFile(path string) ([]byte, error) {
	if strings.HasSuffix(path, defaults.MetadataSuffix) {
		c.reads[path]++
	}
	return c.FileSystem.ReadFile(path)
}

func (c *orphanMetadataReadCounter) reset() {
	c.reads = make(map[string]int)
}

func (c *orphanMetadataReadCounter) readsForSession(id string) int {
	total := 0
	for path, count := range c.reads {
		if strings.HasSuffix(path, id+defaults.MetadataSuffix) {
			total += count
		}
	}
	return total
}

func orphanSummaryNew(result *ingest.PipelineResult) int {
	if result == nil {
		return -1
	}
	return result.Summary.New
}

// seedOrphanUnrelatedRoots stores independent-harness roots a case's harvests
// do not discover. They are settled (current adapter and index revisions, no artifact
// identity, no publication capture) so ordinary maintenance selection does not
// read them either. That leaves the parent-cache reconciliation as the only
// way an incremental harvest could open their metadata, which is exactly what
// the case measures.
func seedOrphanUnrelatedRoots(t *testing.T, ctx context.Context, db *store.Store, byID map[string]orphanSessionFixture, ids []string) {
	t.Helper()
	for _, raw := range ids {
		fixture, ok := byID[raw]
		if !ok {
			t.Fatalf("unknown unrelated root %q", raw)
		}
		meta := makeMinimalMeta(t, raw)
		meta.ModelHarness = orphanHarness(t, fixture.Harness)
		// Adapter maintenance is independent of index freshness. Stamp both
		// producers so this warm-state fixture isolates parent-cache I/O.
		versions := ingest.HarvesterVersionRegistry[meta.ModelHarness]
		meta.AdapterVersion = &versions.AdapterVersion
		meta.ParentUUID = nil
		if err := db.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta, CWDProvenance: ingest.CWDNotRecovered}}); err != nil {
			t.Fatalf("seed unrelated root %q: %v", raw, err)
		}
		sid, err := ingest.NewSessionID(raw)
		if err != nil {
			t.Fatalf("unrelated root %q: %v", raw, err)
		}
		target := versions.IndexerVersion
		if err := db.UpdateIndexState(ctx, sid, target, time.Now().UnixMilli()); err != nil {
			t.Fatalf("settle unrelated root %q: %v", raw, err)
		}
	}
}

type orphanErrorAdapter struct {
	harness  ingest.Harness
	sessions []ingest.DiscoveredSession
	metadata map[ingest.SessionID]*ingest.UnifiedMetadata
	failIDs  map[ingest.SessionID]error
}

func (a *orphanErrorAdapter) Harness() ingest.Harness { return a.harness }

func (a *orphanErrorAdapter) Discover(_ context.Context, _ ingest.SourceConfig) ([]ingest.DiscoveredSession, error) {
	return a.sessions, nil
}

func (a *orphanErrorAdapter) ExtractMetadata(_ context.Context, s ingest.DiscoveredSession) (*ingest.UnifiedMetadata, error) {
	if err, ok := a.failIDs[s.SessionID]; ok {
		return nil, err
	}
	m, ok := a.metadata[s.SessionID]
	if !ok {
		return nil, fmt.Errorf("no metadata for session %s", s.SessionID)
	}
	return m, nil
}

func loadOrphanCorpus(t *testing.T) orphanCorpusFixture {
	t.Helper()
	var corpus orphanCorpusFixture
	if err := testutil.DecodeFixtureYAML(orphanPipelineYAML, &corpus); err != nil {
		t.Fatalf("decode orphan pipeline corpus: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(orphanPipelineManifestYAML, "orphan pipeline")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(corpus.Cases))
	for _, c := range corpus.Cases {
		if c.Name == "" {
			t.Fatal("orphan pipeline case with empty name")
		}
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "orphan pipeline"); err != nil {
		t.Fatal(err)
	}
	for i, want := range manifest.RequiredNames {
		if names[i] != want {
			t.Fatalf("orphan pipeline manifest order changed at %d: manifest=%q corpus=%q", i, want, names[i])
		}
	}
	return corpus
}

// TestOrphanCorpusDecoderRejectsMalformedInput proves the corpus loader is
// strict: an unknown case field or a trailing document is a hard failure
// instead of silently disappearing.
func TestOrphanCorpusDecoderRejectsMalformedInput(t *testing.T) {
	var corpus orphanCorpusFixture
	if err := testutil.DecodeFixtureYAML([]byte("sessions: []\ncases:\n  - name: x\n    misspelledField: 1\n"), &corpus); err == nil {
		t.Fatal("orphan pipeline decoder accepted an unknown case field")
	}
	if err := testutil.DecodeFixtureYAML([]byte("sessions: []\ncases: []\n---\ncases: []\n"), &corpus); err == nil {
		t.Fatal("orphan pipeline decoder accepted a trailing YAML document")
	}
}

func orphanHarness(t *testing.T, raw ingest.Harness) ingest.Harness {
	t.Helper()
	switch raw {
	case ingest.HarnessClaudeCode, ingest.HarnessCodex, ingest.HarnessOpenCode:
		return raw
	default:
		t.Fatalf("unsupported orphan harness %q", string(raw))
		return raw
	}
}

func TestOrphanPipelineIndependentAdmission(t *testing.T) {
	// Warning cases replace the process-wide logger. Run them serially before
	// the parallel cases resume, and keep this parent test nonparallel.
	corpus := loadOrphanCorpus(t)
	byID := orphanSessionsByID(t, corpus)
	for _, tc := range corpus.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			if tc.InvalidateFault == "" && !tc.FailReconcileOnce {
				t.Parallel()
			}
			runOrphanCase(t, byID, tc)
		})
	}
}

// TestOrphanPipelineOSFileSystem runs every case the corpus marks for the real
// operating system filesystem: the managed path and managed bytes must survive
// a missing/unselected parent and a later parent backfill on the production
// filesystem, across a SQLite reopen, exactly as the in-memory control.
func TestOrphanPipelineOSFileSystem(t *testing.T) {
	t.Parallel()
	corpus := loadOrphanCorpus(t)
	byID := orphanSessionsByID(t, corpus)
	ran := false
	for _, tc := range corpus.Cases {
		if !tc.OSControl {
			continue
		}
		ran = true
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			runOrphanCaseWith(t, byID, tc, orphanEnv{
				fs:        &ingest.OSFileSystem{},
				outputDir: filepath.Join(dir, "output"),
				sourceDir: filepath.Join(dir, "sources"),
			})
		})
	}
	if !ran {
		t.Fatal("orphan pipeline corpus declares no OS-filesystem control case")
	}
}

func orphanSessionsByID(t *testing.T, corpus orphanCorpusFixture) map[string]orphanSessionFixture {
	t.Helper()
	byID := make(map[string]orphanSessionFixture, len(corpus.Sessions))
	for _, s := range corpus.Sessions {
		if _, dup := byID[s.ID]; dup {
			t.Fatalf("duplicate orphan session %q", s.ID)
		}
		byID[s.ID] = s
	}
	return byID
}

// orphanEnv is the filesystem and directory set a case runs against. The
// in-memory control uses MemFS; the OS control uses the real filesystem.
type orphanEnv struct {
	fs        ingest.FileSystem
	outputDir string
	sourceDir string
}

func runOrphanCase(t *testing.T, byID map[string]orphanSessionFixture, tc orphanCaseFixture) {
	t.Helper()
	runOrphanCaseWith(t, byID, tc, orphanEnv{
		fs:        testutil.NewMemFS(),
		outputDir: testOutputDir,
		sourceDir: testSourceDir,
	})
}

func runOrphanCaseWith(t *testing.T, byID map[string]orphanSessionFixture, tc orphanCaseFixture, env orphanEnv) {
	t.Helper()
	ctx := t.Context()
	counter := &orphanMetadataReadCounter{FileSystem: env.fs, reads: make(map[string]int)}
	fs := ingest.FileSystem(counter)
	if err := fs.MkdirAll(env.sourceDir, 0o755); err != nil {
		t.Fatalf("create source directory %q: %v", env.sourceDir, err)
	}
	if err := fs.MkdirAll(env.outputDir, 0o755); err != nil {
		t.Fatalf("create output directory %q: %v", env.outputDir, err)
	}
	dbPath := storetest.CopyGoldenDB(t)
	db, err := store.Open(dbPath, store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reconcileStore := &orphanTransientReconcileStore{Store: db, failNext: tc.FailReconcileOnce, invalidateFault: tc.InvalidateFault}
	var warningLog bytes.Buffer
	if tc.InvalidateFault != "" || tc.FailReconcileOnce {
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&warningLog, &slog.HandlerOptions{Level: slog.LevelWarn})))
		t.Cleanup(func() { slog.SetDefault(previous) })
	}

	selfParents := make(map[string]bool, len(tc.SelfParent))
	for _, id := range tc.SelfParent {
		selfParents[id] = true
	}
	failIDs := make(map[string]bool, len(tc.FailParents))
	for _, id := range tc.FailParents {
		failIDs[id] = true
	}
	reparents := make(map[string]*string)
	runs := tc.Runs
	if len(runs) == 0 {
		runs = []orphanRunFixture{{Discovered: tc.Discovered}}
	}

	allowed := orphanAllowedSet(tc.Allowed)
	excluded := orphanStringSet(tc.Excluded)
	filtered := orphanFilteredSet(tc.Filtered)
	byteBaselines := make(map[string]map[string][]byte, len(tc.UnchangedBytes))

	if len(tc.UnrelatedRoots) > 0 {
		seedOrphanUnrelatedRoots(t, ctx, db, byID, tc.UnrelatedRoots)
	}
	seedOrphanStoredRows(t, ctx, db, env, byID, tc.SeedStored, false)
	seedOrphanStoredRows(t, ctx, db, env, byID, tc.SeedCapture, true)
	seedOrphanOrigins(t, ctx, db, tc.SeedOrigin)
	assertOrphanParentCache(t, ctx, db, tc.Name+" after seeding", tc.ExpectParentCacheAfterSeed)

	var finalResult *ingest.PipelineResult
	for runIndex, run := range runs {
		warningLog.Reset()
		if len(tc.UnrelatedRoots) > 0 && runIndex == len(runs)-1 {
			// The final run is the incremental harvest: only its metadata
			// reads are observed, so a stored unrelated root opened by the
			// reconciliation is caught.
			counter.reset()
		}
		for id, parent := range run.Reparent {
			cp := parent
			reparents[id] = cp
		}
		discovered := run.Discovered
		if runIndex == 0 && len(discovered) == 0 {
			discovered = tc.Discovered
		}
		adapters, indexers := orphanAdapters(t, env, byID, discovered, reparents, selfParents, failIDs)
		cfg := makePipelineConfig(env.outputDir)
		cfg.Sources = map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessClaudeCode: {Paths: []ingest.ResolvedPath{ingest.ResolvedPath(env.sourceDir)}, Enabled: true},
			ingest.HarnessCodex:      {Paths: []ingest.ResolvedPath{ingest.ResolvedPath(env.sourceDir)}, Enabled: true},
			ingest.HarnessOpenCode:   {Paths: []ingest.ResolvedPath{ingest.ResolvedPath(env.sourceDir)}, Enabled: true},
		}
		cfg.StalenessThreshold = 0
		if allowed != nil {
			cfg.AllowedSessionIDs = allowed
		}
		if len(excluded) > 0 {
			cfg.SessionExclusionFilter = func(s ingest.DiscoveredSession) bool {
				return excluded[string(s.SessionID)]
			}
		}
		if filtered != nil {
			cfg.SessionFilter = func(s ingest.DiscoveredSession) bool {
				return filtered[string(s.SessionID)]
			}
		}
		pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg,
			ingest.WithStore(reconcileStore), ingest.WithMetricsStore(db), ingest.WithIndexLogger(db),
			ingest.WithIndexers(indexers))
		if err != nil {
			t.Fatalf("run %d NewPipeline: %v", runIndex, err)
		}
		result, err := pipeline.Run(ctx)
		if err != nil {
			t.Fatalf("run %d Run: %v", runIndex, err)
		}
		finalResult = result
		if run.ExpectWarning != nil {
			logged := warningLog.String()
			for _, text := range run.ExpectWarning.Contains {
				if !strings.Contains(logged, text) {
					t.Fatalf("run %d warning lacks %q: %s", runIndex, text, logged)
				}
			}
			for _, text := range run.ExpectWarning.Excludes {
				if strings.Contains(logged, text) {
					t.Fatalf("run %d warning contains forbidden %q: %s", runIndex, text, logged)
				}
			}
		}
		label := fmt.Sprintf("%s after run %d", tc.Name, runIndex)
		assertOrphanParentCache(t, ctx, db, label, run.ExpectParentCache)
		assertOrphanOrigins(t, ctx, db, label, run.ExpectOrigin)
		for _, id := range tc.UnchangedBytes {
			pair := orphanManagedPairBytes(t, env, id)
			if runIndex == 0 {
				byteBaselines[id] = pair
				continue
			}
			baseline, ok := byteBaselines[id]
			if !ok {
				t.Fatalf("case %q: no managed-byte baseline for %q", tc.Name, id)
			}
			if !orphanBytesEqual(baseline, pair) {
				t.Fatalf("case %q: managed bytes of %q changed after run %d; a later parent must reconcile the cache without rewriting the child", tc.Name, id, runIndex)
			}
		}
	}

	if tc.InvalidateFault != "" && reconcileStore.invalidateInjections != 1 {
		t.Fatalf("origin invalidation fault must fire exactly once, got %d", reconcileStore.invalidateInjections)
	}
	if tc.FailReconcileOnce {
		if reconcileStore.injections != 1 {
			t.Fatalf("case %q: injected reconciliation failures = %d, want exactly 1: the transient failure must fire before the retry proves itself", tc.Name, reconcileStore.injections)
		}
		if finalResult == nil || finalResult.Summary.New != 0 {
			t.Fatalf("case %q: the retry harvest must not discover a new session, got new=%d", tc.Name, orphanSummaryNew(finalResult))
		}
	}
	if len(tc.UnrelatedRoots) > 0 {
		for _, id := range tc.UnrelatedRoots {
			if reads := counter.readsForSession(id); reads != 0 {
				t.Fatalf("case %q: the incremental harvest opened %d managed metadata file(s) of unrelated stored root %q; parent-cache reconciliation must read persisted evidence, never unrelated metadata", tc.Name, reads, id)
			}
		}
	}

	assertOrphanExpectations(t, ctx, db, env, byID, tc, reparents)

	if err := db.Close(); err != nil {
		t.Fatalf("close store before reopen: %v", err)
	}
	reopened, err := store.Open(dbPath, store.WithSkipMigrations())
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertOrphanExpectations(t, ctx, reopened, env, byID, tc, reparents)
	for _, id := range tc.UnchangedBytes {
		if !orphanBytesEqual(byteBaselines[id], orphanManagedPairBytes(t, env, id)) {
			t.Fatalf("case %q: managed bytes of %q changed across the SQLite reopen", tc.Name, id)
		}
	}
}

// orphanManagedPairBytes reads the managed metadata and transcript a stored
// session owns at its stable root location. A relocation would make this fail,
// which is the point: the case declares that the child must not move.
func orphanManagedPairBytes(t *testing.T, env orphanEnv, id string) map[string][]byte {
	t.Helper()
	base := filepath.Join(env.outputDir, testutil.TestHostSlug, id)
	out := make(map[string][]byte, 2)
	metadata, err := env.fs.ReadFile(filepath.Join(base, id+defaults.MetadataSuffix))
	if err != nil {
		t.Fatalf("read managed metadata for %q at %q: %v", id, base, err)
	}
	out["metadata"] = metadata
	transcript, err := env.fs.ReadFile(filepath.Join(base, id+"--transcript.jsonl"))
	if err != nil {
		t.Fatalf("read managed transcript for %q at %q: %v", id, base, err)
	}
	out["transcript"] = transcript
	return out
}

func orphanBytesEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for name, data := range a {
		if !bytes.Equal(data, b[name]) {
			return false
		}
	}
	return true
}

func orphanAllowedSet(raw *[]string) map[ingest.SessionID]bool {
	if raw == nil {
		return nil
	}
	out := make(map[ingest.SessionID]bool, len(*raw))
	for _, id := range *raw {
		sid, err := ingest.NewSessionID(id)
		if err != nil {
			panic(err)
		}
		out[sid] = true
	}
	return out
}

func orphanStringSet(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func orphanFilteredSet(raw *[]string) map[string]bool {
	if raw == nil {
		return nil
	}
	return orphanStringSet(*raw)
}

func orphanAdapters(
	t *testing.T,
	env orphanEnv,
	byID map[string]orphanSessionFixture,
	discovered []string,
	reparents map[string]*string,
	selfParents map[string]bool,
	failIDs map[string]bool,
) (map[ingest.Harness]ingest.AdapterFactory, map[ingest.Harness]ingest.TranscriptIndexer) {
	t.Helper()
	now := time.Now().Add(-2 * time.Hour)
	perHarnessSessions := make(map[ingest.Harness][]ingest.DiscoveredSession)
	perHarnessMeta := make(map[ingest.Harness]map[ingest.SessionID]*ingest.UnifiedMetadata)
	perHarnessFail := make(map[ingest.Harness]map[ingest.SessionID]error)
	perHarnessEntries := make(map[ingest.Harness]map[ingest.SessionID][]schema.SessionEntry)
	for _, id := range discovered {
		fixture, ok := byID[id]
		if !ok {
			t.Fatalf("unknown orphan session %q", id)
		}
		sid, err := ingest.NewSessionID(fixture.ID)
		if err != nil {
			t.Fatalf("NewSessionID(%q): %v", fixture.ID, err)
		}
		harness := orphanHarness(t, fixture.Harness)
		sourcePath := filepath.Join(env.sourceDir, fixture.ID+".jsonl")
		if err := env.fs.WriteFile(sourcePath, []byte(fixture.Transcript+"\n"), 0o644); err != nil {
			t.Fatalf("write source %q: %v", sourcePath, err)
		}
		session := ingest.DiscoveredSession{
			SessionID:    sid,
			Harness:      harness,
			SourcePath:   ingest.ResolvedPath(sourcePath),
			SourceFormat: ingest.SourceFormatJSONL,
			ModTime:      now,
		}
		parent := fixture.Parent
		if override, ok := reparents[fixture.ID]; ok {
			parent = override
		}
		if selfParents[fixture.ID] {
			parent = &fixture.ID
		}
		if parent != nil {
			psid, err := ingest.NewSessionID(*parent)
			if err != nil {
				t.Fatalf("NewSessionID(parent %q): %v", *parent, err)
			}
			session.ParentUUID = &psid
		}
		perHarnessSessions[harness] = append(perHarnessSessions[harness], session)
		meta := makeMinimalMeta(t, fixture.ID)
		meta.ModelHarness = harness
		meta.Source = ingest.SourceInfo{FilePath: sourcePath, Format: ingest.SourceFormatJSONL}
		meta.ParentUUID = session.ParentUUID
		if perHarnessMeta[harness] == nil {
			perHarnessMeta[harness] = make(map[ingest.SessionID]*ingest.UnifiedMetadata)
		}
		perHarnessMeta[harness][sid] = meta
		if failIDs[fixture.ID] {
			if perHarnessFail[harness] == nil {
				perHarnessFail[harness] = make(map[ingest.SessionID]error)
			}
			perHarnessFail[harness][sid] = errOrphanParentFailure
		}
		if perHarnessEntries[harness] == nil {
			perHarnessEntries[harness] = make(map[ingest.SessionID][]schema.SessionEntry)
		}
		preview := "synthetic orphan entry"
		perHarnessEntries[harness][sid] = []schema.SessionEntry{
			{SessionID: schema.SessionID(sid), EntryIndex: 0, Role: schema.RoleUser, EntryType: schema.EntryTypeText, ContentPreview: &preview},
		}
	}
	adapters := make(map[ingest.Harness]ingest.AdapterFactory, len(perHarnessSessions))
	for harness, sessions := range perHarnessSessions {
		sessions, metas, fails := sessions, perHarnessMeta[harness], perHarnessFail[harness]
		h := harness
		if len(fails) > 0 {
			adapters[h] = func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter {
				return &orphanErrorAdapter{harness: h, sessions: sessions, metadata: metas, failIDs: fails}
			}
		} else {
			adapters[h] = func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter {
				return &testutil.StubAdapter{ProviderValue: h, Sessions: sessions, Metadata: metas}
			}
		}
	}
	for _, raw := range []ingest.Harness{ingest.HarnessClaudeCode, ingest.HarnessCodex, ingest.HarnessOpenCode} {
		h := orphanHarness(t, raw)
		if _, ok := adapters[h]; !ok {
			h2 := h
			adapters[h2] = func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter {
				return &testutil.StubAdapter{ProviderValue: h2}
			}
		}
	}
	indexers := make(map[ingest.Harness]ingest.TranscriptIndexer, len(perHarnessEntries))
	for harness, entries := range perHarnessEntries {
		indexers[harness] = &testutil.StubIndexer{Kind: ingest.TranscriptSourceFile, Entries: entries}
	}
	for _, raw := range []ingest.Harness{ingest.HarnessClaudeCode, ingest.HarnessCodex, ingest.HarnessOpenCode} {
		h := orphanHarness(t, raw)
		if _, ok := indexers[h]; !ok {
			indexers[h] = &testutil.StubIndexer{Kind: ingest.TranscriptSourceFile}
		}
	}
	return adapters, indexers
}

func assertOrphanExpectations(
	t *testing.T,
	ctx context.Context,
	db *store.Store,
	env orphanEnv,
	byID map[string]orphanSessionFixture,
	tc orphanCaseFixture,
	reparents map[string]*string,
) {
	t.Helper()
	for _, id := range tc.ExpectStored {
		row, err := db.SessionByID(ctx, id)
		if err != nil {
			t.Fatalf("case %q: SessionByID(%q): %v", tc.Name, id, err)
		}
		if row == nil {
			t.Fatalf("case %q: expected stored session %q, found no row", tc.Name, id)
		}
		entries, err := db.ListEntries(ctx, ingest.SessionID(id))
		if err != nil {
			t.Fatalf("case %q: ListEntries(%q): %v", tc.Name, id, err)
		}
		if len(entries) == 0 {
			t.Fatalf("case %q: stored session %q has no detail entries", tc.Name, id)
		}
	}
	for _, id := range tc.ExpectNotStored {
		row, err := db.SessionByID(ctx, id)
		if err != nil {
			t.Fatalf("case %q: SessionByID(%q): %v", tc.Name, id, err)
		}
		if row != nil {
			t.Fatalf("case %q: expected session %q to stay unstored, found row %+v", tc.Name, id, row)
		}
	}
	assertOrphanParentCache(t, ctx, db, tc.Name, tc.ExpectParentCache)
	for _, id := range tc.ExpectRootOwned {
		rootPath := filepath.Join(env.outputDir, testutil.TestHostSlug, id, id+defaults.MetadataSuffix)
		if _, err := env.fs.ReadFile(rootPath); err != nil {
			t.Fatalf("case %q: expected %q to own its root location %q: %v", tc.Name, id, rootPath, err)
		}
	}
	for _, id := range tc.ExpectNested {
		fixture, ok := byID[id]
		if !ok {
			t.Fatalf("case %q: unknown session %q in nested expectation", tc.Name, id)
		}
		parent := fixture.Parent
		if override, ok := reparents[fixture.ID]; ok {
			parent = override
		}
		if parent == nil {
			t.Fatalf("case %q: nested expectation for %q has no logical parent", tc.Name, id)
		}
		nestedPath := filepath.Join(env.outputDir, testutil.TestHostSlug, *parent, "subagents", id, id+defaults.MetadataSuffix)
		if _, err := env.fs.ReadFile(nestedPath); err != nil {
			t.Fatalf("case %q: expected %q to install under its available parent at %q: %v", tc.Name, id, nestedPath, err)
		}
	}
	for id, wantLogical := range tc.ExpectLogical {
		fixture, ok := byID[id]
		if !ok {
			t.Fatalf("case %q: unknown session %q in logical expectation", tc.Name, id)
		}
		parent := fixture.Parent
		if override, ok := reparents[fixture.ID]; ok {
			parent = override
		}
		if !orphanOptionalEqual(parent, wantLogical) {
			t.Fatalf("case %q: logical parent fixture drift for %q: corpus=%v want=%v", tc.Name, id, orphanDisplay(parent), orphanDisplay(wantLogical))
		}
		gotLogical, err := db.LogicalParentForSession(ctx, ingest.SessionID(id))
		if err != nil {
			t.Fatalf("case %q: LogicalParentForSession(%q): %v", tc.Name, id, err)
		}
		// V1 logical evidence for an orphan with a nil cache survives in the
		// managed metadata file; the DB logical reader falls back to the cache.
		// Assert the file evidence directly so the relation is proven retained.
		fileParent := orphanMetadataParent(t, env, id)
		if !orphanOptionalEqual(fileParent, wantLogical) {
			t.Fatalf("case %q: managed metadata logical parent of %q = %v, want %v", tc.Name, id, orphanDisplay(fileParent), orphanDisplay(wantLogical))
		}
		if wantLogical == nil && gotLogical != nil {
			t.Fatalf("case %q: DB logical parent of %q = %v, want nil", tc.Name, id, orphanDisplay(gotLogical))
		}
	}
}

// assertOrphanParentCache requires each named session's stored FK
// availability cache to equal the expected parent, or to be NULL for a null
// expectation.
func assertOrphanParentCache(t *testing.T, ctx context.Context, db *store.Store, label string, want map[string]*string) {
	t.Helper()
	for id, wantCache := range want {
		row, err := db.SessionByID(ctx, id)
		if err != nil {
			t.Fatalf("case %q: SessionByID(%q): %v", label, id, err)
		}
		if row == nil {
			t.Fatalf("case %q: cannot check parent cache of missing session %q", label, id)
		}
		var gotCache *string
		if row.ParentID != nil && *row.ParentID != "" {
			gotCache = row.ParentID
		}
		if !orphanOptionalEqual(gotCache, wantCache) {
			t.Fatalf("case %q: parent cache of %q = %v, want %v", label, id, orphanDisplay(gotCache), orphanDisplay(wantCache))
		}
	}
}

// assertOrphanOrigins requires each named session's stored origin verdict and
// version to match, read through the same stale-origin listing the resolve
// pass uses. The listing bound sits above the current rule version so every
// stored row is returned.
func assertOrphanOrigins(t *testing.T, ctx context.Context, db *store.Store, label string, want map[string]orphanOriginSpec) {
	t.Helper()
	if len(want) == 0 {
		return
	}
	rows, err := db.ListStaleOriginSessions(ctx, ingest.OriginRuleVersion+1)
	if err != nil {
		t.Fatalf("case %q: ListStaleOriginSessions: %v", label, err)
	}
	got := make(map[string]ingest.StoredOriginRow, len(rows))
	for _, row := range rows {
		got[string(row.SessionID)] = row
	}
	for id, spec := range want {
		row, ok := got[id]
		if !ok {
			t.Fatalf("case %q: origin state of %q missing from the stale-origin listing", label, id)
		}
		if row.StoredOrigin != spec.Verdict || row.OriginVersion != spec.Version {
			t.Fatalf("case %q: origin state of %q = %q@%d, want %q@%d", label, id, row.StoredOrigin, row.OriginVersion, spec.Verdict, spec.Version)
		}
	}
}

// seedOrphanStoredRows stores corpus sessions directly, in the listed order,
// with their logical parent retained. Rows are settled for adapter and index
// maintenance. With capture, the row also persists a validated publication
// capture whose snapshot retains the logical parent and carries no
// relationship evidence; its recorded source is absent, so neither the
// maintenance pass nor the write path can rewrite it.
func seedOrphanStoredRows(t *testing.T, ctx context.Context, db *store.Store, env orphanEnv, byID map[string]orphanSessionFixture, ids []string, capture bool) {
	t.Helper()
	for _, raw := range ids {
		fixture, ok := byID[raw]
		if !ok {
			t.Fatalf("unknown seeded session %q", raw)
		}
		meta := makeMinimalMeta(t, raw)
		meta.ModelHarness = orphanHarness(t, fixture.Harness)
		versions := ingest.HarvesterVersionRegistry[meta.ModelHarness]
		meta.AdapterVersion = &versions.AdapterVersion
		if fixture.Parent != nil {
			parent, err := ingest.NewSessionID(*fixture.Parent)
			if err != nil {
				t.Fatalf("seeded session %q parent %q: %v", raw, *fixture.Parent, err)
			}
			meta.ParentUUID = &parent
		}
		entry := ingest.StoreEntry{Metadata: meta, CWDProvenance: ingest.CWDNotRecovered}
		if capture {
			meta.SchemaVersion = ingest.CurrentSchemaVersion
			meta.Model = ingest.ModelID("synthetic-orphan-model")
			meta.Source = ingest.SourceInfo{
				Format:   ingest.SourceFormatJSONL,
				FilePath: filepath.Join(env.sourceDir, "absent", raw+".jsonl"),
			}
			meta.ContentHash = schema.ComputeTranscriptHash([]byte(fixture.Transcript))
			meta.MetadataHash = schema.ComputeMetadataHash(meta)
			entry.PublicationCapture = true
			entry.CWDProvenance = ingest.CWDSourceAbsent
		}
		if err := db.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
			t.Fatalf("seed stored session %q: %v", raw, err)
		}
		sid, err := ingest.NewSessionID(raw)
		if err != nil {
			t.Fatalf("seeded session %q: %v", raw, err)
		}
		if err := db.UpdateIndexState(ctx, sid, versions.IndexerVersion, time.Now().UnixMilli()); err != nil {
			t.Fatalf("settle seeded session %q: %v", raw, err)
		}
	}
}

// seedOrphanOrigins records the given origin verdicts and versions.
func seedOrphanOrigins(t *testing.T, ctx context.Context, db *store.Store, origins map[string]orphanOriginSpec) {
	t.Helper()
	for raw, spec := range origins {
		if spec.Verdict == "" {
			t.Fatalf("seeded origin for %q names no verdict", raw)
		}
		sid, err := ingest.NewSessionID(raw)
		if err != nil {
			t.Fatalf("seeded origin session %q: %v", raw, err)
		}
		if err := db.UpdateOriginState(ctx, sid, spec.Verdict, spec.Version); err != nil {
			t.Fatalf("seed origin of %q: %v", raw, err)
		}
	}
}

func orphanOptionalEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func orphanDisplay(v *string) string {
	if v == nil {
		return "null"
	}
	return *v
}

// orphanMetadataParent reads the retained logical ParentUUID from the managed
// metadata file in the test filesystem. It checks the stable root-owned
// location first, then the parent-nested location, so orphans stored at the
// root remain readable while nested children resolve normally.
func orphanMetadataParent(t *testing.T, env orphanEnv, id string) *string {
	t.Helper()
	metaName := id + "--metadata.json"
	rootPath := filepath.Join(env.outputDir, testutil.TestHostSlug, id, metaName)
	if data, err := env.fs.ReadFile(rootPath); err == nil {
		return orphanParentFromMetadata(t, data)
	}
	// Fall back to a nested scan: the child may be stored under its available
	// parent. Walk one level of the host directory for the session folder.
	hostDir := filepath.Join(env.outputDir, testutil.TestHostSlug)
	entries, err := env.fs.ReadDir(hostDir)
	if err != nil {
		t.Fatalf("read host dir %q: %v", hostDir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == id {
			continue
		}
		nested := filepath.Join(hostDir, entry.Name(), "subagents", id, metaName)
		if data, err := env.fs.ReadFile(nested); err == nil {
			return orphanParentFromMetadata(t, data)
		}
	}
	t.Fatalf("managed metadata for session %q not found under %q", id, hostDir)
	return nil
}

func orphanParentFromMetadata(t *testing.T, data []byte) *string {
	t.Helper()
	var meta ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("decode managed metadata: %v", err)
	}
	if meta.ParentUUID == nil {
		return nil
	}
	v := string(*meta.ParentUUID)
	return &v
}
