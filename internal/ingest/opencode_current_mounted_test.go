package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	metricspkg "github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

const (
	expectedCurrentMountedCases     = 1
	expectedCurrentMountedNegatives = 2
	expectedCurrentMountedMutations = 5
	expectedCurrentMountedBehaviors = 6
)

type currentMountedFixture struct {
	DeclaredCases             int                              `yaml:"declared_cases"`
	Cases                     []currentMountedCase             `yaml:"cases"`
	DeclaredNegativeCases     int                              `yaml:"declared_negative_cases"`
	NegativeCases             []currentMountedNegativeCase     `yaml:"negative_cases"`
	DeclaredLoaderMutations   int                              `yaml:"declared_loader_mutations"`
	LoaderMutations           []currentMountedLoaderMutation   `yaml:"loader_mutations"`
	DeclaredBehaviorMutations int                              `yaml:"declared_behavior_mutations"`
	BehaviorMutations         []currentMountedBehaviorMutation `yaml:"behavior_mutations"`
}

type currentMountedCase struct {
	Name                  string   `yaml:"name"`
	SourceFixture         string   `yaml:"source_fixture"`
	SessionID             string   `yaml:"session_id"`
	NonStaleSessionID     string   `yaml:"non_stale_session_id"`
	ExpectedEntries       int      `yaml:"expected_entries"`
	ExpectedMinimumTurns  int      `yaml:"expected_minimum_turns"`
	ExpectedMetadataTurns int      `yaml:"expected_metadata_turns"`
	ExpectedMetadataTools int      `yaml:"expected_metadata_tools"`
	ExpectedTokensIn      int      `yaml:"expected_tokens_in"`
	ExpectedTokensOut     int      `yaml:"expected_tokens_out"`
	ExpectedNew           int      `yaml:"expected_new"`
	ExpectedUnchanged     int      `yaml:"expected_unchanged"`
	ManagedFormat         string   `yaml:"managed_format"`
	ForbiddenMarkers      []string `yaml:"forbidden_markers"`
}

type currentMountedLoaderMutation struct {
	Name          string `yaml:"name"`
	Kind          string `yaml:"kind"`
	ErrorContains string `yaml:"error_contains"`
}

type currentMountedNegativeCase struct {
	Name          string `yaml:"name"`
	Kind          string `yaml:"kind"`
	ErrorContains string `yaml:"error_contains"`
	RowData       string `yaml:"row_data"`
}

type currentMountedBehaviorMutation struct {
	Name          string `yaml:"name"`
	Kind          string `yaml:"kind"`
	ErrorContains string `yaml:"error_contains"`
}

//go:embed testdata/opencode_current_mounted.yaml
var currentMountedYAML []byte

