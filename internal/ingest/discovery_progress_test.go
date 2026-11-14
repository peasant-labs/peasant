package ingest_test

import (
	"context"
	_ "embed"
	"errors"
	"io/fs"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/discovery_progress.yaml
var discoveryProgressYAML []byte

//go:embed testdata/discovery_progress.manifest.yaml
var discoveryProgressManifest []byte

type discoveryProgressFile struct {
	Path string `yaml:"path"`
	Body string `yaml:"body"`
}

type discoveryProgressCase struct {
	Name          string                  `yaml:"name"`
	Files         []discoveryProgressFile `yaml:"files"`
	Rewrite       discoveryProgressFile   `yaml:"rewrite"`
	WarmCache     bool                    `yaml:"warm_cache"`
	FailedStat    string                  `yaml:"failed_stat"`
	UnreadableDir string                  `yaml:"unreadable_dir"`
	Total         int                     `yaml:"total"`
	Sessions      int                     `yaml:"sessions"`
	// FinalRereads is the adapter's DiscoveryStatistics count after Discover.
	FinalRereads int `yaml:"final_rereads"`
	// StepRereads, when set, is the required re-read count each advance carries.
	StepRereads []int `yaml:"step_rereads"`
}

type discoveryProgressEvent struct {
	harness ingest.Harness
	total   int
	delta   int
	rereads int
	isTotal bool
}

type discoveryProgressRecorder struct {
	events  []discoveryProgressEvent
	onTotal func()
}

func (r *discoveryProgressRecorder) DiscoveryTotal(h ingest.Harness, total int) {
	if r.onTotal != nil {
		r.onTotal()
	}
	r.events = append(r.events, discoveryProgressEvent{harness: h, total: total, isTotal: true})
}

func (r *discoveryProgressRecorder) DiscoveryAdvance(h ingest.Harness, step ingest.DiscoveryStep) {
	r.events = append(r.events, discoveryProgressEvent{harness: h, delta: step.Sources, rereads: step.RequiredRereads})
}

var _ ingest.DiscoveryProgress = (*discoveryProgressRecorder)(nil)

// partialWalkFS models a directory whose contents the walk cannot read.
type partialWalkFS struct {
	*testutil.CountingFS
	unreadable string
}

func (p *partialWalkFS) WalkDir(root string, walk fs.WalkDirFunc) error {
	return p.CountingFS.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if path == p.unreadable && err == nil && entry.IsDir() {
			if err := walk(path, nil, fs.ErrPermission); err != nil {
				return err
			}
			return fs.SkipDir
		}
		return walk(path, entry, err)
	})
}

var _ ingest.FileSystem = (*partialWalkFS)(nil)

func loadDiscoveryProgressCases(t *testing.T) []discoveryProgressCase {
	t.Helper()
	var fixture struct {
		Cases []discoveryProgressCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(discoveryProgressYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(discoveryProgressManifest, "discovery progress")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "discovery progress"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

func TestClaudeDiscoveryProgress(t *testing.T) {
	for _, c := range loadDiscoveryProgressCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			const root = "/sources"
			fs := testutil.NewCountingFS(testutil.NewMemFS())
			if err := fs.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			write := func(file discoveryProgressFile) {
				t.Helper()
				if err := fs.WriteFile(root+"/"+file.Path, []byte(file.Body+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for _, file := range c.Files {
				write(file)
			}
			cfg := ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}}
			walkFS := &partialWalkFS{CountingFS: fs}
			if c.UnreadableDir != "" {
				walkFS.unreadable = root + "/" + c.UnreadableDir
			}
			adapter := ingest.NewClaudeAdapter(walkFS, testutil.DefaultGitResolver(), salt.Salt{})
			if c.WarmCache {
				ingest.AttachClaudeEvidenceCache(adapter, newFakeClaudeEvidenceCache())
				if _, err := adapter.Discover(context.Background(), cfg); err != nil {
					t.Fatal(err)
				}
				write(c.Rewrite)
			}
			if c.FailedStat != "" {
				fs.Fail(testutil.FSOpStat, root+"/"+c.FailedStat, errors.New("fixture: unreadable source"))
			}
			events := discoveryProgressRecorder{}
			ingest.AttachDiscoveryProgress(adapter, &events)
			fs.ResetCounts()
			sessions, err := adapter.Discover(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(sessions) != c.Sessions || adapter.ReminedCount() != c.FinalRereads {
				t.Fatalf("sessions/required re-reads after discovery = %d/%d; want %d/%d", len(sessions), adapter.ReminedCount(), c.Sessions, c.FinalRereads)
			}
			if c.UnreadableDir != "" && len(adapter.DiscoveryDiagnostics()) == 0 {
				t.Fatal("unreadable directory was not reported")
			}
			var stepRereads []int
			for _, event := range events.events[1:] {
				stepRereads = append(stepRereads, event.rereads)
			}
			if c.StepRereads != nil && !reflect.DeepEqual(stepRereads, c.StepRereads) {
				t.Fatalf("required re-reads carried by the advances = %v, want %v", stepRereads, c.StepRereads)
			}
			if len(events.events) != c.Total+1 || !events.events[0].isTotal || events.events[0].total != c.Total {
				t.Fatalf("progress events = %+v; want total %d followed by one advance per source", events.events, c.Total)
			}
			for _, event := range events.events[1:] {
				oneClaudeSource := !event.isTotal && event.harness == ingest.HarnessClaudeCode && event.delta == 1
				rereadsReported := event.rereads >= 0 && event.rereads <= c.FinalRereads
				if !oneClaudeSource || !rereadsReported {
					t.Fatalf("invalid source advance (one source, with a reported re-read count no larger than the final count): %+v", event)
				}
			}
			if events.events[0].harness != ingest.HarnessClaudeCode {
				t.Fatalf("wrong harness: %+v", events.events[0])
			}
			if !c.WarmCache {
				reads, stats := fs.Total(testutil.FSOpReadFile), fs.Total(testutil.FSOpStat)
				ingest.AttachDiscoveryProgress(adapter, nil)
				fs.ResetCounts()
				without, err := adapter.Discover(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(sessions, without) || fs.Total(testutil.FSOpReadFile) != reads || fs.Total(testutil.FSOpStat) != stats {
					t.Fatalf("attaching progress changed discovery output or read/stat counts")
				}
			}
		})
	}
}
