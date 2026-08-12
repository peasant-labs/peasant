package settings

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

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
	// MetaProjectIdentity carries the scanner's stable opaque root identity.
	// Exact harness/worktree identity remains on session descendants. It is never
	// a display label or persisted config field.
	MetaProjectIdentity = "projectIdentity"
	// MetaProjectHarness is the compatibility key used by older single-harness
	// exact trees. Current scanner roots can span harnesses and recover each
	// harness from session leaves through MetaHarness.
	MetaProjectHarness = "projectHarness"
	// MetaProjectName keeps the discovery name separate from a rendered label.
	// A non-Git label may add short path context and must never be persisted as
	// the project's name.
	MetaProjectName = "projectName"
	// MetaClonePath carries a resolver-produced physical identity path.
	MetaClonePath = "clonePath"
	// MetaGitDirectory carries one physical Git directory for diagnostics. It is
	// not a grouping key and never enters config.SelectionConfig.
	MetaGitDirectory = "gitDirectory"
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
	// MetaTracked marks a row included by the previously saved selection. It is
	// display-only and must never be inferred from MetaIngested or current
	// checkbox state.
	MetaTracked = kit.MetaTracked
	// MetaTrackedValue is the value MetaTracked carries when set.
	MetaTrackedValue = kit.MetaTrackedValue
	// MetaChildCount carries how many child (subagent) sessions a parent session
	// groups. A parent session is a LEAF row that summarises its subagents, so
	// the count is display-only context and never affects the derived
	// SelectionConfig - the ingest side expands a selected parent to its children
	// from the discovery listing, not from the tree.
	MetaChildCount = kit.MetaChildCount
)

type selectionScopeKind uint8

const (
	selectionScopeProject selectionScopeKind = iota
	selectionScopeBranch
	selectionScopeSession
)

type selectionScope struct {
	kind            selectionScopeKind
	root            *kit.TreeNode
	projectIdentity string
	harness         string
	clonePath       ingest.ClonePath
	branch          string
	sessionID       string
	selected        bool
}

// Selection provenance markers preserve the grain of saved rules when rollup
// makes every currently available child look like one checked parent. They are
// editor state only and are never persisted.
const (
	// metaExplicitBranchSelection marks a project whose saved rule names
	// branches. It prevents a no-edit save from widening that rule to every
	// branch when all currently available named branches happen to be checked.
	metaExplicitBranchSelection = "explicitBranchSelection"
	// metaExplicitSessionSelection marks a session selected only by its explicit
	// saved ID. It prevents a no-edit save from widening one or all currently
	// available sessions into a branch or whole-project rule.
	metaExplicitSessionSelection = "explicitSessionSelection"
)

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

// isExactProjectFirstForest reports whether a project-first forest carries the
// complete physical identity contract emitted by kickstart.ScannerTreeSource.
// Presentation fixtures may use the same hierarchy without exact paths; those
// continue through the generic draft-selection path.
func isExactProjectFirstForest(roots []*kit.TreeNode) bool {
	if len(roots) == 0 || !isProjectFirstForest(roots) {
		return false
	}
	for _, root := range roots {
		if metaOf(root, MetaProjectIdentity) == "" ||
			!rootCarriesExactSessionPath(root) {
			return false
		}
	}
	return true
}

