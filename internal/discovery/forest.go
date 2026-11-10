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

// PrepareSessionListings delegates to the shared cohort rule.
func PrepareSessionListings(sessions []SessionListing, resolver ingest.PathIdentityResolver) []PreparedSessionListing {
	return onboarding.PrepareSessionListings(sessions, resolver)
}

// PrepareSessionListingsWith delegates to the shared cohort rule.
func PrepareSessionListingsWith(ctx context.Context, sessions []SessionListing, resolver ingest.PathIdentityResolver, repositoryResolver ingest.RepositoryIdentityResolver) []PreparedSessionListing {
	return onboarding.PrepareSessionListingsWith(ctx, sessions, resolver, repositoryResolver)
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

// ProjectRepresentative delegates to the shared cohort rule.
func ProjectRepresentative(rows []PreparedSessionListing) PreparedSessionListing {
	return onboarding.ProjectRepresentative(rows)
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

// FallbackRepositoryIdentity delegates to the shared identity rule.
func FallbackRepositoryIdentity(path ingest.ClonePath) ingest.RepositoryIdentity {
	return onboarding.FallbackRepositoryIdentity(path)
}

// FallbackRepositoryIdentityFromGitDirectory delegates to the shared identity rule.
func FallbackRepositoryIdentityFromGitDirectory(path ingest.RepositoryPath) ingest.RepositoryIdentity {
	return onboarding.FallbackRepositoryIdentityFromGitDirectory(path)
}

// RepositoryIdentityAvailable delegates to the shared identity rule.
func RepositoryIdentityAvailable(identity ingest.RepositoryIdentity) bool {
	return onboarding.RepositoryIdentityAvailable(identity)
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
