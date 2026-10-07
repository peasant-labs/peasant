package store

// The harvest-side sweep hook for the harmonized content model (design §4.5;
// peasant-labs/peasant#568).
//
// The per-session sweep is bounded, resumable, and idempotent; every predicate
// is qualified by session_id. It runs in harvest (after a commit, or alone for
// an unchanged flagged session), in reclaim, and in `peasant migrate` Phase 4.
// The reclaim side already reports through reclaimSeam
// (generation_reclaim.go); this hook is the harvest-side counterpart, following
// the same seam pattern: production leaves it nil, and a crash-recovery test
// sets it to observe the delete-to-rebuild window (the `after-commit-before-
// sweep` and `mid-sweep-batch` cases).
//
// The sweep implementation (which reports these stages) lands with the sweep
// itself; this file only names the stages and the hook so the branches compile
// against one seam.
const (
	// contentSweepSeamAfterCommit is reported after the session's catalog
	// transaction commits and before the sweep deletes anything.
	contentSweepSeamAfterCommit = "after-commit-before-sweep"
	// contentSweepSeamMidBatch is reported between the sweep's bounded delete
	// batches.
	contentSweepSeamMidBatch = "mid-sweep-batch"
)

// contentSweepSeam is a nil production hook a crash-recovery test sets to stop
// the sweep at a named stage. It mirrors reclaimSeam: set it only in tests,
// and reset it to nil when the test finishes.
var contentSweepSeam func(stage string) error

// reportContentSweepSeam reports one sweep stage to the hook, if set. A nil
// hook is success: production sweeps run to completion with no observer.
func reportContentSweepSeam(stage string) error {
	if contentSweepSeam == nil {
		return nil
	}
	return contentSweepSeam(stage)
}
