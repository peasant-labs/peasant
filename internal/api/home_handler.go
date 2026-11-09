package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

type homeReader interface {
	ingest.PublicationMetadataReader
	// HomeIdentityCohort is read only by the app-side auto-publish lane and the
	// project-options picker. A plain root or child page never builds it.
	HomeIdentityCohort(context.Context) ([]store.HomeCandidate, error)
	SyncParentIdentities(context.Context, []ingest.PushSessionRow) ([]store.ParentIdentityRow, error)
	HomePageRows(context.Context, []string) ([]store.HomePageRow, error)
	SessionsByIDs(context.Context, []string) ([]store.SessionRow, error)
	FirstUserMessageBulk(context.Context, []string) (map[string]string, error)
	GroupingEvidenceForSessions(context.Context, []string) (map[string]store.GroupingEvidenceRow, error)
	ClaudeTeammates(context.Context, []string) (map[string]schema.SessionTeammate, error)
	RootPageWindow(context.Context, int, int, store.SyncWindowFilter) (store.OffsetPageRead, error)
	ChildPageWindow(context.Context, string, int, int) (store.OffsetPageRead, error)
	RootSessionIDs(context.Context, store.SyncWindowFilter) ([]string, error)
	AllPushableSessionIDs(context.Context, store.SyncWindowFilter) ([]string, error)
	RootProjectOptions(context.Context) ([]store.RootProjectOption, error)
	CountRootSessions(context.Context, store.SyncWindowFilter) (int, error)
	CountPushableSessions(context.Context, store.SyncWindowFilter) (int, error)
	CountResolvedChildren(context.Context, string) (int, error)
	ResolvedChildCounts(context.Context, []string) (map[string]int, error)
	PushableSessionExists(context.Context, string) (bool, error)
	Revision(context.Context) (string, error)
}

var _ homeReader = (*store.Store)(nil)

type homeHandler struct {
	reader     homeReader
	sync       *syncHandler
	publishing *publishingHandler
	resolver   ingest.RepositoryIdentityResolver
}

type syncPageError struct{ code, message string }

func (e *syncPageError) Error() string { return e.message }

type syncHomePageReader struct {
	db         homeReader
	query      schema.LocalSyncSessionsQuery
	config     *config.Config
	publishing *publishingHandler
	account    *auth.Credentials
	resolver   ingest.RepositoryIdentityResolver
}

func (h *homeHandler) newReader(q schema.LocalSyncSessionsQuery) *syncHomePageReader {
	return &syncHomePageReader{db: h.reader, query: q, config: h.sync.currentConfig(), publishing: h.publishing, account: h.publishing.signedIn(), resolver: h.resolver}
}

func projectValue(p *schema.ProjectHash) string {
	if p == nil {
		return ""
	}
	return p.String()
}

func (r *syncHomePageReader) accountScope() (string, string) {
	if r.account == nil {
		return "", ""
	}
	return r.account.VillageURL, r.account.UserID
}

// syncHomeCohort is per-read metadata only. The auto-publish lane and the
// project-options picker need the complete stored identity set; the plain root
// and child pages do not.
type syncHomeCohort struct {
	rows    map[string]store.HomeCandidate
	ordered []store.SessionRow
}

func (r *syncHomePageReader) cohort(ctx context.Context) (syncHomeCohort, error) {
	c := syncHomeCohort{rows: map[string]store.HomeCandidate{}}
	rows, err := r.db.HomeIdentityCohort(ctx)
	if err != nil {
		return c, err
	}
	for _, candidate := range rows {
		if err := ctx.Err(); err != nil {
			return c, err
		}
		row := candidate.Identity
		c.rows[row.SessionID] = candidate
		c.ordered = append(c.ordered, row)
	}
	return c, nil
}

