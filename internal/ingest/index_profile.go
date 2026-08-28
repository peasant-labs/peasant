package ingest

import (
	"sort"
	"sync"
	"time"
)

const defaultIndexProfileSlowLimit = 10

// IndexProfiler collects opt-in INDEX timing data for one pipeline run.
// It is intentionally in memory only: callers decide how to print or discard it.
type IndexProfiler struct {
	mu       sync.Mutex
	batches  []IndexProfileBatch
	sessions []IndexProfileSession
}

// IndexProfileBatch records aggregate work for one INDEX batch.
type IndexProfileBatch struct {
	Source          string
	Sessions        int
	WorkItems       int
	QueueCapacity   int
	Entries         int
	Bytes           int64
	ParseDuration   time.Duration
	WriteDuration   time.Duration
	MaxParseWorkers int
}

// IndexProfileSession records per-session INDEX timing for slow-tail analysis.
type IndexProfileSession struct {
	SessionID     SessionID
	Harness       Harness
	SourcePath    string
	Outcome       IndexOutcome
	Entries       int
	Bytes         int64
	ParseDuration time.Duration
	WriteDuration time.Duration
}

// TotalDuration is the measured INDEX work time for this session.
func (s IndexProfileSession) TotalDuration() time.Duration {
	return s.ParseDuration + s.WriteDuration
}

// IndexProfileSnapshot is a stable copy of profiler data.
type IndexProfileSnapshot struct {
	Batches      []IndexProfileBatch
	SlowSessions []IndexProfileSession
}

// Record appends one batch and its per-session observations.
func (p *IndexProfiler) Record(batch IndexProfileBatch, sessions []IndexProfileSession) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.batches = append(p.batches, batch)
	p.sessions = append(p.sessions, sessions...)
}

// Snapshot returns all batch observations and the slowest sessions by total time.
func (p *IndexProfiler) Snapshot() IndexProfileSnapshot {
	if p == nil {
		return IndexProfileSnapshot{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	batches := append([]IndexProfileBatch(nil), p.batches...)
	sessions := append([]IndexProfileSession(nil), p.sessions...)
	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].TotalDuration() > sessions[j].TotalDuration()
	})
	if len(sessions) > defaultIndexProfileSlowLimit {
		sessions = sessions[:defaultIndexProfileSlowLimit]
	}
	return IndexProfileSnapshot{Batches: batches, SlowSessions: sessions}
}
