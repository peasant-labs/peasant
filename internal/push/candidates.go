package push

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
)

// PushCandidateQuery captures the inputs that select the BASE candidate set for a
// push run, BEFORE branch-aware selection (ApplySelection) and wizard narrowing.
//
// It exists so the pipeline and the interactive wizard ask "which sessions?" the
// SAME way: both call QueryPushCandidates with this query, eliminating the prior
// divergence where the pipeline handled PushMethodBySource but the wizard did not.
type PushCandidateQuery struct {
	// Force selects all pushable sessions regardless of pushed_at (--force).
	Force bool
	// SourceProvider, when non-empty, filters to a single model_harness value.
	SourceProvider string
	// Method is the configured push method (all / by-source / individual).
	Method config.PushMethod
	// Sources lists the providers iterated when Method == PushMethodBySource.
	Sources []string
}

// CandidateStore is the complete query surface shared by the interactive
// wizard and the push pipeline when selecting base candidate sessions.
type CandidateStore interface {
	UnpushedSessions(context.Context) ([]ingest.PushSessionRow, error)
	UnpushedSessionsByProvider(context.Context, string) ([]ingest.PushSessionRow, error)
	AllPushableSessions(context.Context) ([]ingest.PushSessionRow, error)
}

// QueryPushCandidates returns the base unfiltered candidate rows for a push run,
// resolving force / source-provider / method against the store. This is the
// SINGLE base-query path: both the pipeline's getTargetSessions and the CLI's
// buildPushWizardSessions call it, so the wizard view, the dry-run set, and the
// real push set cannot diverge.
//
// The returned rows are NOT yet branch-filtered — callers apply ApplySelection
// (and, for the pipeline, the wizard whitelist) afterward.
func QueryPushCandidates(ctx context.Context, store CandidateStore, q PushCandidateQuery) ([]ingest.PushSessionRow, error) {
	switch {
	case q.Force:
		// --force: all sessions regardless of pushed_at, optionally provider-filtered.
		sessions, err := store.AllPushableSessions(ctx)
		if err != nil {
			return nil, err
		}
		if q.SourceProvider != "" {
			sessions = filterByProvider(sessions, q.SourceProvider)
		}
		return sessions, nil

	case q.SourceProvider != "":
		// --source-provider: unpushed sessions for one provider.
		return store.UnpushedSessionsByProvider(ctx, q.SourceProvider)

	case q.Method == config.PushMethodBySource:
		// push.method by-source: iterate over configured providers.
		var sessions []ingest.PushSessionRow
		for _, provider := range q.Sources {
			provSessions, provErr := store.UnpushedSessionsByProvider(ctx, provider)
			if provErr != nil {
				return nil, fmt.Errorf("query provider %q: %w", provider, provErr)
			}
			sessions = append(sessions, provSessions...)
		}
		return sessions, nil

	default:
		// Default (method=all or empty): all unpushed sessions.
		return store.UnpushedSessions(ctx)
	}
}
