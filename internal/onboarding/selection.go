package onboarding

import (
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

const (
	branchlessPlaceholder       = "(no branch detected)"
	legacyBranchlessPlaceholder = "(unknown branch)"
)

// RestoreSelection applies a saved selected-mode configuration to a forest.
// It sanitizes display-only branch placeholders, records explicit branch and
// session provenance, marks each row's tri-state, and returns the saved choices
// the current forest cannot show. Mode all is left untouched: that legacy
// policy is converted before it reaches this function.
func RestoreSelection(forest Forest, saved config.SelectionConfig) (Forest, Residual, error) {
	if saved.Mode != config.SelectionModeSelected {
		return cloneForest(forest), Residual{}, nil
	}
	roots := selectionTree(forest)
	projects := availableProjects(roots)
	sanitized, _ := sanitizePlaceholderSelection(saved, projects)
	markExplicitBranches(sanitized, projects)
	markExplicitSessions(sanitized, projects)
	matcher := config.CompileSelectionMatcher(sanitized)
	for projectIndex := range projects {
		project := &projects[projectIndex]
		for sessionIndex := range project.sessions {
			session := &project.sessions[sessionIndex]
			switch matcher.MatchDiscoveryCandidate(session.candidate, sanitized.AutoIngestNewBranches) {
			case ingest.BranchMatchYes:
				session.node.state = triAll
			case ingest.BranchMatchWithheldConflict:
				session.node.state = triLocked
			default:
				session.node.state = triNone
			}
		}
	}
	for _, root := range roots {
		rollupNode(root)
	}
	return writeForest(forest, roots), unmatchedSelection(sanitized, projects), nil
}

// deriveSelection is the inverse of RestoreSelection. ids are the answered
// node ids. A parent id selects its descendants. Provenance already on the
// forest stops a full check from widening an explicit branch or session rule.
// The residual is merged back so a choice the current forest cannot show is
// not deleted.
func deriveSelection(forest Forest, selected SelectedForest, autoIngest bool, residual Residual) (config.SelectionConfig, error) {
	roots := selectionTree(forest)
	applyAnswer(roots, selected.IDs)
	for _, root := range roots {
		rollupNode(root)
	}
	projects := availableProjects(roots)
	var derived config.SelectionConfig
	if projectFirst(roots) {
		derived = deriveProjectFirst(projects)
	} else {
		derived = deriveHarnessFirst(roots)
	}
	derived.AutoIngestNewBranches = autoIngest
	return MergeResidual(derived, residual), nil
}

// MergeResidual copies unavailable saved choices back onto a derived selection.
// Branch residuals are applied only while an available matching project remains
// selected. A nil branch list already means every branch, so it adds nothing.
// A missing-clone residual that already carries the full policy is not also
// recorded as a conditional branch residual.
func MergeResidual(sel config.SelectionConfig, residual Residual) config.SelectionConfig {
	if sel.Mode != config.SelectionModeSelected || (len(residual.Harnesses) == 0 && len(residual.Branches) == 0) {
		return cloneSelection(sel)
	}
	merged := cloneSelection(sel)
	if merged.Harnesses == nil {
		merged.Harnesses = map[string]config.SelectionHarnessConfig{}
	}
	for _, missing := range residual.Branches {
		result, ok := merged.Harnesses[missing.Harness]
		if !ok {
			continue
		}
		for index := range result.Projects {
			project := &result.Projects[index]
			if !sameAvailableProject(*project, missing.Project) {
				continue
			}
			if len(project.Branches) > 0 {
				for _, branch := range missing.Branches {
					project.Branches = appendUnique(project.Branches, branch)
				}
				sort.Strings(project.Branches)
			}
		}
		merged.Harnesses[missing.Harness] = result
	}
	for harness, saved := range residual.Harnesses {
		result := merged.Harnesses[harness]
		for _, sessionID := range saved.Sessions {
			result.Sessions = appendUnique(result.Sessions, sessionID)
		}
		for _, project := range saved.Projects {
			result.Projects = mergeProject(result.Projects, project)
		}
		result.Exclusions = mergeExclusions(result.Exclusions, saved.Exclusions)
		if harnessPresent(result) {
			merged.Harnesses[harness] = result
		}
	}
	if len(merged.Harnesses) == 0 {
		merged.Harnesses = nil
	}
	return merged
}

// SanitizeSelectionPlaceholders resolves a saved selection's display-only
// branch placeholders against a forest, rewriting them into the explicit
// session rules they stand for. It reports whether the value changed. A
// placeholder-free selection is returned unchanged.
func SanitizeSelectionPlaceholders(forest Forest, sel config.SelectionConfig) (config.SelectionConfig, bool) {
	return sanitizePlaceholderSelection(sel, availableProjects(selectionTree(forest)))
}

// DroppedSelections is what a rescan removed from an answered id set. Labels
// holds at most schema.LocalKickstartDroppedLabelsMax names, the number the
// contract allows a scan to carry. Count is the full number removed.
type DroppedSelections struct {
	Count  int
	Labels []string
}

// RescanIDs keeps the answered ids that still exist in the forest the rescan
// found (current) and drops the rest. It never adds an id. Vanished ids are
// reported by count and at most schema.LocalKickstartDroppedLabelsMax labels.
// A vanished row is gone from current, so its label is read from the forest the
// answer was given over (previous); an id that forest does not hold either is
// its own label.
func RescanIDs(ids []string, previous, current Forest) ([]string, DroppedSelections) {
	present := forestRowIDs(current)
	kept := make([]string, 0, len(ids))
	seen := map[string]bool{}
	var dropped DroppedSelections
	var labels map[string]string
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if present[id] {
			kept = append(kept, id)
			continue
		}
		dropped.Count++
		if len(dropped.Labels) >= schema.LocalKickstartDroppedLabelsMax {
			continue
		}
		if labels == nil {
			labels = forestRowLabels(previous)
		}
		label := labels[id]
		if label == "" {
			label = id
		}
		dropped.Labels = append(dropped.Labels, label)
	}
	return kept, dropped
}

