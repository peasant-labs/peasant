package settings

import (
	"fmt"
	"sort"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/kit"
)

// TreeSelection is the central selection contract the [Tree] field round-trips
// through. Its fields ARE the real config types (config.SelectionMode and
// config.SelectionHarnessConfig), so a selection edited in the TUI is the exact
// value the ingest pipeline, push, discovery lists, and prune already consume -
// there is no parallel model to drift out of sync (zero mapping drift).
//
// The round-trip kit.TreeNode/TriState <-> TreeSelection <-> config.SelectionConfig
// obeys the ratified rules:
//
//   - every provider root Checked in the legacy harness-first settings shape
//     -> Mode=all, Harnesses empty
//   - a project-first kickstart forest -> Mode=selected, including select-all,
//     with the exact current ClonePaths
//   - anything else -> Mode=selected
//   - a remote (project) Checked   -> ProjectSelection with Branches nil (all
//     branches of that project)
//   - a remote Partial             -> Branches is the subset of its Checked
//     worktrees; a worktree that is only partially selected contributes its
//     Checked sessions to Harnesses[h].Sessions instead
//   - a session leaf Checked whose worktree is not wholly Checked -> the
//     session id is added to Harnesses[h].Sessions
//   - a Conflict node               -> DISPLAY ONLY: never persisted, and it
//     fails Validate so Commit is blocked fail-closed
//   - an Unchecked node             -> omitted everywhere
type TreeSelection struct {
	// Mode controls whether the selection filter is active.
	Mode config.SelectionMode
	// Harnesses maps harness name to its selection allowlist. Empty when Mode
	// is all.
	Harnesses map[string]config.SelectionHarnessConfig
}

// MetaRemote and MetaBranch are the Meta keys a scanner attaches to remote and
// worktree nodes; the round-trip reads them to recover the git remote and branch
// identity for a selection. They are exported so a real scanner adapter (the
// kickstart slice) writes the exact keys this round-trip reads back.
const (
	MetaRemote = "remote"
	MetaBranch = "branch"
	// MetaProjectIdentity carries the scanner's stable harness + physical-path
	// identity. It is never a display label.
	MetaProjectIdentity = "projectIdentity"
	// MetaProjectHarness carries the harness component of a project identity on
	// the project root. Session leaves continue to use MetaHarness.
	MetaProjectHarness = "projectHarness"
	// MetaProjectName keeps the discovery name separate from a rendered label.
	// A non-Git label may add short path context and must never be persisted as
	// the project's name.
	MetaProjectName = "projectName"
	// MetaClonePath carries a resolver-produced physical identity path.
	MetaClonePath = "clonePath"
	// MetaRemoteMultiplicity and MetaNameMultiplicity carry the complete-cohort
	// uniqueness proof used by DiscoveryCandidate matching.
	MetaRemoteMultiplicity = "remoteMultiplicity"
	MetaNameMultiplicity   = "nameMultiplicity"
	// Multiplicity values are named rather than inferred from absent metadata.
	// An absent or unknown value remains fail-closed.
	MetaMultiplicityUnproven  = "unproven"
	MetaMultiplicityUnique    = "unique"
	MetaMultiplicityAmbiguous = "ambiguous"
	// MetaHarness is the Meta key a project-first scanner attaches to each
	// SESSION leaf, naming the harness that recorded it. The persisted
	// config.SelectionConfig is harness-keyed, but the FTUE-faithful selection
	// tree groups PROJECT -> BRANCH -> SESSION with no harness grouping axis
	// (the harness is a property of a session, not a top-level bucket). When
	// leaves carry this key, [FromTreeNodes] recovers the harness partition from
	// the leaves instead of from a top-level provider root, so the same
	// round-trip serves both the harness-first forest the general settings
	// fixtures use and the project-first forest kickstart's scanner builds.
	MetaHarness = "harness"
	// MetaIngested marks a session node whose transcript the local store already
	// holds. A scanner sets it to MetaIngestedValue after it reads the ingested
	// session ids from the store, so a later view can split the list into
	// already-ingested and not-yet-ingested sessions. It is display-only context
	// and never affects the derived SelectionConfig.
	MetaIngested = kit.MetaIngested
	// MetaIngestedValue is the value MetaIngested carries when set.
	MetaIngestedValue = kit.MetaIngestedValue
	// MetaChildCount carries how many child (subagent) sessions a parent session
	// groups. A parent session is a LEAF row that summarises its subagents, so
	// the count is display-only context and never affects the derived
	// SelectionConfig - the ingest side expands a selected parent to its children
	// from the discovery listing, not from the tree.
	MetaChildCount = kit.MetaChildCount
)

// metaExplicitBranchSelection marks a project whose saved rule names branches.
// When every currently available named branch is checked, the tree normally
// rolls the project up to Checked, which is otherwise indistinguishable from an
// unrestricted whole-project rule. This marker keeps the saved branch policy
// explicit across a no-edit save. It is editor state only and is never persisted.
const metaExplicitBranchSelection = "explicitBranchSelection"

