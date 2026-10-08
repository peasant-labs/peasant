package ingest

import (
	"context"
	"fmt"
	"log/slog"
)

// ContentSweeper is the optional per-session sweep capability a store
// exposes to the harvest (design §4.5): after an activation commits, the
// pipeline sweeps the session it just committed; at harvest start it sweeps
// every flagged session. A store that does not implement it (older builds,
// narrow test doubles) skips both passes through the ok-assertion at each
// call site, so the harvest never depends on the sweep to make progress.
type ContentSweeper interface {
	// SweepSessionForHarvest sweeps one committed session: superseded rows,
	// leftover directories, orphan objects, and the flag clear. The commit
	// stays durable regardless of the sweep error.
	SweepSessionForHarvest(ctx context.Context, session SessionID) error
	// SweepFlaggedSessionsForHarvest sweeps every flagged session and
	// reports how many sessions it swept. Per-session failures come back
	// as warnings (those sessions keep their flag); a listing failure is
	// a fatal error.
	SweepFlaggedSessionsForHarvest(ctx context.Context) (swept int, warnings []error, err error)
}

// sweepCommittedSession runs the after-commit per-session sweep for one
// activation the store committed (or idempotently re-committed): the flag
// the staging set is cleared with the superseded rows and orphans it
// marked. A sweep failure never fails the session: the commit is durable,
// the flag stays set, and the next harvest recovers it.
func (p *Pipeline) sweepCommittedSession(ctx context.Context, session SessionID, logPrefix string) {
	sweeper, ok := p.metricsStore.(ContentSweeper)
	if !ok {
		return
	}
	if err := sweeper.SweepSessionForHarvest(ctx, session); err != nil {
		slog.Warn(logPrefix+": per-session sweep after commit failed", "session_id", session, "error", err)
	}
}

// sweepFlaggedSessionsAtStart runs the harvest-start recovery pass: every
// session the crash marker flags is swept before this harvest selects or
// commits anything, so a crash between staging and commit recovers by
// re-harvest even when the input is unchanged. Per-session failures are
// warnings (those sessions keep their flag for the next pass); only a
// listing failure aborts the harvest. The search-index health gate runs
// before this pass once the search change lands it.
func (p *Pipeline) sweepFlaggedSessionsAtStart(ctx context.Context) error {
	sweeper, ok := p.metricsStore.(ContentSweeper)
	if !ok {
		return nil
	}
	swept, warnings, err := sweeper.SweepFlaggedSessionsForHarvest(ctx)
	if err != nil {
		return fmt.Errorf("pipeline harvest-start sweep: %w; no session was swept; fix database access and retry harvest", err)
	}
	for _, warning := range warnings {
		slog.Warn("pipeline harvest-start sweep skipped a session", "error", warning)
	}
	if swept > 0 {
		slog.Info("pipeline harvest-start sweep recovered flagged sessions", "sessions", swept)
	}
	return nil
}
