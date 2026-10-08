package ingest

import (
	"fmt"
	"log/slog"
)

// WorkerBuffers is the run's pre-allocated worker pool: one byte buffer per
// worker, partitioned so no two workers share a buffer. The pool is allocated
// once, before any worker starts, so the hot path allocates nothing per item;
// each buffer is capped at the configured per-worker cap and the pool refuses
// a total that would exceed the configured staged-memory cap. A buffer never
// grows: a unit above the cap bypasses the pool — the caller queues it as a
// prepared object outside the pool buffers, under the total staged-memory
// cap — and its session stages alone under the byte budget.
//
// The pool stages exist today in the index worker pools (the streaming parser
// pool and the stale-session waves), which claim their partition per item and
// reset it before use. The serialize/hash scratch use arrives with the
// prepare lane; until then the claim establishes the partitioning discipline
// and enforces the caps.
type WorkerBuffers struct {
	buffers   [][]byte
	perBuffer int64
	total     int64
}

// NewWorkerBuffers allocates one zero-length, full-capacity buffer per
// worker. It refuses a non-positive worker count, an unusable config, and a
// pool whose buffers would not fit under the total staged-memory cap — every
// refusal names the numbers and the fix. workers is the run's effective worker
// count; size it with parallelWorkers, as the call sites do.
func NewWorkerBuffers(cfg WriteConfig, workers int) (*WorkerBuffers, error) {
	if workers < 1 {
		return nil, fmt.Errorf("ingest: worker buffers need at least 1 worker, got %d; size the pool with the run's effective worker count (parallelWorkers) so every worker owns a partition", workers)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("ingest: worker buffers refuse an unusable write config: %w", err)
	}
	if err := cfg.checkCapacities(workers); err != nil {
		return nil, err
	}
	buffers := make([][]byte, workers)
	for i := range buffers {
		buffers[i] = make([]byte, 0, cfg.BufferBytes)
	}
	return &WorkerBuffers{buffers: buffers, perBuffer: cfg.BufferBytes, total: cfg.StagedMemoryBytes}, nil
}

// Workers reports how many exclusive partitions the pool holds.
func (b *WorkerBuffers) Workers() int { return len(b.buffers) }

// PerBufferBytes reports the per-buffer cap: no buffer ever holds more.
func (b *WorkerBuffers) PerBufferBytes() int64 { return b.perBuffer }

// StagedMemoryBytes reports the total staged-memory cap the pool was checked
// against.
func (b *WorkerBuffers) StagedMemoryBytes() int64 { return b.total }

// ReservedBytes reports what the pool's buffers hold in reserve:
// workers × the per-buffer cap. The staged-memory gate asserts this stays
// under the total cap.
func (b *WorkerBuffers) ReservedBytes() int64 { return int64(len(b.buffers)) * b.perBuffer }

// Reset truncates worker's partition to zero length, keeping its capacity.
// Workers call it before each item, so no item ever reads another's scratch.
// An out-of-range worker is a programming error and panics.
func (b *WorkerBuffers) Reset(worker int) {
	b.buffers[b.checkWorker(worker)] = b.buffers[worker][:0]
}

// Append stages p in worker's partition and reports the partition's new
// contents. When p would carry the partition past its cap, Append reports
// false and changes nothing: the caller bypasses the pool and queues the
// unit as a prepared object outside it, under the total staged-memory cap.
// The partition never grows past its run-start capacity.
func (b *WorkerBuffers) Append(worker int, p []byte) ([]byte, bool) {
	slot := b.checkWorker(worker)
	if int64(len(b.buffers[slot])+len(p)) > b.perBuffer {
		return nil, false
	}
	b.buffers[slot] = append(b.buffers[slot], p...)
	return b.buffers[slot], true
}

// Bytes returns worker's partition contents: what its item staged so far.
func (b *WorkerBuffers) Bytes(worker int) []byte {
	return b.buffers[b.checkWorker(worker)]
}

func (b *WorkerBuffers) checkWorker(worker int) int {
	if worker < 0 || worker >= len(b.buffers) {
		panic(fmt.Sprintf("ingest: worker %d owns no buffer in a pool of %d: partitions are one per worker, claimed by worker index", worker, len(b.buffers)))
	}
	return worker
}

// checkCapacities refuses a pool whose buffers would not fit under the total
// staged-memory cap at the given worker count. Config load cannot run it:
// the run's effective worker count is not known there, so load checks only
// the structural minimum (room for the in-flight batches plus one buffer)
// and construction (NewPipeline) runs this exact check at the effective
// count. Stage time therefore never fails it.
func (w WriteConfig) checkCapacities(workers int) error {
	reserved := int64(workers) * w.BufferBytes
	if reserved > w.StagedMemoryBytes {
		return fmt.Errorf("ingest: %d worker buffers of %d bytes reserve %d bytes, past the staged-memory cap of %d bytes, so the pool alone would breach the total before any object stages; raise write.stagedMemoryBytes to %d or more, lower write.bufferBytes, or run with fewer workers",
			workers, w.BufferBytes, reserved, w.StagedMemoryBytes, reserved)
	}
	return nil
}

// newRunWorkerBuffers allocates the run's worker pool at the given worker
// count: the single home of run-start buffer allocation, so every pool stage
// pre-allocates the same way. The trust boundary guarantees the inputs:
// config load and NewPipeline validate the knobs and check the capacities at
// the effective worker count, so a stage-time failure is unreachable through
// production. A literal-built pipeline carrying an unusable config still runs
// bounded: it falls back to the shipped defaults loudly instead of running
// uncapped or refusing mid-run.
func (p *Pipeline) newRunWorkerBuffers(workers int) *WorkerBuffers {
	cfg, err := p.config.Write.validatedWithDefaults(workers)
	if err != nil {
		slog.Warn("pipeline: unusable write config, running the shipped write budgets",
			"error", err,
			"what", "the pipeline was built with write budgets no worker pool can honor",
			"why", "a hand-built config bypassed the load-time and construction-time validation",
			"user_impact", "this run enforces the shipped budgets instead of the configured ones",
			"how_to_fix", "build the pipeline through NewPipeline or fix the write.* section")
		cfg = DefaultWriteConfig(workers)
	}
	if err := cfg.checkCapacities(workers); err != nil {
		slog.Warn("pipeline: worker buffers exceed the staged-memory cap, running the shipped write budgets",
			"error", err,
			"what", "the configured worker buffers would breach the total staged-memory cap",
			"why", "a hand-built config bypassed the construction-time capacity check",
			"user_impact", "this run enforces the shipped budgets instead of the configured ones",
			"how_to_fix", "build the pipeline through NewPipeline or raise write.stagedMemoryBytes")
		cfg = DefaultWriteConfig(workers)
	}
	bufs, err := NewWorkerBuffers(cfg, workers)
	if err != nil {
		// Unreachable: cfg is validated and capacity-checked above, and the
		// shipped defaults satisfy both by construction.
		bufs, _ = NewWorkerBuffers(DefaultWriteConfig(workers), workers)
	}
	return bufs
}
