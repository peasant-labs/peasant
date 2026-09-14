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
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
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

type orphanRunFixture struct {
	Discovered []string           `yaml:"discovered"`
	Reparent   map[string]*string `yaml:"reparent"`
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
}

type orphanCorpusFixture struct {
	Sessions []orphanSessionFixture `yaml:"sessions"`
	Cases    []orphanCaseFixture    `yaml:"cases"`
}

var errOrphanParentFailure = errors.New("synthetic parent extraction failure")

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
	t.Parallel()
	corpus := loadOrphanCorpus(t)
	byID := orphanSessionsByID(t, corpus)
	for _, tc := range corpus.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
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
	fs := env.fs
	if err := fs.MkdirAll(env.sourceDir, 0o755); err != nil {
		t.Fatalf("create source directory %q: %v", env.sourceDir, err)
	}
	if err := fs.MkdirAll(env.outputDir, 0o755); err != nil {
		t.Fatalf("create output directory %q: %v", env.outputDir, err)
	}
	dbPath := filepath.Join(t.TempDir(), "peasant.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
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

	for runIndex, run := range runs {
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
		pipeline, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg,
			ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithIndexLogger(db),
			ingest.WithIndexers(indexers))
		if err != nil {
			t.Fatalf("run %d NewPipeline: %v", runIndex, err)
		}
		if _, err := pipeline.Run(ctx); err != nil {
			t.Fatalf("run %d Run: %v", runIndex, err)
		}
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

	assertOrphanExpectations(t, ctx, db, env, byID, tc, reparents)

	if err := db.Close(); err != nil {
		t.Fatalf("close store before reopen: %v", err)
	}
	reopened, err := store.Open(dbPath)
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
	for id, wantCache := range tc.ExpectParentCache {
		row, err := db.SessionByID(ctx, id)
		if err != nil {
			t.Fatalf("case %q: SessionByID(%q): %v", tc.Name, id, err)
		}
		if row == nil {
			t.Fatalf("case %q: cannot check parent cache of missing session %q", tc.Name, id)
		}
		var gotCache *string
		if row.ParentID != nil && *row.ParentID != "" {
			gotCache = row.ParentID
		}
		if !orphanOptionalEqual(gotCache, wantCache) {
			t.Fatalf("case %q: parent cache of %q = %v, want %v", tc.Name, id, orphanDisplay(gotCache), orphanDisplay(wantCache))
		}
	}
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
