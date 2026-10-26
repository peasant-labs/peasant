package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionprojection"
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
	SessionID            string                      `json:"sessionId"`
	LocationLabel        string                      `json:"locationLabel"`
	RepositoryLocationID string                      `json:"repositoryLocationId"`
	Branch               string                      `json:"branch"`
	SelectionStatus      WebDiscoverySelectionStatus `json:"selectionStatus"`
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
	resolver := s.cfg.RepositoryIdentityResolver
	if resolver == nil {
		resolver = ingest.NewGitRepositoryIdentityResolver()
	}
	items, err := buildWebDiscovery(r.Context(), s.cfg.Store, s.cfg.Config, resolver)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), "web_discovery_projection_failed")
		return
	}
	json.NewEncoder(w).Encode(webDiscoveryPayload{Items: items})
}

func buildWebDiscovery(ctx context.Context, db *store.Store, cfg *config.Config, resolver ingest.RepositoryIdentityResolver) ([]WebDiscoveryItem, error) {
	policy, err := sessionvisibility.New(cfg.Selection)
	if err != nil {
		return nil, fmt.Errorf("Web discovery could not build selection status because the saved kickstart selection is invalid in internal/api.buildWebDiscovery. No partial metadata was returned. Run `peasant kickstart` to repair the selection, then retry: %w", err)
	}
	rows, err := db.AllSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("Web discovery could not read stored sessions in internal/api.buildWebDiscovery. No partial metadata was returned. Retry; if the failure repeats, inspect the Peasant server logs: %w", err)
	}
	mode, matcher, err := policy.ProjectionInputs()
	if err != nil {
		return nil, fmt.Errorf("Web discovery could not prepare the canonical selection matcher in internal/api.buildWebDiscovery. No partial metadata was returned. Run `peasant kickstart` to repair the selection, then retry: %w", err)
	}
	prepared, remoteMultiplicity, nameMultiplicity, err := prepareWebDiscoveryIdentities(ctx, rows, cfg.Selection, resolver)
	if err != nil {
		return nil, err
	}
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
		selected := mode == config.SelectionModeAll
		if matcher != nil {
			match := matcher.MatchBranchCandidate(ingest.DiscoveryCandidate{SessionID: ingest.SessionID(row.SessionID), Harness: defaults.Harness(row.ModelHarness), GitRemote: remote, ProjectName: row.ProjectName, ClonePath: prepared[i].clonePath, Branch: branch, RemoteMultiplicity: remoteMultiplicity[i], NameMultiplicity: nameMultiplicity[i]})
			switch match {
			case ingest.BranchMatchYes:
				selected = true
			case ingest.BranchMatchNo:
				selected = false
			case ingest.BranchMatchWithheldConflict:
				return nil, fmt.Errorf("Web discovery could not compute selection status for session %q because persisted project rules conflict in internal/api.buildWebDiscovery. No partial metadata was returned. Run `peasant kickstart`, make the repository branch rules consistent, then retry", row.SessionID)
			default:
				return nil, fmt.Errorf("Web discovery received an unknown canonical matcher result for session %q in internal/api.buildWebDiscovery. No partial metadata was returned. Update Peasant, then retry", row.SessionID)
			}
		}
		status := WebDiscoveryUnselected
		if selected {
			status = WebDiscoverySelected
		}
		presentationBranch := branch
		if prepared[i].unavailable {
			presentationBranch = ""
		}
		items[i] = WebDiscoveryItem{SessionID: row.SessionID, LocationLabel: prepared[i].label, RepositoryLocationID: prepared[i].locationID, Branch: presentationBranch, SelectionStatus: status}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].SessionID < items[j].SessionID })
	return items, nil
}

type preparedWebDiscoveryIdentity struct {
	clonePath         ingest.ClonePath
	locationID, label string
	unavailable       bool
}

