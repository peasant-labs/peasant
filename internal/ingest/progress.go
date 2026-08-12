package ingest

import "sync"

// Stage identifies a pipeline stage for progress reporting.
type Stage string

const (
	StageDiscover Stage = "DISCOVER"
	StageDiff     Stage = "DIFF"
	StageFilter   Stage = "FILTER"
	StageExtract  Stage = "EXTRACT+WRITE"
	StageDBInsert Stage = "DB INSERT"
	StageIndex    Stage = "INDEX"
	StageCompute  Stage = "COMPUTE"
	StageAnnotate Stage = "ANNOTATE"
	StageCleanup  Stage = "CLEANUP"
	StageReport   Stage = "REPORT"
)

// StageOrder is the canonical display order of pipeline stages.
var StageOrder = []Stage{
	StageDiscover,
	StageDiff,
	StageFilter,
	StageExtract,
	StageDBInsert,
	StageIndex,
	StageCompute,
	StageAnnotate,
	StageCleanup,
	StageReport,
}

func (s Stage) String() string { return string(s) }

// EventKind classifies a ProgressEvent.
type EventKind int

const (
	// KindStart marks the beginning of a stage. Total is the expected item count
	// (0 if unknown at start time).
	KindStart EventKind = iota
	// KindAdvance marks incremental progress within a stage. Done is the new
	// cumulative count of completed items.
	KindAdvance
	// KindEnd marks the completion of a stage. Done equals Total on success;
	// Err is non-nil if the stage finished with a fatal error.
	KindEnd
)

// ProgressEvent carries a single progress update from the pipeline.
type ProgressEvent struct {
	Kind  EventKind
	Stage Stage
	// Total is the expected number of items for this stage.
	// For KindStart it is the initial estimate; for KindEnd it is the final count.
	// 0 means unknown / not applicable.
	Total int
	// Done is the number of items completed so far (KindAdvance, KindEnd).
	Done int
	// Err is non-nil only on KindEnd when the stage encountered a fatal error.
	Err error
}

// StageProgress holds the mutable state for a single pipeline stage.
type StageProgress struct {
	Total   int
	Done    int
	Started bool
	Ended   bool
	HasErr  bool
}

// ProgressState is a concurrent-safe aggregation of per-stage progress.
// The pipeline writes via Update; the renderer reads via Snapshot at its own
// tick rate, coalescing any number of intermediate events with zero drops.
type ProgressState struct {
	mu     sync.RWMutex
	stages map[Stage]*StageProgress
}

// NewProgressState creates a ProgressState pre-populated with all pipeline stages.
func NewProgressState() *ProgressState {
	ps := &ProgressState{stages: make(map[Stage]*StageProgress, len(StageOrder))}
	for _, s := range StageOrder {
		ps.stages[s] = &StageProgress{}
	}
	return ps
}

// Update applies a ProgressEvent to the state. Safe for concurrent callers.
func (ps *ProgressState) Update(ev ProgressEvent) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	sp, ok := ps.stages[ev.Stage]
	if !ok {
		return
	}
	switch ev.Kind {
	case KindStart:
		sp.Total = ev.Total
		sp.Done = 0
		sp.Started = true
		sp.Ended = false
		sp.HasErr = false
	case KindAdvance:
		sp.Started = true
		sp.Done = ev.Done
		if ev.Total > sp.Total {
			sp.Total = ev.Total
		}
	case KindEnd:
		sp.Started = true
		sp.Done = ev.Done
		if ev.Total > 0 {
			sp.Total = ev.Total
		}
		sp.Ended = true
		sp.HasErr = ev.Err != nil
	}
}

// Reset clears every stage for a new local-ingest attempt while preserving the
// same concurrent-safe pull source. It never replaces the ProgressState pointer
// held by pipeline and renderer, so retry wiring cannot accidentally observe a
// stale source from the prior attempt.
func (ps *ProgressState) Reset() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, stage := range StageOrder {
		ps.stages[stage] = &StageProgress{}
	}
}

// Snapshot returns a point-in-time copy of all stage progress. Safe for concurrent callers.
func (ps *ProgressState) Snapshot() map[Stage]StageProgress {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	snap := make(map[Stage]StageProgress, len(ps.stages))
	for k, v := range ps.stages {
		snap[k] = *v
	}
	return snap
}

// emitProgress applies a ProgressEvent to ps. If ps is nil, it is a no-op.
func emitProgress(ps *ProgressState, ev ProgressEvent) {
	if ps == nil {
		return
	}
	ps.Update(ev)
}
