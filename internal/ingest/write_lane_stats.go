package ingest

import (
	"sync"
	"sync/atomic"
	"time"
)

// WriteLaneStats summarizes the serial store-write lane of one run. Wait is
// the time a job spent queued behind other jobs; busy is the time the lane
// spent running jobs. Idle time is in neither.
type WriteLaneStats struct {
	Queued    int           `json:"queued"`     // jobs waiting now
	WaitTotal time.Duration `json:"wait_total"` // summed time jobs spent waiting
	BusyTotal time.Duration `json:"busy_total"` // time the lane spent running jobs
	Jobs      int64         `json:"jobs"`
}

// writeLaneRecorder accumulates lane observations for one run. It is observed
// once per lane job, never per entry: a job is one session write or one batch.
// A nil recorder records nothing and reports zero.
type writeLaneRecorder struct {
	queued atomic.Int64
	mu     sync.Mutex
	stats  WriteLaneStats
	waits  map[Stage]time.Duration
}

func (r *writeLaneRecorder) reset() {
	if r == nil {
		return
	}
	r.queued.Store(0)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats = WriteLaneStats{}
	r.waits = nil
}

// enqueue marks one job as waiting for the lane.
func (r *writeLaneRecorder) enqueue() {
	if r == nil {
		return
	}
	r.queued.Add(1)
}

// observe records one finished job: how long it waited, attributed to the
// stage of its caller, and how long the lane ran it.
func (r *writeLaneRecorder) observe(stage Stage, wait, busy time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats.Jobs++
	r.stats.WaitTotal += max(wait, 0)
	r.stats.BusyTotal += max(busy, 0)
	if r.waits == nil {
		r.waits = make(map[Stage]time.Duration)
	}
	r.waits[stage] += max(wait, 0)
}

// Snapshot returns the totals so far and the jobs waiting now.
func (r *writeLaneRecorder) Snapshot() WriteLaneStats {
	if r == nil {
		return WriteLaneStats{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stats := r.stats
	stats.Queued = int(max(r.queued.Load(), 0))
	return stats
}

// stageWait returns the lane wait attributed to one stage.
func (r *writeLaneRecorder) stageWait(stage Stage) time.Duration {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.waits[stage]
}

// run executes one job through do, which serializes it, and records the wait
// and the busy time around it. now is the clock; production passes time.Now.
func (r *writeLaneRecorder) run(stage Stage, now func() time.Time, do func(func()), job func()) {
	if r == nil {
		do(job)
		return
	}
	r.enqueue()
	enqueued := now()
	do(func() {
		// The job holds the lane now: it is running, not waiting.
		r.queued.Add(-1)
		started := now()
		job()
		r.observe(stage, started.Sub(enqueued), now().Sub(started))
	})
}