func loadCurrentMountedFixture(data []byte) (currentMountedFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture currentMountedFixture
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, fmt.Errorf("decode mounted current OpenCode fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fixture, fmt.Errorf("decode mounted current OpenCode fixture: expected exactly one YAML document: %w", err)
	}
	if fixture.DeclaredCases != expectedCurrentMountedCases || len(fixture.Cases) != expectedCurrentMountedCases || fixture.DeclaredNegativeCases != expectedCurrentMountedNegatives || len(fixture.NegativeCases) != expectedCurrentMountedNegatives || fixture.DeclaredLoaderMutations != expectedCurrentMountedMutations || len(fixture.LoaderMutations) != expectedCurrentMountedMutations || fixture.DeclaredBehaviorMutations != expectedCurrentMountedBehaviors || len(fixture.BehaviorMutations) != expectedCurrentMountedBehaviors {
		return fixture, fmt.Errorf("validate mounted current OpenCode fixture row guard: cases=%d/%d negatives=%d/%d mutations=%d/%d behavior_mutations=%d/%d", fixture.DeclaredCases, len(fixture.Cases), fixture.DeclaredNegativeCases, len(fixture.NegativeCases), fixture.DeclaredLoaderMutations, len(fixture.LoaderMutations), fixture.DeclaredBehaviorMutations, len(fixture.BehaviorMutations))
	}
	names := make(map[string]struct{})
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" || testCase.SourceFixture == "" || testCase.SessionID == "" || testCase.NonStaleSessionID == "" || testCase.NonStaleSessionID == testCase.SessionID || testCase.ExpectedEntries <= 0 || testCase.ExpectedMinimumTurns <= 0 || testCase.ExpectedMetadataTurns <= 0 || testCase.ExpectedMetadataTools <= 0 || testCase.ExpectedTokensIn <= 0 || testCase.ExpectedTokensOut <= 0 || testCase.ManagedFormat == "" {
			return fixture, fmt.Errorf("validate mounted current OpenCode fixture %q: required values are incomplete", testCase.Name)
		}
		if _, duplicate := names[testCase.Name]; duplicate {
			return fixture, fmt.Errorf("duplicate mounted case name %q", testCase.Name)
		}
		names[testCase.Name] = struct{}{}
	}
	for _, mutation := range fixture.LoaderMutations {
		if mutation.Name == "" || mutation.Kind == "" || mutation.ErrorContains == "" {
			return fixture, errors.New("validate mounted current OpenCode fixture: incomplete loader mutation")
		}
		if _, duplicate := names[mutation.Name]; duplicate {
			return fixture, fmt.Errorf("duplicate mounted case name %q", mutation.Name)
		}
		names[mutation.Name] = struct{}{}
	}
	for _, negative := range fixture.NegativeCases {
		if negative.Name == "" || negative.Kind == "" || negative.ErrorContains == "" {
			return fixture, errors.New("validate mounted current OpenCode fixture: incomplete negative case")
		}
		if _, duplicate := names[negative.Name]; duplicate {
			return fixture, fmt.Errorf("duplicate mounted case name %q", negative.Name)
		}
		names[negative.Name] = struct{}{}
	}
	for _, mutation := range fixture.BehaviorMutations {
		if mutation.Name == "" || mutation.Kind == "" || mutation.ErrorContains == "" {
			return fixture, errors.New("validate mounted current OpenCode fixture: incomplete behavior mutation")
		}
		switch mutation.Kind {
		case "nondeterministic_managed_bytes", "omitted_stale_target", "wrong_managed_origin", "removed_source_access", "retained_stale_state", "poisoned_retry_state":
		default:
			return fixture, fmt.Errorf("validate mounted current OpenCode fixture: unknown behavior mutation %q", mutation.Kind)
		}
		if _, duplicate := names[mutation.Name]; duplicate {
			return fixture, fmt.Errorf("duplicate mounted case name %q", mutation.Name)
		}
		names[mutation.Name] = struct{}{}
	}
	return fixture, nil
}

type mountedCurrentEnvironment map[string]string

func (environment mountedCurrentEnvironment) LookupEnv(key string) (string, bool) {
	value, ok := environment[key]
	return value, ok
}

type managedProjectionCommitReader struct {
	mu      sync.Mutex
	paths   []string
	payload [][]byte
}

// mountedCurrentSnapshot is the complete observable state produced by the
// mounted pipeline. ComputedAt is deliberately normalized because it records
// wall-clock observation time rather than transcript semantics.
type mountedCurrentSnapshot struct {
	ManagedBytes  []byte
	Metadata      any
	StoreEntries  any
	Entries       []schema.SessionEntry
	Turns         []ingest.Turn
	Detail        any
	Metrics       *ingest.SessionMetrics
	IndexStates   map[ingest.SessionID]int
	ArtifactNames []string
}

type mountedCurrentIndexerRecorder struct {
	ingest.TranscriptIndexer
	mu       sync.Mutex
	origins  []ingest.TranscriptOrigin
	byteRuns int
}

func (recorder *mountedCurrentIndexerRecorder) IndexTranscriptBytes(ctx context.Context, session ingest.DiscoveredSession, data []byte) ([]schema.SessionEntry, error) {
	recorder.mu.Lock()
	recorder.origins = append(recorder.origins, session.TranscriptOrigin)
	recorder.byteRuns++
	recorder.mu.Unlock()
	return recorder.TranscriptIndexer.IndexTranscriptBytes(ctx, session, data)
}

func (recorder *mountedCurrentIndexerRecorder) IndexTranscript(ctx context.Context, session ingest.DiscoveredSession) ([]schema.SessionEntry, error) {
	recorder.mu.Lock()
	recorder.origins = append(recorder.origins, session.TranscriptOrigin)
	recorder.byteRuns++
	recorder.mu.Unlock()
	return recorder.TranscriptIndexer.IndexTranscript(ctx, session)
}

func (recorder *mountedCurrentIndexerRecorder) TranscriptSourceKindFor(session ingest.DiscoveredSession) ingest.TranscriptSourceKind {
	if resolver, ok := recorder.TranscriptIndexer.(ingest.SessionTranscriptSourceResolver); ok {
		return resolver.TranscriptSourceKindFor(session)
	}
	return recorder.TranscriptIndexer.SourceKind()
}