// resolvedWebDiscoveryPath caches one physical directory's repository identity
// across every stored row recorded there. An empty cohort key means the
// directory is an unavailable location, never a resolved repository.
type resolvedWebDiscoveryPath struct {
	identity  preparedWebDiscoveryIdentity
	cohortKey ingest.RepositoryCohortKey
}

func unavailableWebDiscoveryIdentity(row store.SessionRow) preparedWebDiscoveryIdentity {
	// ProjectHash is the canonical project identity used by the mounted Share
	// hierarchy. Fall back to SessionID when an old row has no stable project
	// identity so unrelated sessions can never be conflated.
	basis := strings.TrimSpace(row.ProjectHash)
	if basis == "" {
		basis = row.SessionID
	}
	digest := sha256.Sum256([]byte("unresolved\x00" + basis))
	return preparedWebDiscoveryIdentity{
		locationID:  fmt.Sprintf("rl_%x", digest[:16]),
		label:       "repository unavailable",
		unavailable: true,
	}
}

func prepareWebDiscoveryIdentities(ctx context.Context, rows []store.SessionRow, selection config.SelectionConfig, resolver ingest.RepositoryIdentityResolver) ([]preparedWebDiscoveryIdentity, []ingest.DiscoveryIdentityMultiplicity, []ingest.DiscoveryIdentityMultiplicity, error) {
	if resolver == nil {
		return nil, nil, nil, fmt.Errorf("Web discovery could not resolve repository identity because no topology resolver was configured in internal/api.prepareWebDiscoveryIdentities. No partial metadata was returned. Restart Peasant, then retry")
	}
	prepared := make([]preparedWebDiscoveryIdentity, len(rows))
	remoteEvidence := make([]selectionprojection.CohortEvidence, len(rows))
	nameEvidence := make([]selectionprojection.CohortEvidence, len(rows))
	labelEvidence := make([]selectionprojection.LocationLabelEvidence, len(rows))
	cohortKeys := make([]ingest.RepositoryCohortKey, len(rows))
	pathResolver := ingest.NewPhysicalPathResolver()
	selected := make(map[ingest.Harness]map[ingest.RepositoryCohortKey]ingest.ClonePath)
	for harnessName, harnessSelection := range selection.Harnesses {
		harness := ingest.Harness(harnessName)
		for _, project := range harnessSelection.Projects {
			for _, raw := range project.ClonePaths {
				clone, resolveErr := pathResolver.Resolve(raw)
				if resolveErr != nil {
					continue
				}
				identity, identityErr := resolver.ResolveRepositoryIdentity(ctx, clone)
				if identityErr != nil || identity.CohortKey == "" {
					continue
				}
				if selected[harness] == nil {
					selected[harness] = make(map[ingest.RepositoryCohortKey]ingest.ClonePath)
				}
				selected[harness][identity.CohortKey] = clone
			}
		}
	}
	byPath := map[string]resolvedWebDiscoveryPath{}
	for i := range rows {
		raw := rows[i].GitWorktree
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		if raw == "" {
			prepared[i] = unavailableWebDiscoveryIdentity(rows[i])
			continue
		}
		resolved, ok := byPath[raw]
		if !ok {
			var err error
			resolved, err = resolveWebDiscoveryPath(ctx, pathResolver, resolver, rows[i])
			if err != nil {
				return nil, nil, nil, err
			}
			byPath[raw] = resolved
		}
		prepared[i] = resolved.identity
		if resolved.cohortKey == "" {
			// An unavailable location carries no cohort evidence, so it takes no
			// part in ambiguity or label grouping.
			continue
		}
		cohortKeys[i] = resolved.cohortKey
		if saved := selected[ingest.Harness(rows[i].ModelHarness)][resolved.cohortKey]; saved != "" {
			prepared[i].clonePath = saved
		}
		harness := ingest.Harness(rows[i].ModelHarness)
		remoteEvidence[i] = selectionprojection.CohortEvidence{Harness: harness, Text: ingest.NormalizeRemoteForMatch(value(rows[i].CanonicalRemote)), CohortKey: cohortKeys[i]}
		nameEvidence[i] = selectionprojection.CohortEvidence{Harness: harness, Text: ingest.NormalizeProjectNameForMatch(rows[i].ProjectName), CohortKey: cohortKeys[i]}
		presentationGroup := ingest.NormalizeRemoteForMatch(value(rows[i].CanonicalRemote))
		if presentationGroup == "" {
			presentationGroup = ingest.NormalizeProjectNameForMatch(rows[i].ProjectName)
		}
		labelEvidence[i] = selectionprojection.LocationLabelEvidence{PresentationGroup: presentationGroup, ClonePath: prepared[i].clonePath, CohortKey: cohortKeys[i]}
	}
	labels := selectionprojection.DistinctLocationLabels(labelEvidence)
	for i := range prepared {
		if prepared[i].label == "" {
			prepared[i].label = labels[i]
		}
	}
	return prepared, selectionprojection.CohortMultiplicities(remoteEvidence), selectionprojection.CohortMultiplicities(nameEvidence), nil
}

