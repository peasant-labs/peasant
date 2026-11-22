package ingest_test

import (
	"context"
	"maps"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

type countingSnapshotOpenCodeAdapter struct {
	*ingest.OpenCodeAdapter
	calls    *atomic.Int64
	mu       sync.Mutex
	onReturn func(ingest.OpenCodeHistorySnapshot)
}

func (a *countingSnapshotOpenCodeAdapter) SnapshotOpenCodeProvenance(ctx context.Context, dbPath string, sessionID string, opts ingest.OpenCodeSnapshotOptions) (ingest.OpenCodeHistorySnapshot, error) {
	a.calls.Add(1)
	snap, err := a.OpenCodeAdapter.SnapshotOpenCodeProvenance(ctx, dbPath, sessionID, opts)
	if err == nil {
		a.mu.Lock()
		fn := a.onReturn
		a.onReturn = nil
		a.mu.Unlock()
		if fn != nil {
			fn(snap)
		}
	}
	return snap, err
}

type openCodeMemoTestIndexer struct {
	*ingest.OpenCodeIndexer
	forceContextChange bool
	builds             atomic.Int64
	builtDigest        atomic.Value
}

func (i *openCodeMemoTestIndexer) BuildGenerationMetadata(ctx context.Context, in ingest.GenerationBuildInput) (ingest.GenerationMetadataCandidate, error) {
	locator := ""
	if in.RecordedMetadata != nil {
		locator = in.RecordedMetadata.Source.FilePath
	}
	snap, err := in.OpenCode.Snapshot(ctx, locator)
	if err != nil {
		return ingest.GenerationMetadataCandidate{}, err
	}
	meta := *in.RecordedMetadata
	if i.forceContextChange {
		meta.Project.Name = "changed-context-project"
	}
	return ingest.GenerationMetadataCandidate{
		Metadata:             meta,
		Completeness:         indexformat.GenerationCompletenessComplete,
		SourceEvidenceDigest: snap.SourceEvidenceDigest,
	}, nil
}

func (i *openCodeMemoTestIndexer) BuildGeneration(ctx context.Context, in ingest.GenerationBuildInput) (ingest.GenerationCandidate, error) {
	i.builds.Add(1)
	candidate, err := i.OpenCodeIndexer.BuildGeneration(ctx, in)
	if err == nil {
		i.builtDigest.Store(candidate.Result.Generation.SourceEvidenceDigest)
	}
	return candidate, err
}

var (
	_ ingest.GenerationBuilder         = (*openCodeMemoTestIndexer)(nil)
	_ ingest.GenerationMetadataBuilder = (*openCodeMemoTestIndexer)(nil)
)

type memoRouteStore struct {
	*store.Store
	forceValidationProven bool
	refuseSettle          bool
	settles               atomic.Int64
}

func (s *memoRouteStore) ValidateManagedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationValidationRequest) (ingest.ManagedGenerationValidationResult, error) {
	if s.forceValidationProven {
		return ingest.ManagedGenerationValidationResult{Outcome: ingest.ValidationProven}, nil
	}
	return s.Store.ValidateManagedGenerationUnchanged(ctx, request)
}

func (s *memoRouteStore) SettleCapturedGenerationUnchanged(ctx context.Context, request ingest.ManagedGenerationUnchangedProof) (ingest.ManagedGenerationUnchangedSettleOutcome, error) {
	s.settles.Add(1)
	if s.refuseSettle {
		return ingest.ManagedGenerationUnchangedSettleOutcome{}, &ingest.StaleIndexWorkError{SessionID: request.SessionID}
	}
	return s.Store.SettleCapturedGenerationUnchanged(ctx, request)
}

func mutateOpenCodeSQLiteAfterSnapshot(t *testing.T, dbPath string, sessionID string) {
	t.Helper()
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatalf("open opencode sqlite %s: %v", dbPath, err)
	}
	defer func() { _ = conn.Close() }()
	err = sqlitex.Execute(conn, `INSERT INTO session_message (id, session_id, type, time_created, time_updated, data, seq) VALUES (?1, ?2, 'user', ?3, ?3, ?4, ?5)`, &sqlitex.ExecOptions{
		Args: []any{"msg_mutated_after_snapshot", sessionID, 9999999, `{"id":"msg_mutated_after_snapshot","type":"user","text":"mutated after snapshot"}`, 999},
	})
	if err != nil {
		t.Fatalf("mutate opencode sqlite: %v", err)
	}
}