func captureMountedCurrentSnapshot(t testing.TB, output ingest.ResolvedPath, store *testutil.StubSessionStore, metrics *testutil.StubMetricsStore, sessionID ingest.SessionID, metadata *ingest.UnifiedMetadata) mountedCurrentSnapshot {
	t.Helper()
	managedPath := filepath.Join(ingest.SessionDir(output.String(), string(metadata.HostSlug), string(sessionID), ""), string(sessionID)+"--transcript.json")
	managedBytes, err := os.ReadFile(managedPath)
	if err != nil {
		t.Fatalf("read managed current artifact: %v", err)
	}
	entries := append([]schema.SessionEntry(nil), metrics.IndexedEntries[sessionID]...)
	turns := append([]ingest.Turn(nil), api.EntriesToTurns(entries)...)
	detail := api.SessionToDetail(&ingest.Session{ID: sessionID, Harness: ingest.HarnessOpenCode, Turns: turns, Model: "synthetic-model"})
	if detail == nil {
		t.Fatal("mounted snapshot has no session detail")
	}
	metric := metrics.SavedMetrics[sessionID]
	if metric == nil {
		t.Fatal("mounted snapshot has no metrics")
	}
	metricCopy := *metric
	metricCopy.ComputedAt = nil
	stateCopy := make(map[ingest.SessionID]int, len(metrics.IndexStates))
	for id, version := range metrics.IndexStates {
		stateCopy[id] = version
	}
	artifacts := listMountedArtifacts(t, output.String())
	return mountedCurrentSnapshot{
		ManagedBytes:  append([]byte(nil), managedBytes...),
		Metadata:      normalizeMountedValue(t, metadata),
		StoreEntries:  normalizeMountedValue(t, store.InsertedEntries),
		Entries:       entries,
		Turns:         turns,
		Detail:        detail,
		Metrics:       &metricCopy,
		IndexStates:   stateCopy,
		ArtifactNames: artifacts,
	}
}

func normalizeMountedValue(t testing.TB, value any) any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal mounted state for deterministic comparison: %v", err)
	}
	var normalized any
	if err := json.Unmarshal(data, &normalized); err != nil {
		t.Fatalf("unmarshal mounted state for deterministic comparison: %v", err)
	}
	stripMountedObservationTimes(normalized)
	return normalized
}

func stripMountedObservationTimes(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if key == "ingested_at" || key == "ingestedAt" || key == "ingested" || key == "indexed_at" || key == "indexedAt" || key == "computed_at" || key == "computedAt" || key == "derivedAt" || key == "metadataHash" {
				delete(typed, key)
				continue
			}
			stripMountedObservationTimes(nested)
		}
	case []any:
		for _, nested := range typed {
			stripMountedObservationTimes(nested)
		}
	}
}

