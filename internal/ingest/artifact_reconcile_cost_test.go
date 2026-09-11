package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/artifact_reconcile_cost.yaml
var artifactReconcileCostYAML []byte

// reconcileCostChange names what a case does to a settled session between the
// measured harvests. It is a closed set: an unknown value is a fixture error,
// never a silently skipped case.
type reconcileCostChange string

const (
	reconcileCostChangeNone       reconcileCostChange = "none"
	reconcileCostChangeMetadata   reconcileCostChange = "metadata"
	reconcileCostChangeUnmirrored reconcileCostChange = "unmirrored"
)

func newReconcileCostChange(value string) (reconcileCostChange, error) {
	switch reconcileCostChange(value) {
	case reconcileCostChangeNone, reconcileCostChangeMetadata, reconcileCostChangeUnmirrored:
		return reconcileCostChange(value), nil
	}
	return "", fmt.Errorf("load retained reconciliation cost fixture testdata/artifact_reconcile_cost.yaml: change %q is not a supported case change; no harvest was measured; use one of %q, %q or %q",
		value, reconcileCostChangeNone, reconcileCostChangeMetadata, reconcileCostChangeUnmirrored)
}

type artifactReconcileCostFixtures struct {
	RequiredNames []string `yaml:"requiredNames"`
	Transcript    string   `yaml:"transcript"`
	Cases         []struct {
		Name      string `yaml:"name"`
		SessionID string `yaml:"sessionID"`
		Change    string `yaml:"change"`
	} `yaml:"cases"`
}

func loadArtifactReconcileCostFixtures(t *testing.T) artifactReconcileCostFixtures {
	t.Helper()
	var fixture artifactReconcileCostFixtures
	decoder := yaml.NewDecoder(bytes.NewReader(artifactReconcileCostYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("retained reconciliation cost fixture requires one YAML document")
	}
	required := []string{"settled-session-costs-no-transcript-read", "changed-metadata-session-is-repaired", "logs-only-session-is-bootstrapped"}
	if !reflect.DeepEqual(required, fixture.RequiredNames) {
		t.Fatal("retained reconciliation cost required-name manifest changed")
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid retained reconciliation cost case %q", row.Name)
		}
		if _, err := newReconcileCostChange(row.Change); err != nil {
			t.Fatal(err)
		}
		seen[row.Name] = true
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing retained reconciliation cost case %q", name)
		}
	}
	if fixture.Transcript == "" {
		t.Fatal("retained reconciliation cost fixture has no transcript body")
	}
	return fixture
}

// reconcileCostCounts is the measured I/O of one harvest over the retained
// tree: transcript bytes actually read, and lock files actually created.
//
// It separates the reconciliation walk, which runs before discovery, from
// everything the harvest does afterwards. Only the walk is measured here; the
// later index-selection read of the same retained tree is its own code path
// with its own cost.
type reconcileCostCounts struct {
	mu          sync.Mutex
	afterWalk   bool
	bytes       map[bool]int
	reads       map[bool]map[string]int
	lockCreates map[bool][]string
}

func newReconcileCostCounts() *reconcileCostCounts {
	counts := &reconcileCostCounts{}
	counts.reset()
	return counts
}

func (c *reconcileCostCounts) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.afterWalk = false
	c.bytes = map[bool]int{}
	c.reads = map[bool]map[string]int{false: {}, true: {}}
	c.lockCreates = map[bool][]string{}
}

// beginAfterWalk marks the point the reconciliation walk has finished and the
// rest of the harvest has started.
func (c *reconcileCostCounts) beginAfterWalk() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.afterWalk = true
}

func (c *reconcileCostCounts) addTranscript(name string, size int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bytes[c.afterWalk] += size
	c.reads[c.afterWalk][name] += size
}

func (c *reconcileCostCounts) addLockCreate(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lockCreates[c.afterWalk] = append(c.lockCreates[c.afterWalk], path)
}

// walkCost reports the transcript bytes, the per-session transcript reads and
// the lock files the reconciliation walk itself paid for.
func (c *reconcileCostCounts) walkCost() (int, map[string]int, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	reads := make(map[string]int, len(c.reads[false]))
	for name, size := range c.reads[false] {
		reads[name] = size
	}
	creates := append([]string(nil), c.lockCreates[false]...)
	sort.Strings(creates)
	return c.bytes[false], reads, creates
}

// reconcileCostFS counts what the production reconciliation actually reads and
// creates. It delegates every operation to the real durable filesystem, so the
// measured path is the one a harvest runs.
type reconcileCostFS struct {
	*ingest.OSFileSystem
	counts *reconcileCostCounts
}

var _ ingest.DurableFileSystem = (*reconcileCostFS)(nil)

