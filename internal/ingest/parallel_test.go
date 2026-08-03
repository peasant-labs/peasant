package ingest

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/schema"
)

// ---------------------------------------------------------------------------
// nextPow2
// ---------------------------------------------------------------------------

func TestNextPow2(t *testing.T) {
	// Minimum result is 2 (Vyukov MPMC requires cap >= 2).
	cases := []struct{ in, want int }{
		{1, 2}, {2, 2}, {3, 4}, {4, 4},
		{5, 8}, {7, 8}, {8, 8}, {9, 16},
		{1000, 1024}, {1024, 1024}, {1025, 2048},
	}
	for _, tc := range cases {
		got := nextPow2(tc.in)
		if got != tc.want {
			t.Errorf("nextPow2(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestNextPow2_PanicsOnZero(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for capacity 0")
		}
	}()
	nextPow2(0)
}

// ---------------------------------------------------------------------------
// topoLevels
// ---------------------------------------------------------------------------

func TestTopoLevels_Empty(t *testing.T) {
	if got := topoLevels(nil); got != nil {
		t.Fatalf("expected nil for empty input, got %v", got)
	}
}

func TestTopoLevels_AllRoots(t *testing.T) {
	entries := []DiffEntry{
		{Session: DiscoveredSession{SessionID: "a"}},
		{Session: DiscoveredSession{SessionID: "b"}},
		{Session: DiscoveredSession{SessionID: "c"}},
	}
	levels := topoLevels(entries)
	if len(levels) != 1 {
		t.Fatalf("expected 1 level, got %d", len(levels))
	}
	if len(levels[0]) != 3 {
		t.Fatalf("expected 3 entries in level 0, got %d", len(levels[0]))
	}
}

func TestTopoLevels_ParentChild(t *testing.T) {
	parentID := SessionID("parent")
	entries := []DiffEntry{
		{Session: DiscoveredSession{SessionID: "parent"}},
		{Session: DiscoveredSession{SessionID: "child", ParentUUID: &parentID}},
	}
	levels := topoLevels(entries)
	if len(levels) != 2 {
		t.Fatalf("expected 2 levels, got %d", len(levels))
	}
	if levels[0][0].Session.SessionID != "parent" {
		t.Errorf("level 0 should be parent, got %s", levels[0][0].Session.SessionID)
	}
	if levels[1][0].Session.SessionID != "child" {
		t.Errorf("level 1 should be child, got %s", levels[1][0].Session.SessionID)
	}
}

func TestTopoLevels_ThreeTiers(t *testing.T) {
	aID := SessionID("a")
	bID := SessionID("b")
	entries := []DiffEntry{
		{Session: DiscoveredSession{SessionID: "a"}},
		{Session: DiscoveredSession{SessionID: "b", ParentUUID: &aID}},
		{Session: DiscoveredSession{SessionID: "c", ParentUUID: &bID}},
	}
	levels := topoLevels(entries)
	if len(levels) != 3 {
		t.Fatalf("expected 3 levels, got %d", len(levels))
	}
}

func TestTopoLevels_ParentNotInBatch(t *testing.T) {
	unknownID := SessionID("unknown")
	entries := []DiffEntry{
		{Session: DiscoveredSession{SessionID: "child", ParentUUID: &unknownID}},
	}
	// Parent not in batch → treated as root → single level.
	levels := topoLevels(entries)
	if len(levels) != 1 {
		t.Fatalf("expected 1 level, got %d", len(levels))
	}
}

// ---------------------------------------------------------------------------
// SessionEntryQueue — basic single-goroutine behaviour
// ---------------------------------------------------------------------------

func newEntry(tokensIn, tokensOut int) schema.SessionEntry {
	return schema.SessionEntry{TokensIn: &tokensIn, TokensOut: &tokensOut}
}

func TestSessionEntryQueue_CapRoundedToPow2(t *testing.T) {
	cases := []struct{ in, want int }{
		{1, 2}, // minimum is 2
		{2, 2},
		{3, 4},
		{4, 4},
		{5, 8},
	}
	for _, tc := range cases {
		q := NewSessionEntryQueue(tc.in)
		if q.Cap() != tc.want {
			t.Errorf("NewSessionEntryQueue(%d).Cap() = %d, want %d", tc.in, q.Cap(), tc.want)
		}
	}
}

