package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/schema"
)

// Map / Review REST handlers plus the project-summary picker
// endpoint.
//
// All endpoints follow the handleSessions conventions: nil-provider →
// 503, inline error JSON, camelCase payloads encoded straight from the
// github.com/peasant-labs/schema wire types. Path params come from r.PathValue; commit / path /
// file / branch arrive as query params. Error mapping:
//
//   - unknown projectHash (codemap.ErrProjectNotFound)  → 404
//   - unknown node path  (codemap.ErrNodeNotFound)      → 404
//   - unknown branch     (codemap.ErrBranchNotFound)    → 404
//   - project without a git repo on ChangeDetail
//     (codemap.ErrRepoNotFound)                          → 404
//   - missing required query param                       → 400
//   - anything else                                      → 500

// writeJSONError writes an inline JSON error body with Content-Type
// application/json (http.Error would stamp text/plain over the JSON string).
func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeAPIError(w, code, msg, "")
}

// writeMapError maps a provider error from the Map/Review surfaces to an
// HTTP status with an inline JSON body.
func writeMapError(w http.ResponseWriter, err error) {
	switch {
	case sessionvisibility.IsError(err):
		writeAPIError(w, http.StatusInternalServerError, "Map or changes data could not be loaded because the saved kickstart selection is invalid. No partial project data was returned. Run `peasant kickstart` to repair the selection, then retry the request.", "map_selection_visibility")
	case errors.Is(err, codemap.ErrProjectNotFound):
		writeAPIError(w, http.StatusNotFound, "The project could not be opened because its canonical hash is not in the local index. No project data was returned. Refresh project discovery and open a current link.", "map_project_not_found")
	case errors.Is(err, codemap.ErrProjectAmbiguous):
		writeAPIError(w, http.StatusConflict, "The project could not be opened because the legacy label matches more than one indexed project. No project data was returned. Return to the project picker and open the canonical hash link.", "map_project_ambiguous")
	case errors.Is(err, codemap.ErrProjectIdentityInvalid):
		writeAPIError(w, http.StatusBadRequest, "The project could not be opened because its route identity is malformed. No project data was returned. Use the canonical 64-character lowercase hexadecimal hash from project discovery.", "map_project_identity_invalid")
	case errors.Is(err, codemap.ErrNodeNotFound):
		writeAPIError(w, http.StatusNotFound, "The map node could not be opened because it is absent from the current project graph. No node detail was returned. Refresh the map and select an existing code area.", "map_node_not_found")
	case errors.Is(err, codemap.ErrBranchNotFound):
		writeAPIError(w, http.StatusNotFound, "The change could not be opened because its branch is absent from the current repository. No change detail was returned. Refresh changes and select an existing branch.", "map_branch_not_found")
	case errors.Is(err, codemap.ErrRepoNotFound):
		writeAPIError(w, http.StatusNotFound, "Changes could not be loaded because the indexed project no longer resolves to a Git repository. No change data was returned. Restore the repository path or rerun kickstart, then retry.", "map_repository_not_found")
	default:
		writeAPIError(w, http.StatusInternalServerError, "Map or changes data could not be loaded because the local provider failed while serving the request. No partial data was returned. Retry; if the failure repeats, inspect the Peasant server logs.", "map_provider_failure")
	}
}

type exactRequestSpec struct {
	operation   string
	projectHash bool
	provider    bool
	query       map[string]bool // true means required and nonempty
}

type exactRequest struct {
	projectHash schema.ProjectHash
	query       url.Values
}

