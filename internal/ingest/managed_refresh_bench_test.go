//go:build managed_refresh_bench

// Package ingest_test contains an opt-in, synthetic measurement harness for
// managed-generation refresh after an adapter revision bump. It measures both
// the unchanged settle and ordinary parse paths on the same prepared base.
// Keep this harness to answer whether the refresh saves time and write
// amplification as entry counts grow, while retaining the same authority.
// It is opt-in; build it with -tags managed_refresh_bench.
//
// The harness has two entry points:
//
//   - TestManagedRefreshPrepare imports a deterministic synthetic Claude corpus
//     once into a private managed-generation store, downgrades each stored
//     adapter revision (row and metadata file) to one below the current target,
//     and leaves a reusable base directory behind.
//   - TestManagedRefreshSample copies that base, runs the real Pipeline.Run over
//     it once with the always-on IndexProfiler, verifies the run refreshed the
//     intended managed generations, and writes aggregate, path-free JSON.
//
// Environment knobs:
//
//	PEASANT_MANAGED_REFRESH_PREPARE  base directory to create (prepare only)
//	PEASANT_MANAGED_REFRESH_SESSIONS synthetic sessions (prepare only; default 40)
//	PEASANT_MANAGED_REFRESH_ENTRIES  entries per session (prepare only; default 1000)
//	PEASANT_MANAGED_REFRESH_BASE     prepared base directory (sample only)
//	PEASANT_MANAGED_REFRESH_LANE     retained | native (sample only; default retained)
//	PEASANT_MANAGED_REFRESH_OUT      sample JSON output path (sample only)
//	PEASANT_MANAGED_REFRESH_CLASSIFY 1 | 0: wire the metrics engine and classifier
//	                                 as `peasant harvest` does (default 1)
//
// Example:
//
//	PEASANT_MANAGED_REFRESH_PREPARE=/tmp/opencode/peasant-managed-refresh/base \
//	PEASANT_MANAGED_REFRESH_SESSIONS=200 PEASANT_MANAGED_REFRESH_ENTRIES=3000 \
//	go test -tags managed_refresh_bench ./internal/ingest \
//	  -run '^TestManagedRefreshPrepare$' -v -count=1
//
//	PEASANT_MANAGED_REFRESH_BASE=/tmp/opencode/peasant-managed-refresh/base \
//	PEASANT_MANAGED_REFRESH_LANE=retained \
//	PEASANT_MANAGED_REFRESH_OUT=/tmp/opencode/peasant-managed-refresh/retained-1.json \
//	go test -tags managed_refresh_bench ./internal/ingest \
//	  -run '^TestManagedRefreshSample$' -v -count=1
//
// The retained lane points discovery at an empty directory, so no native session
// is discovered and the stored-metadata refresh is the only path. The native
// lane points discovery back at the synthetic corpus, so the same sessions are
// discovered and captured; the refresh still takes the stored-metadata lane,
// which is what the production decision does for a version-only stamp bump.
// The two lanes are never compared as the same workload.
package ingest_test

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

const (
	managedRefreshShapeFile  = "shape.json"
	managedRefreshSessionFmt = "bec00000-0000-4000-8000-%012d"

	managedRefreshCaseStampOnly = "stamp-only"
	managedRefreshCaseForced    = "forced"

	managedRefreshPathParse   = "parse"
	managedRefreshPathRefresh = "refresh"
)

// managedRefreshShape is the prepared base's description. It carries no
// transcript text and no project path; the sample resolves the corpus from the
// base directory it was given.
type managedRefreshShape struct {
	Sessions           int   `json:"sessions"`
	EntriesPerSession  int   `json:"entriesPerSession"`
	CorpusBytes        int64 `json:"corpusBytes"`
	TargetAdapter      int   `json:"targetAdapter"`
	StoredAdapter      int   `json:"storedAdapter"`
	InitialImportMs    int64 `json:"initialImportMs"`
	InitialEntries     int   `json:"initialEntries"`
	InitialGenerations int64 `json:"initialGenerations"`
	// Case selects the measured workload. "stamp-only" reproduces the common
	// adapter bump whose projection is identical, so the parse baseline skips
	// the generation write and the unchanged proof settles. "forced" also
	// changes the project name in every stored metadata file, so the proof
	// fails and the run parses and installs a new generation; only the parse
	// path measures it.
	Case string `json:"case"`
	// ProjectName is the metadata-only change the prepare step forces into
	// every stored metadata file. Empty for the stamp-only case.
	ProjectName string `json:"projectName"`
}

type managedRefreshTiming struct {
	Count    int     `json:"count"`
	TotalMs  float64 `json:"totalMs"`
	MedianMs float64 `json:"medianMs"`
	P95Ms    float64 `json:"p95Ms"`
	MaxMs    float64 `json:"maxMs"`
}

type managedRefreshStage struct {
	Stage      string  `json:"stage"`
	Ms         float64 `json:"ms"`
	Done       int     `json:"done"`
	Total      int     `json:"total"`
	Bytes      int64   `json:"bytes"`
	LaneWaitMs float64 `json:"laneWaitMs"`
}