func rootCarriesExactSessionPath(root *kit.TreeNode) bool {
	found := false
	valid := true
	walkNodes(root, func(node *kit.TreeNode) {
		if harnessOf(node) == "" {
			return
		}
		found = true
		if metaOf(node, MetaClonePath) == "" {
			valid = false
		}
	})
	return found && valid
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

type projectFirstHarnessBuild struct {
	projects []config.ProjectSelection
	sessions []string
}

// fromProjectFirstForest derives the harness-keyed TreeSelection from a
// PROJECT -> BRANCH -> SESSION forest, recovering each session's harness from
// its MetaHarness leaf key. Even when every current project is checked, this
// shape persists Mode=selected with the exact current physical-clone list. A
// project discovered on a later run therefore starts clear.
func fromProjectFirstForest(roots []*kit.TreeNode) TreeSelection {
	builds := map[string]*projectFirstHarnessBuild{}
	order := []string{}
	get := func(h string) *projectFirstHarnessBuild {
		hb, ok := builds[h]
		if !ok {
			hb = &projectFirstHarnessBuild{}
			builds[h] = hb
			order = append(order, h)
		}
		return hb
	}
	for _, project := range availableProjectsFromForest(roots) {
		if len(project.sessions) == 0 {
			continue
		}
		build := get(project.harness.String())
		allChecked := true
		allCheckedExplicit := true
		checkedCount := 0
		explicitBranch := false
		branchOrder := []string{}
		branchSessions := map[string][]availableSession{}
		for _, session := range project.sessions {
			branch := session.candidate.Branch
			if _, known := branchSessions[branch]; !known {
				branchOrder = append(branchOrder, branch)
			}
			branchSessions[branch] = append(branchSessions[branch], session)
			if session.node.State != kit.Checked {
				allChecked = false
				continue
			}
			checkedCount++
			if metaOf(session.node, metaExplicitSessionSelection) != "true" {
				allCheckedExplicit = false
			}
			if metaOf(session.node, metaExplicitBranchSelection) == "true" {
				explicitBranch = true
			}
		}
		if checkedCount == 0 {
			continue
		}
		if allChecked && allCheckedExplicit {
			for _, session := range project.sessions {
				build.sessions = appendUniqueString(build.sessions, session.node.ID)
			}
			continue
		}

		var branches []string
		if allChecked && !explicitBranch {
			build.projects = appendEquivalentProjectSelection(build.projects, availableProjectSelection(project, nil))
			continue
		}
		for _, branch := range branchOrder {
			sessions := branchSessions[branch]
			branchChecked := len(sessions) > 0
			branchExplicitOnly := true
			for _, session := range sessions {
				if session.node.State != kit.Checked {
					branchChecked = false
					continue
				}
				if metaOf(session.node, metaExplicitSessionSelection) != "true" {
					branchExplicitOnly = false
				}
			}
			if branchChecked && !branchExplicitOnly {
				branches = appendUniqueString(branches, branch)
				continue
			}
			for _, session := range sessions {
				if session.node.State == kit.Checked {
					build.sessions = appendUniqueString(build.sessions, session.node.ID)
				}
			}
		}
		if len(branches) > 0 {
			sort.Strings(branches)
			build.projects = appendEquivalentProjectSelection(build.projects, availableProjectSelection(project, branches))
		}
	}
	ts := TreeSelection{Mode: config.SelectionModeSelected, Harnesses: map[string]config.SelectionHarnessConfig{}}
	for _, h := range order {
		hb := builds[h]
		if len(hb.projects) == 0 && len(hb.sessions) == 0 {
			continue
		}
		ts.Harnesses[h] = config.SelectionHarnessConfig{Projects: hb.projects, Sessions: hb.sessions}
	}
	if len(ts.Harnesses) == 0 {
		ts.Harnesses = nil
	}
	return ts
}

func availableProjectSelection(project availableProject, branches []string) config.ProjectSelection {
	selection := config.ProjectSelection{GitRemote: project.gitRemote, Branches: cloneStrings(branches)}
	if selection.GitRemote == "" {
		selection.Name = project.projectName
	}
	if project.clonePath != "" {
		selection.ClonePaths = []string{project.clonePath.String()}
	}
	return selection
}

func appendEquivalentProjectSelection(projects []config.ProjectSelection, incoming config.ProjectSelection) []config.ProjectSelection {
	key := equivalentProjectSelectionKey(incoming)
	for index := range projects {
		if equivalentProjectSelectionKey(projects[index]) != key {
			continue
		}
		if incoming.GitRemote != "" && (projects[index].GitRemote == "" || incoming.GitRemote < projects[index].GitRemote) {
			projects[index].GitRemote = incoming.GitRemote
		}
		for _, clonePath := range incoming.ClonePaths {
			projects[index].ClonePaths = appendUniqueString(projects[index].ClonePaths, clonePath)
		}
		sort.Strings(projects[index].ClonePaths)
		return projects
	}
	return append(projects, cloneProjectSelection(incoming))
}

func equivalentProjectSelectionKey(project config.ProjectSelection) string {
	policy := branchPolicyKey(project.Branches)
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
// only need tree state. Mode all is handled directly; selected mode delegates to
// the one canonical candidate-matcher path in PrepopulateSelection.
func ApplyExistingSelection(roots []*kit.TreeNode, sel config.SelectionConfig) {
	if sel.Mode == config.SelectionModeAll {
		for _, root := range roots {
			setSubtreeChecked(root)
		}
		return
	}
	if isProjectFirstForest(roots) {
		applyExistingProjectFirstSelection(roots, sel)
		return
	}
	_ = PrepopulateSelection(roots, sel)
}

// applyExistingProjectFirstSelection applies sel to the project -> branch ->
// session forest produced by kickstart.ScannerTreeSource. It delegates to the
// same candidate-aware matcher as every other selected-mode forest so clone
// paths, identity multiplicity, and exact exclusions cannot drift by shape.
func applyExistingProjectFirstSelection(roots []*kit.TreeNode, sel config.SelectionConfig) {
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
	markExplicitSessionSelections(sel, projects)
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

// markExplicitSessionSelections records which available sessions rely only on
// their explicit saved IDs. A duplicate explicit ID that is also admitted by a
// project rule does not need session provenance because removing it would not
// narrow the saved selection.
func markExplicitSessionSelections(sel config.SelectionConfig, projects []availableProject) {
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
		project := &projects[projectIndex]
		for sessionIndex := range project.sessions {
			session := &project.sessions[sessionIndex]
			if session.node.Meta != nil {
				delete(session.node.Meta, metaExplicitSessionSelection)
			}
			if _, selected := explicit[session.candidate.Harness.String()][session.node.ID]; !selected {
				continue
			}
			if projectMatcher.MatchDiscoveryCandidate(session.candidate, sel.AutoIngestNewBranches) == ingest.BranchMatchYes {
				continue
			}
			if session.node.Meta == nil {
				session.node.Meta = map[string]string{}
			}
			session.node.Meta[metaExplicitSessionSelection] = "true"
		}
	}
}

func setExplicitSessionIntent(scope *kit.TreeNode, explicit bool) {
	if scope == nil {
		return
	}
	if harnessOf(scope) != "" {
		if scope.Meta == nil {
			scope.Meta = map[string]string{}
		}
		if explicit {
			scope.Meta[metaExplicitSessionSelection] = "true"
		} else {
			delete(scope.Meta, metaExplicitSessionSelection)
		}
	}
	for _, child := range scope.Children {
		setExplicitSessionIntent(child, explicit)
	}
}

// markExplicitBranchSelections preserves the distinction between a configured
// branch list and an unrestricted whole-project rule. Without the marker, a
// project whose complete current branch set happens to be selected rolls up to
// Checked and a no-edit save widens its Branches list to nil.
func markExplicitBranchSelections(sel config.SelectionConfig, projects []availableProject) {
	for _, project := range projects {
		walkNodes(project.node, func(node *kit.TreeNode) {
			if node.Meta != nil {
				delete(node.Meta, metaExplicitBranchSelection)
			}
		})
	}
	for harness, configured := range sel.Harnesses {
		for _, configuredProject := range configured.Projects {
			if len(configuredProject.Branches) == 0 {
				continue
			}
			for _, project := range matchingAvailableProjects(harness, configuredProject, projects) {
				for _, session := range project.sessions {
					if session.node.Meta == nil {
						session.node.Meta = map[string]string{}
					}
					session.node.Meta[metaExplicitBranchSelection] = "true"
				}
			}
		}
	}
}

func availableProjectsFromForest(roots []*kit.TreeNode) []availableProject {
	var projects []availableProject
	if isProjectFirstForest(roots) {
		for _, projectNode := range roots {
			byPath := map[string]*availableProject{}
			var order []string
			for _, branchNode := range projectNode.Children {
				branch := branchOf(branchNode)
				for _, sessionNode := range branchNode.Children {
					appendAvailableSessionsByPath(&byPath, &order, projectNode, branchNode, sessionNode, branch, "")
				}
			}
			for _, key := range order {
				projects = append(projects, *byPath[key])
			}
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

func appendAvailableSessionsByPath(
	byPath *map[string]*availableProject,
	order *[]string,
	projectNode, branchNode, node *kit.TreeNode,
	branch string,
	inheritedPath ingest.ClonePath,
) {
	harness := ingest.Harness(metaOf(projectNode, MetaProjectHarness))
	if value := harnessOf(node); value != "" {
		harness = ingest.Harness(value)
	}
	clonePath := clonePathOf(node)
	if clonePath == "" {
		clonePath = inheritedPath
	}
	if clonePath == "" {
		clonePath = clonePathOf(branchNode)
	}
	if clonePath == "" {
		clonePath = clonePathOf(projectNode)
	}
	key := harness.String() + "\x00" + clonePath.String()
	project := (*byPath)[key]
	if project == nil {
		gitRemote := gitRemoteOf(node)
		if gitRemote == "" {
			gitRemote = gitRemoteOf(projectNode)
		}
		projectName := metaOf(node, MetaProjectName)
		if projectName == "" {
			projectName = projectNameOf(projectNode)
		}
		remoteMultiplicity := multiplicityOf(node, MetaRemoteMultiplicity)
		if remoteMultiplicity == ingest.DiscoveryIdentityUnproven {
			remoteMultiplicity = multiplicityOf(projectNode, MetaRemoteMultiplicity)
		}
		nameMultiplicity := multiplicityOf(node, MetaNameMultiplicity)
		if nameMultiplicity == ingest.DiscoveryIdentityUnproven {
			nameMultiplicity = multiplicityOf(projectNode, MetaNameMultiplicity)
		}
		project = &availableProject{
			node:               projectNode,
			harness:            harness,
			identity:           projectIdentityOf(projectNode),
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
	appendAvailableSession(project, node, branch)
	for _, child := range node.Children {
		appendAvailableSessionsByPath(byPath, order, projectNode, branchNode, child, branch, clonePath)
	}
}

func appendAvailableSession(project *availableProject, node *kit.TreeNode, branch string) {
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
}

func appendAvailableSessions(project *availableProject, node *kit.TreeNode, branch string) {
	appendAvailableSession(project, node, branch)
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
		residual := config.SelectionHarnessConfig{Exclusions: cloneSelectionExclusions(configured.Exclusions)}
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
		if harnessSelectionPresent(residual) {
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
		result.Exclusions = mergeSelectionExclusions(result.Exclusions, residual.Exclusions)
		if harnessSelectionPresent(result) {
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
	if source == nil {
		return nil
	}
	result := make(map[string]config.SelectionHarnessConfig, len(source))
	for harness, configured := range source {
		copy := config.SelectionHarnessConfig{
			Projects:   cloneProjectSelections(configured.Projects),
			Sessions:   cloneStrings(configured.Sessions),
			Exclusions: cloneSelectionExclusions(configured.Exclusions),
		}
		result[harness] = copy
	}
	return result
}

func cloneProjectSelection(project config.ProjectSelection) config.ProjectSelection {
	project.ClonePaths = cloneStrings(project.ClonePaths)
	project.Branches = cloneStrings(project.Branches)
	return project
}

func cloneProjectSelections(projects []config.ProjectSelection) []config.ProjectSelection {
	if projects == nil {
		return nil
	}
	result := make([]config.ProjectSelection, len(projects))
	for index, project := range projects {
		result[index] = cloneProjectSelection(project)
	}
	return result
}

func cloneStrings(values []string) []string {
	if values == nil {
		return nil
	}
	result := make([]string, len(values))
	copy(result, values)
	return result
}

func cloneSelectionExclusions(exclusions config.SelectionExclusions) config.SelectionExclusions {
	result := config.SelectionExclusions{Sessions: cloneStrings(exclusions.Sessions)}
	if exclusions.Branches != nil {
		result.Branches = make([]config.BranchExclusion, len(exclusions.Branches))
		for index, exclusion := range exclusions.Branches {
			result.Branches[index] = config.BranchExclusion{
				ClonePath: exclusion.ClonePath,
				Branches:  cloneStrings(exclusion.Branches),
			}
		}
	}
	return result
}

func mergeSelectionExclusions(current, residual config.SelectionExclusions) config.SelectionExclusions {
	if len(current.Branches) == 0 && len(current.Sessions) == 0 {
		return cloneSelectionExclusions(residual)
	}
	result := cloneSelectionExclusions(current)
	for _, sessionID := range residual.Sessions {
		result.Sessions = appendUniqueString(result.Sessions, sessionID)
	}
	for _, incoming := range residual.Branches {
		found := false
		for index := range result.Branches {
			if result.Branches[index].ClonePath != incoming.ClonePath {
				continue
			}
			for _, branch := range incoming.Branches {
				result.Branches[index].Branches = appendUniqueString(result.Branches[index].Branches, branch)
			}
			found = true
			break
		}
		if !found {
			result.Branches = append(result.Branches, config.BranchExclusion{
				ClonePath: incoming.ClonePath,
				Branches:  cloneStrings(incoming.Branches),
			})
		}
	}
	return result
}

func harnessSelectionPresent(configured config.SelectionHarnessConfig) bool {
	return len(configured.Projects) > 0 || len(configured.Sessions) > 0 ||
		len(configured.Exclusions.Branches) > 0 || len(configured.Exclusions.Sessions) > 0
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
	// A path-bound non-Git entry is identified by its exact physical path set.
	// Name fallback is reserved for legacy entries that have no path evidence.
	if pathBound {
		paths := append([]string(nil), project.ClonePaths...)
		sort.Strings(paths)
		return fmt.Sprintf("clone:%v", paths)
	}
	if name := ingest.NormalizeProjectNameForMatch(project.Name); name != "" {
		return "name:" + name
	}
	return "unidentified"
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

// reconcileTouchedSelection applies only the exact semantic scopes whose live
// tree state changed. Current is the source of truth, including choices absent
// from the scanner, so unrelated fragments never pass through a lossy forest
// derivation or remote-keyed merge.
func reconcileTouchedSelection(
	current TreeSelection,
	_ UnmatchedBaseline,
	scopes []selectionScope,
	autoIngestNewBranches bool,
) (TreeSelection, bool, error) {
	if current.Mode != config.SelectionModeSelected {
		return current, false, fmt.Errorf("exact tree reconciliation requires selected mode, got %q", current.Mode)
	}
	next := TreeSelection{Mode: current.Mode, Harnesses: cloneHarnessMap(current.Harnesses)}
	for _, scope := range scopes {
		if err := reconcileSelectionScope(&next, scope, autoIngestNewBranches); err != nil {
			return current, false, err
		}
	}
	groupTouchedProjectSelections(&next, scopes)
	if err := next.Validate(); err != nil {
		return current, false, fmt.Errorf("validate reconciled exact selection: %w", err)
	}
	if reflect.DeepEqual(next, current) {
		return current, false, nil
	}
	return next, true, nil
}

// groupTouchedProjectSelections retains the established select-all shape when
// every path in a same-remote, same-policy group was explicitly touched. A
// single-path action cannot pull an unrelated sibling into this grouping.
func groupTouchedProjectSelections(selection *TreeSelection, scopes []selectionScope) {
	touched := map[string]map[string]bool{}
	for _, scope := range scopes {
		if scope.kind != selectionScopeProject || !scope.selected {
			continue
		}
		if touched[scope.harness] == nil {
			touched[scope.harness] = map[string]bool{}
		}
		touched[scope.harness][scope.clonePath.String()] = true
	}
	for harness, paths := range touched {
		if len(paths) < 2 {
			continue
		}
		configured := selection.Harnesses[harness]
		firstByKey := map[string]int{}
		var grouped []config.ProjectSelection
		for _, project := range configured.Projects {
			allTouched := len(project.ClonePaths) > 0
			for _, clonePath := range project.ClonePaths {
				if !paths[clonePath] {
					allTouched = false
					break
				}
			}
			remote := ingest.NormalizeRemoteForMatch(project.GitRemote)
			if !allTouched || remote == "" {
				grouped = append(grouped, project)
				continue
			}
			key := remote + "\x00" + branchPolicyKey(project.Branches)
			if index, found := firstByKey[key]; found {
				for _, clonePath := range project.ClonePaths {
					grouped[index].ClonePaths = appendUniqueString(grouped[index].ClonePaths, clonePath)
				}
				continue
			}
			firstByKey[key] = len(grouped)
			grouped = append(grouped, project)
		}
		configured.Projects = grouped
		selection.Harnesses[harness] = configured
	}
}

func branchPolicyKey(branches []string) string {
	if len(branches) == 0 {
		return "all"
	}
	return strings.Join(branches, "\x00")
}

// ApplyTrackedSelection annotates the rows included by a previously saved
// selection without changing their current checkbox state. It deliberately
// reuses ApplyExistingSelection as the canonical matcher boundary, snapshots
// every TriState first, and restores those states afterward. Local-store
// presence is never consulted, so imported and tracked remain independent.
func ApplyTrackedSelection(roots []*kit.TreeNode, sel config.SelectionConfig) {
	states := map[*kit.TreeNode]kit.TriState{}
	for _, root := range roots {
		walkNodes(root, func(node *kit.TreeNode) {
			states[node] = node.State
			if node.Meta != nil {
				delete(node.Meta, MetaTracked)
			}
		})
	}

	ApplyExistingSelection(roots, sel)
	for _, root := range roots {
		walkNodes(root, func(node *kit.TreeNode) {
			if node.State == kit.Checked || node.State == kit.Conflict {
				if node.Meta == nil {
					node.Meta = map[string]string{}
				}
				node.Meta[MetaTracked] = MetaTrackedValue
			}
			node.State = states[node]
		})
	}
}

// setSubtreeChecked sets n and every descendant to Checked.
func setSubtreeChecked(n *kit.TreeNode) {
	n.State = kit.Checked
	for _, c := range n.Children {
		setSubtreeChecked(c)
	}
}

func reconcileSelectionScope(next *TreeSelection, scope selectionScope, autoIngestNewBranches bool) error {
	if scope.root == nil || scope.projectIdentity == "" || scope.harness == "" || scope.clonePath == "" {
		return fmt.Errorf("exact scope is incomplete: project=%q harness=%q clonePath=%q", scope.projectIdentity, scope.harness, scope.clonePath)
	}
	if metaOf(scope.root, MetaProjectIdentity) != scope.projectIdentity {
		return fmt.Errorf("exact scope %q no longer matches its full-forest repository node", scope.projectIdentity)
	}
	if next.Harnesses == nil {
		next.Harnesses = map[string]config.SelectionHarnessConfig{}
	}
	configured, existed := next.Harnesses[scope.harness]
	wasUnrestricted := existed && len(configured.Projects) == 0 && len(configured.Sessions) == 0
	candidates, err := candidatesForSelectionScope(scope)
	if err != nil {
		return err
	}

	switch scope.kind {
	case selectionScopeProject:
		if scope.selected {
			replacement := projectReplacement(configured.Projects, scope, nil, true)
			configured.Projects = spliceExactProjectPath(configured.Projects, scope.clonePath.String(), &replacement)
			for _, candidate := range candidates {
				configured.Sessions = removeString(configured.Sessions, string(candidate.SessionID))
				configured.Exclusions.Sessions = removeString(configured.Exclusions.Sessions, string(candidate.SessionID))
				configured.Exclusions.Branches = removeBranchExclusion(configured.Exclusions.Branches, scope.clonePath.String(), candidate.Branch)
			}
		} else {
			configured.Projects = spliceExactProjectPath(configured.Projects, scope.clonePath.String(), nil)
			for _, candidate := range candidates {
				configured.Sessions = removeString(configured.Sessions, string(candidate.SessionID))
			}
			for _, candidate := range candidates {
				if positiveSelectionAdmits(*next, scope.harness, configured, candidate, autoIngestNewBranches, wasUnrestricted) {
					configured.Exclusions.Branches = addBranchExclusion(configured.Exclusions.Branches, scope.clonePath.String(), candidate.Branch)
				}
			}
		}
	case selectionScopeBranch:
		if scope.branch == "" {
			return fmt.Errorf("branch scope for project %q has an empty branch", scope.projectIdentity)
		}
		if scope.selected {
			replacement := projectReplacement(configured.Projects, scope, []string{scope.branch}, false)
			configured.Projects = spliceExactProjectPath(configured.Projects, scope.clonePath.String(), &replacement)
			configured.Exclusions.Branches = removeBranchExclusion(configured.Exclusions.Branches, scope.clonePath.String(), scope.branch)
			for _, candidate := range candidates {
				configured.Sessions = removeString(configured.Sessions, string(candidate.SessionID))
				configured.Exclusions.Sessions = removeString(configured.Exclusions.Sessions, string(candidate.SessionID))
			}
		} else {
			configured.Projects = removeExplicitBranchFromExactProject(configured.Projects, scope.clonePath.String(), scope.branch)
			for _, candidate := range candidates {
				configured.Sessions = removeString(configured.Sessions, string(candidate.SessionID))
			}
			if len(configured.Projects) > 0 || wasUnrestricted {
				configured.Exclusions.Branches = addBranchExclusion(configured.Exclusions.Branches, scope.clonePath.String(), scope.branch)
			}
		}
	case selectionScopeSession:
		if scope.sessionID == "" || len(candidates) != 1 {
			return fmt.Errorf("session scope for project %q resolved %d candidates for session %q", scope.projectIdentity, len(candidates), scope.sessionID)
		}
		candidate := candidates[0]
		configured.Sessions = removeString(configured.Sessions, scope.sessionID)
		if scope.selected {
			configured.Exclusions.Sessions = removeString(configured.Exclusions.Sessions, scope.sessionID)
			if !positiveProjectSelectionAdmits(*next, scope.harness, configured, candidate, autoIngestNewBranches, wasUnrestricted) {
				configured.Sessions = appendUniqueString(configured.Sessions, scope.sessionID)
			}
		} else if positiveSelectionAdmits(*next, scope.harness, configured, candidate, autoIngestNewBranches, wasUnrestricted) {
			configured.Exclusions.Sessions = appendUniqueString(configured.Exclusions.Sessions, scope.sessionID)
		}
	default:
		return fmt.Errorf("exact scope for project %q has unknown kind %d", scope.projectIdentity, scope.kind)
	}

	if len(configured.Projects) == 0 && len(configured.Sessions) == 0 && !wasUnrestricted {
		if len(configured.Exclusions.Branches) > 0 || len(configured.Exclusions.Sessions) > 0 {
			return fmt.Errorf("clearing the final positive choice for harness %q would leave unrelated exclusions without a positive selection", scope.harness)
		}
		delete(next.Harnesses, scope.harness)
	} else {
		next.Harnesses[scope.harness] = configured
	}
	if len(next.Harnesses) == 0 {
		next.Harnesses = nil
	}

	matcher := config.CompileSelectionMatcher(next.ToSelectionConfig(autoIngestNewBranches))
	for _, candidate := range candidates {
		selected := matcher.MatchDiscoveryCandidate(candidate, autoIngestNewBranches) == ingest.BranchMatchYes
		if selected != scope.selected {
			if scope.selected && matcher.ExcludesCandidate(candidate) {
				return fmt.Errorf("session %q remains denied by a broader exact branch exclusion; reselect the branch before selecting this session", candidate.SessionID)
			}
			return fmt.Errorf("exact %s scope for project %q resolved selected=%v, want %v", selectionScopeName(scope.kind), scope.projectIdentity, selected, scope.selected)
		}
	}
	return nil
}

func removeExplicitBranchFromExactProject(projects []config.ProjectSelection, clonePath, branch string) []config.ProjectSelection {
	project, found := exactProjectTemplate(projects, clonePath)
	if !found || len(project.Branches) == 0 || !containsString(project.Branches, branch) {
		return projects
	}
	project.Branches = removeString(project.Branches, branch)
	if len(project.Branches) == 0 {
		return spliceExactProjectPath(projects, clonePath, nil)
	}
	project.ClonePaths = []string{clonePath}
	return spliceExactProjectPath(projects, clonePath, &project)
}

func selectionScopeName(kind selectionScopeKind) string {
	switch kind {
	case selectionScopeProject:
		return "project"
	case selectionScopeBranch:
		return "branch"
	case selectionScopeSession:
		return "session"
	default:
		return "unknown"
	}
}

func candidatesForSelectionScope(scope selectionScope) ([]ingest.DiscoveryCandidate, error) {
	projects := availableProjectsFromForest([]*kit.TreeNode{scope.root})
	var matching []availableProject
	for _, project := range projects {
		if project.harness == ingest.Harness(scope.harness) && project.clonePath == scope.clonePath {
			matching = append(matching, project)
		}
	}
	if len(matching) != 1 {
		return nil, fmt.Errorf("repository project %q resolved %d available records for harness/path %q/%q, want one", scope.projectIdentity, len(matching), scope.harness, scope.clonePath)
	}
	project := matching[0]
	var candidates []ingest.DiscoveryCandidate
	for _, session := range project.sessions {
		candidate := session.candidate
		switch scope.kind {
		case selectionScopeProject:
			candidates = append(candidates, candidate)
		case selectionScopeBranch:
			if candidate.Branch == scope.branch {
				candidates = append(candidates, candidate)
			}
		case selectionScopeSession:
			if string(candidate.SessionID) == scope.sessionID && candidate.Branch == scope.branch {
				candidates = append(candidates, candidate)
			}
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("exact %s scope for project %q has no visible session candidate", selectionScopeName(scope.kind), scope.projectIdentity)
	}
	return candidates, nil
}

func projectReplacement(projects []config.ProjectSelection, scope selectionScope, branches []string, unrestricted bool) config.ProjectSelection {
	replacement, found := exactProjectTemplate(projects, scope.clonePath.String())
	if !found {
		replacement = projectSelectionFor(scope.root, nil)
	}
	replacement.ClonePaths = []string{scope.clonePath.String()}
	if unrestricted {
		replacement.Branches = nil
		return replacement
	}
	if found && len(replacement.Branches) == 0 {
		return replacement
	}
	for _, branch := range branches {
		replacement.Branches = appendUniqueString(replacement.Branches, branch)
	}
	return replacement
}

func exactProjectTemplate(projects []config.ProjectSelection, clonePath string) (config.ProjectSelection, bool) {
	for _, project := range projects {
		if containsString(project.ClonePaths, clonePath) {
			return cloneProjectSelection(project), true
		}
	}
	return config.ProjectSelection{}, false
}

func spliceExactProjectPath(projects []config.ProjectSelection, clonePath string, replacement *config.ProjectSelection) []config.ProjectSelection {
	var result []config.ProjectSelection
	if projects != nil {
		result = make([]config.ProjectSelection, 0, len(projects)+1)
	}
	anchored := false
	for _, project := range projects {
		if !containsString(project.ClonePaths, clonePath) {
			result = append(result, cloneProjectSelection(project))
			continue
		}
		residual := cloneProjectSelection(project)
		residual.ClonePaths = removeString(residual.ClonePaths, clonePath)
		if len(residual.ClonePaths) > 0 {
			result = append(result, residual)
		}
		if !anchored && replacement != nil {
			result = append(result, cloneProjectSelection(*replacement))
		}
		anchored = true
	}
	if !anchored && replacement != nil {
		result = append(result, cloneProjectSelection(*replacement))
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func addBranchExclusion(exclusions []config.BranchExclusion, clonePath, branch string) []config.BranchExclusion {
	branch = strings.TrimSpace(branch)
	for index := range exclusions {
		if exclusions[index].ClonePath == clonePath {
			exclusions[index].Branches = appendUniqueString(exclusions[index].Branches, branch)
			return exclusions
		}
	}
	return append(exclusions, config.BranchExclusion{ClonePath: clonePath, Branches: []string{branch}})
}

func removeBranchExclusion(exclusions []config.BranchExclusion, clonePath, branch string) []config.BranchExclusion {
	for index := range exclusions {
		if exclusions[index].ClonePath != clonePath {
			continue
		}
		exclusions[index].Branches = removeString(exclusions[index].Branches, branch)
		if len(exclusions[index].Branches) == 0 {
			result := append(exclusions[:index:index], exclusions[index+1:]...)
			if len(result) == 0 {
				return nil
			}
			return result
		}
		return exclusions
	}
	return exclusions
}

func removeString(values []string, target string) []string {
	if values == nil {
		return nil
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != target {
			result = append(result, value)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func positiveSelectionAdmits(selection TreeSelection, harness string, configured config.SelectionHarnessConfig, candidate ingest.DiscoveryCandidate, autoIngestNewBranches, allowUnrestricted bool) bool {
	if len(configured.Projects) == 0 && len(configured.Sessions) == 0 && !allowUnrestricted {
		return false
	}
	positive := TreeSelection{Mode: selection.Mode, Harnesses: cloneHarnessMap(selection.Harnesses)}
	configured.Exclusions = config.SelectionExclusions{}
	if positive.Harnesses == nil {
		positive.Harnesses = map[string]config.SelectionHarnessConfig{}
	}
	positive.Harnesses[harness] = configured
	matcher := config.CompileSelectionMatcher(positive.ToSelectionConfig(autoIngestNewBranches))
	return matcher.MatchDiscoveryCandidate(candidate, autoIngestNewBranches) == ingest.BranchMatchYes
}

func positiveProjectSelectionAdmits(selection TreeSelection, harness string, configured config.SelectionHarnessConfig, candidate ingest.DiscoveryCandidate, autoIngestNewBranches, allowUnrestricted bool) bool {
	configured.Sessions = nil
	return positiveSelectionAdmits(selection, harness, configured, candidate, autoIngestNewBranches, allowUnrestricted)
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