// forestRowIDs is every id a forest answers to: a row's id and, for a session
// row, its session id.
func forestRowIDs(forest Forest) map[string]bool {
	ids := make(map[string]bool, len(forest.Nodes))
	for _, node := range forest.Nodes {
		if node.ID == "" {
			continue
		}
		ids[node.ID] = true
		if node.SessionID != "" {
			ids[node.SessionID] = true
		}
	}
	return ids
}

// forestRowLabels maps every id a forest answers to onto the row's label.
func forestRowLabels(forest Forest) map[string]string {
	labels := make(map[string]string, len(forest.Nodes))
	for _, node := range forest.Nodes {
		if node.ID == "" || node.Label == "" {
			continue
		}
		labels[node.ID] = node.Label
		if node.SessionID != "" {
			labels[node.SessionID] = node.Label
		}
	}
	return labels
}

// Rollup recomputes parent tri-states from their children. A locked child
// makes the parent some, never a clean all or none. Leaves keep their state.
func Rollup(forest Forest) Forest {
	roots := selectionTree(forest)
	for _, root := range roots {
		rollupNode(root)
	}
	return writeForest(forest, roots)
}

type tri uint8

const (
	triNone tri = iota
	triSome
	triAll
	triLocked
)

type sNode struct {
	id              string
	kind            NodeKind
	label           string
	state           tri
	explicitBranch  bool
	explicitSession bool
	harness         string
	identity        string
	candidate       ingest.DiscoveryCandidate
	children        []*sNode
	forestIndex     int
}

type availSession struct {
	node      *sNode
	candidate ingest.DiscoveryCandidate
}

type availProject struct {
	node               *sNode
	harness            ingest.Harness
	identity           string
	gitRemote          string
	projectName        string
	clonePath          ingest.ClonePath
	remoteMultiplicity ingest.DiscoveryIdentityMultiplicity
	nameMultiplicity   ingest.DiscoveryIdentityMultiplicity
	branches           map[string]struct{}
	sessions           []availSession
}

func (p availProject) candidate() ingest.DiscoveryCandidate {
	return ingest.DiscoveryCandidate{
		Harness:            p.harness,
		GitRemote:          p.gitRemote,
		ProjectName:        p.projectName,
		ClonePath:          p.clonePath,
		RemoteMultiplicity: p.remoteMultiplicity,
		NameMultiplicity:   p.nameMultiplicity,
	}
}

func selectionTree(forest Forest) []*sNode {
	nodes := make([]*sNode, len(forest.Nodes))
	byID := map[string]*sNode{}
	for index := range forest.Nodes {
		node := forest.Nodes[index]
		built := &sNode{
			id:              node.ID,
			kind:            node.Kind,
			label:           node.Label,
			state:           triFrom(node),
			explicitBranch:  node.ExplicitBranch,
			explicitSession: node.ExplicitSession,
			harness:         node.Harness.String(),
			identity:        node.Identity,
			candidate:       node.Candidate,
			forestIndex:     index,
		}
		if built.harness == "" || built.harness == "unknown" {
			built.harness = node.Candidate.Harness.String()
		}
		nodes[index] = built
		if node.ID != "" {
			byID[node.ID] = built
		}
	}
	var roots []*sNode
	for index, node := range forest.Nodes {
		built := nodes[index]
		if node.ParentID == "" {
			roots = append(roots, built)
			continue
		}
		parent := byID[node.ParentID]
		if parent == nil {
			roots = append(roots, built)
			continue
		}
		parent.children = append(parent.children, built)
	}
	return roots
}

func triFrom(node Node) tri {
	if node.Locked {
		return triLocked
	}
	switch node.State {
	case NodeStateAll:
		return triAll
	case NodeStateSome:
		return triSome
	default:
		return triNone
	}
}

func stateFrom(value tri) NodeState {
	switch value {
	case triAll:
		return NodeStateAll
	case triSome:
		return NodeStateSome
	default:
		return NodeStateNone
	}
}