func listMountedArtifacts(t testing.TB, root string) []string {
	t.Helper()
	var artifacts []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if strings.HasPrefix(entry.Name(), ".tmp-") {
				return fmt.Errorf("temporary artifact directory survived at %q", path)
			}
			return nil
		}
		if strings.Contains(entry.Name(), ".tmp-") {
			return fmt.Errorf("temporary artifact survived at %q", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		artifacts = append(artifacts, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return artifacts
}

func assertMountedCurrentSnapshotEqual(t testing.TB, label string, want, got mountedCurrentSnapshot) {
	t.Helper()
	if !bytes.Equal(want.ManagedBytes, got.ManagedBytes) || !reflect.DeepEqual(want.Metadata, got.Metadata) || !reflect.DeepEqual(want.StoreEntries, got.StoreEntries) || !reflect.DeepEqual(want.Entries, got.Entries) || !reflect.DeepEqual(want.Turns, got.Turns) || !reflect.DeepEqual(want.Detail, got.Detail) || !reflect.DeepEqual(want.Metrics, got.Metrics) || !reflect.DeepEqual(want.IndexStates, got.IndexStates) || !reflect.DeepEqual(want.ArtifactNames, got.ArtifactNames) {
		t.Fatalf("%s did not reproduce complete mounted state: bytes=%t metadata=%t store=%t entries=%t turns=%t detail=%t metrics=%t index_state=%t artifacts=%t", label, bytes.Equal(want.ManagedBytes, got.ManagedBytes), reflect.DeepEqual(want.Metadata, got.Metadata), reflect.DeepEqual(want.StoreEntries, got.StoreEntries), reflect.DeepEqual(want.Entries, got.Entries), reflect.DeepEqual(want.Turns, got.Turns), reflect.DeepEqual(want.Detail, got.Detail), reflect.DeepEqual(want.Metrics, got.Metrics), reflect.DeepEqual(want.IndexStates, got.IndexStates), reflect.DeepEqual(want.ArtifactNames, got.ArtifactNames))
	}
}

func snapshotIndependentlyMaterializedCurrentProjection(t testing.TB, metadata *ingest.UnifiedMetadata, sessionID ingest.SessionID, managed []byte) mountedCurrentSnapshot {
	t.Helper()
	metrics := testutil.NewStubMetricsStore()
	indexer := ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeFullDepth(true), ingest.WithOpenCodeFullContent(true))
	entries, err := indexer.IndexTranscriptBytes(t.Context(), ingest.DiscoveredSession{SessionID: sessionID, Harness: ingest.HarnessOpenCode, TranscriptOrigin: ingest.TranscriptOriginOpenCodeCurrentSQLite}, managed)
	if err != nil {
		t.Fatalf("index independently materialized current projection: %v", err)
	}
	if err := metrics.IndexSessionEntries(t.Context(), sessionID, entries); err != nil {
		t.Fatalf("persist independently materialized current entries: %v", err)
	}
	if computed, err := metricspkg.NewEngine(metrics).ComputeMetrics(t.Context(), []ingest.SessionID{sessionID}); err != nil || computed != 1 {
		t.Fatalf("compute independently materialized current metrics: computed=%d error=%v", computed, err)
	}
	turns := append([]ingest.Turn(nil), api.EntriesToTurns(entries)...)
	detail := api.SessionToDetail(&ingest.Session{ID: sessionID, Harness: ingest.HarnessOpenCode, Turns: turns, Model: "synthetic-model"})
	metric := *metrics.SavedMetrics[sessionID]
	metric.ComputedAt = nil
	return mountedCurrentSnapshot{ManagedBytes: append([]byte(nil), managed...), Metadata: normalizeMountedValue(t, metadata), Entries: append([]schema.SessionEntry(nil), entries...), Turns: turns, Detail: detail, Metrics: &metric, IndexStates: map[ingest.SessionID]int{}}
}

type mountedCurrentFaultSource struct {
	ingest.OpenCodeSQLiteSource
	kind       string
	rowData    string
	controller *mountedFaultController
}

type mountedFaultController struct {
	mu        sync.Mutex
	remaining int
}

func (controller *mountedFaultController) consume() bool {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.remaining == 0 {
		return false
	}
	controller.remaining--
	return true
}

func (source mountedCurrentFaultSource) CurrentMessages(ctx context.Context, request ingest.OpenCodeCurrentPageRequest) (ingest.OpenCodeCurrentPage, error) {
	page, err := source.OpenCodeSQLiteSource.CurrentMessages(ctx, request)
	if err != nil {
		return page, err
	}
	if !source.controller.consume() {
		return page, nil
	}
	switch source.kind {
	case "malformed_native_data":
		if len(page.Messages) == 0 {
			return page, errors.New("fault source received empty real page")
		}
		page.Messages[0].Data = source.rowData
		return page, nil
	case "canceled_page":
		return ingest.OpenCodeCurrentPage{}, context.Canceled
	default:
		return page, fmt.Errorf("unknown mounted fault kind %q", source.kind)
	}
}

func (reader *managedProjectionCommitReader) ReadTranscript(_ context.Context, path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	reader.mu.Lock()
	reader.paths = append(reader.paths, path)
	reader.payload = append(reader.payload, append([]byte(nil), data...))
	reader.mu.Unlock()
	return data, nil
}

func TestCurrentOpenCodeMountedHarvestDetailMetricsRepeatAndReindex(t *testing.T) {
	fixture, err := loadCurrentMountedFixture(currentMountedYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, testCase.SourceFixture)
			root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
			if err != nil {
				t.Fatal(err)
			}
			output, err := ingest.NewResolvedPath(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			environment := mountedCurrentEnvironment{"OPENCODE_DB": materialized.Path}
			var sourceOpenMu sync.Mutex
			sourceOpenCount := 0
			opener := func(ctx context.Context, path ingest.OpenCodeSQLiteSourcePath, options ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
				sourceOpenMu.Lock()
				sourceOpenCount++
				sourceOpenMu.Unlock()
				return ingest.OpenOpenCodeSQLiteSource(ctx, path, options)
			}
			adapterFactory := func(fs ingest.FileSystem, git ingest.GitResolver, installationSalt salt.Salt) ingest.SourceAdapter {
				candidateFS, ok := fs.(ingest.OpenCodeCandidateFileSystem)
				if !ok {
					t.Fatalf("production filesystem lacks candidate surface")
				}
				adapter, adapterErr := ingest.NewOpenCodeAdapterWithCandidateProbe(fs, git, installationSalt, "latest", environment, candidateFS, opener, ingest.DefaultOpenCodeSQLiteSourceOptions())
				if adapterErr != nil {
					t.Fatalf("construct production current adapter: %v", adapterErr)
				}
				return adapter
			}
			// Two independent source opens must produce byte-identical managed
			// artifacts. Comparing a persisted file to itself would let clocks or
			// map-order nondeterminism survive.
			forcedAdapter := adapterFactory(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), salt.Salt{})
			discovered, err := forcedAdapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
			if err != nil || len(discovered) != 1 {
				t.Fatalf("discover for independent materialization: sessions=%d error=%v", len(discovered), err)
			}
			materializer := forcedAdapter.(ingest.TranscriptMaterializer)
			firstMetadata, firstManaged, err := materializer.MaterializeTranscript(t.Context(), discovered[0])
			if err != nil {
				t.Fatal(err)
			}
			secondMetadata, secondManaged, err := materializer.MaterializeTranscript(t.Context(), discovered[0])
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(firstManaged, secondManaged) || !reflect.DeepEqual(firstMetadata, secondMetadata) {
				t.Fatalf("independent materializations diverged\nfirst=%s\nsecond=%s", firstManaged, secondManaged)
			}
			sessionID := mustMountedSessionID(t, testCase.SessionID)
			firstProjection := snapshotIndependentlyMaterializedCurrentProjection(t, firstMetadata, sessionID, firstManaged)
			secondProjection := snapshotIndependentlyMaterializedCurrentProjection(t, secondMetadata, sessionID, secondManaged)
			assertMountedCurrentSnapshotEqual(t, "independent materialization", firstProjection, secondProjection)
			adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessOpenCode: adapterFactory}
			store := &testutil.StubSessionStore{}
			metrics := testutil.NewStubMetricsStore()
			metrics.TitleHarness = ingest.HarnessOpenCode
			metrics.TitleProjectPath = "/synthetic/parity"
			commitReader := &managedProjectionCommitReader{}
			gitAnalyzer := &testutil.StubGitDiffAnalyzer{CommitInfos: []ingest.CommitInfo{{Hash: "0123456789abcdef0123456789abcdef01234567", AuthorEmail: testutil.TestEmail, Message: "synthetic commit"}}}
			config := ingest.PipelineConfig{Sources: map[ingest.Harness]ingest.SourceConfig{ingest.HarnessOpenCode: {Enabled: true, Paths: []ingest.ResolvedPath{root}}}, OutputDir: output, Parallelism: 1}
			pipeline, err := ingest.NewPipeline(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), adapters, config,
				ingest.WithStore(store), ingest.WithMetricsStore(metrics),
				ingest.WithAnalyzer(metricspkg.NewEngine(metrics)),
				ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{ingest.HarnessOpenCode: ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeFullDepth(true), ingest.WithOpenCodeFullContent(true))}),
				ingest.WithGitDiffAnalyzer(gitAnalyzer), ingest.WithCommitTranscriptReader(commitReader),
			)
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(t.Context())
			if err != nil {
				t.Fatalf("mounted current harvest: %v", err)
			}
			if result.Summary.New != testCase.ExpectedNew || len(store.InsertedEntries) != 1 {
				t.Fatalf("mounted harvest summary=%+v store=%d", result.Summary, len(store.InsertedEntries))
			}
			entries := metrics.IndexedEntries[sessionID]
			if len(entries) != testCase.ExpectedEntries {
				t.Fatalf("mounted entries=%d want %d", len(entries), testCase.ExpectedEntries)
			}
			if metrics.SavedMetrics[sessionID] == nil {
				t.Fatal("mounted metrics were not computed")
			}
			turns := api.EntriesToTurns(entries)
			if len(turns) < testCase.ExpectedMinimumTurns {
				t.Fatalf("mounted turns=%d want at least %d", len(turns), testCase.ExpectedMinimumTurns)
			}
			detail := api.SessionToDetail(&ingest.Session{ID: sessionID, Harness: ingest.HarnessOpenCode, Turns: turns, Model: "synthetic-model"})
			if detail == nil || len(detail.Turns) != len(turns) {
				t.Fatalf("mounted session detail missing turns: %+v", detail)
			}

			metadata := store.InsertedEntries[0].Metadata
			if metadata.Stats.TurnCount != testCase.ExpectedMetadataTurns || metadata.Stats.ToolCallCount != testCase.ExpectedMetadataTools || metadata.Stats.TokensIn != testCase.ExpectedTokensIn || metadata.Stats.TokensOut != testCase.ExpectedTokensOut {
				t.Fatalf("mounted metadata stats = %+v, want turns/tools/tokens %d/%d/%d/%d from the indexed semantic corpus", metadata.Stats, testCase.ExpectedMetadataTurns, testCase.ExpectedMetadataTools, testCase.ExpectedTokensIn, testCase.ExpectedTokensOut)
			}
			managedPath := filepath.Join(ingest.SessionDir(output.String(), string(metadata.HostSlug), testCase.SessionID, ""), testCase.SessionID+"--transcript.json")
			managedBytes, err := os.ReadFile(managedPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(managedBytes, []byte(`"format":"`+testCase.ManagedFormat+`"`)) {
				t.Fatalf("managed projection has unexpected envelope: %s", managedBytes)
			}
			for _, marker := range testCase.ForbiddenMarkers {
				if bytes.Contains(managedBytes, []byte(marker)) {
					t.Fatalf("managed projection leaked %q", marker)
				}
			}
			commitReader.mu.Lock()
			if len(commitReader.paths) == 0 {
				t.Fatal("commit detection never inspected the managed current projection")
			}
			for index, path := range commitReader.paths {
				if path == materialized.Path || strings.HasSuffix(path, "-wal") || strings.HasSuffix(path, "-shm") || !bytes.Equal(commitReader.payload[index], managedBytes) {
					t.Fatalf("commit detection source %q was not exactly managed projection bytes", path)
				}
			}
			commitReader.mu.Unlock()

			ingested := time.Now().Add(time.Hour).UnixMilli()
			store.LocationsByID = map[ingest.SessionID]ingest.SessionLocation{sessionID: {HostSlug: string(metadata.HostSlug), IngestedMs: &ingested, SchemaVersion: int(ingest.CurrentSchemaVersion)}}
			repeated, err := pipeline.Run(t.Context())
			if err != nil {
				t.Fatalf("repeat mounted harvest: %v", err)
			}
			if repeated.Summary.Unchanged != testCase.ExpectedUnchanged {
				t.Fatalf("repeat summary=%+v", repeated.Summary)
			}
			nonStaleID, err := ingest.NewSessionID(testCase.NonStaleSessionID)
			if err != nil {
				t.Fatal(err)
			}
			nonStaleMetric := *metrics.SavedMetrics[sessionID]
			metrics.IndexedEntries[nonStaleID] = append([]schema.SessionEntry(nil), metrics.IndexedEntries[sessionID]...)
			metrics.SavedMetrics[nonStaleID] = &nonStaleMetric
			metrics.IndexStates[nonStaleID] = ingest.CurrentIndexVersion
			canonicalSnapshot := captureMountedCurrentSnapshot(t, output, store, metrics, sessionID, metadata)

			if err := os.Remove(materialized.Path); err != nil {
				t.Fatal(err)
			}
			metrics.StaleIndexSessions = []ingest.SessionID{sessionID}
			metrics.IndexedEntries = make(map[ingest.SessionID][]schema.SessionEntry)
			metrics.SavedMetrics = make(map[ingest.SessionID]*ingest.SessionMetrics)
			metrics.IndexStates = make(map[ingest.SessionID]int)
			metrics.IndexedEntries[nonStaleID] = append([]schema.SessionEntry(nil), canonicalSnapshot.Entries...)
			metrics.SavedMetrics[nonStaleID] = &nonStaleMetric
			metrics.IndexStates[nonStaleID] = ingest.CurrentIndexVersion
			metrics.ListStaleCalledWithVersion = 0
			config.Reindex = true
			config.Force = false
			sourceOpenMu.Lock()
			sourceOpenCount = 0
			sourceOpenMu.Unlock()
			reindexer := &mountedCurrentIndexerRecorder{TranscriptIndexer: ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeFullDepth(true), ingest.WithOpenCodeFullContent(true))}
			reindex, err := ingest.NewPipeline(&ingest.OSFileSystem{}, testutil.NoGitResolver(), adapters, config, ingest.WithStore(store), ingest.WithMetricsStore(metrics), ingest.WithAnalyzer(metricspkg.NewEngine(metrics)), ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{ingest.HarnessOpenCode: reindexer}))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reindex.Run(t.Context()); err != nil {
				t.Fatalf("source-missing reindex: %v", err)
			}
			reindexedSnapshot := captureMountedCurrentSnapshot(t, output, store, metrics, sessionID, metadata)
			assertMountedCurrentSnapshotEqual(t, "source-free stale reindex", canonicalSnapshot, reindexedSnapshot)
			sourceOpenMu.Lock()
			removedSourceOpens := sourceOpenCount
			sourceOpenMu.Unlock()
			reindexer.mu.Lock()
			reindexOrigins := append([]ingest.TranscriptOrigin(nil), reindexer.origins...)
			reindexByteRuns := reindexer.byteRuns
			reindexer.mu.Unlock()
			if metrics.ListStaleCalledWithVersion != ingest.CurrentIndexVersion || reindexByteRuns != 1 || len(reindexOrigins) != 1 || reindexOrigins[0] != ingest.TranscriptOriginOpenCodeCurrentSQLite || removedSourceOpens != 0 || !reflect.DeepEqual(metrics.IndexedEntries[nonStaleID], canonicalSnapshot.Entries) || !reflect.DeepEqual(metrics.SavedMetrics[nonStaleID], &nonStaleMetric) || metrics.IndexStates[nonStaleID] != ingest.CurrentIndexVersion {
				t.Fatalf("source-free stale reindex selected or mutated the wrong state: stale_version=%d managed_current_runs=%d origins=%v removed_source_opens=%d non_stale_entries=%t non_stale_metrics=%t non_stale_index_version=%d", metrics.ListStaleCalledWithVersion, reindexByteRuns, reindexOrigins, removedSourceOpens, reflect.DeepEqual(metrics.IndexedEntries[nonStaleID], canonicalSnapshot.Entries), reflect.DeepEqual(metrics.SavedMetrics[nonStaleID], &nonStaleMetric), metrics.IndexStates[nonStaleID])
			}
		})
	}
}

