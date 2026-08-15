package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

const expectedOpenCodeBoundaryCases = 1

type openCodeBoundaryCase struct {
	Name                          string `yaml:"name"`
	SourceFixture                 string `yaml:"source_fixture"`
	ExpectedSessions              int    `yaml:"expected_sessions"`
	ExpectedDiscoveryMessageReads int64  `yaml:"expected_discovery_message_reads"`
	ExpectedDiscoveryPartReads    int64  `yaml:"expected_discovery_part_reads"`
	CommitHash                    string `yaml:"commit_hash"`
	CommitEmail                   string `yaml:"commit_email"`
	CommitMessage                 string `yaml:"commit_message"`
	ExpectedCommitAssociations    int    `yaml:"expected_commit_associations"`
}

type openCodeBoundaryDocument struct {
	DeclaredCases int                    `yaml:"declared_cases"`
	Cases         []openCodeBoundaryCase `yaml:"cases"`
}

//go:embed testdata/opencode_review_boundaries.yaml
var openCodeBoundaryYAML []byte

func loadOpenCodeBoundaryDocument(data []byte) (openCodeBoundaryDocument, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var document openCodeBoundaryDocument
	if err := decoder.Decode(&document); err != nil {
		return document, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return document, errors.New("expected exactly one YAML document")
	}
	if document.DeclaredCases != expectedOpenCodeBoundaryCases || len(document.Cases) != expectedOpenCodeBoundaryCases {
		return document, errors.New("boundary fixture count guard failed")
	}
	seen := make(map[string]struct{}, len(document.Cases))
	for _, testCase := range document.Cases {
		if testCase.Name == "" || testCase.SourceFixture == "" || testCase.CommitHash == "" || testCase.CommitEmail == "" {
			return document, errors.New("boundary fixture has an incomplete required row")
		}
		if _, duplicate := seen[testCase.Name]; duplicate {
			return document, errors.New("boundary fixture has a duplicate case name")
		}
		seen[testCase.Name] = struct{}{}
	}
	return document, nil
}

type legacyPayloadReadCounter struct {
	messages atomic.Int64
	parts    atomic.Int64
}

type countingLegacySource struct {
	ingest.OpenCodeSQLiteSource
	counter *legacyPayloadReadCounter
}

func (source countingLegacySource) LegacyMessages(ctx context.Context, request ingest.OpenCodeLegacyMessagePageRequest) (ingest.OpenCodeLegacyMessagePage, error) {
	source.counter.messages.Add(1)
	return source.OpenCodeSQLiteSource.LegacyMessages(ctx, request)
}

func (source countingLegacySource) LegacyParts(ctx context.Context, request ingest.OpenCodeLegacyPartPageRequest) (ingest.OpenCodeLegacyPartPage, error) {
	source.counter.parts.Add(1)
	return source.OpenCodeSQLiteSource.LegacyParts(ctx, request)
}

type fixedOpenCodeEnvironment map[string]string

func (environment fixedOpenCodeEnvironment) LookupEnv(key string) (string, bool) {
	value, ok := environment[key]
	return value, ok
}

type rejectingCommitTranscriptReader struct{ calls atomic.Int64 }

func (reader *rejectingCommitTranscriptReader) ReadTranscript(context.Context, string) ([]byte, error) {
	reader.calls.Add(1)
	return nil, context.Canceled
}

