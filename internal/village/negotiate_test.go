package village_test

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// --- pure semver/window helpers ---
//
// compareSemver is package-internal (no external consumer); its dedicated test
// lives in-package in compareSemver_internal_test.go. The exported window helpers
// (ClassifyContract/CanDowngrade) are tested here, and exercise compareSemver
// transitively.

func TestClassifyContract_Matrix(t *testing.T) {
	cases := []struct {
		name           string
		cli, min, curr schema.PushContractVersion
		want           village.ContractNegotiation
	}{
		{"within", "1.0.0", "0.5.0", "1.0.0", village.NegotiationWithin},
		{"older", "0.1.0", "0.2.0", "0.5.0", village.NegotiationOlderThanMin},
		{"ahead", "2.0.0", "1.0.0", "1.5.0", village.NegotiationAheadOfCurrent},
		{"unadvertised", "1.0.0", "", "", village.NegotiationUnadvertised},
	}
	for _, c := range cases {
		if got := village.ClassifyContract(c.cli, c.min, c.curr); got != c.want {
			t.Errorf("%s: ClassifyContract(%q,[%q,%q]): got %s, want %s",
				c.name, c.cli, c.min, c.curr, got, c.want)
		}
	}
}

func TestCanDowngrade_MajorGapNotDowngradable(t *testing.T) {
	if village.CanDowngrade("2.0.0", "1.5.0") {
		t.Error("major-version gap must NOT be downgradable")
	}
	if !village.CanDowngrade("1.4.0", "1.2.0") {
		t.Error("same-major older target must be downgradable")
	}
}
