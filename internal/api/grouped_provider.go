package api

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// GroupedScopeRevision reports the browsing revision the member scope compares
// against. It is deliberately independent of the saved kickstart selection: a
// changed ingest selector must not invalidate a browsing continuation.
func (p *StoreDataProvider) GroupedScopeRevision() string {
	return p.visibility.Revision()
}

// GroupedCandidates returns the route-predicate-applied candidate sessions for
// one originating grouped variant.
//
// The session-list variant applies the SAME origin discovery scope the flat
// /api/v1/sessions route applies, with one narrow exception: a helper_review
// session is offered as a member of its owner group rather than being withheld
// by the generic agent-child discovery rule. Helper semantics are otherwise
// unchanged. When the parsed filters name one project, only that project's rows
// are candidates, so the counts, the groups and every issued member scope stay
// inside one project. The saved kickstart selection is not applied: it decides
// what Peasant ingests next, not which ingested rows this list shows.
//
// The search variant reads the flat /api/v1/search matches and groups them
// directly. Search keeps its own policy and is not narrowed by origin or by the
// saved selection.
func (p *StoreDataProvider) GroupedCandidates(ctx context.Context, filters GroupedFilters) ([]GroupedCandidate, error) {
	switch filters.Variant {
	case GroupedRouteSessions:
		rows, err := p.store.AllSessions(ctx)
		if err != nil {
			return nil, fmt.Errorf("store adapter: grouped session rows: %w", err)
		}
		indexed := indexedGroupedRows(rows)
		return p.groupedCandidatesFromRows(ctx, scopeGroupedRowsToProject(indexed, filters.ProjectHash), nil)
	case GroupedRouteSearch:
		matches, indexed, err := p.groupedSearchRows(ctx, filters.SearchQuery, filters.SearchLimit)
		if err != nil {
			return nil, err
		}
		return p.groupedCandidatesFromRows(ctx, indexed, matches)
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

// groupedSearchRows reads the flat ranked search stream in windows and keeps the
// first `limit` matched entries, exactly like the flat search page. No
// eligibility judgement is applied: the grouped view shows the same stored
// matches the flat route shows, so an ingested hit can never be displaced by a
// selection decision. A session whose entries were never indexed keeps no
// entries; the returned rows follow first-seen ranked order.
func (p *StoreDataProvider) groupedSearchRows(ctx context.Context, query string, limit int) (map[string][]schema.SearchResult, []store.SessionRow, error) {
	if limit <= 0 {
		limit = codemap.SearchDefaultLimit
	}
	matches := make(map[string][]schema.SearchResult)
	rankedIDs := make([]string, 0)
	entries := 0
	offset := 0
	for {
		page, err := p.codemap.SearchRankedWindow(ctx, query, codemap.SearchMaxLimit, offset)
		if err != nil {
			return nil, nil, fmt.Errorf("store adapter: grouped search ranked window at offset %d: %w", offset, err)
		}
		window := page.Results
		if len(window) == 0 {
			break
		}
		for _, result := range window {
			if entries >= limit {
				break
			}
			if _, kept := matches[result.SessionID]; !kept {
				rankedIDs = append(rankedIDs, result.SessionID)
			}
			matches[result.SessionID] = append(matches[result.SessionID], result)
			entries++
		}
		offset += len(window)
		if entries >= limit || len(window) < codemap.SearchMaxLimit {
			break
		}
	}
	if len(rankedIDs) == 0 {
		return matches, []store.SessionRow{}, nil
	}
	fetched, err := p.store.SessionsByIDs(ctx, rankedIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("store adapter: grouped search summaries: %w", err)
	}
	byID := make(map[string]store.SessionRow, len(fetched))
	for i := range fetched {
		if fetched[i].IndexedAt == nil {
			continue
		}
		byID[fetched[i].SessionID] = fetched[i]
	}
	rows := make([]store.SessionRow, 0, len(rankedIDs))
	for _, id := range rankedIDs {
		if row, indexed := byID[id]; indexed {
			rows = append(rows, row)
		}
	}
	return matches, rows, nil
}

// groupedCandidatesFromRows builds the candidate projection from stored rows.
// The session-list scope applies the origin discovery rule, except that a
// helper_review row is offered as a member of its owner group. The search scope
// already carries only indexed matches and projects them unchanged.
func (p *StoreDataProvider) groupedCandidatesFromRows(ctx context.Context, rows []store.SessionRow, matches map[string][]schema.SearchResult) ([]GroupedCandidate, error) {
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

		if matches == nil && ev.Purpose != schema.SessionPurposeHelperReview {
			visible, visibilityErr := p.discoverableSessionRow(row)
			if visibilityErr != nil {
				return nil, fmt.Errorf("store adapter: grouped session %q discovery scope: %w", row.SessionID, visibilityErr)
			}
			if !visible {
				continue
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
