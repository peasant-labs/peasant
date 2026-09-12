package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/index_parallel.yaml
var indexParallelYAML []byte

//go:embed testdata/warm-path/stream-compute-annotate.yaml
var streamComputeAnnotateYAML []byte

type indexParallelFixture struct {
	RequiredSessions []string               `yaml:"required_sessions"`
	Sessions         []indexParallelSession `yaml:"sessions"`
}

type indexParallelSession struct {
	Name    string `yaml:"name"`
	ID      string `yaml:"id"`
	Preview string `yaml:"preview"`
}

func TestStreamingIndex_DrainKeepsArenaUntilBatchDone(t *testing.T) {
	fixture := loadIndexParallelFixture(t)
	releaseParses := make(chan struct{})
	metas, entries := buildIndexParallelMetas(t, fixture)
	indexer := &blockingParallelIndexer{entries: entries, release: releaseParses}
	store := &serialIndexStore{entries: make(map[SessionID][]schema.SessionEntry)}
	pipeline := &Pipeline{config: PipelineConfig{Parallelism: 2}, indexers: map[Harness]TranscriptIndexer{HarnessClaudeCode: indexer}, metricsStore: store}
	prepareIndexParallelInputs(t, pipeline, metas)
	progress := NewProgressState()
	progress.Update(ProgressEvent{Kind: KindStart, Stage: StageIndex, Total: len(metas)})
	staging := NewStagingBuffer(len(metas)+1, 1024*1024)
	for _, im := range metas {
		staging.Add(indexWorkerResult(im))
	}
	workersDone := atomic.Bool{}
	workersDone.Store(true)
	indexCh := make(chan streamedIndexWork, len(metas))
	indexDoneCh := make(chan DrainBatch, 1)
	drainDone := make(chan []SessionResult, 1)
	go func() {
		drainDone <- pipeline.drainLoop(context.Background(), staging, &workersDone, indexCh, indexDoneCh, make(chan error, 1), progress, len(metas), nil)
		close(indexCh)
	}()
	indexDone := make(chan struct{})
	go func() {
		pipeline.indexLoop(context.Background(), indexCh, indexDoneCh, progress, IndexOutcomeIndexed, "test", nil, nil)
		close(indexDone)
	}()

	waitForActiveParses(t, indexer, int64(len(metas)))
	if staging.ArenaUsed() == 0 {
		t.Fatal("staging arena was acknowledged before parse workers released arena-backed data")
	}
	select {
	case <-drainDone:
		t.Fatal("drainLoop returned before streamed INDEX completed its drain-batch token")
	default:
	}
	close(releaseParses)
	select {
	case results := <-drainDone:
		if len(results) != len(metas) {
			t.Fatalf("drain results = %d, want %d", len(results), len(metas))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drainLoop did not finish")
	}
	select {
	case <-indexDone:
	case <-time.After(2 * time.Second):
		t.Fatal("indexLoop did not finish")
	}
	if got := staging.ArenaUsed(); got != 0 {
		t.Fatalf("staging arena used = %d, want 0 after streamed INDEX completion", got)
	}
	if indexer.maxActive.Load() < int64(len(metas)) {
		t.Fatalf("max active parses = %d, want %d", indexer.maxActive.Load(), len(metas))
	}
	if store.maxActive.Load() != 1 {
		t.Fatalf("max active writes = %d, want 1", store.maxActive.Load())
	}
	if got := progress.Snapshot()[StageIndex].Done; got != len(metas) {
		t.Fatalf("INDEX progress done = %d, want %d", got, len(metas))
	}
	if len(store.entries) != len(metas) {
		t.Fatalf("indexed session count = %d, want %d", len(store.entries), len(metas))
	}
}

func TestStreamingIndex_ProgressAdvancesPerSessionWithinDrainBatch(t *testing.T) {
	fixture := loadIndexParallelFixture(t)
	metas, entries := buildIndexParallelMetas(t, fixture)
	releaseBlocked := make(chan struct{})
	indexer := &selectiveBlockingIndexer{entries: entries, blocked: metas[1].session.SessionID, release: releaseBlocked}
	store := &serialIndexStore{entries: make(map[SessionID][]schema.SessionEntry), wrote: make(chan SessionID, len(metas))}
	pipeline := &Pipeline{config: PipelineConfig{Parallelism: 2}, indexers: map[Harness]TranscriptIndexer{HarnessClaudeCode: indexer}, metricsStore: store}
	prepareIndexParallelInputs(t, pipeline, metas)
	progress := NewProgressState()
	progress.Update(ProgressEvent{Kind: KindStart, Stage: StageIndex, Total: len(metas)})
	indexCh := make(chan streamedIndexWork, len(metas))
	indexDoneCh := make(chan DrainBatch, 1)
	done := make(chan struct{})
	go func() {
		pipeline.indexLoop(context.Background(), indexCh, indexDoneCh, progress, IndexOutcomeIndexed, "test", nil, nil)
		close(done)
	}()

	completion := newIndexBatchCompletion(DrainBatch{Metas: metas}, len(metas))
	for _, im := range metas {
		indexCh <- streamedIndexWork{meta: im, batch: completion}
	}
	close(indexCh)
	select {
	case got := <-store.wrote:
		if got != metas[0].session.SessionID {
			t.Fatalf("first indexed session = %s, want %s", got, metas[0].session.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first streamed INDEX write did not complete")
	}
	waitForIndexProgress(t, progress, 1)
	select {
	case <-indexDoneCh:
		t.Fatal("streamed INDEX signalled drain-batch completion before all sessions parsed")
	default:
	}
	close(releaseBlocked)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("indexLoop did not finish")
	}
	waitForIndexProgress(t, progress, len(metas))
}

func TestStreamingIndex_ParallelismOneWritesInFixtureOrder(t *testing.T) {
	fixture := loadIndexParallelFixture(t)
	metas, entries := buildIndexParallelMetas(t, fixture)
	store := &serialIndexStore{entries: make(map[SessionID][]schema.SessionEntry)}
	pipeline := &Pipeline{config: PipelineConfig{Parallelism: 1}, indexers: map[Harness]TranscriptIndexer{HarnessClaudeCode: &immediateIndexer{entries: entries}}, metricsStore: store}
	prepareIndexParallelInputs(t, pipeline, metas)
	indexCh := make(chan streamedIndexWork, len(metas))
	indexDoneCh := make(chan DrainBatch, 1)
	done := make(chan struct{})
	go func() {
		pipeline.indexLoop(context.Background(), indexCh, indexDoneCh, nil, IndexOutcomeIndexed, "test", nil, nil)
		close(done)
	}()

	completion := newIndexBatchCompletion(DrainBatch{Metas: metas}, len(metas))
	for _, im := range metas {
		indexCh <- streamedIndexWork{meta: im, batch: completion}
	}
	close(indexCh)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("indexLoop did not finish")
	}
	if len(store.writeOrder) != len(metas) {
		t.Fatalf("write order length = %d, want %d", len(store.writeOrder), len(metas))
	}
	for i, im := range metas {
		if store.writeOrder[i] != im.session.SessionID {
			t.Fatalf("write order[%d] = %s, want %s", i, store.writeOrder[i], im.session.SessionID)
		}
		input, err := pipeline.captureIndexInput(t.Context(), im, pipeline.indexers[im.session.Harness])
		if err != nil || input.expected.IndexedInputHash == nil || *input.expected.IndexedInputHash != input.inputHash {
			t.Fatalf("stored proof does not identify captured fixture input for %s: %v", im.session.SessionID, err)
		}
	}
}

func TestIndexBatch_UsesStoreBatchWriterAndProfilesWriteShape(t *testing.T) {
	fixture := loadIndexParallelFixture(t)
	metas, entries := buildIndexParallelMetas(t, fixture)
	profiler := &IndexProfiler{}
	store := &batchIndexStore{serialIndexStore: serialIndexStore{entries: make(map[SessionID][]schema.SessionEntry)}}
	pipeline := &Pipeline{
		config:       PipelineConfig{Parallelism: 1, IndexProfiler: profiler},
		indexers:     map[Harness]TranscriptIndexer{HarnessClaudeCode: &immediateIndexer{entries: entries}},
		metricsStore: store,
	}

	prepareIndexParallelInputs(t, pipeline, metas)
	indexed, logs := pipeline.indexBatch(context.Background(), metas, IndexOutcomeIndexed, "test")
	if len(indexed) != len(metas) {
		t.Fatalf("indexed result count = %d, want %d", len(indexed), len(metas))
	}
	if len(logs) != len(metas) {
		t.Fatalf("index log count = %d, want %d", len(logs), len(metas))
	}
	if got := store.singleWrites.Load(); got != 0 {
		t.Fatalf("single-session writes = %d, want 0", got)
	}
	for _, size := range store.batchSizes {
		if size != 1 {
			t.Fatalf("Store batch size = %d, want one atomic session", size)
		}
	}
	snapshot := profiler.Snapshot()
	var transactions, savepoints int
	var stats SessionEntryWriteStats
	for _, batch := range snapshot.Batches {
		transactions += batch.WriteTxs
		savepoints += batch.WriteSavepoints
		stats.Add(batch.WriteStats)
	}
	if transactions != len(store.batchSizes) || savepoints != transactions {
		t.Fatalf("profile shape = %d transactions/%d savepoints, recorded one-item Store calls=%v", transactions, savepoints, store.batchSizes)
	}
	if stats.HashMatches != len(metas) || stats.AnnotationTargetsCarried != len(metas)*2 {
		t.Fatalf("profile write stats = %+v, want hash matches %d and annotation targets carried %d", stats, len(metas), len(metas)*2)
	}
}

func TestStreamingIndex_StartsDownstreamBeforeAllIndexCompletes(t *testing.T) {
	fixture := loadStreamComputeAnnotateFixture(t)
	metas, entries := buildIndexParallelMetas(t, fixture)
	releaseSecondWrite := make(chan struct{})
	store := &blockingSecondIndexStore{
		serialIndexStore: serialIndexStore{entries: make(map[SessionID][]schema.SessionEntry)},
		blocked:          metas[1].session.SessionID,
		release:          releaseSecondWrite,
	}
	analyzer := &recordingStreamAnalyzer{computeStarted: make(chan SessionID, len(metas)), computeDone: make(chan SessionID, len(metas))}
	classifier := &recordingStreamBufferedClassifier{prepared: make(chan SessionID, len(metas))}
	pipeline := &Pipeline{
		config:       PipelineConfig{Parallelism: 1},
		indexers:     map[Harness]TranscriptIndexer{HarnessClaudeCode: &immediateIndexer{entries: entries}},
		metricsStore: store,
		analyzer:     analyzer,
		classifier:   classifier,
	}
	prepareIndexParallelInputs(t, pipeline, metas)
	progress := NewProgressState()
	progress.Update(ProgressEvent{Kind: KindStart, Stage: StageIndex, Total: len(metas)})
	indexCh := make(chan streamedIndexWork, len(metas))
	indexDoneCh := make(chan DrainBatch, 1)
	downstreamCh := make(chan indexedMeta, len(metas))
	downstreamDone := make(chan streamedDownstreamResult, 1)
	go func() {
		downstreamDone <- pipeline.runStreamedDownstream(context.Background(), downstreamCh, progress, len(metas), "test", nil)
	}()
	indexDone := make(chan struct{})
	go func() {
		pipeline.indexLoop(context.Background(), indexCh, indexDoneCh, progress, IndexOutcomeIndexed, "test", downstreamCh, nil)
		close(downstreamCh)
		close(indexDone)
	}()

	completion := newIndexBatchCompletion(DrainBatch{Metas: metas}, len(metas))
	indexCh <- streamedIndexWork{meta: metas[0], batch: completion}

	select {
	case got := <-analyzer.computeStarted:
		if got != metas[0].session.SessionID {
			t.Fatalf("first streamed COMPUTE session = %s, want %s", got, metas[0].session.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("COMPUTE did not start after the first session finished INDEX")
	}
	select {
	case <-indexDone:
		t.Fatal("INDEX completed all sessions before streamed COMPUTE started")
	default:
	}
	select {
	case got := <-classifier.prepared:
		if got != metas[0].session.SessionID {
			t.Fatalf("first streamed ANNOTATE prepare session = %s, want %s", got, metas[0].session.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ANNOTATE prepare did not start after COMPUTE finished for the first indexed session")
	}

	for _, im := range metas[1:] {
		indexCh <- streamedIndexWork{meta: im, batch: completion}
	}
	close(indexCh)
	close(releaseSecondWrite)
	select {
	case <-indexDone:
	case <-time.After(2 * time.Second):
		t.Fatal("indexLoop did not finish after blocked write released")
	}
	select {
	case got := <-downstreamDone:
		if got.ComputeDone != len(metas) || got.AnnotateDone != len(metas) || got.Computed != len(metas) {
			t.Fatalf("downstream result = %+v, want all %d sessions computed and annotated", got, len(metas))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("streamed downstream worker did not finish")
	}
	if got := progress.Snapshot()[StageCompute].Done; got != len(metas) {
		t.Fatalf("COMPUTE progress done = %d, want %d", got, len(metas))
	}
	if got := progress.Snapshot()[StageAnnotate].Done; got != len(metas) {
		t.Fatalf("ANNOTATE progress done = %d, want %d", got, len(metas))
	}
}

func TestStreamingIndex_StoreWriteLaneSerializesDownstreamWrites(t *testing.T) {
	fixture := loadStreamComputeAnnotateFixture(t)
	metas, entries := buildIndexParallelMetas(t, fixture)
	if len(metas) < 2 {
		t.Fatalf("stream compute fixture has %d sessions, want at least 2", len(metas))
	}
	tracker := &writeOverlapTracker{}
	releaseCompute := make(chan struct{})
	store := &trackedIndexStore{
		serialIndexStore: serialIndexStore{entries: make(map[SessionID][]schema.SessionEntry)},
		tracker:          tracker,
	}
	analyzer := &blockingTrackedAnalyzer{
		tracker: tracker,
		entered: make(chan struct{}),
		release: releaseCompute,
	}
	classifier := &trackedBufferedClassifier{tracker: tracker}
	pipeline := &Pipeline{
		config:       PipelineConfig{Parallelism: 1},
		indexers:     map[Harness]TranscriptIndexer{HarnessClaudeCode: &immediateIndexer{entries: entries}},
		metricsStore: store,
		analyzer:     analyzer,
		classifier:   classifier,
	}
	prepareIndexParallelInputs(t, pipeline, metas)
	progress := NewProgressState()
	progress.Update(ProgressEvent{Kind: KindStart, Stage: StageIndex, Total: len(metas)})
	indexCh := make(chan streamedIndexWork, len(metas))
	indexDoneCh := make(chan DrainBatch, 1)
	downstreamCh := make(chan indexedMeta, len(metas))
	writeLane := newStoreWriteLane(1)

	downstreamDone := make(chan streamedDownstreamResult, 1)
	go func() {
		downstreamDone <- pipeline.runStreamedDownstream(context.Background(), downstreamCh, progress, len(metas), "test", writeLane)
	}()
	indexDone := make(chan struct{})
	go func() {
		pipeline.indexLoop(context.Background(), indexCh, indexDoneCh, progress, IndexOutcomeIndexed, "test", downstreamCh, writeLane)
		close(downstreamCh)
		close(indexDone)
	}()

	completion := newIndexBatchCompletion(DrainBatch{Metas: metas}, len(metas))
	indexCh <- streamedIndexWork{meta: metas[0], batch: completion}
	select {
	case <-analyzer.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("streamed COMPUTE did not enter the writer lane")
	}
	for _, im := range metas[1:] {
		indexCh <- streamedIndexWork{meta: im, batch: completion}
	}
	close(indexCh)
	time.Sleep(25 * time.Millisecond)
	if got := tracker.maxActive.Load(); got != 1 {
		t.Fatalf("concurrent store writes while COMPUTE was blocked = %d, want 1", got)
	}
	close(releaseCompute)

	select {
	case <-indexDone:
	case <-time.After(2 * time.Second):
		t.Fatal("indexLoop did not finish")
	}
	select {
	case got := <-downstreamDone:
		if got.ComputeDone != len(metas) || got.AnnotateDone != len(metas) {
			t.Fatalf("downstream result = %+v, want all %d sessions processed", got, len(metas))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("streamed downstream worker did not finish")
	}
	writeLane.close()
	if got := tracker.maxActive.Load(); got != 1 {
		t.Fatalf("max concurrent store writes = %d, want 1", got)
	}
}

func TestStreamedDownstream_StopsSafelyAfterCancellation(t *testing.T) {
	fixture := loadStreamComputeAnnotateFixture(t)
	metas, _ := buildIndexParallelMetas(t, fixture)
	ctx, cancel := context.WithCancel(context.Background())
	analyzer := &cancelingStreamAnalyzer{cancel: cancel, started: make(chan SessionID, len(metas))}
	classifier := &recordingStreamBufferedClassifier{prepared: make(chan SessionID, len(metas))}
	pipeline := &Pipeline{
		config:     PipelineConfig{Parallelism: 1},
		analyzer:   analyzer,
		classifier: classifier,
	}
	progress := NewProgressState()
	downstreamCh := make(chan indexedMeta, len(metas))
	downstreamCh <- indexedMeta{session: metas[0].session, startMs: metas[0].startMs, indexed: true}
	close(downstreamCh)

	result := pipeline.runStreamedDownstream(ctx, downstreamCh, progress, len(metas), "test", nil)
	if result.ComputeDone != 1 {
		t.Fatalf("computed work after cancellation = %d, want 1", result.ComputeDone)
	}
	if result.AnnotateDone != 0 {
		t.Fatalf("annotated work after cancellation = %d, want 0", result.AnnotateDone)
	}
	if len(analyzer.started) != 1 {
		t.Fatalf("started compute sessions after cancellation = %d, want 1", len(analyzer.started))
	}
}

func TestStreamedDownstream_ComputeErrorStillAnnotatesIndexedSessions(t *testing.T) {
	fixture := loadStreamComputeAnnotateFixture(t)
	metas, _ := buildIndexParallelMetas(t, fixture)
	analyzer := &erroringStreamAnalyzer{err: fmt.Errorf("compute failed")}
	classifier := &recordingStreamBufferedClassifier{prepared: make(chan SessionID, len(metas))}
	pipeline := &Pipeline{
		config:     PipelineConfig{Parallelism: 1},
		analyzer:   analyzer,
		classifier: classifier,
	}
	progress := NewProgressState()
	downstreamCh := make(chan indexedMeta, len(metas))
	for _, im := range metas {
		downstreamCh <- indexedMeta{session: im.session, startMs: im.startMs, indexed: true}
	}
	close(downstreamCh)

	result := pipeline.runStreamedDownstream(context.Background(), downstreamCh, progress, len(metas), "test", nil)
	if result.Computed != 0 {
		t.Fatalf("computed count after compute error = %d, want 0", result.Computed)
	}
	if result.ComputeDone != len(metas) {
		t.Fatalf("compute progress after compute error = %d, want %d", result.ComputeDone, len(metas))
	}
	if result.AnnotateDone != len(metas) {
		t.Fatalf("annotate progress after compute error = %d, want %d", result.AnnotateDone, len(metas))
	}
	for _, im := range metas {
		select {
		case got := <-classifier.prepared:
			if got != im.session.SessionID {
				t.Fatalf("prepared session = %s, want %s", got, im.session.SessionID)
			}
		default:
			t.Fatalf("missing annotation prepare call for %s", im.session.SessionID)
		}
	}
}

func TestStreamedDownstream_PrepareErrorAdvancesBestEffortProgress(t *testing.T) {
	fixture := loadStreamComputeAnnotateFixture(t)
	metas, _ := buildIndexParallelMetas(t, fixture)
	analyzer := &recordingStreamAnalyzer{computeStarted: make(chan SessionID, len(metas)), computeDone: make(chan SessionID, len(metas))}
	classifier := &recordingStreamBufferedClassifier{prepared: make(chan SessionID, len(metas)), prepareErr: fmt.Errorf("prepare failed")}
	pipeline := &Pipeline{
		config:     PipelineConfig{Parallelism: 1},
		analyzer:   analyzer,
		classifier: classifier,
	}
	progress := NewProgressState()
	downstreamCh := make(chan indexedMeta, len(metas))
	for _, im := range metas {
		downstreamCh <- indexedMeta{session: im.session, startMs: im.startMs, indexed: true}
	}
	close(downstreamCh)

	result := pipeline.runStreamedDownstream(context.Background(), downstreamCh, progress, len(metas), "test", nil)
	if result.Computed != len(metas) || result.ComputeDone != len(metas) {
		t.Fatalf("compute result after annotate error = %+v, want %d computed", result, len(metas))
	}
	if result.AnnotateDone != len(metas) {
		t.Fatalf("annotate progress after prepare error = %d, want %d", result.AnnotateDone, len(metas))
	}
	if got := progress.Snapshot()[StageAnnotate].Done; got != len(metas) {
		t.Fatalf("ANNOTATE progress snapshot = %d, want %d", got, len(metas))
	}
}

type blockingParallelIndexer struct {
	entries   map[SessionID][]schema.SessionEntry
	release   <-chan struct{}
	active    atomic.Int64
	maxActive atomic.Int64
}

func (*blockingParallelIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }
func (idx *blockingParallelIndexer) IndexTranscript(ctx context.Context, session DiscoveredSession) ([]schema.SessionEntry, error) {
	return idx.index(ctx, session)
}
func (idx *blockingParallelIndexer) IndexTranscriptBytes(ctx context.Context, session DiscoveredSession, _ []byte) ([]schema.SessionEntry, error) {
	return idx.index(ctx, session)
}
func (idx *blockingParallelIndexer) index(ctx context.Context, session DiscoveredSession) ([]schema.SessionEntry, error) {
	active := idx.active.Add(1)
	defer idx.active.Add(-1)
	recordMax(&idx.maxActive, active)
	select {
	case <-idx.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return idx.entries[session.SessionID], nil
}

type selectiveBlockingIndexer struct {
	entries map[SessionID][]schema.SessionEntry
	blocked SessionID
	release <-chan struct{}
}

func (*selectiveBlockingIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }
func (idx *selectiveBlockingIndexer) IndexTranscript(ctx context.Context, session DiscoveredSession) ([]schema.SessionEntry, error) {
	return idx.IndexTranscriptBytes(ctx, session, nil)
}
func (idx *selectiveBlockingIndexer) IndexTranscriptBytes(ctx context.Context, session DiscoveredSession, _ []byte) ([]schema.SessionEntry, error) {
	if session.SessionID == idx.blocked {
		select {
		case <-idx.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return idx.entries[session.SessionID], nil
}

type immediateIndexer struct {
	entries map[SessionID][]schema.SessionEntry
}

func (*immediateIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }
func (idx *immediateIndexer) IndexTranscript(ctx context.Context, session DiscoveredSession) ([]schema.SessionEntry, error) {
	return idx.IndexTranscriptBytes(ctx, session, nil)
}
func (idx *immediateIndexer) IndexTranscriptBytes(_ context.Context, session DiscoveredSession, _ []byte) ([]schema.SessionEntry, error) {
	return idx.entries[session.SessionID], nil
}

type serialIndexStore struct {
	MetricsStore
	mu         sync.Mutex
	entries    map[SessionID][]schema.SessionEntry
	states     map[SessionID]*SessionIndexState
	writeOrder []SessionID
	wrote      chan SessionID
	active     atomic.Int64
	maxActive  atomic.Int64
}

type writeOverlapTracker struct {
	active    atomic.Int64
	maxActive atomic.Int64
}

func (tracker *writeOverlapTracker) enter() func() {
	active := tracker.active.Add(1)
	recordMax(&tracker.maxActive, active)
	return func() { tracker.active.Add(-1) }
}

type trackedIndexStore struct {
	serialIndexStore
	tracker *writeOverlapTracker
}

func (store *trackedIndexStore) IndexSessionEntryBatch(ctx context.Context, writes []SessionEntryWrite) []SessionEntryWriteResult {
	done := store.tracker.enter()
	defer done()
	select {
	case <-time.After(10 * time.Millisecond):
	case <-ctx.Done():
		return []SessionEntryWriteResult{{SessionID: writes[0].SessionID, Err: ctx.Err()}}
	}
	return store.serialIndexStore.IndexSessionEntryBatch(ctx, writes)
}

type blockingTrackedAnalyzer struct {
	tracker *writeOverlapTracker
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (a *blockingTrackedAnalyzer) ComputeMetrics(ctx context.Context, sessionIDs []SessionID) (int, error) {
	done := a.tracker.enter()
	defer done()
	a.once.Do(func() { close(a.entered) })
	select {
	case <-a.release:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return len(sessionIDs), nil
}

func (*blockingTrackedAnalyzer) ComputeInsights(context.Context, []string) error { return nil }

type trackedBufferedClassifier struct {
	tracker *writeOverlapTracker
}

var _ BufferedSessionClassifier = (*trackedBufferedClassifier)(nil)

func (*trackedBufferedClassifier) Annotate(context.Context, SessionID) error {
	panic("unexpected direct Annotate call for buffered classifier")
}

func (*trackedBufferedClassifier) PrepareAnnotations(_ context.Context, sessionID SessionID, _ *IndexProfiler) (SessionAnnotationBatch, error) {
	return SessionAnnotationBatch{
		SessionID: sessionID,
		Writes: []SessionAnnotationWrite{{
			TypeID:     "test.annotation",
			Value:      "prepared",
			TargetKind: AnnotationProfileTargetSession,
		}},
	}, nil
}

func (c *trackedBufferedClassifier) FlushAnnotationBatches(ctx context.Context, batches []SessionAnnotationBatch, _ *IndexProfiler) []SessionAnnotationBatchResult {
	done := c.tracker.enter()
	defer done()
	select {
	case <-time.After(10 * time.Millisecond):
	case <-ctx.Done():
		results := make([]SessionAnnotationBatchResult, len(batches))
		for i, batch := range batches {
			results[i] = SessionAnnotationBatchResult{SessionID: batch.SessionID, Err: ctx.Err()}
		}
		return results
	}
	results := make([]SessionAnnotationBatchResult, len(batches))
	for i, batch := range batches {
		results[i] = SessionAnnotationBatchResult{SessionID: batch.SessionID}
	}
	return results
}

type blockingSecondIndexStore struct {
	serialIndexStore
	blocked SessionID
	release <-chan struct{}
}

func (store *blockingSecondIndexStore) IndexSessionEntryBatch(ctx context.Context, writes []SessionEntryWrite) []SessionEntryWriteResult {
	if len(writes) == 1 && writes[0].SessionID == store.blocked {
		select {
		case <-store.release:
		case <-ctx.Done():
			return []SessionEntryWriteResult{{SessionID: writes[0].SessionID, Err: ctx.Err()}}
		}
	}
	return store.serialIndexStore.IndexSessionEntryBatch(ctx, writes)
}

type recordingStreamAnalyzer struct {
	computeStarted chan SessionID
	computeDone    chan SessionID
}

type cancelingStreamAnalyzer struct {
	cancel  context.CancelFunc
	started chan SessionID
}

type erroringStreamAnalyzer struct {
	err error
}

func (a *erroringStreamAnalyzer) ComputeMetrics(context.Context, []SessionID) (int, error) {
	return 0, a.err
}

func (*erroringStreamAnalyzer) ComputeInsights(context.Context, []string) error { return nil }

func (a *cancelingStreamAnalyzer) ComputeMetrics(ctx context.Context, sessionIDs []SessionID) (int, error) {
	for _, sid := range sessionIDs {
		a.started <- sid
	}
	a.cancel()
	return 0, ctx.Err()
}

func (*cancelingStreamAnalyzer) ComputeInsights(context.Context, []string) error { return nil }

func (a *recordingStreamAnalyzer) ComputeMetrics(_ context.Context, sessionIDs []SessionID) (int, error) {
	for _, sid := range sessionIDs {
		a.computeStarted <- sid
		a.computeDone <- sid
	}
	return len(sessionIDs), nil
}

func (*recordingStreamAnalyzer) ComputeInsights(context.Context, []string) error { return nil }

type recordingStreamBufferedClassifier struct {
	prepared   chan SessionID
	flushed    chan []SessionID
	prepareErr error
}

var _ BufferedSessionClassifier = (*recordingStreamBufferedClassifier)(nil)

func (*recordingStreamBufferedClassifier) Annotate(context.Context, SessionID) error {
	panic("unexpected direct Annotate call for buffered classifier")
}

func (c *recordingStreamBufferedClassifier) PrepareAnnotations(_ context.Context, sessionID SessionID, _ *IndexProfiler) (SessionAnnotationBatch, error) {
	if c.prepared != nil {
		c.prepared <- sessionID
	}
	if c.prepareErr != nil {
		return SessionAnnotationBatch{SessionID: sessionID}, c.prepareErr
	}
	return SessionAnnotationBatch{
		SessionID: sessionID,
		Writes: []SessionAnnotationWrite{{
			TypeID:     "test.annotation",
			Value:      "prepared",
			TargetKind: AnnotationProfileTargetSession,
		}},
	}, nil
}

func (c *recordingStreamBufferedClassifier) FlushAnnotationBatches(_ context.Context, batches []SessionAnnotationBatch, _ *IndexProfiler) []SessionAnnotationBatchResult {
	ids := make([]SessionID, len(batches))
	results := make([]SessionAnnotationBatchResult, len(batches))
	for i, batch := range batches {
		ids[i] = batch.SessionID
		results[i] = SessionAnnotationBatchResult{SessionID: batch.SessionID}
	}
	if c.flushed != nil {
		c.flushed <- ids
	}
	return results
}

func (*serialIndexStore) IndexSessionEntries(context.Context, SessionID, []schema.SessionEntry) error {
	return fmt.Errorf("streaming fixture requires conditional batch writes")
}

func (*serialIndexStore) UpdateIndexState(context.Context, SessionID, int, int64) error {
	return fmt.Errorf("streaming fixture requires producer state in the conditional write")
}
func (*serialIndexStore) SupportsIndexFormat(version int) bool { return version == 1 }

func (store *serialIndexStore) ReadIndexState(_ context.Context, sid SessionID) (*SessionIndexState, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return cloneParallelIndexState(store.states[sid]), nil
}

// Local because importing Store from package ingest creates a cycle. Artifact
// identity comes only from the real publisher's validated bytes, never a target.
func (store *serialIndexStore) MirrorArtifacts(ctx context.Context, requests []ArtifactMirrorRequest) []ArtifactMirrorResult {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.states == nil {
		store.states = make(map[SessionID]*SessionIndexState)
	}
	results := make([]ArtifactMirrorResult, len(requests))
	for i, request := range requests {
		if request.Artifact != nil {
			results[i].SessionID = request.Artifact.Metadata.SessionID
		}
		if err := ctx.Err(); err != nil {
			results[i].Err = err
			continue
		}
		if err := request.Artifact.Validate(); err != nil {
			results[i].Err = err
			continue
		}
		sid := request.Artifact.Metadata.SessionID
		state := cloneParallelIndexState(store.states[sid])
		if state == nil {
			state = &SessionIndexState{SessionID: sid}
		}
		state.Harness = request.Artifact.Metadata.ModelHarness
		hash := request.Artifact.ArtifactHash
		state.ArtifactHash = &hash
		store.states[sid] = state
		results[i].Mirrored = true
	}
	return results
}

func (store *serialIndexStore) IndexSessionEntryBatch(ctx context.Context, writes []SessionEntryWrite) []SessionEntryWriteResult {
	active := store.active.Add(1)
	defer store.active.Add(-1)
	recordMax(&store.maxActive, active)
	results := make([]SessionEntryWriteResult, len(writes))
	store.mu.Lock()
	defer store.mu.Unlock()
	for i, write := range writes {
		results[i].SessionID = write.SessionID
		if err := ctx.Err(); err != nil {
			results[i].Err = err
			continue
		}
		current := store.states[write.SessionID]
		if len(writes) != 1 || current == nil || write.ExpectedState == nil || write.ExpectedState.SessionID != write.SessionID || !reflect.DeepEqual(current, write.ExpectedState) {
			results[i].Err = &StaleIndexWorkError{SessionID: write.SessionID}
			continue
		}
		output, ok := write.Result.(indexformat.V1)
		if !ok || write.IndexVersion != 1 || write.IndexerVersion < current.IndexerVersion || current.ArtifactHash == nil || write.IndexedInputHash == nil || *write.IndexedInputHash == "" {
			results[i].Err = fmt.Errorf("streaming fixture received an unproven or incompatible index write")
			continue
		}
		next := cloneParallelIndexState(current)
		hash, format, at := *write.IndexedInputHash, write.IndexVersion, write.IndexedAtMs
		next.IndexedInputHash, next.IndexVersion, next.IndexedAt = &hash, &format, &at
		next.IndexerVersion = write.IndexerVersion
		next.SessionEntriesHash = nil // This scheduling double does not compute row hashes.
		store.entries[write.SessionID] = append([]schema.SessionEntry(nil), output.Entries...)
		store.states[write.SessionID] = next
		store.writeOrder = append(store.writeOrder, write.SessionID)
		results[i].EntriesCount, results[i].Written = len(output.Entries), true
		if store.wrote != nil {
			store.wrote <- write.SessionID
		}
	}
	return results
}

type batchIndexStore struct {
	serialIndexStore
	batchSizes   []int
	singleWrites atomic.Int64
}

func (*batchIndexStore) SupportsIndexFormat(version int) bool { return version == 1 }

func (store *batchIndexStore) IndexSessionEntries(_ context.Context, sessionID SessionID, entries []schema.SessionEntry) error {
	store.singleWrites.Add(1)
	return fmt.Errorf("unexpected non-atomic write for %s", sessionID)
}

func (store *batchIndexStore) IndexSessionEntryBatch(ctx context.Context, writes []SessionEntryWrite) []SessionEntryWriteResult {
	store.mu.Lock()
	store.batchSizes = append(store.batchSizes, len(writes))
	store.mu.Unlock()
	results := store.serialIndexStore.IndexSessionEntryBatch(ctx, writes)
	for i := range results {
		if results[i].Written {
			// Fixed profile observations test counter propagation, not proof.
			results[i].Stats = SessionEntryWriteStats{
				HashMatches:              1,
				AnnotationTargetsCarried: 2,
			}
		}
	}
	return results
}

func cloneParallelIndexState(state *SessionIndexState) *SessionIndexState {
	if state == nil {
		return nil
	}
	copy := *state
	if state.ArtifactHash != nil {
		value := *state.ArtifactHash
		copy.ArtifactHash = &value
	}
	if state.IndexedInputHash != nil {
		value := *state.IndexedInputHash
		copy.IndexedInputHash = &value
	}
	if state.SessionEntriesHash != nil {
		value := *state.SessionEntriesHash
		copy.SessionEntriesHash = &value
	}
	if state.IndexVersion != nil {
		value := *state.IndexVersion
		copy.IndexVersion = &value
	}
	if state.IndexedAt != nil {
		value := *state.IndexedAt
		copy.IndexedAt = &value
	}
	return &copy
}

func recordMax(max *atomic.Int64, value int64) {
	for {
		current := max.Load()
		if value <= current || max.CompareAndSwap(current, value) {
			return
		}
	}
}

func waitForActiveParses(t *testing.T, indexer *blockingParallelIndexer, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if indexer.maxActive.Load() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("max active parses = %d, want %d", indexer.maxActive.Load(), want)
}

func waitForIndexProgress(t *testing.T, progress *ProgressState, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if progress.Snapshot()[StageIndex].Done >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("INDEX progress done = %d, want at least %d", progress.Snapshot()[StageIndex].Done, want)
}

func buildIndexParallelMetas(t *testing.T, fixture indexParallelFixture) ([]indexedMeta, map[SessionID][]schema.SessionEntry) {
	t.Helper()
	metas := make([]indexedMeta, 0, len(fixture.Sessions))
	entries := make(map[SessionID][]schema.SessionEntry, len(fixture.Sessions))
	for i, sessionFixture := range fixture.Sessions {
		sessionID, err := NewSessionID(sessionFixture.ID)
		if err != nil {
			t.Fatalf("fixture session %q has invalid ID: %v", sessionFixture.Name, err)
		}
		preview := sessionFixture.Preview
		entries[sessionID] = []schema.SessionEntry{{SessionID: sessionID, EntryIndex: i, Harness: schema.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &preview}}
		metas = append(metas, indexedMeta{
			session: DiscoveredSession{
				SessionID:    sessionID,
				Harness:      HarnessClaudeCode,
				SourcePath:   ResolvedPath("/source/" + sessionFixture.ID + ".jsonl"),
				SourceFormat: SourceFormatJSONL,
			},
			outputTranscriptPath: "/stored/" + sessionFixture.ID + ".jsonl",
			transcriptData:       []byte(sessionFixture.Preview),
		})
	}
	return metas, entries
}

// Prepare actual owned files and a validated artifact-only mirror. Parser input
// proof is deliberately absent until the production capture/write path runs.
func prepareIndexParallelInputs(t testing.TB, pipeline *Pipeline, metas []indexedMeta) {
	t.Helper()
	pipeline.fs = &OSFileSystem{}
	pipeline.config.OutputDir = ResolvedPath(t.TempDir())
	mirror := pipeline.metricsStore.(ArtifactMirrorStore)
	for i := range metas {
		im := &metas[i]
		directory := SessionDir(string(pipeline.config.OutputDir), "index-parallel", string(im.session.SessionID), "")
		im.outputTranscriptPath = filepath.Join(directory, string(im.session.SessionID)+"--transcript.jsonl")
		metadataPath := SessionMetadataPath(string(pipeline.config.OutputDir), "index-parallel", string(im.session.SessionID), "")
		metadata, err := json.Marshal(indexWorkerResult(*im).meta)
		if err != nil {
			t.Fatal(err)
		}
		artifact, err := NewManagedArtifact(metadata, im.transcriptData)
		if err != nil {
			t.Fatal(err)
		}
		// Install the saved pair by writing its files, then record the row with
		// the mirror, exactly as the write path does: transcript then metadata.
		if err := pipeline.fs.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := pipeline.fs.WriteFile(im.outputTranscriptPath, im.transcriptData, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := pipeline.fs.WriteFile(metadataPath, metadata, 0o600); err != nil {
			t.Fatal(err)
		}
		results := mirror.MirrorArtifacts(t.Context(), []ArtifactMirrorRequest{{Artifact: artifact}})
		if len(results) != 1 || results[0].Err != nil || !results[0].Mirrored {
			t.Fatalf("fixture mirror did not record the saved pair: %+v", results)
		}
		state, err := pipeline.metricsStore.(SessionIndexStateReader).ReadIndexState(t.Context(), im.session.SessionID)
		if err != nil || state == nil || state.ArtifactHash == nil || *state.ArtifactHash != artifact.ArtifactHash || state.IndexedInputHash != nil || state.IndexerVersion != 0 {
			t.Fatalf("fixture publication did not establish artifact-only state: %+v %v", state, err)
		}
	}
}

func indexWorkerResult(im indexedMeta) workerResult {
	meta := NewUnifiedMetadata()
	meta.SessionID = im.session.SessionID
	meta.ModelHarness = im.session.Harness
	meta.HostSlug = "index-parallel"
	meta.ContentHash = schema.ComputeTranscriptHash(im.transcriptData)
	meta.Source = SourceInfo{FilePath: im.session.SourcePath.String(), Format: im.session.SourceFormat}
	return workerResult{
		result: SessionResult{
			SessionID:  im.session.SessionID,
			Harness:    im.session.Harness,
			ParentUUID: im.session.ParentUUID,
			Status:     DiffNew,
			OutputPath: filepath.Dir(im.outputTranscriptPath),
		},
		meta:                 &meta,
		outputTranscriptPath: im.outputTranscriptPath,
		transcriptData:       append([]byte(nil), im.transcriptData...),
		startMs:              im.startMs,
	}
}

func loadIndexParallelFixture(t *testing.T) indexParallelFixture {
	t.Helper()
	return decodeIndexParallelFixture(t, indexParallelYAML, "index parallel fixture")
}

func loadStreamComputeAnnotateFixture(t *testing.T) indexParallelFixture {
	t.Helper()
	return decodeIndexParallelFixture(t, streamComputeAnnotateYAML, "stream compute annotate fixture")
}

func decodeIndexParallelFixture(t *testing.T, data []byte, label string) indexParallelFixture {
	t.Helper()
	var fixture indexParallelFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode %s: %v", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found another YAML document")
		}
		t.Fatalf("%s must contain exactly one document: %v", label, err)
	}
	present := make(map[string]bool, len(fixture.Sessions))
	for _, session := range fixture.Sessions {
		if strings.TrimSpace(session.Name) == "" {
			t.Fatalf("%s has an unnamed session", label)
		}
		present[session.Name] = true
	}
	for _, required := range fixture.RequiredSessions {
		if !present[required] {
			t.Fatalf("%s is missing required session %q", label, required)
		}
	}
	return fixture
}

func BenchmarkIndexLoopParallelParse(b *testing.B) {
	const sessionCount = 256
	metas := make([]indexedMeta, 0, sessionCount)
	entries := make(map[SessionID][]schema.SessionEntry, sessionCount)
	for i := range sessionCount {
		sessionID, err := NewSessionID(fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1))
		if err != nil {
			b.Fatalf("NewSessionID: %v", err)
		}
		preview := fmt.Sprintf("entry %d", i)
		entries[sessionID] = []schema.SessionEntry{{SessionID: sessionID, EntryIndex: 0, Harness: schema.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &preview}}
		metas = append(metas, indexedMeta{session: DiscoveredSession{SessionID: sessionID, Harness: HarnessClaudeCode, SourcePath: ResolvedPath("/stored/session.jsonl"), SourceFormat: SourceFormatJSONL}, transcriptData: []byte(strings.Repeat(preview, 32))})
	}
	for _, workers := range []int{1, max(2, runtime.NumCPU())} {
		b.Run(fmt.Sprintf("workers-%d", workers), func(b *testing.B) {
			pipeline := &Pipeline{config: PipelineConfig{Parallelism: workers, Force: true}, indexers: map[Harness]TranscriptIndexer{HarnessClaudeCode: &cpuIndexBenchmarkIndexer{entries: entries}}, metricsStore: &serialIndexStore{entries: make(map[SessionID][]schema.SessionEntry)}}
			prepareIndexParallelInputs(b, pipeline, metas)
			b.ResetTimer()
			for range b.N {
				indexCh := make(chan streamedIndexWork, len(metas))
				indexDoneCh := make(chan DrainBatch, 1)
				completion := newIndexBatchCompletion(DrainBatch{Metas: metas}, len(metas))
				for _, im := range metas {
					indexCh <- streamedIndexWork{meta: im, batch: completion}
				}
				close(indexCh)
				pipeline.indexLoop(context.Background(), indexCh, indexDoneCh, nil, IndexOutcomeIndexed, "benchmark", nil, nil)
			}
		})
	}
}

type cpuIndexBenchmarkIndexer struct {
	entries map[SessionID][]schema.SessionEntry
}

func (*cpuIndexBenchmarkIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }
func (idx *cpuIndexBenchmarkIndexer) IndexTranscript(ctx context.Context, session DiscoveredSession) ([]schema.SessionEntry, error) {
	return idx.IndexTranscriptBytes(ctx, session, nil)
}
func (idx *cpuIndexBenchmarkIndexer) IndexTranscriptBytes(ctx context.Context, session DiscoveredSession, data []byte) ([]schema.SessionEntry, error) {
	hash := sha256.Sum256(data)
	for range 1024 {
		hash = sha256.Sum256(hash[:])
	}
	if hash[0] == 255 && len(data) == 0 {
		return nil, fmt.Errorf("unreachable")
	}
	return idx.entries[session.SessionID], nil
}
