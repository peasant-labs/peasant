package store_test

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/content_write_budget.yaml
var contentWriteBudgetYAML []byte

//go:embed testdata/content_write_budget.manifest.yaml
var contentWriteBudgetManifestYAML []byte

// contentWriteBudgetCase is one content_write_budget case: the section-10 name
// plus, for the cases this change owns, the typed expectations its runner
// asserts. Cases owned by the writer change carry ownedBy-free placeholders:
// a case with no typed fields is a placeholder the writer change fills later,
// following the content_migration convention; the loader's manifest check
// still protects its name.
type contentWriteBudgetCase struct {
	Name             string `yaml:"name"`
	Action           string `yaml:"action,omitempty"`
	Sessions         int    `yaml:"sessions,omitempty"`
	WantCommits      *int   `yaml:"wantCommits,omitempty"`
	WantMaxCommits   *int   `yaml:"wantMaxCommits,omitempty"`
	Workers          []int  `yaml:"workers,omitempty"`
	BatchBytes       *int64 `yaml:"batchBytes,omitempty"`
	BatchSessions    *int   `yaml:"batchSessions,omitempty"`
	StagedMemory     *int64 `yaml:"stagedMemoryBytes,omitempty"`
	FlushIntervalsMs []int  `yaml:"flushIntervalsMs,omitempty"`
	MinSites         *int   `yaml:"minSites,omitempty"`
}

// isPlaceholder reports whether the case carries no typed expectations, in
// which case the runner logs it and asserts nothing.
func (c contentWriteBudgetCase) isPlaceholder() bool {
	return c.Action == "" && c.Sessions == 0 && c.WantCommits == nil && c.WantMaxCommits == nil &&
		len(c.Workers) == 0 && c.BatchBytes == nil && c.BatchSessions == nil && c.StagedMemory == nil &&
		len(c.FlushIntervalsMs) == 0 && c.MinSites == nil
}

type contentWriteBudgetFixtures struct {
	HostSlug    string                   `yaml:"hostSlug"`
	ProjectHash string                   `yaml:"projectHash"`
	Transcript  string                   `yaml:"transcript"`
	Cases       []contentWriteBudgetCase `yaml:"cases"`
}

func loadContentWriteBudgetFixtures(t *testing.T) contentWriteBudgetFixtures {
	t.Helper()
	var fixtures contentWriteBudgetFixtures
	if err := testutil.DecodeFixtureYAML(contentWriteBudgetYAML, &fixtures); err != nil {
		t.Fatalf("decode content_write_budget.yaml: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(contentWriteBudgetManifestYAML, "content write budget")
	if err != nil {
		t.Fatal(err)
	}
	actual := make([]string, 0, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		actual = append(actual, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, actual, "content write budget"); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// TestContentWriteBudgetFixtureManifest pins the content write budget case
// inventory: the loader compiles, the manifest loads, and every required name
// is present with no undeclared extra.
func TestContentWriteBudgetFixtureManifest(t *testing.T) {
	t.Parallel()
	loadContentWriteBudgetFixtures(t)
}

// TestContentWriteBudgetFamily runs the content_write_budget cases this change
// owns under their section-10 names. The ingest gates
// (write_gates_test.go, worker_buffers_test.go, content_batch_test.go) own the
// same invariants under their own names; these cases alias them here through
// the exported knobs so the family asserts the configured budgets rather than
// duplicating the tables. The writer-owned cases stay placeholders until the
// writer change fills them.
func TestContentWriteBudgetFamily(t *testing.T) {
	t.Parallel()
	fixtures := loadContentWriteBudgetFixtures(t)
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			switch c.Name {
			case "splitter-single-home":
				runWriteBudgetSplitterSingleHome(t, c)
			case "batch-under-configured-caps":
				runWriteBudgetBatchUnderConfiguredCaps(t, c)
			case "buffers-preallocated-partitioned":
				runWriteBudgetBuffersPreallocatedPartitioned(t, c)
			case "staged-memory-under-cap":
				runWriteBudgetStagedMemoryUnderCap(t, c)
			case "flush-on-interval":
				runWriteBudgetFlushOnInterval(t, c)
			case "hold-under-busy-timeout":
				runWriteBudgetHoldUnderBusyTimeout(t, c)
			case "state-read-commits-nothing":
				runWriteBudgetStateReadCommitsNothing(t, fixtures, c)
			case "commit-count-budget":
				runWriteBudgetCommitCountBudget(t, fixtures, c)
			default:
				if !c.isPlaceholder() {
					t.Fatalf("%s: not one of the cases this change owns, yet it carries typed expectations; route it to its owner", c.Name)
				}
				t.Logf("%s: placeholder; the writer change fills this case", c.Name)
			}
		})
	}
}

// ingestNonTestSources returns the contents of every non-test Go source file
// in internal/ingest, anchored at this test file so the guard holds wherever
// the test runs from.
func ingestNonTestSources(t *testing.T) []string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the ingest sources: runtime.Caller failed")
	}
	dir := filepath.Join(filepath.Dir(file), "..", "ingest")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the ingest sources: %v", err)
	}
	var sources []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read ingest source %s: %v", name, err)
		}
		sources = append(sources, string(data))
	}
	if len(sources) == 0 {
		t.Fatal("no non-test ingest sources found; the single-home guard asserts against nothing")
	}
	return sources
}

