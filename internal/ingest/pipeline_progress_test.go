package ingest

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/peasant-labs/peasant/internal/salt"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/pipeline_progress.yaml
var pipelineProgressYAML []byte

type pipelineProgressCase struct {
	Name        string `yaml:"name"`
	Boundary    string `yaml:"boundary"`
	ReturnError bool   `yaml:"return_error"`
}

func loadPipelineProgressFixtures(t *testing.T) []pipelineProgressCase {
	t.Helper()
	var fixture struct {
		RequiredCases []string               `yaml:"required_cases"`
		Cases         []pipelineProgressCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(pipelineProgressYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool)
	for _, tc := range fixture.Cases {
		present[tc.Name] = true
	}
	if len(fixture.RequiredCases) == 0 {
		t.Fatal("missing required cancellation cases")
	}
	for _, name := range fixture.RequiredCases {
		if !present[name] {
			t.Fatalf("missing required case %q", name)
		}
	}
	return fixture.Cases
}

func TestPipelineCancellationBeforeDiff(t *testing.T) {
	for _, tc := range loadPipelineProgressFixtures(t) {
		t.Run(tc.Name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			progress := NewProgressState()
			filesystem := &cancelNestedDiffFS{canceled: true}
			adapter := progressAdapter{sessions: []DiscoveredSession{{SessionID: "session-one", Harness: HarnessClaudeCode}}}
			pipeline := &Pipeline{
				fs: filesystem,
				adapters: map[Harness]AdapterFactory{
					HarnessClaudeCode: func(FileSystem, GitResolver, salt.Salt) SourceAdapter { return adapter },
				},
				config: PipelineConfig{Sources: map[Harness]SourceConfig{HarnessClaudeCode: {Enabled: true}}, Progress: progress},
			}
			switch tc.Boundary {
			case "entry":
				cancel()
			case "discovery":
				adapter.discover = cancel
			case "prepare":
				pipeline.config.PrepareSessionFilter = func(context.Context, []DiscoveredSession) error { cancel(); return nil }
			case "bulk":
				pipeline.store = &cancelProgressStore{cancel: cancel, returnError: tc.ReturnError}
			default:
				t.Fatalf("unknown cancellation boundary %q", tc.Boundary)
			}
			_, err := pipeline.Run(ctx)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Run error = %v, want context.Canceled", err)
			}
			if filesystem.afterCancel != 0 {
				t.Fatalf("unexpected filesystem calls = %d", filesystem.afterCancel)
			}
			for _, stage := range StageOrder[1:] {
				if progress.Snapshot()[stage].Started {
					t.Errorf("stage %s started after preparation cancellation", stage)
				}
			}
		})
	}
}

type cancelProgressStore struct {
	SessionStore
	cancel      context.CancelFunc
	returnError bool
}

var _ SessionStore = (*cancelProgressStore)(nil)

func (s *cancelProgressStore) BulkLookupSessionLocations(context.Context, []SessionID) (map[SessionID]SessionLocation, error) {
	s.cancel()
	if s.returnError {
		return nil, context.Canceled
	}
	ingested := int64(1)
	return map[SessionID]SessionLocation{"session-one": {IngestedMs: &ingested, SchemaVersion: CurrentSchemaVersion}}, nil
}

func TestPipelineReindexCancellationDuringDiffLookup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := NewProgressState()
	// A reindex classifies each scanned target against recorded evidence, so the
	// interruption is injected in the stored-compatibility lookup that DIFF
	// performs for the target it is classifying. That needs one managed session
	// on disk for DIFF to have a target at all.
	output := writeReindexProgressFixture(t)
	pipeline := &Pipeline{
		fs:           &OSFileSystem{},
		store:        &cancelProgressStore{cancel: cancel, returnError: true},
		metricsStore: &cancelReindexProgressStore{},
		config:       PipelineConfig{Reindex: true, Progress: progress, OutputDir: ResolvedPath(output)},
	}
	_, err := pipeline.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	snapshot := progress.Snapshot()
	if got := snapshot[StageDiff]; !got.Ended || !got.HasErr || got.Done != 0 {
		t.Fatalf("interrupted reindex DIFF = %+v, want error with no classified sessions", got)
	}
	for _, stage := range StageOrder[2:] {
		if snapshot[stage].Started {
			t.Errorf("stage %s started after cancellation", stage)
		}
	}
}

