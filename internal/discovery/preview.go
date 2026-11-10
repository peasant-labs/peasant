package discovery

import (
	"sort"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
)

const (
	// DefaultSourceTurnsCacheSize is how many parsed sessions a preview reader
	// keeps. The selection tree previews one row at a time.
	DefaultSourceTurnsCacheSize = 4
)

// Preview read budgets are shared by every transcript origin. A file preview
// uses the same numbers as an OpenCode preview so the two paths cannot drift.
const (
	PreviewReadBudgetBytes      = defaults.TranscriptContinuationReadBytes
	PreviewFirstPageBudgetBytes = defaults.TranscriptInitialReadBytes
	PreviewSliceBudgetBytes     = defaults.TranscriptContinuationReadBytes
)

// PreviewKind identifies the non-session row a preview describes.
type PreviewKind uint8

const (
	PreviewUnknown PreviewKind = iota
	PreviewProject
	PreviewBranch
)

// PreviewContext is the resolved repository metadata shown for a project or
// branch row. It is built from the same forest the selection tree renders.
type PreviewContext struct {
	Kind           PreviewKind
	Project        string
	Harnesses      []string
	Remotes        []string
	GitDirectories []string
	ClonePaths     []string
	Branches       []string
	Branch         string
	SessionCount   int
}

// PreviewContexts returns project and branch metadata keyed by node ID. It
// does not rerun Git and does not reconstruct identity from display labels.
func PreviewContexts(forest onboarding.Forest) map[string]PreviewContext {
	children := childrenByParent(forest)
	contexts := make(map[string]PreviewContext)
	for _, node := range forest.Nodes {
		if node.Kind != onboarding.NodeKindProject {
			continue
		}
		contexts[node.ID] = previewContext(node, nil, children)
		for _, child := range children[node.ID] {
			if child.Kind != onboarding.NodeKindBranch {
				continue
			}
			contexts[child.ID] = previewContext(node, &child, children)
		}
	}
	return contexts
}

func childrenByParent(forest onboarding.Forest) map[string][]onboarding.Node {
	children := make(map[string][]onboarding.Node)
	for _, node := range forest.Nodes {
		if node.ParentID == "" {
			continue
		}
		children[node.ParentID] = append(children[node.ParentID], node)
	}
	return children
}

func previewContext(root onboarding.Node, selectedBranch *onboarding.Node, children map[string][]onboarding.Node) PreviewContext {
	kind := PreviewProject
	covered := children[root.ID]
	branchName := ""
	if selectedBranch != nil {
		kind = PreviewBranch
		covered = []onboarding.Node{*selectedBranch}
		branchName = selectedBranch.Label
	}

	harnessSet := map[string]struct{}{}
	remoteSet := map[string]struct{}{}
	clonePathSet := map[string]struct{}{}
	branchSet := map[string]struct{}{}
	gitSet := map[string]struct{}{}
	sessionCount := 0
	for _, child := range covered {
		if child.Kind == onboarding.NodeKindBranch && child.Label != "" {
			branchSet[child.Label] = struct{}{}
		}
	}
	for _, session := range previewSessions(covered, children) {
		sessionCount++
		if harness := string(session.Harness); harness != "" {
			harnessSet[harness] = struct{}{}
		}
		if remote := previewRemote(session.Candidate.GitRemote); remote != "" {
			remoteSet[remote] = struct{}{}
		}
		if clonePath := session.Candidate.ClonePath.String(); clonePath != "" {
			clonePathSet[clonePath] = struct{}{}
		}
		if gitDirectory := session.Labels.Location; gitDirectory != "" {
			gitSet[gitDirectory] = struct{}{}
		}
	}
	if len(remoteSet) == 0 {
		if remote := previewRemote(root.Candidate.GitRemote); remote != "" {
			remoteSet[remote] = struct{}{}
		}
	}
	return PreviewContext{
		Kind:           kind,
		Project:        root.Label,
		Harnesses:      sortedValues(harnessSet),
		Remotes:        sortedValues(remoteSet),
		GitDirectories: sortedValues(gitSet),
		ClonePaths:     sortedValues(clonePathSet),
		Branches:       sortedValues(branchSet),
		Branch:         branchName,
		SessionCount:   sessionCount,
	}
}

func previewSessions(covered []onboarding.Node, children map[string][]onboarding.Node) []onboarding.Node {
	var sessions []onboarding.Node
	for _, child := range covered {
		if child.Kind == onboarding.NodeKindSession {
			sessions = append(sessions, child)
			continue
		}
		for _, nested := range children[child.ID] {
			if nested.Kind == onboarding.NodeKindSession {
				sessions = append(sessions, nested)
			}
		}
	}
	return sessions
}

func previewRemote(remote string) string {
	if normalized := ingest.NormalizeRemoteForMatch(remote); normalized != "" {
		return normalized
	}
	return remote
}

func sortedValues(set map[string]struct{}) []string {
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}