func openCodeMemoVersions(adapterVersion int) map[ingest.Harness]ingest.HarvesterVersions {
	versions := maps.Clone(ingest.HarvesterVersionRegistry)
	versions[ingest.HarnessOpenCode] = ingest.HarvesterVersions{
		AdapterVersion: adapterVersion,
		IndexerVersion: 2,
		IndexVersion:   2,
	}
	return versions
}

func openMemoTestStore(t *testing.T, dbPath string, outputDir string) *store.Store {
	t.Helper()
	artifacts, err := store.NewOSGenerationArtifactStore(outputDir)
	if err != nil {
		t.Fatalf("NewOSGenerationArtifactStore: %v", err)
	}
	locks, err := store.NewFileSessionLocker(outputDir)
	if err != nil {
		t.Fatalf("NewFileSessionLocker: %v", err)
	}
	db, err := store.Open(dbPath, store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locks))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return db
}

func seedOpenCodeMemoStore(t *testing.T) (dbPath string, source testfixture.MaterializedSource, outputDir string) {
	t.Helper()
	source = testfixture.MaterializeByName(t, "native-current-rows")
	outputDir = filepath.Join(t.TempDir(), "managed")
	dbPath = filepath.Join(t.TempDir(), "peasant.db")
	db := openMemoTestStore(t, dbPath, outputDir)
	defer func() { _ = db.Close() }()

	fs := &ingest.OSFileSystem{}
	git := testutil.NoGitResolver()
	cfg := makePipelineConfig(outputDir)
	cfg.Sources = map[ingest.Harness]ingest.SourceConfig{
		ingest.HarnessOpenCode: {Paths: []ingest.ResolvedPath{ingest.ResolvedPath(filepath.Dir(source.Path))}, Enabled: true},
	}
	cfg.StalenessThreshold = 0
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessOpenCode: func(f ingest.FileSystem, g ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
			adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(
				f, g, s, "latest",
				mountedCurrentEnvironment{"OPENCODE_DB": source.Path},
				fs, ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions(),
			)
			if err != nil {
				t.Fatalf("NewOpenCodeAdapterWithCandidateProbe: %v", err)
			}
			return adapter
		},
	}
	indexers := ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})
	v4 := openCodeMemoVersions(4)
	p, err := newTestPipeline(
		fs, git, adapters, cfg,
		ingest.WithStore(db),
		ingest.WithMetricsStore(db),
		ingest.WithIndexLogger(db),
		ingest.WithIndexers(indexers),
		ingest.WithHarvesterVersions(v4),
	)
	if err != nil {
		t.Fatalf("newTestPipeline seed: %v", err)
	}
	res, err := p.Run(context.Background())
	if err != nil || res.Summary.Errors != 0 || res.Summary.StoreError != nil {
		t.Fatalf("seed Run failed: err=%v errors=%d storeErr=%v", err, res.Summary.Errors, res.Summary.StoreError)
	}
	return dbPath, source, outputDir
}