// metaRemote and metaBranch are short unexported aliases of the exported
// MetaRemote / MetaBranch keys, used by the round-trip call sites in this file
// to keep those lines terse. They are not deprecated; they intentionally track
// the exported keys.
const (
	metaRemote = MetaRemote
	metaBranch = MetaBranch
)

// gitRemoteOf returns the git remote URL a remote node carries, or "" when it
// only has a folder-name identity.
func gitRemoteOf(n *kit.TreeNode) string {
	if n.Meta == nil {
		return ""
	}
	return n.Meta[metaRemote]
}

// projectNameOf returns discovery identity text, not rendered row text. Older
// harness-first fixture trees do not carry MetaProjectName, so their label
// remains the compatibility fallback.
func projectNameOf(n *kit.TreeNode) string {
	if n.Meta != nil {
		if name := n.Meta[MetaProjectName]; name != "" {
			return name
		}
	}
	return n.Label
}

func clonePathOf(n *kit.TreeNode) ingest.ClonePath {
	if n.Meta == nil {
		return ""
	}
	return ingest.ClonePath(n.Meta[MetaClonePath])
}

func projectIdentityOf(n *kit.TreeNode) string {
	if n.Meta != nil {
		if identity := n.Meta[MetaProjectIdentity]; identity != "" {
			return identity
		}
	}
	return n.ID
}

// branchOf returns the branch name a worktree node carries, falling back to its
// label when no explicit branch meta is present.
func branchOf(n *kit.TreeNode) string {
	if n.Meta != nil {
		if b := n.Meta[metaBranch]; b != "" {
			return b
		}
	}
	return n.Label
}

// projectSelectionFor builds a ProjectSelection identifying a remote node,
// preferring its git remote and falling back to a folder-name identity.
func projectSelectionFor(remote *kit.TreeNode, branches []string) config.ProjectSelection {
	ps := config.ProjectSelection{Branches: branches}
	if r := gitRemoteOf(remote); r != "" {
		ps.GitRemote = r
	} else {
		ps.Name = projectNameOf(remote)
	}
	if clonePath := clonePathOf(remote); clonePath != "" {
		ps.ClonePaths = []string{clonePath.String()}
	}
	return ps
}

// allRootsChecked reports whether every root node is cleanly Checked. It is used
// only by the harness-first compatibility shape; project-first kickstart trees
// always save an exact selected-mode list. An empty forest is not "all".
func allRootsChecked(roots []*kit.TreeNode) bool {
	if len(roots) == 0 {
		return false
	}
	for _, r := range roots {
		if r.State != kit.Checked {
			return false
		}
	}
	return true
}

// HasConflict reports whether any node in the forest is in the display-only
// Conflict state. A Conflict never persists, and its presence blocks Commit.
func HasConflict(roots []*kit.TreeNode) bool {
	found := false
	var visit func(n *kit.TreeNode)
	visit = func(n *kit.TreeNode) {
		if n.State == kit.Conflict {
			found = true
		}
		for _, c := range n.Children {
			visit(c)
		}
	}
	for _, r := range roots {
		visit(r)
	}
	return found
}

// FromTreeNodes derives a TreeSelection from either forest shape, applying the
// ratified round-trip rules. It dispatches on the shape detected by
// [isProjectFirstForest]: a project-first (project->branch->session) forest —
// the FTUE-faithful onboarding tree — is handled by [fromProjectFirstForest],
// while a harness-first forest (the general settings fixtures) takes the
// original harness->project->session derivation below. Conflict nodes are
// omitted from the persisted shape (they never round-trip); use [HasConflict]
// to block Commit when any are present.
func FromTreeNodes(roots []*kit.TreeNode) TreeSelection {
	if isProjectFirstForest(roots) {
		return fromProjectFirstForest(roots)
	}
	if allRootsChecked(roots) {
		return TreeSelection{Mode: config.SelectionModeAll}
	}
	ts := TreeSelection{
		Mode:      config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{},
	}
	for _, provider := range roots {
		if provider.State == kit.Unchecked || provider.State == kit.Conflict {
			continue
		}
		hc := harnessSelectionFor(provider)
		if len(hc.Projects) > 0 || len(hc.Sessions) > 0 {
			ts.Harnesses[provider.ID] = hc
		}
	}
	if len(ts.Harnesses) == 0 {
		ts.Harnesses = nil
	}
	return ts
}

// harnessOf returns the harness a session leaf carries in its Meta, or "" when
// it carries none.
func harnessOf(n *kit.TreeNode) string {
	if n.Meta == nil {
		return ""
	}
	return n.Meta[MetaHarness]
}

// isProjectFirstForest reports whether roots is the FTUE-faithful
// PROJECT -> BRANCH -> SESSION shape, detected by any session node carrying a
// MetaHarness key. A session may itself nest child (subagent) sessions, so the
// detector inspects every node, not just leaves. A harness-first forest (the
// general settings fixtures) carries no MetaHarness and takes the original
// derivation.
func isProjectFirstForest(roots []*kit.TreeNode) bool {
	var any func(n *kit.TreeNode) bool
	any = func(n *kit.TreeNode) bool {
		if harnessOf(n) != "" {
			return true
		}
		for _, c := range n.Children {
			if any(c) {
				return true
			}
		}
		return false
	}
	for _, r := range roots {
		if any(r) {
			return true
		}
	}
	return false
}

