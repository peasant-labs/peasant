package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/refusal_churn.yaml
var refusalChurnInternalYAML []byte

type refusalChurnSettledCase struct {
	Name                 string `yaml:"name"`
	FailureCode          string `yaml:"failure_code"`
	StoredIndexerVersion int    `yaml:"stored_indexer_version"`
	TargetIndexerVersion int    `yaml:"target_indexer_version"`
	InputProof           bool   `yaml:"input_proof"`
	Expect               string `yaml:"expect"`
}

// refusalChurnSettledFixture is the predicate-matrix view of the shared fixture
// document. The five whole-harvest churn cases in the same document are loaded
// by the external test beside this one, so that section is captured raw here:
// the shared document passes the known-field check, and each section's fields
// are validated by the loader that actually reads them.
type refusalChurnSettledFixture struct {
	Required            []string                  `yaml:"settled_required_names"`
	SettledCases        []refusalChurnSettledCase `yaml:"settled_cases"`
	RequiredNames       []string                  `yaml:"required_names"`
	SessionID           string                    `yaml:"session_id"`
	Transcript          string                    `yaml:"transcript"`
	UnrepresentedRecord string                    `yaml:"unrepresented_record"`
	ChurnCases          yaml.Node                 `yaml:"cases"`
}

func loadRefusalChurnSettledFixture(t *testing.T) refusalChurnSettledFixture {
	t.Helper()
	var fixture refusalChurnSettledFixture
	decoder := yaml.NewDecoder(bytes.NewReader(refusalChurnInternalYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode the refusal churn fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("the refusal churn fixture must hold exactly one YAML document: %v", err)
	}
	if len(fixture.Required) == 0 {
		t.Fatal("refusal churn fixture declares no required settled cases")
	}
	present := make(map[string]bool, len(fixture.SettledCases))
	for _, testCase := range fixture.SettledCases {
		if testCase.Name == "" || present[testCase.Name] {
			t.Fatalf("the refusal churn fixture has an empty or repeated settled case name %q", testCase.Name)
		}
		present[testCase.Name] = true
	}
	for _, name := range fixture.Required {
		if !present[name] {
			t.Fatalf("refusal churn fixture is missing required settled case %q", name)
		}
	}
	return fixture
}

// TestSettledRefusalIsNotChangeEvidence states the discovery hint's decision
// over the stored refusal fields and the harness target directly: a refusal
// this build cannot lift is not change evidence, and every term that lifts the
// steady state (a newer target, a missing input proof, a code nothing refused)
// still sends the session to the worker. The whole-harvest fixture proves the
// caller asks this; this proves each term.
func TestSettledRefusalIsNotChangeEvidence(t *testing.T) {
	fixture := loadRefusalChurnSettledFixture(t)

	sid := SessionID("11111111-1111-4111-8111-111111111111")
	for _, testCase := range fixture.SettledCases {
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
