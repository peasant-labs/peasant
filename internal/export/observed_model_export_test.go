package export_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/observed_model_export.yaml
var observedModelExportFixtureYAML []byte

type observedModelExportTurn struct {
	Name          string `yaml:"name"`
	Index         int    `yaml:"index"`
	Role          string `yaml:"role"`
	Depth         int    `yaml:"depth"`
	Content       string `yaml:"content"`
	ObservedModel string `yaml:"observedModel"`
}

type observedModelExportFixture struct {
	SessionID              string                    `yaml:"sessionId"`
	ExpectedSeed           string                    `yaml:"expectedSeed"`
	Turns                  []observedModelExportTurn `yaml:"turns"`
	ExpectedObservedModels []string                  `yaml:"expectedObservedModels"`
	RequiredNames          []string                  `yaml:"requiredNames"`
}

func TestExportSessionEmitsObservedModelEvidence(t *testing.T) {
	t.Parallel()
	var fixture observedModelExportFixture
	if err := yaml.Unmarshal(observedModelExportFixtureYAML, &fixture); err != nil {
		t.Fatalf("decode export fixture: %v", err)
	}
	if fixture.SessionID == "" || len(fixture.ExpectedObservedModels) != len(fixture.Turns) {
		t.Fatalf("export fixture inventory is incomplete: %+v", fixture)
	}
	seen := map[string]bool{}
	for _, turn := range fixture.Turns {
		if turn.Name == "" || seen[turn.Name] {
			t.Fatalf("export fixture has empty or duplicate name %q", turn.Name)
		}
		seen[turn.Name] = true
	}
	for _, required := range fixture.RequiredNames {
		if !seen[required] {
			t.Fatalf("export fixture is missing required name %q", required)
		}
	}
	store := storetest.Open(t)
	storetest.SeedSession(t, store, fixture.SessionID)
	var native strings.Builder
	for _, source := range fixture.Turns {
		line, err := json.Marshal(map[string]any{"type": source.Role, "message": map[string]string{"role": source.Role, "content": source.Content, "model": source.ObservedModel}})
		if err != nil {
			t.Fatal(err)
		}
		native.Write(line)
		native.WriteByte('\n')
	}
	fs := testutil.NewMemFS()
	seedEntriesFromJSONL(t, context.Background(), store, fs, fixture.SessionID, []byte(native.String()))
	payload, err := export.ExportSession(context.Background(), store, fs, fixture.SessionID, "/managed")
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if payload.Model != fixture.ExpectedSeed || len(payload.Turns) != len(fixture.ExpectedObservedModels) {
		t.Fatalf("exported payload mismatch: %+v", payload)
	}
	for index, expected := range fixture.ExpectedObservedModels {
		if got := payload.Turns[index].ObservedModel.String(); got != expected {
			t.Fatalf("turn %d observedModel=%q, want %q", index, got, expected)
		}
	}
}