type cancelReindexProgressStore struct {
	MetricsStore
}

var _ MetricsStore = (*cancelReindexProgressStore)(nil)

// writeReindexProgressFixture writes one managed session under a fresh output
// directory and returns that directory. A reindex DIFF classifies the sessions
// it scans, so it needs at least one to classify.
func writeReindexProgressFixture(t *testing.T) string {
	t.Helper()
	output := t.TempDir()
	sid := SessionID("11111111-2222-3333-4444-555555555555")
	sessionDir := filepath.Join(output, "test-host", string(sid))
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := []byte(`{"type":"user","message":{"role":"user","content":"hello"}}` + "\n")
	if err := os.WriteFile(filepath.Join(sessionDir, string(sid)+"--transcript.jsonl"), transcript, 0o644); err != nil {
		t.Fatal(err)
	}
	meta := NewUnifiedMetadata()
	meta.SessionID = sid
	meta.ModelHarness = HarnessClaudeCode
	meta.HostSlug = HostSlug("test-host")
	meta.Source = SourceInfo{FilePath: "/nonexistent/source.jsonl", Format: SourceFormatJSONL}
	meta.Timestamp = TimestampInfo{Start: 1708300800000, End: 1708300860000}
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, string(sid)+"--metadata.json"), encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	return output
}

func TestPipelineCancellationInsideNestedDiffWalk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := NewProgressState()
	filesystem := &cancelNestedDiffFS{cancel: cancel, progress: progress, t: t}
	pipeline := &Pipeline{
		fs: filesystem,
		adapters: map[Harness]AdapterFactory{
			HarnessClaudeCode: func(FileSystem, GitResolver, salt.Salt) SourceAdapter {
				return progressAdapter{sessions: []DiscoveredSession{
					{SessionID: "session-one", Harness: HarnessClaudeCode},
					{SessionID: "session-two", Harness: HarnessClaudeCode},
					{SessionID: "session-three", Harness: HarnessClaudeCode},
				}}
			},
		},
		config: PipelineConfig{
			Sources:   map[Harness]SourceConfig{HarnessClaudeCode: {Enabled: true}},
			OutputDir: ResolvedPath(cancelNestedDiffOutputDir), Progress: progress,
			SessionFilter: func(DiscoveredSession) bool { t.Error("FILTER ran after cancellation"); return false },
		},
	}
	_, err := pipeline.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if !filesystem.canceled || filesystem.afterCancel != 0 {
		t.Fatalf("nested cancellation reached=%v, later filesystem calls=%d", filesystem.canceled, filesystem.afterCancel)
	}
	if filesystem.reads == 0 || filesystem.stats == 0 {
		t.Fatalf("DIFF did no nested filesystem work: ReadDir=%d Stat=%d", filesystem.reads, filesystem.stats)
	}
	snapshot := progress.Snapshot()
	if got := snapshot[StageDiff]; got.Done != 1 || got.Total != 3 || !got.Ended || !got.HasErr {
		t.Fatalf("interrupted DIFF = %+v, want 1/3 ended with error", got)
	}
	for _, stage := range StageOrder[2:] {
		if snapshot[stage].Started {
			t.Errorf("stage %s started after DIFF cancellation", stage)
		}
	}
}

const cancelNestedDiffOutputDir = "/out"

type cancelNestedDiffFS struct {
	emptyProgressFS
	lookups     int
	cancel      context.CancelFunc
	progress    *ProgressState
	t           *testing.T
	reads       int
	stats       int
	canceled    bool
	afterCancel int
}

var _ FileSystem = (*cancelNestedDiffFS)(nil)