func (f *reconcileCostFS) OpenArtifactRoot(path string) (ingest.ArtifactRoot, error) {
	root, err := f.OSFileSystem.OpenArtifactRoot(path)
	if err != nil {
		return nil, err
	}
	return &reconcileCostRoot{ArtifactRoot: root, counts: f.counts}, nil
}

func (f *reconcileCostFS) CreateArtifactRoot(path string) (ingest.ArtifactRoot, error) {
	root, err := f.OSFileSystem.CreateArtifactRoot(path)
	if err != nil {
		return nil, err
	}
	return &reconcileCostRoot{ArtifactRoot: root, counts: f.counts}, nil
}

type reconcileCostRoot struct {
	ingest.ArtifactRoot
	counts *reconcileCostCounts
}

func (r *reconcileCostRoot) ReadFile(path string) ([]byte, error) {
	data, err := r.ArtifactRoot.ReadFile(path)
	if name := filepath.Base(path); strings.Contains(name, "--transcript.") {
		r.counts.addTranscript(name, len(data))
	}
	return data, err
}

func (r *reconcileCostRoot) Lock(ctx context.Context, path string, mode ingest.ArtifactLockMode, create bool) (io.Closer, error) {
	if create {
		r.counts.addLockCreate(path)
	}
	return r.ArtifactRoot.Lock(ctx, path, mode, create)
}

func reconcileCostLockFiles(t *testing.T, output string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(output, ".peasant-state", "locks"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// TestReconcileStoredSettledSessionCostsNoTranscriptRead measures the harvest
// cost of a retained tree that has not changed. A settled session is already
// mirrored and indexed, so reconciliation has nothing to do for it and may not
// read its transcript or create a lock file to find that out. Only a session
// whose retained metadata moved, or one the database no longer mirrors, pays
// the full read.
func TestReconcileStoredSettledSessionCostsNoTranscriptRead(t *testing.T) {
	fixture := loadArtifactReconcileCostFixtures(t)
	root := t.TempDir()
	output := filepath.Join(root, "managed")
	database, err := store.Open(filepath.Join(root, "peasant.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	changes := make(map[ingest.SessionID]reconcileCostChange, len(fixture.Cases))
	names := make(map[ingest.SessionID]string, len(fixture.Cases))
	sessions := make([]ingest.DiscoveredSession, 0, len(fixture.Cases))
	metadata := make(map[ingest.SessionID]*ingest.UnifiedMetadata, len(fixture.Cases))
	settled := make([]ingest.DiscoveredSession, 0, len(fixture.Cases))
	settledMeta := make(map[ingest.SessionID]*ingest.UnifiedMetadata, len(fixture.Cases))
	for index, row := range fixture.Cases {
		change, err := newReconcileCostChange(row.Change)
		if err != nil {
			t.Fatal(err)
		}
		source := filepath.Join(root, row.SessionID+".jsonl")
		if err := os.WriteFile(source, []byte(fixture.Transcript), 0600); err != nil {
			t.Fatal(err)
		}
		session := makeDiscoveredSession(t, row.SessionID, source, time.Now().Add(-time.Duration(index+1)*time.Hour))
		sessions = append(sessions, session)
		metadata[session.SessionID] = makeReindexMeta(t, row.SessionID, source)
		changes[session.SessionID] = change
		names[session.SessionID] = row.Name
		if change != reconcileCostChangeUnmirrored {
			settled = append(settled, session)
			settledMeta[session.SessionID] = metadata[session.SessionID]
		}
	}

	config := makePipelineConfig(output)
	seed, err := ingest.NewPipeline(&ingest.OSFileSystem{}, testutil.DefaultGitResolver(), map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(settled, settledMeta),
	}, config, ingest.WithStore(database), ingest.WithMetricsStore(database),
		ingest.WithIndexers(ingest.NewIndexerRegistry(&ingest.OSFileSystem{}, ingest.IndexerRegistryOptions{})))
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := seed.Run(t.Context())
	if err != nil || seeded.Summary.New != len(settled) || seeded.Summary.Errors != 0 {
		t.Fatalf("seed the settled corpus: %+v %v", seeded, err)
	}

	counts := newReconcileCostCounts()
	filesystem := &reconcileCostFS{OSFileSystem: &ingest.OSFileSystem{}, counts: counts}
	config.Sources = nil
	config.SessionFilter = func(ingest.DiscoveredSession) bool { return false }
	// Discovery ends the reconciliation walk, so everything counted before this
	// hook is what the walk itself cost.
	config.PrepareSessionFilter = func(context.Context, []ingest.DiscoveredSession) error {
		counts.beginAfterWalk()
		return nil
	}
	adapter := &metadataPolicyAdapter{StubAdapter: &testutil.StubAdapter{ProviderValue: ingest.HarnessClaudeCode}}
	harvest, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(), map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter { return adapter },
	}, config, ingest.WithStore(database), ingest.WithMetricsStore(database),
		ingest.WithIndexers(ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})))
	if err != nil {
		t.Fatal(err)
	}

	locksBefore := reconcileCostLockFiles(t, output)
	counts.reset()
	if _, err := harvest.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	bytesRead, reads, lockCreates := counts.walkCost()
	if bytesRead != 0 {
		t.Errorf("reconciling a settled corpus read %d transcript bytes on a second harvest, want 0: %v", bytesRead, reads)
	}
	if len(lockCreates) != 0 {
		t.Errorf("reconciling a settled corpus created %d lock files on a second harvest, want 0: %v", len(lockCreates), lockCreates)
	}
	if locksAfter := reconcileCostLockFiles(t, output); !reflect.DeepEqual(locksBefore, locksAfter) {
		t.Errorf("settled corpus changed the retained lock directory: before=%v after=%v", locksBefore, locksAfter)
	}
	if adapter.extracts.Load() != 0 {
		t.Errorf("settled corpus invoked native extraction %d times, want 0", adapter.extracts.Load())
	}

	wantReads := map[string]bool{}
	for _, session := range sessions {
		meta := metadata[session.SessionID]
		path := ingest.SessionMetadataPath(output, string(meta.HostSlug), string(session.SessionID), "")
		switch changes[session.SessionID] {
		case reconcileCostChangeNone:
		case reconcileCostChangeMetadata:
			reconcileCostRewriteMetadata(t, path)
			wantReads[string(session.SessionID)+"--transcript.jsonl"] = true
		case reconcileCostChangeUnmirrored:
			reconcileCostRetainUnmirrored(t, root, output, session, meta)
			wantReads[string(session.SessionID)+"--transcript.jsonl"] = true
		}
	}

	counts.reset()
	locksBefore = reconcileCostLockFiles(t, output)
	if _, err := harvest.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	bytesRead, reads, lockCreates = counts.walkCost()
	readNames := map[string]bool{}
	for name, size := range reads {
		if size <= 0 {
			t.Errorf("reconciling a changed corpus recorded a %d-byte read of %s", size, name)
		}
		readNames[name] = true
	}
	if !reflect.DeepEqual(readNames, wantReads) {
		t.Errorf("reconciling a changed corpus read transcripts %v, want exactly %v (a settled session pays nothing)", readNames, wantReads)
	}
	if bytesRead <= 0 {
		t.Errorf("reconciling a changed corpus read %d transcript bytes, want the changed pairs to be read", bytesRead)
	}
	if len(lockCreates) != len(wantReads) {
		t.Errorf("reconciling a changed corpus created %d lock files, want %d (one per session with work): %v", len(lockCreates), len(wantReads), lockCreates)
	}
	if locksAfter := reconcileCostLockFiles(t, output); len(locksAfter) != len(locksBefore) {
		t.Errorf("changed corpus lock directory = %v, want the settled set %v", locksAfter, locksBefore)
	}

	publisher, err := ingest.NewArtifactPublisher(&ingest.OSFileSystem{}, output, ingest.ArtifactPublisherOptions{Mirror: database})
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		meta := metadata[session.SessionID]
		path := ingest.SessionMetadataPath(output, string(meta.HostSlug), string(session.SessionID), "")
		captured, err := publisher.Capture(t.Context(), session.SessionID, path)
		if err != nil {
			t.Fatal(err)
		}
		state, err := database.ReadIndexState(t.Context(), session.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if state == nil || state.ArtifactHash == nil || *state.ArtifactHash != captured.ArtifactHash {
			t.Errorf("case %q: retained pair is not mirrored after reconciliation: %+v", names[session.SessionID], state)
		}
	}
}