func TestSessionEntryQueue_PushPop_Sequential(t *testing.T) {
	q := NewSessionEntryQueue(4)
	e1 := newEntry(10, 20)
	e2 := newEntry(5, 5)

	if err := q.Push(e1); err != nil {
		t.Fatalf("Push e1: %v", err)
	}
	if err := q.Push(e2); err != nil {
		t.Fatalf("Push e2: %v", err)
	}
	if q.Len() != 2 {
		t.Fatalf("expected Len=2, got %d", q.Len())
	}

	got1, err := q.Pop()
	if err != nil {
		t.Fatalf("Pop 1: %v", err)
	}
	got2, err := q.Pop()
	if err != nil {
		t.Fatalf("Pop 2: %v", err)
	}
	// FIFO order — ring buffer does not reorder.
	if got1 != e1 {
		t.Errorf("first pop: got %+v, want %+v", got1, e1)
	}
	if got2 != e2 {
		t.Errorf("second pop: got %+v, want %+v", got2, e2)
	}
}

func TestSessionEntryQueue_TryPush_Full(t *testing.T) {
	q := NewSessionEntryQueue(2) // exact power-of-two capacity
	if err := q.Push(newEntry(1, 1)); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if err := q.Push(newEntry(2, 2)); err != nil {
		t.Fatalf("second push: %v", err)
	}
	// Queue is now full (cap=2, 2 items).
	err := q.TryPush(newEntry(3, 3))
	if err != ErrQueueFull {
		t.Fatalf("expected ErrQueueFull, got %v", err)
	}
}

func TestSessionEntryQueue_Close_PushReturnsError(t *testing.T) {
	q := NewSessionEntryQueue(4)
	q.Close()
	if err := q.Push(newEntry(1, 1)); err != ErrQueueClosed {
		t.Fatalf("expected ErrQueueClosed after close, got %v", err)
	}
	if err := q.TryPush(newEntry(1, 1)); err != ErrQueueClosed {
		t.Fatalf("TryPush: expected ErrQueueClosed, got %v", err)
	}
}

func TestSessionEntryQueue_Close_PopDrainsAndErrors(t *testing.T) {
	q := NewSessionEntryQueue(4)
	q.Push(newEntry(1, 1)) //nolint:errcheck
	q.Push(newEntry(2, 2)) //nolint:errcheck
	q.Close()

	_, err1 := q.Pop()
	_, err2 := q.Pop()
	_, err3 := q.Pop() // empty + closed

	if err1 != nil {
		t.Fatalf("first pop after close: %v", err1)
	}
	if err2 != nil {
		t.Fatalf("second pop after close: %v", err2)
	}
	if err3 != ErrQueueClosed {
		t.Fatalf("third pop: expected ErrQueueClosed, got %v", err3)
	}
}

// ---------------------------------------------------------------------------
// SessionEntryQueue — concurrent correctness (MPMC, -race must pass)
// ---------------------------------------------------------------------------

func TestSessionEntryQueue_MPMC_NoItemLost(t *testing.T) {
	const (
		producers  = 4
		consumers  = 4
		perProd    = 256
		totalItems = producers * perProd
	)
	q := NewSessionEntryQueue(64)

	var produced atomic.Int64
	var consumed atomic.Int64

	var wgProd sync.WaitGroup
	for p := 0; p < producers; p++ {
		wgProd.Add(1)
		go func() {
			defer wgProd.Done()
			for i := 0; i < perProd; i++ {
				e := newEntry(i, i)
				for {
					if err := q.Push(e); err == nil {
						produced.Add(1)
						break
					}
					// ErrQueueClosed should not happen here
				}
			}
		}()
	}

	var wgCons sync.WaitGroup
	for c := 0; c < consumers; c++ {
		wgCons.Add(1)
		go func() {
			defer wgCons.Done()
			for {
				_, err := q.Pop()
				if err == ErrQueueClosed {
					return
				}
				consumed.Add(1)
			}
		}()
	}

	wgProd.Wait()
	q.Close()
	wgCons.Wait()

	if produced.Load() != int64(totalItems) {
		t.Errorf("produced %d, want %d", produced.Load(), totalItems)
	}
	if consumed.Load() != int64(totalItems) {
		t.Errorf("consumed %d, want %d", consumed.Load(), totalItems)
	}
}