// harnessesUnder returns the distinct harnesses of every session node in n's
// subtree, in first-seen order. A session that nests child (subagent) sessions
// is an interior node that still carries MetaHarness, so the walk collects the
// harness from every node that carries one, not only leaves.
func harnessesUnder(n *kit.TreeNode) []string {
	seen := map[string]bool{}
	var order []string
	var walk func(m *kit.TreeNode)
	walk = func(m *kit.TreeNode) {
		if h := harnessOf(m); h != "" && !seen[h] {
			seen[h] = true
			order = append(order, h)
		}
		for _, c := range m.Children {
			walk(c)
		}
	}
	walk(n)
	return order
}

// harnessBuild accumulates one harness's allowlist while the project-first
// forest is walked. Physical clones of one normalized remote/name merge into a
// single ProjectSelection with an exact ClonePaths list and one shared branch
// policy.
type harnessBuild struct {
	order    []string
	projects map[string]*projectBuild
	sessions []string
}

type projectBuild struct {
	selection   config.ProjectSelection
	branches    map[string]struct{}
	allBranches bool
}

func projectBuildKey(remote, name, identity string, pathBound bool) string {
	if normalized := ingest.NormalizeRemoteForMatch(remote); normalized != "" {
		return "remote:" + normalized
	}
	if normalized := ingest.NormalizeProjectNameForMatch(name); normalized != "" {
		return "name:" + normalized
	}
	if pathBound {
		return "identity:" + identity
	}
	return "unidentified:" + identity
}

func (hb *harnessBuild) project(remote, name string, clonePath ingest.ClonePath, identity string) *projectBuild {
	if hb.projects == nil {
		hb.projects = map[string]*projectBuild{}
	}
	key := projectBuildKey(remote, name, identity, clonePath != "")
	pb, ok := hb.projects[key]
	if !ok {
		pb = &projectBuild{
			selection: config.ProjectSelection{GitRemote: remote, Name: name},
			branches:  map[string]struct{}{},
		}
		hb.projects[key] = pb
		hb.order = append(hb.order, key)
	}
	if clonePath != "" && !containsString(pb.selection.ClonePaths, clonePath.String()) {
		pb.selection.ClonePaths = append(pb.selection.ClonePaths, clonePath.String())
	}
	return pb
}

// fromProjectFirstForest derives the harness-keyed TreeSelection from a
// PROJECT -> BRANCH -> SESSION forest, recovering each session's harness from
// its MetaHarness leaf key. Even when every current project is checked, this
// shape persists Mode=selected with the exact current physical-clone list. A
// project discovered on a later run therefore starts clear.
func fromProjectFirstForest(roots []*kit.TreeNode) TreeSelection {
	builds := map[string]*harnessBuild{}
	order := []string{}
	get := func(h string) *harnessBuild {
		hb, ok := builds[h]
		if !ok {
			hb = &harnessBuild{}
			builds[h] = hb
			order = append(order, h)
		}
		return hb
	}
	addProjectAllBranches := func(project *kit.TreeNode, remote, name string) {
		for _, h := range harnessesUnder(project) {
			pb := get(h).project(remote, name, clonePathOf(project), projectIdentityOf(project))
			pb.allBranches = true
			pb.branches = nil
		}
	}
	addBranch := func(project, scope *kit.TreeNode, remote, name, branch string) {
		for _, h := range harnessesUnder(scope) {
			pb := get(h).project(remote, name, clonePathOf(project), projectIdentityOf(project))
			if !pb.allBranches {
				pb.branches[branch] = struct{}{}
			}
		}
	}
	for _, project := range roots {
		if project.State == kit.Unchecked || project.State == kit.Conflict {
			continue
		}
		remote := gitRemoteOf(project)
		name := projectNameOf(project)
		if remote != "" {
			name = ""
		}
		switch project.State {
		case kit.Checked:
			if metaOf(project, metaExplicitBranchSelection) == "true" {
				for _, branch := range project.Children {
					addBranch(project, branch, remote, name, branchOf(branch))
				}
			} else {
				addProjectAllBranches(project, remote, name)
			}
		case kit.Partial:
			for _, branch := range project.Children {
				switch branch.State {
				case kit.Checked:
					addBranch(project, branch, remote, name, branchOf(branch))
				case kit.Partial:
					for _, session := range branch.Children {
						collectCheckedSessionIDs(session, get)
					}
				}
			}
		}
	}
	ts := TreeSelection{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{}}
	for _, h := range order {
		hb := builds[h]
		var projects []config.ProjectSelection
		for _, key := range hb.order {
			pb := hb.projects[key]
			selection := pb.selection
			if !pb.allBranches {
				for branch := range pb.branches {
					selection.Branches = append(selection.Branches, branch)
				}
				sort.Strings(selection.Branches)
			}
			projects = append(projects, selection)
		}
		if len(projects) == 0 && len(hb.sessions) == 0 {
			continue
		}
		ts.Harnesses[h] = config.SelectionHarnessConfig{Projects: projects, Sessions: hb.sessions}
	}
	if len(ts.Harnesses) == 0 {
		ts.Harnesses = nil
	}
	return ts
}

