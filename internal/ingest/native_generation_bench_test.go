package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// Simulated per-candidate cost model for the native batch benchmark. The
// numbers mirror the live observation that made the batch stall visible: one
// giant candidate takes seconds to stage while small candidates and their
// commits wait behind it.
const (
	benchGiantBlobs    = 2000
	benchSmallSessions = 8
	benchSmallBlobs    = 20
	benchStagePerBlob  = time.Millisecond
	benchCommitDelay   = 100 * time.Millisecond
)

// benchNativeStore is a metrics store with simulated staging and commit
// latencies. The base type has no StageNativeGeneration method, so the
// pipeline takes its inline serial path (stage inside the activation);
// benchNativeStagingStore adds the method so the pipeline pre-stages in
// parallel and streams commits.
type benchNativeStore struct {
	MetricsStore
	stagePerBlob time.Duration
	commitDelay  time.Duration

	stageCalls  atomic.Int64
	commitCalls atomic.Int64
	firstCommit atomic.Int64 // unix nanos of the first commit, 0 if none
}

type benchFirstCommitReporter interface {
	firstCommitNanos() int64
	commitCount() int64
}

func (s *benchNativeStore) commitCount() int64 { return s.commitCalls.Load() }

func (s *benchNativeStore) firstCommitNanos() int64 { return s.firstCommit.Load() }

func (s *benchNativeStore) IndexSessionEntryBatch(_ context.Context, writes []SessionEntryWrite) []SessionEntryWriteResult {
	results := make([]SessionEntryWriteResult, len(writes))
	for i, write := range writes {
		results[i] = SessionEntryWriteResult{SessionID: write.SessionID, Written: true}
	}
	return results
}

// stage simulates writing and fsyncing one candidate's content blobs.
func (s *benchNativeStore) stage(blobCount int) {
	s.stageCalls.Add(1)
	time.Sleep(time.Duration(blobCount) * s.stagePerBlob)
}

// commitOnly simulates the database activation transaction.
func (s *benchNativeStore) commitOnly(activation NativeGenerationActivation) (ActivationOutcome, error) {
	if s.commitCalls.Add(1) == 1 {
		s.firstCommit.CompareAndSwap(0, time.Now().UnixNano())
	}
	time.Sleep(s.commitDelay)
	return ActivationOutcome{Disposition: ActivationCommittedNow, CandidateID: activation.Generation.Generation.ID}, nil
}

func (s *benchNativeStore) ActivateNativeGeneration(_ context.Context, activation NativeGenerationActivation) (ActivationOutcome, error) {
	// Inline path: the activation owns the staging.
	s.stage(len(activation.Blobs))
	return s.commitOnly(activation)
}

// benchNativeStagingStore adds pre-staging: files are staged before the
// activation, so the activation pays only the commit.
type benchNativeStagingStore struct {
	*benchNativeStore
}

// benchStaged is the inert prepared-files handle the bench stager returns.
type benchStaged string

func (b benchStaged) NativeGenerationCandidateID() string { return string(b) }

func (s *benchNativeStagingStore) StageNativeGeneration(_ context.Context, activation NativeGenerationActivation) (NativeGenerationStaged, error) {
	s.stage(len(activation.Blobs))
	return benchStaged(activation.Generation.Generation.ID), nil
}

func (s *benchNativeStagingStore) ActivateStagedNativeGeneration(_ context.Context, activation NativeGenerationActivation, _ NativeGenerationStaged) (ActivationOutcome, error) {
	return s.commitOnly(activation)
}

func (s *benchNativeStagingStore) ActivateNativeGeneration(ctx context.Context, activation NativeGenerationActivation) (ActivationOutcome, error) {
	// Without a prepared handle the activation stages inline.
	return s.benchNativeStore.ActivateNativeGeneration(ctx, activation)
}

var (
	_ NativeGenerationStager            = (*benchNativeStagingStore)(nil)
	_ NativeGenerationPreparedActivator = (*benchNativeStagingStore)(nil)
)

