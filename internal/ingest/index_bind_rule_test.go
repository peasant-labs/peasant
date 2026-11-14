package ingest

import (
	"context"
	_ "embed"
	"maps"
	"testing"
)

//go:embed testdata/index_bind_rule.yaml
var indexBindRuleFixtureData []byte

type indexBindRuleFixtures struct {
	Required []string `yaml:"required_names"`
	Cases    []struct {
		Name               string `yaml:"name"`
		Published          bool   `yaml:"published"`
		Bound              bool   `yaml:"bound"`
		Force              bool   `yaml:"force"`
		Reindex            bool   `yaml:"reindex"`
		LatestAttempt      bool   `yaml:"latestAttempt"`
		ProducerBehind     bool   `yaml:"producerBehind"`
		IndexFormatMissing bool   `yaml:"indexFormatMissing"`
		InputProof         string `yaml:"inputProof"`
		ContentIncomplete  bool   `yaml:"contentIncomplete"`
		ManagedGeneration  bool   `yaml:"managedGeneration"`
		NoBinder           bool   `yaml:"noBinder"`
		DirectoryInput     bool   `yaml:"directoryInput"`
		Want               bool   `yaml:"want"`
	} `yaml:"cases"`
}

// indexBindRuleStore can bind; the embedded store is never called.
type indexBindRuleStore struct{ MetricsStore }

func (indexBindRuleStore) BindPublicationIndex(context.Context, PublicationIndexBind) error {
	return nil
}

// indexBindRuleNoBinderStore cannot bind.
type indexBindRuleNoBinderStore struct{ MetricsStore }

// TestIndexBindWithoutReparseRule pins each term of the rule "bind without a
// re-parse" on the production predicate: the two ways a session is behind
// bind, and every other pending term keeps the ordinary parse.
func TestIndexBindWithoutReparseRule(t *testing.T) {
	t.Parallel()
	var fixtures indexBindRuleFixtures
	decodeOneFixture(t, indexBindRuleFixtureData, &fixtures, "index bind rule fixture")
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate index bind rule fixture %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	requireNamedCases(t, "index bind rule fixture", fixtures.Required, names)
	const (
		captured = "1111111111111111111111111111111111111111111111111111111111111111"
		other    = "2222222222222222222222222222222222222222222222222222222222222222"
	)
	sid := SessionID("11111111-1111-1111-1111-111111111111")
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			targets := maps.Clone(HarvesterVersionRegistry)
			if fixture.ManagedGeneration {
				targets[HarnessClaudeCode] = GenerationRepairTargets[HarnessClaudeCode]
			}
			target := targets[HarnessClaudeCode]
			format, proof, latest := target.IndexVersion, captured, other
			state := &SessionIndexState{
				SessionID: sid, Harness: HarnessClaudeCode,
				IndexerVersion: target.IndexerVersion, IndexVersion: &format,
				IndexedInputHash:           &proof,
				ContentStatus:              ContentCaptureComplete,
				PublicationCaptureRevision: 2,
				PublicationBound:           fixture.Bound,
			}
			switch fixture.InputProof {
			case "", "same":
			case "different":
				differs := other
				state.IndexedInputHash = &differs
			case "missing":
				state.IndexedInputHash = nil
			default:
				t.Fatalf("unknown inputProof %q", fixture.InputProof)
			}
			if fixture.LatestAttempt {
				state.LatestAttemptArtifactHash = &latest
			}
			if fixture.ProducerBehind {
				state.IndexerVersion--
			}
			if fixture.IndexFormatMissing {
				state.IndexVersion = nil
			}
			if fixture.ContentIncomplete {
				state.ContentStatus = ContentCaptureIncomplete
			}
			var metricsStore MetricsStore = indexBindRuleStore{}
			if fixture.NoBinder {
				metricsStore = indexBindRuleNoBinderStore{}
			}
			pipeline := &Pipeline{
				metricsStore:     metricsStore,
				resolvedVersions: targets,
				indexers:         map[Harness]TranscriptIndexer{HarnessClaudeCode: &ClaudeIndexer{}},
				config:           PipelineConfig{Force: fixture.Force, Reindex: fixture.Reindex},
			}
			input := &CapturedIndexInput{
				session:   DiscoveredSession{SessionID: sid, Harness: HarnessClaudeCode, SourceFormat: SourceFormatJSONL},
				kind:      TranscriptSourceFile,
				published: fixture.Published,
				inputHash: captured,
				expected:  state,
			}
			if fixture.DirectoryInput {
				input.kind = TranscriptSourceDirectory
			}
			if got := pipeline.capturedInputBindsWithoutReparse(input); got != fixture.Want {
				t.Fatalf("capturedInputBindsWithoutReparse = %t, want %t", got, fixture.Want)
			}
		})
	}
}
