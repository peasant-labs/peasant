package export_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
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
	ExpectedCaseCount      int                       `yaml:"expectedCaseCount"`
	RequiredNames          []string                  `yaml:"requiredNames"`
}

func TestExportSessionEmitsObservedModelEvidence(t *testing.T) {
	t.Parallel()
	var fixture observedModelExportFixture
	if err := yaml.Unmarshal(observedModelExportFixtureYAML, &fixture); err != nil {
		t.Fatalf("decode export fixture: %v", err)
	}
	if fixture.SessionID == "" || fixture.ExpectedCaseCount != 2 || len(fixture.Turns) != fixture.ExpectedCaseCount || len(fixture.RequiredNames) != fixture.ExpectedCaseCount || len(fixture.ExpectedObservedModels) != len(fixture.Turns) {
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
	entries := make([]schema.SessionEntry, len(fixture.Turns))
	for index, source := range fixture.Turns {
		extra, _ := json.Marshal(map[string]string{"model_id": source.ObservedModel})
		extraString := string(extra)
		entries[index] = schema.SessionEntry{SessionID: schema.SessionID(fixture.SessionID), EntryIndex: source.Index, Harness: ingest.HarnessClaudeCode, Role: schema.Role(source.Role), EntryType: schema.EntryTypeText, Depth: source.Depth, ContentPreview: &source.Content, Extra: &extraString}
	}
	if err := store.IndexSessionEntries(context.Background(), schema.SessionID(fixture.SessionID), entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}
	payload, err := export.ExportSession(context.Background(), store, testutil.NewMemFS(), fixture.SessionID)
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
