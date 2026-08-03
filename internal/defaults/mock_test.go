package defaults

import (
	"slices"
	"testing"
)

func TestIsValidMockSection_MapAndReview(t *testing.T) {
	for _, s := range []MockSection{MockSections.Map, MockSections.Review} {
		if !IsValidMockSection(s) {
			t.Errorf("IsValidMockSection(%q) = false, want true", s)
		}
		if !slices.Contains(AllMockSections, s) {
			t.Errorf("AllMockSections does not contain %q", s)
		}
	}
}

func TestIsValidMockSection_AllListedAreValid(t *testing.T) {
	for _, s := range AllMockSections {
		if !IsValidMockSection(s) {
			t.Errorf("AllMockSections entry %q fails IsValidMockSection", s)
		}
	}
	if IsValidMockSection(MockSection("not-a-section")) {
		t.Error("IsValidMockSection accepted an unknown section")
	}
}
