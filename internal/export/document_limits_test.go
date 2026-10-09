package export_test

import (
	_ "embed"
	"encoding/json"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/document_limits.yaml
var documentLimitsYAML []byte

//go:embed testdata/document_limits.manifest.yaml
var documentLimitsManifestYAML []byte

type documentLimitCase struct {
	Name          string `yaml:"name"`
	DocumentBytes int    `yaml:"documentBytes"`
	Accepted      bool   `yaml:"accepted"`
	ErrorContains string `yaml:"errorContains,omitempty"`
}

func loadDocumentLimitFixtures(t *testing.T) []documentLimitCase {
	t.Helper()
	var fixture struct {
		Cases []documentLimitCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(documentLimitsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(documentLimitsManifestYAML, "document limits")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for i, c := range fixture.Cases {
		names[i] = c.Name
		if c.DocumentBytes <= 0 || (!c.Accepted && c.ErrorContains == "") {
			t.Fatalf("document limit fixture %q must name a positive byte size and a refusal message when rejected", c.Name)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "document limits"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

// Exercise the actual export validator, including its indented emitted bytes.
// Keep large cases serial to bound peak memory; no files or services are needed.
func TestExportDocumentLimits(t *testing.T) {
	for _, c := range loadDocumentLimitFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			payload := &schema.SessionDetailPayload{
				ID: testutil.TestSessionUUID, Harness: schema.HarnessClaudeCode,
				Turns: []schema.TurnDetail{{Index: 0, Role: schema.RoleAssistant, Depth: 0, Content: "x"}},
			}
			base, err := json.MarshalIndent(payload, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			payload.Turns[0].Content = strings.Repeat("x", c.DocumentBytes-len(base)+1)
			encoded, err := json.MarshalIndent(payload, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if len(encoded) != c.DocumentBytes {
				t.Fatalf("fixture produced %d bytes, want %d", len(encoded), c.DocumentBytes)
			}
			encoded = nil
			err = export.ValidateExportPayload(payload)
			assertDocumentLimitOutcome(t, c, err)
			if !c.Accepted {
				if !strings.HasPrefix(err.Error(), "export session: final serialized detail exceeds the public transfer limit:") ||
					!strings.Contains(err.Error(), "local evidence is unchanged and nothing exported; use a receiver and contract supporting larger transfers when available") {
					t.Fatalf("export refusal lost its actionable message: %v", err)
				}
			}
		})
	}
}

// Whitespace is part of the raw document size. Padding a small valid envelope
// tests the schema-owned transcript cap without changing any nested policy.
func TestTranscriptDocumentLimits(t *testing.T) {
	for _, c := range loadDocumentLimitFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			envelope, err := json.Marshal(schema.TranscriptContent{
				Kind: schema.ContentKindSessionDetail, ContractVersion: "1.0.0",
				SessionDetail: &schema.SessionDetailPayload{
					ID: testutil.TestSessionUUID, Harness: schema.HarnessClaudeCode,
					Turns: []schema.TurnDetail{},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			raw := make([]byte, c.DocumentBytes)
			copy(raw, envelope)
			for i := len(envelope); i < len(raw); i++ {
				raw[i] = ' '
			}
			_, err = schema.DecodeTranscriptContentRaw(raw)
			assertDocumentLimitOutcome(t, c, err)
		})
	}
}

func assertDocumentLimitOutcome(t *testing.T, c documentLimitCase, err error) {
	t.Helper()
	if c.Accepted {
		if err != nil {
			t.Fatalf("%d-byte document refused: %v", c.DocumentBytes, err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), c.ErrorContains) {
		t.Fatalf("%d-byte document refusal = %v, want %q", c.DocumentBytes, err, c.ErrorContains)
	}
}
