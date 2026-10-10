//go:build !race

package push

// publicationRaceEnabled records whether the race detector is active. The
// real-size publication cap materialization runs only in this non-race pass.
const publicationRaceEnabled = false