// countIngestOccurrences counts a substring across every non-test ingest
// source file.
func countIngestOccurrences(t *testing.T, substr string) int {
	t.Helper()
	total := 0
	for _, source := range ingestNonTestSources(t) {
		total += strings.Count(source, substr)
	}
	return total
}

// runWriteBudgetSplitterSingleHome pins the single home of the count and byte
// terms: exactly one exceedsWriteBudget definition, every grouping site asking
// it (at least the fixture's minSites call sites beyond the definition), and
// none of the retired budget literals left in the write path.
func runWriteBudgetSplitterSingleHome(t *testing.T, c contentWriteBudgetCase) {
	t.Helper()
	if c.MinSites == nil {
		t.Fatal("splitter-single-home carries no minSites; the guard asserts against nothing")
	}
	if got := countIngestOccurrences(t, "func exceedsWriteBudget("); got != 1 {
		t.Fatalf("exceedsWriteBudget definitions = %d, want exactly 1: the splitter is the single home of the count and byte terms", got)
	}
	calls := countIngestOccurrences(t, "exceedsWriteBudget(") - 1
	if calls < *c.MinSites {
		t.Fatalf("exceedsWriteBudget call sites = %d, want at least %d: every grouping site asks the one splitter", calls, *c.MinSites)
	}
	for _, retired := range []string{"exceedsIndexWriteBudget", "indexWriteBatchLimit", "annotationFlushInterval"} {
		if got := countIngestOccurrences(t, retired); got != 0 {
			t.Fatalf("retired budget literal %q still occurs %d time(s) in the write path; every bound is read from config", retired, got)
		}
	}
}

