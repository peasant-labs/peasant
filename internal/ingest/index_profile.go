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
	stages   []IndexProfileStage
	annotate AnnotationProfileStats
}

const (
	// StagePrepare covers work between DISCOVER and DIFF, including stored-origin
	// resolution and lookup-cache loading. It is profile-only and is not shown by
	// the progress renderer.
	StagePrepare Stage = "PREPARE"
	// StageIndexLog covers index_log persistence after INDEX work completes.
	StageIndexLog Stage = "INDEX LOG"
	// StageAudit covers the final ingest_log write.
	StageAudit Stage = "AUDIT"
)

// IndexProfileStageOrder is the display order for profile-only stage timings.
var IndexProfileStageOrder = []Stage{
	StageDiscover,
	StagePrepare,
	StageDiff,
	StageFilter,
	StageExtract,
	StageDBInsert,
	StageIndex,
	StageIndexLog,
	StageCompute,
	StageAnnotate,
	StageCleanup,
	StageReport,
	StageAudit,
}

// IndexProfileStage records elapsed time for one pipeline stage segment. Some
// stages overlap by design, so stage durations are diagnostic timings and do not
// sum to wall time.
type IndexProfileStage struct {
	Stage    Stage
	Duration time.Duration
	Done     int
	Total    int
}

// SessionEntryWriteStats records the branch decisions made by the INDEX writer.
// Counters, not logs, make warm dirty-corpus profiles explainable.
type SessionEntryWriteStats struct {
	HashMatches                int
	HashMisses                 int
	FallbackCompares           int
	SkippedByHash              int
	SkippedByCompare           int
	Rewrites                   int
	ProjectionRepairRewrites   int
	AnnotationRollbackFailures int
	AnnotationTargetsCarried   int
	AnnotationTargetsRemapped  int
}

// Add folds other into s.
func (s *SessionEntryWriteStats) Add(other SessionEntryWriteStats) {
	s.HashMatches += other.HashMatches
	s.HashMisses += other.HashMisses
	s.FallbackCompares += other.FallbackCompares
	s.SkippedByHash += other.SkippedByHash
	s.SkippedByCompare += other.SkippedByCompare
	s.Rewrites += other.Rewrites
	s.ProjectionRepairRewrites += other.ProjectionRepairRewrites
	s.AnnotationRollbackFailures += other.AnnotationRollbackFailures
	s.AnnotationTargetsCarried += other.AnnotationTargetsCarried
	s.AnnotationTargetsRemapped += other.AnnotationTargetsRemapped
}

// Any reports whether any counter is non-zero.
func (s SessionEntryWriteStats) Any() bool {
	return s.HashMatches != 0 ||
		s.HashMisses != 0 ||
		s.FallbackCompares != 0 ||
		s.SkippedByHash != 0 ||
		s.SkippedByCompare != 0 ||
		s.Rewrites != 0 ||
		s.ProjectionRepairRewrites != 0 ||
		s.AnnotationRollbackFailures != 0 ||
		s.AnnotationTargetsCarried != 0 ||
		s.AnnotationTargetsRemapped != 0
}

// AnnotationProfileStats records aggregate work inside the ANNOTATE stage.
// It is populated only for opt-in index profiles and is safe to add across
// parallel classifier workers through IndexProfiler.RecordAnnotation.
type AnnotationProfileStats struct {
	ListEntriesCount   int
	ListEntriesTime    time.Duration
	GetMetricsCount    int
	GetMetricsTime     time.Duration
	ClassifierRunCount int
	ClassifierRunTime  time.Duration

	ResultCount        int
	SessionResultCount int
	EntryResultCount   int
	StateSkipCount     int
	IDCacheHits        int
	IDCacheMisses      int
	BatchWriteCount    int
	BatchWriteTime     time.Duration
	BatchResultCount   int
	BatchErrorCount    int

	DedupLookupCount       int
	DedupLookupTime        time.Duration
	CreateSessionCount     int
	CreateSessionTime      time.Duration
	CreateEntryCount       int
	CreateEntryTime        time.Duration
	UpdateContentHashCount int
	UpdateContentHashTime  time.Duration
	SupersedeCount         int
	SupersedeTime          time.Duration

	DedupSkipCount      int
	DedupCreateCount    int
	DedupSupersedeCount int
}

