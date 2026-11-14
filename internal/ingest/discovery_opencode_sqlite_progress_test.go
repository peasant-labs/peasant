package ingest_test

import (
	_ "embed"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/discovery_opencode_sqlite_progress.yaml
var discoverySQLiteProgressYAML []byte

//go:embed testdata/discovery_opencode_sqlite_progress.manifest.yaml
var discoverySQLiteProgressManifest []byte

func TestOpenCodeSQLiteDiscoveryProgress(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name   string `yaml:"name"`
			Source string `yaml:"source"`
			Total  int    `yaml:"total"`
			Opens  int    `yaml:"opens"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(discoverySQLiteProgressYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(discoverySQLiteProgressManifest, "OpenCode SQLite discovery progress")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "OpenCode SQLite discovery progress"); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			source := testfixture.MaterializeByName(t, c.Source)
			root, err := ingest.NewResolvedPath(filepath.Dir(source.Path))
			if err != nil {
				t.Fatal(err)
			}
			fs := &ingest.OSFileSystem{}
			counter := &openCounter{counts: make(map[string]int)}
			adapter, err := ingest.NewOpenCodeAdapterWithCandidateProbe(fs, testutil.NoGitResolver(), salt.Salt{}, "latest", fixedCandidateEnvironment{}, fs, counter.opener(), ingest.DefaultOpenCodeSQLiteSourceOptions())
			if err != nil {
				t.Fatal(err)
			}
			var progress discoveryProgressRecorder
			ingest.AttachDiscoveryProgress(adapter, &progress)
			sessions, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
			if err != nil {
				t.Fatal(err)
			}
			if len(sessions) == 0 || len(progress.events) != c.Total+1 {
				t.Fatalf("SQLite discovery = %d sessions, events = %+v; want %d source(s) counted", len(sessions), progress.events, c.Total)
			}
			if !progress.events[0].isTotal || progress.events[0].total != c.Total {
				t.Fatalf("SQLite total event = %+v, want %d", progress.events[0], c.Total)
			}
			for _, event := range progress.events[1:] {
				if event.isTotal || event.delta != 1 || event.harness != ingest.HarnessOpenCode {
					t.Fatalf("invalid SQLite source advance: %+v", event)
				}
			}
			counter.mu.Lock()
			opens := counter.counts[filepath.Clean(source.Path)]
			counter.mu.Unlock()
			if opens != c.Opens {
				t.Fatalf("SQLite source opened %d times, want %d (schema probe and shared discovery)", opens, c.Opens)
			}
		})
	}
}