// runWriteBudgetBatchUnderConfiguredCaps asserts the grouping caps against the
// configured knobs: a custom byte cap survives WithDefaults, differs from the
// shipped cap (the knob is read, never compiled in), validates, and derives
// its staged-memory total from the same knob.
func runWriteBudgetBatchUnderConfiguredCaps(t *testing.T, c contentWriteBudgetCase) {
	t.Helper()
	if c.BatchBytes == nil || c.BatchSessions == nil || c.StagedMemory == nil || len(c.Workers) == 0 {
		t.Fatal("batch-under-configured-caps needs batchBytes, batchSessions, stagedMemoryBytes, and workers")
	}
	for _, workers := range c.Workers {
		shipped := ingest.DefaultWriteConfig(workers)
		custom := ingest.WriteConfig{BatchBytes: *c.BatchBytes, BatchSessions: *c.BatchSessions, StagedMemoryBytes: *c.StagedMemory}.WithDefaults(workers)
		if custom.BatchBytes != *c.BatchBytes {
			t.Fatalf("workers=%d: BatchBytes=%d, want the configured %d", workers, custom.BatchBytes, *c.BatchBytes)
		}
		if custom.BatchSessions != *c.BatchSessions {
			t.Fatalf("workers=%d: BatchSessions=%d, want the configured %d", workers, custom.BatchSessions, *c.BatchSessions)
		}
		if custom.BatchBytes == shipped.BatchBytes {
			t.Fatalf("workers=%d: the custom %d-byte cap equals the shipped cap; the case must run off-default", workers, custom.BatchBytes)
		}
		if custom.StagedMemoryBytes != *c.StagedMemory {
			t.Fatalf("workers=%d: StagedMemoryBytes=%d, want the configured %d", workers, custom.StagedMemoryBytes, *c.StagedMemory)
		}
		if err := custom.Validate(); err != nil {
			t.Fatalf("workers=%d: the configured caps must validate: %v", workers, err)
		}
		if want := 2*custom.BatchBytes + int64(workers)*custom.BufferBytes; custom.StagedMemoryBytes != want {
			t.Fatalf("workers=%d: StagedMemoryBytes=%d, want 2*batchBytes + workers*bufferBytes = %d: the total is sized from the configured caps", workers, custom.StagedMemoryBytes, want)
		}
	}
}

// runWriteBudgetBuffersPreallocatedPartitioned holds the run-start allocation
// through the exported pool: one zero-length, full-capacity buffer per worker,
// pairwise disjoint, each capped at the configured per-worker cap.
func runWriteBudgetBuffersPreallocatedPartitioned(t *testing.T, c contentWriteBudgetCase) {
	t.Helper()
	if len(c.Workers) == 0 {
		t.Fatal("buffers-preallocated-partitioned needs workers")
	}
	for _, workers := range c.Workers {
		cfg := ingest.DefaultWriteConfig(workers)
		bufs, err := ingest.NewWorkerBuffers(cfg, workers)
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		if bufs.Workers() != workers {
			t.Fatalf("workers=%d: pool holds %d, want one buffer per worker", workers, bufs.Workers())
		}
		if bufs.PerBufferBytes() != cfg.BufferBytes {
			t.Fatalf("workers=%d: PerBufferBytes()=%d, want the configured cap %d", workers, bufs.PerBufferBytes(), cfg.BufferBytes)
		}
		for i := 0; i < workers; i++ {
			if got := len(bufs.Bytes(i)); got != 0 {
				t.Fatalf("workers=%d buffer %d starts with length %d, want zero: pre-allocated means capacity, not contents", workers, i, got)
			}
			if got := int64(cap(bufs.Bytes(i))); got != cfg.BufferBytes {
				t.Fatalf("workers=%d buffer %d has capacity %d, want the full per-worker cap %d", workers, i, got, cfg.BufferBytes)
			}
			if _, ok := bufs.Append(i, []byte{byte(i)}); !ok {
				t.Fatalf("workers=%d buffer %d refuses a one-byte stage", workers, i)
			}
		}
		for i := 0; i < workers; i++ {
			got := bufs.Bytes(i)
			if len(got) != 1 || got[0] != byte(i) {
				t.Fatalf("workers=%d buffer %d holds %v, want only its own marker: partitions are mutually exclusive", workers, i, got)
			}
		}
	}
}

