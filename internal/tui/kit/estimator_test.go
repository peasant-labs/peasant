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
	estimator.Observe(start, 0)
	estimator.Observe(start.Add(2*time.Second), 4)
	eta, ok := estimator.ETA(10, 4)
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
	estimator.Observe(start, 0)
	estimator.Observe(start.Add(4*time.Second), 8)
	estimator.Observe(start.Add(9*time.Second), 10)
	eta, ok := estimator.ETA(20, 10)
	if !ok || eta != 25*time.Second {
		t.Errorf("ETA = %v, %t; want 25s, true", eta, ok)
	}
}

// TestEstimatorRefusesStallAndSingleSample proves no estimate without
// observed forward progress over a positive span.
func TestEstimatorRefusesStallAndSingleSample(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	estimator := kit.NewEstimator(5 * time.Second)
	if _, ok := estimator.ETA(10, 0); ok {
		t.Error("ETA with no samples must be invalid")
	}
	estimator.Observe(start, 4)
	if _, ok := estimator.ETA(10, 4); ok {
		t.Error("ETA with a single sample must be invalid")
	}
	estimator.Observe(start.Add(2*time.Second), 4)
	if _, ok := estimator.ETA(10, 4); ok {
		t.Error("ETA with stalled progress must be invalid")
	}
}
