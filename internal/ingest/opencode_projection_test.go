package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	metricspkg "github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/opencode_projection_cap.yaml
var openCodeProjectionCapData []byte

const capProjectionPath = "/synthetic/store/opencode-managed-projection.json"

// openCodeSQLiteFileHeader is the first bytes of every SQLite database. It is
// how the provider's own file announces itself, and the only thing a reader
// needs in order to refuse it.
const openCodeSQLiteFileHeader = "SQLite format 3\x00"

// projectionOutcome is what a reader owes one file at the managed-projection
// path. There are two answers: read it, or refuse it for being the provider's
// database. Size is not one of them, which is the whole point of this corpus.
type projectionOutcome string

const (
	// projectionRead: the reader proceeds to read the file, whatever its size.
	projectionRead projectionOutcome = "read"
	// projectionRefusedAsDatabase: the file holds the provider's database and
	// is refused without being read into memory.
	projectionRefusedAsDatabase projectionOutcome = "refused-as-provider-database"
)

// openCodeProjectionCapCase sizes one synthetic projection file relative to the
// preview bound and states what the readers owe it.
type openCodeProjectionCapCase struct {
	Name            string            `yaml:"name"`
	Origin          string            `yaml:"origin"`
	OffsetFromBound int64             `yaml:"offset_from_bound"`
	SQLiteHeader    bool              `yaml:"sqlite_header"`
	Outcome         projectionOutcome `yaml:"outcome"`
}

func (c openCodeProjectionCapCase) size() int64 {
	return int64(defaults.OpenCodeManagedProjectionMaxBytes) + c.OffsetFromBound
}

func (c openCodeProjectionCapCase) transcriptOrigin(t *testing.T) ingest.TranscriptOrigin {
	t.Helper()
	switch c.Origin {
	case "opencode-legacy-sqlite":
		return ingest.TranscriptOriginOpenCodeLegacySQLite
	case "opencode-current-sqlite":
		return ingest.TranscriptOriginOpenCodeCurrentSQLite
	default:
		t.Fatalf("cap fixture case %q has an unsupported origin %q", c.Name, c.Origin)
		return ingest.TranscriptOriginFile
	}
}

type openCodeProjectionCapDoc struct {
	RequiredCases []string                    `yaml:"required_cases"`
	Cases         []openCodeProjectionCapCase `yaml:"cases"`
}

func loadOpenCodeProjectionCapDoc(t *testing.T) []openCodeProjectionCapCase {
	t.Helper()
	var doc openCodeProjectionCapDoc
	if err := testutil.DecodeFixtureYAML(openCodeProjectionCapData, &doc); err != nil {
		t.Fatalf("decode projection cap fixture: %v", err)
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("projection cap fixture declares no required cases")
	}
	seen := make(map[string]struct{}, len(doc.Cases))
	readsPastTheBound, refuses := false, false
	for _, c := range doc.Cases {
		if c.Name == "" || c.Origin == "" {
			t.Fatalf("projection cap fixture has an incomplete case: %+v", c)
		}
		if _, dup := seen[c.Name]; dup {
			t.Fatalf("projection cap fixture has a duplicate case name %q", c.Name)
		}
		seen[c.Name] = struct{}{}
		switch c.Outcome {
		case projectionRead:
			if c.SQLiteHeader {
				t.Fatalf("case %q holds a database header but expects to be read; a database must never be read into memory", c.Name)
			}
			if c.OffsetFromBound > 0 {
				readsPastTheBound = true
			}
		case projectionRefusedAsDatabase:
			if !c.SQLiteHeader {
				t.Fatalf("case %q expects the provider-database refusal without holding a database header, so it would be refused for some other reason", c.Name)
			}
			refuses = true
		default:
			t.Fatalf("case %q states the unknown outcome %q; a reader either reads the file or refuses it as the provider's database", c.Name, c.Outcome)
		}
	}
	if !readsPastTheBound {
		t.Fatal("no case sizes a projection past the preview bound and requires it to be read; without one, a reinstated size gate would keep this corpus green while long sessions failed")
	}
	if !refuses {
		t.Fatal("no case presents the provider's database; without one, deleting the defence entirely would keep this corpus green")
	}
	for _, name := range doc.RequiredCases {
		if _, ok := seen[name]; !ok {
			t.Fatalf("projection cap fixture is missing required case %q", name)
		}
	}
	return doc.Cases
}