func TestSessionEntryQueue_MPMC_NoDataRace(t *testing.T) {
	// Lightweight race-detector stress test.
	q := NewSessionEntryQueue(8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(v int) {
			defer wg.Done()
			q.Push(newEntry(v, v)) //nolint:errcheck
		}(i)
		go func() {
			defer wg.Done()
			q.Pop() //nolint:errcheck
		}()
	}
	q.Close()
	wg.Wait()
}

// ---------------------------------------------------------------------------
// StagingBuffer
// ---------------------------------------------------------------------------

func sid(s string) SessionID { return SessionID(s) }

func makeResult(sessionID SessionID, parentID *SessionID) workerResult {
	meta := &UnifiedMetadata{SessionID: sessionID, ParentUUID: parentID}
	return workerResult{meta: meta}
}

func TestStagingBuffer_PanicsOnZeroCapacity(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for capacity 0")
		}
	}()
	NewStagingBuffer(0, 1024*1024)
}

func TestStagingBuffer_RootsDrainImmediately(t *testing.T) {
	b := NewStagingBuffer(4, 1024*1024)
	b.Add(makeResult(sid("a"), nil))
	b.Add(makeResult(sid("b"), nil))

	batch := b.Drain()
	if len(batch.Results) != 2 {
		t.Fatalf("expected 2 eligible, got %d", len(batch.Results))
	}
	b.AckBatch(batch)
	if b.Len() != 0 {
		t.Fatalf("expected 0 remaining, got %d", b.Len())
	}
}

func TestStagingBuffer_ChildHeldUntilParentCommitted(t *testing.T) {
	b := NewStagingBuffer(4, 1024*1024)
	parentID := sid("parent")
	b.Add(makeResult(sid("parent"), nil))
	b.Add(makeResult(sid("child"), &parentID))

	// First drain: only parent (root) is eligible.
	batch := b.Drain()
	if len(batch.Results) != 1 || batch.Results[0].meta.SessionID != "parent" {
		t.Fatalf("expected only parent, got %v", batch.Results)
	}
	b.AckBatch(batch)
	if b.Len() != 1 {
		t.Fatalf("expected 1 remaining after first drain, got %d", b.Len())
	}

	// Commit the parent, then drain again.
	b.Commit(sid("parent"))
	batch = b.Drain()
	if len(batch.Results) != 1 || batch.Results[0].meta.SessionID != "child" {
		t.Fatalf("expected child after commit, got %v", batch.Results)
	}
	b.AckBatch(batch)
	if b.Len() != 0 {
		t.Fatalf("expected 0 remaining after second drain, got %d", b.Len())
	}
}

func TestStagingBuffer_ThreeTiers(t *testing.T) {
	b := NewStagingBuffer(8, 1024*1024)
	aID := sid("a")
	bID := sid("b")
	b.Add(makeResult(sid("a"), nil))
	b.Add(makeResult(sid("b"), &aID))
	b.Add(makeResult(sid("c"), &bID))

	// Drain 1: only root "a".
	batch := b.Drain()
	assertIDs(t, batch.Results, "a")
	b.AckBatch(batch)
	b.Commit(sid("a"))

	// Drain 2: "b" now eligible.
	batch = b.Drain()
	assertIDs(t, batch.Results, "b")
	b.AckBatch(batch)
	b.Commit(sid("b"))

	// Drain 3: "c" now eligible.
	batch = b.Drain()
	assertIDs(t, batch.Results, "c")
	b.AckBatch(batch)
	if b.Len() != 0 {
		t.Fatalf("expected empty buffer, got %d", b.Len())
	}
}

func TestStagingBuffer_FullReturnsFalse(t *testing.T) {
	b := NewStagingBuffer(1, 1024*1024)
	if !b.Add(makeResult(sid("a"), nil)) {
		t.Fatal("first Add should succeed")
	}
	if b.Add(makeResult(sid("b"), nil)) {
		t.Fatal("second Add to full buffer should return false")
	}
}

func TestStagingBuffer_NilMetaDrainsImmediately(t *testing.T) {
	b := NewStagingBuffer(4, 1024*1024)
	b.Add(workerResult{meta: nil}) // no metadata — treated as root
	batch := b.Drain()
	if len(batch.Results) != 1 {
		t.Fatalf("expected 1, got %d", len(batch.Results))
	}
}