// collectCheckedSessionIDs adds the ids of the Checked sessions in a session
// subtree to their harness allowlists. A session may nest child (subagent)
// sessions, so the walk is recursive: a Checked session contributes its whole
// subtree (every nested session id), while a Partial session is not itself
// wholly chosen and the walk descends to find the Checked sessions within it. A
// flat session leaf (no children) is Checked or Unchecked, so this reduces to
// "add a checked leaf's id" and preserves the original round-trip.
func collectCheckedSessionIDs(n *kit.TreeNode, get func(string) *harnessBuild) {
	switch n.State {
	case kit.Checked:
		addSubtreeSessionIDs(n, get)
	case kit.Partial:
		for _, c := range n.Children {
			collectCheckedSessionIDs(c, get)
		}
	}
}

// addSubtreeSessionIDs adds every session node id in n's subtree (n included) to
// its harness allowlist, keyed by the MetaHarness each node carries.
func addSubtreeSessionIDs(n *kit.TreeNode, get func(string) *harnessBuild) {
	if h := harnessOf(n); h != "" {
		get(h).sessions = append(get(h).sessions, n.ID)
	}
	for _, c := range n.Children {
		addSubtreeSessionIDs(c, get)
	}
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// collectCheckedSubtreeIDs appends the ids of the Checked sessions in a session
// subtree to out. Its harness is fixed by the enclosing provider root (not a
// per-node meta), so unlike collectCheckedSessionIDs it collects raw ids. A
// Checked session contributes its whole subtree; a Partial session descends to
// its Checked descendants. A flat leaf reduces to "add a checked leaf's id".
func collectCheckedSubtreeIDs(n *kit.TreeNode, out *[]string) {
	switch n.State {
	case kit.Checked:
		var add func(m *kit.TreeNode)
		add = func(m *kit.TreeNode) {
			*out = append(*out, m.ID)
			for _, c := range m.Children {
				add(c)
			}
		}
		add(n)
	case kit.Partial:
		for _, c := range n.Children {
			collectCheckedSubtreeIDs(c, out)
		}
	}
}

// harnessSelectionFor walks one provider's remotes and produces its allowlist.
func harnessSelectionFor(provider *kit.TreeNode) config.SelectionHarnessConfig {
	var hc config.SelectionHarnessConfig
	for _, remote := range provider.Children {
		switch remote.State {
		case kit.Unchecked, kit.Conflict:
			continue
		case kit.Checked:
			hc.Projects = append(hc.Projects, projectSelectionFor(remote, nil))
		case kit.Partial:
			var branches []string
			for _, worktree := range remote.Children {
				switch worktree.State {
				case kit.Checked:
					branches = append(branches, branchOf(worktree))
				case kit.Partial:
					for _, session := range worktree.Children {
						collectCheckedSubtreeIDs(session, &hc.Sessions)
					}
				}
			}
			if len(branches) > 0 {
				hc.Projects = append(hc.Projects, projectSelectionFor(remote, branches))
			}
		}
	}
	return hc
}

// ToSelectionConfig converts a TreeSelection into the config.SelectionConfig
// the persisted config carries. It preserves an existing AutoIngestNewBranches
// flag from prior, which the caller passes through when merging into a draft.
func (ts TreeSelection) ToSelectionConfig(autoIngestNewBranches bool) config.SelectionConfig {
	return config.SelectionConfig{
		Mode:                  ts.Mode,
		AutoIngestNewBranches: autoIngestNewBranches,
		Harnesses:             ts.Harnesses,
	}
}

// Validate reports whether a TreeSelection is committable. A Mode outside the
// known set, or Harnesses populated while Mode is all, is rejected with an
// actionable error. (Conflict is a property of the source TREE, not the derived
// TreeSelection, so it is checked on the tree via [HasConflict] before this.)
func (ts TreeSelection) Validate() error {
	if !ts.Mode.IsValid() {
		return fmt.Errorf(
			"invalid transcript selection.\n"+
				"what: selection mode %q is not one of the known modes.\n"+
				"why: a settings edit produced a mode the config layer does not accept.\n"+
				"where: settings.TreeSelection.Validate.\n"+
				"fix: choose either %q (import everything) or %q (import a chosen subset) and retry.",
			ts.Mode, config.SelectionModeAll, config.SelectionModeSelected)
	}
	if ts.Mode == config.SelectionModeAll && len(ts.Harnesses) > 0 {
		return fmt.Errorf(
			"inconsistent transcript selection.\n" +
				"what: mode is \"all\" but a per-harness allowlist is also set.\n" +
				"why: \"all\" means every project is included, so an allowlist would be silently ignored.\n" +
				"where: settings.TreeSelection.Validate.\n" +
				"fix: either clear the allowlist to keep \"all\", or switch to \"selected\".")
	}
	return nil
}

// UnmatchedBaseline is the part of a saved selected-mode configuration that the
// current tree cannot offer for editing. MergeSelection carries it forward so a
// missing clone, branch, or explicit session is not deleted as a side effect of
// saving another visible edit.
type UnmatchedBaseline struct {
	Harnesses map[string]config.SelectionHarnessConfig
	// branches are unavailable branch names under an otherwise available
	// project. They are merged only when that available project remains selected;
	// clearing the project must not recreate it merely to preserve a child the
	// user could not see.
	branches []unmatchedBranchSelection
}

type unmatchedBranchSelection struct {
	harness  string
	project  config.ProjectSelection
	branches []string
}

type availableSession struct {
	node      *kit.TreeNode
	candidate ingest.DiscoveryCandidate
}

type availableProject struct {
	node               *kit.TreeNode
	harness            ingest.Harness
	identity           string
	gitRemote          string
	projectName        string
	clonePath          ingest.ClonePath
	remoteMultiplicity ingest.DiscoveryIdentityMultiplicity
	nameMultiplicity   ingest.DiscoveryIdentityMultiplicity
	branches           map[string]struct{}
	sessions           []availableSession
}

func (p availableProject) candidate() ingest.DiscoveryCandidate {
	return ingest.DiscoveryCandidate{
		Harness:            p.harness,
		GitRemote:          p.gitRemote,
		ProjectName:        p.projectName,
		ClonePath:          p.clonePath,
		RemoteMultiplicity: p.remoteMultiplicity,
		NameMultiplicity:   p.nameMultiplicity,
	}
}

// ApplyExistingSelection remains the compatibility entry point for callers that
// only need tree state. The implementation is exactly PrepopulateSelection; no
// positional or second matcher path exists.
func ApplyExistingSelection(roots []*kit.TreeNode, sel config.SelectionConfig) {
	_ = PrepopulateSelection(roots, sel)
}

// PrepopulateSelection applies one selected-mode configuration to a freshly
// loaded forest through the canonical candidate matcher and returns the saved
// choices that are unavailable in that forest. Mode all is deliberately not
// compiled as selected; conversion of that legacy policy must produce an exact
// selected-mode configuration before it reaches this function.
func PrepopulateSelection(roots []*kit.TreeNode, sel config.SelectionConfig) UnmatchedBaseline {
	if sel.Mode != config.SelectionModeSelected {
		return UnmatchedBaseline{}
	}

	projects := availableProjectsFromForest(roots)
	markExplicitBranchSelections(sel, projects)
	matcher := config.CompileSelectionMatcher(sel)
	for projectIndex := range projects {
		project := &projects[projectIndex]
		for sessionIndex := range project.sessions {
			session := &project.sessions[sessionIndex]
			switch matcher.MatchDiscoveryCandidate(session.candidate, sel.AutoIngestNewBranches) {
			case ingest.BranchMatchYes:
				session.node.State = kit.Checked
			case ingest.BranchMatchWithheldConflict:
				session.node.State = kit.Conflict
			default:
				session.node.State = kit.Unchecked
			}
		}
	}
	for _, root := range roots {
		rollup(root)
	}
	return unmatchedSelection(sel, projects)
}

// markExplicitBranchSelections preserves the distinction between a configured
// branch list and an unrestricted whole-project rule. Without the marker, a
// project whose complete current branch set happens to be selected rolls up to
// Checked and a no-edit save widens its Branches list to nil.
func markExplicitBranchSelections(sel config.SelectionConfig, projects []availableProject) {
	for _, project := range projects {
		if project.node.Meta != nil {
			delete(project.node.Meta, metaExplicitBranchSelection)
		}
	}
	for harness, configured := range sel.Harnesses {
		for _, configuredProject := range configured.Projects {
			if len(configuredProject.Branches) == 0 {
				continue
			}
			for _, project := range matchingAvailableProjects(harness, configuredProject, projects) {
				if project.node.Meta == nil {
					project.node.Meta = map[string]string{}
				}
				project.node.Meta[metaExplicitBranchSelection] = "true"
			}
		}
	}
}

func availableProjectsFromForest(roots []*kit.TreeNode) []availableProject {
	var projects []availableProject
	if isProjectFirstForest(roots) {
		for _, projectNode := range roots {
			harness := ingest.Harness(metaOf(projectNode, MetaProjectHarness))
			if harness == "" {
				if harnesses := harnessesUnder(projectNode); len(harnesses) > 0 {
					harness = ingest.Harness(harnesses[0])
				}
			}
			project := availableProject{
				node:               projectNode,
				harness:            harness,
				identity:           projectIdentityOf(projectNode),
				gitRemote:          gitRemoteOf(projectNode),
				projectName:        projectNameOf(projectNode),
				clonePath:          clonePathOf(projectNode),
				remoteMultiplicity: multiplicityOf(projectNode, MetaRemoteMultiplicity),
				nameMultiplicity:   multiplicityOf(projectNode, MetaNameMultiplicity),
				branches:           map[string]struct{}{},
			}
			for _, branchNode := range projectNode.Children {
				branch := branchOf(branchNode)
				project.branches[branch] = struct{}{}
				for _, sessionNode := range branchNode.Children {
					appendAvailableSessions(&project, sessionNode, branch)
				}
			}
			projects = append(projects, project)
		}
	} else {
		for _, provider := range roots {
			harness := ingest.Harness(provider.ID)
			for _, projectNode := range provider.Children {
				project := availableProject{
					node:               projectNode,
					harness:            harness,
					identity:           harness.String() + "\x00" + projectIdentityOf(projectNode),
					gitRemote:          gitRemoteOf(projectNode),
					projectName:        projectNameOf(projectNode),
					clonePath:          clonePathOf(projectNode),
					remoteMultiplicity: multiplicityOf(projectNode, MetaRemoteMultiplicity),
					nameMultiplicity:   multiplicityOf(projectNode, MetaNameMultiplicity),
					branches:           map[string]struct{}{},
				}
				for _, branchNode := range projectNode.Children {
					branch := branchOf(branchNode)
					project.branches[branch] = struct{}{}
					for _, sessionNode := range branchNode.Children {
						appendAvailableSessions(&project, sessionNode, branch)
					}
				}
				projects = append(projects, project)
			}
		}
	}
	annotateAvailableMultiplicity(projects)
	return projects
}

func appendAvailableSessions(project *availableProject, node *kit.TreeNode, branch string) {
	harness := project.harness
	if value := harnessOf(node); value != "" {
		harness = ingest.Harness(value)
	}
	gitRemote := gitRemoteOf(node)
	if gitRemote == "" {
		gitRemote = project.gitRemote
	}
	projectName := metaOf(node, MetaProjectName)
	if projectName == "" {
		projectName = project.projectName
	}
	clonePath := clonePathOf(node)
	if clonePath == "" {
		clonePath = project.clonePath
	}
	project.sessions = append(project.sessions, availableSession{
		node: node,
		candidate: ingest.DiscoveryCandidate{
			Harness:            harness,
			GitRemote:          gitRemote,
			ProjectName:        projectName,
			ClonePath:          clonePath,
			Branch:             branch,
			SessionID:          ingest.SessionID(node.ID),
			RemoteMultiplicity: multiplicityOf(node, MetaRemoteMultiplicity),
			NameMultiplicity:   multiplicityOf(node, MetaNameMultiplicity),
		},
	})
	for _, child := range node.Children {
		appendAvailableSessions(project, child, branch)
	}
}

func multiplicityOf(node *kit.TreeNode, key string) ingest.DiscoveryIdentityMultiplicity {
	switch metaOf(node, key) {
	case MetaMultiplicityUnique:
		return ingest.DiscoveryIdentityUnique
	case MetaMultiplicityAmbiguous:
		return ingest.DiscoveryIdentityAmbiguous
	default:
		return ingest.DiscoveryIdentityUnproven
	}
}

type availableMultiplicityKey struct {
	harness ingest.Harness
	text    string
}

func annotateAvailableMultiplicity(projects []availableProject) {
	remoteIdentities := map[availableMultiplicityKey]map[string]struct{}{}
	nameIdentities := map[availableMultiplicityKey]map[string]struct{}{}
	for _, project := range projects {
		addAvailableIdentity(remoteIdentities, availableMultiplicityKey{harness: project.harness, text: ingest.NormalizeRemoteForMatch(project.gitRemote)}, project.identity)
		addAvailableIdentity(nameIdentities, availableMultiplicityKey{harness: project.harness, text: ingest.NormalizeProjectNameForMatch(project.projectName)}, project.identity)
		for _, session := range project.sessions {
			addAvailableIdentity(remoteIdentities, availableMultiplicityKey{harness: session.candidate.Harness, text: ingest.NormalizeRemoteForMatch(session.candidate.GitRemote)}, project.identity)
			addAvailableIdentity(nameIdentities, availableMultiplicityKey{harness: session.candidate.Harness, text: ingest.NormalizeProjectNameForMatch(session.candidate.ProjectName)}, project.identity)
		}
	}
	for projectIndex := range projects {
		project := &projects[projectIndex]
		if project.remoteMultiplicity == ingest.DiscoveryIdentityUnproven {
			project.remoteMultiplicity = availableMultiplicity(remoteIdentities, project.harness, ingest.NormalizeRemoteForMatch(project.gitRemote))
		}
		if project.nameMultiplicity == ingest.DiscoveryIdentityUnproven {
			project.nameMultiplicity = availableMultiplicity(nameIdentities, project.harness, ingest.NormalizeProjectNameForMatch(project.projectName))
		}
		for sessionIndex := range project.sessions {
			candidate := &project.sessions[sessionIndex].candidate
			if candidate.RemoteMultiplicity == ingest.DiscoveryIdentityUnproven {
				candidate.RemoteMultiplicity = availableMultiplicity(remoteIdentities, candidate.Harness, ingest.NormalizeRemoteForMatch(candidate.GitRemote))
			}
			if candidate.NameMultiplicity == ingest.DiscoveryIdentityUnproven {
				candidate.NameMultiplicity = availableMultiplicity(nameIdentities, candidate.Harness, ingest.NormalizeProjectNameForMatch(candidate.ProjectName))
			}
		}
	}
}

func addAvailableIdentity(cohorts map[availableMultiplicityKey]map[string]struct{}, key availableMultiplicityKey, identity string) {
	if key.text == "" || identity == "" {
		return
	}
	if cohorts[key] == nil {
		cohorts[key] = map[string]struct{}{}
	}
	cohorts[key][identity] = struct{}{}
}

func availableMultiplicity(cohorts map[availableMultiplicityKey]map[string]struct{}, harness ingest.Harness, text string) ingest.DiscoveryIdentityMultiplicity {
	if text == "" {
		return ingest.DiscoveryIdentityUnique
	}
	if len(cohorts[availableMultiplicityKey{harness: harness, text: text}]) == 1 {
		return ingest.DiscoveryIdentityUnique
	}
	return ingest.DiscoveryIdentityAmbiguous
}

func unmatchedSelection(sel config.SelectionConfig, projects []availableProject) UnmatchedBaseline {
	unmatched := UnmatchedBaseline{Harnesses: map[string]config.SelectionHarnessConfig{}}
	availableSessions := map[string]map[string]struct{}{}
	for _, project := range projects {
		for _, session := range project.sessions {
			harness := session.candidate.Harness.String()
			if availableSessions[harness] == nil {
				availableSessions[harness] = map[string]struct{}{}
			}
			availableSessions[harness][session.node.ID] = struct{}{}
		}
	}

	for harness, configured := range sel.Harnesses {
		residual := config.SelectionHarnessConfig{}
		for _, sessionID := range configured.Sessions {
			if _, available := availableSessions[harness][sessionID]; !available {
				residual.Sessions = appendUniqueString(residual.Sessions, sessionID)
			}
		}
		for _, configuredProject := range configured.Projects {
			matching := matchingAvailableProjects(harness, configuredProject, projects)
			if len(matching) == 0 {
				residual.Projects = append(residual.Projects, cloneProjectSelection(configuredProject))
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
				missingPaths := make([]string, 0, len(configuredProject.ClonePaths))
				for _, clonePath := range configuredProject.ClonePaths {
					if _, available := matchedPaths[clonePath]; !available {
						missingPaths = appendUniqueString(missingPaths, clonePath)
					}
				}
				if len(missingPaths) > 0 {
					hasMissingPaths = true
					missing := cloneProjectSelection(configuredProject)
					missing.ClonePaths = missingPaths
					residual.Projects = append(residual.Projects, missing)
				}
			}

			if len(configuredProject.Branches) > 0 {
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
						missingBranches = appendUniqueString(missingBranches, branch)
					}
				}
				if len(missingBranches) > 0 {
					// A missing clone-path residual already carries the complete
					// configured branch policy. Otherwise preserve missing branches
					// conditionally: only while an available matching project remains
					// selected in the current tree.
					if !hasMissingPaths {
						target := cloneProjectSelection(configuredProject)
						target.Branches = nil
						if len(configuredProject.ClonePaths) > 0 {
							target.ClonePaths = nil
							for _, project := range matching {
								if project.clonePath != "" {
									target.ClonePaths = appendUniqueString(target.ClonePaths, project.clonePath.String())
								}
							}
						}
						unmatched.branches = append(unmatched.branches, unmatchedBranchSelection{
							harness:  harness,
							project:  target,
							branches: missingBranches,
						})
					}
				}
			}
		}
		if len(residual.Projects) > 0 || len(residual.Sessions) > 0 {
			unmatched.Harnesses[harness] = residual
		}
	}
	if len(unmatched.Harnesses) == 0 {
		unmatched.Harnesses = nil
	}
	return unmatched
}