func writeForest(forest Forest, roots []*sNode) Forest {
	out := cloneForest(forest)
	var walk func(*sNode)
	walk = func(node *sNode) {
		if node == nil || node.forestIndex < 0 || node.forestIndex >= len(out.Nodes) {
			return
		}
		target := &out.Nodes[node.forestIndex]
		target.State = stateFrom(node.state)
		target.Locked = node.state == triLocked
		target.ExplicitBranch = node.explicitBranch
		target.ExplicitSession = node.explicitSession
		for _, child := range node.children {
			walk(child)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	return out
}

func cloneForest(forest Forest) Forest {
	out := forest
	out.Nodes = append([]Node(nil), forest.Nodes...)
	return out
}

func applyAnswer(roots []*sNode, ids []string) {
	if ids == nil {
		return
	}
	flat := flatten(roots)
	byID := map[string][]*sNode{}
	for _, node := range flat {
		if node.id != "" {
			byID[node.id] = append(byID[node.id], node)
		}
		if node.candidate.SessionID != "" {
			byID[node.candidate.SessionID.String()] = append(byID[node.candidate.SessionID.String()], node)
		}
	}
	for _, node := range flat {
		if node.state != triLocked {
			node.state = triNone
		}
	}
	selected := map[string]struct{}{}
	for _, id := range ids {
		if id == "" {
			continue
		}
		selected[id] = struct{}{}
	}
	for id := range selected {
		matches := byID[id]
		if len(matches) != 1 {
			continue
		}
		markSelected(matches[0])
	}
	for _, node := range flat {
		if node.kind != NodeKindSession && node.candidate.SessionID == "" {
			continue
		}
		sessionID := node.id
		if node.candidate.SessionID != "" {
			sessionID = node.candidate.SessionID.String()
		}
		if _, ok := selected[sessionID]; ok && node.state != triLocked {
			node.state = triAll
		}
	}
}

func markSelected(node *sNode) {
	if node == nil || node.state == triLocked {
		return
	}
	if len(node.children) == 0 {
		node.state = triAll
		return
	}
	for _, child := range node.children {
		markSelected(child)
	}
}

func flatten(roots []*sNode) []*sNode {
	var out []*sNode
	var walk func(*sNode)
	walk = func(node *sNode) {
		if node == nil {
			return
		}
		out = append(out, node)
		for _, child := range node.children {
			walk(child)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	return out
}

func rollupNode(node *sNode) tri {
	if node == nil {
		return triNone
	}
	if len(node.children) == 0 {
		return node.state
	}
	var all, none, other int
	for _, child := range node.children {
		switch rollupNode(child) {
		case triAll:
			all++
		case triNone:
			none++
		default:
			other++
		}
	}
	switch {
	case other > 0:
		node.state = triSome
	case all > 0 && none == 0:
		node.state = triAll
	case all == 0 && none > 0:
		node.state = triNone
	default:
		node.state = triSome
	}
	return node.state
}

func projectFirst(roots []*sNode) bool {
	var any func(*sNode) bool
	any = func(node *sNode) bool {
		if node == nil {
			return false
		}
		if node.kind == NodeKindSession && node.harness != "" && node.harness != "unknown" {
			return true
		}
		for _, child := range node.children {
			if any(child) {
				return true
			}
		}
		return false
	}
	for _, root := range roots {
		if any(root) {
			return true
		}
	}
	return false
}

func availableProjects(roots []*sNode) []availProject {
	var projects []availProject
	if projectFirst(roots) {
		for _, projectNode := range roots {
			byPath := map[string]*availProject{}
			var order []string
			for _, child := range projectNode.children {
				if child.kind == NodeKindSession || child.harness != "" {
					appendSessionsByPath(&byPath, &order, projectNode, nil, child, "", "")
					continue
				}
				branch := semanticBranch(child)
				for _, session := range child.children {
					appendSessionsByPath(&byPath, &order, projectNode, child, session, branch, "")
				}
			}
			for _, key := range order {
				projects = append(projects, *byPath[key])
			}
		}
	} else {
		for _, provider := range roots {
			harness := ingest.Harness(provider.id)
			if provider.harness != "" && provider.harness != "unknown" {
				harness = ingest.Harness(provider.harness)
			}
			for _, projectNode := range provider.children {
				project := availProject{
					node:               projectNode,
					harness:            harness,
					identity:           firstNonEmpty(projectNode.identity, projectNode.id),
					gitRemote:          projectNode.candidate.GitRemote,
					projectName:        firstNonEmpty(projectNode.candidate.ProjectName, projectNode.label),
					clonePath:          projectNode.candidate.ClonePath,
					remoteMultiplicity: projectNode.candidate.RemoteMultiplicity,
					nameMultiplicity:   projectNode.candidate.NameMultiplicity,
					branches:           map[string]struct{}{},
				}
				for _, branchNode := range projectNode.children {
					branch := semanticBranch(branchNode)
					project.branches[branch] = struct{}{}
					appendSessions(&project, branchNode.children, branch)
					if len(branchNode.children) == 0 && branchNode.kind == NodeKindSession {
						appendSession(&project, branchNode, branch)
					}
				}
				projects = append(projects, project)
			}
		}
	}
	annotateMultiplicity(projects)
	return projects
}

func appendSessionsByPath(
	byPath *map[string]*availProject,
	order *[]string,
	projectNode, branchNode, node *sNode,
	branch string,
	inherited ingest.ClonePath,
) {
	if node == nil {
		return
	}
	if node.kind != NodeKindSession && node.candidate.SessionID == "" && len(node.children) > 0 {
		for _, child := range node.children {
			appendSessionsByPath(byPath, order, projectNode, branchNode, child, branch, inherited)
		}
		return
	}
	harness := ingest.Harness(projectNode.candidate.Harness)
	if projectNode.harness != "" && projectNode.harness != "unknown" {
		harness = ingest.Harness(projectNode.harness)
	}
	if node.harness != "" && node.harness != "unknown" {
		harness = ingest.Harness(node.harness)
	}
	if node.candidate.Harness != "" {
		harness = node.candidate.Harness
	}
	clonePath := node.candidate.ClonePath
	if clonePath == "" {
		clonePath = inherited
	}
	if clonePath == "" && branchNode != nil {
		clonePath = branchNode.candidate.ClonePath
	}
	if clonePath == "" {
		clonePath = projectNode.candidate.ClonePath
	}
	key := harness.String() + "\x00" + clonePath.String()
	project := (*byPath)[key]
	if project == nil {
		gitRemote := node.candidate.GitRemote
		if gitRemote == "" {
			gitRemote = projectNode.candidate.GitRemote
		}
		projectName := node.candidate.ProjectName
		if projectName == "" {
			projectName = firstNonEmpty(projectNode.candidate.ProjectName, projectNode.label)
		}
		remoteMultiplicity := node.candidate.RemoteMultiplicity
		if remoteMultiplicity == ingest.DiscoveryIdentityUnproven {
			remoteMultiplicity = projectNode.candidate.RemoteMultiplicity
		}
		nameMultiplicity := node.candidate.NameMultiplicity
		if nameMultiplicity == ingest.DiscoveryIdentityUnproven {
			nameMultiplicity = projectNode.candidate.NameMultiplicity
		}
		project = &availProject{
			node:               projectNode,
			harness:            harness,
			identity:           firstNonEmpty(projectNode.identity, projectNode.id),
			gitRemote:          gitRemote,
			projectName:        projectName,
			clonePath:          clonePath,
			remoteMultiplicity: remoteMultiplicity,
			nameMultiplicity:   nameMultiplicity,
			branches:           map[string]struct{}{},
		}
		(*byPath)[key] = project
		*order = append(*order, key)
	}
	project.branches[branch] = struct{}{}
	appendSession(project, node, branch)
	for _, child := range node.children {
		appendSessionsByPath(byPath, order, projectNode, branchNode, child, branch, clonePath)
	}
}

func appendSessions(project *availProject, nodes []*sNode, branch string) {
	for _, node := range nodes {
		appendSession(project, node, branch)
		appendSessions(project, node.children, branch)
	}
}

func appendSession(project *availProject, node *sNode, branch string) {
	if node == nil {
		return
	}
	harness := project.harness
	if node.harness != "" && node.harness != "unknown" {
		harness = ingest.Harness(node.harness)
	}
	if node.candidate.Harness != "" {
		harness = node.candidate.Harness
	}
	gitRemote := node.candidate.GitRemote
	if gitRemote == "" {
		gitRemote = project.gitRemote
	}
	projectName := node.candidate.ProjectName
	if projectName == "" {
		projectName = project.projectName
	}
	clonePath := node.candidate.ClonePath
	if clonePath == "" {
		clonePath = project.clonePath
	}
	sessionID := node.candidate.SessionID
	if sessionID == "" {
		sessionID = ingest.SessionID(node.id)
	}
	if node.kind == NodeKindSession || node.candidate.SessionID != "" || len(node.children) == 0 {
		project.sessions = append(project.sessions, availSession{
			node: node,
			candidate: ingest.DiscoveryCandidate{
				Harness:            harness,
				GitRemote:          gitRemote,
				ProjectName:        projectName,
				ClonePath:          clonePath,
				Branch:             branch,
				SessionID:          sessionID,
				RemoteMultiplicity: node.candidate.RemoteMultiplicity,
				NameMultiplicity:   node.candidate.NameMultiplicity,
			},
		})
	}
}

func semanticBranch(node *sNode) string {
	if node == nil {
		return ""
	}
	name := node.candidate.Branch
	if name == "" {
		name = node.label
	}
	if isBranchless(name) {
		return ""
	}
	return name
}

func isBranchless(name string) bool {
	return name == branchlessPlaceholder || name == legacyBranchlessPlaceholder
}

func annotateMultiplicity(projects []availProject) {
	remoteIDs := map[multiplicityKey]map[string]struct{}{}
	nameIDs := map[multiplicityKey]map[string]struct{}{}
	for _, project := range projects {
		addIdentity(remoteIDs, multiplicityKey{harness: project.harness, text: ingest.NormalizeRemoteForMatch(project.gitRemote)}, project.identity)
		addIdentity(nameIDs, multiplicityKey{harness: project.harness, text: ingest.NormalizeProjectNameForMatch(project.projectName)}, project.identity)
		for _, session := range project.sessions {
			addIdentity(remoteIDs, multiplicityKey{harness: session.candidate.Harness, text: ingest.NormalizeRemoteForMatch(session.candidate.GitRemote)}, project.identity)
			addIdentity(nameIDs, multiplicityKey{harness: session.candidate.Harness, text: ingest.NormalizeProjectNameForMatch(session.candidate.ProjectName)}, project.identity)
		}
	}
	for projectIndex := range projects {
		project := &projects[projectIndex]
		if project.remoteMultiplicity == ingest.DiscoveryIdentityUnproven {
			project.remoteMultiplicity = multiplicityOf(remoteIDs, project.harness, ingest.NormalizeRemoteForMatch(project.gitRemote))
		}
		if project.nameMultiplicity == ingest.DiscoveryIdentityUnproven {
			project.nameMultiplicity = multiplicityOf(nameIDs, project.harness, ingest.NormalizeProjectNameForMatch(project.projectName))
		}
		for sessionIndex := range project.sessions {
			candidate := &project.sessions[sessionIndex].candidate
			if candidate.RemoteMultiplicity == ingest.DiscoveryIdentityUnproven {
				candidate.RemoteMultiplicity = multiplicityOf(remoteIDs, candidate.Harness, ingest.NormalizeRemoteForMatch(candidate.GitRemote))
			}
			if candidate.NameMultiplicity == ingest.DiscoveryIdentityUnproven {
				candidate.NameMultiplicity = multiplicityOf(nameIDs, candidate.Harness, ingest.NormalizeProjectNameForMatch(candidate.ProjectName))
			}
		}
	}
}

type multiplicityKey struct {
	harness ingest.Harness
	text    string
}

func addIdentity(cohorts map[multiplicityKey]map[string]struct{}, key multiplicityKey, identity string) {
	if key.text == "" || identity == "" {
		return
	}
	if cohorts[key] == nil {
		cohorts[key] = map[string]struct{}{}
	}
	cohorts[key][identity] = struct{}{}
}

func multiplicityOf(cohorts map[multiplicityKey]map[string]struct{}, harness ingest.Harness, text string) ingest.DiscoveryIdentityMultiplicity {
	if text == "" {
		return ingest.DiscoveryIdentityUnique
	}
	if len(cohorts[multiplicityKey{harness: harness, text: text}]) == 1 {
		return ingest.DiscoveryIdentityUnique
	}
	return ingest.DiscoveryIdentityAmbiguous
}

func markExplicitBranches(sel config.SelectionConfig, projects []availProject) {
	for _, project := range projects {
		walkSNodes(project.node, func(node *sNode) {
			node.explicitBranch = false
		})
	}
	for harness, configured := range sel.Harnesses {
		for _, configuredProject := range configured.Projects {
			if len(configuredProject.Branches) == 0 {
				continue
			}
			for _, project := range matchingProjects(harness, configuredProject, projects) {
				for _, session := range project.sessions {
					session.node.explicitBranch = true
				}
			}
		}
	}
}

func markExplicitSessions(sel config.SelectionConfig, projects []availProject) {
	explicit := map[string]map[string]struct{}{}
	projectHarnesses := map[string]config.SelectionHarnessConfig{}
	for harness, configured := range sel.Harnesses {
		if len(configured.Sessions) > 0 {
			explicit[harness] = map[string]struct{}{}
			for _, sessionID := range configured.Sessions {
				explicit[harness][sessionID] = struct{}{}
			}
		}
		if len(configured.Projects) > 0 {
			projectHarnesses[harness] = config.SelectionHarnessConfig{Projects: configured.Projects}
		}
	}
	projectMatcher := config.CompileSelectionMatcher(config.SelectionConfig{
		Mode:                  config.SelectionModeSelected,
		AutoIngestNewBranches: sel.AutoIngestNewBranches,
		Harnesses:             projectHarnesses,
	})
	for projectIndex := range projects {
		for sessionIndex := range projects[projectIndex].sessions {
			session := &projects[projectIndex].sessions[sessionIndex]
			session.node.explicitSession = false
			if _, selected := explicit[session.candidate.Harness.String()][session.candidate.SessionID.String()]; !selected {
				continue
			}
			if projectMatcher.MatchDiscoveryCandidate(session.candidate, sel.AutoIngestNewBranches) == ingest.BranchMatchYes {
				continue
			}
			session.node.explicitSession = true
		}
	}
}

func matchingProjects(harness string, configured config.ProjectSelection, projects []availProject) []availProject {
	selection := config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			harness: {Projects: []config.ProjectSelection{configured}},
		},
	}
	matcher := config.CompileSelectionMatcher(selection)
	var matching []availProject
	for _, project := range projects {
		if string(project.harness) == harness && matcher.MatchesCandidate(project.candidate()) {
			matching = append(matching, project)
		}
	}
	return matching
}

func unmatchedSelection(sel config.SelectionConfig, projects []availProject) Residual {
	residual := Residual{Harnesses: map[string]config.SelectionHarnessConfig{}}
	availableSessions := map[string]map[string]struct{}{}
	for _, project := range projects {
		for _, session := range project.sessions {
			harness := session.candidate.Harness.String()
			if availableSessions[harness] == nil {
				availableSessions[harness] = map[string]struct{}{}
			}
			availableSessions[harness][session.candidate.SessionID.String()] = struct{}{}
		}
	}
	for harness, configured := range sel.Harnesses {
		saved := config.SelectionHarnessConfig{Exclusions: cloneExclusions(configured.Exclusions)}
		for _, sessionID := range configured.Sessions {
			if _, available := availableSessions[harness][sessionID]; !available {
				saved.Sessions = appendUnique(saved.Sessions, sessionID)
			}
		}
		for _, configuredProject := range configured.Projects {
			matching := matchingProjects(harness, configuredProject, projects)
			if len(matching) == 0 {
				saved.Projects = append(saved.Projects, cloneProject(configuredProject))
				continue
			}
			hasMissingPaths := false
			if len(configuredProject.ClonePaths) > 0 {
				matchedPaths := map[string]struct{}{}
				for _, project := range matching {
					if project.clonePath != "" {
						matchedPaths[project.clonePath.String()] = struct{}{}
					}
				}
				var missingPaths []string
				for _, clonePath := range configuredProject.ClonePaths {
					if _, available := matchedPaths[clonePath]; !available {
						missingPaths = appendUnique(missingPaths, clonePath)
					}
				}
				if len(missingPaths) > 0 {
					hasMissingPaths = true
					missing := cloneProject(configuredProject)
					missing.ClonePaths = missingPaths
					saved.Projects = append(saved.Projects, missing)
				}
			}
			if len(configuredProject.Branches) > 0 && !hasMissingPaths {
				var missingBranches []string
				for _, branch := range configuredProject.Branches {
					available := false
					for _, project := range matching {
						if _, ok := project.branches[branch]; ok {
							available = true
							break
						}
					}
					if !available {
						missingBranches = appendUnique(missingBranches, branch)
					}
				}
				if len(missingBranches) > 0 {
					target := cloneProject(configuredProject)
					target.Branches = nil
					if len(configuredProject.ClonePaths) > 0 {
						target.ClonePaths = nil
						for _, project := range matching {
							if project.clonePath != "" {
								target.ClonePaths = appendUnique(target.ClonePaths, project.clonePath.String())
							}
						}
					}
					residual.Branches = append(residual.Branches, ResidualBranch{
						Harness:  harness,
						Project:  target,
						Branches: missingBranches,
					})
				}
			}
		}
		if harnessPresent(saved) {
			residual.Harnesses[harness] = saved
		}
	}
	if len(residual.Harnesses) == 0 {
		residual.Harnesses = nil
	}
	return residual
}

// CheckoutSession is one session of a checkout as the derivation reads it.
type CheckoutSession struct {
	// ID is the session id.
	ID string
	// Branch is the branch the session is listed under. It is empty for a
	// session that recorded none.
	Branch string
	// Chosen reports whether the person chose the session.
	Chosen bool
	// ExplicitSession is provenance: a saved rule named this session by id, so
	// a derivation that finds every session of the checkout chosen keeps naming
	// them instead of widening the rule to a branch.
	ExplicitSession bool
}

// CheckoutRule is how a checkout's chosen sessions are saved.
type CheckoutRule struct {
	// Whole is the checkout's own rule with no branch list. It is saved only for
	// a checkout with no named branch, where there is no branch to name.
	Whole bool
	// Branches are the named branches every one of whose sessions is chosen,
	// sorted. A branch that appears later is admitted by the new-branch setting,
	// not by this rule.
	Branches []string
	// Sessions are the chosen sessions that no rule above covers, in the order
	// given: sessions named explicitly, and the chosen sessions of a group with
	// no branch to name.
	Sessions []string
}

// DeriveCheckout is the one derivation of the rule that saves a checkout's
// chosen sessions. The setup page, the terminal's select-all and the terminal's
// project row all call it, so the same sessions chosen give the same rule
// whichever row was ticked.
//
// Choosing a branch saves that branch, even when it is the only branch in its
// checkout and even when every branch of the checkout is chosen. The
// whole-checkout rule is saved only when the checkout has no named branch.
func DeriveCheckout(sessions []CheckoutSession) CheckoutRule {
	allChosen, allExplicit, chosen := true, true, 0
	var branchOrder []string
	byBranch := map[string][]CheckoutSession{}
	named := false
	for _, session := range sessions {
		if _, known := byBranch[session.Branch]; !known {
			branchOrder = append(branchOrder, session.Branch)
		}
		byBranch[session.Branch] = append(byBranch[session.Branch], session)
		if session.Branch != "" {
			named = true
		}
		if !session.Chosen {
			allChosen = false
			continue
		}
		chosen++
		if !session.ExplicitSession {
			allExplicit = false
		}
	}
	var rule CheckoutRule
	if chosen == 0 {
		return rule
	}
	if allChosen && allExplicit {
		for _, session := range sessions {
			rule.Sessions = appendUnique(rule.Sessions, session.ID)
		}
		return rule
	}
	if allChosen && !named {
		rule.Whole = true
		return rule
	}
	for _, branch := range branchOrder {
		group := byBranch[branch]
		branchChosen, branchExplicit := len(group) > 0, true
		for _, session := range group {
			if !session.Chosen {
				branchChosen = false
				continue
			}
			if !session.ExplicitSession {
				branchExplicit = false
			}
		}
		if branch != "" && branchChosen && !branchExplicit {
			rule.Branches = appendUnique(rule.Branches, branch)
			continue
		}
		for _, session := range group {
			if session.Chosen {
				rule.Sessions = appendUnique(rule.Sessions, session.ID)
			}
		}
	}
	sort.Strings(rule.Branches)
	return rule
}

func deriveProjectFirst(projects []availProject) config.SelectionConfig {
	type build struct {
		projects []config.ProjectSelection
		sessions []string
	}
	builds := map[string]*build{}
	var order []string
	get := func(harness string) *build {
		row, ok := builds[harness]
		if !ok {
			row = &build{}
			builds[harness] = row
			order = append(order, harness)
		}
		return row
	}
	for _, project := range projects {
		if len(project.sessions) == 0 {
			continue
		}
		row := get(project.harness.String())
		sessions := make([]CheckoutSession, len(project.sessions))
		for index, session := range project.sessions {
			sessions[index] = CheckoutSession{
				ID:              session.candidate.SessionID.String(),
				Branch:          session.candidate.Branch,
				Chosen:          session.node.state == triAll,
				ExplicitSession: session.node.explicitSession,
			}
		}
		rule := DeriveCheckout(sessions)
		for _, sessionID := range rule.Sessions {
			row.sessions = appendUnique(row.sessions, sessionID)
		}
		switch {
		case rule.Whole:
			row.projects = appendEquivalent(row.projects, projectSelection(project, nil))
		case len(rule.Branches) > 0:
			row.projects = appendEquivalent(row.projects, projectSelection(project, rule.Branches))
		}
	}
	cfg := config.SelectionConfig{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{}}
	for _, harness := range order {
		row := builds[harness]
		if len(row.projects) == 0 && len(row.sessions) == 0 {
			continue
		}
		cfg.Harnesses[harness] = config.SelectionHarnessConfig{Projects: row.projects, Sessions: row.sessions}
	}
	if len(cfg.Harnesses) == 0 {
		cfg.Harnesses = nil
	}
	return cfg
}

func deriveHarnessFirst(roots []*sNode) config.SelectionConfig {
	if allRootsChecked(roots) {
		return config.SelectionConfig{Mode: config.SelectionModeAll}
	}
	cfg := config.SelectionConfig{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{}}
	for _, provider := range roots {
		if provider.state == triNone || provider.state == triLocked {
			continue
		}
		harness := harnessSelection(provider)
		if len(harness.Projects) == 0 && len(harness.Sessions) == 0 {
			continue
		}
		id := provider.id
		if provider.harness != "" && provider.harness != "unknown" {
			id = provider.harness
		}
		cfg.Harnesses[id] = harness
	}
	if len(cfg.Harnesses) == 0 {
		cfg.Harnesses = nil
	}
	return cfg
}

func allRootsChecked(roots []*sNode) bool {
	if len(roots) == 0 {
		return false
	}
	for _, root := range roots {
		if root.state != triAll {
			return false
		}
	}
	return true
}

func harnessSelection(provider *sNode) config.SelectionHarnessConfig {
	var harness config.SelectionHarnessConfig
	for _, remote := range provider.children {
		switch remote.state {
		case triNone, triLocked:
			continue
		case triAll:
			harness.Projects = append(harness.Projects, nodeProject(remote, nil))
		case triSome:
			var branches []string
			for _, worktree := range remote.children {
				switch worktree.state {
				case triAll:
					if branch := semanticBranch(worktree); branch != "" {
						branches = append(branches, branch)
					} else {
						collectChecked(worktree, &harness.Sessions)
					}
				case triSome:
					collectChecked(worktree, &harness.Sessions)
				}
			}
			if len(branches) > 0 {
				harness.Projects = append(harness.Projects, nodeProject(remote, branches))
			}
		}
	}
	return harness
}

func collectChecked(node *sNode, out *[]string) {
	if node == nil {
		return
	}
	switch node.state {
	case triAll:
		if node.kind == NodeKindSession || node.candidate.SessionID != "" || len(node.children) == 0 {
			id := node.id
			if node.candidate.SessionID != "" {
				id = node.candidate.SessionID.String()
			}
			*out = appendUnique(*out, id)
		}
		for _, child := range node.children {
			collectChecked(child, out)
		}
	case triSome:
		for _, child := range node.children {
			collectChecked(child, out)
		}
	}
}

func nodeProject(node *sNode, branches []string) config.ProjectSelection {
	selection := config.ProjectSelection{Branches: cloneStrings(branches)}
	if remote := node.candidate.GitRemote; remote != "" {
		selection.GitRemote = remote
	} else {
		selection.Name = firstNonEmpty(node.candidate.ProjectName, node.label)
	}
	if node.candidate.ClonePath != "" {
		selection.ClonePaths = []string{node.candidate.ClonePath.String()}
	}
	return selection
}

func projectSelection(project availProject, branches []string) config.ProjectSelection {
	selection := config.ProjectSelection{GitRemote: project.gitRemote, Branches: cloneStrings(branches)}
	if selection.GitRemote == "" {
		selection.Name = project.projectName
	}
	if project.clonePath != "" {
		selection.ClonePaths = []string{project.clonePath.String()}
	}
	return selection
}

func appendEquivalent(projects []config.ProjectSelection, incoming config.ProjectSelection) []config.ProjectSelection {
	key := equivalentKey(incoming)
	for index := range projects {
		if equivalentKey(projects[index]) != key {
			continue
		}
		if incoming.GitRemote != "" && (projects[index].GitRemote == "" || incoming.GitRemote < projects[index].GitRemote) {
			projects[index].GitRemote = incoming.GitRemote
		}
		for _, clonePath := range incoming.ClonePaths {
			projects[index].ClonePaths = appendUnique(projects[index].ClonePaths, clonePath)
		}
		sort.Strings(projects[index].ClonePaths)
		return projects
	}
	return append(projects, cloneProject(incoming))
}

func equivalentKey(project config.ProjectSelection) string {
	policy := branchPolicy(project.Branches)
	if remote := ingest.NormalizeRemoteForMatch(project.GitRemote); remote != "" {
		return "remote:" + remote + "\x00" + policy
	}
	if len(project.ClonePaths) > 0 {
		paths := append([]string(nil), project.ClonePaths...)
		sort.Strings(paths)
		return "clone:" + strings.Join(paths, "\x00") + "\x00" + policy
	}
	return "name:" + ingest.NormalizeProjectNameForMatch(project.Name) + "\x00" + policy
}

func branchPolicy(branches []string) string {
	if len(branches) == 0 {
		return "all"
	}
	return strings.Join(branches, "\x00")
}

func sanitizePlaceholderSelection(sel config.SelectionConfig, projects []availProject) (config.SelectionConfig, bool) {
	if len(sel.Harnesses) == 0 {
		return sel, false
	}
	sanitized := config.SelectionConfig{
		Mode:                  sel.Mode,
		AutoIngestNewBranches: sel.AutoIngestNewBranches,
		DeprecatedProviders:   cloneHarnesses(sel.DeprecatedProviders),
		Harnesses:             make(map[string]config.SelectionHarnessConfig, len(sel.Harnesses)),
	}
	changed := false
	for harness, configured := range sel.Harnesses {
		originallyUnrestricted := len(configured.Projects) == 0 && len(configured.Sessions) == 0
		next := config.SelectionHarnessConfig{
			Sessions:   cloneStrings(configured.Sessions),
			Exclusions: cloneExclusions(configured.Exclusions),
		}
		var allowed []ingest.DiscoveryCandidate
		var denied []ingest.DiscoveryCandidate
		for _, project := range configured.Projects {
			if !containsBranchless(project.Branches) {
				next.Projects = append(next.Projects, cloneProject(project))
				continue
			}
			changed = true
			allowed = append(allowed, placeholderProjectCandidates(harness, project, projects)...)
			branches := removeBranchless(project.Branches)
			if len(branches) == 0 {
				continue
			}
			project = cloneProject(project)
			project.Branches = branches
			next.Projects = append(next.Projects, project)
		}
		var exclusions []config.BranchExclusion
		for _, exclusion := range next.Exclusions.Branches {
			if !containsBranchless(exclusion.Branches) {
				exclusions = append(exclusions, exclusion)
				continue
			}
			changed = true
			denied = append(denied, placeholderExclusionCandidates(harness, exclusion.ClonePath, projects)...)
			branches := removeBranchless(exclusion.Branches)
			if len(branches) > 0 {
				exclusions = append(exclusions, config.BranchExclusion{ClonePath: exclusion.ClonePath, Branches: branches})
			}
		}
		next.Exclusions.Branches = exclusions
		for _, candidate := range allowed {
			if placeholderAdmits(harness, next, candidate, sel.AutoIngestNewBranches, originallyUnrestricted) {
				continue
			}
			next.Sessions = appendUnique(next.Sessions, candidate.SessionID.String())
		}
		for _, candidate := range denied {
			if !placeholderAdmits(harness, next, candidate, sel.AutoIngestNewBranches, originallyUnrestricted) {
				continue
			}
			next.Exclusions.Sessions = appendUnique(next.Exclusions.Sessions, candidate.SessionID.String())
		}
		if originallyUnrestricted || len(next.Projects) > 0 || len(next.Sessions) > 0 {
			sanitized.Harnesses[harness] = next
		}
	}
	if !changed {
		return sel, false
	}
	if len(sanitized.Harnesses) == 0 {
		sanitized.Harnesses = nil
	}
	return sanitized, true
}

func placeholderProjectCandidates(harness string, configured config.ProjectSelection, projects []availProject) []ingest.DiscoveryCandidate {
	var candidates []ingest.DiscoveryCandidate
	for _, project := range matchingProjects(harness, configured, projects) {
		for _, session := range project.sessions {
			if session.candidate.Branch == "" && session.candidate.SessionID != "" {
				candidates = append(candidates, session.candidate)
			}
		}
	}
	return candidates
}

func placeholderExclusionCandidates(harness, clonePath string, projects []availProject) []ingest.DiscoveryCandidate {
	var candidates []ingest.DiscoveryCandidate
	for _, project := range projects {
		if project.harness.String() != harness || project.clonePath.String() != clonePath {
			continue
		}
		for _, session := range project.sessions {
			if session.candidate.Branch == "" && session.candidate.SessionID != "" {
				candidates = append(candidates, session.candidate)
			}
		}
	}
	return candidates
}

func placeholderAdmits(harness string, configured config.SelectionHarnessConfig, candidate ingest.DiscoveryCandidate, auto bool, unrestricted bool) bool {
	if len(configured.Projects) == 0 && len(configured.Sessions) == 0 && !unrestricted {
		return false
	}
	positive := config.SelectionConfig{
		Mode:                  config.SelectionModeSelected,
		AutoIngestNewBranches: auto,
		Harnesses:             map[string]config.SelectionHarnessConfig{harness: configured},
	}
	positive.Harnesses[harness] = config.SelectionHarnessConfig{
		Projects: configured.Projects,
		Sessions: configured.Sessions,
	}
	matcher := config.CompileSelectionMatcher(positive)
	return matcher.MatchDiscoveryCandidate(candidate, auto) == ingest.BranchMatchYes
}

func containsBranchless(branches []string) bool {
	for _, branch := range branches {
		if isBranchless(branch) {
			return true
		}
	}
	return false
}

func removeBranchless(branches []string) []string {
	var kept []string
	for _, branch := range branches {
		if isBranchless(branch) {
			continue
		}
		kept = append(kept, branch)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

func sameAvailableProject(current, target config.ProjectSelection) bool {
	if len(target.ClonePaths) > 0 {
		for _, clonePath := range target.ClonePaths {
			if contains(current.ClonePaths, clonePath) {
				return true
			}
		}
		return false
	}
	if remote := ingest.NormalizeRemoteForMatch(target.GitRemote); remote != "" {
		return ingest.NormalizeRemoteForMatch(current.GitRemote) == remote
	}
	if name := ingest.NormalizeProjectNameForMatch(target.Name); name != "" {
		return ingest.NormalizeProjectNameForMatch(current.Name) == name
	}
	return false
}

func mergeProject(projects []config.ProjectSelection, incoming config.ProjectSelection) []config.ProjectSelection {
	key := mergeKey(incoming)
	for index := range projects {
		if mergeKey(projects[index]) != key {
			continue
		}
		for _, clonePath := range incoming.ClonePaths {
			projects[index].ClonePaths = appendUnique(projects[index].ClonePaths, clonePath)
		}
		if len(projects[index].Branches) > 0 && len(incoming.Branches) > 0 {
			for _, branch := range incoming.Branches {
				projects[index].Branches = appendUnique(projects[index].Branches, branch)
			}
			sort.Strings(projects[index].Branches)
		} else if len(incoming.Branches) == 0 {
			projects[index].Branches = nil
		}
		return projects
	}
	return append(projects, cloneProject(incoming))
}

func mergeKey(project config.ProjectSelection) string {
	if len(project.ClonePaths) > 0 {
		paths := append([]string(nil), project.ClonePaths...)
		sort.Strings(paths)
		return "clone:" + strings.Join(paths, "\x00")
	}
	if remote := ingest.NormalizeRemoteForMatch(project.GitRemote); remote != "" {
		return "remote:" + remote
	}
	return "name:" + ingest.NormalizeProjectNameForMatch(project.Name)
}

func mergeExclusions(current, residual config.SelectionExclusions) config.SelectionExclusions {
	merged := cloneExclusions(current)
	for _, sessionID := range residual.Sessions {
		merged.Sessions = appendUnique(merged.Sessions, sessionID)
	}
	for _, exclusion := range residual.Branches {
		merged.Branches = append(merged.Branches, config.BranchExclusion{
			ClonePath: exclusion.ClonePath,
			Branches:  cloneStrings(exclusion.Branches),
		})
	}
	return merged
}

func harnessPresent(configured config.SelectionHarnessConfig) bool {
	return len(configured.Projects) > 0 || len(configured.Sessions) > 0 || len(configured.Exclusions.Sessions) > 0 || len(configured.Exclusions.Branches) > 0
}

func cloneSelection(source config.SelectionConfig) config.SelectionConfig {
	clone := source
	clone.Harnesses = cloneHarnesses(source.Harnesses)
	clone.DeprecatedProviders = cloneHarnesses(source.DeprecatedProviders)
	return clone
}

func cloneHarnesses(source map[string]config.SelectionHarnessConfig) map[string]config.SelectionHarnessConfig {
	if source == nil {
		return nil
	}
	clone := make(map[string]config.SelectionHarnessConfig, len(source))
	for harness, configured := range source {
		clone[harness] = config.SelectionHarnessConfig{
			Projects:   cloneProjects(configured.Projects),
			Sessions:   cloneStrings(configured.Sessions),
			Exclusions: cloneExclusions(configured.Exclusions),
		}
	}
	return clone
}

func cloneProjects(source []config.ProjectSelection) []config.ProjectSelection {
	if source == nil {
		return nil
	}
	clone := make([]config.ProjectSelection, len(source))
	for index, project := range source {
		clone[index] = cloneProject(project)
	}
	return clone
}

func cloneProject(project config.ProjectSelection) config.ProjectSelection {
	project.ClonePaths = cloneStrings(project.ClonePaths)
	project.Branches = cloneStrings(project.Branches)
	return project
}

func cloneExclusions(source config.SelectionExclusions) config.SelectionExclusions {
	clone := config.SelectionExclusions{Sessions: cloneStrings(source.Sessions)}
	if source.Branches != nil {
		clone.Branches = make([]config.BranchExclusion, len(source.Branches))
		for index, exclusion := range source.Branches {
			clone.Branches[index] = exclusion
			clone.Branches[index].Branches = cloneStrings(exclusion.Branches)
		}
	}
	return clone
}

func cloneStrings(source []string) []string {
	if source == nil {
		return nil
	}
	return append([]string(nil), source...)
}

func appendUnique(values []string, value string) []string {
	if value == "" || contains(values, value) {
		return values
	}
	return append(values, value)
}

func contains(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" && value != "unknown" {
			return value
		}
	}
	return ""
}

func walkSNodes(node *sNode, visit func(*sNode)) {
	if node == nil {
		return
	}
	visit(node)
	for _, child := range node.children {
		walkSNodes(child, visit)
	}
}