// exactRequestGuard is the single trust boundary for the Map, Changes,
// discovery, and search REST handlers. It rejects malformed encoding, unknown
// fields, duplicate values (including duplicate empty values), absent required
// fields, malformed project hashes, and an unavailable provider before any
// provider method can run.
func (s *Server) exactRequestGuard(w http.ResponseWriter, r *http.Request, spec exactRequestSpec) (exactRequest, bool) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	request := exactRequest{}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "The "+spec.operation+" request could not start because its query string is malformed in internal/api.exactRequestGuard. No provider call was made. Correct the percent encoding and retry the request.", "request_query_malformed")
		return request, false
	}
	request.query = values
	queryNames := make([]string, 0, len(values))
	for name := range values {
		queryNames = append(queryNames, name)
	}
	sort.Strings(queryNames)
	for _, name := range queryNames {
		if _, allowed := spec.query[name]; !allowed {
			writeAPIError(w, http.StatusBadRequest, "The "+spec.operation+" request could not start because query field "+strconv.Quote(name)+" is not supported in internal/api.exactRequestGuard. No provider call was made. Remove the unknown field and retry with the documented query shape.", "request_query_unknown")
			return request, false
		}
		if len(values[name]) != 1 {
			recovery := "Provide exactly one value for the field and retry."
			if name == "limit" {
				recovery = "The limit must be a single positive integer; provide exactly one value or remove the field, then retry."
			}
			writeAPIError(w, http.StatusBadRequest, "The "+spec.operation+" request could not start because query field "+strconv.Quote(name)+" was provided more than once in internal/api.exactRequestGuard. No provider call was made. "+recovery, "request_query_duplicate")
			return request, false
		}
	}
	requiredNames := make([]string, 0, len(spec.query))
	for name, required := range spec.query {
		if required {
			requiredNames = append(requiredNames, name)
		}
	}
	sort.Strings(requiredNames)
	for _, name := range requiredNames {
		if entries, present := values[name]; !present || len(entries) != 1 || entries[0] == "" {
			writeAPIError(w, http.StatusBadRequest, "The "+spec.operation+" request could not start because required query field "+strconv.Quote(name)+" is missing or empty in internal/api.exactRequestGuard. No provider call was made. Provide one nonempty value for the field and retry.", "request_query_required")
			return request, false
		}
	}

	if spec.projectHash {
		projectHash := r.PathValue("projectHash")
		if projectHash == "" {
			writeAPIError(w, http.StatusBadRequest, "The "+spec.operation+" request could not start because the projectHash path value is missing in internal/api.exactRequestGuard. No provider call was made. Open the project from current discovery so the route contains its canonical 64-character lowercase hexadecimal hash, then retry.", "map_project_hash_missing")
			return request, false
		}
		validatedHash, hashErr := schema.NewProjectHash(projectHash)
		if hashErr != nil {
			writeAPIError(w, http.StatusBadRequest, "The "+spec.operation+" request could not start because the projectHash path value is malformed in internal/api.exactRequestGuard. No provider call was made. Open the project from current discovery and use its canonical 64-character lowercase hexadecimal hash, then retry.", "map_project_hash_invalid")
			return request, false
		}
		request.projectHash = validatedHash
	}
	if spec.provider && s.cfg.Provider == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "The "+spec.operation+" request could not start because no data provider was configured in internal/api.exactRequestGuard. No provider call was made and no data can be served. Restart Peasant with its store provider configured, then retry.", "provider_unavailable")
		return request, false
	}
	return request, true
}

// handleProjectSummaries serves GET /api/v1/projects/summary — the home
// picker rows. No path params; the only preconditions are a live provider.
func (s *Server) handleProjectSummaries(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.exactRequestGuard(w, r, exactRequestSpec{operation: "project summaries", provider: true, query: map[string]bool{}}); !ok {
		return
	}

	payload, err := s.cfg.Provider.ProjectSummaries(r.Context())
	if err != nil {
		writeDiscoveryError(w, "failed to fetch project summaries", err)
		return
	}
	json.NewEncoder(w).Encode(payload)
}

// handleProjectResolve serves one explicitly named project without returning
// the selection-filtered sibling list.
func (s *Server) handleProjectResolve(w http.ResponseWriter, r *http.Request) {
	request, ok := s.exactRequestGuard(w, r, exactRequestSpec{operation: "project resolution", provider: true, query: map[string]bool{"name": true}})
	if !ok {
		return
	}
	project := request.query.Get("name")
	payload, err := s.cfg.Provider.ResolveProject(r.Context(), project)
	if err != nil {
		writeMapError(w, err)
		return
	}
	json.NewEncoder(w).Encode(payload)
}

// handleMapGraph serves GET /api/v1/map/{projectHash}?commit=<sha>.
func (s *Server) handleMapGraph(w http.ResponseWriter, r *http.Request) {
	request, ok := s.exactRequestGuard(w, r, exactRequestSpec{operation: "map graph", projectHash: true, provider: true, query: map[string]bool{"commit": false}})
	if !ok {
		return
	}

	payload, err := s.cfg.Provider.MapGraph(r.Context(), request.projectHash, request.query.Get("commit"))
	if err != nil {
		writeMapError(w, err)
		return
	}
	json.NewEncoder(w).Encode(payload)
}

// handleMapNode serves GET /api/v1/map/{projectHash}/node?path=<id>.
// path is required.
func (s *Server) handleMapNode(w http.ResponseWriter, r *http.Request) {
	request, ok := s.exactRequestGuard(w, r, exactRequestSpec{operation: "map node", projectHash: true, provider: true, query: map[string]bool{"path": true}})
	if !ok {
		return
	}

	payload, err := s.cfg.Provider.MapNodeDetail(r.Context(), request.projectHash, request.query.Get("path"))
	if err != nil {
		writeMapError(w, err)
		return
	}
	json.NewEncoder(w).Encode(payload)
}

