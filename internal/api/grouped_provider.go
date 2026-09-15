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
// the ranked stream in windows and counts the requested page limit only over
// the SELECTED entries, so an unselected hit can never take the place of a
// selected one — not even when a whole window of unselected hits outranks every
// selected match.
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
		matches, indexed, err := p.selectedGroupedSearchRows(ctx, filters.SearchQuery, filters.SearchLimit)
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
// ranked global search stream BEFORE it counts the requested page limit. It
// walks successive ranked windows of the flat search's own predicate and keeps
// the entries whose session passes SELECTION scope, until it has a full page of
// selected entries or the ranked stream is exhausted. The page limit counts
// matched ENTRIES, exactly like the flat search page, but it is applied after
// selection, so an unselected entry can never displace a selected one — even
// when an entire window of unselected matches outranks every selected match.
// Sessions that keep no entry are dropped, and a session is never returned with
// an empty matches arm. The returned rows follow first-seen ranked order.
func (p *StoreDataProvider) selectedGroupedSearchRows(ctx context.Context, query string, limit int) (map[string][]schema.SearchResult, []store.SessionRow, error) {
	if limit <= 0 {
		limit = codemap.SearchDefaultLimit
	}
	matches := make(map[string][]schema.SearchResult)
	selected := make(map[string]bool)
	rows := make(map[string]store.SessionRow)
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
		if err := p.resolveGroupedSearchSelection(ctx, window, selected, rows); err != nil {
			return nil, nil, err
		}
		for _, result := range window {
			if !selected[result.SessionID] {
				continue
			}
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
	keptRows := make([]store.SessionRow, 0, len(rankedIDs))
	for _, id := range rankedIDs {
		keptRows = append(keptRows, rows[id])
	}
	return matches, keptRows, nil
}

// resolveGroupedSearchSelection fetches the stored rows named by one ranked
// window that have not been judged yet and records the canonical SELECTION
// verdict for them in selected. rows keeps the fetched indexed rows for the
// final projection, so a session whose entries span several windows is fetched
// and judged once instead of once per window. A session the store does not
// return, or whose entries were never indexed, is not selectable and keeps no
// entries.
func (p *StoreDataProvider) resolveGroupedSearchSelection(ctx context.Context, window []schema.SearchResult, selected map[string]bool, rows map[string]store.SessionRow) error {
	pending := make([]string, 0, len(window))
	queued := make(map[string]bool, len(window))
	for _, result := range window {
		if _, judged := selected[result.SessionID]; judged {
			continue
		}
		if queued[result.SessionID] {
			continue
		}
		queued[result.SessionID] = true
		pending = append(pending, result.SessionID)
	}
	if len(pending) > 0 {
		fetched, err := p.store.SessionsByIDs(ctx, pending)
		if err != nil {
			return fmt.Errorf("store adapter: grouped search summaries: %w", err)
		}
		for i := range fetched {
			if fetched[i].IndexedAt == nil {
				continue
			}
			rows[fetched[i].SessionID] = fetched[i]
		}
	}
	for _, id := range pending {
		row, indexed := rows[id]
		if !indexed {
			selected[id] = false
			continue
		}
		visible, err := p.visibility.Visible(p.sessionCandidate(&row))
		if err != nil {
			return fmt.Errorf("store adapter: grouped search %q selection scope: %w", id, err)
		}
		selected[id] = visible
	}
	return nil
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
