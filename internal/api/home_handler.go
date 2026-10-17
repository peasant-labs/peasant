package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

type homeReader interface {
	ingest.PublicationMetadataReader
	HomeCandidates(context.Context, string, string) ([]store.HomeCandidate, error)
	HomePageRows(context.Context, []string) ([]store.HomePageRow, error)
	SyncParentIdentities(context.Context, []ingest.PushSessionRow) ([]store.ParentIdentityRow, error)
	FirstUserMessageBulk(context.Context, []string) (map[string]string, error)
	GroupingEvidenceForSessions(context.Context, []string) (map[string]store.GroupingEvidenceRow, error)
}

var _ homeReader = (*store.Store)(nil)

type homeHandler struct {
	reader     homeReader
	sync       *syncHandler
	publishing *publishingHandler
	resolver   ingest.RepositoryIdentityResolver
}

func parseHomeQuery(values url.Values) (schema.LocalHomeSessionsQuery, error) {
	q := schema.LocalHomeSessionsQuery{}
	for key, list := range values {
		if len(list) != 1 {
			return q, fmt.Errorf("query %s repeats; supply it once", key)
		}
		v := list[0]
		switch key {
		case "limit", "offset":
			n, err := strconv.Atoi(v)
			if err != nil {
				return q, fmt.Errorf("query %s must be an integer: %w", key, err)
			}
			if key == "limit" {
				q.Limit = &n
			} else {
				q.Offset = &n
			}
		case "parentSessionId":
			q.ParentSessionID = &v
		case "projectHash":
			hash, err := schema.NewProjectHash(v)
			if err != nil {
				return q, err
			}
			q.ProjectHash = &hash
		case "publication":
			filter := schema.LocalHomePublicationFilter(v)
			q.Publication = &filter
		default:
			return q, fmt.Errorf("unknown Home query %q; use limit, offset, parentSessionId, projectHash or publication", key)
		}
	}
	return q, q.Validate()
}

func (h *homeHandler) handleSessions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	q, err := parseHomeQuery(r.URL.Query())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "Home page query rejected before reading sessions: "+err.Error()+"; correct the query and retry.", "home_query_invalid")
		return
	}
	if h.reader == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "Home sessions cannot be read because the local store is unavailable; start Peasant with its normal store and retry.", "home_unavailable")
		return
	}
	page, status, err := h.page(r.Context(), q)
	if err != nil {
		if status == http.StatusNotFound {
			writeAPIError(w, status, "Home children could not be listed because the parent is unknown or outside the saved selection; choose a visible parent and retry.", "home_parent_not_found")
			return
		}
		writeDiscoveryError(w, "Home sessions could not be read; no page returned; retry or inspect the store", err)
		return
	}
	if err := page.Validate(); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "Home page violates the Local API contract: "+err.Error()+"; no page returned; inspect the store and retry.", "home_contract_breach")
		return
	}
	_ = json.NewEncoder(w).Encode(page)
}

