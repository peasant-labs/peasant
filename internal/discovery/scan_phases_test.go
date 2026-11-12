package discovery_test

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/scan_phases.yaml
var scanPhasesData []byte

type scanPhasesCase struct {
	Name        string `yaml:"name"`
	Harness     string `yaml:"harness"`
	Discovering string `yaml:"discovering"`
	Resolving   string `yaml:"resolving"`
}

type scanPhasesDocument struct {
	RequiredNames []string         `yaml:"requiredNames"`
	Cases         []scanPhasesCase `yaml:"cases"`
}

// TestScanPhases runs the scan-phase fixture: the lines a running scan reports,
// which the terminal spinner and the setup page both show.
func TestScanPhases(t *testing.T) {
	t.Parallel()
	var document scanPhasesDocument
	if err := testutil.DecodeFixtureYAML(scanPhasesData, &document); err != nil {
		t.Fatalf("decode scan_phases.yaml: %v", err)
	}
	present := make(map[string]bool, len(document.Cases))
	for _, row := range document.Cases {
		present[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("internal/discovery/testdata/scan_phases.yaml", "case", document.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			harness := defaults.Harness(row.Harness)
			if got := discovery.DiscoveringPhase(harness); got != row.Discovering {
				t.Errorf("DiscoveringPhase(%s) = %q, want %q", harness, got, row.Discovering)
			}
			if got := discovery.ResolvingPhase(harness); got != row.Resolving {
				t.Errorf("ResolvingPhase(%s) = %q, want %q", harness, got, row.Resolving)
			}
		})
	}
}
