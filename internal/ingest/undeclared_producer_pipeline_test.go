package ingest_test

import (
	_ "embed"
	"maps"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/undeclared_producer_pipeline.yaml
var undeclaredProducerPipelineCases []byte

//go:embed testdata/undeclared_producer_pipeline.manifest.yaml
var undeclaredProducerPipelineManifest []byte

// withholdingClaudeAdapter registers the claude-code adapter as one that
// withholds its change scope: an undeclared producer.
type withholdingClaudeAdapter struct {
	*ingest.ClaudeAdapter
}

func (*withholdingClaudeAdapter) WithholdsChangeScope() bool { return true }

var _ ingest.ChangeScopeWithholder = (*withholdingClaudeAdapter)(nil)

// TestUndeclaredProducerTakesTheFullStep runs the production pipeline on a
// stamp-only adapter bump with the shipped adapter and with one that withholds
// its change scope. The undeclared adapter makes the run parse instead of
// proving, settling or copying, and the session's decision names it.
func TestUndeclaredProducerTakesTheFullStep(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name       string `yaml:"name"`
			Undeclared bool   `yaml:"undeclared"`
			Want       struct {
				Reason  string `yaml:"reason"`
				Lane    string `yaml:"lane"`
				Proofs  int64  `yaml:"proofs"`
				Settles int64  `yaml:"settles"`
				Copies  int64  `yaml:"copies"`
				Builds  int64  `yaml:"builds"`
			} `yaml:"want"`
		} `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := testutil.DecodeFixtureYAML(undeclaredProducerPipelineCases, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := testutil.DecodeFixtureYAML(undeclaredProducerPipelineManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		present[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			reason, err := ingest.NewRefreshReason(c.Want.Reason)
			if err != nil {
				t.Fatal(err)
			}
			lane, err := ingest.NewRefreshLane(c.Want.Lane)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			source, output := filepath.Join(root, "source"), filepath.Join(root, "managed")
			mustMkdir(t, source)
			id := writeManagedRefreshPipelineCorpus(t, source, 1, 8)[0]
			db := openManagedRefreshPipelineStore(t, filepath.Join(root, "db"), output)
			t.Cleanup(func() { _ = db.Close() })
			if initial := runManagedRefreshPipeline(t, db, source, output); initial.Summary.Errors != 0 || initial.Summary.Indexed != 1 {
				t.Fatalf("initial import: %+v", initial.Summary)
			}
			managedRefreshPipelineDowngrade(t, db, ingest.GenerationTargets(ingest.HarvesterVersionRegistry)[ingest.HarnessClaudeCode].AdapterVersion-1)
			// The registered factory refuses missing dependencies, as a
			// production factory that needs a real filesystem does. Reading a
			// declaration from an adapter built without the pipeline's own
			// dependencies fails here, not only in a wider suite. Errorf, not
			// Fatalf: the factory runs on pipeline goroutines.
			var missingDeps atomic.Int64
			adapters := maps.Clone(ingest.DefaultAdapterRegistry)
			adapters[ingest.HarnessClaudeCode] = func(fs ingest.FileSystem, git ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
				if fs == nil || git == nil {
					missingDeps.Add(1)
				}
				adapter := ingest.NewClaudeAdapter(fs, git, s)
				if c.Undeclared {
					return &withholdingClaudeAdapter{ClaudeAdapter: adapter}
				}
				return adapter
			}
			counted := &proofCountingRefreshStore{countedManagedRefreshStore: &countedManagedRefreshStore{Store: db}}
			var parses, builds atomic.Int64
			var transcript []byte
			result := runManagedRefreshPipelineConfigured(t, counted, source, output, &parses, &builds, &transcript, &ingest.OSFileSystem{}, &ingest.IndexProfiler{}, adapters, nil)
			if missingDeps.Load() != 0 {
				t.Fatalf("the pipeline built the adapter %d times without its filesystem or git resolver", missingDeps.Load())
			}
			if result.Summary.Errors != 0 || result.Census == nil || len(result.Census.Rows) != 1 {
				t.Fatalf("summary %+v census %+v", result.Summary, result.Census)
			}
			if row := result.Census.Rows[0]; row.Reason != reason.OutcomeReason() || row.Lane != lane {
				t.Fatalf("census row %+v, want %s on %s", row, reason, lane)
			}
			named := 0
			for _, session := range result.Sessions {
				decision := session.Decision
				if session.SessionID != id || decision.Reason != reason {
					continue
				}
				if decision.Lane == lane && decision.AdapterUndeclared {
					named++
				}
			}
			if want := map[bool]int{true: 1, false: 0}[c.Undeclared]; named != want {
				t.Fatalf("%d session results name the undeclared adapter, want %d: %+v", named, want, result.Sessions)
			}
			got := [4]int64{counted.proofs.Load(), counted.settles.Load(), counted.calls.Load(), builds.Load()}
			if want := [4]int64{c.Want.Proofs, c.Want.Settles, c.Want.Copies, c.Want.Builds}; got != want {
				t.Fatalf("proofs, settles, copies, builds = %v, want %v", got, want)
			}
		})
	}
	if err := testutil.RequireFixtureNames("undeclared_producer_pipeline.yaml", "case", manifest.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
}
