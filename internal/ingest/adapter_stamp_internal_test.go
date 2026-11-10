package ingest

import (
	"context"
	_ "embed"
	"testing"
	"time"

	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/adapter_stamp_cases.yaml
var adapterStampCasesYAML []byte

type adapterStampCase struct {
	Name                 string `yaml:"name"`
	StoredRevisionOffset *int   `yaml:"storedRevisionOffset"`
	Want                 string `yaml:"want"`
}

type adapterStampFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	TargetSets    []struct {
		Name              string `yaml:"name"`
		GenerationOverlay bool   `yaml:"generationOverlay"`
	} `yaml:"targetSets"`
	Cases []adapterStampCase `yaml:"cases"`
}

func loadAdapterStampFixture(t *testing.T) adapterStampFixture {
	t.Helper()
	var fixture adapterStampFixture
	if err := yaml.Unmarshal(adapterStampCasesYAML, &fixture); err != nil {
		t.Fatalf("load adapter stamp cases: %v", err)
	}
	seen := make(map[string]bool, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" || seen[testCase.Name] {
			t.Fatalf("invalid adapter stamp case %q", testCase.Name)
		}
		seen[testCase.Name] = true
		switch testCase.Want {
		case "updated", "unchanged":
		default:
			t.Fatalf("adapter stamp case %q has unknown want %q", testCase.Name, testCase.Want)
		}
	}
	if len(fixture.RequiredNames) == 0 {
		t.Fatal("adapter stamp cases declare no required names; list every case the fixture must retain")
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing adapter stamp case %q", name)
		}
	}
	overlays := 0
	seenSets := make(map[string]bool, len(fixture.TargetSets))
	for _, set := range fixture.TargetSets {
		if set.Name == "" || seenSets[set.Name] {
			t.Fatalf("invalid adapter stamp target set %q", set.Name)
		}
		seenSets[set.Name] = true
		if set.GenerationOverlay {
			overlays++
		}
	}
	if len(fixture.TargetSets) < 2 || overlays == 0 || overlays == len(fixture.TargetSets) {
		t.Fatal("adapter stamp cases must cover the managed-generation overlay and the baseline registry")
	}
	return fixture
}

// TestCapturedSourceSettlesOnlyWhenTheAdapterStampIsCurrent protects: identical
// bytes republish once while the stored adapter revision is behind this build's
// target, a NULL stamp counts as baseline 1, and the row settles once the
// revision matches. Both target sets are covered because a generation-capable
// store and a store-free harvest resolve different OpenCode revisions.
//
// This pins the captured classifier only. The harvest test proves the status
// reaches the write that stamps the revision.
func TestCapturedSourceSettlesOnlyWhenTheAdapterStampIsCurrent(t *testing.T) {
	fixture := loadAdapterStampFixture(t)
	if GenerationTargets(HarvesterVersionRegistry)[HarnessOpenCode].AdapterVersion ==
		HarvesterVersionRegistry[HarnessOpenCode].AdapterVersion {
		t.Fatal("the managed-generation and baseline targets are equal; the two target sets would test the same threshold")
	}
	sessionID, err := NewSessionID("ses_3cd91f52effeXd3QAJ54jStmp")
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := []byte("captured-source-fingerprint")
	ingested := time.Now().Add(-time.Hour).UnixMilli()
	wants := map[string]DiffStatus{"updated": DiffUpdated, "unchanged": DiffUnchanged}

	for _, targetSet := range fixture.TargetSets {
		t.Run(targetSet.Name, func(t *testing.T) {
			targets := HarvesterVersionRegistry
			if targetSet.GenerationOverlay {
				targets = GenerationTargets(HarvesterVersionRegistry)
			}
			target := targets[HarnessOpenCode].AdapterVersion
			for _, testCase := range fixture.Cases {
				t.Run(testCase.Name, func(t *testing.T) {
					var storedRevision *int
					if testCase.StoredRevisionOffset != nil {
						stored := target + *testCase.StoredRevisionOffset
						storedRevision = &stored
					}
					outputDir, err := NewResolvedPath(t.TempDir())
					if err != nil {
						t.Fatal(err)
					}
					pipeline := &Pipeline{
						fs:            &OSFileSystem{},
						config:        PipelineConfig{OutputDir: outputDir},
						locationCache: make(map[SessionID]SessionLocation, 1),
					}
					pipeline.resolvedVersions = targets
					pipeline.locationCache[sessionID] = SessionLocation{
						IngestedMs:              &ingested,
						SchemaVersion:           CurrentSchemaVersion,
						AdapterVersion:          storedRevision,
						SourceEvidenceSupported: true,
						SourceFingerprint:       fingerprint,
						PublicationReadiness:    PublicationReady,
					}
					session := DiscoveredSession{
						SessionID:        sessionID,
						Harness:          HarnessOpenCode,
						TranscriptOrigin: TranscriptOriginOpenCodeCurrentSQLite,
						SourceFormat:     SourceFormatJSON,
					}
					captured := &MaterializedTranscript{
						Metadata:          &UnifiedMetadata{SessionID: schema.SessionID(string(sessionID))},
						Data:              []byte("{}\n"),
						SourceFingerprint: fingerprint,
					}

					status, err := pipeline.classifyCapturedSession(context.Background(), session, captured)
					if err != nil {
						t.Fatalf("classify captured source: %v", err)
					}
					if status != wants[testCase.Want] {
						t.Fatalf("status = %v, want %v", status, wants[testCase.Want])
					}
				})
			}
		})
	}
}
