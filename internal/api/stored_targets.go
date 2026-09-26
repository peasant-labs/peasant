package api

import (
	"context"
)

// StoredTarget is one link-resolution outcome for an already-stored session.
// Found reports whether the identifier names a stored session; Missing names
// are reported, never failed, so one stale parent link cannot break the
// resolution of the sessions that do exist.
type StoredTarget struct {
	ID      string
	Found   bool
	Summary *SessionSummary
}

// ResolveStoredTargets resolves stored sessions for parent/context navigation.
// It deliberately applies NEITHER origin scope NOR selection scope: selection
// scopes discovery and lists only, and a stored-but-unselected parent stays
// deep-linkable. An identifier that names no stored session resolves to an
// explicit unavailable target ({Found:false}) rather than an error, so the
// caller can render an honest unresolved reference and keep the child
// readable. Summaries come from the same construction site as the list path,
// so link targets and list rows cannot drift in what a summary contains.
func (p *StoreDataProvider) ResolveStoredTargets(ctx context.Context, ids []string) ([]StoredTarget, error) {
	if len(ids) == 0 {
		return []StoredTarget{}, nil
	}
	summaries, err := p.SessionSummariesByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*SessionSummary, len(summaries))
	for i := range summaries {
		byID[summaries[i].ID] = &summaries[i]
	}
	targets := make([]StoredTarget, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		target := StoredTarget{ID: id}
		if summary, ok := byID[id]; ok {
			target.Found = true
			target.Summary = summary
		}
		targets = append(targets, target)
	}
	return targets, nil
}