// Add folds other into s.
func (s *AnnotationProfileStats) Add(other AnnotationProfileStats) {
	s.ListEntriesCount += other.ListEntriesCount
	s.ListEntriesTime += other.ListEntriesTime
	s.GetMetricsCount += other.GetMetricsCount
	s.GetMetricsTime += other.GetMetricsTime
	s.ClassifierRunCount += other.ClassifierRunCount
	s.ClassifierRunTime += other.ClassifierRunTime
	s.ResultCount += other.ResultCount
	s.SessionResultCount += other.SessionResultCount
	s.EntryResultCount += other.EntryResultCount
	s.StateSkipCount += other.StateSkipCount
	s.IDCacheHits += other.IDCacheHits
	s.IDCacheMisses += other.IDCacheMisses
	s.BatchWriteCount += other.BatchWriteCount
	s.BatchWriteTime += other.BatchWriteTime
	s.BatchResultCount += other.BatchResultCount
	s.BatchErrorCount += other.BatchErrorCount
	s.DedupLookupCount += other.DedupLookupCount
	s.DedupLookupTime += other.DedupLookupTime
	s.CreateSessionCount += other.CreateSessionCount
	s.CreateSessionTime += other.CreateSessionTime
	s.CreateEntryCount += other.CreateEntryCount
	s.CreateEntryTime += other.CreateEntryTime
	s.UpdateContentHashCount += other.UpdateContentHashCount
	s.UpdateContentHashTime += other.UpdateContentHashTime
	s.SupersedeCount += other.SupersedeCount
	s.SupersedeTime += other.SupersedeTime
	s.DedupSkipCount += other.DedupSkipCount
	s.DedupCreateCount += other.DedupCreateCount
	s.DedupSupersedeCount += other.DedupSupersedeCount
}

// Any reports whether any timing or counter is non-zero.
func (s AnnotationProfileStats) Any() bool {
	return s.ListEntriesCount != 0 ||
		s.GetMetricsCount != 0 ||
		s.ClassifierRunCount != 0 ||
		s.ResultCount != 0 ||
		s.SessionResultCount != 0 ||
		s.EntryResultCount != 0 ||
		s.StateSkipCount != 0 ||
		s.IDCacheHits != 0 ||
		s.IDCacheMisses != 0 ||
		s.BatchWriteCount != 0 ||
		s.BatchResultCount != 0 ||
		s.BatchErrorCount != 0 ||
		s.DedupLookupCount != 0 ||
		s.CreateSessionCount != 0 ||
		s.CreateEntryCount != 0 ||
		s.UpdateContentHashCount != 0 ||
		s.SupersedeCount != 0 ||
		s.DedupSkipCount != 0 ||
		s.DedupCreateCount != 0 ||
		s.DedupSupersedeCount != 0
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
	WriteTxs        int
	WriteSavepoints int
	WriteSkipped    int
	WriteStats      SessionEntryWriteStats
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
	Stages       []IndexProfileStage
	Annotation   AnnotationProfileStats
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

// RecordStage appends one stage timing observation.
func (p *IndexProfiler) RecordStage(stage Stage, duration time.Duration, done int, total int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stages = append(p.stages, IndexProfileStage{Stage: stage, Duration: duration, Done: done, Total: total})
}

// RecordAnnotation folds one annotation profile observation into the run total.
func (p *IndexProfiler) RecordAnnotation(stats AnnotationProfileStats) {
	if p == nil || !stats.Any() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.annotate.Add(stats)
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
	stages := append([]IndexProfileStage(nil), p.stages...)
	annotate := p.annotate
	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].TotalDuration() > sessions[j].TotalDuration()
	})
	if len(sessions) > defaultIndexProfileSlowLimit {
		sessions = sessions[:defaultIndexProfileSlowLimit]
	}
	return IndexProfileSnapshot{Batches: batches, SlowSessions: sessions, Stages: stages, Annotation: annotate}
}
