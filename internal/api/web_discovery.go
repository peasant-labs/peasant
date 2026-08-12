package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
)

// WebDiscoverySelectionStatus is the private dashboard selection projection.
type WebDiscoverySelectionStatus string

const (
	WebDiscoverySelected   WebDiscoverySelectionStatus = "selected"
	WebDiscoveryUnselected WebDiscoverySelectionStatus = "unselected"
)

// WebDiscoveryItem contains only display-safe metadata needed to join a stored session.
type WebDiscoveryItem struct {
	SessionID       string                      `json:"sessionId"`
	LocationLabel   string                      `json:"locationLabel"`
	Branch          string                      `json:"branch"`
	SelectionStatus WebDiscoverySelectionStatus `json:"selectionStatus"`
}

type webDiscoveryPayload struct {
	Items []WebDiscoveryItem `json:"items"`
}

func (s *Server) handleWebDiscovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	if s.cfg.Store == nil || s.cfg.Config == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "Web discovery could not start because the local store or loaded configuration is unavailable in internal/api.handleWebDiscovery. No session metadata was returned. Restart Peasant with its normal store and configuration, then retry.", "web_discovery_unavailable")
		return
	}
	if len(r.URL.Query()) != 0 {
		writeAPIError(w, http.StatusBadRequest, "Web discovery could not start because this endpoint accepts no query fields in internal/api.handleWebDiscovery. No session metadata was returned. Remove the query string and retry.", "web_discovery_query_unsupported")
		return
	}
	items, err := buildWebDiscovery(r.Context(), s.cfg.Store, s.cfg.Config)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "web_discovery_projection_failed")
		return
	}
	json.NewEncoder(w).Encode(webDiscoveryPayload{Items: items})
}

func buildWebDiscovery(ctx context.Context, db *store.Store, cfg *config.Config) ([]WebDiscoveryItem, error) {
	policy, err := sessionvisibility.New(cfg.Selection)
	if err != nil {
		return nil, fmt.Errorf("Web discovery could not build selection status because the saved kickstart selection is invalid in internal/api.buildWebDiscovery. No partial metadata was returned. Run `peasant kickstart` to repair the selection, then retry: %w", err)
	}
	rows, err := db.AllSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("Web discovery could not read stored sessions in internal/api.buildWebDiscovery. No partial metadata was returned. Retry; if the failure repeats, inspect the Peasant server logs: %w", err)
	}
	labels := safeLocationLabels(rows)
	items := make([]WebDiscoveryItem, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for i := range rows {
		row := &rows[i]
		if row.SessionID == "" {
			return nil, fmt.Errorf("Web discovery found a stored session with no session ID in internal/api.buildWebDiscovery. No partial metadata was returned because exact joins would be unsafe. Repair or re-ingest the affected store row, then retry")
		}
		if _, duplicate := seen[row.SessionID]; duplicate {
			return nil, fmt.Errorf("Web discovery found duplicate stored session ID %q in internal/api.buildWebDiscovery. No partial metadata was returned because exact joins would be ambiguous. Repair the local store, then retry", row.SessionID)
		}
		seen[row.SessionID] = struct{}{}
		branch, remote := "", ""
		if row.GitBranch != nil {
			branch = *row.GitBranch
		}
		if row.CanonicalRemote != nil {
			remote = *row.CanonicalRemote
		}
		selected, matchErr := policy.Visible(sessionvisibility.Candidate{SessionID: ingest.SessionID(row.SessionID), Harness: defaults.Harness(row.ModelHarness), GitRemote: remote, ProjectName: row.ProjectName, ClonePath: resolvedClonePath(row), GitBranch: branch})
		if matchErr != nil {
			return nil, fmt.Errorf("Web discovery could not compute selection status for session %q through the canonical matcher in internal/api.buildWebDiscovery. No partial metadata was returned. Run `peasant kickstart` to repair conflicting selection rules, then retry: %w", row.SessionID, matchErr)
		}
		status := WebDiscoveryUnselected
		if selected {
			status = WebDiscoverySelected
		}
		items[i] = WebDiscoveryItem{SessionID: row.SessionID, LocationLabel: labels[i], Branch: branch, SelectionStatus: status}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].SessionID < items[j].SessionID })
	return items, nil
}

func resolvedClonePath(row *store.SessionRow) ingest.ClonePath {
	raw := row.GitWorktree
	if raw == "" && row.ProjectName != row.ProjectHash {
		raw = row.ProjectName
	}
	if raw == "" {
		return ""
	}
	path, err := ingest.NewPhysicalPathResolver().Resolve(raw)
	if err != nil {
		return ""
	}
	return path
}

func safeLocationLabels(rows []store.SessionRow) []string {
	base := make([]string, len(rows))
	pathSets := make(map[string]map[string]struct{})
	for i := range rows {
		label := "local repository"
		if rows[i].GitWorktree != "" {
			label = filepath.Base(filepath.Clean(rows[i].GitWorktree))
		}
		base[i] = label
		if pathSets[label] == nil {
			pathSets[label] = make(map[string]struct{})
		}
		pathSets[label][rows[i].GitWorktree] = struct{}{}
	}
	paths := make(map[string][]string, len(pathSets))
	for label, set := range pathSets {
		for value := range set {
			paths[label] = append(paths[label], value)
		}
		sort.Strings(paths[label])
	}
	labels := make([]string, len(rows))
	for i := range rows {
		variants := paths[base[i]]
		labels[i] = base[i]
		if len(variants) > 1 {
			index := sort.SearchStrings(variants, rows[i].GitWorktree)
			labels[i] = fmt.Sprintf("%s %d", base[i], index+1)
		}
	}
	return labels
}