// selectionMembershipForRows computes the saved INGEST/PUSH selector's verdict
// for requested IDs from the stored identity rows the caller already holds.
func (r *syncHomePageReader) selectionMembershipForRows(ctx context.Context, rows []store.SessionRow, ids []string) (map[string]bool, error) {
	selection := config.SelectionConfig{Mode: config.SelectionModeAll}
	if r.config != nil {
		selection = r.config.Selection
	}
	// Repository multiplicity is a property of the whole stored identity set,
	// not of one page: a selected project rule can be ambiguous only because a
	// clone of a returned row is off-page. The default selection needs no
	// multiplicity, so the plain page never reads the cohort; a selected rule
	// reads it only to keep outsideSelection truthful.
	if selection.Mode != config.SelectionModeAll {
		cohort, err := r.db.HomeIdentityCohort(ctx)
		if err != nil {
			return nil, err
		}
		rows = make([]store.SessionRow, 0, len(cohort))
		for _, candidate := range cohort {
			rows = append(rows, candidate.Identity)
		}
	}
	membership, err := storedSelectionMembership(ctx, rows, ids, selection, r.resolver)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, known := membership[id]; !known {
			return nil, fmt.Errorf("api.syncHomePageReader.selectionMembership: session %q is in the requested cohort but has no stored identity row, so outsideSelection cannot be reported truthfully; no page was returned; retry after verifying the local store", id)
		}
	}
	return membership, nil
}

// selectionMembership computes membership from the complete stored cohort. The
// auto-publish lane needs it because the decision is app-side, and it already
// holds the cohort, so this reads no second identity pass.
func (r *syncHomePageReader) selectionMembership(ctx context.Context, cohort syncHomeCohort, ids []string) (map[string]bool, error) {
	selection := config.SelectionConfig{Mode: config.SelectionModeAll}
	if r.config != nil {
		selection = r.config.Selection
	}
	membership, err := storedSelectionMembership(ctx, cohort.ordered, ids, selection, r.resolver)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, known := membership[id]; !known {
			return nil, fmt.Errorf("api.syncHomePageReader.selectionMembership: session %q is in the requested cohort but has no stored identity row, so outsideSelection cannot be reported truthfully; no page was returned; retry after verifying the local store", id)
		}
	}
	return membership, nil
}

// pageFilter is the root filter one page request carries.
func (r *syncHomePageReader) pageFilter() store.SyncWindowFilter {
	filter := schema.LocalSyncPublicationAll
	if r.query.Publication != nil {
		filter = *r.query.Publication
	}
	origin, owner := r.accountScope()
	return store.SyncWindowFilter{ProjectHash: projectValue(r.query.ProjectHash), Origin: origin, Owner: owner, Publication: filter}
}

// clampPage returns the last page for a one-based request past the end. An
// empty store answers page one. The caller echoes the clamped page, and the
// client rewrites the URL from it.
func clampPage(page, total, perPage int) int {
	last := 1
	if total > 0 {
		last = (total + perPage - 1) / perPage
	}
	if page > last {
		return last
	}
	if page < 1 {
		return 1
	}
	return page
}

// ReadPage serves the bounded offset root page or the resolved child page. The
// page, the counts, and the payload all carry one store revision.
func (r *syncHomePageReader) ReadPage(ctx context.Context) (schema.LocalSyncSessionPage, error) {
	perPage, err := r.query.EffectivePageSize()
	if err != nil {
		return schema.LocalSyncSessionPage{}, err
	}
	page := r.query.EffectivePage()
	revision, err := r.db.Revision(ctx)
	if err != nil {
		return schema.LocalSyncSessionPage{}, err
	}
	result := schema.LocalSyncSessionPage{
		OffsetPage: schema.OffsetPage[schema.LocalSyncSessionRow]{
			Items:     make([]schema.LocalSyncSessionRow, 0),
			NextItems: make([]schema.LocalSyncSessionRow, 0),
			Page:      page,
			PerPage:   perPage,
			Revision:  revision,
		},
		Totals: &schema.LocalSyncSessionTotals{},
	}
	filter := r.pageFilter()
	parent := value(r.query.ParentSessionID)
	var ids []string
	var total, sessions int
	if parent == "" {
		if filter.Publication == schema.LocalSyncPublicationAuto {
			ids, total, sessions, page, err = r.autoRootPage(ctx, page, int(perPage), filter)
		} else {
			total, err = r.db.CountRootSessions(ctx, filter)
			if err == nil {
				sessions, err = r.db.CountPushableSessions(ctx, filter)
			}
			if err == nil {
				page = clampPage(page, total, int(perPage))
				var window store.OffsetPageRead
				window, err = r.db.RootPageWindow(ctx, page, int(perPage), filter)
				ids = window.IDs
			}
		}
	} else {
		var exists bool
		exists, err = r.db.PushableSessionExists(ctx, parent)
		if err == nil && !exists {
			return result, &syncPageError{code: "home_parent_not_found", message: "api.syncHomePageReader: parent is not a stored pushable session; no child identities returned; open a parent from the current Home list and retry"}
		}
		if err == nil {
			total, err = r.db.CountResolvedChildren(ctx, parent)
		}
		if err == nil {
			sessions, err = r.db.CountPushableSessions(ctx, filter)
		}
		if err == nil {
			page = clampPage(page, total, int(perPage))
			var window store.OffsetPageRead
			window, err = r.db.ChildPageWindow(ctx, parent, page, int(perPage))
			ids = window.IDs
		}
	}
	if err != nil {
		return result, err
	}
	result.Page = page
	result.Total = total
	result.Totals.Sessions = sessions
	if total == 0 || len(ids) == 0 {
		return result, nil
	}
	pageIDs := ids
	nextIDs := []string{}
	if len(pageIDs) > int(perPage) {
		pageIDs = ids[:int(perPage)]
		end := 2 * int(perPage)
		if end > len(ids) {
			end = len(ids)
		}
		nextIDs = ids[int(perPage):end]
	}
	result.Items, err = r.hydrate(ctx, pageIDs)
	if err != nil {
		return result, err
	}
	result.NextItems, err = r.hydrate(ctx, nextIDs)
	if err != nil {
		return result, err
	}
	return result, nil
}

