package testutil

import (
	"strings"
	"testing"
)

func TestRequireFixtureNames_Accepts(t *testing.T) {
	t.Parallel()
	present := map[string]bool{"alpha": true, "beta": true}
	if err := RequireFixtureNames("widget fixture", "widget", []string{"alpha", "beta"}, present); err != nil {
		t.Fatalf("RequireFixtureNames = %v, want nil for a fully satisfied manifest", err)
	}
}

func TestRequireFixtureNames_RejectsEmptyRequiredList(t *testing.T) {
	t.Parallel()
	err := RequireFixtureNames("widget fixture", "widget", nil, map[string]bool{"alpha": true})
	if err == nil {
		t.Fatal("RequireFixtureNames accepted an empty required-names list")
	}
	if !strings.Contains(err.Error(), "widget fixture declares no required widget names") {
		t.Fatalf("empty-required error = %v, want it to name the fixture and the axis", err)
	}
}

func TestRequireFixtureNames_RejectsBlankRequiredEntry(t *testing.T) {
	t.Parallel()
	err := RequireFixtureNames("widget fixture", "widget", []string{"alpha", "  "}, map[string]bool{"alpha": true})
	if err == nil {
		t.Fatal("RequireFixtureNames accepted a blank required name")
	}
	if !strings.Contains(err.Error(), "widget fixture required widget names has a blank entry") {
		t.Fatalf("blank-entry error = %v, want it to name the fixture and the axis", err)
	}
}

// TestRequireFixtureNames_RejectsDuplicateRequiredEntry mutation-proves the
// duplicate-detection branch actually fires: no shipped call site's own
// fixture currently repeats a required name, so this is the only place that
// branch is exercised. A helper whose second check never runs is half
// theater; this test is the proof it runs.
func TestRequireFixtureNames_RejectsDuplicateRequiredEntry(t *testing.T) {
	t.Parallel()
	err := RequireFixtureNames("widget fixture", "widget", []string{"alpha", "alpha"}, map[string]bool{"alpha": true})
	if err == nil {
		t.Fatal("RequireFixtureNames accepted a required-names list that repeats a name")
	}
	if !strings.Contains(err.Error(), `widget fixture required widget names repeats "alpha"`) {
		t.Fatalf("duplicate-required error = %v, want it to name the repeated entry", err)
	}
}

func TestRequireFixtureNames_RejectsMissingPresentEntry(t *testing.T) {
	t.Parallel()
	err := RequireFixtureNames("widget fixture", "widget", []string{"alpha", "beta"}, map[string]bool{"alpha": true})
	if err == nil {
		t.Fatal("RequireFixtureNames accepted a manifest whose named row is absent")
	}
	if !strings.Contains(err.Error(), `widget fixture is missing required widget "beta"`) {
		t.Fatalf("missing-present error = %v, want it to name the missing widget", err)
	}
}
