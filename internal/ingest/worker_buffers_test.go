package ingest

import (
	"io"
	"log/slog"
	"runtime"
	"sync"
	"testing"
)

// TestNewWorkerBuffersPreallocatedPartitioned holds the run-start allocation:
// one zero-length, full-capacity buffer per worker, pairwise disjoint, each
// capped at the per-worker cap and the pool checked against the total cap.
func TestNewWorkerBuffersPreallocatedPartitioned(t *testing.T) {
	t.Parallel()
	const workers = 4
	cfg := DefaultWriteConfig(workers)
	bufs, err := NewWorkerBuffers(cfg, workers)
	if err != nil {
		t.Fatal(err)
	}
	if bufs.Workers() != workers {
		t.Fatalf("Workers()=%d, want one buffer per worker (%d)", bufs.Workers(), workers)
	}
	if bufs.PerBufferBytes() != cfg.BufferBytes {
		t.Fatalf("PerBufferBytes()=%d, want the configured cap %d", bufs.PerBufferBytes(), cfg.BufferBytes)
	}
	if bufs.StagedMemoryBytes() != cfg.StagedMemoryBytes {
		t.Fatalf("StagedMemoryBytes()=%d, want the configured total %d", bufs.StagedMemoryBytes(), cfg.StagedMemoryBytes)
	}
	for i := 0; i < workers; i++ {
		if got := len(bufs.Bytes(i)); got != 0 {
			t.Fatalf("buffer %d starts with length %d, want zero: pre-allocated means capacity, not contents", i, got)
		}
		if got := int64(cap(bufs.Bytes(i))); got != cfg.BufferBytes {
			t.Fatalf("buffer %d has capacity %d, want the full per-worker cap %d", i, got, cfg.BufferBytes)
		}
		if _, ok := bufs.Append(i, []byte{byte(i)}); !ok {
			t.Fatalf("buffer %d refuses a one-byte stage", i)
		}
	}
	// Pairwise disjoint: every partition holds exactly its own marker.
	for i := 0; i < workers; i++ {
		got := bufs.Bytes(i)
		if len(got) != 1 || got[0] != byte(i) {
			t.Fatalf("buffer %d holds %v, want only its own marker: partitions are mutually exclusive", i, got)
		}
	}
}

