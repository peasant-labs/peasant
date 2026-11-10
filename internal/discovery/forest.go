package discovery

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/projectlabel"
	"github.com/peasant-labs/peasant/internal/selectionprojection"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
)

// branchlessPlaceholder labels a session group discovery could not resolve a
// Git branch for. It is display-only. A project whose only branch group is
// this one renders its sessions directly under the project row.
const branchlessPlaceholder = "(no branch detected)"

// PrepareSessionListings resolves every non-empty working directory, then
// annotates every listing from the complete cohort. A listing with unresolved
// path evidence remains in the cohort so an explicit session ID can still
// match, but it cannot become a project row.
func PrepareSessionListings(sessions []SessionListing, resolver ingest.PathIdentityResolver) []PreparedSessionListing {
	return PrepareSessionListingsWith(context.Background(), sessions, resolver, ingest.NewGitRepositoryIdentityResolver())
}

// PrepareSessionListingsWith is PrepareSessionListings with the repository
// identity resolver injected. Grouping and preview diagnostics use that
// resolver. Exact worktree paths still come from resolver.
func PrepareSessionListingsWith(
	ctx context.Context,
	sessions []SessionListing,
	resolver ingest.PathIdentityResolver,
	repositoryResolver ingest.RepositoryIdentityResolver,
) []PreparedSessionListing {
	cohort := make([]PreparedSessionListing, len(sessions))
	remoteEvidence := make([]selectionprojection.CohortEvidence, len(sessions))
	nameEvidence := make([]selectionprojection.CohortEvidence, len(sessions))
	repositoryIdentities := map[ingest.ClonePath]ingest.RepositoryIdentity{}

	for index, listing := range sessions {
		harness := ingest.Harness(listing.Harness)
		row := PreparedSessionListing{
			Listing: listing,
			Candidate: ingest.DiscoveryCandidate{
				Harness:     harness,
				GitRemote:   listing.GitRemote,
				ProjectName: listing.ProjectName,
				Branch:      listing.Branch,
				SessionID:   ingest.SessionID(listing.SessionID),
			},
		}
		if listing.WorkingDir != "" && resolver != nil {
			if clonePath, err := resolver.Resolve(listing.WorkingDir); err == nil && clonePath != "" {
				row.ProjectIdentity = ProjectIdentity{Harness: harness, ClonePath: clonePath}
				row.Candidate.ClonePath = clonePath
				repositoryIdentity, resolved := repositoryIdentities[clonePath]
				if !resolved {
					repositoryIdentity = FallbackRepositoryIdentity(clonePath)
					if repositoryResolver != nil {
						if resolvedIdentity, identityErr := repositoryResolver.ResolveRepositoryIdentity(ctx, clonePath); identityErr == nil && RepositoryIdentityAvailable(resolvedIdentity) {
							repositoryIdentity = resolvedIdentity
						} else if resolvedIdentity.GitDirectory != "" {
							repositoryIdentity = FallbackRepositoryIdentityFromGitDirectory(resolvedIdentity.GitDirectory)
						}
					}
					repositoryIdentities[clonePath] = repositoryIdentity
				}
				row.RepositoryIdentity = repositoryIdentity
			}
		}
		cohort[index] = row
		remoteEvidence[index] = selectionprojection.CohortEvidence{Harness: harness, Text: ingest.NormalizeRemoteForMatch(listing.GitRemote), CohortKey: row.RepositoryIdentity.CohortKey}
		nameEvidence[index] = selectionprojection.CohortEvidence{Harness: harness, Text: ingest.NormalizeProjectNameForMatch(listing.ProjectName), CohortKey: row.RepositoryIdentity.CohortKey}
	}

	remoteMultiplicities := selectionprojection.CohortMultiplicities(remoteEvidence)
	nameMultiplicities := selectionprojection.CohortMultiplicities(nameEvidence)
	for index := range cohort {
		row := &cohort[index]
		row.Candidate.RemoteMultiplicity = remoteMultiplicities[index]
		row.Candidate.NameMultiplicity = nameMultiplicities[index]
	}
	return cohort
}

