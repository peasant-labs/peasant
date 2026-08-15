package ingest

import (
	"container/heap"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/peasant-labs/schema"
)

// ---------------------------------------------------------------------------
// Topo sort
// ---------------------------------------------------------------------------

// topoLevels partitions a flat slice of DiffEntries into topological levels.
// All entries in level 0 have no parent in the batch (roots). Entries in level
// N depend only on entries in levels < N, so each level can be processed
// concurrently after the previous level's DB writes are committed.
//
// The algorithm uses a simple iterative fixed-point that converges in
// O(depth) passes. Real transcript trees almost never exceed depth 1
// (root session + subagent children), so this is typically 2 passes.
//
// Cycles (invalid data) are broken by leaving the later-encountered node at
// its current level, so processing always terminates.
func topoLevels(entries []DiffEntry) [][]DiffEntry {
	if len(entries) == 0 {
		return nil
	}

	// sessionIndex maps each session ID to its position in entries.
	sessionIndex := make(map[SessionID]int, len(entries))
	for i, e := range entries {
		sessionIndex[e.Session.SessionID] = i
	}

	// level[i] = topo depth of entries[i].
	level := make([]int, len(entries))
	maxLevel := 0

	changed := true
	for changed {
		changed = false
		for i, e := range entries {
			if e.Session.ParentUUID == nil {
				continue // root — always level 0
			}
			parentIdx, ok := sessionIndex[*e.Session.ParentUUID]
			if !ok {
				continue // parent not in batch — treat as root
			}
			want := level[parentIdx] + 1
			if level[i] < want {
				level[i] = want
				if want > maxLevel {
					maxLevel = want
				}
				changed = true
			}
		}
	}

	// Bucket entries by level.
	levels := make([][]DiffEntry, maxLevel+1)
	for i, e := range entries {
		l := level[i]
		levels[l] = append(levels[l], e)
	}
	return levels
}

// ---------------------------------------------------------------------------
// Session size estimation (Phase 1 scan)
// ---------------------------------------------------------------------------

// sessionSize pairs a DiffEntry with its estimated work size.
// For Claude, size is the source file byte count (os.Stat).
// For OpenCode, size is the message-file count (ReadDir count).
// Larger size → higher priority → scheduled first in the worker pool.
type sessionSize struct {
	entry DiffEntry
	size  int64 // estimated work size; 0 = unknown (treated as smallest)
}

// ---------------------------------------------------------------------------
// Max-heap of sessionSize (largest size dequeued first)
// ---------------------------------------------------------------------------

// sessionSizeHeap is a max-heap of sessionSize ordered by size descending.
// Implements heap.Interface.
type sessionSizeHeap []sessionSize

