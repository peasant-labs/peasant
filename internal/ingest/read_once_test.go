package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/read_once.yaml
var readOnceYAML []byte

type readOnceFixture struct {
	RequiredCaseNames []string `yaml:"required_case_names"`
	Cases             []struct {
		Name                  string `yaml:"name"`
		Transcript            string `yaml:"transcript"`
		WantSourceReads       int    `yaml:"want_source_reads"`
		WantCommitReaderCalls int64  `yaml:"want_commit_reader_calls"`
		WantCommits           int    `yaml:"want_commits"`
	} `yaml:"cases"`
}

func loadReadOnceFixture(t *testing.T) readOnceFixture {
	t.Helper()
	var fixture readOnceFixture
	decoder := yaml.NewDecoder(bytes.NewReader(readOnceYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool)
	for _, tc := range fixture.Cases {
		if present[tc.Name] {
			t.Fatalf("duplicate read-once case %q", tc.Name)
		}
		present[tc.Name] = true
	}
	if err := testutil.RequireFixtureNames("read once", "case", fixture.RequiredCaseNames, present); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// countingCommitTranscriptReader records every disk read command validation
// asks for. It returns no bytes, so a read it served would confirm nothing.
type countingCommitTranscriptReader struct{ calls atomic.Int64 }

var _ ingest.CommitTranscriptReader = (*countingCommitTranscriptReader)(nil)

func (reader *countingCommitTranscriptReader) ReadTranscript(context.Context, string) ([]byte, error) {
	reader.calls.Add(1)
	return []byte{}, nil
}

func TestPipeline_ReadOnce(t *testing.T) {
	for _, tc := range loadReadOnceFixture(t).Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			fs := testutil.NewCountingFS(testutil.NewMemFS())
			source := filepath.Join(testSourceDir, testSessionID+".jsonl")
			if err := fs.WriteFile(source, []byte(tc.Transcript+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			session := makeDiscoveredSession(t, testSessionID, source, time.Date(2024, 2, 19, 0, 0, 0, 0, time.UTC))
			session.TranscriptOrigin = ingest.TranscriptOriginFile
			meta := makeMinimalMeta(t, testSessionID)
			meta.Source.FilePath = source
			adapters := map[ingest.Harness]ingest.AdapterFactory{
				ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta}),
			}
			output := "/output"
			commitReader := &countingCommitTranscriptReader{}
			analyzer := &testutil.StubGitDiffAnalyzer{CommitInfos: []ingest.CommitInfo{{Hash: "abc123", AuthorEmail: testutil.TestEmail, Message: "work"}}}
			pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), adapters, makePipelineConfig(output),
				ingest.WithGitDiffAnalyzer(analyzer), ingest.WithCommitTranscriptReader(commitReader))
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(context.Background())
			if err != nil || result.Summary.Errors != 0 {
				t.Fatalf("ingestion failed: %v, result: %+v", err, result)
			}
			if got := fs.ReadCount(source); got != tc.WantSourceReads {
				t.Errorf("source reads = %d, want %d", got, tc.WantSourceReads)
			}
			if got := commitReader.calls.Load(); got != tc.WantCommitReaderCalls {
				t.Errorf("commit detection read the transcript from disk %d times, want %d: it must parse the captured bytes", got, tc.WantCommitReaderCalls)
			}
			data, err := fs.MemFS.ReadFile(filepath.Join(expectedOutputBase(output, testSessionID), testSessionID+"--metadata.json"))
			if err != nil {
				t.Fatal(err)
			}
			var written ingest.UnifiedMetadata
			if err := json.Unmarshal(data, &written); err != nil {
				t.Fatal(err)
			}
			if got := len(written.Git.Commits); got != tc.WantCommits {
				t.Errorf("confirmed commits = %d (%+v), want %d", got, written.Git.Commits, tc.WantCommits)
			}
		})
	}
}
