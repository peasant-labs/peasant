package ingest

import "testing"

// TestCoverageClampsUnsupportedEnum pins the fail-closed clamp for enum
// values no constructor can produce: an out-of-range coverage reads as
// Unknown and refuses conversion, and an out-of-range read policy refuses
// conversion even on an otherwise full assessment. These states are only
// constructible white-box (all constructors emit valid enums), so the pin
// lives in-package rather than in the YAML fixture matrix.
func TestCoverageClampsUnsupportedEnum(t *testing.T) {
	t.Parallel()
	outOfRange := CaptureAssessment{coverage: CaptureCoverage(99), policy: CaptureFreshCandidate}
	if got := outOfRange.Coverage(); got != CaptureCoverageUnknown {
		t.Fatalf("coverage(99) = %d, want unknown", uint8(got))
	}
	if _, err := outOfRange.ContentCapture(ContentSourceNewIngest, TranscriptOriginFile, 1); err == nil {
		t.Fatal("unsupported coverage converted, want refusal")
	}
	badPolicy := CaptureAssessment{coverage: CaptureCoverageFull, policy: CaptureReadPolicy(9)}
	if _, err := badPolicy.ContentCapture(ContentSourceNewIngest, TranscriptOriginFile, 1); err == nil {
		t.Fatal("unsupported read policy converted, want refusal")
	}
}