// autoRootPage is the app-side auto-publish lane: the decision depends on the
// saved selection, the installed hooks, and the stored worktree, so the ordered
// root list is filtered in memory. Only this lane builds the cohort.
func (r *syncHomePageReader) autoRootPage(ctx context.Context, page, perPage int, filter store.SyncWindowFilter) ([]string, int, int, int, error) {
	roots, err := r.db.RootSessionIDs(ctx, filter)
	if err != nil {
		return nil, 0, 0, page, err
	}
	cohort, err := r.cohort(ctx)
	if err != nil {
		return nil, 0, 0, page, err
	}
	rootsMembership, err := r.selectionMembership(ctx, cohort, roots)
	if err != nil {
		return nil, 0, 0, page, err
	}
	auto := r.publishing.autoPublicationChecker(ctx)
	filtered := make([]string, 0, len(roots))
	for _, id := range roots {
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, page, err
		}
		if rootsMembership[id] && auto(cohort.rows[id].Identity.GitWorktree) {
			filtered = append(filtered, id)
		}
	}
	all, err := r.db.AllPushableSessionIDs(ctx, store.SyncWindowFilter{ProjectHash: filter.ProjectHash, Publication: schema.LocalSyncPublicationAll})
	if err != nil {
		return nil, 0, 0, page, err
	}
	allMembership, err := r.selectionMembership(ctx, cohort, all)
	if err != nil {
		return nil, 0, 0, page, err
	}
	sessions := 0
	for _, id := range all {
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, page, err
		}
		if allMembership[id] && auto(cohort.rows[id].Identity.GitWorktree) {
			sessions++
		}
	}
	total := len(filtered)
	page = clampPage(page, total, perPage)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + 2*perPage
	if end > total {
		end = total
	}
	return filtered[start:end], total, sessions, page, nil
}

