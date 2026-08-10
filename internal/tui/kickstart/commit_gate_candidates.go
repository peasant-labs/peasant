package kickstart

import (
	"fmt"
	"sort"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionprojection"
	"github.com/peasant-labs/peasant/internal/store"
)

// CommitGateCandidates builds the complete available project cohort that the
// kickstart save gate evaluates. Scanner rows keep the editor's physical
// ProjectIdentity. Stored rows keep the same distinct parent-project and
// descendant-worktree carriers used by the viewer: ProjectHash identifies the
// parent, CanonicalCwd is the project path and name carrier, and GitWorktree is
// the descendant path. Both raw paths cross the resolver independently and a
// failed resolution stays empty.
//
// The two candidate sets are joined once before canonical EffectiveProjects
// evaluates them. A scanner descendant already represented by the same stored
// harness, session, and effective physical path is removed as duplicate
// evidence. A distinct scanner or stored path remains distinct. Display labels
// never participate in identity.
func (s *ScannerTreeSource) CommitGateCandidates(
	stored []store.IngestedSessionRow,
) ([]selectionprojection.ProjectCandidate, error) {
	if s == nil {
		return nil, nil
	}
	scannerCandidates := projectCandidatesFromPreparedListings(PrepareSessionListings(s.sessions, s.resolver))
	storedCandidates, err := storedCommitGateCandidates(stored, s.resolver)
	if err != nil {
		return nil, err
	}
	return mergeCommitGateCandidates(scannerCandidates, storedCandidates), nil
}

type commitGateProjectCohort struct {
	identity ProjectIdentity
	parentID selectionprojection.ParentProjectID
	harness  ingest.Harness
	rows     []PreparedSessionListing
}

type commitGateParentRelation struct {
	parent    ingest.SessionID
	ambiguous bool
}

func projectCandidatesFromPreparedListings(prepared []PreparedSessionListing) []selectionprojection.ProjectCandidate {
	parents := commitGateParentSessions(prepared)
	projects := make(map[string]*commitGateProjectCohort)
	projectOrder := make([]string, 0)
	for _, row := range prepared {
		if !row.ProjectIdentity.available() || row.Listing.SessionID == "" {
			continue
		}
		if _, err := ingest.NewSessionID(row.Listing.SessionID); err != nil {
			continue
		}
		key := row.ProjectIdentity.String()
		project := projects[key]
		if project == nil {
			project = &commitGateProjectCohort{
				identity: row.ProjectIdentity,
				parentID: selectionprojection.ParentProjectID(key),
				harness:  row.ProjectIdentity.Harness,
			}
			projects[key] = project
			projectOrder = append(projectOrder, key)
		}
		project.rows = append(project.rows, row)
	}

	sort.Strings(projectOrder)
	candidates := make([]selectionprojection.ProjectCandidate, 0, len(projectOrder))
	for _, key := range projectOrder {
		project := projects[key]
		representative := projectRepresentative(project.rows)
		// An unresolved scanner row cannot become a project candidate, but its
		// complete scanner-cohort ambiguity must still prevent a resolved sibling
		// from exposing remote or name fallback evidence.
		gitRemote := ""
		if project.identity.available() && representative.Candidate.RemoteMultiplicity == ingest.DiscoveryIdentityUnique {
			gitRemote = representative.Listing.GitRemote
		}
		projectName := ""
		if project.identity.available() && representative.Candidate.NameMultiplicity == ingest.DiscoveryIdentityUnique {
			projectName = representative.Listing.ProjectName
		}
		rows := append([]PreparedSessionListing(nil), project.rows...)
		sort.SliceStable(rows, func(i, j int) bool {
			return rows[i].Listing.SessionID < rows[j].Listing.SessionID
		})
		descendants := make([]selectionprojection.SessionCandidate, 0, len(rows))
		seenSessions := make(map[ingest.SessionID]struct{}, len(rows))
		for _, row := range rows {
			sessionID, err := ingest.NewSessionID(row.Listing.SessionID)
			if err != nil {
				continue
			}
			if _, duplicate := seenSessions[sessionID]; duplicate {
				continue
			}
			seenSessions[sessionID] = struct{}{}
			descendant := selectionprojection.SessionCandidate{
				SessionID: sessionID,
				Branch:    row.Listing.Branch,
				ClonePath: row.Candidate.ClonePath,
			}
			if relation := parents[sessionID]; relation != nil && !relation.ambiguous {
				descendant.ParentSessionID = relation.parent
			}
			descendants = append(descendants, descendant)
		}
		candidates = append(candidates, selectionprojection.ProjectCandidate{
			ParentProjectID: project.parentID,
			Harness:         project.harness,
			GitRemote:       gitRemote,
			ProjectName:     projectName,
			ClonePath:       project.identity.ClonePath,
			Descendants:     descendants,
		})
	}
	return candidates
}

type commitGateStoredCandidateKey struct {
	parentProjectID selectionprojection.ParentProjectID
	harness         ingest.Harness
	sessionID       ingest.SessionID
	projectPath     ingest.ClonePath
	sessionPath     ingest.ClonePath
	gitRemote       string
	projectName     string
	branch          string
}

