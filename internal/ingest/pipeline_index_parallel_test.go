package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
		drainDone <- pipeline.drainLoop(context.Background(), staging, &workersDone, indexCh, indexDoneCh, make(chan error, 1), progress, len(metas))
		close(indexCh)
	}()
	indexDone := make(chan struct{})
	go func() {
		pipeline.indexLoop(context.Background(), indexCh, indexDoneCh, progress, IndexOutcomeIndexed, "test", nil)
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
	progress := NewProgressState()
	progress.Update(ProgressEvent{Kind: KindStart, Stage: StageIndex, Total: len(metas)})
	indexCh := make(chan streamedIndexWork, len(metas))
	indexDoneCh := make(chan DrainBatch, 1)
	done := make(chan struct{})
	go func() {
		pipeline.indexLoop(context.Background(), indexCh, indexDoneCh, progress, IndexOutcomeIndexed, "test", nil)
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
	indexCh := make(chan streamedIndexWork, len(metas))
	indexDoneCh := make(chan DrainBatch, 1)
	done := make(chan struct{})
	go func() {
		pipeline.indexLoop(context.Background(), indexCh, indexDoneCh, nil, IndexOutcomeIndexed, "test", nil)
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
	}
}

func TestIndexBatch_UsesStoreBatchWriterAndProfilesWriteShape(t *testing.T) {
	fixture := loadIndexParallelFixture(t)
	metas, entries := buildIndexParallelMetas(t, fixture)
	profiler := &IndexProfiler{}
	store := &batchIndexStore{entries: make(map[SessionID][]schema.SessionEntry)}
	pipeline := &Pipeline{
		config:       PipelineConfig{Parallelism: 1, IndexProfiler: profiler},
		indexers:     map[Harness]TranscriptIndexer{HarnessClaudeCode: &immediateIndexer{entries: entries}},
		metricsStore: store,
	}

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
	if len(store.batchSizes) != 1 || store.batchSizes[0] != len(metas) {
		t.Fatalf("batch sizes = %v, want one batch of %d", store.batchSizes, len(metas))
	}
	snapshot := profiler.Snapshot()
	if len(snapshot.Batches) != 1 {
		t.Fatalf("profile batches = %d, want 1", len(snapshot.Batches))
	}
	batch := snapshot.Batches[0]
	if batch.WriteTxs != 1 || batch.WriteSavepoints != len(metas) {
		t.Fatalf("profile write shape = %d txs, %d savepoints; want 1 tx and %d savepoints", batch.WriteTxs, batch.WriteSavepoints, len(metas))
	}
	if batch.WriteStats.HashMatches != len(metas) || batch.WriteStats.AnnotationTargetsCarried != len(metas)*2 {
		t.Fatalf("profile write stats = %+v, want hash matches %d and annotation targets carried %d", batch.WriteStats, len(metas), len(metas)*2)
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
	classifier := &recordingStreamClassifier{annotated: make(chan SessionID, len(metas))}
	pipeline := &Pipeline{
		config:       PipelineConfig{Parallelism: 1},
		indexers:     map[Harness]TranscriptIndexer{HarnessClaudeCode: &immediateIndexer{entries: entries}},
		metricsStore: store,
		analyzer:     analyzer,
		classifier:   classifier,
	}
	progress := NewProgressState()
	progress.Update(ProgressEvent{Kind: KindStart, Stage: StageIndex, Total: len(metas)})
	indexCh := make(chan streamedIndexWork, len(metas))
	indexDoneCh := make(chan DrainBatch, 1)
	downstreamCh := make(chan indexedMeta, len(metas))
	downstreamDone := make(chan streamedDownstreamResult, 1)
	go func() {
		downstreamDone <- pipeline.runStreamedDownstream(context.Background(), downstreamCh, progress, len(metas), "test")
	}()
	indexDone := make(chan struct{})
	go func() {
		pipeline.indexLoop(context.Background(), indexCh, indexDoneCh, progress, IndexOutcomeIndexed, "test", downstreamCh)
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
	case got := <-classifier.annotated:
		if got != metas[0].session.SessionID {
			t.Fatalf("first streamed ANNOTATE session = %s, want %s", got, metas[0].session.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ANNOTATE did not start after COMPUTE finished for the first indexed session")
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

func TestStreamedDownstream_StopsSafelyAfterCancellation(t *testing.T) {
	fixture := loadStreamComputeAnnotateFixture(t)
	metas, _ := buildIndexParallelMetas(t, fixture)
	ctx, cancel := context.WithCancel(context.Background())
	analyzer := &cancelingStreamAnalyzer{cancel: cancel, started: make(chan SessionID, len(metas))}
	classifier := &recordingStreamClassifier{annotated: make(chan SessionID, len(metas))}
	pipeline := &Pipeline{
		config:     PipelineConfig{Parallelism: 1},
		analyzer:   analyzer,
		classifier: classifier,
	}
	progress := NewProgressState()
	downstreamCh := make(chan indexedMeta, len(metas))
	downstreamCh <- indexedMeta{session: metas[0].session, startMs: metas[0].startMs, indexed: true}
	close(downstreamCh)

	result := pipeline.runStreamedDownstream(ctx, downstreamCh, progress, len(metas), "test")
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
	classifier := &recordingStreamClassifier{annotated: make(chan SessionID, len(metas))}
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

	result := pipeline.runStreamedDownstream(context.Background(), downstreamCh, progress, len(metas), "test")
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
		case got := <-classifier.annotated:
			if got != im.session.SessionID {
				t.Fatalf("annotated session = %s, want %s", got, im.session.SessionID)
			}
		default:
			t.Fatalf("missing annotation call for %s", im.session.SessionID)
		}
	}
}

func TestStreamedDownstream_AnnotateErrorAdvancesBestEffortProgress(t *testing.T) {
	fixture := loadStreamComputeAnnotateFixture(t)
	metas, _ := buildIndexParallelMetas(t, fixture)
	analyzer := &recordingStreamAnalyzer{computeStarted: make(chan SessionID, len(metas)), computeDone: make(chan SessionID, len(metas))}
	classifier := &erroringStreamClassifier{recordingStreamClassifier: recordingStreamClassifier{annotated: make(chan SessionID, len(metas))}, err: fmt.Errorf("annotate failed")}
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

	result := pipeline.runStreamedDownstream(context.Background(), downstreamCh, progress, len(metas), "test")
	if result.Computed != len(metas) || result.ComputeDone != len(metas) {
		t.Fatalf("compute result after annotate error = %+v, want %d computed", result, len(metas))
	}
	if result.AnnotateDone != len(metas) {
		t.Fatalf("annotate progress after annotate error = %d, want %d", result.AnnotateDone, len(metas))
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
	writeOrder []SessionID
	wrote      chan SessionID
	active     atomic.Int64
	maxActive  atomic.Int64
}

type blockingSecondIndexStore struct {
	serialIndexStore
	blocked SessionID
	release <-chan struct{}
}

func (store *blockingSecondIndexStore) IndexSessionEntries(ctx context.Context, sessionID SessionID, entries []schema.SessionEntry) error {
	if sessionID == store.blocked {
		select {
		case <-store.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return store.serialIndexStore.IndexSessionEntries(ctx, sessionID, entries)
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

type recordingStreamClassifier struct {
	annotated chan SessionID
}

func (c *recordingStreamClassifier) Annotate(_ context.Context, sessionID SessionID) error {
	c.annotated <- sessionID
	return nil
}

type erroringStreamClassifier struct {
	recordingStreamClassifier
	err error
}

func (c *erroringStreamClassifier) Annotate(_ context.Context, sessionID SessionID) error {
	c.annotated <- sessionID
	return c.err
}

func (store *serialIndexStore) IndexSessionEntries(_ context.Context, sessionID SessionID, entries []schema.SessionEntry) error {
	active := store.active.Add(1)
	defer store.active.Add(-1)
	recordMax(&store.maxActive, active)
	store.mu.Lock()
	store.entries[sessionID] = append([]schema.SessionEntry(nil), entries...)
	store.writeOrder = append(store.writeOrder, sessionID)
	store.mu.Unlock()
	if store.wrote != nil {
		store.wrote <- sessionID
	}
	return nil
}

func (*serialIndexStore) UpdateIndexState(context.Context, SessionID, int, int64) error { return nil }

type batchIndexStore struct {
	MetricsStore
	mu           sync.Mutex
	entries      map[SessionID][]schema.SessionEntry
	batchSizes   []int
	singleWrites atomic.Int64
}

func (store *batchIndexStore) IndexSessionEntries(_ context.Context, sessionID SessionID, entries []schema.SessionEntry) error {
	store.singleWrites.Add(1)
	store.mu.Lock()
	defer store.mu.Unlock()
	store.entries[sessionID] = append([]schema.SessionEntry(nil), entries...)
	return nil
}

func (store *batchIndexStore) IndexSessionEntryBatch(_ context.Context, writes []SessionEntryWrite) []SessionEntryWriteResult {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.batchSizes = append(store.batchSizes, len(writes))
	results := make([]SessionEntryWriteResult, len(writes))
	for i, write := range writes {
		store.entries[write.SessionID] = append([]schema.SessionEntry(nil), write.Entries...)
		results[i] = SessionEntryWriteResult{
			SessionID: write.SessionID,
			Written:   true,
			Stats: SessionEntryWriteStats{
				HashMatches:              1,
				AnnotationTargetsCarried: 2,
			},
		}
	}
	return results
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

func indexWorkerResult(im indexedMeta) workerResult {
	meta := NewUnifiedMetadata()
	meta.SessionID = im.session.SessionID
	meta.ModelHarness = im.session.Harness
	meta.Source = SourceInfo{FilePath: im.session.SourcePath.String(), Format: im.session.SourceFormat}
	return workerResult{
		result: SessionResult{
			SessionID:  im.session.SessionID,
			Harness:    im.session.Harness,
			ParentUUID: im.session.ParentUUID,
			Status:     DiffNew,
			OutputPath: "/stored/" + string(im.session.SessionID),
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
			pipeline := &Pipeline{config: PipelineConfig{Parallelism: workers}, indexers: map[Harness]TranscriptIndexer{HarnessClaudeCode: &cpuIndexBenchmarkIndexer{entries: entries}}, metricsStore: &benchmarkIndexStore{}}
			b.ResetTimer()
			for range b.N {
				indexCh := make(chan streamedIndexWork, len(metas))
				indexDoneCh := make(chan DrainBatch, 1)
				completion := newIndexBatchCompletion(DrainBatch{Metas: metas}, len(metas))
				for _, im := range metas {
					indexCh <- streamedIndexWork{meta: im, batch: completion}
				}
				close(indexCh)
				pipeline.indexLoop(context.Background(), indexCh, indexDoneCh, nil, IndexOutcomeIndexed, "benchmark", nil)
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

type benchmarkIndexStore struct{ MetricsStore }

func (*benchmarkIndexStore) IndexSessionEntries(context.Context, SessionID, []schema.SessionEntry) error {
	return nil
}

func (*benchmarkIndexStore) UpdateIndexState(context.Context, SessionID, int, int64) error {
	return nil
}
