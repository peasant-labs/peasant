package ingest_test

import (
	"context"
	_ "embed"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/discovery_progress_adapters.yaml
var discoveryAdaptersYAML []byte

//go:embed testdata/discovery_progress_adapters.manifest.yaml
var discoveryAdaptersManifest []byte

func TestAdapterDiscoveryProgress(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name    string                  `yaml:"name"`
			Harness string                  `yaml:"harness"`
			Files   []discoveryProgressFile `yaml:"files"`
			Roots   []string                `yaml:"roots"`
			Total   int                     `yaml:"total"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(discoveryAdaptersYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(discoveryAdaptersManifest, "adapter discovery progress")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "adapter discovery progress"); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			const root = "/sources"
			fs := testutil.NewCountingFS(testutil.NewMemFS())
			for _, file := range c.Files {
				if err := fs.WriteFile(root+"/"+file.Path, []byte(file.Body+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var adapter ingest.SourceAdapter
			switch c.Harness {
			case ingest.HarnessCodex.String():
				adapter = ingest.NewCodexAdapter(fs, testutil.NoGitResolver(), salt.Salt{})
			case ingest.HarnessOpenCode.String():
				adapter = newTestOpenCodeAdapter(fs, testutil.NoGitResolver(), salt.Salt{})
			case ingest.HarnessPi.String():
				adapter = ingest.NewPiAdapter(fs, testutil.NoGitResolver(), salt.Salt{})
			case ingest.HarnessCursor.String():
				adapter = ingest.NewCursorAdapter(fs, testutil.NoGitResolver(), salt.Salt{})
			case ingest.HarnessStrike.String():
				adapter = ingest.NewStrikeAdapter(fs, testutil.NoGitResolver(), salt.Salt{})
			default:
				t.Fatalf("unknown fixture harness %q", c.Harness)
			}
			cfg := ingest.SourceConfig{Enabled: true}
			if len(c.Roots) == 0 {
				cfg.Paths = []ingest.ResolvedPath{root}
			} else {
				for _, subroot := range c.Roots {
					path, err := ingest.NewResolvedPath(root + "/" + subroot)
					if err != nil {
						t.Fatal(err)
					}
					cfg.Paths = append(cfg.Paths, path)
				}
			}
			fs.ResetCounts()
			baseline, err := adapter.Discover(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			reads, stats := fs.Total(testutil.FSOpReadFile), fs.Total(testutil.FSOpStat)
			progress := discoveryProgressRecorder{onTotal: func() {
				for _, file := range c.Files {
					if fs.Count(testutil.FSOpReadFile, root+"/"+file.Path) != 0 {
						t.Fatalf("source %q was read before its discovery total was reported", file.Path)
					}
				}
			}}
			ingest.AttachDiscoveryProgress(adapter, &progress)
			fs.ResetCounts()
			sessions, err := adapter.Discover(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(sessions, baseline) || fs.Total(testutil.FSOpReadFile) != reads || fs.Total(testutil.FSOpStat) != stats {
				t.Fatalf("progress changed discovery or its file reads/stats")
			}
			if len(progress.events) != c.Total+1 || !progress.events[0].isTotal || progress.events[0].total != c.Total {
				t.Fatalf("progress = %+v, want total %d then one advance per source", progress.events, c.Total)
			}
			if len(sessions) == 0 || progress.events[0].harness != adapter.Harness() {
				t.Fatalf("fixture discovered no sessions or reported the wrong harness: %+v", progress.events)
			}
			for _, event := range progress.events[1:] {
				oneSource := !event.isTotal && event.harness == adapter.Harness() && event.delta == 1
				if !oneSource || event.rereads >= 0 {
					t.Fatalf("invalid progress event (one source, no re-read count from an adapter without an evidence cache): %+v", event)
				}
			}
		})
	}
}