func TestOpenCodeDiscoveryAndFilteredPipelineDoNotReadLegacyPayloads(t *testing.T) {
	document, err := loadOpenCodeBoundaryDocument(openCodeBoundaryYAML)
	if err != nil {
		t.Fatalf("load OpenCode boundary expectations: %v", err)
	}
	testCase := document.Cases[0]
	materialized := testfixture.MaterializeByName(t, testCase.SourceFixture)
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic SQLite-only root: %v", err)
	}
	counter := &legacyPayloadReadCounter{}
	opener := func(ctx context.Context, path ingest.OpenCodeSQLiteSourcePath, options ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
		source, openErr := ingest.OpenOpenCodeSQLiteSource(ctx, path, options)
		if openErr != nil {
			return nil, openErr
		}
		return countingLegacySource{OpenCodeSQLiteSource: source, counter: counter}, nil
	}
	newAdapter := func() *ingest.OpenCodeAdapter {
		adapter, adapterErr := ingest.NewOpenCodeAdapterWithCandidateProbe(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{}, "latest", fixedOpenCodeEnvironment{}, &ingest.OSFileSystem{}, opener, ingest.DefaultOpenCodeSQLiteSourceOptions())
		if adapterErr != nil {
			t.Fatalf("construct counted production adapter: %v", adapterErr)
		}
		return adapter
	}

	discovered, err := newAdapter().Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil || len(discovered) != testCase.ExpectedSessions {
		t.Fatalf("counted discovery sessions=%d error=%v, want %d typed IDs", len(discovered), err, testCase.ExpectedSessions)
	}
	assertLegacyPayloadReads(t, counter, "discovery", testCase.ExpectedDiscoveryMessageReads, testCase.ExpectedDiscoveryPartReads)

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessOpenCode: func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter { return newAdapter() },
	}
	config := ingest.PipelineConfig{Sources: map[ingest.Harness]ingest.SourceConfig{ingest.HarnessOpenCode: {Enabled: true, Paths: []ingest.ResolvedPath{root}}}, OutputDir: ingest.ResolvedPath(t.TempDir()), Parallelism: 1, SessionFilter: func(ingest.DiscoveredSession) bool { return false }}
	pipeline, err := ingest.NewPipeline(&ingest.OSFileSystem{}, testutil.NoGitResolver(), adapters, config)
	if err != nil {
		t.Fatalf("construct unselected-session pipeline: %v", err)
	}
	if _, err := pipeline.Run(t.Context()); err != nil {
		t.Fatalf("run unselected-session pipeline: %v", err)
	}
	assertLegacyPayloadReads(t, counter, "unselected pipeline", testCase.ExpectedDiscoveryMessageReads, testCase.ExpectedDiscoveryPartReads)

	ingested := time.Now().Add(time.Hour).UnixMilli()
	locations := make(map[ingest.SessionID]ingest.SessionLocation, len(discovered))
	for _, session := range discovered {
		locations[session.SessionID] = ingest.SessionLocation{HostSlug: "synthetic", IngestedMs: &ingested, SchemaVersion: int(ingest.CurrentSchemaVersion)}
	}
	store := &testutil.StubSessionStore{LocationsByID: locations}
	config.SessionFilter = nil
	config.OutputDir = ingest.ResolvedPath(t.TempDir())
	unchangedPipeline, err := ingest.NewPipeline(&ingest.OSFileSystem{}, testutil.NoGitResolver(), adapters, config, ingest.WithStore(store))
	if err != nil {
		t.Fatalf("construct unchanged-session pipeline: %v", err)
	}
	if _, err := unchangedPipeline.Run(t.Context()); err != nil {
		t.Fatalf("run unchanged-session pipeline: %v", err)
	}
	assertLegacyPayloadReads(t, counter, "unchanged pipeline", testCase.ExpectedDiscoveryMessageReads, testCase.ExpectedDiscoveryPartReads)

	commitReader := &rejectingCommitTranscriptReader{}
	commit := ingest.CommitInfo{Hash: testCase.CommitHash, AuthorEmail: testCase.CommitEmail, Message: testCase.CommitMessage}
	commitAnalyzer := &testutil.StubGitDiffAnalyzer{CommitInfos: []ingest.CommitInfo{commit}}
	commitStore := &testutil.StubSessionStore{}
	config.OutputDir = ingest.ResolvedPath(t.TempDir())
	selectedPipeline, err := ingest.NewPipeline(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), adapters, config,
		ingest.WithGitDiffAnalyzer(commitAnalyzer),
		ingest.WithCommitTranscriptReader(commitReader),
		ingest.WithStore(commitStore),
	)
	if err != nil {
		t.Fatalf("construct selected commit-detection pipeline: %v", err)
	}
	if _, err := selectedPipeline.Run(t.Context()); err != nil {
		t.Fatalf("run selected commit-detection pipeline: %v", err)
	}
	if calls := commitReader.calls.Load(); calls != 0 {
		t.Fatalf("typed SQLite commit detection sent %d DB/WAL/SHM paths to transcript reading; want timestamp-only fallback", calls)
	}
	if len(commitStore.UpsertedCommits) != testCase.ExpectedCommitAssociations {
		t.Fatalf("timestamp-only commit detection persisted %d session associations, want %d", len(commitStore.UpsertedCommits), testCase.ExpectedCommitAssociations)
	}
	for sessionID, commits := range commitStore.UpsertedCommits {
		if len(commits) != 1 || commits[0].Hash != commit.Hash {
			t.Errorf("timestamp-only commit evidence for %s = %+v, want candidate %s", sessionID, commits, commit.Hash)
		}
	}
}

func TestOpenCodeBoundaryFixtureRejectsUnknownFields(t *testing.T) {
	mutated := bytes.Replace(openCodeBoundaryYAML, []byte("source_fixture:"), []byte("unknown_source_fixture:"), 1)
	if _, err := loadOpenCodeBoundaryDocument(mutated); err == nil {
		t.Fatal("OpenCode boundary fixture loader accepted an unknown field mutation")
	}
}

func assertLegacyPayloadReads(t testing.TB, counter *legacyPayloadReadCounter, stage string, expectedMessages, expectedParts int64) {
	t.Helper()
	if messages, parts := counter.messages.Load(), counter.parts.Load(); messages != expectedMessages || parts != expectedParts {
		t.Fatalf("%s legacy payload reads: messages=%d parts=%d, want %d/%d", stage, messages, parts, expectedMessages, expectedParts)
	}
}