// handleMapTasks serves GET /api/v1/map/{projectHash}/tasks?file=<path>.
// file is optional ("" = all tasks).
func (s *Server) handleMapTasks(w http.ResponseWriter, r *http.Request) {
	request, ok := s.exactRequestGuard(w, r, exactRequestSpec{operation: "map tasks", projectHash: true, provider: true, query: map[string]bool{"file": false}})
	if !ok {
		return
	}

	payload, err := s.cfg.Provider.ProjectTasks(r.Context(), request.projectHash, request.query.Get("file"))
	if err != nil {
		writeMapError(w, err)
		return
	}
	json.NewEncoder(w).Encode(payload)
}

// handleReviewChanges serves GET /api/v1/review/{projectHash}.
func (s *Server) handleReviewChanges(w http.ResponseWriter, r *http.Request) {
	request, ok := s.exactRequestGuard(w, r, exactRequestSpec{operation: "changes list", projectHash: true, provider: true, query: map[string]bool{}})
	if !ok {
		return
	}

	payload, err := s.cfg.Provider.ReviewChanges(r.Context(), request.projectHash)
	if err != nil {
		writeMapError(w, err)
		return
	}
	json.NewEncoder(w).Encode(payload)
}

// handleReviewChange serves GET /api/v1/review/{projectHash}/change?branch=<name>.
// branch is required (branch names contain slashes, hence a query param).
func (s *Server) handleReviewChange(w http.ResponseWriter, r *http.Request) {
	request, ok := s.exactRequestGuard(w, r, exactRequestSpec{operation: "change detail", projectHash: true, provider: true, query: map[string]bool{"branch": true}})
	if !ok {
		return
	}

	payload, err := s.cfg.Provider.ChangeDetail(r.Context(), request.projectHash, request.query.Get("branch"))
	if err != nil {
		writeMapError(w, err)
		return
	}
	json.NewEncoder(w).Encode(payload)
}

// handleReviewDiff serves
// GET /api/v1/review/{projectHash}/diff?branch=<name>&file=<path> — the lazy
// per-file rendered diff. Both branch and file are required (both contain
// slashes, hence query params).
func (s *Server) handleReviewDiff(w http.ResponseWriter, r *http.Request) {
	request, ok := s.exactRequestGuard(w, r, exactRequestSpec{operation: "change diff", projectHash: true, provider: true, query: map[string]bool{"branch": true, "file": true}})
	if !ok {
		return
	}

	payload, err := s.cfg.Provider.ChangeDiff(r.Context(), request.projectHash, request.query.Get("branch"), request.query.Get("file"))
	if err != nil {
		writeMapError(w, err)
		return
	}
	json.NewEncoder(w).Encode(payload)
}

// handleSearch serves GET /api/v1/search?q=<query>&limit=<n> — global
// full-text search over recorded message entries (the Cmd-K "Messages" group).
// No path param (search spans all projects); q is required and must be at
// least 2 characters. limit is optional; when present it must be one positive
// integer. The provider applies the documented maximum clamp to valid oversized
// values. There is no codemap sentinel for search, so any provider error → 500.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	request, ok := s.exactRequestGuard(w, r, exactRequestSpec{operation: "search", provider: true, query: map[string]bool{"q": true, "limit": false}})
	if !ok {
		return
	}

	query := request.query.Get("q")
	if len(strings.TrimSpace(query)) < 2 {
		writeAPIError(w, http.StatusBadRequest, "The search request could not start because query field \"q\" has fewer than two non-whitespace characters in internal/api.handleSearch. No provider call was made. Enter at least two characters and retry.", "search_query_too_short")
		return
	}

	limit := 0 // 0 => provider default
	if rawValues, present := request.query["limit"]; present {
		if len(rawValues) != 1 {
			writeAPIError(w, http.StatusBadRequest, "The search request could not start because query field \"limit\" was not a single value in internal/api.handleSearch. No provider call was made. Provide one positive integer or remove the field, then retry.", "search_limit_invalid")
			return
		}
		n, parseErr := strconv.Atoi(rawValues[0])
		if parseErr != nil || n <= 0 {
			writeAPIError(w, http.StatusBadRequest, "The search request could not start because the limit must be a positive integer; query field \"limit\" is invalid in internal/api.handleSearch. No provider call was made. Provide one positive integer or remove the field, then retry.", "search_limit_invalid")
			return
		}
		limit = n
	}

	payload, err := s.cfg.Provider.Search(r.Context(), query, limit)
	if err != nil {
		if sessionvisibility.IsError(err) {
			writeAPIError(w, http.StatusInternalServerError, "Search results could not be loaded because the saved kickstart selection is invalid. No partial search results were returned. Run `peasant kickstart` to repair the selection, then retry the search.", "search_selection_visibility")
		} else {
			writeAPIError(w, http.StatusInternalServerError, "Search results could not be loaded because the local search provider failed while serving the request. No partial search results were returned. Retry; if the failure repeats, inspect the Peasant server logs.", "search_provider_failure")
		}
		return
	}
	json.NewEncoder(w).Encode(payload)
}