type managedRefreshSummary struct {
	New       int `json:"new"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Active    int `json:"active"`
	Indexed   int `json:"indexed"`
	Errors    int `json:"errors"`
}

type managedRefreshRowCount struct {
	Before int64 `json:"before"`
	After  int64 `json:"after"`
}

type managedRefreshDB struct {
	DBBytesBefore       int64 `json:"dbBytesBefore"`
	WALBytesBefore      int64 `json:"walBytesBefore"`
	DBBytesAfterRun     int64 `json:"dbBytesAfterRun"`
	WALBytesAfterRun    int64 `json:"walBytesAfterRun"`
	DBBytesAfterSweep   int64 `json:"dbBytesAfterSweep"`
	WALBytesAfterSweep  int64 `json:"walBytesAfterSweep"`
	DBBytesAfterClose   int64 `json:"dbBytesAfterClose"`
	WALBytesAfterClose  int64 `json:"walBytesAfterClose"`
	PageCountBefore     int64 `json:"pageCountBefore"`
	PageCountAfterRun   int64 `json:"pageCountAfterRun"`
	PageCountAfterSweep int64 `json:"pageCountAfterSweep"`
	PageSize            int64 `json:"pageSize"`
	FreelistBefore      int64 `json:"freelistBefore"`
	FreelistAfterRun    int64 `json:"freelistAfterRun"`
	FreelistAfterSweep  int64 `json:"freelistAfterSweep"`
}

type managedRefreshSample struct {
	Lane                  string                            `json:"lane"`
	Sessions              int                               `json:"sessions"`
	EntriesPerSession     int                               `json:"entriesPerSession"`
	CorpusBytes           int64                             `json:"corpusBytes"`
	WallMs                float64                           `json:"wallMs"`
	RunMs                 float64                           `json:"runMs"`
	SetupCopyMs           float64                           `json:"setupCopyMs"`
	SetupOpenMs           float64                           `json:"setupOpenMs"`
	Summary               managedRefreshSummary             `json:"summary"`
	Census                []ingest.RefreshCensusRow         `json:"census"`
	CensusSessions        int                               `json:"censusSessions"`
	Outcomes              map[string]int                    `json:"outcomes"`
	OutcomeReasons        map[string]int                    `json:"outcomeReasons"`
	Stages                []managedRefreshStage             `json:"stages"`
	Parse                 managedRefreshTiming              `json:"parse"`
	Write                 managedRefreshTiming              `json:"write"`
	Entries               int                               `json:"entries"`
	ParsedBytes           int64                             `json:"parsedBytes"`
	ParsedSessions        int                               `json:"parsedSessions"`
	GenerationWrites      int                               `json:"generationWrites"`
	GenerationWriteMs     float64                           `json:"generationWriteMs"`
	IndexBatches          int                               `json:"indexBatches"`
	BatchSessions         int                               `json:"batchSessions"`
	BatchEntries          int                               `json:"batchEntries"`
	BatchBytes            int64                             `json:"batchBytes"`
	BatchParseMs          float64                           `json:"batchParseMs"`
	BatchWriteMs          float64                           `json:"batchWriteMs"`
	WriteTxs              int                               `json:"writeTxs"`
	WriteSavepoints       int                               `json:"writeSavepoints"`
	WriteSkipped          int                               `json:"writeSkipped"`
	WriteStats            ingest.SessionEntryWriteStats     `json:"writeStats"`
	WriteLane             *ingest.WriteLaneStats            `json:"writeLane"`
	StagedPeakBytes       int64                             `json:"stagedPeakBytes"`
	FlushWaitCount        int                               `json:"flushWaitCount"`
	MaxParseWorkers       int                               `json:"maxParseWorkers"`
	QueueCapacity         int                               `json:"queueCapacity"`
	GOMAXPROCS            int                               `json:"gomaxprocs"`
	NumCPU                int                               `json:"numCPU"`
	DB                    managedRefreshDB                  `json:"db"`
	Rows                  map[string]managedRefreshRowCount `json:"rows"`
	RowsAfterSweep        map[string]managedRefreshRowCount `json:"rowsAfterSweep"`
	StatsUpdatedBefore    int64                             `json:"statsUpdatedBefore"`
	StatsUpdatedAfter     int64                             `json:"statsUpdatedAfter"`
	PublicationBound      int                               `json:"publicationBound"`
	GenerationInstalls    int                               `json:"generationInstalls"`
	GenerationsAdded      int64                             `json:"generationsAdded"`
	GenerationsAfterSweep int64                             `json:"generationsAfterSweep"`
	SweepMs               float64                           `json:"sweepMs"`
	AdapterSessions       int                               `json:"adapterSessions"`
	MetadataFiles         int                               `json:"metadataFiles"`
	MetadataAtTarget      int                               `json:"metadataAtTarget"`
	Notes                 []string                          `json:"notes"`
}

type managedRefreshDBStats struct {
	pageCount int64
	pageSize  int64
	freelist  int64
}

// TestManagedRefreshPrepare builds the reusable base. It runs once per corpus
// shape and refuses to overwrite an existing base.
func TestManagedRefreshPrepare(t *testing.T) {
	base := os.Getenv("PEASANT_MANAGED_REFRESH_PREPARE")
	if base == "" {
		t.Skip("set PEASANT_MANAGED_REFRESH_PREPARE to create a managed-refresh base")
	}
	sessions := managedRefreshEnvInt(t, "PEASANT_MANAGED_REFRESH_SESSIONS", 40)
	entries := managedRefreshEnvInt(t, "PEASANT_MANAGED_REFRESH_ENTRIES", 1000)
	if _, err := os.Stat(filepath.Join(base, managedRefreshShapeFile)); err == nil {
		t.Fatalf("base %s already holds a prepared shape; choose a new directory", base)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(base, "source")
	output := filepath.Join(base, "output")
	dbDir := filepath.Join(base, "db")
	for _, dir := range []string{source, output, dbDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	corpusBytes := writeSyntheticClaudeCorpus(t, source, sessions, entries)
	dbPath := filepath.Join(dbDir, "peasant.db")
	db := openManagedRefreshStore(t, dbPath, output)
	profiler := &ingest.IndexProfiler{}
	pipeline := newManagedRefreshPipeline(t, db, source, output, profiler, true)
	start := time.Now()
	result, err := pipeline.Run(t.Context())
	importMs := time.Since(start).Milliseconds()
	if err != nil {
		_ = db.Close()
		t.Fatalf("initial import: %v", err)
	}
	if result.Summary.New != sessions || result.Summary.Errors != 0 {
		_ = db.Close()
		t.Fatalf("initial import summary = %+v, want %d new sessions and no errors", result.Summary, sessions)
	}
	initialEntries := 0
	for _, entry := range result.IndexLog {
		initialEntries += entry.EntriesCount
	}
	generations := managedRefreshQueryInt(t, db, `SELECT count(*) FROM session_generations`)
	active := managedRefreshQueryInt(t, db, `SELECT count(*) FROM sessions WHERE active_generation_id IS NOT NULL`)
	if int(active) != sessions || int(generations) < sessions {
		_ = db.Close()
		t.Fatalf("initial import stored %d active generations and %d generation rows for %d sessions; want one managed generation per session", active, generations, sessions)
	}

	target := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode].AdapterVersion
	stored := target - 1
	if stored < 1 {
		_ = db.Close()
		t.Fatalf("claude adapter target %d cannot be downgraded to a positive revision", target)
	}
	managedRefreshDowngradeAdapter(t, db, stored)
	caseName := managedRefreshCase(t)
	projectName := ""
	if caseName == managedRefreshCaseForced {
		projectName = "managed refresh benchmark forced context"
	}
	metadata := managedRefreshStampMetadata(t, output, stored, projectName)
	if metadata != sessions {
		_ = db.Close()
		t.Fatalf("downgraded %d metadata files, want %d", metadata, sessions)
	}
	if got := managedRefreshCountMetadataRevision(t, output, stored); got != sessions {
		_ = db.Close()
		t.Fatalf("%d metadata files carry adapter revision %d after downgrade, want %d", got, stored, sessions)
	}
	if rows := managedRefreshQueryInt(t, db, `SELECT count(*) FROM sessions WHERE adapter_version = ?`, stored); int(rows) != sessions {
		_ = db.Close()
		t.Fatalf("downgraded %d session rows, want %d", rows, sessions)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	shape := managedRefreshShape{
		Sessions: sessions, EntriesPerSession: entries, CorpusBytes: corpusBytes,
		TargetAdapter: target, StoredAdapter: stored,
		InitialImportMs: importMs, InitialEntries: initialEntries,
		InitialGenerations: generations, Case: caseName, ProjectName: projectName,
	}
	data, err := json.MarshalIndent(shape, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, managedRefreshShapeFile), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("prepared %d sessions x %d entries (%d bytes) in %d ms; adapter %d -> %d; %d initial entries",
		sessions, entries, corpusBytes, importMs, target, stored, initialEntries)
}

// TestManagedRefreshSample measures one refresh over a prepared base and writes
// aggregate JSON. It is the primary harness entry point.
func TestManagedRefreshSample(t *testing.T) {
	base := os.Getenv("PEASANT_MANAGED_REFRESH_BASE")
	if base == "" {
		t.Skip("set PEASANT_MANAGED_REFRESH_BASE to a prepared managed-refresh base")
	}
	lane := managedRefreshLane(t)
	out := os.Getenv("PEASANT_MANAGED_REFRESH_OUT")
	if out == "" {
		t.Skip("set PEASANT_MANAGED_REFRESH_OUT to the sample JSON path")
	}
	classify := managedRefreshEnvBool(t, "PEASANT_MANAGED_REFRESH_CLASSIFY", true)
	sample := runManagedRefreshSample(t, base, lane, out, classify)
	t.Logf("%s lane: wall %.0f ms, parse %.0f ms, write %.0f ms, entries %d, generation delta %d, outcomes %v",
		lane, sample.WallMs, sample.Parse.TotalMs, sample.Write.TotalMs, sample.Entries, sample.GenerationsAdded, sample.Outcomes)
}

// BenchmarkManagedRefresh exposes the same one-shot sample to `-bench` users.
// It deliberately requires exactly one iteration; the test entry point above is
// the durable runner.
func BenchmarkManagedRefresh(b *testing.B) {
	base := os.Getenv("PEASANT_MANAGED_REFRESH_BASE")
	if base == "" {
		b.Skip("set PEASANT_MANAGED_REFRESH_BASE to a prepared managed-refresh base")
	}
	out := os.Getenv("PEASANT_MANAGED_REFRESH_OUT")
	if out == "" {
		b.Skip("set PEASANT_MANAGED_REFRESH_OUT to the sample JSON path")
	}
	lane := managedRefreshLane(b)
	classify := managedRefreshEnvBool(b, "PEASANT_MANAGED_REFRESH_CLASSIFY", true)
	if b.N != 1 {
		b.Fatalf("BenchmarkManagedRefresh measures one full refresh; run it with -benchtime 1x (got b.N=%d)", b.N)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		start := time.Now()
		sample := runManagedRefreshSample(b, base, lane, out, classify)
		b.ReportMetric(sample.WallMs, "wall-ms")
		b.ReportMetric(sample.Parse.TotalMs, "parse-ms")
		b.ReportMetric(sample.Write.TotalMs, "write-ms")
		b.ReportMetric(float64(sample.Entries), "entries")
		b.ReportMetric(float64(time.Since(start).Milliseconds()), "whole-harness-ms")
	}
}

const (
	managedRefreshSourceNative   = "native"
	managedRefreshSourceRetained = "retained"
)

func runManagedRefreshSample(tb testing.TB, base, lane, out string, classify bool) managedRefreshSample {
	tb.Helper()
	shapeData, err := os.ReadFile(filepath.Join(base, managedRefreshShapeFile))
	if err != nil {
		tb.Fatalf("read prepared shape: %v", err)
	}
	var shape managedRefreshShape
	if err := json.Unmarshal(shapeData, &shape); err != nil {
		tb.Fatalf("decode prepared shape: %v", err)
	}
	if shape.Sessions < 1 || shape.EntriesPerSession < 1 {
		tb.Fatalf("prepared shape declares %d sessions x %d entries", shape.Sessions, shape.EntriesPerSession)
	}

	sampleDir := tb.TempDir()
	copyStart := time.Now()
	copyErr := managedRefreshCopyTree(filepath.Join(base, "db"), filepath.Join(sampleDir, "db"))
	if copyErr == nil {
		copyErr = managedRefreshCopyTree(filepath.Join(base, "output"), filepath.Join(sampleDir, "output"))
	}
	if copyErr != nil {
		tb.Fatalf("copy prepared base: %v", copyErr)
	}
	copyMs := float64(time.Since(copyStart).Microseconds()) / 1000
	output := filepath.Join(sampleDir, "output")
	dbPath := filepath.Join(sampleDir, "db", "peasant.db")
	source := filepath.Join(sampleDir, "empty-source")
	if lane == managedRefreshSourceNative {
		source = filepath.Join(base, "source")
	} else if err := os.MkdirAll(source, 0o700); err != nil {
		tb.Fatal(err)
	}

	openStart := time.Now()
	db := openManagedRefreshStore(tb, dbPath, output)
	openMs := float64(time.Since(openStart).Microseconds()) / 1000
	profiler := &ingest.IndexProfiler{}
	pipeline := newManagedRefreshPipeline(tb, db, source, output, profiler, classify)
	path := managedRefreshPath(tb)
	if path == managedRefreshPathRefresh && shape.Case == managedRefreshCaseForced {
		_ = db.Close()
		tb.Fatalf("%s sample: the forced case changes the metadata context, so the unchanged proof fails and the run parses; no path settles it without a parse; measure it with PEASANT_MANAGED_REFRESH_PATH=%s", lane, managedRefreshPathParse)
	}

	rowsBefore := managedRefreshRowCounts(tb, db)
	pageBefore := managedRefreshPageStats(tb, db)
	statsUpdatedBefore := managedRefreshQueryInt(tb, db, `SELECT COALESCE(MAX(updated_at_ms), 0) FROM session_captured_stats`)
	dbBefore, walBefore := managedRefreshFileSizes(tb, dbPath)

	wallStart := time.Now()
	result, err := pipeline.Run(tb.Context())
	wall := time.Since(wallStart)
	if err != nil {
		_ = db.Close()
		tb.Fatalf("%s refresh: %v", lane, err)
	}
	dbAfterRun, walAfterRun := managedRefreshFileSizes(tb, dbPath)
	pageAfterRun := managedRefreshPageStats(tb, db)
	rowsAfterRun := managedRefreshRowCounts(tb, db)
	adapterSessions := managedRefreshQueryInt(tb, db, `SELECT count(*) FROM sessions WHERE adapter_version = ?`, shape.TargetAdapter)
	metadataFiles, metadataAtTarget := managedRefreshCountMetadata(tb, output)
	projectNames := managedRefreshActiveProjectNames(tb, db)
	statsUpdatedAfter := managedRefreshQueryInt(tb, db, `SELECT COALESCE(MAX(updated_at_ms), 0) FROM session_captured_stats`)
	publicationBound := int(managedRefreshQueryInt(tb, db, `SELECT count(*) FROM sessions s
JOIN session_publication_metadata p ON p.session_id = s.session_id
WHERE p.capture_revision > 0
  AND p.capture_revision = s.publication_capture_revision
  AND p.capture_revision = s.indexed_publication_capture_revision
  AND s.cwd_provenance_kind != 'not_recovered'`))
	// The parse path sweeps a superseded generation inside the run; the refresh
	// path flags it for the next sweep. Run that sweep here, outside the timed
	// region, so the post-sweep DB size and row counts compare the same steady
	// state on both paths. The pre-sweep values are kept separately to show the
	// transient cost.
	sweepStart := time.Now()
	_, sweepWarnings, sweepErr := db.SweepFlaggedSessionsForHarvest(tb.Context(), ingest.AllStoredSessions())
	sweepMs := float64(time.Since(sweepStart).Microseconds()) / 1000
	if sweepErr != nil {
		_ = db.Close()
		tb.Fatalf("%s post-run sweep: %v", lane, sweepErr)
	}
	if len(sweepWarnings) != 0 {
		tb.Logf("%s post-run sweep warnings: %v", lane, sweepWarnings)
	}
	dbAfterSweep, walAfterSweep := managedRefreshFileSizes(tb, dbPath)
	pageAfterSweep := managedRefreshPageStats(tb, db)
	rowsAfterSweep := managedRefreshRowCounts(tb, db)
	_ = db.Close()
	dbClosed, walClosed := managedRefreshFileSizes(tb, dbPath)

	sample := managedRefreshSample{
		Lane: lane, Sessions: shape.Sessions, EntriesPerSession: shape.EntriesPerSession,
		CorpusBytes: shape.CorpusBytes,
		WallMs:      float64(wall.Microseconds()) / 1000,
		RunMs:       float64(result.Duration.Microseconds()) / 1000,
		SetupCopyMs: copyMs, SetupOpenMs: openMs,
		Summary: managedRefreshSummary{
			New: result.Summary.New, Updated: result.Summary.Updated, Unchanged: result.Summary.Unchanged,
			Active: result.Summary.Active, Indexed: result.Summary.Indexed, Errors: result.Summary.Errors,
		},
		Outcomes:              map[string]int{},
		OutcomeReasons:        map[string]int{},
		Rows:                  make(map[string]managedRefreshRowCount, len(rowsBefore)),
		RowsAfterSweep:        make(map[string]managedRefreshRowCount, len(rowsBefore)),
		StatsUpdatedBefore:    statsUpdatedBefore,
		StatsUpdatedAfter:     statsUpdatedAfter,
		PublicationBound:      publicationBound,
		GenerationInstalls:    0,
		GenerationsAdded:      rowsAfterRun["session_generations"] - rowsBefore["session_generations"],
		GenerationsAfterSweep: rowsAfterSweep["session_generations"] - rowsBefore["session_generations"],
		SweepMs:               sweepMs,
		AdapterSessions:       int(adapterSessions),
		MetadataFiles:         metadataFiles,
		MetadataAtTarget:      metadataAtTarget,
		GOMAXPROCS:            runtime.GOMAXPROCS(0),
		NumCPU:                runtime.NumCPU(),
		DB: managedRefreshDB{
			DBBytesBefore: dbBefore, WALBytesBefore: walBefore,
			DBBytesAfterRun: dbAfterRun, WALBytesAfterRun: walAfterRun,
			DBBytesAfterSweep: dbAfterSweep, WALBytesAfterSweep: walAfterSweep,
			DBBytesAfterClose: dbClosed, WALBytesAfterClose: walClosed,
			PageCountBefore: pageBefore.pageCount, PageCountAfterRun: pageAfterRun.pageCount,
			PageCountAfterSweep: pageAfterSweep.pageCount, PageSize: pageAfterSweep.pageSize,
			FreelistBefore: pageBefore.freelist, FreelistAfterRun: pageAfterRun.freelist,
			FreelistAfterSweep: pageAfterSweep.freelist,
		},
	}
	for table, before := range rowsBefore {
		sample.Rows[table] = managedRefreshRowCount{Before: before, After: rowsAfterRun[table]}
		sample.RowsAfterSweep[table] = managedRefreshRowCount{Before: before, After: rowsAfterSweep[table]}
	}
	if result.Census != nil {
		sample.Census = append(sample.Census, result.Census.Rows...)
		sample.CensusSessions = result.Census.Sessions()
	}
	var parseDurations, writeDurations []float64
	for _, entry := range result.IndexLog {
		sample.Outcomes[string(entry.Outcome)]++
		if entry.Reason != nil {
			sample.OutcomeReasons[*entry.Reason]++
		}
		sample.Entries += entry.EntriesCount
		if entry.Bytes != nil {
			sample.ParsedBytes += *entry.Bytes
		}
		if entry.ParseDuration != nil {
			duration := float64(entry.ParseDuration.Microseconds()) / 1000
			parseDurations = append(parseDurations, duration)
			if duration > 0 {
				sample.ParsedSessions++
			}
		}
		if entry.WriteDuration != nil {
			duration := float64(entry.WriteDuration.Microseconds()) / 1000
			writeDurations = append(writeDurations, duration)
			if duration > 0 {
				sample.GenerationWrites++
			}
		}
	}
	sample.Parse = managedRefreshTimingFrom(parseDurations)
	sample.Write = managedRefreshTimingFrom(writeDurations)
	sample.GenerationWriteMs = sample.Write.TotalMs
	snapshot := profiler.Snapshot()
	for _, batch := range snapshot.Batches {
		sample.IndexBatches++
		sample.MaxParseWorkers = max(sample.MaxParseWorkers, batch.MaxParseWorkers)
		sample.QueueCapacity = max(sample.QueueCapacity, batch.QueueCapacity)
		sample.BatchSessions += batch.Sessions
		sample.BatchEntries += batch.Entries
		sample.BatchBytes += batch.Bytes
		sample.BatchParseMs += float64(batch.ParseDuration.Microseconds()) / 1000
		sample.BatchWriteMs += float64(batch.WriteDuration.Microseconds()) / 1000
		sample.WriteTxs += batch.WriteTxs
		sample.WriteSavepoints += batch.WriteSavepoints
		sample.WriteSkipped += batch.WriteSkipped
		sample.WriteStats.Add(batch.WriteStats)
	}
	for _, stage := range profiler.StageTotals() {
		sample.Stages = append(sample.Stages, managedRefreshStage{
			Stage: stage.Stage.String(), Ms: float64(stage.Duration.Microseconds()) / 1000,
			Done: stage.Done, Total: stage.Total, Bytes: stage.Bytes,
			LaneWaitMs: float64(stage.LaneWait.Microseconds()) / 1000,
		})
	}
	sample.StagedPeakBytes = snapshot.StagedPeakBytes
	sample.FlushWaitCount = snapshot.FlushWaitCount
	if result.WriteLane != nil {
		laneStats := *result.WriteLane
		sample.WriteLane = &laneStats
	}

	// The harness runs the same prepared base through either the ordinary parse
	// path or the unchanged settle path. The assertions differ only by the
	// path and the prepared case; the measured metrics are identical.
	if sample.Summary.Errors != 0 || len(result.Diagnostics) != 0 {
		tb.Fatalf("%s refresh reported %d errors and %d diagnostics", lane, sample.Summary.Errors, len(result.Diagnostics))
	}
	if sample.Summary.Updated != shape.Sessions || sample.Summary.Unchanged != 0 || sample.Summary.New != 0 {
		tb.Fatalf("%s refresh summary = %+v, want %d updated, 0 unchanged, 0 new", lane, sample.Summary, shape.Sessions)
	}
	if sample.CensusSessions != shape.Sessions {
		tb.Fatalf("%s refresh census covers %d sessions, want %d (%+v)", lane, sample.CensusSessions, shape.Sessions, sample.Census)
	}
	for _, row := range sample.Census {
		if row.Reason != ingest.OutcomeReasonRefreshAdapterBehind || row.Lane != ingest.RefreshLaneStoredMetadata {
			tb.Fatalf("%s refresh census row = %+v, want adapter_behind on the stored-metadata lane", lane, row)
		}
	}
	if len(result.IndexLog) != shape.Sessions {
		tb.Fatalf("%s refresh produced %d index rows, want %d", lane, len(result.IndexLog), shape.Sessions)
	}
	for _, entry := range result.IndexLog {
		if entry.Outcome == ingest.IndexOutcomeError || entry.Outcome == ingest.IndexOutcomeFallback {
			tb.Fatalf("%s refresh index outcome for session %s is %q (%v)", lane, entry.SessionID, entry.Outcome, entry.ErrorMessage)
		}
	}
	if sample.AdapterSessions != shape.Sessions {
		tb.Fatalf("%s refresh left %d session rows at adapter %d, want %d", lane, sample.AdapterSessions, shape.TargetAdapter, shape.Sessions)
	}
	if sample.MetadataFiles != shape.Sessions || sample.MetadataAtTarget != shape.Sessions {
		tb.Fatalf("%s refresh left %d metadata files, %d at adapter %d; want %d at the target", lane, sample.MetadataFiles, sample.MetadataAtTarget, shape.TargetAdapter, shape.Sessions)
	}
	if len(projectNames) != shape.Sessions {
		tb.Fatalf("%s refresh left %d active managed generations, want %d", lane, len(projectNames), shape.Sessions)
	}
	if sample.PublicationBound != shape.Sessions {
		tb.Fatalf("%s refresh left %d bound sessions, want %d", lane, sample.PublicationBound, shape.Sessions)
	}
	if shape.Case == managedRefreshCaseForced {
		for _, name := range projectNames {
			if name != shape.ProjectName {
				tb.Fatalf("%s refreshed generation project = %q, want the forced metadata change %q", lane, name, shape.ProjectName)
			}
		}
	} else {
		for _, name := range projectNames {
			if name == "" {
				tb.Fatalf("%s refreshed generation carries no project name", lane)
			}
		}
	}

	switch path {
	case managedRefreshPathParse:
		if sample.Summary.Indexed != shape.Sessions {
			tb.Fatalf("%s parse index count = %d, want %d", lane, sample.Summary.Indexed, shape.Sessions)
		}
		if sample.ParsedSessions != shape.Sessions {
			tb.Fatalf("%s parse parsed %d sessions, want %d", lane, sample.ParsedSessions, shape.Sessions)
		}
		if sample.Entries <= 0 || sample.ParsedBytes <= 0 || sample.Parse.TotalMs <= 0 {
			tb.Fatalf("%s parse parsed %d entries, %d bytes and %.0f ms; want parse work for every session", lane, sample.Entries, sample.ParsedBytes, sample.Parse.TotalMs)
		}
		switch shape.Case {
		case managedRefreshCaseStampOnly:
			sample.GenerationInstalls = 0
			if sample.GenerationsAdded != 0 {
				tb.Fatalf("%s parse added %d generation rows, want 0 for an identical stamp-only projection", lane, sample.GenerationsAdded)
			}
			if sample.Outcomes[string(ingest.IndexOutcomeSkipped)] != shape.Sessions {
				tb.Fatalf("%s parse skipped outcomes = %d, want %d (%v)", lane, sample.Outcomes[string(ingest.IndexOutcomeSkipped)], shape.Sessions, sample.Outcomes)
			}
		case managedRefreshCaseForced:
			sample.GenerationInstalls = sample.Summary.Indexed
			if sample.GenerationInstalls != shape.Sessions {
				tb.Fatalf("%s parse installed %d generations, want %d", lane, sample.GenerationInstalls, shape.Sessions)
			}
			if sample.Outcomes[string(ingest.IndexOutcomeIndexed)] != shape.Sessions {
				tb.Fatalf("%s parse indexed outcomes = %d, want %d (%v)", lane, sample.Outcomes[string(ingest.IndexOutcomeIndexed)], shape.Sessions, sample.Outcomes)
			}
			if sample.GenerationWrites == 0 {
				sample.Notes = append(sample.Notes, "the parse baseline's generation activation write is included in writer-lane busy but is not separately timed by the index log")
			}
			if sample.GenerationsAdded != 0 {
				sample.Notes = append(sample.Notes, "the parse run installed a generation and swept the superseded one inside the run, so the pre-sweep net generation-row delta is zero")
			}
		default:
			tb.Fatalf("%s prepared case %q is not %s or %s", lane, shape.Case, managedRefreshCaseStampOnly, managedRefreshCaseForced)
		}
	case managedRefreshPathRefresh:
		if sample.Summary.Indexed != 0 {
			tb.Fatalf("%s refresh indexed %d sessions, want 0", lane, sample.Summary.Indexed)
		}
		if sample.ParsedSessions != 0 || sample.Entries != 0 || sample.ParsedBytes != 0 || sample.Parse.TotalMs != 0 {
			tb.Fatalf("%s refresh parsed %d sessions, %d entries and %d bytes in %.0f ms; want no parse", lane, sample.ParsedSessions, sample.Entries, sample.ParsedBytes, sample.Parse.TotalMs)
		}
		if sample.GenerationWrites != shape.Sessions || len(writeDurations) != shape.Sessions {
			tb.Fatalf("%s refresh measured %d generation writes and %d timings, want %d", lane, sample.GenerationWrites, len(writeDurations), shape.Sessions)
		}
		if sample.Outcomes[string(ingest.IndexOutcomeSkipped)] != shape.Sessions {
			tb.Fatalf("%s refresh skipped outcomes = %d, want %d (%v)", lane, sample.Outcomes[string(ingest.IndexOutcomeSkipped)], shape.Sessions, sample.Outcomes)
		}
		settles := managedRefreshDispositionCount(sample.OutcomeReasons, "unchanged")
		if settles != shape.Sessions {
			tb.Fatalf("%s refresh disposition %q count = %d, want %d (%v)", lane, "unchanged", settles, shape.Sessions, sample.OutcomeReasons)
		}
		sample.GenerationInstalls = 0
		if sample.StatsUpdatedAfter <= sample.StatsUpdatedBefore {
			tb.Fatalf("%s refresh did not settle captured stats: %d <= %d", lane, sample.StatsUpdatedAfter, sample.StatsUpdatedBefore)
		}
		if sample.GenerationsAdded != 0 {
			tb.Fatalf("%s refresh added %d generation rows, want 0 for the unchanged case", lane, sample.GenerationsAdded)
		}
		sample.Notes = append(sample.Notes, "the unchanged proof held, parsed nothing, installed no generation, and settled stats and binding")
	default:
		tb.Fatalf("%s measurement path %q is not %s or %s", lane, path, managedRefreshPathParse, managedRefreshPathRefresh)
	}
	if sample.GenerationsAfterSweep != 0 {
		tb.Fatalf("%s post-run sweep left %d net generation rows, want the same one-generation steady state", lane, sample.GenerationsAfterSweep)
	}

	managedRefreshWriteJSON(tb, out, sample)
	return sample
}

func newManagedRefreshPipeline(tb testing.TB, db *store.Store, source, output string, profiler *ingest.IndexProfiler, classify bool) *ingest.Pipeline {
	tb.Helper()
	fs := &ingest.OSFileSystem{}
	cfg := ingest.PipelineConfig{
		Sources: map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessClaudeCode: {Enabled: true, Paths: []ingest.ResolvedPath{ingest.ResolvedPath(source)}},
		},
		OutputDir:     ingest.ResolvedPath(output),
		IndexProfiler: profiler,
	}
	options := []ingest.PipelineOption{
		ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithSalt(db.InstallationSalt()),
		ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
		ingest.WithLogger(db), ingest.WithIndexLogger(db),
	}
	if classify {
		options = append(options,
			ingest.WithAnalyzer(metrics.NewEngineWithModels(db, db)),
			ingest.WithClassifier(metrics.NewClassifierAnnotator(db, db)))
	}
	pipeline, err := ingest.NewPipeline(fs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg, options...)
	if err != nil {
		tb.Fatalf("build refresh pipeline: %v", err)
	}
	return pipeline
}

func openManagedRefreshStore(tb testing.TB, dbPath, output string) *store.Store {
	tb.Helper()
	options, err := store.ManagedGenerationOptions(output)
	if err != nil {
		tb.Fatalf("managed-generation options: %v", err)
	}
	db, err := store.Open(dbPath, options...)
	if err != nil {
		tb.Fatalf("open managed-generation store: %v", err)
	}
	return db
}

// writeSyntheticClaudeCorpus writes one deterministic JSONL file per session.
// The text is synthetic and carries no user data.
func writeSyntheticClaudeCorpus(tb testing.TB, root string, sessions, entries int) int64 {
	tb.Helper()
	filler := strings.Repeat("synthetic transcript text for the managed refresh measurement. ", 12)
	var total int64
	workspace := filepath.Join(root, "-workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		tb.Fatal(err)
	}
	for n := range sessions {
		id := fmt.Sprintf(managedRefreshSessionFmt, n+1)
		path := filepath.Join(workspace, id+".jsonl")
		content := syntheticClaudeTranscript(id, entries, filler)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			tb.Fatal(err)
		}
		stamp := time.Unix(1_700_000_000+int64(n), 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			tb.Fatal(err)
		}
		total += int64(len(content))
	}
	return total
}

func syntheticClaudeTranscript(sessionID string, entries int, filler string) string {
	var content strings.Builder
	for n := range entries {
		at := time.Unix(1_700_000_000+int64(n), 0).UTC().Format(time.RFC3339)
		if n%2 == 0 {
			fmt.Fprintf(&content, `{"type":"user","sessionId":"%s","uuid":"u%d","cwd":"/work/project","timestamp":"%s","message":{"role":"user","content":"prompt %d: %s"}}`+"\n", sessionID, n, at, n, filler)
			continue
		}
		fmt.Fprintf(&content, `{"type":"assistant","sessionId":"%s","uuid":"a%d","parentUuid":"u%d","cwd":"/work/project","timestamp":"%s","message":{"role":"assistant","model":"claude-synthetic","content":[{"type":"text","text":"reply %d: %s"}],"usage":{"input_tokens":120,"output_tokens":80}}}`+"\n", sessionID, n, n-1, at, n, filler)
	}
	return content.String()
}

// managedRefreshDowngradeAdapter sets every stored session row's adapter
// revision below the current target, as an older build would have left it.
func managedRefreshDowngradeAdapter(tb testing.TB, db *store.Store, stored int) {
	tb.Helper()
	conn, err := db.Pool().Take(tb.Context())
	if err != nil {
		tb.Fatal(err)
	}
	defer db.Pool().Put(conn)
	if err := sqlitex.Execute(conn, `UPDATE sessions SET adapter_version = ?`, &sqlitex.ExecOptions{Args: []any{stored}}); err != nil {
		tb.Fatalf("downgrade session adapter rows: %v", err)
	}
}

// managedRefreshStampMetadata rewrites every managed metadata file's adapter
// revision, forces a context change when a project name is given, and
// recomputes the metadata hash. A stamp-only change reproduces the active
// generation, so the unchanged proof settles it and installs nothing; the
// forced project name fails the proof, so the run parses and installs a new
// generation. It returns the file count.
func managedRefreshStampMetadata(tb testing.TB, output string, stored int, projectName string) int {
	tb.Helper()
	files, err := managedRefreshMetadataFiles(output)
	if err != nil {
		tb.Fatal(err)
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			tb.Fatal(err)
		}
		var meta schema.UnifiedMetadata
		if err := json.Unmarshal(data, &meta); err != nil {
			tb.Fatalf("decode managed metadata: %v", err)
		}
		meta.AdapterVersion = &stored
		if projectName != "" {
			meta.Project.Name = projectName
		}
		meta.MetadataHash = schema.ComputeMetadataHash(&meta)
		encoded, err := json.Marshal(&meta)
		if err != nil {
			tb.Fatal(err)
		}
		temp, err := os.CreateTemp(filepath.Dir(path), ".managed-refresh-stamp-*")
		if err != nil {
			tb.Fatal(err)
		}
		tempName := temp.Name()
		if _, err := temp.Write(encoded); err != nil {
			_ = temp.Close()
			_ = os.Remove(tempName)
			tb.Fatal(err)
		}
		if err := temp.Close(); err != nil {
			_ = os.Remove(tempName)
			tb.Fatal(err)
		}
		if err := os.Chmod(tempName, 0o600); err != nil {
			_ = os.Remove(tempName)
			tb.Fatal(err)
		}
		if err := os.Rename(tempName, path); err != nil {
			_ = os.Remove(tempName)
			tb.Fatal(err)
		}
	}
	return len(files)
}

func managedRefreshMetadataFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), defaults.MetadataSuffix) {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func managedRefreshCountMetadata(tb testing.TB, output string) (int, int) {
	tb.Helper()
	target := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode].AdapterVersion
	files, err := managedRefreshMetadataFiles(output)
	if err != nil {
		tb.Fatal(err)
	}
	atTarget := 0
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			tb.Fatal(err)
		}
		var meta schema.UnifiedMetadata
		if err := json.Unmarshal(data, &meta); err != nil {
			tb.Fatalf("decode refreshed metadata: %v", err)
		}
		if meta.AdapterVersion != nil && *meta.AdapterVersion == target {
			atTarget++
		}
	}
	return len(files), atTarget
}

// managedRefreshCountMetadataRevision counts the managed metadata files whose
// adapter revision equals revision.
func managedRefreshCountMetadataRevision(tb testing.TB, output string, revision int) int {
	tb.Helper()
	files, err := managedRefreshMetadataFiles(output)
	if err != nil {
		tb.Fatal(err)
	}
	count := 0
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			tb.Fatal(err)
		}
		var meta schema.UnifiedMetadata
		if err := json.Unmarshal(data, &meta); err != nil {
			tb.Fatalf("decode stamped metadata: %v", err)
		}
		if meta.AdapterVersion != nil && *meta.AdapterVersion == revision {
			count++
		}
	}
	return count
}

// managedRefreshActiveProjectNames reads the project name of every active
// managed generation, so the sample proves the refreshed context reached the
// installed generation rather than only the stored pair.
func managedRefreshActiveProjectNames(tb testing.TB, db *store.Store) []string {
	tb.Helper()
	conn, err := db.Pool().Take(tb.Context())
	if err != nil {
		tb.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var names []string
	if err := sqlitex.ExecuteTransient(conn, `SELECT g.project_name
FROM sessions s
JOIN session_generations g ON g.session_id = s.session_id AND g.generation_id = s.active_generation_id
WHERE s.active_generation_id IS NOT NULL
ORDER BY s.session_id`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			names = append(names, stmt.ColumnText(0))
			return nil
		},
	}); err != nil {
		tb.Fatalf("read active generation project names: %v", err)
	}
	return names
}

func managedRefreshRowCounts(tb testing.TB, db *store.Store) map[string]int64 {
	tb.Helper()
	tables := []string{
		"sessions", "session_generations", "session_generation_entries",
		"session_generation_content", "session_generation_title_refs",
		"session_section_native_metadata", "session_entry_bodies",
		"session_projection_entries", "session_projection_content",
		"session_projection_aliases", "session_projection_sections",
		"session_context_segments", "session_metrics", "annotations",
	}
	rows := make(map[string]int64, len(tables))
	for _, table := range tables {
		rows[table] = managedRefreshQueryInt(tb, db, `SELECT count(*) FROM `+table)
	}
	return rows
}

func managedRefreshPageStats(tb testing.TB, db *store.Store) managedRefreshDBStats {
	tb.Helper()
	return managedRefreshDBStats{
		pageCount: managedRefreshQueryInt(tb, db, `PRAGMA page_count`),
		pageSize:  managedRefreshQueryInt(tb, db, `PRAGMA page_size`),
		freelist:  managedRefreshQueryInt(tb, db, `PRAGMA freelist_count`),
	}
}

func managedRefreshFileSizes(tb testing.TB, dbPath string) (int64, int64) {
	tb.Helper()
	size := func(path string) int64 {
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return 0
			}
			tb.Fatal(err)
		}
		return info.Size()
	}
	return size(dbPath), size(dbPath + "-wal")
}

func managedRefreshQueryInt(tb testing.TB, db *store.Store, query string, args ...any) int64 {
	tb.Helper()
	conn, err := db.Pool().Take(tb.Context())
	if err != nil {
		tb.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var value int64
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			value = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		tb.Fatalf("query %q: %v", query, err)
	}
	return value
}

func managedRefreshCopyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			_ = in.Close()
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			_ = in.Close()
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeInErr := in.Close()
		closeOutErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeInErr != nil {
			return closeInErr
		}
		return closeOutErr
	})
}

func managedRefreshTimingFrom(values []float64) managedRefreshTiming {
	if len(values) == 0 {
		return managedRefreshTiming{}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	sum := 0.0
	for _, value := range sorted {
		sum += value
	}
	return managedRefreshTiming{
		Count:    len(sorted),
		TotalMs:  sum,
		MedianMs: sorted[(len(sorted)-1)/2],
		P95Ms:    sorted[(len(sorted)*95)/100],
		MaxMs:    sorted[len(sorted)-1],
	}
}

func managedRefreshWriteJSON(tb testing.TB, out string, sample managedRefreshSample) {
	tb.Helper()
	data, err := json.MarshalIndent(sample, "", "  ")
	if err != nil {
		tb.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(out, append(data, '\n'), 0o600); err != nil {
		tb.Fatal(err)
	}
}

func managedRefreshLane(tb testing.TB) string {
	tb.Helper()
	lane := os.Getenv("PEASANT_MANAGED_REFRESH_LANE")
	if lane == "" {
		return managedRefreshSourceRetained
	}
	if lane != managedRefreshSourceRetained && lane != managedRefreshSourceNative {
		tb.Fatalf("PEASANT_MANAGED_REFRESH_LANE=%q is not retained or native", lane)
	}
	return lane
}

func managedRefreshEnvInt(tb testing.TB, name string, fallback int) int {
	tb.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		tb.Fatalf("%s=%q is not a positive integer", name, raw)
	}
	return value
}

func managedRefreshEnvBool(tb testing.TB, name string, fallback bool) bool {
	tb.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		tb.Fatalf("%s=%q is not a boolean", name, raw)
	}
	return value
}

// managedRefreshDispositionCount counts index-log reasons that name one
// unchanged-settle disposition. The pipeline appends the store's disposition
// to the reason, so a test can prove the settle was the branch actually
// exercised.
func managedRefreshDispositionCount(reasons map[string]int, disposition string) int {
	total := 0
	needle := "(" + disposition + ")"
	for reason, count := range reasons {
		if strings.Contains(reason, needle) {
			total += count
		}
	}
	return total
}

// managedRefreshCase selects the prepared workload. The default is the common
// adapter bump whose refreshed projection is identical.
func managedRefreshCase(tb testing.TB) string {
	tb.Helper()
	raw := os.Getenv("PEASANT_MANAGED_REFRESH_CASE")
	if raw == "" {
		return managedRefreshCaseStampOnly
	}
	if raw != managedRefreshCaseStampOnly && raw != managedRefreshCaseForced {
		tb.Fatalf("PEASANT_MANAGED_REFRESH_CASE=%q is not %s or %s", raw, managedRefreshCaseStampOnly, managedRefreshCaseForced)
	}
	return raw
}

// managedRefreshPath selects which lane the sample measures: the ordinary parse
// path or the unchanged settle path. It is required so a sample can never
// silently assert the wrong lane.
func managedRefreshPath(tb testing.TB) string {
	tb.Helper()
	raw := os.Getenv("PEASANT_MANAGED_REFRESH_PATH")
	if raw == "" {
		tb.Fatalf("set PEASANT_MANAGED_REFRESH_PATH=%s or %s", managedRefreshPathParse, managedRefreshPathRefresh)
	}
	if raw != managedRefreshPathParse && raw != managedRefreshPathRefresh {
		tb.Fatalf("PEASANT_MANAGED_REFRESH_PATH=%q is not %s or %s", raw, managedRefreshPathParse, managedRefreshPathRefresh)
	}
	return raw
}