func TestCurrentMountedFixtureLoaderRejectsMutations(t *testing.T) {
	fixture, err := loadCurrentMountedFixture(currentMountedYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range fixture.LoaderMutations {
		mutated := append([]byte(nil), currentMountedYAML...)
		switch mutation.Kind {
		case "unknown_field":
			mutated = bytes.Replace(mutated, []byte("source_fixture:"), []byte("unexpected:"), 1)
		case "trailing_document":
			mutated = append(mutated, []byte("\n---\nextra: true\n")...)
		case "declared_count":
			mutated = bytes.Replace(mutated, []byte("declared_cases: 1"), []byte("declared_cases: 2"), 1)
		case "duplicate_name":
			mutated = bytes.Replace(mutated, []byte("reject-unknown-field"), []byte(fixture.Cases[0].Name), 1)
		case "unknown_behavior_kind":
			mutated = bytes.Replace(mutated, []byte("nondeterministic_managed_bytes"), []byte("unknown_behavior"), 1)
		default:
			t.Fatalf("unknown mutation kind %q", mutation.Kind)
		}
		if _, err := loadCurrentMountedFixture(mutated); err == nil || !strings.Contains(err.Error(), mutation.ErrorContains) {
			t.Errorf("mutation %q error=%v", mutation.Name, err)
		}
	}
}

func TestCurrentOpenCodeMountedFailuresLeaveNoPartialState(t *testing.T) {
	fixture, err := loadCurrentMountedFixture(currentMountedYAML)
	if err != nil {
		t.Fatal(err)
	}
	base := fixture.Cases[0]
	for _, negative := range fixture.NegativeCases {
		negative := negative
		t.Run(negative.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, base.SourceFixture)
			root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
			if err != nil {
				t.Fatal(err)
			}
			output, err := ingest.NewResolvedPath(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			environment := mountedCurrentEnvironment{"OPENCODE_DB": materialized.Path}
			controller := &mountedFaultController{remaining: 1}
			opener := func(ctx context.Context, path ingest.OpenCodeSQLiteSourcePath, options ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
				source, openErr := ingest.OpenOpenCodeSQLiteSource(ctx, path, options)
				if openErr != nil {
					return nil, openErr
				}
				return mountedCurrentFaultSource{OpenCodeSQLiteSource: source, kind: negative.Kind, rowData: negative.RowData, controller: controller}, nil
			}
			adapterFactory := func(fs ingest.FileSystem, git ingest.GitResolver, installationSalt salt.Salt) ingest.SourceAdapter {
				adapter, adapterErr := ingest.NewOpenCodeAdapterWithCandidateProbe(fs, git, installationSalt, "latest", environment, fs.(ingest.OpenCodeCandidateFileSystem), opener, ingest.DefaultOpenCodeSQLiteSourceOptions())
				if adapterErr != nil {
					t.Fatal(adapterErr)
				}
				return adapter
			}
			store := &testutil.StubSessionStore{}
			metrics := testutil.NewStubMetricsStore()
			config := ingest.PipelineConfig{Sources: map[ingest.Harness]ingest.SourceConfig{ingest.HarnessOpenCode: {Enabled: true, Paths: []ingest.ResolvedPath{root}}}, OutputDir: output, Parallelism: 1}
			pipeline, err := ingest.NewPipeline(&ingest.OSFileSystem{}, testutil.NoGitResolver(), map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessOpenCode: adapterFactory}, config, ingest.WithStore(store), ingest.WithMetricsStore(metrics), ingest.WithAnalyzer(metricspkg.NewEngine(metrics)), ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{ingest.HarnessOpenCode: ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeFullDepth(true), ingest.WithOpenCodeFullContent(true))}))
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(t.Context())
			if err != nil {
				t.Fatalf("pipeline should report per-session failure without aborting: %v", err)
			}
			if len(result.Sessions) != 1 || result.Sessions[0].Error == nil || !strings.Contains(result.Sessions[0].Error.Error(), negative.ErrorContains) {
				t.Fatalf("mounted failure result=%+v want %q", result.Sessions, negative.ErrorContains)
			}
			if negative.Kind == "canceled_page" && !errors.Is(result.Sessions[0].Error, context.Canceled) {
				t.Fatalf("mounted cancellation cause was not preserved for errors.Is: %v", result.Sessions[0].Error)
			}
			if len(store.InsertedEntries) != 0 || len(metrics.IndexedEntries) != 0 || len(metrics.SavedMetrics) != 0 {
				t.Fatalf("mounted failure left partial state: store=%d entries=%d metrics=%d", len(store.InsertedEntries), len(metrics.IndexedEntries), len(metrics.SavedMetrics))
			}
			entries, readErr := os.ReadDir(output.String())
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("mounted failure left temporary or final artifact %q", entries[0].Name())
			}
			// The same source, opener, adapter, and pipeline must remain reusable
			// after a malformed or canceled read; the fault controller permits the
			// second bounded attempt through unchanged production wiring.
			reused, reuseErr := pipeline.Run(t.Context())
			if reuseErr != nil || reused.Summary.New != base.ExpectedNew || len(store.InsertedEntries) != 1 || len(metrics.IndexedEntries) != 1 || len(metrics.SavedMetrics) != 1 {
				t.Fatalf("bounded reuse after %s failed: summary=%+v store=%d entries=%d metrics=%d error=%v", negative.Kind, reused.Summary, len(store.InsertedEntries), len(metrics.IndexedEntries), len(metrics.SavedMetrics), reuseErr)
			}
			reusedSnapshot := captureMountedCurrentSnapshot(t, output, store, metrics, mustMountedSessionID(t, base.SessionID), store.InsertedEntries[0].Metadata)

			expectedOutput, err := ingest.NewResolvedPath(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			expectedStore := &testutil.StubSessionStore{}
			expectedMetrics := testutil.NewStubMetricsStore()
			expectedConfig := config
			expectedConfig.OutputDir = expectedOutput
			expectedPipeline, err := ingest.NewPipeline(&ingest.OSFileSystem{}, testutil.NoGitResolver(), map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessOpenCode: adapterFactory}, expectedConfig, ingest.WithStore(expectedStore), ingest.WithMetricsStore(expectedMetrics), ingest.WithAnalyzer(metricspkg.NewEngine(expectedMetrics)), ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{ingest.HarnessOpenCode: ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeFullDepth(true), ingest.WithOpenCodeFullContent(true))}))
			if err != nil {
				t.Fatal(err)
			}
			if expectedResult, expectedErr := expectedPipeline.Run(t.Context()); expectedErr != nil || expectedResult.Summary.New != base.ExpectedNew || len(expectedStore.InsertedEntries) != 1 {
				t.Fatalf("independent successful mounted baseline after %s: summary=%+v store=%d error=%v", negative.Kind, expectedResult.Summary, len(expectedStore.InsertedEntries), expectedErr)
			}
			expectedSnapshot := captureMountedCurrentSnapshot(t, expectedOutput, expectedStore, expectedMetrics, mustMountedSessionID(t, base.SessionID), expectedStore.InsertedEntries[0].Metadata)
			assertMountedCurrentSnapshotEqual(t, "bounded retry after "+negative.Kind, expectedSnapshot, reusedSnapshot)
		})
	}
}

func mustMountedSessionID(t testing.TB, raw string) ingest.SessionID {
	t.Helper()
	sessionID, err := ingest.NewSessionID(raw)
	if err != nil {
		t.Fatal(err)
	}
	return sessionID
}
