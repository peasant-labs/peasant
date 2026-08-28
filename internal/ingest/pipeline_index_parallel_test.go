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

type indexParallelFixture struct {
	RequiredSessions []string               `yaml:"required_sessions"`
	Sessions         []indexParallelSession `yaml:"sessions"`
}

type indexParallelSession struct {
	Name    string `yaml:"name"`
	ID      string `yaml:"id"`
	Preview string `yaml:"preview"`
}

func TestIndexLoop_ParallelParseKeepsArenaUntilBatchDone(t *testing.T) {
	fixture := loadIndexParallelFixture(t)
	releaseParses := make(chan struct{})
	indexer := &blockingParallelIndexer{entries: make(map[SessionID][]schema.SessionEntry), release: releaseParses}
	metas := make([]indexedMeta, 0, len(fixture.Sessions))
	for i, sessionFixture := range fixture.Sessions {
		sessionID, err := NewSessionID(sessionFixture.ID)
		if err != nil {
			t.Fatalf("fixture session %q has invalid ID: %v", sessionFixture.Name, err)
		}
		preview := sessionFixture.Preview
		indexer.entries[sessionID] = []schema.SessionEntry{{SessionID: sessionID, EntryIndex: i, Harness: schema.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &preview}}
		metas = append(metas, indexedMeta{session: DiscoveredSession{SessionID: sessionID, Harness: HarnessClaudeCode, SourcePath: ResolvedPath("/stored/" + sessionFixture.ID + ".jsonl"), SourceFormat: SourceFormatJSONL}, transcriptData: []byte(sessionFixture.Preview)})
	}
	store := &serialIndexStore{entries: make(map[SessionID][]schema.SessionEntry)}
	pipeline := &Pipeline{config: PipelineConfig{Parallelism: 2}, indexers: map[Harness]TranscriptIndexer{HarnessClaudeCode: indexer}, metricsStore: store}
	progress := NewProgressState()
	progress.Update(ProgressEvent{Kind: KindStart, Stage: StageIndex, Total: len(metas)})
	indexCh := make(chan DrainBatch, 1)
	indexDoneCh := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		pipeline.indexLoop(context.Background(), indexCh, indexDoneCh, progress, IndexOutcomeIndexed, "test")
		close(done)
	}()

	indexCh <- DrainBatch{Metas: metas}
	close(indexCh)
	waitForActiveParses(t, indexer, int64(len(metas)))
	select {
	case <-indexDoneCh:
		t.Fatal("indexLoop signalled batch completion before parse workers released arena-backed data")
	default:
	}
	close(releaseParses)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("indexLoop did not finish")
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

type serialIndexStore struct {
	MetricsStore
	mu        sync.Mutex
	entries   map[SessionID][]schema.SessionEntry
	active    atomic.Int64
	maxActive atomic.Int64
}

func (store *serialIndexStore) IndexSessionEntries(_ context.Context, sessionID SessionID, entries []schema.SessionEntry) error {
	active := store.active.Add(1)
	defer store.active.Add(-1)
	recordMax(&store.maxActive, active)
	store.mu.Lock()
	defer store.mu.Unlock()
	store.entries[sessionID] = append([]schema.SessionEntry(nil), entries...)
	return nil
}

func (*serialIndexStore) UpdateIndexState(context.Context, SessionID, int, int64) error { return nil }

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

func loadIndexParallelFixture(t *testing.T) indexParallelFixture {
	t.Helper()
	var fixture indexParallelFixture
	decoder := yaml.NewDecoder(bytes.NewReader(indexParallelYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode index parallel fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found another YAML document")
		}
		t.Fatalf("index parallel fixture must contain exactly one document: %v", err)
	}
	present := make(map[string]bool, len(fixture.Sessions))
	for _, session := range fixture.Sessions {
		if strings.TrimSpace(session.Name) == "" {
			t.Fatal("index parallel fixture has an unnamed session")
		}
		present[session.Name] = true
	}
	for _, required := range fixture.RequiredSessions {
		if !present[required] {
			t.Fatalf("index parallel fixture is missing required session %q", required)
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
				indexCh := make(chan DrainBatch, 1)
				indexDoneCh := make(chan struct{}, 1)
				indexCh <- DrainBatch{Metas: metas}
				close(indexCh)
				pipeline.indexLoop(context.Background(), indexCh, indexDoneCh, nil, IndexOutcomeIndexed, "benchmark")
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
