package ingest

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"

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
			// Built by literal to reach the unexported flush. The path under test
			// reads exactly these: fs and config.OutputDir for the artifact
			// publisher, and metricsStore for the atomic write. Every other field
			// is nil or zero on purpose: store nil makes the metadata-compatibility
			// check fall back to metricsStore and return, and a nil harvester
			// version map means the shipped registry.
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

//go:embed testdata/content_batch_budget.yaml
var contentBatchBudgetFixtureData []byte

// budgetIndexer is the parser seam for the byte-budget cases: for one session it
// returns entries whose total write bytes are exactly what the case declares.
//
// The grouping boundary measures the PARSE OUTPUT, not the transcript, so a case
// can cross a 32 MiB budget from a transcript of a few bytes. Everything else on
// the path is real: the artifact is committed and validated, the input is captured
// and hashed, and the grouping and the flush are the production ones.
type budgetIndexer struct{ bytes map[SessionID]int }

var (
	_ TranscriptIndexer              = (*budgetIndexer)(nil)
	_ AuthoritativeTranscriptIndexer = (*budgetIndexer)(nil)
)

func (idx *budgetIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }

func (idx *budgetIndexer) entries(session DiscoveredSession) []schema.SessionEntry {
	size := idx.bytes[session.SessionID]
	if size < 1 {
		size = 1
	}
	content := strings.Repeat("a", size)
	return []schema.SessionEntry{{
		SessionID: schema.SessionID(session.SessionID), EntryIndex: 0, Harness: HarnessClaudeCode,
		Role: schema.RoleUser, EntryType: schema.EntryTypeText, ContentPreview: &content,
	}}
}

func (idx *budgetIndexer) IndexTranscript(_ context.Context, session DiscoveredSession) ([]schema.SessionEntry, error) {
	return idx.entries(session), nil
}

func (idx *budgetIndexer) IndexTranscriptBytes(_ context.Context, session DiscoveredSession, _ []byte) ([]schema.SessionEntry, error) {
	return idx.entries(session), nil
}

func (idx *budgetIndexer) IndexTranscriptForCapture(_ context.Context, session DiscoveredSession) (TranscriptCaptureResult, error) {
	return TranscriptCaptureResult{Entries: idx.entries(session)}, nil
}

func (idx *budgetIndexer) IndexTranscriptBytesForCapture(_ context.Context, session DiscoveredSession, _ []byte) (TranscriptCaptureResult, error) {
	return TranscriptCaptureResult{Entries: idx.entries(session)}, nil
}

// budgetStore is the store the grouping boundary writes through: it records the
// ORDER sessions were committed in and answers the index-state read the capture
// requires. The order is what reveals the grouping; see the fixture header.
type budgetStore struct {
	MetricsStore
	states    map[SessionID]*SessionIndexState
	committed []SessionID
}

var (
	_ SessionEntryBatchStore  = (*budgetStore)(nil)
	_ SessionIndexStateReader = (*budgetStore)(nil)
)

func (s *budgetStore) ReadIndexState(_ context.Context, id SessionID) (*SessionIndexState, error) {
	return s.states[id], nil
}

func (s *budgetStore) IndexSessionEntryBatch(_ context.Context, writes []SessionEntryWrite) []SessionEntryWriteResult {
	results := make([]SessionEntryWriteResult, len(writes))
	for i, write := range writes {
		s.committed = append(s.committed, write.SessionID)
		results[i] = SessionEntryWriteResult{SessionID: write.SessionID, Written: true}
	}
	return results
}