// sizedFileInfo reports a chosen size for one synthetic path, so this corpus
// can present a very large file without writing one.
type sizedFileInfo struct{ size int64 }

func (i sizedFileInfo) Name() string       { return "opencode-managed-projection.json" }
func (i sizedFileInfo) Size() int64        { return i.size }
func (i sizedFileInfo) Mode() os.FileMode  { return 0o600 }
func (i sizedFileInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (i sizedFileInfo) IsDir() bool        { return false }
func (i sizedFileInfo) Sys() any           { return nil }

// countingCapFileSystem presents one synthetic projection: a chosen size, a
// chosen first-bytes header, and a counter for every WHOLE read of it. Reads
// return a sentinel error, so no real projection bytes are needed; a case
// asserts on whether the whole file was read, not on decode.
type countingCapFileSystem struct {
	*ingest.OSFileSystem
	size   int64
	header string
	reads  int
}

var _ ingest.FileSystem = (*countingCapFileSystem)(nil)

var errCapReadAttempted = errors.New("synthetic projection read attempted")

func (fsys *countingCapFileSystem) Stat(path string) (os.FileInfo, error) {
	if path == capProjectionPath {
		return sizedFileInfo{size: fsys.size}, nil
	}
	return fsys.OSFileSystem.Stat(path)
}

func (fsys *countingCapFileSystem) ReadFile(path string) ([]byte, error) {
	if path == capProjectionPath {
		fsys.reads++
		return nil, errCapReadAttempted
	}
	return fsys.OSFileSystem.ReadFile(path)
}

// ReadFileHeader serves the synthetic first bytes WITHOUT counting a read: the
// point of the capability is that identifying the file never loads it.
func (fsys *countingCapFileSystem) ReadFileHeader(path string, limit int) ([]byte, error) {
	if path != capProjectionPath {
		return fsys.OSFileSystem.ReadFileHeader(path, limit)
	}
	header := fsys.header
	if len(header) > limit {
		header = header[:limit]
	}
	return []byte(header), nil
}

// TestOpenCodeProjectionReadersRefuseTheProviderDatabaseNotLargeSessions pins
// what decides whether a managed projection is read. A long session's
// projection is large and must be read, so no size may refuse it; the
// provider's own database must be refused, and identified from its first bytes
// so that it is never loaded. Both readers, the indexer and the capture, follow
// the one rule.
func TestOpenCodeProjectionReadersRefuseTheProviderDatabaseNotLargeSessions(t *testing.T) {
	t.Parallel()
	for _, c := range loadOpenCodeProjectionCapDoc(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			session := ingest.DiscoveredSession{
				SessionID:        ingest.SessionID("ses_3cd91f52effeXd3QAJ54jOyzv5"),
				Harness:          ingest.HarnessOpenCode,
				SourcePath:       ingest.ResolvedPath(capProjectionPath),
				TranscriptOrigin: c.transcriptOrigin(t),
			}
			for _, reader := range []struct {
				name string
				run  func(ingest.FileSystem) error
			}{
				{"index", func(filesystem ingest.FileSystem) error {
					_, err := ingest.NewOpenCodeIndexer(filesystem).IndexTranscript(context.Background(), session)
					return err
				}},
				{"capture", func(filesystem ingest.FileSystem) error {
					_, err := ingest.NewOpenCodeIndexer(filesystem).IndexTranscriptForCapture(context.Background(), session)
					return err
				}},
			} {
				t.Run(reader.name, func(t *testing.T) {
					header := ""
					if c.SQLiteHeader {
						header = openCodeSQLiteFileHeader
					}
					fsys := &countingCapFileSystem{OSFileSystem: &ingest.OSFileSystem{}, size: c.size(), header: header}
					err := reader.run(fsys)
					if err == nil {
						t.Fatal("the reader returned no error; either the sentinel read error or the provider-database refusal was expected")
					}
					if c.Outcome == projectionRead {
						if !errors.Is(err, errCapReadAttempted) {
							t.Fatalf("a projection of %d bytes was not read: %v; a long session's projection is legitimately large and refusing it loses the whole session", c.size(), err)
						}
						if fsys.reads != 1 {
							t.Fatalf("the projection was read %d times, want exactly 1", fsys.reads)
						}
						return
					}
					if errors.Is(err, errCapReadAttempted) {
						t.Fatal("the provider's database was read into memory; it must be refused from its first bytes alone")
					}
					if fsys.reads != 0 {
						t.Fatalf("the provider's database was read %d times; identifying it must never load it", fsys.reads)
					}
					for _, want := range []string{capProjectionPath, string(session.SessionID), "rerun harvest"} {
						if !strings.Contains(err.Error(), want) {
							t.Fatalf("the refusal must name %q so the user can act on it; got %q", want, err)
						}
					}
				})
			}
		})
	}
}

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
	var fixture currentMountedFixture
	if err := testutil.DecodeFixtureYAML(data, &fixture); err != nil {
		return fixture, fmt.Errorf("decode mounted current OpenCode fixture: %w", err)
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
			first, err := materializer.MaterializeTranscript(t.Context(), discovered[0])
			if err != nil {
				t.Fatal(err)
			}
			second, err := materializer.MaterializeTranscript(t.Context(), discovered[0])
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first.Data, second.Data) || !reflect.DeepEqual(first.Metadata, second.Metadata) {
				t.Fatalf("independent materializations diverged\nfirst=%s\nsecond=%s", first.Data, second.Data)
			}
			sessionID := mustMountedSessionID(t, testCase.SessionID)
			firstProjection := snapshotIndependentlyMaterializedCurrentProjection(t, first.Metadata, sessionID, first.Data)
			secondProjection := snapshotIndependentlyMaterializedCurrentProjection(t, second.Metadata, sessionID, second.Data)
			assertMountedCurrentSnapshotEqual(t, "independent materialization", firstProjection, secondProjection)
			adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessOpenCode: adapterFactory}
			store := &testutil.StubSessionStore{}
			metrics := testutil.NewStubMetricsStore()
			metrics.TitleHarness = ingest.HarnessOpenCode
			metrics.TitleProjectPath = "/synthetic/parity"
			commitReader := &managedProjectionCommitReader{}
			gitAnalyzer := &testutil.StubGitDiffAnalyzer{CommitInfos: []ingest.CommitInfo{{Hash: "0123456789abcdef0123456789abcdef01234567", AuthorEmail: testutil.TestEmail, Message: "synthetic commit"}}}
			config := ingest.PipelineConfig{Sources: map[ingest.Harness]ingest.SourceConfig{ingest.HarnessOpenCode: {Enabled: true, Paths: []ingest.ResolvedPath{root}}}, OutputDir: output, Parallelism: 1}
			fixtureStore := newPipelineFixtureStore(t, store, metrics)
			pipeline, err := newTestPipeline(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), adapters, config,
				ingest.WithStore(fixtureStore), ingest.WithMetricsStore(fixtureStore),
				ingest.WithAnalyzer(metricspkg.NewEngine(fixtureStore)),
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
			peerMeta := *metadata
			peerMeta.SessionID = nonStaleID
			peerBytes := bytes.ReplaceAll(managedBytes, []byte(sessionID), []byte(nonStaleID))
			peerEntries := storetest.SeedManagedInput(t, fixtureStore.Store, &ingest.OSFileSystem{}, output.String(), peerMeta, peerBytes)
			if _, err := metricspkg.NewEngine(fixtureStore).ComputeMetrics(t.Context(), []ingest.SessionID{nonStaleID}); err != nil {
				t.Fatal(err)
			}
			// Read the peer metric from the store that holds it. The seed
			// computes it through the metrics engine, which leaves current
			// metrics alone on a later compute, so the recorded copy is the
			// only place it is guaranteed to be.
			storedPeerMetric, err := fixtureStore.GetMetrics(t.Context(), nonStaleID)
			if err != nil || storedPeerMetric == nil {
				t.Fatalf("peer metrics were not computed for %s: %v", nonStaleID, err)
			}
			nonStaleMetric := *storedPeerMetric
			metrics.IndexedEntries[nonStaleID] = peerEntries
			metrics.SavedMetrics[nonStaleID] = &nonStaleMetric
			peerState, err := fixtureStore.ReadIndexState(t.Context(), nonStaleID)
			if err != nil {
				t.Fatal(err)
			}
			metrics.IndexStates[nonStaleID] = peerState.IndexerVersion
			canonicalSnapshot := captureMountedCurrentSnapshot(t, output, store, metrics, sessionID, metadata)

			if err := os.Remove(materialized.Path); err != nil {
				t.Fatal(err)
			}
			state, err := fixtureStore.ReadIndexState(t.Context(), sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if err := fixtureStore.UpdateIndexState(t.Context(), sessionID, state.IndexerVersion, *state.IndexedAt); err != nil {
				t.Fatal(err)
			}
			conn, err := fixtureStore.Pool().Take(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			err = sqlitex.ExecuteTransient(conn, "DELETE FROM session_metrics WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{string(sessionID)}})
			fixtureStore.Pool().Put(conn)
			if err != nil {
				t.Fatal(err)
			}
			metrics.IndexedEntries = make(map[ingest.SessionID][]schema.SessionEntry)
			metrics.SavedMetrics = make(map[ingest.SessionID]*ingest.SessionMetrics)
			metrics.IndexStates = make(map[ingest.SessionID]int)
			metrics.IndexedEntries[nonStaleID] = append([]schema.SessionEntry(nil), peerEntries...)
			metrics.SavedMetrics[nonStaleID] = &nonStaleMetric
			metrics.IndexStates[nonStaleID] = peerState.IndexerVersion
			config.Reindex = true
			config.Force = false
			sourceOpenMu.Lock()
			sourceOpenCount = 0
			sourceOpenMu.Unlock()
			reindexer := &mountedCurrentIndexerRecorder{TranscriptIndexer: ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeFullDepth(true), ingest.WithOpenCodeFullContent(true))}
			reindex, err := newTestPipeline(&ingest.OSFileSystem{}, testutil.NoGitResolver(), adapters, config, ingest.WithStore(fixtureStore), ingest.WithMetricsStore(fixtureStore), ingest.WithAnalyzer(metricspkg.NewEngine(fixtureStore)), ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{ingest.HarnessOpenCode: reindexer}))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reindex.Run(t.Context()); err != nil {
				t.Fatalf("source-missing reindex: %v", err)
			}
			reindexedSnapshot := captureMountedCurrentSnapshot(t, output, store, metrics, sessionID, metadata)
			assertMountedCurrentSnapshotEqual(t, "source-free stale reindex", canonicalSnapshot, reindexedSnapshot)
			reindexedState, err := fixtureStore.ReadIndexState(t.Context(), sessionID)
			if err != nil {
				t.Fatal(err)
			}
			sourceOpenMu.Lock()
			removedSourceOpens := sourceOpenCount
			sourceOpenMu.Unlock()
			reindexer.mu.Lock()
			reindexOrigins := append([]ingest.TranscriptOrigin(nil), reindexer.origins...)
			reindexByteRuns := reindexer.byteRuns
			reindexer.mu.Unlock()
			if reindexedState.IndexerVersion != ingest.HarvesterVersionRegistry[ingest.HarnessOpenCode].IndexerVersion || reindexedState.IndexedInputHash == nil || !reflect.DeepEqual(reindexedState.IndexedInputHash, state.IndexedInputHash) || reindexByteRuns != 1 || len(reindexOrigins) != 1 || reindexOrigins[0] != ingest.TranscriptOriginOpenCodeCurrentSQLite || removedSourceOpens != 0 || !reflect.DeepEqual(metrics.IndexedEntries[nonStaleID], peerEntries) || !reflect.DeepEqual(metrics.SavedMetrics[nonStaleID], &nonStaleMetric) || metrics.IndexStates[nonStaleID] != peerState.IndexerVersion {
				t.Fatalf("source-free stale reindex selected or mutated the wrong state: indexed_version=%d managed_current_runs=%d origins=%v removed_source_opens=%d non_stale_entries=%t non_stale_metrics=%t non_stale_index_version=%d", reindexedState.IndexerVersion, reindexByteRuns, reindexOrigins, removedSourceOpens, reflect.DeepEqual(metrics.IndexedEntries[nonStaleID], peerEntries), reflect.DeepEqual(metrics.SavedMetrics[nonStaleID], &nonStaleMetric), metrics.IndexStates[nonStaleID])
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
			fixtureStore := newPipelineFixtureStore(t, store, metrics)
			pipeline, err := newTestPipeline(&ingest.OSFileSystem{}, testutil.NoGitResolver(), map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessOpenCode: adapterFactory}, config, ingest.WithStore(fixtureStore), ingest.WithMetricsStore(fixtureStore), ingest.WithAnalyzer(metricspkg.NewEngine(fixtureStore)), ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{ingest.HarnessOpenCode: ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeFullDepth(true), ingest.WithOpenCodeFullContent(true))}))
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
			assertCurrentFailureHasOnlyCoordination(t, output.String(), base.SessionID)
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
			expectedFixtureStore := newPipelineFixtureStore(t, expectedStore, expectedMetrics)
			expectedPipeline, err := newTestPipeline(&ingest.OSFileSystem{}, testutil.NoGitResolver(), map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessOpenCode: adapterFactory}, expectedConfig, ingest.WithStore(expectedFixtureStore), ingest.WithMetricsStore(expectedFixtureStore), ingest.WithAnalyzer(metricspkg.NewEngine(expectedFixtureStore)), ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{ingest.HarnessOpenCode: ingest.NewOpenCodeIndexer(&ingest.OSFileSystem{}, ingest.WithOpenCodeFullDepth(true), ingest.WithOpenCodeFullContent(true))}))
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

