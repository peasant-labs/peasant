package ingest

import (
	"bytes"
	_ "embed"
	"io"
	"runtime"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/progress_advance.yaml
var progressAdvanceYAML []byte

type progressAdvanceCase struct {
	Name      string `yaml:"name"`
	Total     int    `yaml:"total"`
	Deltas    []int  `yaml:"deltas"`
	WantDone  int    `yaml:"want_done"`
	WantTotal int    `yaml:"want_total"`
}

func loadProgressAdvanceCases(t *testing.T) []progressAdvanceCase {
	t.Helper()
	var fixture struct {
		RequiredCases []string              `yaml:"required_cases"`
		Cases         []progressAdvanceCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(progressAdvanceYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode the progress advance fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("the progress advance fixture must hold exactly one YAML document: %v", err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" || present[testCase.Name] {
			t.Fatalf("the progress advance fixture has an empty or repeated case name %q", testCase.Name)
		}
		present[testCase.Name] = true
	}
	for _, required := range fixture.RequiredCases {
		if !present[required] {
			t.Fatalf("required progress advance case %q is missing; the regression it pins would stop being tested", required)
		}
	}
	return fixture.Cases
}

// TestProgressState_NilSafe verifies emitProgress and emitAdvance do not panic
// when ps is nil.
func TestProgressState_NilSafe(t *testing.T) {
	// Must not panic.
	emitProgress(nil, ProgressEvent{Kind: KindStart, Stage: StageDiscover, Total: 10})
	emitAdvance(nil, StageDiscover, 5, 0)
	emitProgress(nil, ProgressEvent{Kind: KindEnd, Stage: StageDiscover, Done: 10, Total: 10})
}

// TestProgressState_Update_KindStart verifies KindStart resets stage state.
func TestProgressState_Update_KindStart(t *testing.T) {
	ps := NewProgressState()
	// Advance to non-zero state first.
	ps.Update(ProgressEvent{Kind: KindAdvance, Stage: StageDiscover, Delta: 5, Total: 10})
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

// TestProgressState_Update_KindAdvance verifies KindAdvance adds its Delta to
// the stage's cumulative Done and raises Total.
func TestProgressState_Update_KindAdvance(t *testing.T) {
	ps := NewProgressState()
	ps.Update(ProgressEvent{Kind: KindStart, Stage: StageDiff, Total: 10})
	ps.Update(ProgressEvent{Kind: KindAdvance, Stage: StageDiff, Delta: 3})
	ps.Update(ProgressEvent{Kind: KindAdvance, Stage: StageDiff, Delta: 4, Total: 15}) // total grows
	ps.Update(ProgressEvent{Kind: KindAdvance, Stage: StageDiff, Delta: 0})            // a zero delta adds nothing

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

// TestProgressState_KindAdvanceIgnoresDone pins the new contract: KindAdvance
// carries a delta and the store owns the cumulative count, so a stray absolute
// Done cannot move the stage's count, not even forwards.
func TestProgressState_KindAdvanceIgnoresDone(t *testing.T) {
	ps := NewProgressState()
	ps.Update(ProgressEvent{Kind: KindStart, Stage: StageDiff, Total: 10})
	ps.Update(ProgressEvent{Kind: KindAdvance, Stage: StageDiff, Delta: 2})
	ps.Update(ProgressEvent{Kind: KindAdvance, Stage: StageDiff, Done: 999})

	if got := ps.Snapshot()[StageDiff].Done; got != 2 {
		t.Fatalf("Done = %d, want 2; KindAdvance must add Delta and ignore Done", got)
	}
}

// TestProgressState_KindAdvanceAccumulatesDeltas is the regression for the
// absolute-value store: a worker pool emits an advance when a worker finishes,
// so the deltas arrive in completion order and may be any permutation of the
// same set. The store adds each delta, so every permutation reaches the same
// cumulative Done.
func TestProgressState_KindAdvanceAccumulatesDeltas(t *testing.T) {
	for _, testCase := range loadProgressAdvanceCases(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			ps := NewProgressState()
			ps.Update(ProgressEvent{Kind: KindStart, Stage: StageDiff, Total: testCase.Total})
			for _, delta := range testCase.Deltas {
				ps.Update(ProgressEvent{Kind: KindAdvance, Stage: StageDiff, Delta: delta, Total: testCase.WantTotal})
			}
			got := ps.Snapshot()[StageDiff]
			if got.Done != testCase.WantDone {
				t.Fatalf("Done = %d, want %d after deltas %v", got.Done, testCase.WantDone, testCase.Deltas)
			}
			if got.Total != testCase.WantTotal {
				t.Fatalf("Total = %d, want %d", got.Total, testCase.WantTotal)
			}
		})
	}
}

// TestProgressState_KindAdvanceConcurrentAddsStayMonotone proves the cumulative
// count never decreases while many workers add deltas at once, and reaches the
// exact sum. Run with -race: the store mutex is the only synchronization.
func TestProgressState_KindAdvanceConcurrentAddsStayMonotone(t *testing.T) {
	const writers = 8
	const iterations = 500
	ps := NewProgressState()
	ps.Update(ProgressEvent{Kind: KindStart, Stage: StageDiff, Total: writers * iterations})

	decreased := make(chan int, 1)
	stop := make(chan struct{})
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		previous := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			done := ps.Snapshot()[StageDiff].Done
			if done < previous {
				select {
				case decreased <- done:
				default:
				}
				return
			}
			previous = done
			runtime.Gosched()
		}
	}()

	var writersWG sync.WaitGroup
	for range writers {
		writersWG.Add(1)
		go func() {
			defer writersWG.Done()
			for range iterations {
				ps.Update(ProgressEvent{Kind: KindAdvance, Stage: StageDiff, Delta: 1})
			}
		}()
	}
	writersWG.Wait()
	close(stop)
	readerWG.Wait()

	select {
	case got := <-decreased:
		t.Fatalf("DIFF Done decreased to %d; the store-owned counter must be monotone", got)
	default:
	}
	if got := ps.Snapshot()[StageDiff].Done; got != writers*iterations {
		t.Fatalf("Done = %d, want %d", got, writers*iterations)
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
	ps.Update(ProgressEvent{Kind: KindAdvance, Stage: StageDiscover, Delta: 5})
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
				ps.Update(ProgressEvent{Kind: KindAdvance, Stage: stage, Delta: 1})
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

func TestProgressState_ResetClearsEveryCanonicalStage(t *testing.T) {
	ps := NewProgressState()
	for _, stage := range StageOrder {
		ps.Update(ProgressEvent{Kind: KindStart, Stage: stage, Total: 9})
		ps.Update(ProgressEvent{Kind: KindAdvance, Stage: stage, Delta: 4, Total: 9})
		ps.Update(ProgressEvent{Kind: KindEnd, Stage: stage, Done: 7, Total: 9, Err: errTest})
	}
	for stage, progress := range ps.Snapshot() {
		if progress.Total == 0 || progress.Done == 0 || !progress.Started || !progress.Ended || !progress.HasErr {
			t.Fatalf("precondition: stage %s did not populate every mutable field: %#v", stage, progress)
		}
	}

	ps.Reset()
	snapshot := ps.Snapshot()
	if len(snapshot) != len(StageOrder) {
		t.Fatalf("reset snapshot has %d stages, want %d canonical stages", len(snapshot), len(StageOrder))
	}
	for _, stage := range StageOrder {
		progress, present := snapshot[stage]
		if !present {
			t.Errorf("reset removed canonical stage %s", stage)
			continue
		}
		if progress != (StageProgress{}) {
			t.Errorf("reset stage %s = %#v, want zero state", stage, progress)
		}
	}
}

// TestProgressState_ConcurrentResetUpdateSnapshot exercises the retry reset
// against the same producer and renderer operations used in production. Run
// with -race: synchronization failures are part of this test's contract.
func TestProgressState_ConcurrentResetUpdateSnapshot(t *testing.T) {
	ps := NewProgressState()
	const iterations = 300
	var wg sync.WaitGroup

	wg.Add(3)
	go func() {
		defer wg.Done()
		for index := range iterations {
			stage := StageOrder[index%len(StageOrder)]
			ps.Update(ProgressEvent{Kind: KindStart, Stage: stage, Total: iterations})
			ps.Update(ProgressEvent{Kind: KindAdvance, Stage: stage, Delta: 1, Total: iterations})
		}
	}()
	go func() {
		defer wg.Done()
		for range iterations {
			ps.Reset()
		}
	}()
	go func() {
		defer wg.Done()
		for range iterations {
			snapshot := ps.Snapshot()
			if len(snapshot) != len(StageOrder) {
				t.Errorf("concurrent snapshot has %d stages, want %d", len(snapshot), len(StageOrder))
				return
			}
			for _, stage := range StageOrder {
				if _, present := snapshot[stage]; !present {
					t.Errorf("concurrent snapshot omitted canonical stage %s", stage)
					return
				}
			}
		}
	}()

	wg.Wait()
}