func (h sessionSizeHeap) Len() int           { return len(h) }
func (h sessionSizeHeap) Less(i, j int) bool { return h[i].size > h[j].size } // max-heap
func (h sessionSizeHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *sessionSizeHeap) Push(x any)        { *h = append(*h, x.(sessionSize)) }
func (h *sessionSizeHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// buildSizeHeap wraps a slice of sessionSize values into a heap in O(n).
func buildSizeHeap(items []sessionSize) *sessionSizeHeap {
	h := sessionSizeHeap(items)
	heap.Init(&h)
	return &h
}

// drainHeap pops all items from the heap in priority order (largest first).
func drainHeap(h *sessionSizeHeap) []sessionSize {
	out := make([]sessionSize, 0, h.Len())
	for h.Len() > 0 {
		out = append(out, heap.Pop(h).(sessionSize))
	}
	return out
}

// ---------------------------------------------------------------------------
// Worker result
// ---------------------------------------------------------------------------

// workerResult is the output of a single processSession call.
type workerResult struct {
	result SessionResult
	meta   *UnifiedMetadata
	// transcriptData holds the already-read bytes for the in-memory index path.
	// Nil on error or skip.
	//
	// It is NOT nil for OpenCode, despite what this comment used to say. OpenCode
	// writes a single JSON session file, so the extract step reads it like any
	// other single-file harness - the bytes simply are not where OpenCode's
	// ENTRIES live, which is a directory of message and part files. The pipeline
	// used to choose its index call by whether these bytes were present, so
	// OpenCode took the bytes path and its indexer discarded them; see
	// originalRoot below and TranscriptSourceKind.
	transcriptData []byte
	// outputTranscriptPath is the on-disk path of the written copy.
	outputTranscriptPath string
	// originalRoot is the provider root a directory-based harness indexes from.
	//
	// It has to be carried explicitly. The session handed to the index step is
	// rebuilt from this result, and its source path is deliberately replaced with
	// outputTranscriptPath, so a root derived from that path points into the
	// output tree instead of at the provider.
	//
	// What dropping it actually costs, measured on a real run rather than assumed:
	// the drain loop indexes NOTHING for the session and records a skipped row, and
	// then the stale-index sweep re-indexes it in the same run and it ends with its
	// entries. So the session is not left empty - an earlier version of this comment
	// said it was. The cost is a wasted indexing pass and an index_log row that
	// reports a skip nobody can act on, on every ingest of every directory-based
	// session. Carrying the root removes both.
	originalRoot     ResolvedPath
	transcriptOrigin TranscriptOrigin
	startMs          int64
	// metaFilename and sessionDir are set by processSession so that drainLoop
	// can write metadata.json after DB INSERT (DB-as-SOT write order, v8+).
	// metaFilename is the bare filename (e.g. "{sessionId}--metadata.json").
	// sessionDir is the final on-disk session directory.
	metaFilename string
	sessionDir   string
}

// DrainBatch is the value returned by StagingBuffer.Drain. It bundles the
// drained results with the slot indices needed to free arena space via
// AckBatch. Multiple DrainBatch values may be outstanding simultaneously;
// each owns its own Claimed slice and does not share state with others.
//
// Metas is reserved for the consumer: after DB INSERT the pipeline
// populates Metas with per-session index metadata before calling AckBatch.
// Drain leaves Metas nil.
type DrainBatch struct {
	Results []workerResult
	Claimed []int         // slot indices (for AckBatch)
	Metas   []indexedMeta // populated by consumer after DB INSERT; nil on Drain
}

// ---------------------------------------------------------------------------
// Parallelism helpers
// ---------------------------------------------------------------------------

// parallelWorkers returns the effective worker count from a PipelineConfig.
// 0 means auto → runtime.NumCPU().
func parallelWorkers(cfg PipelineConfig) int {
	if cfg.Parallelism > 0 {
		return cfg.Parallelism
	}
	return runtime.NumCPU()
}

// ---------------------------------------------------------------------------
// Lock-free bounded MPMC ring buffer for SessionEntry (COMPUTE stage)
// ---------------------------------------------------------------------------

// ErrQueueFull is returned by TryPush when the queue is at capacity.
var ErrQueueFull = errors.New("ingest: session entry queue is full")

// ErrQueueClosed is returned by Push or Pop after Close has been called.
var ErrQueueClosed = errors.New("ingest: session entry queue is closed")

// entrySlot is one cell in the ring buffer. The sequence field is the sole
// synchronisation point; all other fields are written/read only when sequence
// is in a state owned by that side.
//
// Padding to 64 bytes (typical cache line) prevents false sharing between
// adjacent slots accessed by different goroutines simultaneously.
type entrySlot struct {
	sequence atomic.Uint64
	entry    schema.SessionEntry
	_        [40]byte // padding — SessionEntry is ~24 bytes; 64-24-8 = 32, round up
}

// SessionEntryQueue is a bounded, lock-free MPMC (multiple-producer,
// multiple-consumer) ring buffer of SessionEntry values. It is designed
// for the COMPUTE stage where multiple indexer goroutines push entries and
// multiple MetricFunc workers pop them concurrently.
//
// Capacity is rounded up to the next power of two at construction so that
// the modulo operation reduces to a bitwise AND. The ring buffer is
// statically allocated; no heap growth occurs after construction.
//
// Push spins (with runtime.Gosched) until a slot is free or the queue is
// closed. Pop spins until a slot is filled or the queue is closed and
// drained. Both operations are O(1) amortised.
//
// Lifecycle:
//
//	q := NewSessionEntryQueue(cap)
//	// producers: q.Push(entry)   — spins when full, returns ErrQueueClosed if closed
//	// consumers: q.Pop()         — spins when empty, returns (zero, ErrQueueClosed) when done
//	q.Close()                     // signals producers/consumers to stop
//
// Algorithm: Dmitry Vyukov's MPMC queue (public domain).
// https://www.1024cores.net/home/lock-free-algorithms/queues/bounded-mpmc-queue
type SessionEntryQueue struct {
	_      [64]byte      // head and tail on separate cache lines
	head   atomic.Uint64 // next Push position
	_      [56]byte      // pad to 64 bytes
	tail   atomic.Uint64 // next Pop position
	_      [56]byte      // pad to 64 bytes
	closed atomic.Bool   // set by Close; checked by Push/Pop
	mask   uint64        // cap-1 (cap is always power of two)
	slots  []entrySlot   // statically allocated ring
}

// nextPow2 returns the smallest power of two >= n that is also >= 2.
// Vyukov's MPMC algorithm requires capacity >= 2 because the "slot filled"
// sequence (pos+1) and the "slot recycled" sequence (pos+cap) must be
// distinct; with cap=1 they collide.
// Panics if n < 1.
func nextPow2(n int) int {
	if n < 1 {
		panic("ingest: SessionEntryQueue capacity must be >= 1")
	}
	v := 2 // minimum 2
	for v < n {
		v <<= 1
	}
	return v
}

// NewSessionEntryQueue creates a bounded lock-free SessionEntryQueue.
// capacity is rounded up to the next power of two (minimum 2); must be >= 1.
func NewSessionEntryQueue(capacity int) *SessionEntryQueue {
	cap := nextPow2(capacity)
	slots := make([]entrySlot, cap)
	for i := range slots {
		slots[i].sequence.Store(uint64(i))
	}
	return &SessionEntryQueue{
		mask:  uint64(cap - 1),
		slots: slots,
	}
}

// Push adds entry to the queue. It spins until space is available.
// Returns ErrQueueClosed if the queue has been closed.
func (q *SessionEntryQueue) Push(entry schema.SessionEntry) error {
	for {
		if q.closed.Load() {
			return ErrQueueClosed
		}
		pos := q.head.Load()
		slot := &q.slots[pos&q.mask]
		seq := slot.sequence.Load()
		diff := int64(seq) - int64(pos)
		switch {
		case diff == 0:
			// Slot is empty and owned by this position. Try to claim it.
			if q.head.CompareAndSwap(pos, pos+1) {
				slot.entry = entry
				slot.sequence.Store(pos + 1) // hand off to consumer
				return nil
			}
		case diff < 0:
			// Queue is full — slot not yet consumed.
			runtime.Gosched()
		default:
			// Another producer advanced head; retry with fresh load.
		}
	}
}

// TryPush adds entry without spinning. Returns ErrQueueFull if the queue
// is at capacity, ErrQueueClosed if the queue has been closed.
func (q *SessionEntryQueue) TryPush(entry schema.SessionEntry) error {
	if q.closed.Load() {
		return ErrQueueClosed
	}
	pos := q.head.Load()
	slot := &q.slots[pos&q.mask]
	seq := slot.sequence.Load()
	diff := int64(seq) - int64(pos)
	if diff < 0 {
		return ErrQueueFull
	}
	if diff == 0 && q.head.CompareAndSwap(pos, pos+1) {
		slot.entry = entry
		slot.sequence.Store(pos + 1)
		return nil
	}
	return ErrQueueFull
}

// Pop removes and returns the next entry from the queue. It spins until
// an entry is available. Returns (zero, ErrQueueClosed) when the queue is
// closed and drained.
func (q *SessionEntryQueue) Pop() (schema.SessionEntry, error) {
	for {
		pos := q.tail.Load()
		slot := &q.slots[pos&q.mask]
		seq := slot.sequence.Load()
		diff := int64(seq) - int64(pos+1)
		switch {
		case diff == 0:
			// Slot is filled and owned by this position. Try to claim it.
			if q.tail.CompareAndSwap(pos, pos+1) {
				entry := slot.entry
				slot.entry = schema.SessionEntry{}    // release reference
				slot.sequence.Store(pos + q.mask + 1) // recycle slot for producers
				return entry, nil
			}
		case diff < 0:
			// Queue is empty — no entry written yet.
			if q.closed.Load() {
				// Closed and empty: signal consumers to stop.
				return schema.SessionEntry{}, ErrQueueClosed
			}
			runtime.Gosched()
		default:
			// Another consumer advanced tail; retry.
		}
	}
}

// Len returns a snapshot of the current number of items in the queue.
// Due to the lock-free nature this value may be stale by the time it is read.
func (q *SessionEntryQueue) Len() int {
	head := int64(q.head.Load())
	tail := int64(q.tail.Load())
	n := head - tail
	if n < 0 {
		return 0
	}
	return int(n)
}

// Cap returns the actual capacity of the queue (rounded up to next power of two).
func (q *SessionEntryQueue) Cap() int { return int(q.mask + 1) }

// Close signals that no more entries will be pushed. Spinning Push callers
// return ErrQueueClosed. Spinning Pop callers return ErrQueueClosed once
// the queue is drained.
func (q *SessionEntryQueue) Close() {
	q.closed.Store(true)
}

// ---------------------------------------------------------------------------
// In-memory staging buffer with byte arena (parent-before-child ordering)
// ---------------------------------------------------------------------------

// DefaultArenaSizeBytes is the pre-allocated transcript byte arena: 2 GiB.
// On Linux with overcommit enabled (the default), the OS does not back virtual
// pages with physical RAM until they are first written, so RSS stays near zero
// until transcript data actually flows through the buffer.
const DefaultArenaSizeBytes = 2 * 1024 * 1024 * 1024 // 2 GiB

// EnvArenaSizeBytes overrides the staging-arena size (in bytes) for a pipeline
// run. A positive integer wins; otherwise DefaultArenaSizeBytes is used. Tests
// set a small arena (a few MiB — fixtures are tiny) so each pipeline run does
// not allocate the full 2 GiB slab; under the race detector that slab is the
// dominant test-memory cost. Mirrors EnvPoolSize.
const EnvArenaSizeBytes = "PEASANT_INGEST_ARENA_BYTES"

// resolveArenaSizeBytes returns the EnvArenaSizeBytes override if it parses as a
// positive integer, else def.
func resolveArenaSizeBytes(def int64) int64 {
	if v := os.Getenv(EnvArenaSizeBytes); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// DefaultMaxDrainBatch is the maximum number of entries returned by a single
// Drain call. Capping the batch size ensures that DB INSERT + INDEX per cycle
// stays bounded, keeping TUI progress responsive and SQLite transactions small.
const DefaultMaxDrainBatch = 512

// stagedEntry is a slot in the StagingBuffer's slot array. It pairs the
// workerResult with the arena byte range that backs its transcriptData payload.
// arenaStart..arenaStart+arenaLen is the region inside StagingBuffer.arena
// that holds the copied transcript bytes. arenaLen==0 means no arena copy
// (payload was nil or empty).
type stagedEntry struct {
	result     workerResult
	arenaStart int64 // offset into arena where transcriptData begins
	arenaLen   int64 // length of transcriptData in arena (0 = no arena copy)
}

// StagingBuffer holds completed workerResults in memory while they wait for
// their parent session's DB row to be committed. Once a parent is committed
// (via Commit), its children become eligible and are returned by the next
// Drain call.
//
// # Transcript byte arena
//
// A single contiguous slab (arena) is allocated at construction. When a
// workerResult carrying a non-nil transcriptData payload is added, its bytes
// are copied into the next available region of the arena rather than remaining
// as a separate heap allocation. The workerResult stored in the slot has its
// transcriptData slice pointing into the arena slab.
//
// The arena is treated as a ring: arenaHead bumps forward as entries are
// added; Drain advances arenaTail past the regions freed by released entries.
// Producers spin (runtime.Gosched) when the arena has insufficient contiguous
// space, blocking until Drain reclaims bytes.
//
// A payload that does not fit in the remaining arena space (e.g. a single
// transcript larger than the whole arena) is stored as a plain heap slice
// and does not consume arena space; arenaLen is recorded as 0 for such entries.
//
// # Concurrency model
//
// Add is safe to call from multiple producer goroutines concurrently.
// Drain may run concurrently with Add (MPMC). Ack, Commit must be called
// from the same goroutine as Drain (the pipeline orchestrator).
//
// Slot lifecycle: each slot transitions through four states via an atomic
// flag:
//
//	empty(0) → ready(1) → claimed(2) → acked(3)
//
// Add claims an index with a CAS on count, writes the slot, then publishes
// it by storing ready(1). Drain scans for ready+eligible slots, marks them
// claimed(2), and returns their results. Ack finalises the batch: marks
// claimed slots as acked(3) and frees their arena space. Arena bytes remain
// live until Ack, so the consumer can safely read transcriptData between
// Drain and Ack without risk of producer overwrites.
//
// # Ordering contract
//
//   - Root sessions (ParentUUID == nil) are always eligible immediately.
//   - A child session is eligible only after its parent's SessionID has been
//     passed to Commit.
//   - Drain returns all currently eligible entries whose slot state is ready.
type StagingBuffer struct {
	// slot array — statically allocated, indexed by monotonically increasing count.
	slots []stagedEntry
	state []atomic.Int32 // per-slot: 0=empty, 1=ready, 2=claimed, 3=acked
	count atomic.Int64   // claimed slots (monotonically increasing via CAS)

	// byte arena — ring buffer of raw transcript bytes.
	arena     []byte       // 2 GiB slab, allocated once
	arenaHead atomic.Int64 // next free byte offset (producers bump this)
	arenaTail atomic.Int64 // oldest live byte offset (Ack advances this)

	// committed set — DB-committed session IDs; guarded by commitMu on writes,
	// read only from the single drainer goroutine (no lock needed for reads).
	commitMu  sync.Mutex
	committed map[SessionID]struct{}
}

// NewStagingBuffer allocates a StagingBuffer with the given slot capacity and
// a transcript byte arena of arenaSizeBytes. Both values must be >= 1.
//
// For production use, pass DefaultArenaSizeBytes as arenaSizeBytes. On Linux
// with overcommit (vm.overcommit_memory=0 or 1), the 2 GiB virtual reservation
// does not consume physical RAM until written.
func NewStagingBuffer(capacity int, arenaSizeBytes int64) *StagingBuffer {
	if capacity < 1 {
		panic("ingest: StagingBuffer capacity must be >= 1")
	}
	if arenaSizeBytes < 1 {
		panic("ingest: StagingBuffer arena size must be >= 1")
	}
	return &StagingBuffer{
		slots:     make([]stagedEntry, capacity),
		state:     make([]atomic.Int32, capacity),
		arena:     make([]byte, arenaSizeBytes),
		committed: make(map[SessionID]struct{}),
	}
}

// copyToArena copies src into the arena ring and returns (start, length).
// If the arena has insufficient free space the call spins until Drain
// reclaims enough bytes. Returns (-1, 0) if src is nil or empty (no copy).
// Panics if src is larger than the arena — size the arena appropriately.
func (b *StagingBuffer) copyToArena(src []byte) (start, length int64) {
	if len(src) == 0 {
		return -1, 0
	}
	sz := int64(len(src))
	arenaSize := int64(len(b.arena))
	if sz > arenaSize {
		panic(fmt.Sprintf("ingest: transcript (%d bytes) exceeds arena size (%d bytes)", sz, arenaSize))
	}

	const (
		backoffMin = 1 * time.Millisecond
		backoffMax = 16 * time.Millisecond
	)
	backoff := backoffMin

	for {
		tail := b.arenaTail.Load()
		head := b.arenaHead.Load()
		free := arenaSize - (head - tail)
		if free >= sz {
			// Try to claim [head, head+sz) in the arena.
			// We use the linear (non-wrapping) coordinate space; actual index
			// is head % arenaSize. We require the region to not straddle the
			// end of the slab — if it would, pad head to the next wrap boundary
			// and retry so that all copies are contiguous.
			linearStart := head
			physStart := head % arenaSize
			if physStart+sz > arenaSize {
				// Would straddle end: advance head to the wrap boundary and retry.
				// The gap bytes are wasted but the invariant (contiguous copy) holds.
				gap := arenaSize - physStart
				if b.arenaHead.CompareAndSwap(head, head+gap) {
					// Retry with the new head after the wrap.
					backoff = backoffMin // reset on successful CAS
					continue
				}
				continue
			}
			if b.arenaHead.CompareAndSwap(head, head+sz) {
				copy(b.arena[physStart:physStart+sz], src)
				return linearStart, sz
			}
			// CAS lost to another producer; retry.
			continue
		}
		// Arena full — bounded exponential sleep until Drain advances arenaTail.
		time.Sleep(backoff)
		if backoff < backoffMax {
			backoff *= 2
		}
	}
}

// Add stores a completed workerResult in the buffer. The transcriptData
// payload (if any) is copied into the arena slab; the stored result's
// transcriptData slice points into the arena. Add spins if the arena is
// full until Drain reclaims space.
//
// Add is safe to call from multiple producer goroutines concurrently.
// Returns false only if the slot array is exhausted (capacity limit hit).
func (b *StagingBuffer) Add(r workerResult) bool {
	// Copy transcript bytes into the arena before claiming a slot.
	// Arena operations are lock-free CAS-based; multiple producers copy concurrently.
	aStart, aLen := b.copyToArena(r.transcriptData)
	if aLen > 0 {
		physStart := aStart % int64(len(b.arena))
		r.transcriptData = b.arena[physStart : physStart+aLen]
	}

	// CAS loop: claim exactly one slot index. Each index is claimed by at most
	// one producer, so the subsequent slot write is data-race-free.
	for {
		idx := b.count.Load()
		if int(idx) >= len(b.slots) {
			if aLen > 0 {
				b.arenaTail.Add(aLen)
			}
			return false
		}
		if b.count.CompareAndSwap(idx, idx+1) {
			b.slots[idx] = stagedEntry{
				result:     r,
				arenaStart: aStart,
				arenaLen:   aLen,
			}
			b.state[idx].Store(1) // publish: slot ready for draining
			return true
		}
		// CAS lost to another producer; retry.
	}
}

// Commit marks sessionIDs as DB-committed, making their children eligible for
// the next Drain call. Must be called from the single drainer goroutine.
func (b *StagingBuffer) Commit(ids ...SessionID) {
	b.commitMu.Lock()
	for _, id := range ids {
		b.committed[id] = struct{}{}
	}
	b.commitMu.Unlock()
}

// isEligible reports whether r is ready to be drained.
// Must be called from the single drainer goroutine (reads committed without lock).
func (b *StagingBuffer) isEligible(r workerResult) bool {
	if r.meta == nil || r.meta.ParentUUID == nil {
		return true
	}
	_, ok := b.committed[*r.meta.ParentUUID]
	return ok
}

// Drain returns all currently ready and eligible entries (roots and children
// whose parent is committed) as a DrainBatch. Each returned slot is atomically
// marked as claimed(2) so it is not returned by subsequent Drain calls. Arena
// space is NOT freed — the consumer may still be reading transcriptData slices
// that point into the arena. Call AckBatch after the batch is successfully
// processed to free arena space and finalise the removal.
//
// Multiple DrainBatch values may be outstanding simultaneously; each owns its
// own Claimed slice. Must be called from a single goroutine (the pipeline
// orchestrator). Drain leaves the Metas field nil; the consumer populates it
// after DB INSERT before calling AckBatch.
func (b *StagingBuffer) Drain() DrainBatch {
	n := int(b.count.Load())

	var results []workerResult
	var claimed []int

	for i := 0; i < n; i++ {
		if len(results) >= DefaultMaxDrainBatch {
			break
		}
		if b.state[i].Load() != 1 {
			continue // empty, claimed, or already acked
		}
		se := b.slots[i]
		if !b.isEligible(se.result) {
			continue // parent not yet committed
		}
		results = append(results, se.result)
		claimed = append(claimed, i)
		b.state[i].Store(2) // claimed: invisible to next Drain
	}

	return DrainBatch{Results: results, Claimed: claimed}
}

// AckBatch finalises a batch returned by Drain. It marks each claimed slot as
// acked(3) and frees the arena space those entries occupied, allowing producers
// blocked in copyToArena to make progress.
//
// Must be called from the same goroutine as Drain, after the consumer has
// finished reading the batch's transcriptData slices. Multiple in-flight
// batches may be acked in any order.
func (b *StagingBuffer) AckBatch(batch DrainBatch) {
	var freedArenaBytes int64
	for _, idx := range batch.Claimed {
		freedArenaBytes += b.slots[idx].arenaLen
		b.state[idx].Store(3) // acked
	}

	if freedArenaBytes > 0 {
		b.arenaTail.Add(freedArenaBytes)
	}
}

// Len returns the current number of buffered entries that have not been acked.
// This counts slots in states empty(0), ready(1), and claimed(2).
func (b *StagingBuffer) Len() int {
	n := int(b.count.Load())
	var live int
	for i := 0; i < n; i++ {
		if s := b.state[i].Load(); s != 3 {
			live++
		}
	}
	return live
}

// Cap returns the fixed slot capacity of the buffer.
func (b *StagingBuffer) Cap() int { return len(b.slots) }

// ArenaSize returns the total arena size in bytes.
func (b *StagingBuffer) ArenaSize() int64 { return int64(len(b.arena)) }

// ArenaUsed returns the number of arena bytes currently in use (live entries).
func (b *StagingBuffer) ArenaUsed() int64 {
	used := b.arenaHead.Load() - b.arenaTail.Load()
	if used < 0 {
		return 0
	}
	return used
}

// runParallel fans out work items to at most `workers` goroutines and
// collects results in input order. fn is called exactly once per item.
// Items are dispatched in the order they appear in the slice (callers
// should pass size-sorted slices to schedule large work first).
//
// The generic context parameter is constrained to types with an Err() method
// so we can short-circuit on cancellation without importing context.
func runParallel[T any, R any](ctxErr func() error, items []T, workers int, fn func(T) R) []R {
	results := make([]R, len(items))
	if len(items) == 0 {
		return results
	}
	if workers <= 0 {
		workers = 1
	}
	if workers > len(items) {
		workers = len(items)
	}

	type work struct {
		idx  int
		item T
	}
	ch := make(chan work, len(items))
	for i, item := range items {
		ch <- work{i, item}
	}
	close(ch)

	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for w := range ch {
				if ctxErr() != nil {
					return
				}
				results[w.idx] = fn(w.item)
			}
		}()
	}
	wg.Wait()
	return results
}