// A failed native read may leave its empty coordination lock, but no session
// payload, publication intent, or other session's state is allowed.
func assertCurrentFailureHasOnlyCoordination(t testing.TB, output, sessionID string) {
	t.Helper()
	lockDirectory := filepath.Join(".peasant-state", "locks")
	lockPath := filepath.Join(lockDirectory, schema.ComputeTranscriptHash([]byte(sessionID))+".lock")
	err := filepath.WalkDir(output, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(output, path)
		if err != nil {
			return err
		}
		switch relative {
		case ".", ".peasant-state", lockDirectory:
			if !entry.IsDir() {
				return fmt.Errorf("coordination directory %q is not a directory", relative)
			}
		case lockPath:
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || info.Size() != 0 {
				return fmt.Errorf("coordination lock %q is not an empty regular file", relative)
			}
		default:
			return fmt.Errorf("mounted failure left temporary or final artifact %q", relative)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
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

//go:embed testdata/opencode_double_encoded_text.yaml
var openCodeDoubleEncodedTextData []byte

// openCodeDoubleEncodedTextCase is one stored text value and the text the
// indexer must produce for it.
type openCodeDoubleEncodedTextCase struct {
	Name   string `yaml:"name"`
	Stored string `yaml:"stored"`
	Unwrap bool   `yaml:"unwrap"`
	Want   string `yaml:"want"`
}

type openCodeDoubleEncodedTextDoc struct {
	RequiredCases []string                        `yaml:"required_cases"`
	Cases         []openCodeDoubleEncodedTextCase `yaml:"cases"`
}

func loadOpenCodeDoubleEncodedTextDoc(t *testing.T) openCodeDoubleEncodedTextDoc {
	t.Helper()
	var doc openCodeDoubleEncodedTextDoc
	if err := testutil.DecodeFixtureYAML(openCodeDoubleEncodedTextData, &doc); err != nil {
		t.Fatalf("decode double-encoded text fixture: %v", err)
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("double-encoded text fixture declares no required cases")
	}
	present := make(map[string]struct{}, len(doc.Cases))
	for _, testCase := range doc.Cases {
		if testCase.Name == "" || testCase.Stored == "" || testCase.Want == "" {
			t.Fatalf("double-encoded text fixture has an incomplete case: %+v", testCase)
		}
		if testCase.Unwrap == (testCase.Stored == testCase.Want) {
			t.Fatalf("double-encoded text case %q declares unwrap=%v but its stored and wanted text %s; the case would pass whatever the code does",
				testCase.Name, testCase.Unwrap, map[bool]string{true: "are equal", false: "differ"}[testCase.Stored == testCase.Want])
		}
		if _, duplicate := present[testCase.Name]; duplicate {
			t.Fatalf("double-encoded text fixture has a duplicate case name %q", testCase.Name)
		}
		present[testCase.Name] = struct{}{}
	}
	for _, name := range doc.RequiredCases {
		if _, ok := present[name]; !ok {
			t.Fatalf("double-encoded text fixture is missing required case %q", name)
		}
	}
	return doc
}

// managedProjectionWithPartText builds the managed legacy projection bytes for
// one user message carrying one text part with the given text. It is the same
// artifact the materializer writes, so the assertion runs over the production
// indexing path rather than over the helper alone.
func managedProjectionWithPartText(t *testing.T, sessionID, text string) []byte {
	t.Helper()
	partData, err := json.Marshal(map[string]any{"id": "prt_double_encoded", "type": "text", "text": text})
	if err != nil {
		t.Fatalf("encode synthetic part: %v", err)
	}
	messageData, err := json.Marshal(map[string]any{"id": "msg_double_encoded", "role": "user", "time": map[string]any{"created": 1}})
	if err != nil {
		t.Fatalf("encode synthetic message: %v", err)
	}
	projection, err := json.Marshal(map[string]any{
		"format":     "peasant.opencode.legacy-sqlite",
		"version":    2,
		"session_id": sessionID,
		"messages": []any{map[string]any{
			"id": "msg_double_encoded", "session_id": sessionID, "time_created": 1, "time_updated": 1,
			"data": json.RawMessage(messageData),
			"parts": []any{map[string]any{
				"id": "prt_double_encoded", "message_id": "msg_double_encoded", "session_id": sessionID,
				"time_created": 1, "time_updated": 1, "data": json.RawMessage(partData),
			}},
		}},
	})
	if err != nil {
		t.Fatalf("encode synthetic projection: %v", err)
	}
	return projection
}

// TestOpenCodeIndexer_UnwrapsDoubleEncodedPromptText proves the indexer decodes
// a text value that is itself one JSON string literal, and leaves every other
// value alone. Indexing is where the unwrap belongs, so the preview, the stored
// transcript, and a push all carry the same text.
func TestOpenCodeIndexer_UnwrapsDoubleEncodedPromptText(t *testing.T) {
	t.Parallel()
	doc := loadOpenCodeDoubleEncodedTextDoc(t)
	for _, testCase := range doc.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			sessionID, err := ingest.NewSessionID("ses_3cd91f52effeXd3QAJ54jOyzv5")
			if err != nil {
				t.Fatalf("build session identifier: %v", err)
			}
			session := ingest.DiscoveredSession{
				SessionID:        sessionID,
				Harness:          ingest.HarnessOpenCode,
				SourcePath:       ingest.ResolvedPath("/synthetic/opencode.db"),
				SourceFormat:     ingest.SourceFormatJSON,
				TranscriptOrigin: ingest.TranscriptOriginOpenCodeLegacySQLite,
			}
			indexer, ok := ingest.NewIndexerRegistry(&ingest.OSFileSystem{}, ingest.IndexerRegistryOptions{FullContent: true})[ingest.HarnessOpenCode]
			if !ok {
				t.Fatal("the registry holds no OpenCode indexer")
			}
			data := managedProjectionWithPartText(t, string(sessionID), testCase.Stored)
			entries, err := indexer.IndexTranscriptBytes(t.Context(), session, data)
			if err != nil {
				t.Fatalf("index the synthetic managed projection: %v", err)
			}
			preview := firstIndexedContent(t, entries)
			if preview != testCase.Want {
				t.Errorf("indexed content = %q, want %q (stored %q)", preview, testCase.Want, testCase.Stored)
			}
		})
	}
}

