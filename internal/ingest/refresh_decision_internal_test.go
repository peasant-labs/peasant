package ingest

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/salt"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/refresh_decision_adapter_matrix.yaml
var internalRefreshDecisionAdapterMatrixYAML []byte

//go:embed testdata/refresh_decision_adapter_matrix.manifest.yaml
var internalRefreshDecisionAdapterMatrixManifestYAML []byte

type laneProfileStubAdapter struct {
	SourceAdapter
}

type laneProfileExtractAdapter struct {
	SourceAdapter
}

func (laneProfileExtractAdapter) ExtractMetadataFromTranscript(context.Context, []byte, *UnifiedMetadata) (*UnifiedMetadata, error) {
	return nil, nil
}

type laneProfileStubIndexer struct {
	TranscriptIndexer
}

type laneProfileHalfIndexer struct {
	TranscriptIndexer
}

func (laneProfileHalfIndexer) BuildGenerationMetadata(context.Context, GenerationBuildInput) (GenerationMetadataCandidate, error) {
	return GenerationMetadataCandidate{}, nil
}

type laneProfileProver struct {
	MetricsStore
	ManagedGenerationUnchangedProver
}

func TestManagedGenerationRefreshLaneOverAllRefreshReasons(t *testing.T) {
	var fixture struct {
		DecideRefreshCases []struct {
			Name                  string        `yaml:"name"`
			AdapterCanExtract     bool          `yaml:"adapterCanExtract"`
			AdapterUndeclared     bool          `yaml:"adapterUndeclared"`
			AdapterStampPresent   bool          `yaml:"adapterStampPresent"`
			WantReason            RefreshReason `yaml:"wantReason"`
			WantLane              RefreshLane   `yaml:"wantLane"`
			WantAdapterUndeclared bool          `yaml:"wantAdapterUndeclared"`
		} `yaml:"decideRefreshCases"`
		LaneProfiles []struct {
			Name                   string          `yaml:"name"`
			AdapterCanExtract      bool            `yaml:"adapterCanExtract"`
			IndexerHasMetadataHalf bool            `yaml:"indexerHasMetadataHalf"`
			AcceptedReasons        []RefreshReason `yaml:"acceptedReasons"`
		} `yaml:"laneProfiles"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(internalRefreshDecisionAdapterMatrixYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode refresh_decision_adapter_matrix.yaml: %v", err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	manifestDecoder := yaml.NewDecoder(strings.NewReader(string(internalRefreshDecisionAdapterMatrixManifestYAML)))
	manifestDecoder.KnownFields(true)
	if err := manifestDecoder.Decode(&manifest); err != nil {
		t.Fatalf("decode refresh_decision_adapter_matrix.manifest.yaml: %v", err)
	}
	required := make(map[string]bool, len(manifest.RequiredNames))
	for _, name := range manifest.RequiredNames {
		if name == "" || required[name] {
			t.Fatalf("empty or duplicate required name %q", name)
		}
		required[name] = true
	}
	actual := make(map[string]bool)
	for _, c := range fixture.DecideRefreshCases {
		actual[c.Name] = true
	}
	for _, p := range fixture.LaneProfiles {
		actual[p.Name] = true
	}
	for name := range required {
		if !actual[name] {
			t.Fatalf("required name %q missing from fixture", name)
		}
	}
	for name := range actual {
		if !required[name] {
			t.Fatalf("fixture name %q missing from manifest", name)
		}
	}
	if len(fixture.LaneProfiles) != 4 {
		t.Fatalf("laneProfiles count = %d, want 4", len(fixture.LaneProfiles))
	}

	session := DiscoveredSession{
		Harness:          HarnessOpenCode,
		TranscriptOrigin: TranscriptOriginOpenCodeCurrentSQLite,
	}

	for _, profile := range fixture.LaneProfiles {
		t.Run(profile.Name, func(t *testing.T) {
			accepted := make(map[RefreshReason]bool, len(profile.AcceptedReasons))
			for _, reason := range profile.AcceptedReasons {
				accepted[reason] = true
			}

			var adapter SourceAdapter = laneProfileStubAdapter{}
			if profile.AdapterCanExtract {
				adapter = laneProfileExtractAdapter{}
			}
			var indexer TranscriptIndexer = laneProfileStubIndexer{}
			if profile.IndexerHasMetadataHalf {
				indexer = laneProfileHalfIndexer{}
			}

			p := &Pipeline{
				adapters: map[Harness]AdapterFactory{
					HarnessOpenCode: func(FileSystem, GitResolver, salt.Salt) SourceAdapter { return adapter },
				},
				indexers: map[Harness]TranscriptIndexer{
					HarnessOpenCode: indexer,
				},
				metricsStore: laneProfileProver{},
				resolvedVersions: map[Harness]HarvesterVersions{
					HarnessOpenCode: {AdapterVersion: 5, IndexerVersion: 2, IndexVersion: 2},
				},
			}

			for _, reason := range AllRefreshReasons {
				want := accepted[reason]
				decision := RefreshDecision{Reason: reason}

				if got := p.isManagedGenerationAdapterRefresh(session, decision); got != want {
					t.Errorf("isManagedGenerationAdapterRefresh(%q) = %v, want %v", reason, got, want)
				}
				if got := p.managedGenerationRefreshLane(session, decision); got != want {
					t.Errorf("managedGenerationRefreshLane(%q) = %v, want %v", reason, got, want)
				}

				undeclaredDecision := RefreshDecision{Reason: reason, AdapterUndeclared: true}
				if got := p.isManagedGenerationAdapterRefresh(session, undeclaredDecision); got {
					t.Errorf("isManagedGenerationAdapterRefresh(%q, AdapterUndeclared=true) = true, want false", reason)
				}
				if got := p.managedGenerationRefreshLane(session, undeclaredDecision); got {
					t.Errorf("managedGenerationRefreshLane(%q, AdapterUndeclared=true) = true, want false", reason)
				}
			}

			p.config.DisableRefreshSkips = true
			for _, reason := range AllRefreshReasons {
				want := accepted[reason]
				decision := RefreshDecision{Reason: reason}
				if got := p.isManagedGenerationAdapterRefresh(session, decision); got != want {
					t.Errorf("DisableRefreshSkips=true: isManagedGenerationAdapterRefresh(%q) = %v, want %v", reason, got, want)
				}
				if got := p.managedGenerationRefreshLane(session, decision); got {
					t.Errorf("DisableRefreshSkips=true: managedGenerationRefreshLane(%q) = true, want false", reason)
				}
			}
		})
	}
}

func TestSessionVersionTargetFallsBackForInapplicableOriginUnderOverride(t *testing.T) {
	p := &Pipeline{
		resolvedVersions: map[Harness]HarvesterVersions{
			HarnessOpenCode: {AdapterVersion: 5, IndexerVersion: 2, IndexVersion: 2},
		},
	}
	baseline := HarvesterVersionRegistry[HarnessOpenCode]
	if baseline.IndexVersion != strictIndexFormat {
		t.Fatalf("expected OpenCode baseline IndexVersion=%d, got %d", strictIndexFormat, baseline.IndexVersion)
	}

	current := p.sessionVersionTarget(DiscoveredSession{
		Harness:          HarnessOpenCode,
		TranscriptOrigin: TranscriptOriginOpenCodeCurrentSQLite,
	})
	if current.IndexVersion != 2 {
		t.Fatalf("current-sqlite target IndexVersion = %d, want 2", current.IndexVersion)
	}

	for _, origin := range []TranscriptOrigin{
		TranscriptOriginOpenCodeLegacySQLite,
		TranscriptOriginFile,
	} {
		got := p.sessionVersionTarget(DiscoveredSession{
			Harness:          HarnessOpenCode,
			TranscriptOrigin: origin,
		})
		if got != baseline {
			t.Fatalf("origin %q: sessionVersionTarget = %+v, want baseline %+v", origin, got, baseline)
		}
	}
}
