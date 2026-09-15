package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// groupedViewValue is the only accepted value of the existing view parameter.
// Omission keeps every legacy flat route exactly as it was.
const groupedViewValue = "grouped"

// groupedMembersRoute is the registered local helper member operation. It is
// the single member endpoint owner for every grouped local route variant.
const groupedMembersRoute = "/api/v1/session-groups/{groupId}/members"

// RegisterGroupedRouteVariant installs a route owner's predicate and row
// callback so the shared member operation can replay it. Registration is
// explicit: a variant with no source stays unserviceable and returns an
// actionable refusal rather than an empty page.
func (s *Server) RegisterGroupedRouteVariant(source GroupedVariantSource) error {
	if !source.Variant.IsValid() {
		return fmt.Errorf("api.Server.RegisterGroupedRouteVariant: route variant %q is outside the closed set; no grouped source was registered and its member scope cannot be replayed; register one of the published variants", source.Variant)
	}
	if source.Gather == nil {
		return fmt.Errorf("api.Server.RegisterGroupedRouteVariant: route variant %q registered no candidate callback; a member fetch could not replay the originating predicate; supply the route's Gather callback", source.Variant)
	}
	s.groupedMu.Lock()
	defer s.groupedMu.Unlock()
	if s.groupedVariants == nil {
		s.groupedVariants = make(map[GroupedRouteVariant]GroupedVariantSource)
	}
	s.groupedVariants[source.Variant] = source
	return nil
}

// groupedSource returns the registered source for a variant.
func (s *Server) groupedSource(variant GroupedRouteVariant) (GroupedVariantSource, bool) {
	s.groupedMu.RLock()
	defer s.groupedMu.RUnlock()
	source, ok := s.groupedVariants[variant]
	return source, ok
}

// registerProviderGroupedVariants installs the store provider's session-list
// and search predicates. It runs during Listen so a production server serves
// grouped views without any additional wiring.
func (s *Server) registerProviderGroupedVariants() {
	provider, ok := s.cfg.Provider.(groupedCandidateProvider)
	if !ok {
		return
	}
	s.groupedRevision = provider.GroupedScopeRevision()
	_ = s.RegisterGroupedRouteVariant(GroupedVariantSource{Variant: GroupedRouteSessions, Gather: provider.GroupedCandidates})
	_ = s.RegisterGroupedRouteVariant(GroupedVariantSource{Variant: GroupedRouteSearch, Gather: provider.GroupedCandidates})
}

