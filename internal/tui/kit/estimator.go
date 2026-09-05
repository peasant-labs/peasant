package kit

import (
	"time"
)

// Estimator tracks completion samples and reports remaining time from the
// recent windowed rate. It is domain-free: callers own eligibility (stable
// totals, error and retry policy, focus) and feed it observations. A fast
// start ages out of the window instead of anchoring the average forever,
// and one bursty batch cannot collapse the display for a frame. Stages
// younger than the window use their full history.
type Estimator struct {
	window  time.Duration
	samples []sample
}

// sample is one observed completion count at one time, oldest-first.
type sample struct {
	at   time.Time
	done int
}

// NewEstimator builds an estimator over the given lookback window, which
// must be positive.
func NewEstimator(window time.Duration) Estimator {
	return Estimator{window: window}
}

// Estimate records done completed at time at, then reports the remaining
// time for total at the windowed completions rate. Samples must arrive in
// non-decreasing time order. It reports false with fewer than two spanning
// samples, no windowed progress, or nothing remaining.
func (e *Estimator) Estimate(at time.Time, done, total int) (time.Duration, bool) {
	e.samples = append(e.samples, sample{at: at, done: done})
	cutoff := at.Add(-e.window)
	keep := 0
	for keep < len(e.samples) && e.samples[keep].at.Before(cutoff) {
		keep++
	}
	e.samples = e.samples[keep:]
	if total <= done || len(e.samples) < 2 {
		return 0, false
	}
	first, last := e.samples[0], e.samples[len(e.samples)-1]
	span := last.at.Sub(first.at)
	delta := last.done - first.done
	if span <= 0 || delta <= 0 {
		return 0, false
	}
	rate := float64(delta) / span.Seconds()
	return time.Duration(float64(total-done) / rate * float64(time.Second)), true
}