// TestFullContentWriteBatchBudgetGroupsByBytes holds the byte budget that bounds
// one write wave.
//
// The budget is what keeps a wave of large sessions from being held in memory at
// once; losing the split is an out-of-memory failure on a real store, not a
// cosmetic regression. It is asserted at the boundary that applies it: the batch
// grouping that decides how many parsed sessions travel to one flush.
//
// Sizes are declared as a FRACTION of the shipped budget rather than in bytes, so
// the corpus still describes the same situations if the budget ever moves.
func TestFullContentWriteBatchBudgetGroupsByBytes(t *testing.T) {
	var fixtures struct {
		Budget   string   `yaml:"budget"`
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name      string    `yaml:"name"`
			Fractions []float64 `yaml:"session_budget_fractions"`
			Groups    [][]int   `yaml:"expected_flush_groups"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(contentBatchBudgetFixtureData, &fixtures); err != nil {
		t.Fatal(err)
	}
	if fixtures.Budget != "defaults.FullContentWriteBatchBytes" {
		t.Fatalf("the corpus must name the budget under test, got %q", fixtures.Budget)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if names[fixture.Name] {
			t.Fatalf("duplicate fixture %s", fixture.Name)
		}
		names[fixture.Name] = true
		t.Run(fixture.Name, func(t *testing.T) {
			ctx := context.Background()
			output := t.TempDir()
			indexer := &budgetIndexer{bytes: make(map[SessionID]int)}
			store := &budgetStore{states: make(map[SessionID]*SessionIndexState)}
			// Built by literal to reach the unexported grouping. This path reads
			// fs and config.OutputDir for the artifact publisher, metricsStore for
			// both the index-state read and the atomic write, and indexers for the
			// parse. store stays nil so the metadata-compatibility check falls back
			// to metricsStore and returns; the harvester version map stays nil so
			// the shipped registry applies; the profiler stays nil and records
			// nothing.
			pipeline := &Pipeline{
				fs:           &OSFileSystem{},
				metricsStore: store,
				indexers:     map[Harness]TranscriptIndexer{HarnessClaudeCode: indexer},
				// One parser worker keeps the waves and therefore the observed
				// grouping deterministic; Force makes every session need work, so
				// no case is skipped for being current.
				config: PipelineConfig{OutputDir: ResolvedPath(output), Parallelism: 1, Force: true},
			}
			var metas []indexedMeta
			for index, fraction := range fixture.Fractions {
				written := int(float64(defaults.FullContentWriteBatchBytes) * fraction)
				meta, artifactHash := publishSyntheticArtifact(t, output, index)
				indexer.bytes[meta.session.SessionID] = written
				store.states[meta.session.SessionID] = &SessionIndexState{
					SessionID: meta.session.SessionID, Harness: HarnessClaudeCode, ArtifactHash: &artifactHash,
				}
				metas = append(metas, meta)
			}
			indexed, logs := pipeline.indexBatch(ctx, metas, IndexOutcomeIndexed, "budget test")
			if len(indexed) != len(metas) || len(logs) != len(metas) {
				t.Fatalf("the grouping lost a session: indexed=%d logs=%d for %d sessions", len(indexed), len(logs), len(metas))
			}
			want := expectedCommitOrder(t, fixture.Groups, metas)
			if !reflect.DeepEqual(store.committed, want) {
				t.Fatalf("commit order %v does not match the flush groups %v (that grouping commits in order %v); the budget is %d bytes and this case wrote fractions %v",
					store.committed, fixture.Groups, want, defaults.FullContentWriteBatchBytes, fixture.Fractions)
			}
		})
	}
	for _, name := range fixtures.Required {
		if !names[name] {
			t.Fatalf("missing required fixture %s", name)
		}
	}
}

// publishSyntheticArtifact commits one tiny managed artifact and returns the meta
// the index stage consumes plus the artifact hash the capture will demand.
func publishSyntheticArtifact(t *testing.T, output string, index int) (indexedMeta, string) {
	t.Helper()
	// Descending identifiers: a flush sorts its own sessions, so this is what makes
	// a change in grouping show up as a change in commit order.
	sessionID, err := NewSessionID(fmt.Sprintf("22222222-2222-4222-8222-00000000000%d", 9-index))
	if err != nil {
		t.Fatal(err)
	}
	const hostSlug = "github.com--peasant-labs--content-batch-budget"
	meta := NewUnifiedMetadata()
	meta.SessionID = sessionID
	meta.ModelHarness = HarnessClaudeCode
	meta.Model = "claude-opus-4-8"
	meta.HostSlug = hostSlug
	meta.Timestamp = TimestampInfo{Start: 1700000000000, End: 1700000001000}
	meta.Project = ProjectInfo{Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "content-batch", FilePath: "/synthetic/content-batch"}
	transcriptPath := filepath.Join(SessionDir(output, hostSlug, string(sessionID), ""), string(sessionID)+"--transcript."+string(SourceFormatJSONL))
	meta.Source = SourceInfo{Format: SourceFormatJSONL, FilePath: transcriptPath}
	transcript := syntheticClaudeTranscript(string(sessionID), 256)
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
	session := DiscoveredSession{SessionID: sessionID, Harness: HarnessClaudeCode, SourceFormat: SourceFormatJSONL, SourcePath: ResolvedPath(transcriptPath)}
	return indexedMeta{session: session, outputTranscriptPath: transcriptPath, startMs: meta.Timestamp.Start}, artifact.ArtifactHash
}

// expectedCommitOrder turns the flush groups a case declares into the commit
// order they produce.
//
// A flush commits each of its sessions separately, so the NUMBER of commits
// cannot show the grouping. The order can: a flush sorts its own sessions by
// identifier before committing them, while the flushes themselves run in parse
// order, and these fixtures seed identifiers that DESCEND as parsing advances.
// Any change in grouping therefore changes the order, which is what makes a lost
// split visible here instead of silently identical.
func expectedCommitOrder(t *testing.T, groups [][]int, metas []indexedMeta) []SessionID {
	t.Helper()
	seen := make(map[int]bool)
	var order []SessionID
	for _, group := range groups {
		var ids []SessionID
		for _, index := range group {
			if index < 0 || index >= len(metas) {
				t.Fatalf("a flush group names session %d, which this case does not seed", index)
			}
			if seen[index] {
				t.Fatalf("session %d appears in more than one flush group", index)
			}
			seen[index] = true
			ids = append(ids, metas[index].session.SessionID)
		}
		slices.Sort(ids)
		order = append(order, ids...)
	}
	if len(seen) != len(metas) {
		t.Fatalf("the flush groups cover %d of %d seeded sessions; every session is committed exactly once", len(seen), len(metas))
	}
	return order
}
