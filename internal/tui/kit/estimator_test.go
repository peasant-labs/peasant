package kit_test

import (
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/tui/kit"
)

// TestEstimatorUsesFullHistoryWhenYoung proves stages younger than the window
// estimate from everything observed: 4 of 10 done in 2s leaves 3s.
func TestEstimatorUsesFullHistoryWhenYoung(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	estimator := kit.NewEstimator(5 * time.Second)
	if _, ok := estimator.Estimate(start, 0, 10); ok {
		t.Error("ETA with a single sample must be invalid")
	}
	eta, ok := estimator.Estimate(start.Add(2*time.Second), 4, 10)
	if !ok || eta != 3*time.Second {
		t.Errorf("ETA = %v, %t; want 3s, true", eta, ok)
	}
}

// TestEstimatorAgesOutFastStarts proves stale history leaves the window: the
// burst at second 3 dominates a lifetime average but the windowed rate over
// the last five seconds still reports the recent pace.
func TestEstimatorAgesOutFastStarts(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	estimator := kit.NewEstimator(5 * time.Second)
	estimator.Estimate(start, 0, 20)
	estimator.Estimate(start.Add(4*time.Second), 8, 20)
	eta, ok := estimator.Estimate(start.Add(9*time.Second), 10, 20)
	if !ok || eta != 25*time.Second {
		t.Errorf("ETA = %v, %t; want 25s, true", eta, ok)
	}
}

// TestEstimatorRefusesStallAndFinishedWork proves no estimate without
// observed forward progress over a positive span, or when nothing remains.
func TestEstimatorRefusesStallAndFinishedWork(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	estimator := kit.NewEstimator(5 * time.Second)
	if _, ok := estimator.Estimate(start, 0, 10); ok {
		t.Error("ETA with no history must be invalid")
	}
	estimator2 := kit.NewEstimator(5 * time.Second)
	estimator2.Estimate(start, 4, 10)
	estimator2.Estimate(start.Add(2*time.Second), 4, 10)
	if _, ok := estimator2.Estimate(start.Add(4*time.Second), 4, 10); ok {
		t.Error("ETA with stalled progress must be invalid")
	}
	estimator3 := kit.NewEstimator(5 * time.Second)
	estimator3.Estimate(start, 10, 10)
	if _, ok := estimator3.Estimate(start.Add(time.Second), 10, 10); ok {
		t.Error("ETA with nothing remaining must be invalid")
	}
}
