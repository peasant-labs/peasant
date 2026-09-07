package perf_test

import (
	"context"
	_ "embed"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/perf"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/profile_contract/parent_context.yaml
var parentContextYAML []byte

//go:embed testdata/profile_contract/parent_context_manifest.yaml
var parentContextManifestYAML []byte

type parentContextFixture struct {
	Name     string   `yaml:"name"`
	Enabled  bool     `yaml:"enabled"`
	Parents  []string `yaml:"parents"`
	Children []string `yaml:"children"`
}

func loadParentContextFixtures(t *testing.T) []parentContextFixture {
	t.Helper()
	var fixtures struct {
		Cases []parentContextFixture `yaml:"cases"`
	}
	if err := yaml.Unmarshal(parentContextYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(parentContextManifestYAML, "parent context")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, fixture := range fixtures.Cases {
		names = append(names, fixture.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "parent context"); err != nil {
		t.Fatal(err)
	}
	return fixtures.Cases
}

func TestParentSpanContext(t *testing.T) {
	for _, fixture := range loadParentContextFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			collector := perf.NewCollector()
			var rec perf.Recorder = perf.Nop{}
			if fixture.Enabled {
				rec = collector
			}
			ctx, cancel := context.WithCancel(perf.ContextWithRecorder(context.Background(), rec))
			defer cancel()
			var scopes []context.Context
			scopes = append(scopes, ctx)
			for _, id := range fixture.Parents {
				ctx = perf.ContextWithParentSpan(ctx, id)
				scopes = append(scopes, ctx)
			}
			var wg sync.WaitGroup
			for _, id := range fixture.Children {
				wg.Add(1)
				go func() {
					defer wg.Done()
					child := perf.ContextWithParentSpan(ctx, id)
					if perf.ParentSpanFromContext(child) != id || perf.RecorderFromContext(child) != rec {
						t.Error("child scope changed recorder or parent identity")
					}
					perf.RecorderFromContext(child).StartChildSpan(perf.StagePushSessionLoad, perf.ParentSpanFromContext(child), nil).End(perf.OutcomeOK, nil)
				}()
			}
			wg.Wait()
			cancel()
			for i, scope := range scopes {
				want := ""
				if i > 0 {
					want = fixture.Parents[i-1]
				}
				if perf.ParentSpanFromContext(scope) != want || perf.RecorderFromContext(scope) != rec {
					t.Error("descendant scope mutated an ancestor or recorder")
				}
				if scope.Err() != context.Canceled {
					t.Error("parent-span scope lost cancellation")
				}
			}
			if !fixture.Enabled && len(collector.Events()) != 0 {
				t.Fatal("disabled parent context emitted events")
			}
			if fixture.Enabled {
				remaining := make(map[string]int)
				for _, parent := range fixture.Children {
					remaining[parent]++
				}
				for _, event := range collector.Events() {
					remaining[event.ParentSpanID]--
				}
				for parent, count := range remaining {
					if count != 0 {
						t.Errorf("recorded child ancestry differs for parent %q by %d", parent, count)
					}
				}
			}
		})
	}
}