// BuildForest is the only grouping implementation. It folds a resolved cohort
// into the ordered PROJECT -> BRANCH -> SESSION forest. There is no harness
// grouping axis, no cap, and no cursor.
//
// A child session is summarised on its parent and is not a row of its own. An
// agent-driven top-level session is hidden. User and unknown origins stay
// visible. A project whose only branch group is unresolved omits that level.
func BuildForest(cohort []PreparedSessionListing, ingested map[string]bool, relation SubagentRelation) onboarding.Forest {
	childIDs := map[string]bool{}
	for _, row := range cohort {
		sess := row.Listing
		if sess.SessionID == "" {
			continue
		}
		for _, childID := range sess.SubagentIDs {
			if childID != "" {
				childIDs[childID] = true
			}
		}
	}
	counts := subagentCountIndex(cohort, relation)

	projectOrder := []string{}
	projects := map[string]*projectAgg{}
	for _, row := range cohort {
		sess := row.Listing
		if sess.SessionID == "" {
			continue
		}
		if !row.ProjectIdentity.Available() || !RepositoryIdentityAvailable(row.RepositoryIdentity) {
			continue
		}
		if childIDs[sess.SessionID] {
			continue
		}
		if sess.Origin == sessionorigin.Agent {
			continue
		}
		pKey := row.RepositoryIdentity.CohortKey.String()
		p, ok := projects[pKey]
		if !ok {
			p = &projectAgg{identity: row.RepositoryIdentity, branches: map[string]*branchAgg{}}
			projects[pKey] = p
			projectOrder = append(projectOrder, pKey)
		}
		p.rows = append(p.rows, row)
		bKey := sess.Branch
		if bKey == "" {
			bKey = branchlessPlaceholder
		}
		b, ok := p.branches[bKey]
		if !ok {
			b = &branchAgg{}
			p.branches[bKey] = b
			p.order = append(p.order, bKey)
		}
		b.sessions = append(b.sessions, row)
	}

	sort.Strings(projectOrder)
	labels := projectLabels(projectOrder, projects)
	var nodes []onboarding.Node
	for _, pKey := range projectOrder {
		p := projects[pKey]
		representative := ProjectRepresentative(p.rows)
		projectNode := onboarding.Node{
			ID:        p.identity.CohortKey.String(),
			Kind:      onboarding.NodeKindProject,
			Label:     labels[pKey],
			Candidate: projectCandidate(representative, p.rows),
			State:     onboarding.NodeStateNone,
		}
		sort.Strings(p.order)
		if len(p.order) == 1 && p.order[0] == branchlessPlaceholder {
			b := p.branches[p.order[0]]
			sortListings(b.sessions)
			ordered := groupByImportState(b.sessions, ingested)
			projectNode.ChildCount = len(ordered)
			nodes = append(nodes, projectNode)
			for _, row := range ordered {
				nodes = append(nodes, sessionNode(row, counts, projectNode.ID))
			}
			continue
		}
		var branchNodes []onboarding.Node
		for _, bKey := range p.order {
			b := p.branches[bKey]
			sortListings(b.sessions)
			branch := onboarding.Node{
				ID:       branchID(pKey, bKey),
				ParentID: projectNode.ID,
				Kind:     onboarding.NodeKindBranch,
				Label:    bKey,
				Candidate: ingest.DiscoveryCandidate{
					Branch: bKey,
				},
				State: onboarding.NodeStateNone,
			}
			if clonePath, unique := UniqueClonePath(b.sessions); unique {
				branch.Candidate.ClonePath = clonePath
			}
			ordered := groupByImportState(b.sessions, ingested)
			branch.ChildCount = len(ordered)
			branchNodes = append(branchNodes, branch)
			for _, row := range ordered {
				branchNodes = append(branchNodes, sessionNode(row, counts, branch.ID))
			}
		}
		projectNode.ChildCount = len(p.order)
		nodes = append(nodes, projectNode)
		nodes = append(nodes, branchNodes...)
	}
	return onboarding.Forest{Nodes: nodes, Total: len(nodes)}
}

