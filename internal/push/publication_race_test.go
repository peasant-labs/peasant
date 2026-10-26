//go:build race

package push

// publicationRaceEnabled records whether the race detector is active. The
// real-size publication cap materialization is a serial, memory-only case that
// the race detector only inflates; the injected-limit mechanics cover the same
// production scans in the race pass.
const publicationRaceEnabled = true