func TestOpenCodeSingleSnapshotAcrossValidationAndFallbackParse(t *testing.T) {
	t.Run("reference-refused-fallback-parse", func(t *testing.T) {
		dbPath, source, outputDir := seedOpenCodeMemoStore(t)
		db := openMemoTestStore(t, dbPath, outputDir)
		defer func() { _ = db.Close() }()

		var snapshotCalls atomic.Int64
		var preMutationDigest atomic.Value
		fs := &ingest.OSFileSystem{}
		git := testutil.NoGitResolver()
		cfg := makePipelineConfig(outputDir)
		cfg.Sources = map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessOpenCode: {Paths: []ingest.ResolvedPath{ingest.ResolvedPath(filepath.Dir(source.Path))}, Enabled: true},
		}
		cfg.StalenessThreshold = 0

		adapters := map[ingest.Harness]ingest.AdapterFactory{
			ingest.HarnessOpenCode: func(f ingest.FileSystem, g ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
				base, err := ingest.NewOpenCodeAdapterWithCandidateProbe(
					f, g, s, "latest",
					mountedCurrentEnvironment{"OPENCODE_DB": source.Path},
					fs, ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions(),
				)
				if err != nil {
					t.Fatalf("NewOpenCodeAdapterWithCandidateProbe: %v", err)
				}
				return &countingSnapshotOpenCodeAdapter{
					OpenCodeAdapter: base,
					calls:           &snapshotCalls,
					onReturn: func(snap ingest.OpenCodeHistorySnapshot) {
						preMutationDigest.Store(snap.SourceEvidenceDigest)
						mutateOpenCodeSQLiteAfterSnapshot(t, source.Path, snap.SessionID)
					},
				}
			},
		}
		idx := &openCodeMemoTestIndexer{
			OpenCodeIndexer:    ingest.NewOpenCodeIndexer(fs),
			forceContextChange: true,
		}
		indexers := map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessOpenCode: idx,
		}
		v5 := openCodeMemoVersions(5)
		p, err := newTestPipeline(
			fs, git, adapters, cfg,
			ingest.WithStore(db),
			ingest.WithMetricsStore(db),
			ingest.WithIndexLogger(db),
			ingest.WithIndexers(indexers),
			ingest.WithHarvesterVersions(v5),
			ingest.WithParseInputContextFieldsForTest(func(ingest.Harness) []string { return []string{"project.name"} }),
		)
		if err != nil {
			t.Fatalf("newTestPipeline refresh: %v", err)
		}
		res, err := p.Run(context.Background())
		if err != nil || res.Summary.Errors != 0 || res.Summary.StoreError != nil {
			t.Fatalf("refresh Run failed: err=%v errors=%d storeErr=%v", err, res.Summary.Errors, res.Summary.StoreError)
		}
		if snapshotCalls.Load() != 1 {
			t.Fatalf("SnapshotOpenCodeProvenance calls = %d, want 1", snapshotCalls.Load())
		}
		if idx.builds.Load() != 1 {
			t.Fatalf("BuildGeneration calls = %d, want 1", idx.builds.Load())
		}
		wantDigest, _ := preMutationDigest.Load().(string)
		gotDigest, _ := idx.builtDigest.Load().(string)
		if wantDigest == "" || gotDigest != wantDigest {
			t.Fatalf("fallback BuildGeneration digest = %q, want pre-mutation %q", gotDigest, wantDigest)
		}
	})

	t.Run("validated-settle-refused-fallback-parse", func(t *testing.T) {
		dbPath, source, outputDir := seedOpenCodeMemoStore(t)
		baseDB := openMemoTestStore(t, dbPath, outputDir)
		defer func() { _ = baseDB.Close() }()
		wrappedDB := &memoRouteStore{Store: baseDB, forceValidationProven: true, refuseSettle: true}

		var snapshotCalls atomic.Int64
		fs := &ingest.OSFileSystem{}
		git := testutil.NoGitResolver()
		cfg := makePipelineConfig(outputDir)
		cfg.Sources = map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessOpenCode: {Paths: []ingest.ResolvedPath{ingest.ResolvedPath(filepath.Dir(source.Path))}, Enabled: true},
		}
		cfg.StalenessThreshold = 0

		adapters := map[ingest.Harness]ingest.AdapterFactory{
			ingest.HarnessOpenCode: func(f ingest.FileSystem, g ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
				base, err := ingest.NewOpenCodeAdapterWithCandidateProbe(
					f, g, s, "latest",
					mountedCurrentEnvironment{"OPENCODE_DB": source.Path},
					fs, ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions(),
				)
				if err != nil {
					t.Fatalf("NewOpenCodeAdapterWithCandidateProbe: %v", err)
				}
				return &countingSnapshotOpenCodeAdapter{
					OpenCodeAdapter: base,
					calls:           &snapshotCalls,
				}
			},
		}
		idx := &openCodeMemoTestIndexer{
			OpenCodeIndexer: ingest.NewOpenCodeIndexer(fs),
		}
		indexers := map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessOpenCode: idx,
		}
		v5 := openCodeMemoVersions(5)
		p, err := newTestPipeline(
			fs, git, adapters, cfg,
			ingest.WithStore(wrappedDB),
			ingest.WithMetricsStore(wrappedDB),
			ingest.WithIndexLogger(wrappedDB),
			ingest.WithIndexers(indexers),
			ingest.WithHarvesterVersions(v5),
		)
		if err != nil {
			t.Fatalf("newTestPipeline refresh: %v", err)
		}
		res, err := p.Run(context.Background())
		if err != nil || res.Summary.Errors != 0 || res.Summary.StoreError != nil {
			t.Fatalf("refresh Run failed: err=%v errors=%d storeErr=%v", err, res.Summary.Errors, res.Summary.StoreError)
		}
		if snapshotCalls.Load() != 1 {
			t.Fatalf("SnapshotOpenCodeProvenance calls = %d, want 1", snapshotCalls.Load())
		}
		if wrappedDB.settles.Load() != 1 {
			t.Fatalf("SettleCapturedGenerationUnchanged calls = %d, want 1", wrappedDB.settles.Load())
		}
		if idx.builds.Load() != 1 {
			t.Fatalf("BuildGeneration calls = %d, want 1", idx.builds.Load())
		}
	})
}