type branchAgg struct {
	sessions []PreparedSessionListing
}

type projectAgg struct {
	identity ingest.RepositoryIdentity
	rows     []PreparedSessionListing
	order    []string
	branches map[string]*branchAgg
}

func projectCandidate(row PreparedSessionListing, rows []PreparedSessionListing) ingest.DiscoveryCandidate {
	candidate := ingest.DiscoveryCandidate{
		GitRemote:          row.Listing.GitRemote,
		ProjectName:        normalizedProjectName(row.Listing),
		RemoteMultiplicity: row.Candidate.RemoteMultiplicity,
		NameMultiplicity:   row.Candidate.NameMultiplicity,
	}
	if clonePath, unique := UniqueClonePath(rows); unique {
		candidate.ClonePath = clonePath
	}
	return candidate
}

func sessionNode(row PreparedSessionListing, counts SubagentRelation, parentID string) onboarding.Node {
	sess := row.Listing
	label := sess.Title
	if label == "" {
		label = sess.SessionID
	}
	return onboarding.Node{
		ID:         sess.SessionID,
		ParentID:   parentID,
		Kind:       onboarding.NodeKindSession,
		Label:      label,
		Labels:     onboarding.Labels{Location: row.RepositoryIdentity.GitDirectory.String()},
		Harness:    defaults.Harness(sess.Harness),
		SessionID:  sess.SessionID,
		ChildCount: countSubagents(sess.SubagentIDs, counts, map[string]bool{sess.SessionID: true}),
		Candidate:  row.Candidate,
		State:      onboarding.NodeStateNone,
	}
}

func branchID(repositoryID, branch string) string {
	return "branch:" + strconv.Itoa(len(repositoryID)) + ":" + repositoryID + branch
}

// ProjectRepresentative chooses the listing that names a project row. A remote
// wins over a missing remote, then a name, then the session ID.
func ProjectRepresentative(rows []PreparedSessionListing) PreparedSessionListing {
	if len(rows) == 0 {
		return PreparedSessionListing{}
	}
	ordered := append([]PreparedSessionListing(nil), rows...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := ordered[i].Listing, ordered[j].Listing
		if (left.GitRemote == "") != (right.GitRemote == "") {
			return left.GitRemote != ""
		}
		if left.GitRemote != right.GitRemote {
			return left.GitRemote < right.GitRemote
		}
		if (left.ProjectName == "") != (right.ProjectName == "") {
			return left.ProjectName != ""
		}
		if left.ProjectName != right.ProjectName {
			return left.ProjectName < right.ProjectName
		}
		return left.SessionID < right.SessionID
	})
	return ordered[0]
}

// UniqueClonePath reports the clone path when every row carries that same
// non-empty path.
func UniqueClonePath(rows []PreparedSessionListing) (ingest.ClonePath, bool) {
	var clonePath ingest.ClonePath
	for _, row := range rows {
		candidate := row.Candidate.ClonePath
		if candidate == "" {
			return "", false
		}
		if clonePath == "" {
			clonePath = candidate
			continue
		}
		if candidate != clonePath {
			return "", false
		}
	}
	return clonePath, clonePath != ""
}

func projectLabels(order []string, projects map[string]*projectAgg) map[string]string {
	labels := make(map[string]string, len(order))
	pathGroups := map[string][]string{}
	for _, key := range order {
		representative := ProjectRepresentative(projects[key].rows)
		remote := representative.Listing.GitRemote
		if label, ok := projectlabel.FromRemote(remote); ok {
			labels[key] = label
			continue
		}
		name := normalizedProjectName(representative.Listing)
		if remote != "" && name != "" {
			labels[key] = name
			continue
		}
		pathGroups[name] = append(pathGroups[name], key)
	}
	for name, keys := range pathGroups {
		paths := make([]ingest.ClonePath, len(keys))
		for index, key := range keys {
			paths[index] = representativeClonePath(projects[key].rows, projects[key].identity.GitDirectory)
		}
		for index, key := range keys {
			shortPath := selectionprojection.ShortestDistinctCloneSuffix(paths[index], paths)
			switch {
			case name == "" && shortPath == "":
				labels[key] = paths[index].String()
			case name == "":
				labels[key] = shortPath
			case shortPath == "":
				labels[key] = name
			default:
				labels[key] = fmt.Sprintf("%s (%s)", name, shortPath)
			}
		}
	}
	return labels
}

