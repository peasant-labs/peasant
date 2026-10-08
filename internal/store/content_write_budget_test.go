package store_test

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
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
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
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
			case "staging-batch-commits-once":
				runWriteBudgetStagingBatchCommitsOnce(t, fixtures, c)
			case "activation-batch-commits-once":
				runWriteBudgetActivationBatchCommitsOnce(t, fixtures, c)
			case "oversized-session-stages-alone":
				runWriteBudgetOversizedStagesAlone(t, fixtures, c)
			case "skip-commits-nothing":
				runWriteBudgetSkipCommitsNothing(t, fixtures, c)
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
// terms: exactly one ExceedsWriteBudget definition, every grouping site asking
// it (at least the fixture's minSites call sites beyond the definition), and
// none of the retired budget literals left in the write path.
func runWriteBudgetSplitterSingleHome(t *testing.T, c contentWriteBudgetCase) {
	t.Helper()
	if c.MinSites == nil {
		t.Fatal("splitter-single-home carries no minSites; the guard asserts against nothing")
	}
	if got := countIngestOccurrences(t, "func ExceedsWriteBudget("); got != 1 {
		t.Fatalf("ExceedsWriteBudget definitions = %d, want exactly 1: the splitter is the single home of the count and byte terms", got)
	}
	calls := countIngestOccurrences(t, "ExceedsWriteBudget(") - 1
	if calls < *c.MinSites {
		t.Fatalf("ExceedsWriteBudget call sites = %d, want at least %d: every grouping site asks the one splitter", calls, *c.MinSites)
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

// writeBudgetV2 builds one minimal valid harmonized candidate: text entries
// with filled content records and their bytes, so the budget runners stage
// and commit through the production writer without a parser.
func writeBudgetV2(t *testing.T, sid schema.SessionID, genID string, texts []string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	entries := make([]schema.SessionEntry, 0, len(texts))
	blobs := make(map[schema.SourceEntryRef][]byte, len(texts))
	content := make([]indexformat.ContentRecord, 0, len(texts))
	for i, text := range texts {
		ref := schema.SourceEntryRef(fmt.Sprintf("e_b%d", i))
		preview := text
		entries = append(entries, schema.SessionEntry{
			SessionID: sid, EntryIndex: i, Harness: defaults.HarnessClaudeCode,
			EntryType: schema.EntryTypeText, Role: schema.RoleUser,
			ContentPreview: &preview, SourceEntryRef: ref,
		})
		payload := []byte(text)
		sum := sha256.Sum256(payload)
		content = append(content, indexformat.ContentRecord{
			Ref: ref, RelativeBlob: "c_" + hex.EncodeToString(sum[:]) + ".blob",
			ByteLength: int64(len(payload)), Digest: hex.EncodeToString(sum[:]),
		})
		blobs[ref] = payload
	}
	inputCount := int64(1)
	return indexformat.V2{Generation: indexformat.Generation{
		ID:           genID,
		Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  defaults.HarnessClaudeCode,
			Model:         schema.ModelID("budget-model"),
			Version:       "budget-version",
			Stats:         schema.SessionStats{TurnCount: len(entries), InputSubmissionCount: &inputCount},
		},
		Main:                 indexformat.Partition{Entries: entries},
		Content:              content,
		SourceEvidenceDigest: strings.Repeat("b", 64),
	}}, blobs
}

// openWriteBudgetHarmonized opens one commit-counted store with the
// harmonized writer registered, the owned-artifact file store, and the
// session lock. Extra options (such as a tiny write budget) append after
// the autorecovery-disabled log configuration the frame counter needs.
func openWriteBudgetHarmonized(t *testing.T, dbPath string, options ...store.OpenOption) *store.Store {
	t.Helper()
	root := t.TempDir()
	artifacts, err := store.NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	base := []store.OpenOption{
		store.WithWALAutocheckpointDisabled(),
		store.WithPoolSize(1),
		store.WithIndexFormats(store.V2IndexFormat()),
		store.WithGenerationArtifacts(artifacts, locker),
	}
	// WithSkipMigrations is inline (not only in base) so the open-time
	// rule can see it: the file opens an already-migrated golden copy, so
	// skipping the migration replay changes nothing. Go spreads a single
	// slice only, hence the nested append.
	db, err := store.Open(dbPath, append([]store.OpenOption{store.WithSkipMigrations()}, append(base, options...)...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedWriteBudgetSession inserts one bare session row for a harmonized
// budget candidate: no artifacts, no index state, just the identity the
// staging flag and the generation rows hang from.
func seedWriteBudgetSession(t *testing.T, ctx context.Context, db *store.Store, id string, fixtures contentWriteBudgetFixtures) schema.SessionID {
	t.Helper()
	sid := schema.SessionID(id)
	if err := db.InsertSessions(ctx, []ingest.StoreEntry{
		makeStoreEntry(t, id, fixtures.ProjectHash, fixtures.HostSlug, defaults.HarnessClaudeCode, 1700000000000, 100, 50),
	}); err != nil {
		t.Fatal(err)
	}
	return sid
}

// writeBudgetActivation builds the production activation for one budget
// candidate: a complete generation with a full capture.
func writeBudgetActivation(v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte) store.GenerationActivation {
	return store.GenerationActivation{
		Generation:     v2,
		Blobs:          blobs,
		IndexerVersion: 1,
		IndexedAtMs:    1,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status:           ingest.ContentCaptureComplete,
			SourceAuthority:  ingest.ContentSourceNewIngest,
			TranscriptOrigin: ingest.TranscriptOriginFile,
			CaptureFormat:    ingest.ContentCaptureFormatFull,
			CapturedAtMs:     1,
		},
	}
}

// runWriteBudgetStagingBatchCommitsOnce proves one ordinary session's
// object batch stages in exactly one commit: the flag, every body, and
// every blob share the batch's single transaction.
func runWriteBudgetStagingBatchCommitsOnce(t *testing.T, fixtures contentWriteBudgetFixtures, c contentWriteBudgetCase) {
	t.Helper()
	if c.Action != "stage-batch" || c.Sessions != 1 || c.WantCommits == nil {
		t.Fatal("staging-batch-commits-once needs action stage-batch, sessions 1, and wantCommits")
	}
	dbPath := storetest.CopyGoldenDB(t)
	db := openWriteBudgetHarmonized(t, dbPath)
	ctx := t.Context()
	sid := seedWriteBudgetSession(t, ctx, db, "11111111-1111-4111-8111-111111111111", fixtures)
	v2, blobs := writeBudgetV2(t, sid, "gen_stage_1", []string{"staging batch one", "staging batch two"})
	before, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.StageGeneration(ctx, writeBudgetActivation(v2, blobs)); err != nil {
		t.Fatal(err)
	}
	after, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := after - before; got != *c.WantCommits {
		t.Fatalf("a staging batch gained %d commit frames, want %d", got, *c.WantCommits)
	}
}

// runWriteBudgetActivationBatchCommitsOnce proves a two-session activation
// batch commits once: one outer transaction with a per-session savepoint
// serves the whole batch.
func runWriteBudgetActivationBatchCommitsOnce(t *testing.T, fixtures contentWriteBudgetFixtures, c contentWriteBudgetCase) {
	t.Helper()
	if c.Action != "activate-batch" || c.Sessions != 2 || c.WantCommits == nil {
		t.Fatal("activation-batch-commits-once needs action activate-batch, sessions 2, and wantCommits")
	}
	dbPath := storetest.CopyGoldenDB(t)
	db := openWriteBudgetHarmonized(t, dbPath)
	ctx := t.Context()
	writes := make([]ingest.SessionEntryWrite, 0, c.Sessions)
	for i := 0; i < c.Sessions; i++ {
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", i+1, i+1)
		sid := seedWriteBudgetSession(t, ctx, db, id, fixtures)
		v2, blobs := writeBudgetV2(t, sid, fmt.Sprintf("gen_active_%d", i), []string{fmt.Sprintf("activation batch %d", i)})
		if _, err := db.StageGeneration(ctx, writeBudgetActivation(v2, blobs)); err != nil {
			t.Fatal(err)
		}
		writes = append(writes, ingest.SessionEntryWrite{
			SessionID: sid, Result: v2, IndexVersion: 2,
			IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, IndexedAtMs: 1700000001000,
			RequireFullContent: true,
			ContentCapture: ingest.SessionContentCaptureWrite{
				Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceNewIngest,
				TranscriptOrigin: ingest.TranscriptOriginFile, CaptureFormat: ingest.ContentCaptureFormatFull, CapturedAtMs: 1700000001000,
			},
		})
	}
	before, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	results := db.IndexSessionEntryBatch(ctx, writes)
	for _, result := range results {
		if result.Err != nil || !result.Written {
			t.Fatalf("activation batch write: %+v", results)
		}
	}
	after, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := after - before; got != *c.WantCommits {
		t.Fatalf("an activation batch gained %d commit frames, want %d", got, *c.WantCommits)
	}
}

// runWriteBudgetOversizedStagesAlone proves a session whose objects exceed
// the byte budget stages alone in budget-sized transactions: with a tiny
// budget every body exceeds it, so each commits in its own transaction and
// the ordinary single-commit fast path does not merge them.
func runWriteBudgetOversizedStagesAlone(t *testing.T, fixtures contentWriteBudgetFixtures, c contentWriteBudgetCase) {
	t.Helper()
	if c.Action != "stage-oversized" || c.Sessions != 1 || c.WantCommits == nil {
		t.Fatal("oversized-session-stages-alone needs action stage-oversized, sessions 1, and wantCommits")
	}
	dbPath := storetest.CopyGoldenDB(t)
	tiny := ingest.WriteConfig{BatchBytes: 64, BatchSessions: 64}.WithDefaults(1)
	db := openWriteBudgetHarmonized(t, dbPath, store.WithWriteConfig(tiny))
	ctx := t.Context()
	sid := seedWriteBudgetSession(t, ctx, db, "22222222-2222-4222-8222-222222222222", fixtures)
	v2, blobs := writeBudgetV2(t, sid, "gen_oversized", []string{"oversized body one....................", "oversized body two...................."})
	before, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.StageGeneration(ctx, writeBudgetActivation(v2, blobs)); err != nil {
		t.Fatal(err)
	}
	after, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := after - before; got != *c.WantCommits {
		t.Fatalf("an oversized session gained %d commit frames, want %d alone-sized transactions", got, *c.WantCommits)
	}
}

// runWriteBudgetSkipCommitsNothing proves an identical refresh commits no
// objects and no generation: the second activation reports skipped, the
// generation and object tables hold exactly the first activation's rows,
// and the log gains at most the one bookkeeping commit.
func runWriteBudgetSkipCommitsNothing(t *testing.T, fixtures contentWriteBudgetFixtures, c contentWriteBudgetCase) {
	t.Helper()
	if c.Action != "skip-refresh" || c.Sessions != 1 || c.WantMaxCommits == nil {
		t.Fatal("skip-commits-nothing needs action skip-refresh, sessions 1, and wantMaxCommits")
	}
	dbPath := storetest.CopyGoldenDB(t)
	db := openWriteBudgetHarmonized(t, dbPath)
	ctx := t.Context()
	sid := seedWriteBudgetSession(t, ctx, db, "33333333-3333-4333-8333-333333333333", fixtures)
	v2, blobs := writeBudgetV2(t, sid, "gen_skip_1", []string{"skip-unchanged body"})
	first, err := db.ActivateGeneration(ctx, writeBudgetActivation(v2, blobs))
	if err != nil || first.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("first activation: %+v %v", first, err)
	}
	generationsBefore, bodiesBefore := countSkipTables(t, dbPath, sid)
	before, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// A refresh carries a fresh generation identifier over identical
	// content: the same identifier would report AlreadyCommitted through
	// the immutable identity instead of comparing.
	refreshed, refreshedBlobs := writeBudgetV2(t, sid, "gen_skip_2", []string{"skip-unchanged body"})
	second, err := db.ActivateGeneration(ctx, writeBudgetActivation(refreshed, refreshedBlobs))
	if err != nil {
		t.Fatal(err)
	}
	if second.Disposition != ingest.ActivationSkipped {
		t.Fatalf("identical refresh disposition = %v, want skipped", second.Disposition)
	}
	after, err := storetest.CountWALCommitFrames(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := after - before; got > *c.WantMaxCommits {
		t.Fatalf("an identical refresh committed %d frames, want at most %d (the bookkeeping alone)", got, *c.WantMaxCommits)
	}
	generationsAfter, bodiesAfter := countSkipTables(t, dbPath, sid)
	if generationsAfter != generationsBefore || bodiesAfter != bodiesBefore {
		t.Fatalf("a skip wrote rows: generations %d->%d bodies %d->%d; a skip writes nothing", generationsBefore, generationsAfter, bodiesBefore, bodiesAfter)
	}
}

// countSkipTables counts one session's generation rows and body rows
// through a direct connection: the skip writes neither.
func countSkipTables(t *testing.T, dbPath string, sid schema.SessionID) (int, int) {
	t.Helper()
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var generations, bodies int
	if err := sqlitex.ExecuteTransient(conn, `SELECT (SELECT COUNT(*) FROM session_generations WHERE session_id = ?), (SELECT COUNT(*) FROM session_entry_bodies WHERE session_id = ?)`, &sqlitex.ExecOptions{
		Args: []any{string(sid), string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			generations, bodies = stmt.ColumnInt(0), stmt.ColumnInt(1)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return generations, bodies
}