func firstIndexedContent(t *testing.T, entries []schema.SessionEntry) string {
	t.Helper()
	for _, entry := range entries {
		if entry.ContentPreview != nil && *entry.ContentPreview != "" {
			return *entry.ContentPreview
		}
	}
	t.Fatal("the indexed projection carried no content")
	return ""
}

//go:embed testdata/opencode_tool_turn_rendering.yaml
var openCodeToolTurnRenderingData []byte

// openCodeToolCallExpectation is one folded tool call as a reader sees it.
type openCodeToolCallExpectation struct {
	Name   string `yaml:"name"`
	Input  string `yaml:"input"`
	Output string `yaml:"output"`
}

// openCodeTurnExpectation is one rendered turn of the folded message.
type openCodeTurnExpectation struct {
	Role      string                        `yaml:"role"`
	EntryType string                        `yaml:"entry_type"`
	Content   string                        `yaml:"content"`
	Tools     []openCodeToolCallExpectation `yaml:"tools"`
}

type openCodeToolTurnRenderingCase struct {
	Name                 string                    `yaml:"name"`
	Role                 string                    `yaml:"role"`
	Parts                []string                  `yaml:"parts"`
	ContentRenderedOnce  string                    `yaml:"content_rendered_once"`
	WantMessageEntryType string                    `yaml:"want_message_entry_type"`
	WantMessageRole      string                    `yaml:"want_message_role"`
	WantTurns            []openCodeTurnExpectation `yaml:"want_turns"`
}