// runWriteBudgetStagedMemoryUnderCap asserts the total gate across the worker
// counts a run can take: what the pool reserves never exceeds the configured
// total cap, and buffers that alone would breach it refuse the allocation.
func runWriteBudgetStagedMemoryUnderCap(t *testing.T, c contentWriteBudgetCase) {
	t.Helper()
	if len(c.Workers) == 0 {
		t.Fatal("staged-memory-under-cap needs workers")
	}
	for _, workers := range c.Workers {
		bufs, err := ingest.NewWorkerBuffers(ingest.DefaultWriteConfig(workers), workers)
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		if bufs.ReservedBytes() > bufs.StagedMemoryBytes() {
			t.Fatalf("workers=%d: reserved %d bytes past the staged cap %d", workers, bufs.ReservedBytes(), bufs.StagedMemoryBytes())
		}
	}
	cfg := ingest.DefaultWriteConfig(2)
	cfg.StagedMemoryBytes = cfg.BufferBytes
	if _, err := ingest.NewWorkerBuffers(cfg, 2); err == nil {
		t.Fatal("buffers reserving past the staged-memory cap must refuse the allocation, not run uncapped")
	}
}

// runWriteBudgetFlushOnInterval asserts the lane's idle wait against the
// configured knob: every listed interval resolves through WithDefaults to
// itself, the zero value resolves to the shipped default, and every flush
// site reads the knob (at least the fixture's minSites FlushInterval readers
// in the write path).
func runWriteBudgetFlushOnInterval(t *testing.T, c contentWriteBudgetCase) {
	t.Helper()
	if len(c.FlushIntervalsMs) == 0 || c.MinSites == nil {
		t.Fatal("flush-on-interval needs flushIntervalsMs and minSites")
	}
	for _, ms := range c.FlushIntervalsMs {
		cfg := ingest.WriteConfig{FlushIntervalMs: ms}.WithDefaults(1)
		if got := cfg.FlushInterval(); got != time.Duration(ms)*time.Millisecond {
			t.Fatalf("FlushInterval()=%s, want %dms: the lane must read the configured knob", got, ms)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("flushIntervalsMs=%d: the configured interval must validate: %v", ms, err)
		}
	}
	shipped := ingest.DefaultWriteConfig(1)
	if got := (ingest.WriteConfig{}).WithDefaults(1).FlushInterval(); got != shipped.FlushInterval() {
		t.Fatalf("zero-value FlushInterval()=%s, want the shipped default %s", got, shipped.FlushInterval())
	}
	if got := countIngestOccurrences(t, ".FlushInterval()"); got < *c.MinSites {
		t.Fatalf("FlushInterval() readers = %d, want at least %d: every flush site reads the configured interval", got, *c.MinSites)
	}
}

// runWriteBudgetHoldUnderBusyTimeout asserts the writer-lane invariant against
// the open path's busy_timeout through the exported knobs: no resolved
// configuration holds the single SQLite writer past the wait every other
// process honors.
func runWriteBudgetHoldUnderBusyTimeout(t *testing.T, c contentWriteBudgetCase) {
	t.Helper()
	if len(c.Workers) == 0 {
		t.Fatal("hold-under-busy-timeout needs workers")
	}
	for _, workers := range c.Workers {
		cfg := ingest.WriteConfig{}.WithDefaults(workers)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("workers=%d: the shipped budgets must validate: %v", workers, err)
		}
		if cfg.HoldTarget > defaults.SQLiteBusyTimeout {
			t.Fatalf("workers=%d: hold %s exceeds the open path's busy_timeout %s",
				workers, cfg.HoldTarget, defaults.SQLiteBusyTimeout)
		}
	}
	atLimit := ingest.WriteConfig{HoldTarget: defaults.SQLiteBusyTimeout}.WithDefaults(8)
	if err := atLimit.Validate(); err != nil {
		t.Fatalf("a hold at the busy_timeout must validate: %v", err)
	}
	if atLimit.HoldTarget != defaults.SQLiteBusyTimeout {
		t.Fatalf("HoldTarget=%s, want the configured %s", atLimit.HoldTarget, defaults.SQLiteBusyTimeout)
	}
	if err := (ingest.WriteConfig{HoldTarget: defaults.SQLiteBusyTimeout + time.Millisecond}).Validate(); err == nil {
		t.Fatal("a hold past the busy_timeout must refuse: the lane may equal the wait, never exceed it")
	}
}