// BenchmarkNativeGenerationBatchCommits drives the real native flush over one
// giant candidate and eight small ones. "serial-inline" stages inline inside
// each activation (a store without a stager, one candidate at a time);
// "streamed" prepares in parallel and commits as each preparation completes.
func BenchmarkNativeGenerationBatchCommits(b *testing.B) {
	results := benchNativeResults(b)
	for _, tc := range []struct {
		name  string
		store func() MetricsStore
	}{
		{
			name: "serial-inline",
			store: func() MetricsStore {
				return &benchNativeStore{stagePerBlob: benchStagePerBlob, commitDelay: benchCommitDelay}
			},
		},
		{
			name: "streamed",
			store: func() MetricsStore {
				return &benchNativeStagingStore{benchNativeStore: &benchNativeStore{stagePerBlob: benchStagePerBlob, commitDelay: benchCommitDelay}}
			},
		},
	} {
		b.Run(tc.name, func(b *testing.B) {
			var firstCommitTotal time.Duration
			for i := 0; i < b.N; i++ {
				store := tc.store()
				pipeline := &Pipeline{config: PipelineConfig{Parallelism: 8}, metricsStore: store}
				start := time.Now()
				pipeline.flushIndexParseResults(context.Background(), results, IndexOutcomeIndexed, "bench", nil)
				reporter, ok := store.(benchFirstCommitReporter)
				if !ok {
					b.Fatalf("store %T does not report first commit", store)
				}
				if got := reporter.commitCount(); got != int64(len(results)) {
					b.Fatalf("commits = %d, want one per result (%d)", got, len(results))
				}
				first := reporter.firstCommitNanos()
				if first == 0 {
					b.Fatal("no activation ran")
				}
				firstCommitTotal += time.Duration(first - start.UnixNano())
			}
			b.ReportMetric(float64(firstCommitTotal.Milliseconds())/float64(b.N), "ms/first-commit")
		})
	}
}

// benchNativeResults builds the benchmark's candidate set: the giant first,
// then the small sessions.
func benchNativeResults(tb testing.TB) []indexParseResult {
	tb.Helper()
	blobCounts := make([]int, 0, benchSmallSessions+1)
	blobCounts = append(blobCounts, benchGiantBlobs)
	for i := 0; i < benchSmallSessions; i++ {
		blobCounts = append(blobCounts, benchSmallBlobs)
	}
	results := make([]indexParseResult, 0, len(blobCounts))
	for i, blobCount := range blobCounts {
		results = append(results, benchNativeResult(tb, i, blobCount))
	}
	return results
}

// benchNativeResult builds one valid native candidate plus the captured input
// the write path verifies: a complete V2 generation, its blobs, and metadata
// whose content checksum binds the artifact identity the commit re-checks.
func benchNativeResult(tb testing.TB, index, blobCount int) indexParseResult {
	tb.Helper()
	sid, err := schema.NewSessionID(fmt.Sprintf("00000000-0000-4000-8000-%012d", index))
	if err != nil {
		tb.Fatal(err)
	}
	harness := HarnessClaudeCode
	entries := make([]schema.SessionEntry, 0, blobCount)
	content := make([]indexformat.ContentRecord, 0, blobCount)
	blobs := make(map[schema.SourceEntryRef][]byte, blobCount)
	for i := 0; i < blobCount; i++ {
		ref := schema.SourceEntryRef(fmt.Sprintf("e_%06d", i))
		text := fmt.Sprintf("bench text %d", i)
		entries = append(entries, schema.SessionEntry{
			SessionID: sid, EntryIndex: i, Harness: schema.Harness(harness),
			Role: RoleUser, EntryType: EntryTypeText, ContentPreview: &text, SourceEntryRef: ref,
		})
		content = append(content, indexformat.ContentRecord{Ref: ref})
		blobs[ref] = []byte(text)
	}
	inputCount := int64(1)
	generation := indexformat.Generation{
		ID:           fmt.Sprintf("g_bench_%d", index),
		Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  schema.Harness(harness),
			Stats:         schema.SessionStats{TurnCount: len(entries), InputSubmissionCount: &inputCount},
		},
		Main:                 indexformat.Partition{Entries: entries},
		Content:              content,
		SourceEvidenceDigest: strings.Repeat("a", 64),
		TitleRefs:            []schema.SourceEntryRef{content[0].Ref},
	}
	v2 := indexformat.V2{Generation: generation}

	meta := schema.UnifiedMetadata{
		SchemaVersion: CurrentSchemaVersion,
		SessionID:     sid,
		ModelHarness:  schema.Harness(harness),
		ContentHash:   strings.Repeat("c", 64),
	}
	metadataData, err := json.Marshal(meta)
	if err != nil {
		tb.Fatal(err)
	}
	semantic, err := artifactSemanticJSON(metadataData, meta.ContentHash)
	if err != nil {
		tb.Fatal(err)
	}
	input := &CapturedIndexInput{
		session:      DiscoveredSession{SessionID: sid, Harness: harness},
		metadataData: metadataData,
		artifactHash: schema.ComputeTranscriptHash(semantic),
		inputHash:    "bench-input-hash",
	}
	return indexParseResult{
		im:              indexedMeta{session: DiscoveredSession{SessionID: sid, Harness: harness}},
		input:           input,
		output:          v2,
		nativeCandidate: &NativeGenerationCandidate{Result: v2, Blobs: blobs},
		startedAt:       time.Now().UnixMilli(),
	}
}
