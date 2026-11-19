package ingest_test

import (
	_ "embed"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/refresh_skip_baseline.yaml
var refreshSkipBaselineCases []byte

//go:embed testdata/refresh_skip_baseline.manifest.yaml
var refreshSkipBaselineManifest []byte

type refreshSkipCounts struct {
	Proofs  int64 `yaml:"proofs"`
	Settles int64 `yaml:"settles"`
	Copies  int64 `yaml:"copies"`
	Builds  int64 `yaml:"builds"`
	Parses  int64 `yaml:"parses"`
}

// TestRefreshSkipBaselineChangesTheExecutedPath proves the measurement
// baseline really parses where the skips would not. The same adapter bump runs
// with and without PipelineConfig.DisableRefreshSkips against a store that
// keeps every capability; the decision and its census stay the same, and only
// the executed work differs.
func TestRefreshSkipBaselineChangesTheExecutedPath(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name          string            `yaml:"name"`
			ChangeContext bool              `yaml:"changeContext"`
			Optimized     refreshSkipCounts `yaml:"optimized"`
			Baseline      refreshSkipCounts `yaml:"baseline"`
		} `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := testutil.DecodeFixtureYAML(refreshSkipBaselineCases, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := testutil.DecodeFixtureYAML(refreshSkipBaselineManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		present[c.Name] = true
		for _, mode := range []struct {
			label    string
			disabled bool
			want     refreshSkipCounts
		}{{"optimized", false, c.Optimized}, {"baseline", true, c.Baseline}} {
			t.Run(c.Name+"/"+mode.label, func(t *testing.T) {
				root := t.TempDir()
				source, output, empty := filepath.Join(root, "source"), filepath.Join(root, "managed"), filepath.Join(root, "empty")
				mustMkdir(t, source)
				mustMkdir(t, empty)
				id := writeManagedRefreshPipelineCorpus(t, source, 1, 8)[0]
				db := openManagedRefreshPipelineStore(t, filepath.Join(root, "db"), output)
				t.Cleanup(func() { _ = db.Close() })
				if initial := runManagedRefreshPipeline(t, db, source, output); initial.Summary.Errors != 0 || initial.Summary.Indexed != 1 {
					t.Fatalf("initial import: %+v", initial.Summary)
				}
				previous := managedRefreshPipelineActiveGeneration(t, db, id)
				stored := ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode].AdapterVersion - 1
				managedRefreshPipelineDowngrade(t, db, stored)
				if c.ChangeContext && managedRefreshPipelineChangeMetadata(t, output, stored, managedRefreshPipelineForcedProject) != 1 {
					t.Fatal("expected one changed metadata file")
				}
				counted := &proofCountingRefreshStore{countedManagedRefreshStore: &countedManagedRefreshStore{Store: db}}
				var parses, builds atomic.Int64
				var transcript []byte
				result := runManagedRefreshPipelineConfigured(t, counted, empty, output, &parses, &builds, &transcript, &ingest.OSFileSystem{}, &ingest.IndexProfiler{}, ingest.DefaultAdapterRegistry,
					func(cfg *ingest.PipelineConfig) { cfg.DisableRefreshSkips = mode.disabled })
				if result.Summary.Errors != 0 || result.Census == nil || len(result.Census.Rows) != 1 {
					t.Fatalf("summary %+v census %+v", result.Summary, result.Census)
				}
				if row := result.Census.Rows[0]; row.Lane != ingest.RefreshLaneStoredMetadata || row.Reason != ingest.OutcomeReasonRefreshAdapterBehind {
					t.Fatalf("decision changed: summary %+v census %+v", result.Summary, result.Census)
				}
				got := refreshSkipCounts{Proofs: counted.proofs.Load(), Settles: counted.settles.Load(), Copies: counted.calls.Load(), Builds: builds.Load(), Parses: parses.Load()}
				if got != mode.want {
					t.Fatalf("executed %+v, want %+v", got, mode.want)
				}
				active := managedRefreshPipelineActiveGeneration(t, db, id)
				if c.ChangeContext {
					if active == previous || managedRefreshPipelineProjectName(t, db, id, active) != managedRefreshPipelineForcedProject {
						t.Fatalf("generation %q did not install the changed context", active)
					}
				} else if active != previous {
					t.Fatalf("an identical projection replaced generation %q with %q", previous, active)
				}
			})
		}
	}
	if err := testutil.RequireFixtureNames("refresh_skip_baseline.yaml", "case", manifest.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
}