// hydrate renders only the returned IDs. Parent markers and child counts are
// read for the page, never for the whole store.
func (r *syncHomePageReader) hydrate(ctx context.Context, ids []string) ([]schema.LocalSyncSessionRow, error) {
	items := make([]schema.LocalSyncSessionRow, 0, len(ids))
	if len(ids) == 0 {
		return items, nil
	}
	rows, err := r.db.HomePageRows(ctx, ids)
	if err != nil {
		return nil, err
	}
	identity, err := r.db.SessionsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	membership, err := r.selectionMembershipForRows(ctx, identity, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]store.HomePageRow, len(rows))
	hydrated := make([]ingest.PushSessionRow, 0, len(rows))
	for _, row := range rows {
		byID[row.Sync.SessionID] = row
		hydrated = append(hydrated, row.Sync)
	}
	parents, err := r.db.SyncParentIdentities(ctx, hydrated)
	if err != nil {
		return nil, err
	}
	parentByID := make(map[string]store.ParentIdentityRow, len(parents))
	for _, parent := range parents {
		parentByID[parent.SessionID] = parent
	}
	children, err := r.db.ResolvedChildCounts(ctx, ids)
	if err != nil {
		return nil, err
	}
	metadata, err := push.LoadPublicationMetadata(ctx, r.db, hydrated)
	if err != nil {
		return nil, err
	}
	previews, err := r.db.FirstUserMessageBulk(ctx, ids)
	if err != nil {
		return nil, err
	}
	evidence, err := r.db.GroupingEvidenceForSessions(ctx, ids)
	if err != nil {
		return nil, err
	}
	teammates, err := r.db.ClaudeTeammates(ctx, ids)
	if err != nil {
		return nil, err
	}
	scope := make(map[string]bool, len(ids))
	for _, id := range ids {
		inSelection, known := membership[id]
		if !known {
			return nil, fmt.Errorf("api.syncHomePageReader.hydrate: session %q has no saved ingest/push selection membership, so outsideSelection cannot be reported truthfully; no page was returned; retry after verifying the local store and saved selection", id)
		}
		scope[id] = inSelection
	}
	publications, err := r.publishing.localPublications(ctx, ids, scope, r.account)
	if err != nil {
		return nil, err
	}
	publicationByID := map[string]schema.LocalPublication{}
	for _, p := range publications {
		publicationByID[p.SessionID] = p
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("api.syncHomePageReader: session %q changed during hydration; no page returned; restart traversal", id)
		}
		status, hold := computeSyncStatus(row.Sync, false, push.PublicationMetadataReady(metadata[id]))
		_, summary, err := buildSyncGroupedRow(syncSessionEntry{row: row.Sync, status: status, hold: hold, identity: parentByID[id]}, evidence[id], previews[id], teammateFor(teammates, id))
		if err != nil {
			return nil, err
		}
		items = append(items, schema.LocalSyncSessionRow{Sync: summary, Publication: publicationByID[id], Title: row.Title, Preview: previews[id], ChildCount: children[id]})
	}
	return items, nil
}

func (r *syncHomePageReader) projectOptions(ctx context.Context) (schema.LocalSyncProjectOptions, error) {
	options := schema.LocalSyncProjectOptions{Projects: make([]schema.LocalSyncProjectOption, 0)}
	projects, err := r.db.RootProjectOptions(ctx)
	if err != nil {
		return options, err
	}
	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			return options, err
		}
		hash, err := schema.NewProjectHash(project.ProjectHash)
		if err != nil {
			return options, err
		}
		options.Projects = append(options.Projects, schema.LocalSyncProjectOption{ProjectHash: hash, ProjectName: project.ProjectName})
	}
	sort.Slice(options.Projects, func(i, j int) bool {
		a, b := options.Projects[i], options.Projects[j]
		if a.ProjectName != b.ProjectName {
			return a.ProjectName < b.ProjectName
		}
		return a.ProjectHash < b.ProjectHash
	})
	if err := ctx.Err(); err != nil {
		return options, err
	}
	return options, options.Validate()
}

func (h *homeHandler) handleSessions(w http.ResponseWriter, req *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	query, err := schema.ParseLocalSyncSessionsQuery(req.URL.Query())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error(), "sync_page_query_invalid")
		return
	}
	for name := range req.URL.Query() {
		switch name {
		case "view", "maxItems", "page", "pageToken", "parentSessionId", "projectHash", "publication":
		default:
			writeAPIError(w, http.StatusBadRequest, "api.syncHomePageReader: unknown query field "+name+"; no page returned; use the published sync view query fields", "sync_page_query_invalid")
			return
		}
	}
	if h.reader == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "api.syncHomePageReader: store unavailable; no page returned; start Peasant with its normal store and retry", "sync_page_unavailable")
		return
	}
	r := h.newReader(query)
	var result any
	if query.View == "project-options" {
		result, err = r.projectOptions(req.Context())
	} else {
		var page schema.LocalSyncSessionPage
		page, err = r.ReadPage(req.Context())
		if err == nil {
			err = page.Validate()
		}
		result = page
	}
	if err != nil {
		var refusal *syncPageError
		if errors.As(err, &refusal) {
			status := http.StatusBadRequest
			if refusal.code == "home_parent_not_found" {
				status = http.StatusNotFound
			}
			writeAPIError(w, status, err.Error(), refusal.code)
			return
		}
		writeDiscoveryError(w, "sync Home projection failed", err)
		return
	}
	_ = json.NewEncoder(w).Encode(result)
}