// TestWorkerBuffersNeverGrow stages up to exactly the cap, then refuses: the
// partition keeps its run-start capacity and the caller bypasses the pool for
// the oversized unit instead of growing into unbounded memory.
func TestWorkerBuffersNeverGrow(t *testing.T) {
	t.Parallel()
	cfg := DefaultWriteConfig(1)
	cfg.BufferBytes = 16
	cfg.StagedMemoryBytes = 2*cfg.BatchBytes + cfg.BufferBytes
	bufs, err := NewWorkerBuffers(cfg, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bufs.Append(0, make([]byte, 16)); !ok {
		t.Fatal("a unit exactly at the cap must stage")
	}
	if got := len(bufs.Bytes(0)); got != 16 {
		t.Fatalf("staged %d bytes, want 16", got)
	}
	if _, ok := bufs.Append(0, []byte{0}); ok {
		t.Fatal("a unit past the cap must bypass, not grow the buffer")
	}
	if got := len(bufs.Bytes(0)); got != 16 {
		t.Fatalf("a refused append changed the partition to %d bytes", got)
	}
	if got := int64(cap(bufs.Bytes(0))); got != 16 {
		t.Fatalf("the partition capacity moved to %d, want the run-start 16", got)
	}
	// Reset truncates without freeing: the next item starts clean on the same
	// backing.
	bufs.Reset(0)
	if got := len(bufs.Bytes(0)); got != 0 {
		t.Fatalf("Reset left %d bytes, want zero length", got)
	}
	if got := int64(cap(bufs.Bytes(0))); got != 16 {
		t.Fatalf("Reset changed the capacity to %d, want the run-start 16", got)
	}
}

// TestWorkerBuffersConcurrentExclusivity runs the claim discipline the pool
// stages use — reset, then stage a per-worker marker — across goroutines under
// the race detector: partitions stay mutually exclusive.
func TestWorkerBuffersConcurrentExclusivity(t *testing.T) {
	t.Parallel()
	const workers = 8
	const rounds = 50
	bufs, err := NewWorkerBuffers(DefaultWriteConfig(workers), workers)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				bufs.Reset(w)
				marker := []byte{byte(w), byte(r)}
				if _, ok := bufs.Append(w, marker); !ok {
					t.Errorf("worker %d round %d: a two-byte stage must fit the 4 MiB partition", w, r)
					return
				}
				got := bufs.Bytes(w)
				if len(got) != 2 || got[0] != byte(w) || got[1] != byte(r) {
					t.Errorf("worker %d round %d: partition holds %v, want only its own marker", w, r, got)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestRunWorkerBuffersStagedMemoryUnderCap asserts the total gate across the
// worker counts a run can take: what the pool reserves never exceeds the
// derived total cap.
func TestRunWorkerBuffersStagedMemoryUnderCap(t *testing.T) {
	t.Parallel()
	for _, workers := range []int{1, 2, 7, 8, 32} {
		bufs, err := NewWorkerBuffers(DefaultWriteConfig(workers), workers)
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		if bufs.ReservedBytes() > bufs.StagedMemoryBytes() {
			t.Fatalf("workers=%d: reserved %d bytes past the staged cap %d", workers, bufs.ReservedBytes(), bufs.StagedMemoryBytes())
		}
	}
}

// TestNewWorkerBuffersRefusesZeroWorkers fails the allocation, not the run:
// a pool with no partition per worker cannot honor exclusivity.
func TestNewWorkerBuffersRefusesZeroWorkers(t *testing.T) {
	t.Parallel()
	if _, err := NewWorkerBuffers(DefaultWriteConfig(1), 0); err == nil {
		t.Fatal("zero workers must refuse: every worker owns a partition")
	}
}

// TestNewWorkerBuffersRefusesUnusableConfig carries the knob validation into
// the pool: a negative byte cap fails before any buffer exists.
func TestNewWorkerBuffersRefusesUnusableConfig(t *testing.T) {
	t.Parallel()
	cfg := DefaultWriteConfig(2)
	cfg.BufferBytes = -1
	if _, err := NewWorkerBuffers(cfg, 2); err == nil {
		t.Fatal("an unusable write config must refuse the pool")
	}
}

// TestNewWorkerBuffersRefusesInconsistentCaps fails when the buffers alone
// would breach the total: two 4 MiB buffers do not fit under a single
// buffer's worth of staged memory.
func TestNewWorkerBuffersRefusesInconsistentCaps(t *testing.T) {
	t.Parallel()
	cfg := DefaultWriteConfig(2)
	cfg.StagedMemoryBytes = cfg.BufferBytes
	if _, err := NewWorkerBuffers(cfg, 2); err == nil {
		t.Fatal("buffers reserving past the staged-memory cap must refuse")
	}
}

// TestNewRunWorkerBuffersSizesByEffectiveWorkers holds run-start allocation:
// the pool is sized by the pipeline's effective worker count, explicit or
// auto.
func TestNewRunWorkerBuffersSizesByEffectiveWorkers(t *testing.T) {
	t.Parallel()
	explicit := &Pipeline{config: PipelineConfig{Parallelism: 3}}
	if got := explicit.newRunWorkerBuffers(3).Workers(); got != 3 {
		t.Fatalf("explicit workers: pool holds %d, want 3", got)
	}
	auto := &Pipeline{config: PipelineConfig{}}
	if got := auto.newRunWorkerBuffers(parallelWorkers(auto.config)).Workers(); got != runtime.NumCPU() {
		t.Fatalf("auto workers: pool holds %d, want runtime.NumCPU()=%d", got, runtime.NumCPU())
	}
}

// TestNewRunWorkerBuffersFallsBackToDefaults keeps a literal-built pipeline
// with an unusable config bounded: the run enforces the shipped budgets
// instead of running uncapped.
func TestNewRunWorkerBuffersFallsBackToDefaults(t *testing.T) {
	t.Parallel()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	defer slog.SetDefault(slog.Default())
	slog.SetDefault(quiet)
	p := &Pipeline{config: PipelineConfig{Write: WriteConfig{BatchBytes: -1}}}
	bufs := p.newRunWorkerBuffers(2)
	if bufs == nil {
		t.Fatal("the fallback must still hand the run a pool")
	}
	if bufs.PerBufferBytes() != DefaultWriteConfig(2).BufferBytes {
		t.Fatalf("PerBufferBytes()=%d, want the shipped default %d", bufs.PerBufferBytes(), DefaultWriteConfig(2).BufferBytes)
	}
}

// TestWorkerBufferCheckPanicsOutOfRange pins the contract: partitions are
// claimed by worker index, and anything else is a programming error.
func TestWorkerBufferCheckPanicsOutOfRange(t *testing.T) {
	t.Parallel()
	bufs, err := NewWorkerBuffers(DefaultWriteConfig(2), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("claiming a partition with no worker must panic")
		}
	}()
	bufs.Reset(2)
}