func (h *homeHandler) page(ctx context.Context, q schema.LocalHomeSessionsQuery) (schema.LocalHomeSessionsPage, int, error) {
	limit, offset := 25, 0
	if q.Limit != nil {
		limit = *q.Limit
	}
	if q.Offset != nil {
		offset = *q.Offset
	}
	page := schema.LocalHomeSessionsPage{Rows: make([]schema.LocalHomeSessionRow, 0), Limit: limit, Offset: offset}
	creds := h.publishing.signedIn()
	origin, owner := "", ""
	if creds != nil {
		origin, owner = creds.VillageURL, creds.UserID
	}
	cohort, err := h.reader.HomeCandidates(ctx, origin, owner)
	if err != nil {
		return page, 0, err
	}
	cfg := h.sync.currentConfig()
	policy, err := syncSelectionPolicy(cfg)
	if err != nil {
		return page, 0, err
	}
	mode, matcher, err := policy.ProjectionInputs()
	if err != nil {
		return page, 0, err
	}
	identityRows := make([]store.SessionRow, len(cohort))
	for i := range cohort {
		identityRows[i] = cohort[i].Identity
	}
	var prepared []preparedWebDiscoveryIdentity
	var remotes, names []ingest.DiscoveryIdentityMultiplicity
	if mode != config.SelectionModeAll {
		prepared, remotes, names, err = prepareWebDiscoveryIdentities(ctx, identityRows, cfg.Selection, h.resolver)
		if err != nil {
			return page, 0, err
		}
	}
	visible := make(map[string]bool, len(cohort))
	pushRows := make([]ingest.PushSessionRow, 0, len(cohort))
	for i, candidate := range cohort {
		if err := ctx.Err(); err != nil {
			return page, 0, err
		}
		row := candidate.Identity
		selected := true
		if matcher != nil {
			match := matcher.MatchBranchCandidate(ingest.DiscoveryCandidate{SessionID: ingest.SessionID(row.SessionID), Harness: defaults.Harness(row.ModelHarness), GitRemote: value(row.CanonicalRemote), ProjectName: row.ProjectName, ClonePath: prepared[i].clonePath, Branch: value(row.GitBranch), RemoteMultiplicity: remotes[i], NameMultiplicity: names[i]})
			switch match {
			case ingest.BranchMatchYes:
				selected = true
			case ingest.BranchMatchNo:
				selected = false
			default:
				// Policy.Visible returns the canonical discovery error for a
				// conflicting selection, preserving existing recovery status/copy.
				_, err = policy.Visible(h.syncCandidate(row))
				if err == nil {
					err = fmt.Errorf("Home selection matcher returned an unsupported result for %q; run peasant kickstart and retry", row.SessionID)
				}
				return page, 0, err
			}
		}
		if candidate.Pushable {
			visible[row.SessionID] = selected
			pushRows = append(pushRows, ingest.PushSessionRow{SessionID: row.SessionID, ParentID: value(row.ParentID)})
		}
	}
	parents, err := h.reader.SyncParentIdentities(ctx, pushRows)
	if err != nil {
		return page, 0, err
	}
	parentByID := make(map[string]store.ParentIdentityRow, len(parents))
	children := make(map[string]int)
	for _, parent := range parents {
		if err := ctx.Err(); err != nil {
			return page, 0, err
		}
		if _, duplicate := parentByID[parent.SessionID]; duplicate {
			return page, 0, fmt.Errorf("Home parent evidence repeats session %q; repair the store and retry", parent.SessionID)
		}
		parentByID[parent.SessionID] = parent
		if visible[parent.SessionID] && !parent.ParentUnresolved && parent.ParentIdentity != "" {
			children[parent.ParentIdentity]++
		}
	}
	if q.ParentSessionID != nil && !visible[*q.ParentSessionID] {
		return page, http.StatusNotFound, fmt.Errorf("parent not visible")
	}
	filter := schema.LocalHomePublicationAll
	if q.Publication != nil {
		filter = *q.Publication
	}
	var auto func(string) bool
	if filter == schema.LocalHomePublicationAuto {
		auto = h.publishing.autoPublicationChecker(ctx)
	}
	matches := make([]store.HomeCandidate, 0)
	for _, candidate := range cohort {
		if err := ctx.Err(); err != nil {
			return page, 0, err
		}
		row := candidate.Identity
		if !visible[row.SessionID] {
			continue
		}
		parent, ok := parentByID[row.SessionID]
		if !ok {
			return page, 0, fmt.Errorf("Home parent evidence missing for %q; repair the store and retry", row.SessionID)
		}
		if q.ParentSessionID != nil {
			if parent.ParentUnresolved || parent.ParentIdentity != *q.ParentSessionID {
				continue
			}
		} else {
			if parent.ParentUnresolved || parent.ParentIdentity != "" {
				continue
			}
			if q.ProjectHash != nil && row.ProjectHash != q.ProjectHash.String() {
				continue
			}
			wrongPublished := filter == schema.LocalHomePublicationPublished && !candidate.Published
			wrongUnpublished := filter == schema.LocalHomePublicationNotPublished && candidate.Published
			if wrongPublished || wrongUnpublished {
				continue
			}
			if auto != nil && !auto(row.GitWorktree) {
				continue
			}
		}
		matches = append(matches, candidate)
	}
	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i].Identity, matches[j].Identity
		if a.StartMs != b.StartMs {
			return a.StartMs > b.StartMs
		}
		return a.SessionID < b.SessionID
	})
	if err := ctx.Err(); err != nil {
		return page, 0, err
	}
	if offset >= len(matches) {
		return page, 0, nil
	}
	end := offset + min(limit, len(matches)-offset)
	if end < len(matches) {
		page.NextOffset = &end
	}
	ids := make([]string, 0, end-offset)
	scope := make(map[string]bool, end-offset)
	for _, candidate := range matches[offset:end] {
		id := candidate.Identity.SessionID
		ids = append(ids, id)
		scope[id] = true
	}
	rows, err := h.reader.HomePageRows(ctx, ids)
	if err != nil {
		return page, 0, err
	}
	byID := make(map[string]store.HomePageRow, len(rows))
	hydrated := make([]ingest.PushSessionRow, 0, len(rows))
	for _, row := range rows {
		byID[row.Sync.SessionID] = row
		hydrated = append(hydrated, row.Sync)
	}
	metadata, err := push.LoadPublicationMetadata(ctx, h.reader, hydrated)
	if err != nil {
		return page, 0, err
	}
	previews, err := h.reader.FirstUserMessageBulk(ctx, ids)
	if err != nil {
		return page, 0, err
	}
	evidence, err := h.reader.GroupingEvidenceForSessions(ctx, ids)
	if err != nil {
		return page, 0, err
	}
	publications, err := h.publishing.localPublications(ctx, ids, scope, creds)
	if err != nil {
		return page, 0, err
	}
	publicationByID := make(map[string]schema.LocalPublication, len(publications))
	for _, p := range publications {
		publicationByID[p.SessionID] = p
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return page, 0, err
		}
		row, ok := byID[id]
		if !ok {
			return page, 0, fmt.Errorf("Home page session %q changed while reading; refresh the page", id)
		}
		input := metadata[id]
		status, hold := computeSyncStatus(row.Sync, false, push.PublicationMetadataReady(input))
		_, summary, err := buildSyncGroupedRow(syncSessionEntry{row: row.Sync, status: status, hold: hold, identity: parentByID[id]}, evidence[id], previews[id])
		if err != nil {
			return page, 0, err
		}
		page.Rows = append(page.Rows, schema.LocalHomeSessionRow{Sync: summary, Publication: publicationByID[id], Title: row.Title, Preview: previews[id], ChildCount: children[id]})
	}
	return page, 0, nil
}

func (h *homeHandler) syncCandidate(row store.SessionRow) sessionvisibility.Candidate {
	return syncVisibilityCandidate(ingest.PushSessionRow{SessionID: row.SessionID, ModelHarness: row.ModelHarness, GitRemote: value(row.CanonicalRemote), ProjectName: row.ProjectName, ProjectPath: row.GitWorktree, GitBranch: row.GitBranch})
}