// reconcileCostRewriteMetadata changes a committed metadata file the way an
// out-of-band edit does: a new semantic value with its own consistent digest.
func reconcileCostRewriteMetadata(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var meta ingest.UnifiedMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	meta.Timestamp.End += 1000
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	encoded, err := json.Marshal(&meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
}

// reconcileCostRetainUnmirrored publishes a retained pair through a real
// harvest against a throwaway database, so the pair exists in the retained tree
// while the database under test has never mirrored it. That is the
// retained-logs-only session a harvest has to bootstrap from its own files.
func reconcileCostRetainUnmirrored(t *testing.T, root, output string, session ingest.DiscoveredSession, meta *ingest.UnifiedMetadata) {
	t.Helper()
	scratch, err := store.Open(filepath.Join(root, "scratch-"+string(session.SessionID)+".db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = scratch.Close() }()
	filesystem := &ingest.OSFileSystem{}
	pipeline, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(), map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta}),
	}, makePipelineConfig(output), ingest.WithStore(scratch), ingest.WithMetricsStore(scratch),
		ingest.WithIndexers(ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})))
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(t.Context())
	if err != nil || result.Summary.New != 1 || result.Summary.Errors != 0 {
		t.Fatalf("retain an unmirrored pair: %+v %v", result, err)
	}
}