func matchingAvailableProjects(harness string, configured config.ProjectSelection, projects []availableProject) []availableProject {
	selection := config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			harness: {Projects: []config.ProjectSelection{configured}},
		},
	}
	matcher := config.CompileSelectionMatcher(selection)
	var matching []availableProject
	for _, project := range projects {
		if string(project.harness) == harness && matcher.MatchesCandidate(project.candidate()) {
			matching = append(matching, project)
		}
	}
	return matching
}

// MergeSelection combines the currently visible tree derivation with the
// unavailable part of the saved baseline. Available choices come only from the
// tree, so clearing one removes it. Unavailable choices come only from the
// baseline, so a refresh or unrelated edit cannot remove them.
func MergeSelection(current TreeSelection, unmatched UnmatchedBaseline) TreeSelection {
	if current.Mode != config.SelectionModeSelected || (len(unmatched.Harnesses) == 0 && len(unmatched.branches) == 0) {
		return current
	}
	merged := TreeSelection{Mode: current.Mode, Harnesses: cloneHarnessMap(current.Harnesses)}
	if merged.Harnesses == nil {
		merged.Harnesses = map[string]config.SelectionHarnessConfig{}
	}
	for _, missing := range unmatched.branches {
		result, ok := merged.Harnesses[missing.harness]
		if !ok {
			continue
		}
		for index := range result.Projects {
			project := &result.Projects[index]
			if !sameAvailableProject(*project, missing.project) {
				continue
			}
			// nil branches already means every branch, including an unavailable
			// saved one. An explicit list needs the unavailable names restored.
			if len(project.Branches) > 0 {
				for _, branch := range missing.branches {
					project.Branches = appendUniqueString(project.Branches, branch)
				}
				sort.Strings(project.Branches)
			}
		}
		merged.Harnesses[missing.harness] = result
	}
	for harness, residual := range unmatched.Harnesses {
		result := merged.Harnesses[harness]
		for _, sessionID := range residual.Sessions {
			result.Sessions = appendUniqueString(result.Sessions, sessionID)
		}
		for _, project := range residual.Projects {
			result.Projects = mergeProjectSelection(result.Projects, project)
		}
		if len(result.Projects) > 0 || len(result.Sessions) > 0 {
			merged.Harnesses[harness] = result
		}
	}
	if len(merged.Harnesses) == 0 {
		merged.Harnesses = nil
	}
	return merged
}

