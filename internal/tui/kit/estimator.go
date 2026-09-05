package kit

import (
	"time"
)

// Estimator tracks completion samples and reports a time-remaining estimate
// from the recent windowed rate. It is domain-free: callers own eligibility
// (stable totals, error and retry policy, focus) and only feed it observed
// (time, done) pairs. A fast start ages out of the window instead of
// anchoring the average forever, and one bursty batch cannot collapse the
// display for a frame. Stages younger than the window use their full history.
type Estimator struct {
	window  time.Duration
	samples []Sample
}

// Sample is one observed completion count at one time, oldest-first in use.
type Sample struct {
	At   time.Time
	Done int
}

// NewEstimator builds an estimator over the given lookback window, which
// must be positive.
func NewEstimator(window time.Duration) Estimator {
	return Estimator{window: window}
}

// Observe records one completion sample and drops samples older than the
// window. Samples must arrive in non-decreasing time order.
func (e *Estimator) Observe(at time.Time, done int) {
	e.samples = append(e.samples, Sample{At: at, Done: done})
	cutoff := at.Add(-e.window)
	keep := 0
	for keep < len(e.samples) && e.samples[keep].At.Before(cutoff) {
		keep++
	}
	e.samples = e.samples[keep:]
}

// ETA reports the remaining time for total at the windowed completions rate:
// the observed pace with stale history aged out, not the lifetime average.
// It reports false with fewer than two spanning samples or no windowed
// progress.
func (e Estimator) ETA(total, done int) (time.Duration, bool) {
	if len(e.samples) < 2 {
		return 0, false
	}
	first, last := e.samples[0], e.samples[len(e.samples)-1]
	span := last.At.Sub(first.At)
	delta := last.Done - first.Done
	if span <= 0 || delta <= 0 {
		return 0, false
	}
	rate := float64(delta) / span.Seconds()
	return time.Duration(float64(total-done) / rate * float64(time.Second)), true
}