type openCodeToolTurnRenderingDoc struct {
	RequiredCases []string                        `yaml:"required_cases"`
	Cases         []openCodeToolTurnRenderingCase `yaml:"cases"`
}

func loadOpenCodeToolTurnRenderingDoc(t *testing.T) openCodeToolTurnRenderingDoc {
	t.Helper()
	var doc openCodeToolTurnRenderingDoc
	if err := testutil.DecodeFixtureYAML(openCodeToolTurnRenderingData, &doc); err != nil {
		t.Fatalf("decode tool-turn rendering fixture: %v", err)
	}
	if len(doc.RequiredCases) == 0 {
		t.Fatal("tool-turn rendering fixture declares no required cases")
	}
	present := make(map[string]struct{}, len(doc.Cases))
	for _, testCase := range doc.Cases {
		if testCase.Name == "" || testCase.Role == "" || testCase.WantMessageEntryType == "" || testCase.WantMessageRole == "" || len(testCase.Parts) == 0 || len(testCase.WantTurns) == 0 {
			t.Fatalf("tool-turn rendering fixture has an incomplete case: %+v", testCase)
		}
		for _, part := range testCase.Parts {
			if !json.Valid([]byte(part)) {
				t.Fatalf("tool-turn rendering case %q holds a part that is not valid JSON: %s", testCase.Name, part)
			}
		}
		if _, duplicate := present[testCase.Name]; duplicate {
			t.Fatalf("tool-turn rendering fixture has a duplicate case name %q", testCase.Name)
		}
		present[testCase.Name] = struct{}{}
	}
	for _, name := range doc.RequiredCases {
		if _, ok := present[name]; !ok {
			t.Fatalf("tool-turn rendering fixture is missing required case %q", name)
		}
	}
	return doc
}

