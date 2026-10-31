package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

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
	HomeIdentityCohort(context.Context) ([]store.HomeCandidate, error)
	SyncHomeWindow(context.Context, *store.SyncPageBoundary, int, store.SyncWindowFilter) (store.SyncCandidateWindow, error)
	HomePageRows(context.Context, []string) ([]store.HomePageRow, error)
	SyncParentIdentities(context.Context, []ingest.PushSessionRow) ([]store.ParentIdentityRow, error)
	FirstUserMessageBulk(context.Context, []string) (map[string]string, error)
	GroupingEvidenceForSessions(context.Context, []string) (map[string]store.GroupingEvidenceRow, error)
	ClaudeTeammates(context.Context, []string) (map[string]schema.SessionTeammate, error)
}

var _ homeReader = (*store.Store)(nil)

type homeHandler struct {
	reader     homeReader
	sync       *syncHandler
	publishing *publishingHandler
	resolver   ingest.RepositoryIdentityResolver
}

const syncPageScopeCode = "sync_page_scope_changed"
const syncPageTokenCode = "sync_page_token_invalid"

type syncPageError struct{ code, message string }

func (e *syncPageError) Error() string { return e.message }
func pageTokenError(code, reason string) error {
	return &syncPageError{code: code, message: "api.syncHomePageReader.ReadPage: " + reason + "; no page is usable; discard continuation and restart the sync Home traversal"}
}

// The token is an opaque order position and scope digest, not a credential.
// No private path, API key, server-side cursor or snapshot is retained.
type syncPageToken struct {
	Version int    `json:"v"`
	Scope   string `json:"scope"`
	StartMs int64  `json:"startMs"`
	ID      string `json:"id"`
}

type syncHomePageReader struct {
	db         homeReader
	query      schema.LocalSyncSessionsQuery
	config     *config.Config
	publishing *publishingHandler
	account    *auth.Credentials
	resolver   ingest.RepositoryIdentityResolver
	scope      string
}

var _ schema.PageReader[schema.LocalSyncSessionRow] = (*syncHomePageReader)(nil)

