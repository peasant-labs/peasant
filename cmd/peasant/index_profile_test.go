package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
)

func TestPrintIndexProfileShowsStagesAndWriteCauseCounters(t *testing.T) {
	profile := ingest.IndexProfileSnapshot{
		Batches: []ingest.IndexProfileBatch{{
			Sessions:        2,
			WorkItems:       2,
			QueueCapacity:   64,
			Entries:         4,
			Bytes:           128,
			ParseDuration:   2 * time.Second,
			WriteDuration:   3 * time.Second,
			WriteTxs:        1,
			WriteSavepoints: 2,
			WriteSkipped:    1,
			MaxParseWorkers: 2,
			WriteStats: ingest.SessionEntryWriteStats{
				HashMatches:                1,
				HashMisses:                 2,
				FallbackCompares:           3,
				SkippedByHash:              4,
				SkippedByCompare:           5,
				Rewrites:                   6,
				ProjectionRepairRewrites:   7,
				AnnotationRollbackFailures: 8,
				AnnotationTargetsCarried:   9,
				AnnotationTargetsRemapped:  10,
			},
		}},
		Stages: []ingest.IndexProfileStage{{
			Stage:    ingest.StageDiscover,
			Duration: 1500 * time.Millisecond,
			Done:     2,
			Total:    3,
		}},
		Annotation: ingest.AnnotationProfileStats{
			ListEntriesCount:       1,
			ListEntriesTime:        time.Second,
			GetMetricsCount:        2,
			GetMetricsTime:         2 * time.Second,
			ClassifierRunCount:     3,
			ClassifierRunTime:      3 * time.Second,
			ResultCount:            4,
			SessionResultCount:     1,
			EntryResultCount:       3,
			IDCacheHits:            5,
			IDCacheMisses:          6,
			BatchWriteCount:        15,
			BatchWriteTime:         9 * time.Second,
			BatchResultCount:       16,
			BatchErrorCount:        1,
			BatchMutexWaitCount:    17,
			BatchMutexWaitTime:     10 * time.Second,
			BatchConnectionCount:   18,
			BatchConnectionTime:    11 * time.Second,
			BatchSavepointCount:    19,
			BatchSavepointTime:     12 * time.Second,
			BatchDedupLookupCount:  20,
			BatchDedupLookupTime:   13 * time.Second,
			BatchInsertParentCount: 21,
			BatchInsertParentTime:  14 * time.Second,
			BatchInsertTargetCount: 22,
			BatchInsertTargetTime:  15 * time.Second,
			BatchUpdateHashCount:   23,
			BatchUpdateHashTime:    16 * time.Second,
			BatchSupersedeCount:    24,
			BatchSupersedeTime:     17 * time.Second,
			BatchCommitCount:       25,
			BatchCommitTime:        18 * time.Second,
			DedupLookupCount:       7,
			DedupLookupTime:        4 * time.Second,
			CreateSessionCount:     8,
			CreateSessionTime:      5 * time.Second,
			CreateEntryCount:       9,
			CreateEntryTime:        6 * time.Second,
			UpdateContentHashCount: 10,
			UpdateContentHashTime:  7 * time.Second,
			SupersedeCount:         11,
			SupersedeTime:          8 * time.Second,
			DedupSkipCount:         12,
			DedupCreateCount:       13,
			DedupSupersedeCount:    14,
			AnnotationResults: map[ingest.AnnotationProfileBreakdownKey]ingest.AnnotationProfileBreakdown{
				{TypeID: "session_outcome", Value: "resolved", TargetKind: ingest.AnnotationProfileTargetSession}: {
					TypeID: "session_outcome", Value: "resolved", TargetKind: ingest.AnnotationProfileTargetSession, CreateCount: 2,
				},
				{TypeID: "resolution_evidence", Value: "present", TargetKind: ingest.AnnotationProfileTargetEntry}: {
					TypeID: "resolution_evidence", Value: "present", TargetKind: ingest.AnnotationProfileTargetEntry, SkipCount: 1, SupersedeCount: 2, ErrorCount: 1,
				},
			},
		},
	}

	var out bytes.Buffer
	printIndexProfile(&out, profile)
	got := out.String()
	for _, want := range []string{
		"INDEX profile: 1 batches, 2 sessions, 4 entries, 128 bytes",
		"  batch sizes: 2x1",
		"  work items: 2; queue capacity: 64",
		"  write txs: 1; savepoints: 2; skipped rewrites: 1",
		"  write causes:",
		"    hash matches: 1",
		"    hash misses: 2",
		"    fallback compares: 3",
		"    skipped by hash: 4",
		"    skipped by compare: 5",
		"    rewrites: 6",
		"    projection repair rewrites: 7",
		"    annotation rollback failures: 8",
		"    annotation targets carried: 9",
		"    annotation targets remapped: 10",
		"  parse: 2s total; write: 3s total; max parse workers: 2",
		"  stage timings:",
		"    note: stage timings can overlap and do not sum to wall time",
		"    DISCOVER: 1.5s done=2/3",
		"  annotation detail:",
		"    list entries: 1s total; count=1",
		"    get metrics: 2s total; count=2",
		"    classifier run: 3s total; count=3",
		"    results: total=4 session-target=1 entry-target=3",
		"    id cache: hits=5 misses=6",
		"    batch persistence: 9s total; batches=15 results=16 errors=1",
		"    batch persistence detail:",
		"      mutex wait: 10s total; count=17",
		"      connection checkout: 11s total; count=18",
		"      savepoint SQL: 12s total; count=19",
		"      dedup lookup: 13s total; count=20",
		"      insert annotation row: 14s total; count=21",
		"      insert target row: 15s total; count=22",
		"      update content hash: 16s total; count=23",
		"      supersede annotation: 17s total; count=24",
		"      commit: 18s total; count=25",
		"    dedup lookup: 4s total; count=7",
		"    create session annotation: 5s total; count=8",
		"    create entry annotation: 6s total; count=9",
		"    update content hash: 7s total; count=10",
		"    supersede annotation: 8s total; count=11",
		"    dedup decisions: skip=12 create=13 supersede=14",
		"    annotation results by type:",
		"      type=resolution_evidence value=present target=entry total=3 skip=1 create=0 supersede=2 errors=1",
		"      type=session_outcome value=resolved target=session total=2 skip=0 create=2 supersede=0 errors=0",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("profile output missing %q\noutput:\n%s", want, got)
		}
	}
}
