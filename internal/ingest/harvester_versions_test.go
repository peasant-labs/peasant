package ingest

import (
	"context"
	_ "embed"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/harvester_versions.yaml
var harvesterVersionsYAML []byte

type harvesterVersionFixture struct {
	Name      string    `yaml:"name"`
	Harnesses []Harness `yaml:"harnesses"`
	Adapter   int       `yaml:"adapter"`
	Indexer   int       `yaml:"indexer"`
	Index     int       `yaml:"index"`
	Error     string    `yaml:"error"`
}

type nonAtomicHarvesterStore struct {
	MetricsStore
	wrote bool
}

var _ MetricsStore = (*nonAtomicHarvesterStore)(nil)

func (s *nonAtomicHarvesterStore) IndexSessionEntries(context.Context, SessionID, []schema.SessionEntry) error {
	s.wrote = true
	return nil
}

func TestPipelineRefusesNonAtomicIndexerWrites(t *testing.T) {
	t.Parallel()
	store := &nonAtomicHarvesterStore{}
	pipeline := &Pipeline{metricsStore: store}
	sid := SessionID("77777777-7777-4777-8777-777777777777")
	result := indexParseResult{
		im:      indexedMeta{session: DiscoveredSession{SessionID: sid, Harness: HarnessClaudeCode}},
		entries: []schema.SessionEntry{{SessionID: sid, EntryIndex: 0, EntryType: schema.EntryTypeText, Role: schema.RoleUser}},
	}
	flush := pipeline.flushIndexParseResults(t.Context(), []indexParseResult{result}, IndexOutcomeIndexed, "pipeline", nil)
	if store.wrote || len(flush.indexed) != 1 || flush.indexed[0].indexed || flush.writeTxs != 0 || len(flush.logEntries) != 1 || flush.logEntries[0].Outcome != IndexOutcomeError {
		t.Fatalf("nontransactional index store was not refused safely: wrote=%v result=%+v", store.wrote, flush)
	}
	if got := flush.logEntries[0].IndexerVersion; got != HarvesterVersionRegistry[HarnessClaudeCode].IndexerVersion {
		t.Fatalf("direct Pipeline attempt version=%d", got)
	}
	data, err := json.Marshal(flush.logEntries[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["IndexVersion"]; !ok {
		t.Fatal("existing index-log JSON key changed")
	}
	if _, ok := fields["IndexerVersion"]; ok {
		t.Fatal("unapproved index-log JSON key added")
	}
}

func loadHarvesterVersionFixtures(t *testing.T) []harvesterVersionFixture {
	t.Helper()
	var fixtures struct {
		Cases []harvesterVersionFixture `yaml:"cases"`
	}
	if err := yaml.Unmarshal(harvesterVersionsYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate harvester fixture name %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	for _, required := range []string{"canonical_registry", "injected_subset_without_indexers", "missing_adapter_target", "nonpositive_adapter", "nonpositive_indexer", "nonpositive_index", "unsupported_output_format", "unimplemented_harness"} {
		if !names[required] {
			t.Errorf("missing required harvester fixture %q", required)
		}
	}
	return fixtures.Cases
}

func TestHarvesterVersionRegistry(t *testing.T) {
	t.Parallel()
	for _, fixture := range loadHarvesterVersionFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			targets := make(map[Harness]HarvesterVersions)
			for _, harness := range fixture.Harnesses {
				targets[harness] = HarvesterVersions{AdapterVersion: fixture.Adapter, IndexerVersion: fixture.Indexer, IndexVersion: fixture.Index}
			}
			adapters := map[Harness]AdapterFactory{HarnessClaudeCode: DefaultAdapterRegistry[HarnessClaudeCode]}
			if fixture.Name == "canonical_registry" {
				if !maps.Equal(targets, HarvesterVersionRegistry) {
					t.Fatalf("canonical targets = %+v, want %+v", HarvesterVersionRegistry, targets)
				}
				want := slices.Sorted(maps.Keys(targets))
				if !slices.Equal(want, slices.Sorted(maps.Keys(DefaultAdapterRegistry))) || !slices.Equal(want, slices.Sorted(maps.Keys(NewIndexerRegistry(nil, IndexerRegistryOptions{})))) {
					t.Fatal("production adapters, indexers and version targets must have exact harness membership")
				}
				adapters = DefaultAdapterRegistry
			}
			option := WithHarvesterVersions(targets)
			clear(targets) // neither this map nor one pipeline may mutate another's targets
			p, err := NewPipeline(nil, nil, adapters, PipelineConfig{}, option)
			if fixture.Error != "" {
				if err == nil || !strings.Contains(err.Error(), fixture.Error) {
					t.Fatalf("constructor error = %v, want %q", err, fixture.Error)
				}
				// Direct struct construction must fail before touching filesystem or DB.
				direct := &Pipeline{adapters: adapters}
				option(direct)
				if _, err := direct.Run(context.Background()); err == nil || !strings.Contains(err.Error(), fixture.Error) {
					t.Fatalf("direct Run error = %v, want %q", err, fixture.Error)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			clear(p.harvesterVersions)
			if _, err := NewPipeline(nil, nil, adapters, PipelineConfig{}, option); err != nil {
				t.Fatalf("reused option lost targets: %v", err)
			}
		})
	}
}
