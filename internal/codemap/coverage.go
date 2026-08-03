package codemap

import "sort"

// coverage is the file-granularity traceability measure: which files' latest
// state is attributable to a recorded session.
type coverage struct {
	// universe is the sorted set of files coverage is measured over.
	universe []string
	// recorded marks the universe files attributed to recorded sessions.
	recorded map[string]bool
}

// computeCoverage classifies each file as recorded or not.
//
// Coverage would ideally compare the latest recorded edit with the latest
// commit touching each file. gitops.Repository exposes no per-file commit log,
// so this implementation uses the deterministic comparison below. Per-file
// `git log -1` metadata would enable the timestamp comparison.
//
//   - repo present: the universe is the union of tracked files at the
//     resolved ref and recorded-edit files; a file is recorded iff it is
//     tracked at the ref AND has at least one recorded edit. Tracked-at-ref
//     keeps deleted/renamed-away recorded paths out of the recorded set
//     while unedited tracked files still widen the denominator ("34 of 37
//     files recorded").
//   - repo missing: the universe is the recorded-edit files alone and every
//     one of them is recorded — the contract's "has any recorded edit"
//     fallback.
func computeCoverage(repoFound bool, trackedFiles []string, stats map[string]*fileStats) coverage {
	cov := coverage{recorded: make(map[string]bool)}
	universe := make(map[string]bool)

	if repoFound {
		tracked := make(map[string]bool, len(trackedFiles))
		for _, f := range trackedFiles {
			tracked[f] = true
			universe[f] = true
		}
		for f := range stats {
			universe[f] = true
			if tracked[f] {
				cov.recorded[f] = true
			}
		}
	} else {
		for f := range stats {
			universe[f] = true
			cov.recorded[f] = true
		}
	}

	cov.universe = make([]string, 0, len(universe))
	for f := range universe {
		cov.universe = append(cov.universe, f)
	}
	sort.Strings(cov.universe)
	return cov
}
