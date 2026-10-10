package store

import (
	"errors"
	"testing"
)

// TestReportContentSweepSeamNilHook proves a production sweep (no hook set)
// reports success at every stage without an observer.
func TestReportContentSweepSeamNilHook(t *testing.T) {
	contentSweepSeam = nil
	for _, stage := range []string{contentSweepSeamAfterCommit, contentSweepSeamMidBatch} {
		if err := reportContentSweepSeam(stage); err != nil {
			t.Fatalf("reportContentSweepSeam(%q) with nil hook = %v, want nil", stage, err)
		}
	}
}

// TestReportContentSweepSeamReports proves a crash-recovery test observes the
// stage the sweep reports, and that the hook's error propagates.
func TestReportContentSweepSeamReports(t *testing.T) {
	contentSweepSeam = nil
	defer func() { contentSweepSeam = nil }()

	var got []string
	boom := errors.New("boom")
	contentSweepSeam = func(stage string) error {
		got = append(got, stage)
		return boom
	}
	if err := reportContentSweepSeam(contentSweepSeamMidBatch); !errors.Is(err, boom) {
		t.Fatalf("reportContentSweepSeam = %v, want the hook error", err)
	}
	if len(got) != 1 || got[0] != contentSweepSeamMidBatch {
		t.Fatalf("hook observed %v, want [%q]", got, contentSweepSeamMidBatch)
	}
}