// Only the second session has a nested layout to walk. DIFF progress names the
// session the walk belongs to; a call ordinal cannot, because one session's
// lookup probes both the flat and the nested layout more than once.
// The nested layout belongs to the SECOND session's context-aware lookup.
//
// Each session is looked up twice. The first lookup runs under a context of its
// own (metadataForRewrite in metadata_compatibility.go) and therefore cannot
// observe the run's cancellation, so the fixture gives that lookup nothing to
// walk. The second lookup carries the run's context and is the one whose nested
// walk the boundary interrupts. A bare call ordinal cannot name either lookup.
func (f *cancelNestedDiffFS) ReadDir(path string) ([]os.DirEntry, error) {
	if f.canceled {
		f.afterCancel++
	}
	f.reads++
	if path == cancelNestedDiffOutputDir {
		f.lookups++
	}
	if f.progress.Snapshot()[StageDiff].Done == 0 || f.lookups%2 == 1 {
		return nil, os.ErrNotExist
	}
	return fs.ReadDir(fstest.MapFS{
		"parent":  &fstest.MapFile{Mode: fs.ModeDir},
		"sibling": &fstest.MapFile{Mode: fs.ModeDir},
	}, ".")
}

func (f *cancelNestedDiffFS) Stat(path string) (os.FileInfo, error) {
	f.stats++
	if f.canceled {
		f.afterCancel++
	}
	if !f.canceled && strings.Contains(path, "/subagents/session-two/") {
		if got := f.progress.Snapshot()[StageDiff]; !got.Started || got.Done != 1 {
			f.t.Errorf("cancel boundary progress = %+v, want the first session already classified", got)
		}
		f.canceled = true
		f.cancel()
	}
	return nil, os.ErrNotExist
}

func TestPipelineDiffProgressAdvancesBeforeSlowSecondSession(t *testing.T) {
	progress := NewProgressState()
	secondReadStarted := make(chan struct{})
	releaseSecondRead := make(chan struct{})
	filesystem := &blockingReadDirFS{
		secondReadStarted: secondReadStarted,
		releaseSecondRead: releaseSecondRead,
	}
	sessions := []DiscoveredSession{
		{SessionID: "session-one", Harness: HarnessClaudeCode},
		{SessionID: "session-two", Harness: HarnessClaudeCode},
	}
	pipeline := &Pipeline{
		fs: filesystem,
		adapters: map[Harness]AdapterFactory{
			HarnessClaudeCode: func(FileSystem, GitResolver, salt.Salt) SourceAdapter {
				return progressAdapter{sessions: sessions}
			},
		},
		config: PipelineConfig{
			Sources: map[Harness]SourceConfig{
				HarnessClaudeCode: {Enabled: true},
			},
			OutputDir:     ResolvedPath("/out"),
			DryRun:        true,
			Progress:      progress,
			SessionFilter: func(DiscoveredSession) bool { return false },
		},
	}

	done := make(chan error, 1)
	go func() {
		_, err := pipeline.Run(context.Background())
		done <- err
	}()

	select {
	case <-secondReadStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("DIFF did not reach the controlled second-session metadata lookup")
	}

	if got := progress.Snapshot()[StageDiff].Done; got != 1 {
		t.Fatalf("DIFF progress while second session is blocked = %d, want 1", got)
	}
	close(releaseSecondRead)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pipeline run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DIFF did not finish after releasing the blocked metadata lookup")
	}
	diffProgress := progress.Snapshot()[StageDiff]
	if diffProgress.Done != 2 || diffProgress.Total != 2 || !diffProgress.Ended {
		t.Fatalf("final DIFF progress = %+v, want done=2 total=2 ended=true", diffProgress)
	}
}

