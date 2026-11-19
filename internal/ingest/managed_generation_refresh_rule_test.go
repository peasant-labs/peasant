package ingest

import (
	"context"
	_ "embed"
	"maps"
	"testing"

	"github.com/peasant-labs/peasant/internal/salt"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/managed_generation_refresh.yaml
var managedGenerationRefreshFixture []byte

//go:embed testdata/managed_generation_refresh.manifest.yaml
var managedGenerationRefreshManifest []byte

type managedGenerationRefreshCase struct {
	Name             string `yaml:"name"`
	LaneMarked       bool   `yaml:"laneMarked"`
	ManagedTarget    *bool  `yaml:"managedTarget"`
	AdapterExtractor *bool  `yaml:"adapterExtractor"`
	StoreCapability  *bool  `yaml:"storeCapability"`
	Proof            string `yaml:"proof"`
	LatestAttempt    bool   `yaml:"latestAttempt"`
	Force            bool   `yaml:"force"`
	Reindex          bool   `yaml:"reindex"`
	Want             bool   `yaml:"want"`
}

const (
	refreshProofComplete   = "complete"
	refreshProofIncomplete = "incomplete"
	refreshProofMissing    = "missing"
)

type managedGenerationRefreshFixtureFamily struct {
	Cases []managedGenerationRefreshCase `yaml:"cases"`
}

// managedGenerationRefreshRuleStore advertises the metadata-only refresh
// capability. The embedded MetricsStore is never called: the rule only asks
// whether the capability exists.
type managedGenerationRefreshRuleStore struct{ MetricsStore }

func (managedGenerationRefreshRuleStore) RefreshManagedGenerationWithoutParse(context.Context, ManagedGenerationRefreshRequest) (ManagedGenerationRefreshOutcome, error) {
	return ManagedGenerationRefreshOutcome{}, nil
}

// managedGenerationRefreshRuleNoCapabilityStore deliberately omits the refresh
// capability, as a store without managed-generation support does.
type managedGenerationRefreshRuleNoCapabilityStore struct{ MetricsStore }

// managedGenerationRefreshRuleAdapter is the minimal SourceAdapter a rule case
// needs. The native-only case returns it without the extractor surface.
type managedGenerationRefreshRuleAdapter struct{ harness Harness }

func (a managedGenerationRefreshRuleAdapter) Harness() Harness { return a.harness }
func (managedGenerationRefreshRuleAdapter) Discover(context.Context, SourceConfig) ([]DiscoveredSession, error) {
	return nil, nil
}
func (managedGenerationRefreshRuleAdapter) ExtractMetadata(context.Context, DiscoveredSession) (*UnifiedMetadata, error) {
	return nil, nil
}

type managedGenerationRefreshRuleExtractor struct {
	managedGenerationRefreshRuleAdapter
}

func (managedGenerationRefreshRuleExtractor) ExtractMetadataFromTranscript(context.Context, []byte, *UnifiedMetadata) (*UnifiedMetadata, error) {
	return nil, nil
}

// TestManagedGenerationRefreshRule pins every term of the one admission of a
// managed generation to a no-parse settle. The rule is deliberately narrower
// than capturedInputBindsWithoutReparse: a managed generation may install a
// refreshed generation, but it may never bind its stale context. A term that
// moves keeps the ordinary parse path.
func TestManagedGenerationRefreshRule(t *testing.T) {
	var family managedGenerationRefreshFixtureFamily
	decodeOneFixture(t, managedGenerationRefreshFixture, &family, "managed generation refresh fixture")
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(managedGenerationRefreshManifest, &manifest); err != nil {
		t.Fatalf("decode managed generation refresh manifest: %v", err)
	}
	present := make(map[string]bool, len(family.Cases))
	for _, fixture := range family.Cases {
		if fixture.Name == "" || present[fixture.Name] {
			t.Fatalf("empty or duplicate managed generation refresh case %q", fixture.Name)
		}
		present[fixture.Name] = true
	}
	requireNamedCases(t, "managed generation refresh fixture", manifest.RequiredNames, present)

	const (
		captured = "1111111111111111111111111111111111111111111111111111111111111111"
		other    = "2222222222222222222222222222222222222222222222222222222222222222"
	)
	sid := SessionID("11111111-1111-1111-1111-111111111111")
	for _, fixture := range family.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			targets := GenerationTargets(HarvesterVersionRegistry)
			if fixture.ManagedTarget != nil && !*fixture.ManagedTarget {
				targets = maps.Clone(HarvesterVersionRegistry)
			}
			targets = maps.Clone(targets)
			extractor := fixture.AdapterExtractor == nil || *fixture.AdapterExtractor
			capability := fixture.StoreCapability == nil || *fixture.StoreCapability
			target := targets[HarnessClaudeCode]

			format := target.IndexVersion
			indexedAt := int64(1)
			state := &SessionIndexState{
				SessionID:      sid,
				Harness:        HarnessClaudeCode,
				IndexerVersion: target.IndexerVersion,
				IndexVersion:   &format,
				IndexedAt:      &indexedAt,
				IndexedInputHash: func() *string {
					value := captured
					return &value
				}(),
				ContentStatus: ContentCaptureComplete,
			}
			switch fixture.Proof {
			case "", refreshProofComplete:
			case refreshProofIncomplete:
				state.IndexedAt = nil
			case refreshProofMissing:
				state.IndexedInputHash = nil
			case "different":
				value := other
				state.IndexedInputHash = &value
			case "index-format-old":
				old := target.IndexVersion - 1
				state.IndexVersion = &old
			case "producer-behind":
				state.IndexerVersion = target.IndexerVersion - 1
			default:
				t.Fatalf("unknown proof %q", fixture.Proof)
			}
			if fixture.LatestAttempt {
				value := other
				state.LatestAttemptArtifactHash = &value
			}

			var metricsStore MetricsStore = managedGenerationRefreshRuleStore{}
			if !capability {
				metricsStore = managedGenerationRefreshRuleNoCapabilityStore{}
			}
			adapter := func(FileSystem, GitResolver, salt.Salt) SourceAdapter {
				base := managedGenerationRefreshRuleAdapter{harness: HarnessClaudeCode}
				if extractor {
					return managedGenerationRefreshRuleExtractor{base}
				}
				return base
			}
			pipeline := &Pipeline{
				metricsStore:     metricsStore,
				resolvedVersions: targets,
				adapters:         map[Harness]AdapterFactory{HarnessClaudeCode: adapter},
				config:           PipelineConfig{Force: fixture.Force, Reindex: fixture.Reindex},
			}
			input := &CapturedIndexInput{
				session:                  DiscoveredSession{SessionID: sid, Harness: HarnessClaudeCode, SourceFormat: SourceFormatJSONL},
				kind:                     TranscriptSourceFile,
				inputHash:                captured,
				expected:                 state,
				metadata:                 &UnifiedMetadata{SessionID: sid, ModelHarness: HarnessClaudeCode},
				managedGenerationRefresh: fixture.LaneMarked,
			}
			if got := pipeline.managedGenerationRefreshApplies(input); got != fixture.Want {
				t.Fatalf("managedGenerationRefreshApplies = %t, want %t", got, fixture.Want)
			}
		})
	}
}
