package ingest

import (
	"testing"
)

// TestActionableDiagnosticRegistersDiscoveryStages proves the discovery and
// freshness stages map to the discovery-failed code through the shared
// constructor, so recordCandidateFailure no longer hand-builds a diagnostic
// that could drift from the registered mapping.
func TestActionableDiagnosticRegistersDiscoveryStages(t *testing.T) {
	for _, stage := range []OpenCodeProbeStage{OpenCodeProbeDiscover, OpenCodeProbeFreshness} {
		diagnostic := actionableOpenCodeDiagnostic(stage, "what", "why", "/synthetic/opencode.db", "when", "meaning", "fix")
		if diagnostic.Code != OpenCodeDiagnosticDiscoveryFailed {
			t.Fatalf("stage %q mapped to code %q, want %q", stage, diagnostic.Code, OpenCodeDiagnosticDiscoveryFailed)
		}
		if diagnostic.Stage != stage {
			t.Fatalf("stage %q was not preserved on the diagnostic, got %q", stage, diagnostic.Stage)
		}
	}
}
