package kickstart

import (
	"fmt"
	"sort"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionprojection"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
)

// CommitGateCandidates builds the complete available project cohort that the
// kickstart save gate evaluates. It resolves the union of current scanner rows
// and locally stored rows through the same path resolver and complete-cohort
// multiplicity preparation. The editor still uses this ScannerTreeSource alone;
// stored rows extend only the gate evidence so the warning agrees with the
// store-backed viewer and push chooser.
//
// A stored row prefers GitWorktree and falls back to CanonicalCwd only when the
// worktree is empty. A stored session whose chosen path is unavailable remains
// an explicit-session descendant under a synthetic, non-matchable parent. This
// preserves proof that the session is locally available without allowing a
// remote or name to guess project identity. Display labels never participate in
// identity.
func (s *ScannerTreeSource) CommitGateCandidates(
	stored []store.IngestedSessionRow,
) ([]selectionprojection.ProjectCandidate, error) {
	if s == nil {
		return nil, nil
	}
	listings := append([]ftue.SessionListing(nil), s.sessions...)
	storedSessions := make(map[commitGateStoredSession]struct{}, len(stored))
	for index, row := range stored {
		harness := ingest.Harness(row.Harness)
		if !harness.IsKnown() {
			return nil, fmt.Errorf("stored row %d has unknown harness %q", index, row.Harness)
		}
		sessionID, err := ingest.NewSessionID(row.SessionID)
		if err != nil {
			return nil, fmt.Errorf("stored row %d has invalid session ID %q: %w", index, row.SessionID, err)
		}
		storedSessions[commitGateStoredSession{harness: harness, sessionID: sessionID}] = struct{}{}
		rawPath := row.GitWorktree
		if rawPath == "" {
			rawPath = row.CanonicalCwd
		}
		// CanonicalCwd is physical path evidence, not a stored project-name
		// assertion. Recasting it as a name would permit fuzzy fallback matching
		// that the store read model cannot prove.
		listings = append(listings, ftue.SessionListing{
			Harness:    harness.String(),
			GitRemote:  row.GitRemote,
			Branch:     row.Branch,
			Title:      row.Title,
			SessionID:  sessionID.String(),
			WorkingDir: rawPath,
		})
	}
	prepared := PrepareSessionListings(listings, s.resolver)
	return projectCandidatesFromPreparedListings(prepared, storedSessions), nil
}

type commitGateProjectCohort struct {
	identity ProjectIdentity
	parentID selectionprojection.ParentProjectID
	harness  ingest.Harness
	rows     []PreparedSessionListing
}

type commitGateStoredSession struct {
	harness   ingest.Harness
	sessionID ingest.SessionID
}

type commitGateParentRelation struct {
	parent    ingest.SessionID
	ambiguous bool
}

func projectCandidatesFromPreparedListings(
	prepared []PreparedSessionListing,
	storedSessions map[commitGateStoredSession]struct{},
) []selectionprojection.ProjectCandidate {
	parents := commitGateParentSessions(prepared)
	resolvedSessions := make(map[commitGateStoredSession]struct{})
	for _, row := range prepared {
		if !row.ProjectIdentity.available() {
			continue
		}
		sessionID, err := ingest.NewSessionID(row.Listing.SessionID)
		if err != nil {
			continue
		}
		resolvedSessions[commitGateStoredSession{harness: row.ProjectIdentity.Harness, sessionID: sessionID}] = struct{}{}
	}
	projects := make(map[string]*commitGateProjectCohort)
	projectOrder := make([]string, 0)
	for _, row := range prepared {
		if row.Listing.SessionID == "" {
			continue
		}
		sessionID, err := ingest.NewSessionID(row.Listing.SessionID)
		if err != nil {
			continue
		}
		key := row.ProjectIdentity.String()
		parentID := selectionprojection.ParentProjectID(key)
		harness := row.ProjectIdentity.Harness
		if !row.ProjectIdentity.available() {
			harness = row.Candidate.Harness
			storedKey := commitGateStoredSession{harness: harness, sessionID: sessionID}
			if _, availableInStore := storedSessions[storedKey]; !availableInStore {
				continue
			}
			if _, hasResolvedCarrier := resolvedSessions[storedKey]; hasResolvedCarrier {
				continue
			}
			key = unresolvedStoredGateProjectKey(harness, sessionID)
			parentID = selectionprojection.ParentProjectID(key)
		}
		project := projects[key]
		if project == nil {
			project = &commitGateProjectCohort{
				identity: row.ProjectIdentity,
				parentID: parentID,
				harness:  harness,
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
		// Only a resolved physical project may carry a remote or name fallback.
		// Synthetic stored-session parents exist solely to retain exact session
		// evidence; allowing project text on them would turn missing path identity
		// into a guess. Complete-cohort ambiguity must also survive projection.
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

func unresolvedStoredGateProjectKey(harness ingest.Harness, sessionID ingest.SessionID) string {
	return "stored-session:" + harness.String() + ":" + sessionID.String()
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