// serveGroupedSessions answers GET /api/v1/sessions?view=grouped.
func (s *Server) serveGroupedSessions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	source, ok := s.groupedSource(GroupedRouteSessions)
	if !ok {
		writeAPIError(w, http.StatusServiceUnavailable,
			"Grouped sessions could not be listed because no grouped session source is registered in api.Server.serveGroupedSessions, which ran while the grouped view was requested. No rows were returned, because an unregistered route cannot be replayed for member expansion. Omit view=grouped to use the flat list, or restart Peasant with its store provider configured, then retry.",
			"grouped_unavailable")
		return
	}
	filters := GroupedFilters{Variant: GroupedRouteSessions}
	// project is opt-in and scopes the WHOLE grouped response, not just the
	// visible page: the candidate set, the ordinary/helper counts and every
	// issued member scope replay one project. Omission keeps the legacy
	// cross-project list, and a malformed value is refused rather than widened.
	if raw := r.URL.Query().Get("project"); raw != "" {
		project, projectErr := schema.NewProjectHash(raw)
		if projectErr != nil {
			writeAPIError(w, http.StatusBadRequest,
				fmt.Sprintf("Grouped sessions could not be listed because query field \"project\" is not the opaque 64-character project hash in internal/api.serveGroupedSessions. No rows were returned, because a malformed project filter cannot be matched to a project and must not widen the grouped list to every project. Send the projectHash from the project route, or omit project for the cross-project list, then retry: %v", projectErr),
				"grouped_project_invalid")
			return
		}
		filters.ProjectHash = project
	}
	candidates, err := source.Gather(r.Context(), filters)
	if err != nil {
		writeDiscoveryError(w, "failed to fetch grouped sessions", err)
		return
	}
	payload, err := buildGroupedListPayload(filters, candidates, 1, 0, s)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError,
			"Grouped sessions could not be assembled because the grouped projection failed in internal/api.serveGroupedSessions after the session rows were read. No grouped response was returned, because a partial list would misstate the counts. Retry the grouped list; if the failure repeats, inspect the Peasant server logs.",
			"grouped_projection_failure")
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// serveGroupedSearch answers GET /api/v1/search?q=...&view=grouped.
func (s *Server) serveGroupedSearch(w http.ResponseWriter, r *http.Request, query string, limit int) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	source, ok := s.groupedSource(GroupedRouteSearch)
	if !ok {
		writeAPIError(w, http.StatusServiceUnavailable,
			"Grouped search could not run because no grouped search source is registered in api.Server.serveGroupedSearch, which ran while the grouped view was requested. No results were returned, because an unregistered route cannot be replayed for member expansion. Omit view=grouped to use the flat search, or restart Peasant with its store provider configured, then retry.",
			"grouped_unavailable")
		return
	}
	filters := GroupedFilters{Variant: GroupedRouteSearch, SearchQuery: query, SearchLimit: limit}
	candidates, err := source.Gather(r.Context(), filters)
	if err != nil {
		writeDiscoveryError(w, "failed to fetch grouped search results", err)
		return
	}
	payload, err := buildGroupedListPayload(filters, candidates, 1, 0, s)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError,
			"Grouped search results could not be assembled because the grouped projection failed in internal/api.serveGroupedSearch after the matching sessions were read. No grouped response was returned, because a partial list would misstate the counts. Retry the search; if the failure repeats, inspect the Peasant server logs.",
			"grouped_projection_failure")
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// handleHelperGroupMembers answers the registered local member operation. It
// validates the opaque scope against the viewer and group, replays the exact
// originating predicate with current authorization and selection, then pages
// the group's saved helpers. It never widens the originating scope.
func (s *Server) handleHelperGroupMembers(w http.ResponseWriter, r *http.Request) {
	request, ok := s.exactRequestGuard(w, r, exactRequestSpec{
		operation: "helper group member",
		provider:  true,
		query:     map[string]bool{"scope": true, "page": false, "limit": false},
	})
	if !ok {
		return
	}
	groupID := r.PathValue("groupId")
	if groupID == "" {
		writeAPIError(w, http.StatusBadRequest,
			"Helper group members could not be listed because the groupId path value is missing in internal/api.handleHelperGroupMembers. No members were returned, because the scope cannot be matched to a group. Open the group from a current grouped list so the route carries its groupId, then retry.",
			"grouped_group_missing")
		return
	}

	entry, live := s.lookupMemberScope(request.query.Get("scope"))
	if !live {
		s.writeGroupedScopeExpired(w, groupID)
		return
	}
	if entry.GroupID != groupID {
		s.writeGroupedScopeExpired(w, groupID)
		return
	}
	if entry.Revision != s.groupedRevision {
		s.writeGroupedScopeExpired(w, groupID)
		return
	}

	page, pageOK := parseGroupedPageValue(request.query, "page", 1)
	if !pageOK {
		writeAPIError(w, http.StatusBadRequest,
			"Helper group members could not be listed because query field \"page\" is not a positive integer in internal/api.handleHelperGroupMembers. No provider call was made and no members were returned. Provide one page number of at least 1, or remove the field, then retry.",
			"grouped_page_invalid")
		return
	}
	limit, limitOK := parseGroupedPageValue(request.query, "limit", groupedMembersDefaultLimit)
	if !limitOK {
		writeAPIError(w, http.StatusBadRequest,
			"Helper group members could not be listed because query field \"limit\" is not a positive integer in internal/api.handleHelperGroupMembers. No provider call was made and no members were returned. Provide one page size of at least 1, or remove the field, then retry.",
			"grouped_limit_invalid")
		return
	}

	source, registered := s.groupedSource(entry.Variant)
	if !registered {
		s.writeGroupedScopeExpired(w, groupID)
		return
	}
	candidates, err := source.Gather(r.Context(), entry.Filters)
	if err != nil {
		writeDiscoveryError(w, "failed to replay grouped member scope", err)
		return
	}
	payload, err := buildGroupedMembersPayload(groupID, entry.Filters, candidates, page, limit, s)
	switch {
	case errors.Is(err, errGroupedGroupUnknown):
		s.writeGroupedScopeExpired(w, groupID)
		return
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError,
			"Helper group members could not be assembled because the member projection failed in internal/api.handleHelperGroupMembers after the scope was validated. No member page was returned, because a partial page would misstate the total. Refresh the originating grouped list and retry; if the failure repeats, inspect the Peasant server logs.",
			"grouped_projection_failure")
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// writeGroupedScopeExpired is the ONE refusal for a member scope that is
// missing, expired, restarted, group-mismatched, selection-changed or no longer
// matching the replayed predicate. It never broadens and never returns an empty
// success page, so the caller refreshes the originating list.
func (s *Server) writeGroupedScopeExpired(w http.ResponseWriter, groupID string) {
	writeAPIError(w, http.StatusConflict,
		fmt.Sprintf("Helper group %s could not be expanded because its member scope is missing, expired, or no longer matches the originating list in internal/api.handleHelperGroupMembers. No members were returned and the request was not broadened to the whole library: the scope is a short-lived lookup token, not an access grant, and Peasant restarted or the persisted selection changed. Refresh the originating grouped list and expand the group from the fresh response.", groupID),
		"group_scope_expired")
}

// parseGroupedPageValue reads one optional positive-integer paging value. An
// absent or empty value returns the default; a non-positive or malformed value
// is refused so paging is never guessed.
func parseGroupedPageValue(values url.Values, name string, defaultValue int) (int, bool) {
	raw, present := values[name]
	if !present || raw[0] == "" {
		return defaultValue, true
	}
	parsed, err := strconv.Atoi(raw[0])
	if err != nil || parsed < 1 {
		return 0, false
	}
	return parsed, true
}