// managedProjectionWithParts builds the managed legacy projection bytes for one
// message carrying the given stored part rows. It is the artifact the
// materializer writes, so the assertion runs the production indexing and fold
// path rather than a helper's shortcut.
func managedProjectionWithParts(t *testing.T, sessionID, role string, parts []string) []byte {
	t.Helper()
	messageData, err := json.Marshal(map[string]any{"id": "msg_render", "role": role, "time": map[string]any{"created": 1}})
	if err != nil {
		t.Fatalf("encode synthetic message: %v", err)
	}
	rows := make([]any, 0, len(parts))
	for index, part := range parts {
		var identity struct {
			ID string `json:"id"`
		}
		if unmarshalErr := json.Unmarshal([]byte(part), &identity); unmarshalErr != nil {
			t.Fatalf("read the identity of synthetic part %d: %v", index, unmarshalErr)
		}
		rows = append(rows, map[string]any{
			"id": identity.ID, "message_id": "msg_render", "session_id": sessionID,
			"time_created": int64(index + 1), "time_updated": int64(index + 1),
			"data": json.RawMessage(part),
		})
	}
	projection, err := json.Marshal(map[string]any{
		"format":     "peasant.opencode.legacy-sqlite",
		"version":    2,
		"session_id": sessionID,
		"messages": []any{map[string]any{
			"id": "msg_render", "session_id": sessionID, "time_created": 1, "time_updated": 1,
			"data":  json.RawMessage(messageData),
			"parts": rows,
		}},
	})
	if err != nil {
		t.Fatalf("encode synthetic projection: %v", err)
	}
	return projection
}

