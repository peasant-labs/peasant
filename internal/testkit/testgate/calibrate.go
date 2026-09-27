package testgate

import (
	"runtime"
	"time"
)

// CalibrationReferenceNS is the probe's wall on the reference machine, measured
// once and committed. The calibration factor L = probe_on_this_box /
// CalibrationReferenceNS tells a reader how loaded this box is relative to the
// reference; a budget is reference-machine time, so L has to be reported
// alongside any raw wall.
const CalibrationReferenceNS = 36_800_000

// calibrationRuns is the number of probe repetitions; the minimum is used so a
// scheduling hiccup does not inflate L.
const calibrationRuns = 5

// calibrationSink defeats dead-code elimination of the probe loop.
var calibrationSink uint64

// Calibrate runs a fixed CPU-bound probe and returns L and the raw minimum
// probe wall.
func Calibrate() (float64, time.Duration) {
	best := time.Duration(1<<62 - 1)
	for i := 0; i < calibrationRuns; i++ {
		start := time.Now()
		probe()
		d := time.Since(start)
		if d < best {
			best = d
		}
	}
	runtime.KeepAlive(calibrationSink)
	return float64(best) / float64(CalibrationReferenceNS), best
}

// probe is a deterministic integer mix with a fixed iteration count.
func probe() {
	var x uint64 = 0x9e3779b97f4a7c15
	for i := 0; i < 30_000_000; i++ {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
	}
	calibrationSink = x
}