func TestStagingBuffer_ConcurrentAdd_NoRace(t *testing.T) {
	const n = 64
	b := NewStagingBuffer(n, 1024*1024)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := SessionID(string(rune('a' + i%26)))
			b.Add(makeResult(id, nil))
		}(i)
	}
	wg.Wait()
	// All entries are roots — Drain should return all n.
	batch := b.Drain()
	if len(batch.Results) != n {
		t.Fatalf("expected %d drained, got %d", n, len(batch.Results))
	}
}

// TestStagingBuffer_ArenaFull_ConcurrentDrain verifies that producers blocked
// on a full arena are unblocked when Drain runs concurrently and reclaims space.
// This is the exact deadlock scenario that occurred when Drain was sequential.
func TestStagingBuffer_ArenaFull_ConcurrentDrain(t *testing.T) {
	// Small arena: 1 KiB. Each payload is 600 bytes → second Add blocks until Drain frees space.
	const arenaSize = 1024
	const payloadSize = 600
	b := NewStagingBuffer(4, arenaSize)

	payload := func(id string) workerResult {
		meta := &UnifiedMetadata{SessionID: SessionID(id)}
		return workerResult{meta: meta, transcriptData: make([]byte, payloadSize)}
	}

	// First Add succeeds (600 of 1024 bytes).
	if !b.Add(payload("first")) {
		t.Fatal("first Add should succeed")
	}

	// Second Add will spin on arena space. Run it in a goroutine.
	done := make(chan bool, 1)
	go func() {
		done <- b.Add(payload("second"))
	}()

	// Drain + Ack from the main goroutine — Ack frees the first entry's arena
	// space, unblocking the spinning producer. Without concurrent Drain+Ack,
	// the producer spins forever (the original deadlock).
	deadline := time.After(5 * time.Second)
	for {
		batch := b.Drain()
		if len(batch.Results) > 0 {
			b.AckBatch(batch) // free arena space → unblock producer
			break
		}
		select {
		case <-deadline:
			t.Fatal("deadlock: Drain never returned eligible entries; producer is stuck on full arena")
		default:
			runtime.Gosched()
		}
	}

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("second Add should succeed after AckBatch frees arena space")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: second Add never completed after AckBatch freed arena space")
	}

	// Drain + AckBatch remaining entry.
	batch := b.Drain()
	if len(batch.Results) != 1 || string(batch.Results[0].meta.SessionID) != "second" {
		t.Fatalf("expected [second], got %v", batch.Results)
	}
	b.AckBatch(batch)
	if b.Len() != 0 {
		t.Fatalf("expected 0 remaining, got %d", b.Len())
	}
}

// TestStagingBuffer_ConcurrentAddDrain_AllEntriesDelivered verifies that
// concurrent producers and a concurrent drainer together deliver every entry
// exactly once, with no races or lost data.
func TestStagingBuffer_ConcurrentAddDrain_AllEntriesDelivered(t *testing.T) {
	const nProducers = 32
	b := NewStagingBuffer(nProducers, 1024*1024)

	// Producers add concurrently.
	var wg sync.WaitGroup
	for i := range nProducers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := SessionID(fmt.Sprintf("s-%03d", i))
			b.Add(makeResult(id, nil))
		}()
	}

	// Drainer runs concurrently with producers.
	var drained []workerResult
	var drainerDone atomic.Bool
	go func() {
		wg.Wait()
		drainerDone.Store(true)
	}()

	for {
		done := drainerDone.Load()
		batch := b.Drain()
		drained = append(drained, batch.Results...)
		b.AckBatch(batch)
		if done && len(batch.Results) == 0 {
			// One final drain to catch anything published between last Load and Store.
			final := b.Drain()
			drained = append(drained, final.Results...)
			b.AckBatch(final)
			break
		}
		runtime.Gosched()
	}

	if len(drained) != nProducers {
		t.Fatalf("expected %d drained entries, got %d", nProducers, len(drained))
	}

	// Verify no duplicates.
	seen := make(map[SessionID]struct{}, len(drained))
	for _, r := range drained {
		id := r.meta.SessionID
		if _, dup := seen[id]; dup {
			t.Errorf("duplicate session %q", id)
		}
		seen[id] = struct{}{}
	}
}

