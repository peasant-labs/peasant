package api

import (
	_ "embed"
	"maps"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/sourcepreview"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/kickstart_preview_reasons.yaml
var previewReasonsYAML []byte

func TestPreviewReasonMappings(t *testing.T) {
	var fixture struct {
		Cut   []struct{ Source, Wire string } `yaml:"cut"`
		Close []struct{ Source, Wire string } `yaml:"close"`
	}
	if err := testutil.DecodeFixtureYAML(previewReasonsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	cuts := make(map[string]string)
	for _, row := range fixture.Cut {
		if _, exists := cuts[row.Source]; exists {
			t.Fatalf("duplicate cut source %q", row.Source)
		}
		cuts[row.Source] = row.Wire
	}
	actualCuts := make(map[string]bool)
	for _, reason := range sourcepreview.AllCutReasons() {
		mapped, ok := previewCutReason(reason)
		if !ok || cuts[reason.String()] != mapped.String() || !mapped.IsValid() {
			t.Fatalf("cut mapping for %s disagrees with fixture", reason)
		}
		actualCuts[mapped.String()] = true
		delete(cuts, reason.String())
	}
	expectedCuts := make(map[string]bool)
	for _, reason := range schema.AllLocalKickstartPreviewCutReasons {
		expectedCuts[reason.String()] = true
	}
	if len(cuts) != 0 || !slices.Equal(slices.Sorted(maps.Keys(actualCuts)), slices.Sorted(maps.Keys(expectedCuts))) {
		t.Fatal("cut fixture or wire membership differs")
	}
	closes := make(map[string]string)
	for _, row := range fixture.Close {
		if _, exists := closes[row.Source]; exists {
			t.Fatalf("duplicate close source %q", row.Source)
		}
		closes[row.Source] = row.Wire
	}
	for _, reason := range sourcepreview.AllCloseReasons() {
		mapped, ok := previewCloseReason(reason)
		if !ok || closes[reason.String()] != mapped.String() || !mapped.IsValid() {
			t.Fatalf("close mapping for %s disagrees with fixture", reason)
		}
		delete(closes, reason.String())
	}
	if len(closes) != 0 {
		t.Fatal("close fixture has unknown source reasons")
	}
	if _, ok := previewCutReason(sourcepreview.CutNone); ok {
		t.Fatal("an absent cut reason was accepted")
	}
	if _, ok := previewCloseReason(sourcepreview.CloseReason(255)); ok {
		t.Fatal("an unknown close reason was accepted")
	}
}
