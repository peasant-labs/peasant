package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// overlapGate holds each caller until `want` callers are inside at once, so a
// pass that serializes its per-session work never reaches the overlap. The wake
// source is the arrival of the want-th caller; the deadline only stops a serial
// pass from hanging, and then the recorded maximum stays at 1.
type overlapGate struct {
	want      int64
	active    atomic.Int64
	maxActive atomic.Int64
	once      sync.Once
	reached   chan struct{}
}

func newOverlapGate(want int64) *overlapGate {
	return &overlapGate{want: want, reached: make(chan struct{})}
}

func (g *overlapGate) enter() {
	active := g.active.Add(1)
	recordMax(&g.maxActive, active)
	if active >= g.want {
		g.once.Do(func() { close(g.reached) })
	}
	select {
	case <-g.reached:
	case <-time.After(2 * time.Second):
	}
	g.active.Add(-1)
}

// maintenanceStore answers the maintenance selection queries and counts the
// location reads, so the test sees whether locations were batched.
type maintenanceStore struct {
	MetricsStore
	SessionStore
	ids          []SessionID
	gate         *overlapGate
	bulkSizes    []int
	singleLookup atomic.Int64
	mu           sync.Mutex
}

func (s *maintenanceStore) ListStaleIndexSessions(context.Context, map[Harness]HarvesterVersions) ([]SessionID, error) {
	return s.ids, nil
}

func (s *maintenanceStore) ListStaleAdapterSessions(context.Context, map[Harness]HarvesterVersions, []int) ([]SessionID, error) {
	return s.ids, nil
}

func (s *maintenanceStore) ReadIndexState(_ context.Context, sid SessionID) (*SessionIndexState, error) {
	if s.gate != nil {
		s.gate.enter()
	}
	return &SessionIndexState{SessionID: sid, Harness: HarnessClaudeCode}, nil
}

func (s *maintenanceStore) BulkLookupSessionLocations(_ context.Context, ids []SessionID) (map[SessionID]SessionLocation, error) {
	s.mu.Lock()
	s.bulkSizes = append(s.bulkSizes, len(ids))
	s.mu.Unlock()
	out := make(map[SessionID]SessionLocation, len(ids))
	for _, sid := range ids {
		out[sid] = SessionLocation{HostSlug: "host", SchemaVersion: CurrentSchemaVersion}
	}
	return out, nil
}

func (s *maintenanceStore) LookupSessionLocation(context.Context, SessionID) (string, string, error) {
	s.singleLookup.Add(1)
	return "host", "", nil
}

// gatedReadFS holds metadata reads at the overlap gate and counts directory
// listings, which is how a whole-tree fallback scan would show up.
type gatedReadFS struct {
	*OSFileSystem
	gate     *overlapGate
	readDirs atomic.Int64
}

func (f *gatedReadFS) ReadFile(path string) ([]byte, error) {
	f.gate.enter()
	return f.OSFileSystem.ReadFile(path)
}

func (f *gatedReadFS) ReadDir(path string) ([]os.DirEntry, error) {
	f.readDirs.Add(1)
	return f.OSFileSystem.ReadDir(path)
}

func maintenanceIDs(n int) []SessionID {
	ids := make([]SessionID, n)
	for i := range ids {
		ids[i] = SessionID(fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
	}
	return ids
}

func maintenancePipeline(t *testing.T, filesystem FileSystem, store *maintenanceStore, workers int) *Pipeline {
	t.Helper()
	p := &Pipeline{
		fs:           filesystem,
		config:       PipelineConfig{OutputDir: ResolvedPath(t.TempDir()), Parallelism: workers},
		metricsStore: store,
		store:        store,
	}
	p.resolvedVersions = p.resolveVersionTargets()
	return p
}

// The pair-repair selection decides its candidates on the bounded pool, reads
// their locations with one bulk query, and still appends repairs in candidate
// order.
func TestAppendPairRepairWorkParallelPreservesOrder(t *testing.T) {
	const workers = 4
	ids := maintenanceIDs(16)
	store := &maintenanceStore{ids: ids, gate: newOverlapGate(workers)}
	source := filepath.Join(t.TempDir(), "source.jsonl")
	if err := os.WriteFile(source, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	discovered := make([]DiscoveredSession, len(ids))
	for i, sid := range ids {
		discovered[i] = DiscoveredSession{SessionID: sid, Harness: HarnessClaudeCode, SourcePath: ResolvedPath(source)}
	}
	p := maintenancePipeline(t, &OSFileSystem{}, store, workers)

	entries := p.appendPairRepairWork(context.Background(), nil, discovered)

	if len(entries) != len(ids) {
		t.Fatalf("repair entries = %d, want %d (every pair is missing)", len(entries), len(ids))
	}
	for i, entry := range entries {
		if entry.Session.SessionID != ids[i] || !entry.pairRepair {
			t.Fatalf("entry %d = %s (repair %v), want %s in candidate order", i, entry.Session.SessionID, entry.pairRepair, ids[i])
		}
	}
	if got := store.gate.maxActive.Load(); got < 2 {
		t.Fatalf("max concurrent index-state reads = %d, want > 1: per-session work is serialized", got)
	}
	if got := store.singleLookup.Load(); got != 0 {
		t.Fatalf("per-session location lookups = %d, want 0 (locations come from the bulk read)", got)
	}
	for _, size := range store.bulkSizes {
		if size == 1 {
			t.Fatalf("bulk location reads %v include a single-session read", store.bulkSizes)
		}
	}
}

// The stale-adapter pass reconstructs its candidates on the bounded pool, and
// a recorded location whose metadata file is gone yields no work without any
// directory listing of the saved tree.
func TestAppendStoredAdapterWorkParallelWithoutTreeScan(t *testing.T) {
	const workers = 4
	ids := maintenanceIDs(16)
	store := &maintenanceStore{ids: ids}
	filesystem := &gatedReadFS{OSFileSystem: &OSFileSystem{}, gate: newOverlapGate(workers)}
	p := maintenancePipeline(t, filesystem, store, workers)
	for _, name := range []string{"host", "other-host"} {
		if err := os.MkdirAll(filepath.Join(string(p.config.OutputDir), name), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	entries := p.appendStoredAdapterWork(context.Background(), nil, nil)

	if len(entries) != 0 {
		t.Fatalf("entries = %d, want 0 for sessions whose recorded metadata is missing", len(entries))
	}
	if got := filesystem.readDirs.Load(); got != 0 {
		t.Fatalf("directory listings = %d, want 0: a recorded location must not fall back to a tree scan", got)
	}
	if got := filesystem.gate.maxActive.Load(); got < 2 {
		t.Fatalf("max concurrent metadata reads = %d, want > 1: per-session work is serialized", got)
	}
	if got := store.singleLookup.Load(); got != 0 {
		t.Fatalf("per-session location lookups = %d, want 0 (locations come from the bulk read)", got)
	}
}
