package ingest

import (
	"sync"
	"testing"
)

// TestProgressState_NilSafe verifies emitProgress does not panic when ps is nil.
func TestProgressState_NilSafe(t *testing.T) {
	// Must not panic.
	emitProgress(nil, ProgressEvent{Kind: KindStart, Stage: StageDiscover, Total: 10})
	emitProgress(nil, ProgressEvent{Kind: KindAdvance, Stage: StageDiscover, Done: 5})
	emitProgress(nil, ProgressEvent{Kind: KindEnd, Stage: StageDiscover, Done: 10, Total: 10})
}

// TestProgressState_Update_KindStart verifies KindStart resets stage state.
func TestProgressState_Update_KindStart(t *testing.T) {
	ps := NewProgressState()
	// Advance to non-zero state first.
	ps.Update(ProgressEvent{Kind: KindAdvance, Stage: StageDiscover, Done: 5, Total: 10})
	// KindStart should reset.
	ps.Update(ProgressEvent{Kind: KindStart, Stage: StageDiscover, Total: 20})

	snap := ps.Snapshot()
	sp := snap[StageDiscover]
	if sp.Total != 20 {
		t.Errorf("Total: got %d, want 20", sp.Total)
	}
	if sp.Done != 0 {
		t.Errorf("Done: got %d, want 0", sp.Done)
	}
	if sp.Ended {
		t.Error("Ended: got true, want false")
	}
	if sp.HasErr {
		t.Error("HasErr: got true, want false")
	}
}

// TestProgressState_Update_KindAdvance verifies KindAdvance updates Done and Total.
func TestProgressState_Update_KindAdvance(t *testing.T) {
	ps := NewProgressState()
	ps.Update(ProgressEvent{Kind: KindStart, Stage: StageDiff, Total: 10})
	ps.Update(ProgressEvent{Kind: KindAdvance, Stage: StageDiff, Done: 3})
	ps.Update(ProgressEvent{Kind: KindAdvance, Stage: StageDiff, Done: 7, Total: 15}) // total grows

	snap := ps.Snapshot()
	sp := snap[StageDiff]
	if sp.Done != 7 {
		t.Errorf("Done: got %d, want 7", sp.Done)
	}
	if sp.Total != 15 {
		t.Errorf("Total: got %d, want 15 (should grow)", sp.Total)
	}
	if sp.Ended {
		t.Error("Ended: got true after Advance, want false")
	}
}

// TestProgressState_Update_KindEnd verifies KindEnd sets Ended and HasErr.
func TestProgressState_Update_KindEnd(t *testing.T) {
	ps := NewProgressState()
	ps.Update(ProgressEvent{Kind: KindStart, Stage: StageExtract, Total: 5})
	ps.Update(ProgressEvent{Kind: KindEnd, Stage: StageExtract, Done: 5, Total: 5})

	snap := ps.Snapshot()
	sp := snap[StageExtract]
	if !sp.Ended {
		t.Error("Ended: got false, want true")
	}
	if sp.HasErr {
		t.Error("HasErr: got true on success end, want false")
	}
	if sp.Done != 5 || sp.Total != 5 {
		t.Errorf("Done/Total: got %d/%d, want 5/5", sp.Done, sp.Total)
	}
}

// TestProgressState_Update_KindEnd_WithError verifies KindEnd with Err sets HasErr.
func TestProgressState_Update_KindEnd_WithError(t *testing.T) {
	ps := NewProgressState()
	ps.Update(ProgressEvent{Kind: KindStart, Stage: StageIndex})
	ps.Update(ProgressEvent{Kind: KindEnd, Stage: StageIndex, Err: errTest})

	snap := ps.Snapshot()
	sp := snap[StageIndex]
	if !sp.Ended {
		t.Error("Ended: got false, want true")
	}
	if !sp.HasErr {
		t.Error("HasErr: got false, want true when Err is set")
	}
}

// errTest is a sentinel error for testing.
var errTest = &testError{}

type testError struct{}

func (*testError) Error() string { return "test error" }

// TestProgressState_Snapshot_IsIndependent verifies Snapshot returns a copy, not a reference.
func TestProgressState_Snapshot_IsIndependent(t *testing.T) {
	ps := NewProgressState()
	ps.Update(ProgressEvent{Kind: KindStart, Stage: StageDiscover, Total: 10})

	snap1 := ps.Snapshot()
	// Mutate state after snapshot.
	ps.Update(ProgressEvent{Kind: KindAdvance, Stage: StageDiscover, Done: 5})
	snap2 := ps.Snapshot()

	if snap1[StageDiscover].Done != 0 {
		t.Errorf("snap1 Done: got %d, want 0 (snapshot must be independent)", snap1[StageDiscover].Done)
	}
	if snap2[StageDiscover].Done != 5 {
		t.Errorf("snap2 Done: got %d, want 5", snap2[StageDiscover].Done)
	}
}

// TestProgressState_UnknownStage verifies Update ignores unknown stages.
func TestProgressState_UnknownStage(t *testing.T) {
	ps := NewProgressState()
	// Must not panic on unknown stage.
	ps.Update(ProgressEvent{Kind: KindStart, Stage: Stage("NONEXISTENT"), Total: 1})
	// Known stages are unaffected.
	snap := ps.Snapshot()
	if snap[StageDiscover].Ended {
		t.Error("known stage should be unaffected by unknown stage update")
	}
}

// TestProgressState_ConcurrentReadWrite verifies thread safety under parallel
// writes and reads. Must be run with -race.
func TestProgressState_ConcurrentReadWrite(t *testing.T) {
	ps := NewProgressState()
	const goroutines = 8
	const iterations = 200

	var wg sync.WaitGroup

	// Writers: emit start, advance, end events concurrently.
	for i := range goroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			stage := StageOrder[id%len(StageOrder)]
			for range iterations {
				ps.Update(ProgressEvent{Kind: KindStart, Stage: stage, Total: iterations})
				ps.Update(ProgressEvent{Kind: KindAdvance, Stage: stage, Done: iterations / 2})
				ps.Update(ProgressEvent{Kind: KindEnd, Stage: stage, Done: iterations, Total: iterations})
			}
		}(i)
	}

	// Readers: take snapshots concurrently with writers.
	for range goroutines / 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				snap := ps.Snapshot()
				// Just access the values to ensure no data race.
				for _, sp := range snap {
					_ = sp.Done + sp.Total
					_ = sp.Ended
					_ = sp.HasErr
				}
			}
		}()
	}

	wg.Wait()
}

// TestProgressState_AllStagesPrePopulated verifies NewProgressState initialises all stages.
func TestProgressState_AllStagesPrePopulated(t *testing.T) {
	ps := NewProgressState()
	snap := ps.Snapshot()
	for _, stage := range StageOrder {
		if _, ok := snap[stage]; !ok {
			t.Errorf("stage %s: missing from initial snapshot", stage)
		}
	}
	if len(snap) != len(StageOrder) {
		t.Errorf("snapshot len: got %d, want %d", len(snap), len(StageOrder))
	}
}
