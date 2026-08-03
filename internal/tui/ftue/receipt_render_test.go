package ftue

import (
	"bytes"
	_ "embed"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/journey_contract.yaml
var receiptRenderYAML []byte

type receiptRenderDocument struct {
	DeclaredRows int      `yaml:"declaredRows"`
	RequiredArms []string `yaml:"requiredArms"`
	Cases        []struct {
		ID                  string            `yaml:"id"`
		Arm                 string            `yaml:"arm"`
		Destination         Destination       `yaml:"destination"`
		RequestedVisibility string            `yaml:"requestedVisibility"`
		Effects             []PersistedEffect `yaml:"effects"`
		Retry               []RetryTarget     `yaml:"retry"`
		Valid               bool              `yaml:"valid"`
		OperationError      string            `yaml:"operationError"`
	} `yaml:"cases"`
}

func TestCompletionReceiptIsSafeCompleteAndCountBounded(t *testing.T) {
	decoder := yaml.NewDecoder(bytes.NewReader(receiptRenderYAML))
	decoder.KnownFields(true)
	var doc receiptRenderDocument
	if err := decoder.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("receipt render fixture accepted another YAML document")
	}
	var receipt PersistedEffect
	for _, row := range doc.Cases {
		if row.Arm == "private" {
			receipt = row.Effects[0]
		}
	}
	if receipt.Receipt == nil {
		t.Fatal("fixture does not carry a complete receipt")
	}
	effects := []PersistedEffect{receipt}
	for i := 0; i < 6; i++ {
		effects = append(effects, PersistedEffect{Stage: StagePublication, Status: StatusPersisted, SessionID: string(rune('a' + i))})
	}
	wizard := WizardModel{journeyResult: &JourneyResult{Effects: effects}}
	view := wizard.renderJourneyResult()
	for _, required := range []string{"village_origin=", "owner_user_id=", "project_hash=", "remote_transcript_id=", "url=", "visibility=", "content_hash=", "operation_fingerprint=", "applied.license=", "applied.associations=", "applied.normalized.", "and 2 more"} {
		if !strings.Contains(view, required) {
			t.Fatalf("completion receipt omitted %q: %s", required, view)
		}
	}
	if strings.Contains(view, "blobKey") || strings.Contains(view, "transcripts/x") {
		t.Fatalf("completion receipt leaked storage details: %s", view)
	}
}
