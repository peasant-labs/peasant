package ingest

import (
	"context"
	_ "embed"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/refusal_churn.yaml
var refusalChurnInternalYAML []byte

type refusalChurnSettledFixture struct {
	Required []string `yaml:"settled_required_names"`
	Cases    []struct {
		Name                 string `yaml:"name"`
		FailureCode          string `yaml:"failure_code"`
		StoredIndexerVersion int    `yaml:"stored_indexer_version"`
		TargetIndexerVersion int    `yaml:"target_indexer_version"`
		InputProof           bool   `yaml:"input_proof"`
		Expect               string `yaml:"expect"`
	} `yaml:"settled_cases"`
}

// TestSettledRefusalIsNotChangeEvidence states the discovery hint's decision
// over the stored refusal fields and the harness target directly: a refusal
// this build cannot lift is not change evidence, and every term that lifts the
// steady state (a newer target, a missing input proof, a code nothing refused)
// still sends the session to the worker. The whole-harvest fixture proves the
// caller asks this; this proves each term.
func TestSettledRefusalIsNotChangeEvidence(t *testing.T) {
	var fixture refusalChurnSettledFixture
	if err := yaml.Unmarshal(refusalChurnInternalYAML, &fixture); err != nil {
		t.Fatalf("decode refusal churn fixture: %v", err)
	}
	if len(fixture.Required) == 0 {
		t.Fatal("refusal churn fixture declares no required settled cases")
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		present[testCase.Name] = true
	}
	for _, name := range fixture.Required {
		if !present[name] {
			t.Fatalf("refusal churn fixture is missing required settled case %q", name)
		}
	}

	sid := SessionID("11111111-1111-4111-8111-111111111111")
	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			ingested := int64(1_700_000_000_000)
			session := DiscoveredSession{
				SessionID:    sid,
				Harness:      HarnessClaudeCode,
				SourceFormat: SourceFormatJSONL,
				ModTime:      time.UnixMilli(ingested - 60_000),
			}
			var proof *string
			if testCase.InputProof {
				hash := strings.Repeat("a", 64)
				proof = &hash
			}
			location := SessionLocation{
				IngestedMs:              &ingested,
				SchemaVersion:           int(CurrentSchemaVersion),
				SourceEvidenceSupported: true,
				SourceFingerprint:       []byte("captured-source-fingerprint"),
				PublicationReadiness:    PublicationNeedsIngest,
				ContentFailureCode:      ContentCaptureFailureCode(testCase.FailureCode),
				IndexerVersion:          testCase.StoredIndexerVersion,
				IndexedInputHash:        proof,
			}
			pipeline := &Pipeline{
				fs:     &OSFileSystem{},
				config: PipelineConfig{StalenessThreshold: 0, OutputDir: ResolvedPath(t.TempDir())},
				locationCache: map[SessionID]SessionLocation{
					sid: location,
				},
				harvesterVersions: map[Harness]HarvesterVersions{
					HarnessClaudeCode: {AdapterVersion: 1, IndexerVersion: testCase.TargetIndexerVersion, IndexVersion: 1},
				},
			}
			got, err := pipeline.classifySession(context.Background(), session)
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != testCase.Expect {
				t.Fatalf("classify %q = %q, want %q; a settled refusal must not be change evidence and every lifting term must be", testCase.Name, got.String(), testCase.Expect)
			}
		})
	}
}