func sameAvailableProject(current, target config.ProjectSelection) bool {
	if len(target.ClonePaths) > 0 {
		for _, clonePath := range target.ClonePaths {
			if containsString(current.ClonePaths, clonePath) {
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

func cloneHarnessMap(source map[string]config.SelectionHarnessConfig) map[string]config.SelectionHarnessConfig {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]config.SelectionHarnessConfig, len(source))
	for harness, configured := range source {
		copy := config.SelectionHarnessConfig{Sessions: append([]string(nil), configured.Sessions...)}
		for _, project := range configured.Projects {
			copy.Projects = append(copy.Projects, cloneProjectSelection(project))
		}
		result[harness] = copy
	}
	return result
}

func cloneProjectSelection(project config.ProjectSelection) config.ProjectSelection {
	project.ClonePaths = append([]string(nil), project.ClonePaths...)
	project.Branches = append([]string(nil), project.Branches...)
	return project
}

func appendUniqueString(values []string, value string) []string {
	if !containsString(values, value) {
		return append(values, value)
	}
	return values
}

func mergeProjectSelection(projects []config.ProjectSelection, incoming config.ProjectSelection) []config.ProjectSelection {
	key := mergeProjectKey(incoming)
	for index := range projects {
		if mergeProjectKey(projects[index]) != key {
			continue
		}
		existing := &projects[index]
		if existing.GitRemote == "" {
			existing.GitRemote = incoming.GitRemote
		}
		if existing.Name == "" {
			existing.Name = incoming.Name
		}
		for _, clonePath := range incoming.ClonePaths {
			existing.ClonePaths = appendUniqueString(existing.ClonePaths, clonePath)
		}
		existing.Branches = mergeBranches(existing.Branches, incoming.Branches)
		return projects
	}
	return append(projects, cloneProjectSelection(incoming))
}

func mergeProjectKey(project config.ProjectSelection) string {
	pathBound := len(project.ClonePaths) > 0
	if remote := ingest.NormalizeRemoteForMatch(project.GitRemote); remote != "" {
		return fmt.Sprintf("remote:%s|path:%t", remote, pathBound)
	}
	if name := ingest.NormalizeProjectNameForMatch(project.Name); name != "" {
		return fmt.Sprintf("name:%s|path:%t", name, pathBound)
	}
	paths := append([]string(nil), project.ClonePaths...)
	sort.Strings(paths)
	return fmt.Sprintf("clone:%v|path:%t", paths, pathBound)
}

func mergeBranches(existing, incoming []string) []string {
	if len(existing) == 0 || len(incoming) == 0 {
		return nil
	}
	result := append([]string(nil), existing...)
	for _, branch := range incoming {
		result = appendUniqueString(result, branch)
	}
	sort.Strings(result)
	return result
}

// rollup recomputes an interior node's state from its children (leaves keep the
// state a source assigned). A Conflict anywhere below forces the ancestor to
// Partial (never a clean Checked/Unchecked), matching the kit tree's own
// rollup: any Conflict, or a mix of Checked and Unchecked, is Partial.
func rollup(n *kit.TreeNode) kit.TriState {
	if len(n.Children) == 0 {
		return n.State
	}
	var checked, unchecked, other int
	for _, c := range n.Children {
		switch rollup(c) {
		case kit.Checked:
			checked++
		case kit.Unchecked:
			unchecked++
		default:
			other++
		}
	}
	switch {
	case other > 0:
		n.State = kit.Partial
	case checked > 0 && unchecked == 0:
		n.State = kit.Checked
	case checked == 0 && unchecked > 0:
		n.State = kit.Unchecked
	default:
		n.State = kit.Partial
	}
	return n.State
}