func representativeClonePath(rows []PreparedSessionListing, fallback ingest.RepositoryPath) ingest.ClonePath {
	for _, row := range rows {
		if row.Candidate.ClonePath != "" {
			return row.Candidate.ClonePath
		}
	}
	return ingest.ClonePath(fallback.String())
}

func normalizedProjectName(sess SessionListing) string {
	return ingest.NormalizeProjectNameForMatch(sess.ProjectName)
}

func sortListings(ss []PreparedSessionListing) {
	sort.SliceStable(ss, func(i, j int) bool {
		if !ss[i].Listing.Date.Equal(ss[j].Listing.Date) {
			return ss[i].Listing.Date.Before(ss[j].Listing.Date)
		}
		return ss[i].Listing.SessionID < ss[j].Listing.SessionID
	})
}

func groupByImportState(sessions []PreparedSessionListing, ingested map[string]bool) []PreparedSessionListing {
	out := make([]PreparedSessionListing, 0, len(sessions))
	for _, row := range sessions {
		if !ingested[row.Listing.SessionID] {
			out = append(out, row)
		}
	}
	for _, row := range sessions {
		if ingested[row.Listing.SessionID] {
			out = append(out, row)
		}
	}
	return out
}

// FallbackRepositoryIdentity is the non-Git identity used when repository
// topology cannot be resolved. The clone path is both the cohort key and the
// diagnostic directory.
func FallbackRepositoryIdentity(clonePath ingest.ClonePath) ingest.RepositoryIdentity {
	return ingest.RepositoryIdentity{
		CohortKey:    ingest.RepositoryCohortKey(clonePath.String()),
		GitDirectory: ingest.RepositoryPath(clonePath.String()),
	}
}

// FallbackRepositoryIdentityFromGitDirectory keeps a resolved physical Git
// directory as the cohort when the rest of the topology lookup failed.
func FallbackRepositoryIdentityFromGitDirectory(gitDirectory ingest.RepositoryPath) ingest.RepositoryIdentity {
	return ingest.RepositoryIdentity{
		CohortKey:    ingest.RepositoryCohortKey(gitDirectory.String()),
		GitDirectory: gitDirectory,
	}
}

// RepositoryIdentityAvailable reports that both the cohort key and the Git
// directory were resolved.
func RepositoryIdentityAvailable(identity ingest.RepositoryIdentity) bool {
	return identity.CohortKey != "" && identity.GitDirectory != ""
}

func subagentCountIndex(cohort []PreparedSessionListing, relation SubagentRelation) SubagentRelation {
	counts := make(SubagentRelation, len(relation)+len(cohort))
	for parentID, childIDs := range relation {
		if parentID != "" {
			counts[parentID] = childIDs
		}
	}
	for _, row := range cohort {
		sess := row.Listing
		if sess.SessionID == "" {
			continue
		}
		if _, indexed := counts[sess.SessionID]; !indexed {
			counts[sess.SessionID] = sess.SubagentIDs
		}
	}
	return counts
}

func countSubagents(childIDs []string, counts SubagentRelation, seen map[string]bool) int {
	total := 0
	for _, childID := range childIDs {
		if childID == "" || seen[childID] {
			continue
		}
		grandchildren, discovered := counts[childID]
		if !discovered {
			continue
		}
		seen[childID] = true
		total += 1 + countSubagents(grandchildren, counts, seen)
	}
	return total
}
