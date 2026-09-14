package api

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// GroupedScopeRevision reports the selection revision the member scope compares
// against, so a changed persisted selection refuses a stale scope instead of
// replaying it against a different visible set.
func (p *StoreDataProvider) GroupedScopeRevision() string {
	return p.visibility.Revision()
}

// GroupedCandidates returns the route-predicate-applied candidate sessions for
// one originating grouped variant. The session-list variant applies the SAME
// discovery scope the flat /api/v1/sessions route applies, with one narrow
// exception: a helper_review session is offered as a member of its owner group
// when it passes SELECTION scope, rather than being withheld by the generic
// agent-child discovery rule. A helper that fails selection is withheld
// entirely, so hidden, unselected and ineligible helpers contribute zero rows
// and zero counts.
//
// The search variant mirrors the flat /api/v1/search route: matches are global
// full-text hits, so grouping never narrows or widens the existing search set.
func (p *StoreDataProvider) GroupedCandidates(ctx context.Context, filters GroupedFilters) ([]GroupedCandidate, error) {
	switch filters.Variant {
	case GroupedRouteSessions:
		rows, err := p.store.AllSessions(ctx)
		if err != nil {
			return nil, fmt.Errorf("store adapter: grouped session rows: %w", err)
		}
		indexed := make([]store.SessionRow, 0, len(rows))
		for i := range rows {
			if rows[i].IndexedAt == nil {
				continue
			}
			indexed = append(indexed, rows[i])
		}
		return p.groupedCandidatesFromRows(ctx, indexed, nil, false)
	case GroupedRouteSearch:
		payload, err := p.Search(ctx, filters.SearchQuery, filters.SearchLimit)
		if err != nil {
			return nil, fmt.Errorf("store adapter: grouped search rows: %w", err)
		}
		matches := make(map[string][]schema.SearchResult, len(payload.Results))
		ids := make([]string, 0, len(payload.Results))
		seen := make(map[string]bool, len(payload.Results))
		for _, result := range payload.Results {
			matches[result.SessionID] = append(matches[result.SessionID], result)
			if !seen[result.SessionID] {
				seen[result.SessionID] = true
				ids = append(ids, result.SessionID)
			}
		}
		rows, err := p.store.SessionsByIDs(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("store adapter: grouped search summaries: %w", err)
		}
		indexed := make([]store.SessionRow, 0, len(rows))
		for i := range rows {
			if rows[i].IndexedAt == nil {
				continue
			}
			indexed = append(indexed, rows[i])
		}
		return p.groupedCandidatesFromRows(ctx, indexed, matches, true)
	default:
		return nil, fmt.Errorf("store adapter: grouped candidates: route variant %q has no local predicate in this build; the grouped list cannot be served from it; request view=grouped from a registered route", filters.Variant)
	}
}

// groupedCandidatesFromRows builds the candidate projection from stored rows.
// searchScoped selects the global search predicate (no discovery scope);
// otherwise the session-list discovery scope applies with the helper_review
// member-widening rule described on GroupedCandidates.
func (p *StoreDataProvider) groupedCandidatesFromRows(ctx context.Context, rows []store.SessionRow, matches map[string][]schema.SearchResult, searchScoped bool) ([]GroupedCandidate, error) {
	if len(rows) == 0 {
		return []GroupedCandidate{}, nil
	}
	summaries, err := p.summariesFromRows(ctx, rows)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].SessionID
	}
	evidence, err := p.store.GroupingEvidenceForSessions(ctx, ids)
	if err != nil {
		return nil, err
	}

	candidates := make([]GroupedCandidate, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		ev := evidence[row.SessionID]
		summary := summaries[i]
		summary.Purpose = ev.Purpose
		summary.InputSubmissionCount = ev.InputSubmissionCount

		if !searchScoped {
			if ev.Purpose == schema.SessionPurposeHelperReview {
				visible, visibilityErr := p.visibility.Visible(p.sessionCandidate(row))
				if visibilityErr != nil {
					return nil, fmt.Errorf("store adapter: grouped helper %q selection scope: %w", row.SessionID, visibilityErr)
				}
				if !visible {
					continue
				}
			} else {
				visible, visibilityErr := p.discoverableSessionRow(row)
				if visibilityErr != nil {
					return nil, fmt.Errorf("store adapter: grouped session %q discovery scope: %w", row.SessionID, visibilityErr)
				}
				if !visible {
					continue
				}
			}
		}

		candidate := GroupedCandidate{
			Summary:    summary,
			StartMs:    row.StartMs,
			StableID:   row.SessionID,
			HostSlug:   row.HostSlug,
			Harness:    row.ModelHarness,
			Purpose:    ev.Purpose,
			OwnerState: ev.OwnerState,
			Matches:    matches[row.SessionID],
		}
		if ev.OwnerTargetLocalID != nil {
			candidate.OwnerTargetLocalID = *ev.OwnerTargetLocalID
		}
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}
