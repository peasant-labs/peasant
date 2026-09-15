package api

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// groupedCandidateScope selects the visibility predicate one candidate set is
// projected through.
type groupedCandidateScope int

const (
	// groupedScopeDiscovery is the local session-list route: an ordinary row
	// passes BOTH discovery scopes, while a helper_review row is offered as a
	// member of its owner group when it passes SELECTION scope rather than being
	// withheld by the generic agent-child discovery rule.
	groupedScopeDiscovery groupedCandidateScope = iota
	// groupedScopeSelectedSearch is the local search route. Its caller has
	// already applied the canonical selection matcher to the ranked matches, so
	// this scope only projects the surviving rows. Search keeps its own origin
	// policy: it is narrowed by selection, never by origin scope.
	groupedScopeSelectedSearch
)

// GroupedScopeRevision reports the selection revision the member scope compares
// against, so a changed persisted selection refuses a stale scope instead of
// replaying it against a different visible set.
func (p *StoreDataProvider) GroupedScopeRevision() string {
	return p.visibility.Revision()
}

// GroupedCandidates returns the route-predicate-applied candidate sessions for
// one originating grouped variant.
//
// The session-list variant applies the SAME discovery scope the flat
// /api/v1/sessions route applies, with one narrow exception: a helper_review
// session is offered as a member of its owner group when it passes SELECTION
// scope, rather than being withheld by the generic agent-child discovery rule.
// A helper that fails selection is withheld entirely, so hidden, unselected and
// ineligible helpers contribute zero rows and zero counts. When the parsed
// filters name one project, only that project's rows are candidates, so the
// counts, the groups and every issued member scope stay inside one project.
//
// The search variant reads the flat /api/v1/search matches, then applies the
// canonical SELECTION matcher before grouping, counting and member replay: the
// grouped browse surfaces narrow discovery to the persisted selection, while the
// flat search route and direct by-ID access stay exactly as they were. It reads
// the widest ranked window the flat route allows and applies the requested page
// limit to the SELECTED matches, so an unselected hit can never take the place
// of a selected one inside the bounded window.
func (p *StoreDataProvider) GroupedCandidates(ctx context.Context, filters GroupedFilters) ([]GroupedCandidate, error) {
	switch filters.Variant {
	case GroupedRouteSessions:
		rows, err := p.store.AllSessions(ctx)
		if err != nil {
			return nil, fmt.Errorf("store adapter: grouped session rows: %w", err)
		}
		indexed := indexedGroupedRows(rows)
		return p.groupedCandidatesFromRows(ctx, scopeGroupedRowsToProject(indexed, filters.ProjectHash), nil, groupedScopeDiscovery)
	case GroupedRouteSearch:
		payload, err := p.Search(ctx, filters.SearchQuery, codemap.SearchMaxLimit)
		if err != nil {
			return nil, fmt.Errorf("store adapter: grouped search rows: %w", err)
		}
		matches, indexed, err := p.selectedGroupedSearchRows(ctx, payload.Results, filters.SearchLimit)
		if err != nil {
			return nil, err
		}
		return p.groupedCandidatesFromRows(ctx, indexed, matches, groupedScopeSelectedSearch)
	default:
		return nil, fmt.Errorf("store adapter: grouped candidates: route variant %q has no local predicate in this build; the grouped list cannot be served from it; request view=grouped from a registered route", filters.Variant)
	}
}

// indexedGroupedRows keeps only the rows whose entries were indexed. A row
// without a completed index pass has no transcript to open, so it never enters a
// grouped candidate set or its counts.
func indexedGroupedRows(rows []store.SessionRow) []store.SessionRow {
	indexed := make([]store.SessionRow, 0, len(rows))
	for i := range rows {
		if rows[i].IndexedAt == nil {
			continue
		}
		indexed = append(indexed, rows[i])
	}
	return indexed
}

// scopeGroupedRowsToProject keeps only the rows that belong to ONE opaque
// project identity. It is the route predicate a project-scoped grouped request
// applies, so the ordinary items, the helper groups, the counts and every issued
// member scope derive from that one project and never from a sibling. An empty
// project keeps every row, which is the legacy cross-project grouped list.
func scopeGroupedRowsToProject(rows []store.SessionRow, project schema.ProjectHash) []store.SessionRow {
	if project == "" {
		return rows
	}
	scoped := make([]store.SessionRow, 0, len(rows))
	for i := range rows {
		if rows[i].ProjectHash == project.String() {
			scoped = append(scoped, rows[i])
		}
	}
	return scoped
}

// selectedGroupedSearchRows applies the canonical selection matcher to the
// ranked global search matches BEFORE paging. It returns the per-session matches
// that survive selection and the page limit, plus the stored rows they belong to,
// in ranked first-seen order. The page limit counts matched ENTRIES, exactly like
// the flat search page, but it is applied after selection: the caller reads the
// widest ranked window the flat route allows, so an unselected entry can never
// displace a selected one. Sessions that keep no entry are dropped, and a session
// is never returned with an empty matches arm.
func (p *StoreDataProvider) selectedGroupedSearchRows(ctx context.Context, results []schema.SearchResult, limit int) (map[string][]schema.SearchResult, []store.SessionRow, error) {
	rankedIDs := make([]string, 0, len(results))
	seen := make(map[string]bool, len(results))
	for _, result := range results {
		if seen[result.SessionID] {
			continue
		}
		seen[result.SessionID] = true
		rankedIDs = append(rankedIDs, result.SessionID)
	}
	rows, err := p.store.SessionsByIDs(ctx, rankedIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("store adapter: grouped search summaries: %w", err)
	}
	indexed := indexedGroupedRows(rows)

	selected := make(map[string]bool, len(indexed))
	for i := range indexed {
		visible, visibilityErr := p.visibility.Visible(p.sessionCandidate(&indexed[i]))
		if visibilityErr != nil {
			return nil, nil, fmt.Errorf("store adapter: grouped search %q selection scope: %w", indexed[i].SessionID, visibilityErr)
		}
		selected[indexed[i].SessionID] = visible
	}

	if limit <= 0 {
		limit = codemap.SearchDefaultLimit
	}
	matches := make(map[string][]schema.SearchResult, len(indexed))
	kept := make(map[string]bool, len(indexed))
	entries := 0
	for _, result := range results {
		if !selected[result.SessionID] {
			continue
		}
		if entries >= limit {
			break
		}
		matches[result.SessionID] = append(matches[result.SessionID], result)
		kept[result.SessionID] = true
		entries++
	}
	keptRows := make([]store.SessionRow, 0, len(kept))
	for i := range indexed {
		if kept[indexed[i].SessionID] {
			keptRows = append(keptRows, indexed[i])
		}
	}
	return matches, keptRows, nil
}

// groupedCandidatesFromRows builds the candidate projection from stored rows.
// For groupedScopeDiscovery the session-list scope applies with the helper_review
// member-widening rule described on GroupedCandidates; for
// groupedScopeSelectedSearch the caller already applied selection and this path
// only projects the survivors.
func (p *StoreDataProvider) groupedCandidatesFromRows(ctx context.Context, rows []store.SessionRow, matches map[string][]schema.SearchResult, scope groupedCandidateScope) ([]GroupedCandidate, error) {
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

		if scope == groupedScopeDiscovery {
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