func TestPipelineFilterProgressDoesNotEndBeforeSlowFilterReturns(t *testing.T) {
	progress := NewProgressState()
	secondFilterStarted := make(chan struct{})
	releaseSecondFilter := make(chan struct{})
	var filterCalls atomic.Int64
	sessions := []DiscoveredSession{
		{SessionID: "session-one", Harness: HarnessClaudeCode},
		{SessionID: "session-two", Harness: HarnessClaudeCode},
	}
	pipeline := &Pipeline{
		fs: emptyProgressFS{},
		adapters: map[Harness]AdapterFactory{
			HarnessClaudeCode: func(FileSystem, GitResolver, salt.Salt) SourceAdapter {
				return progressAdapter{sessions: sessions}
			},
		},
		config: PipelineConfig{
			Sources: map[Harness]SourceConfig{
				HarnessClaudeCode: {Enabled: true},
			},
			OutputDir: ResolvedPath("/out"),
			DryRun:    true,
			Progress:  progress,
			SessionFilter: func(DiscoveredSession) bool {
				if filterCalls.Add(1) == 2 {
					close(secondFilterStarted)
					<-releaseSecondFilter
				}
				return false
			},
		},
	}

	done := make(chan error, 1)
	go func() {
		_, err := pipeline.Run(context.Background())
		done <- err
	}()

	select {
	case <-secondFilterStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("FILTER did not reach the controlled second-session callback")
	}

	filterProgress := progress.Snapshot()[StageFilter]
	if filterProgress.Done != 1 {
		t.Fatalf("FILTER progress while second session is blocked = %d, want 1", filterProgress.Done)
	}
	if filterProgress.Ended {
		t.Fatal("FILTER progress ended before the blocked filter callback returned")
	}
	close(releaseSecondFilter)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("pipeline run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline did not finish after releasing the blocked filter callback")
	}
	filterProgress = progress.Snapshot()[StageFilter]
	if filterProgress.Done != 2 || filterProgress.Total != 2 || !filterProgress.Ended {
		t.Fatalf("final FILTER progress = %+v, want done=2 total=2 ended=true", filterProgress)
	}
}

type progressAdapter struct {
	sessions []DiscoveredSession
	discover func()
}

func (adapter progressAdapter) Harness() Harness { return HarnessClaudeCode }

func (adapter progressAdapter) Discover(context.Context, SourceConfig) ([]DiscoveredSession, error) {
	if adapter.discover != nil {
		adapter.discover()
	}
	return adapter.sessions, nil
}

func (adapter progressAdapter) ExtractMetadata(context.Context, DiscoveredSession) (*UnifiedMetadata, error) {
	return nil, errors.New("progressAdapter: ExtractMetadata should not run in this dry-run test")
}

type emptyProgressFS struct{}

func (emptyProgressFS) ReadFile(string) ([]byte, error) { return nil, os.ErrNotExist }

func (emptyProgressFS) WriteFile(string, []byte, os.FileMode) error { return os.ErrPermission }

func (emptyProgressFS) MkdirAll(string, os.FileMode) error { return os.ErrPermission }

func (emptyProgressFS) Stat(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

func (emptyProgressFS) Lstat(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

func (emptyProgressFS) WalkDir(string, fs.WalkDirFunc) error { return os.ErrNotExist }

func (emptyProgressFS) Rename(string, string) error { return os.ErrPermission }

func (emptyProgressFS) ReadDir(string) ([]os.DirEntry, error) { return nil, os.ErrNotExist }

func (emptyProgressFS) Remove(string) error { return os.ErrPermission }

func (emptyProgressFS) RemoveAll(string) error { return os.ErrPermission }

func (emptyProgressFS) CopyFile(string, string, os.FileMode) error { return os.ErrPermission }

type blockingReadDirFS struct {
	emptyProgressFS
	blocked           atomic.Bool
	secondReadStarted chan struct{}
	releaseSecondRead chan struct{}
}

func (filesystem *blockingReadDirFS) ReadDir(string) ([]os.DirEntry, error) {
	return fs.ReadDir(fstest.MapFS{"host": &fstest.MapFile{Mode: fs.ModeDir}}, ".")
}

// Block the metadata lookup of the SECOND session, named by its path. A call
// ordinal cannot name it: one session's lookup probes both the flat and the
// nested layout, so the number of probes per session is incidental.
func (filesystem *blockingReadDirFS) Stat(path string) (os.FileInfo, error) {
	if strings.Contains(path, "session-two") && filesystem.blocked.CompareAndSwap(false, true) {
		close(filesystem.secondReadStarted)
		<-filesystem.releaseSecondRead
	}
	return nil, os.ErrNotExist
}