// resolveWebDiscoveryPath resolves one recorded directory to its repository
// location.
//
// A session with a captured Git worktree must resolve to repository topology:
// a failure there stays fail-closed, because a non-empty worktree that no
// longer describes a repository is ambiguous evidence. A session without a
// captured worktree only has a recorded directory — the canonical project
// directory, which may be outside Git entirely — so a failed topology there
// yields the unavailable location instead of failing the whole discovery
// projection. Historical sessions whose project was never a Git checkout are
// ordinary stored data; one of them must not take down every list that joins it.
func resolveWebDiscoveryPath(ctx context.Context, pathResolver ingest.PathIdentityResolver, resolver ingest.RepositoryIdentityResolver, row store.SessionRow) (resolvedWebDiscoveryPath, error) {
	raw := row.GitWorktree
	clone, err := pathResolver.Resolve(raw)
	if err != nil {
		if row.RecordedWorktree == "" || errors.Is(err, fs.ErrNotExist) {
			return resolvedWebDiscoveryPath{identity: unavailableWebDiscoveryIdentity(row)}, nil
		}
		return resolvedWebDiscoveryPath{}, fmt.Errorf("Web discovery could not resolve repository identity for session %q because its stored worktree is unavailable in internal/api.prepareWebDiscoveryIdentities. No partial metadata was returned. Restore the repository or re-ingest the session, then retry: %w", row.SessionID, err)
	}
	identity, identityErr := resolver.ResolveRepositoryIdentity(ctx, clone)
	if identityErr == nil && identity.CohortKey != "" {
		digest := sha256.Sum256([]byte(identity.CohortKey.String()))
		return resolvedWebDiscoveryPath{
			identity:  preparedWebDiscoveryIdentity{clonePath: clone, locationID: fmt.Sprintf("rl_%x", digest[:16])},
			cohortKey: identity.CohortKey,
		}, nil
	}
	if row.RecordedWorktree != "" {
		if identityErr == nil {
			identityErr = fmt.Errorf("the repository identity resolver returned no cohort key for %q", clone)
		}
		return resolvedWebDiscoveryPath{}, fmt.Errorf("Web discovery could not resolve repository topology for session %q in internal/api.prepareWebDiscoveryIdentities. No partial metadata was returned because repository identity would be ambiguous. Restore a valid Git repository and retry: %w", row.SessionID, identityErr)
	}
	unavailable := unavailableWebDiscoveryIdentity(row)
	// The directory is still exact-path selection evidence even though it is not
	// a repository location, so a configured project that records this directory
	// keeps selecting its sessions.
	unavailable.clonePath = clone
	return resolvedWebDiscoveryPath{identity: unavailable}, nil
}

func value(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
