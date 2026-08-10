package kickstart

import (
	"sort"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionprojection"
)

// CommitGateCandidates builds the complete available project cohort that the
// kickstart save gate evaluates. It resolves every session through the same
// scanner evidence and ProjectIdentity used by the editor tree. Display labels
// never participate in identity.
func (s *ScannerTreeSource) CommitGateCandidates() []selectionprojection.ProjectCandidate {
	if s == nil {
		return nil
	}
	prepared := PrepareSessionListings(s.sessions, s.resolver)
	return projectCandidatesFromPreparedListings(prepared)
}

type commitGateProjectCohort struct {
	identity ProjectIdentity
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
			project = &commitGateProjectCohort{identity: row.ProjectIdentity}
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
		// The gate drops unresolved rows because they cannot become project
		// candidates, but their complete-cohort ambiguity must survive that
		// projection. Only expose a pathless fallback that the scanner proved
		// unique before filtering; exact clone and session evidence stays below.
		gitRemote := ""
		if representative.Candidate.RemoteMultiplicity == ingest.DiscoveryIdentityUnique {
			gitRemote = representative.Listing.GitRemote
		}
		projectName := ""
		if representative.Candidate.NameMultiplicity == ingest.DiscoveryIdentityUnique {
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
			ParentProjectID: selectionprojection.ParentProjectID(project.identity.String()),
			Harness:         project.identity.Harness,
			GitRemote:       gitRemote,
			ProjectName:     projectName,
			ClonePath:       project.identity.ClonePath,
			Descendants:     descendants,
		})
	}
	return candidates
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