// writeBudgetSeed mirrors sessions worth of artifacts into a fresh store whose
// log is never checkpointed, and returns the store, its path, and the seeded
// session IDs. Seeding commits before the measurement starts.
func writeBudgetSeed(t *testing.T, fixtures contentWriteBudgetFixtures, sessions int) (*store.Store, string, []ingest.ArtifactMirrorRequest) {
	t.Helper()
	dbPath := storetest.CopyGoldenDB(t)
	db, err := store.Open(dbPath, store.WithSkipMigrations(), store.WithWALAutocheckpointDisabled(), store.WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := t.Context()
	requests := make([]ingest.ArtifactMirrorRequest, 0, sessions)
	for i := range sessions {
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", i+1, i+1)
		entry := makeStoreEntry(t, id, fixtures.ProjectHash, fixtures.HostSlug, defaults.HarnessClaudeCode, 1700000000000+int64(i), 100, 50)
		requests = append(requests, ingest.ArtifactMirrorRequest{Artifact: mirrorTestArtifact(t, entry.Metadata, fixtures.Transcript)})
	}
	for _, result := range db.MirrorArtifacts(ctx, requests) {
		if result.Err != nil || !result.Mirrored {
			t.Fatalf("seed mirror %s: %v", result.SessionID, result.Err)
		}
	}
	return db, dbPath, requests
}

// runWriteBudgetStateReadCommitsNothing proves a state read commits nothing:
// the log gains exactly the fixture's wantCommits frames across a
// ReadIndexState on a seeded session.
func runWriteBudgetStateReadCommitsNothing(t *testing.T, fixtures contentWriteBudgetFixtures, c contentWriteBudgetCase) {
	t.Helper()
	if c.Action != "read-index-state" || c.Sessions < 1 || c.WantCommits == nil {
		t.Fatal("state-read-commits-nothing needs action read-index-state, sessions, and wantCommits")
	}
	db, dbPath, requests := writeBudgetSeed(t, fixtures, c.Sessions)
	before, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sid := requests[0].Artifact.Metadata.SessionID
	if _, err := db.ReadIndexState(t.Context(), sid); err != nil {
		t.Fatal(err)
	}
	after, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := after - before; got != *c.WantCommits {
		t.Fatalf("a state read gained %d commit frames, want %d", got, *c.WantCommits)
	}
}

// runWriteBudgetCommitCountBudget proves the ordinary-refresh contract: a
// harvest refresh of one seeded session — re-mirror, state read, one entry
// batch — commits at most the fixture's wantMaxCommits frames.
func runWriteBudgetCommitCountBudget(t *testing.T, fixtures contentWriteBudgetFixtures, c contentWriteBudgetCase) {
	t.Helper()
	if c.Action != "refresh" || c.Sessions < 1 || c.WantMaxCommits == nil {
		t.Fatal("commit-count-budget needs action refresh, sessions, and wantMaxCommits")
	}
	db, dbPath, requests := writeBudgetSeed(t, fixtures, c.Sessions)
	ctx := t.Context()
	before, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sid := requests[0].Artifact.Metadata.SessionID
	for _, result := range db.MirrorArtifacts(ctx, requests[:1]) {
		if result.Err != nil || !result.Mirrored {
			t.Fatalf("refresh mirror %s: %v", result.SessionID, result.Err)
		}
	}
	state, err := db.ReadIndexState(ctx, sid)
	if err != nil || state == nil {
		t.Fatalf("read refreshed index state: %v", err)
	}
	results := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: sid, Result: indexformat.V1{Entries: []schema.SessionEntry{}}, IndexVersion: 1,
		IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, IndexedAtMs: 1700000001000, ExpectedState: state,
	}})
	if len(results) != 1 || results[0].Err != nil || !results[0].Written {
		t.Fatalf("refresh entry batch: %+v", results)
	}
	after, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := after - before; got > *c.WantMaxCommits {
		t.Fatalf("an ordinary refresh committed %d frames, want at most %d", got, *c.WantMaxCommits)
	}
}
