package ingest

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_batch.yaml
var contentBatchFixtureData []byte

type captureBatchObserver struct {
	MetricsStore
	batches []int
	cancel  context.CancelFunc
}

var _ SessionEntryBatchStore = (*captureBatchObserver)(nil)

func (s *captureBatchObserver) IndexSessionEntryBatch(_ context.Context, writes []SessionEntryWrite) []SessionEntryWriteResult {
	s.batches = append(s.batches, len(writes))
	results := make([]SessionEntryWriteResult, len(writes))
	for i, write := range writes {
		results[i] = SessionEntryWriteResult{SessionID: write.SessionID, Written: true}
	}
	if s.cancel != nil {
		s.cancel()
	}
	return results
}

// TestFullContentWriteBatchCommitsEachSession drives the write flush over input
// captured at the production boundary.
//
// The cases used to hand the flush a hand-built parse result whose captured input
// was nil. Production never produces that: a parse runs on input captured from the
// committed artifact, and the write carries that input's hash as its proof of what
// was parsed. The flush dereferenced it and the test died in a nil panic, which is
// a fixture arranging a state the product cannot be in.
//
// Each case now writes a managed artifact pair, captures it through
// CaptureIndexInput, and parses THAT. What the flush then owes is one atomic
// commit per session while that session's artifact is still the one it parsed, an
// outcome recorded for every session, and a stop once the run is cancelled.
func TestFullContentWriteBatchCommitsEachSession(t *testing.T) {
	var fixtures struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name    string `yaml:"name"`
			Bytes   []int  `yaml:"session_bytes"`
			Commits []int  `yaml:"expected_commits"`
			Cancel  bool   `yaml:"cancel_after_commit"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(contentBatchFixtureData, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if names[fixture.Name] {
			t.Fatalf("duplicate fixture %s", fixture.Name)
		}
		names[fixture.Name] = true
		t.Run(fixture.Name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			observer := &captureBatchObserver{}
			if fixture.Cancel {
				observer.cancel = cancel
			}
			output := t.TempDir()
			pipeline := &Pipeline{metricsStore: observer, fs: &OSFileSystem{}, config: PipelineConfig{OutputDir: ResolvedPath(output)}}
			var parsed []indexParseResult
			for index, size := range fixture.Bytes {
				parsed = append(parsed, captureSyntheticIndexInput(t, ctx, pipeline, output, index, size))
			}
			flush := pipeline.flushIndexParseResults(ctx, parsed, IndexOutcomeIndexed, "capture test", nil)
			if len(observer.batches) != len(fixture.Commits) {
				t.Fatalf("commits=%v want=%v", observer.batches, fixture.Commits)
			}
			for position, got := range observer.batches {
				if got != fixture.Commits[position] {
					t.Fatalf("commit %d carried %d sessions, want %d (commits=%v)", position, got, fixture.Commits[position], observer.batches)
				}
			}
			if len(flush.logEntries) != len(parsed) {
				t.Fatal("session outcome lost")
			}
		})
	}
	for _, name := range fixtures.Required {
		if !names[name] {
			t.Fatalf("missing required fixture %s", name)
		}
	}
}

// captureSyntheticIndexInput publishes one synthetic managed artifact of about
// the requested size and captures its parser input the way the pipeline does, so
// the flush under test receives a result production could have produced.
func captureSyntheticIndexInput(t *testing.T, ctx context.Context, pipeline *Pipeline, output string, index, size int) indexParseResult {
	t.Helper()
	sessionID, err := NewSessionID(fmt.Sprintf("11111111-1111-4111-8111-00000000000%d", index))
	if err != nil {
		t.Fatal(err)
	}
	const hostSlug = "github.com--peasant-labs--content-batch"
	meta := NewUnifiedMetadata()
	meta.SessionID = sessionID
	meta.ModelHarness = HarnessClaudeCode
	meta.Model = "claude-opus-4-8"
	meta.HostSlug = hostSlug
	meta.Timestamp = TimestampInfo{Start: 1700000000000, End: 1700000001000}
	meta.Project = ProjectInfo{Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "content-batch", FilePath: "/synthetic/content-batch"}
	transcriptPath := filepath.Join(SessionDir(output, hostSlug, string(sessionID), ""), string(sessionID)+"--transcript."+string(SourceFormatJSONL))
	meta.Source = SourceInfo{Format: SourceFormatJSONL, FilePath: transcriptPath}
	transcript := syntheticClaudeTranscript(string(sessionID), size)
	meta.ContentHash = schema.ComputeTranscriptHash(transcript)
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	metadataPath := SessionMetadataPath(output, hostSlug, string(sessionID), "")
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, metaJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath, transcript, 0o600); err != nil {
		t.Fatal(err)
	}
	// A published artifact has its coordination lock beside it; a read-only
	// capture takes that lock and never creates one. This arrangement stands in
	// for a completed publish, so it creates the same file the publish would.
	lockPath := filepath.Join(output, artifactLockPath(artifactKey(sessionID)))
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	artifact, err := NewManagedArtifact(metaJSON, transcript)
	if err != nil {
		t.Fatalf("publish a synthetic managed artifact: %v", err)
	}
	indexer := NewClaudeIndexer(pipeline.fs, WithClaudeFullContent(true))
	session := DiscoveredSession{SessionID: sessionID, Harness: HarnessClaudeCode, SourceFormat: SourceFormatJSONL, SourcePath: ResolvedPath(transcriptPath)}
	input, err := CaptureIndexInput(ctx, indexer, session, artifact)
	if err != nil {
		t.Fatalf("capture the parser's input from the committed artifact: %v", err)
	}
	input.metadataPath = metadataPath
	result, err := input.Parse(ctx, indexer)
	if err != nil {
		t.Fatalf("parse the captured input: %v", err)
	}
	return indexParseResult{
		im:          indexedMeta{session: session, outputTranscriptPath: transcriptPath, startMs: meta.Timestamp.Start},
		input:       input,
		output:      result,
		fullContent: true,
		bytes:       int64(len(transcript)),
	}
}

// syntheticClaudeTranscript builds a JSONL transcript of about size bytes. The
// content is synthetic text, never a recorded session.
func syntheticClaudeTranscript(sessionID string, size int) []byte {
	line := func(text string) string {
		return fmt.Sprintf(`{"type":"user","sessionId":%q,"timestamp":"2023-11-14T22:13:20.000Z","cwd":"/synthetic/content-batch","message":{"role":"user","content":%q}}`, sessionID, text)
	}
	overhead := len(line("")) + 1
	body := size - overhead
	if body < 1 {
		body = 1
	}
	return []byte(line(strings.Repeat("a", body)) + "\n")
}