// TestOpenCodeToolTurnRendering proves that one OpenCode message folds to the
// turns a reader sees: a tool turn names its tool and carries the tool's own
// output, and a message's prose renders exactly once.
//
// Mutation proof, one guard per defect. Dropping the "tool" fallback in
// openCodeSemanticToolName makes tool-name-comes-from-the-tool-field fail with
// an empty name. Dropping State.Output from the output precedence makes
// tool-output-comes-from-state-output and output-wins-over-result-error-and-content
// fail with an empty or aliased output. Restoring the RoleUser condition on the
// duplicate text-part drop makes a-trailing-assistant-text-part-renders-once
// fail with the report on two turns. Counting "reasoning" as a tool part in
// inspectOpenCodeSemanticParts makes a-reasoning-part-does-not-make-a-tool-turn
// fail with a tool_use turn. Dropping the Synthetic read from
// inspectOpenCodeSemanticParts makes the synthetic-task-result cases fail with
// the injected result still standing as a user turn.
func TestOpenCodeToolTurnRendering(t *testing.T) {
	t.Parallel()
	doc := loadOpenCodeToolTurnRenderingDoc(t)
	for _, testCase := range doc.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			sessionID, err := ingest.NewSessionID("ses_3cd91f52effeXd3QAJ54jOyzv5")
			if err != nil {
				t.Fatalf("build session identifier: %v", err)
			}
			session := ingest.DiscoveredSession{
				SessionID:        sessionID,
				Harness:          ingest.HarnessOpenCode,
				SourcePath:       ingest.ResolvedPath("/synthetic/opencode.db"),
				SourceFormat:     ingest.SourceFormatJSON,
				TranscriptOrigin: ingest.TranscriptOriginOpenCodeLegacySQLite,
			}
			indexer, ok := ingest.NewIndexerRegistry(&ingest.OSFileSystem{}, ingest.IndexerRegistryOptions{FullContent: true})[ingest.HarnessOpenCode]
			if !ok {
				t.Fatal("the registry holds no OpenCode indexer")
			}
			data := managedProjectionWithParts(t, string(sessionID), testCase.Role, testCase.Parts)
			entries, err := indexer.IndexTranscriptBytes(t.Context(), session, data)
			if err != nil {
				t.Fatalf("index the synthetic managed projection: %v", err)
			}
			if len(entries) == 0 {
				t.Fatal("the indexed projection carried no entries")
			}
			if string(entries[0].EntryType) != testCase.WantMessageEntryType {
				t.Errorf("message entry type = %q, want %q", entries[0].EntryType, testCase.WantMessageEntryType)
			}
			if string(entries[0].Role) != testCase.WantMessageRole {
				t.Errorf("message role = %q, want %q", entries[0].Role, testCase.WantMessageRole)
			}
			turns := transcript.EntriesToTurns(entries)
			if len(turns) != len(testCase.WantTurns) {
				for index, turn := range turns {
					t.Logf("turn[%d] role=%s type=%s tools=%d content=%q", index, turn.Role, turn.EntryType, len(turn.ToolCalls), turn.Content)
				}
				t.Fatalf("turn count = %d, want %d", len(turns), len(testCase.WantTurns))
			}
			for index, want := range testCase.WantTurns {
				got := turns[index]
				if string(got.Role) != want.Role {
					t.Errorf("turn[%d] role = %q, want %q", index, got.Role, want.Role)
				}
				if string(got.EntryType) != want.EntryType {
					t.Errorf("turn[%d] entry type = %q, want %q", index, got.EntryType, want.EntryType)
				}
				if got.Content != want.Content {
					t.Errorf("turn[%d] content = %q, want %q", index, got.Content, want.Content)
				}
				if len(got.ToolCalls) != len(want.Tools) {
					t.Fatalf("turn[%d] tool call count = %d, want %d", index, len(got.ToolCalls), len(want.Tools))
				}
				for callIndex, wantCall := range want.Tools {
					gotCall := got.ToolCalls[callIndex]
					if gotCall.Name != wantCall.Name {
						t.Errorf("turn[%d] tool[%d] name = %q, want %q", index, callIndex, gotCall.Name, wantCall.Name)
					}
					if gotCall.Arguments != wantCall.Input {
						t.Errorf("turn[%d] tool[%d] input = %q, want %q", index, callIndex, gotCall.Arguments, wantCall.Input)
					}
					if gotCall.Result != wantCall.Output {
						t.Errorf("turn[%d] tool[%d] output = %q, want %q", index, callIndex, gotCall.Result, wantCall.Output)
					}
				}
			}
			if testCase.ContentRenderedOnce != "" {
				rendered := 0
				for _, turn := range turns {
					if strings.Contains(turn.Content, testCase.ContentRenderedOnce) {
						rendered++
					}
				}
				if rendered != 1 {
					t.Errorf("content %q renders on %d turns, want exactly 1", testCase.ContentRenderedOnce, rendered)
				}
			}
		})
	}
}