func (h *homeHandler) newReader(q schema.LocalSyncSessionsQuery) *syncHomePageReader {
	r := &syncHomePageReader{db: h.reader, query: q, config: h.sync.currentConfig(), publishing: h.publishing, account: h.publishing.signedIn(), resolver: h.resolver}
	filter := schema.LocalSyncPublicationAll
	if q.Publication != nil {
		filter = *q.Publication
	}
	origin, owner := r.accountScope()
	// Immutable request configuration. The saved kickstart selection is
	// deliberately absent: it controls future ingestion and hook/CLI push, never
	// which ingested rows this page may show, so changing it must not invalidate
	// a browsing continuation. MaxItems is intentionally absent, so callers can
	// change their page size.
	encoded, _ := json.Marshal(struct {
		Resource, Order, Parent, Project, Publication, Origin, Owner string
	}{"sync/home", "start-desc-id-asc/v1", value(q.ParentSessionID), projectValue(q.ProjectHash), filter.String(), origin, owner})
	sum := sha256.Sum256(encoded)
	r.scope = hex.EncodeToString(sum[:])
	return r
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

func (r *syncHomePageReader) boundary(token schema.PageToken) (*store.SyncPageBoundary, error) {
	if token == "" {
		return nil, nil
	}
	if len(token) > 4096 {
		return nil, pageTokenError(syncPageTokenCode, "pageToken exceeds 4096 bytes")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(string(token))
	if err != nil {
		return nil, pageTokenError(syncPageTokenCode, "pageToken is not valid base64url")
	}
	var decoded struct {
		Version *int    `json:"v"`
		Scope   *string `json:"scope"`
		StartMs *int64  `json:"startMs"`
		ID      *string `json:"id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, pageTokenError(syncPageTokenCode, "pageToken shape is malformed")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, pageTokenError(syncPageTokenCode, "pageToken contains trailing values")
	}
	missingScope := decoded.Version == nil || decoded.Scope == nil
	missingBoundary := decoded.StartMs == nil || decoded.ID == nil
	if missingScope || missingBoundary {
		return nil, pageTokenError(syncPageTokenCode, "pageToken is missing its order boundary or scope")
	}
	badIDLength := len(*decoded.ID) == 0 || len(*decoded.ID) > 1024
	badIDText := strings.TrimSpace(*decoded.ID) != *decoded.ID || strings.ContainsAny(*decoded.ID, "/\\\x00\r\n")
	if *decoded.Version != 1 || badIDLength || badIDText {
		return nil, pageTokenError(syncPageTokenCode, "pageToken version or boundary is unsupported")
	}
	scopeBytes, err := hex.DecodeString(*decoded.Scope)
	if err != nil || len(scopeBytes) != sha256.Size {
		return nil, pageTokenError(syncPageTokenCode, "pageToken scope digest is malformed")
	}
	canonical, _ := json.Marshal(syncPageToken{Version: *decoded.Version, Scope: *decoded.Scope, StartMs: *decoded.StartMs, ID: *decoded.ID})
	if !bytes.Equal(raw, canonical) {
		return nil, pageTokenError(syncPageTokenCode, "pageToken encoding or shape is unsupported")
	}
	if *decoded.Scope != r.scope {
		return nil, pageTokenError(syncPageScopeCode, "pageToken scope no longer matches account, parent, project or publication filter")
	}
	return &store.SyncPageBoundary{StartMs: *decoded.StartMs, ID: *decoded.ID}, nil
}
func (r *syncHomePageReader) token(boundary store.SyncPageBoundary) schema.PageToken {
	raw, _ := json.Marshal(syncPageToken{Version: 1, Scope: r.scope, StartMs: boundary.StartMs, ID: boundary.ID})
	return schema.PageToken(base64.RawURLEncoding.EncodeToString(raw))
}

// syncHomeCohort is per-read metadata only. Complete clone multiplicity and
// exact ancestry/counts remain explicit O(N) work, separate from window paging.
type syncHomeCohort struct {
	rows     map[string]store.HomeCandidate
	ordered  []store.SessionRow
	visible  map[string]bool
	parents  map[string]store.ParentIdentityRow
	children map[string]int
}

func (r *syncHomePageReader) cohort(ctx context.Context) (syncHomeCohort, error) {
	c := syncHomeCohort{rows: map[string]store.HomeCandidate{}, visible: map[string]bool{}, parents: map[string]store.ParentIdentityRow{}, children: map[string]int{}}
	rows, err := r.db.HomeIdentityCohort(ctx)
	if err != nil {
		return c, err
	}
	pushRows := make([]ingest.PushSessionRow, 0, len(rows))
	for _, candidate := range rows {
		if err := ctx.Err(); err != nil {
			return c, err
		}
		row := candidate.Identity
		c.rows[row.SessionID] = candidate
		c.ordered = append(c.ordered, row)
		if candidate.Pushable {
			// The saved kickstart selection is not a browsing predicate: every
			// ingested, pushable session belongs to the page, whatever the saved
			// selection says about future ingest or hook pushes.
			c.visible[row.SessionID] = true
			pushRows = append(pushRows, ingest.PushSessionRow{SessionID: row.SessionID, ParentID: value(row.ParentID)})
		}
	}
	parents, err := r.db.SyncParentIdentities(ctx, pushRows)
	if err != nil {
		return c, err
	}
	for _, parent := range parents {
		if err := ctx.Err(); err != nil {
			return c, err
		}
		if _, exists := c.parents[parent.SessionID]; exists {
			return c, fmt.Errorf("api.syncHomePageReader: duplicate parent evidence for %q; no page returned; repair the store and retry", parent.SessionID)
		}
		c.parents[parent.SessionID] = parent
		if c.visible[parent.SessionID] && !parent.ParentUnresolved && parent.ParentIdentity != "" {
			c.children[parent.ParentIdentity]++
		}
	}
	return c, nil
}

// selectionMembership computes the saved INGEST/PUSH selector's verdict for
// requested IDs, using the complete cohort as repository identity evidence.
// Only publication metadata and the explicit publication=auto filter need it;
// project options and selection-free browse pages never evaluate it.
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

func (c syncHomeCohort) matches(id, parent string) bool {
	if !c.visible[id] {
		return false
	}
	p, ok := c.parents[id]
	return ok && !p.ParentUnresolved && p.ParentIdentity == parent
}

func (r *syncHomePageReader) ReadPage(ctx context.Context, request schema.PageRequest) (schema.Page[schema.LocalSyncSessionRow], error) {
	page := schema.Page[schema.LocalSyncSessionRow]{Items: make([]schema.LocalSyncSessionRow, 0)}
	if request.MaxItems < 0 {
		return page, pageTokenError(syncPageTokenCode, "MaxItems is negative; supply zero/default or a positive maximum")
	}
	maximum := request.MaxItems
	if maximum == 0 {
		maximum = 25
	}
	maximum = min(maximum, 100)
	boundary, err := r.boundary(request.Token)
	if err != nil {
		return page, err
	}
	cohort, err := r.cohort(ctx)
	if err != nil {
		return page, err
	}
	parent := value(r.query.ParentSessionID)
	if parent != "" && !cohort.visible[parent] {
		return page, &syncPageError{code: "home_parent_not_found", message: "api.syncHomePageReader: parent is not a stored pushable session; no child identities returned; open a parent from the current Home list and retry"}
	}
	filter := schema.LocalSyncPublicationAll
	if r.query.Publication != nil {
		filter = *r.query.Publication
	}
	origin, owner := r.accountScope()
	window, err := r.db.SyncHomeWindow(ctx, boundary, 4*maximum, store.SyncWindowFilter{ProjectHash: projectValue(r.query.ProjectHash), Origin: origin, Owner: owner, Publication: filter})
	if err != nil {
		return page, err
	}
	var auto func(string) bool
	var membership map[string]bool
	if filter == schema.LocalSyncPublicationAuto {
		auto = r.publishing.autoPublicationChecker(ctx)
		autoIDs := make([]string, 0, len(window.Candidates))
		for _, candidate := range window.Candidates {
			if cohort.matches(candidate.ID, parent) {
				autoIDs = append(autoIDs, candidate.ID)
			}
		}
		membership, err = r.selectionMembership(ctx, cohort, autoIDs)
		if err != nil {
			return page, err
		}
	}
	ids := make([]string, 0, maximum)
	for i, candidate := range window.Candidates {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		if cohort.matches(candidate.ID, parent) && (auto == nil || (membership[candidate.ID] && auto(cohort.rows[candidate.ID].Identity.GitWorktree))) {
			ids = append(ids, candidate.ID)
		}
		// Only consumed candidates advance traversal. An empty/short eligible
		// page can continue through the next raw metadata window.
		if i+1 < len(window.Candidates) || window.More {
			page.Next = r.token(candidate)
		} else {
			page.Next = ""
		}
		if len(ids) == maximum {
			break
		}
	}
	if len(ids) == 0 {
		return page, nil
	}
	page.Items, err = r.hydrate(ctx, ids, cohort, membership)
	return page, err
}

func (r *syncHomePageReader) hydrate(ctx context.Context, ids []string, cohort syncHomeCohort, membership map[string]bool) ([]schema.LocalSyncSessionRow, error) {
	rows, err := r.db.HomePageRows(ctx, ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]store.HomePageRow, len(rows))
	hydrated := make([]ingest.PushSessionRow, 0, len(rows))
	for _, row := range rows {
		byID[row.Sync.SessionID] = row
		hydrated = append(hydrated, row.Sync)
	}
	// Publication metadata describes the saved INGEST/PUSH selector, which is a
	// different question from whether the row may appear here. Membership uses
	// the complete cohort to resolve repository ambiguity; only returned IDs are
	// passed to publication hydration. An unselected row remains browsable.
	if membership == nil {
		membership, err = r.selectionMembership(ctx, cohort, ids)
		if err != nil {
			return nil, err
		}
	}
	scope := make(map[string]bool, len(ids))
	for _, id := range ids {
		inSelection, known := membership[id]
		if !known {
			return nil, fmt.Errorf("api.syncHomePageReader.hydrate: session %q has no saved ingest/push selection membership, so outsideSelection cannot be reported truthfully; no page was returned; retry after verifying the local store and saved selection", id)
		}
		scope[id] = inSelection
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
	publications, err := r.publishing.localPublications(ctx, ids, scope, r.account)
	if err != nil {
		return nil, err
	}
	publicationByID := map[string]schema.LocalPublication{}
	for _, p := range publications {
		publicationByID[p.SessionID] = p
	}
	items := make([]schema.LocalSyncSessionRow, 0, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("api.syncHomePageReader: session %q changed during hydration; no page returned; restart traversal", id)
		}
		status, hold := computeSyncStatus(row.Sync, false, push.PublicationMetadataReady(metadata[id]))
		_, summary, err := buildSyncGroupedRow(syncSessionEntry{row: row.Sync, status: status, hold: hold, identity: cohort.parents[id]}, evidence[id], previews[id], teammateFor(teammates, id))
		if err != nil {
			return nil, err
		}
		items = append(items, schema.LocalSyncSessionRow{Sync: summary, Publication: publicationByID[id], Title: row.Title, Preview: previews[id], ChildCount: cohort.children[id]})
	}
	return items, nil
}

func (r *syncHomePageReader) projectOptions(ctx context.Context) (schema.LocalSyncProjectOptions, error) {
	options := schema.LocalSyncProjectOptions{Projects: make([]schema.LocalSyncProjectOption, 0)}
	cohort, err := r.cohort(ctx)
	if err != nil {
		return options, err
	}
	seen := map[string]bool{}
	for id, candidate := range cohort.rows {
		if err := ctx.Err(); err != nil {
			return options, err
		}
		row := candidate.Identity
		if !cohort.matches(id, "") || seen[row.ProjectHash] {
			continue
		}
		hash, err := schema.NewProjectHash(row.ProjectHash)
		if err != nil {
			return options, err
		}
		seen[row.ProjectHash] = true
		options.Projects = append(options.Projects, schema.LocalSyncProjectOption{ProjectHash: hash, ProjectName: row.ProjectName})
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
		case "view", "maxItems", "pageToken", "parentSessionId", "projectHash", "publication":
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
		page, err = r.ReadPage(req.Context(), query.PageRequest())
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