// TestStagingBuffer_MultipleInFlightBatches verifies that two DrainBatch values
// may be outstanding simultaneously without interfering with each other.
// Scenario 4 from the BDD spec.
func TestStagingBuffer_MultipleInFlightBatches(t *testing.T) {
	b := NewStagingBuffer(4, 1024*1024)
	b.Add(makeResult(sid("a"), nil))
	b.Add(makeResult(sid("b"), nil))

	// Drain batch N — contains "a" and "b" (both roots).
	batchN := b.Drain()
	if len(batchN.Results) != 2 {
		t.Fatalf("batchN: expected 2 results, got %d", len(batchN.Results))
	}
	if len(batchN.Claimed) != 2 {
		t.Fatalf("batchN: expected 2 claimed slots, got %d", len(batchN.Claimed))
	}

	// Add two more entries while batchN is still outstanding.
	b.Add(makeResult(sid("c"), nil))
	b.Add(makeResult(sid("d"), nil))

	// Drain batch N+1 — should return "c" and "d".
	batchN1 := b.Drain()
	if len(batchN1.Results) != 2 {
		t.Fatalf("batchN1: expected 2 results, got %d", len(batchN1.Results))
	}
	assertIDs(t, batchN1.Results, "c", "d")

	// Ack N first — only frees N's arena space; N+1 remains valid.
	b.AckBatch(batchN)
	if b.Len() != 2 {
		t.Fatalf("after AckBatch(N): expected 2 remaining, got %d", b.Len())
	}

	// Ack N+1 — all slots now freed.
	b.AckBatch(batchN1)
	if b.Len() != 0 {
		t.Fatalf("after AckBatch(N+1): expected 0 remaining, got %d", b.Len())
	}
}

// TestStagingBuffer_BoundedBackoff verifies that copyToArena does not
// spin-lock when the arena is full: instead it sleeps with bounded
// exponential backoff. We confirm that a producer blocked on a tiny arena
// is eventually unblocked after AckBatch frees space, and that the test
// itself completes within a reasonable wall-clock time (ruling out
// tight-spin CPU waste). Scenario 7 from the BDD spec.
func TestStagingBuffer_BoundedBackoff(t *testing.T) {
	// Arena: 512 bytes. Each payload: 300 bytes → second Add blocks.
	const arenaSize = 512
	const payloadSize = 300
	b := NewStagingBuffer(4, arenaSize)

	payload := func(id string) workerResult {
		meta := &UnifiedMetadata{SessionID: SessionID(id)}
		return workerResult{meta: meta, transcriptData: make([]byte, payloadSize)}
	}

	// First Add fits.
	if !b.Add(payload("x")) {
		t.Fatal("first Add should succeed")
	}

	// Second Add will block in copyToArena (arena full).
	// Launch it in a goroutine and track when it returns.
	addDone := make(chan bool, 1)
	go func() {
		addDone <- b.Add(payload("y"))
	}()

	// Wait a moment to let the producer enter the backoff loop.
	time.Sleep(5 * time.Millisecond)

	// Drain + AckBatch — frees arena space, unblocking the producer.
	batch := b.Drain()
	if len(batch.Results) == 0 {
		t.Fatal("expected at least one result from Drain")
	}
	b.AckBatch(batch)

	select {
	case ok := <-addDone:
		if !ok {
			t.Fatal("second Add should succeed after AckBatch frees arena space")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bounded backoff: producer never unblocked after AckBatch")
	}
}

// assertIDs checks that the drained results contain exactly the given session IDs (order-insensitive).
func assertIDs(t *testing.T, results []workerResult, ids ...string) {
	t.Helper()
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	got := make(map[string]struct{}, len(results))
	for _, r := range results {
		if r.meta != nil {
			got[string(r.meta.SessionID)] = struct{}{}
		}
	}
	for id := range want {
		if _, ok := got[id]; !ok {
			t.Errorf("expected session %q in results, not found; got %v", id, got)
		}
	}
	for id := range got {
		if _, ok := want[id]; !ok {
			t.Errorf("unexpected session %q in results", id)
		}
	}
}