func storedCommitGateCandidates(
	rows []store.IngestedSessionRow,
	resolver ingest.PathIdentityResolver,
) ([]selectionprojection.ProjectCandidate, error) {
	resolvedPaths := make(map[string]ingest.ClonePath)
	seen := make(map[commitGateStoredCandidateKey]struct{}, len(rows))
	candidates := make([]selectionprojection.ProjectCandidate, 0, len(rows))
	for index, row := range rows {
		harness := ingest.Harness(row.Harness)
		if !harness.IsKnown() {
			return nil, fmt.Errorf("stored row %d session %q has unknown harness %q", index, row.SessionID, row.Harness)
		}
		sessionID, err := ingest.NewSessionID(row.SessionID)
		if err != nil {
			return nil, fmt.Errorf("stored row %d has invalid session ID %q: %w", index, row.SessionID, err)
		}
		projectHash, err := ingest.NewProjectHash(row.ProjectHash)
		if err != nil {
			return nil, fmt.Errorf("stored row %d session %q has invalid project hash %q: %w", index, row.SessionID, row.ProjectHash, err)
		}
		projectPath := resolveCommitGatePath(row.CanonicalCwd, resolver, resolvedPaths)
		sessionPath := resolveCommitGatePath(row.GitWorktree, resolver, resolvedPaths)
		parentProjectID := selectionprojection.ParentProjectID(projectHash.String())
		key := commitGateStoredCandidateKey{
			parentProjectID: parentProjectID,
			harness:         harness,
			sessionID:       sessionID,
			projectPath:     projectPath,
			sessionPath:     sessionPath,
			gitRemote:       row.GitRemote,
			projectName:     row.CanonicalCwd,
			branch:          row.Branch,
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		candidates = append(candidates, selectionprojection.ProjectCandidate{
			ParentProjectID: parentProjectID,
			Harness:         harness,
			GitRemote:       row.GitRemote,
			ProjectName:     row.CanonicalCwd,
			ClonePath:       projectPath,
			Descendants: []selectionprojection.SessionCandidate{{
				SessionID: sessionID,
				Branch:    row.Branch,
				ClonePath: sessionPath,
			}},
		})
	}
	return candidates, nil
}

func resolveCommitGatePath(
	raw string,
	resolver ingest.PathIdentityResolver,
	resolved map[string]ingest.ClonePath,
) ingest.ClonePath {
	if raw == "" || resolver == nil {
		return ""
	}
	if path, ok := resolved[raw]; ok {
		return path
	}
	path, err := resolver.Resolve(raw)
	if err != nil {
		path = ""
	}
	resolved[raw] = path
	return path
}

type commitGateSessionEvidenceKey struct {
	harness   ingest.Harness
	sessionID ingest.SessionID
	clonePath ingest.ClonePath
}

func mergeCommitGateCandidates(
	scanner []selectionprojection.ProjectCandidate,
	stored []selectionprojection.ProjectCandidate,
) []selectionprojection.ProjectCandidate {
	storedSessions := make(map[commitGateSessionEvidenceKey]struct{})
	for _, candidate := range stored {
		for _, descendant := range candidate.Descendants {
			storedSessions[commitGateSessionKey(candidate, descendant)] = struct{}{}
		}
	}

	merged := make([]selectionprojection.ProjectCandidate, 0, len(scanner)+len(stored))
	for _, candidate := range scanner {
		kept := make([]selectionprojection.SessionCandidate, 0, len(candidate.Descendants))
		for _, descendant := range candidate.Descendants {
			if _, duplicate := storedSessions[commitGateSessionKey(candidate, descendant)]; duplicate {
				continue
			}
			kept = append(kept, descendant)
		}
		if len(kept) == 0 {
			continue
		}
		candidate.Descendants = kept
		merged = append(merged, candidate)
	}
	return append(merged, stored...)
}

func commitGateSessionKey(
	project selectionprojection.ProjectCandidate,
	session selectionprojection.SessionCandidate,
) commitGateSessionEvidenceKey {
	clonePath := session.ClonePath
	if clonePath == "" {
		clonePath = project.ClonePath
	}
	return commitGateSessionEvidenceKey{
		harness:   project.Harness,
		sessionID: session.SessionID,
		clonePath: clonePath,
	}
}

func commitGateParentSessions(prepared []PreparedSessionListing) map[ingest.SessionID]*commitGateParentRelation {
	parents := make(map[ingest.SessionID]*commitGateParentRelation)
	for _, row := range prepared {
		parentID, err := ingest.NewSessionID(row.Listing.SessionID)
		if err != nil {
			continue
		}
		for _, child := range row.Listing.SubagentIDs {
			childID, err := ingest.NewSessionID(child)
			if err != nil || childID == parentID {
				continue
			}
			relation := parents[childID]
			if relation == nil {
				parents[childID] = &commitGateParentRelation{parent: parentID}
				continue
			}
			if relation.parent != parentID {
				relation.ambiguous = true
			}
		}
	}
	return parents
}
