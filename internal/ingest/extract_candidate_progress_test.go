package ingest_test

import (
	"context"
	_ "embed"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/extract_candidate_progress.yaml
var extractCandidateProgressYAML []byte

type noDeleteFileSystem struct {
	ingest.FileSystem
	deletions atomic.Int64
}

func (f *noDeleteFileSystem) Remove(string) error {
	f.deletions.Add(1)
	return errors.New("unexpected removal")
}
func (f *noDeleteFileSystem) RemoveAll(string) error {
	f.deletions.Add(1)
	return errors.New("unexpected recursive removal")
}

var _ ingest.FileSystem = (*noDeleteFileSystem)(nil)

type blockedCandidateAdapter struct {
	sessions []ingest.DiscoveredSession
	started  chan struct{}
	once     sync.Once
}

func (*blockedCandidateAdapter) Harness() ingest.Harness { return ingest.HarnessClaudeCode }
func (a *blockedCandidateAdapter) Discover(context.Context, ingest.SourceConfig) ([]ingest.DiscoveredSession, error) {
	return a.sessions, nil
}
func (a *blockedCandidateAdapter) ExtractMetadata(ctx context.Context, _ ingest.DiscoveredSession) (*ingest.UnifiedMetadata, error) {
	a.once.Do(func() { close(a.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

var _ ingest.SourceAdapter = (*blockedCandidateAdapter)(nil)

type failingCandidateAdapter struct{ blockedCandidateAdapter }

func (*failingCandidateAdapter) ExtractMetadata(context.Context, ingest.DiscoveredSession) (*ingest.UnifiedMetadata, error) {
	return nil, errors.New("synthetic extraction failure")
}

var _ ingest.SourceAdapter = (*failingCandidateAdapter)(nil)

func TestFailedCaptureAdvancesExtractBytes(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredFailureNames"`
		Cases         []struct {
			Name       string   `yaml:"name"`
			SessionIDs []string `yaml:"sessionIds"`
		} `yaml:"failureCases"`
	}
	if err := yaml.Unmarshal(extractCandidateProgressYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			fs := &noDeleteFileSystem{FileSystem: testutil.NewMemFS()}
			adapter := &failingCandidateAdapter{}
			for _, raw := range row.SessionIDs {
				id, err := ingest.NewSessionID(raw)
				if err != nil {
					t.Fatal(err)
				}
				path := "/synthetic/" + raw + ".jsonl"
				if err := fs.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				adapter.sessions = append(adapter.sessions, ingest.DiscoveredSession{SessionID: id, Harness: ingest.HarnessClaudeCode, SourcePath: ingest.ResolvedPath(path), SourceFormat: ingest.SourceFormatJSONL, ModTime: time.Unix(1, 0)})
			}
			progress := ingest.NewProgressState()
			cfg := ingest.PipelineConfig{OutputDir: "/synthetic-output", Progress: progress, Parallelism: 1, Sources: map[ingest.Harness]ingest.SourceConfig{ingest.HarnessClaudeCode: {Enabled: true}}}
			adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter { return adapter }}
			pipeline, err := newTestPipeline(fs, testutil.NoGitResolver(), adapters, cfg)
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary.Errors != len(row.SessionIDs) {
				t.Fatalf("errors = %d, want %d", result.Summary.Errors, len(row.SessionIDs))
			}
			stage := progress.Snapshot()[ingest.StageExtract]
			if !stage.Ended || stage.BytesTotal != int64(len(row.SessionIDs))*3 || stage.BytesDone != stage.BytesTotal {
				t.Fatalf("failed capture progress = %+v", stage)
			}
		})
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing case %q", name)
		}
	}
}

func TestPipelinePublishesCandidatesBeforeCaptureCompletes(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name           string   `yaml:"name"`
			SessionIDs     []string `yaml:"sessionIds"`
			WantCandidates int      `yaml:"wantCandidates"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(extractCandidateProgressYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if seen[row.Name] {
			t.Fatalf("duplicate case %q", row.Name)
		}
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			fs := &noDeleteFileSystem{FileSystem: testutil.NewMemFS()}
			adapter := &blockedCandidateAdapter{started: make(chan struct{})}
			for _, raw := range row.SessionIDs {
				id, err := ingest.NewSessionID(raw)
				if err != nil {
					t.Fatal(err)
				}
				path := "/synthetic/" + raw + ".jsonl"
				if err := fs.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				adapter.sessions = append(adapter.sessions, ingest.DiscoveredSession{SessionID: id, Harness: ingest.HarnessClaudeCode, SourcePath: ingest.ResolvedPath(path), SourceFormat: ingest.SourceFormatJSONL, ModTime: time.Unix(1, 0)})
			}
			progress := ingest.NewProgressState()
			cfg := ingest.PipelineConfig{OutputDir: "/synthetic-output", Progress: progress, IndexProfiler: &ingest.IndexProfiler{}, Parallelism: 1, Sources: map[ingest.Harness]ingest.SourceConfig{ingest.HarnessClaudeCode: {Enabled: true}}}
			adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter { return adapter }}
			pipeline, err := newTestPipeline(fs, testutil.NoGitResolver(), adapters, cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			go func() { _, _ = pipeline.Run(ctx); close(done) }()
			testwait.Receive(t, adapter.started, "candidate capture started")
			stage := progress.Snapshot()[ingest.StageExtract]
			if !stage.CandidatesKnown || stage.CandidateCount != row.WantCandidates || stage.Done != 0 {
				t.Fatalf("candidate state = %+v", stage)
			}
			if stage.BytesTotal != int64(row.WantCandidates)*3 || stage.BytesDone != 0 {
				t.Fatalf("extract byte state = %+v, want three bytes per source and no completed work", stage)
			}
			if _, ok := progress.WriteLaneSnapshot(); !ok {
				t.Fatal("production run did not expose its writer recorder")
			}
			cancel()
			testwait.Receive(t, done, "cancelled synthetic pipeline stopped")
			if fs.deletions.Load() != 0 {
				t.Fatal("synthetic pipeline attempted a filesystem removal")
			}
		})
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing case %q", name)
		}
	}
}
