package settings

import (
	"fmt"

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
//   - every provider root Checked  -> Mode=all, Harnesses empty (standing
//     policy: future projects are auto-included)
//   - anything else                -> Mode=selected
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
		ps.Name = remote.Label
	}
	return ps
}

// allRootsChecked reports whether every root node is cleanly Checked (the
// standing "select everything, future projects auto-included" state). An empty
// forest is not "all".
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
// forest is walked, merging branches of the same project into a single
// ProjectSelection.
type harnessBuild struct {
	order    []string
	projects map[string]*config.ProjectSelection
	sessions []string
}

func (hb *harnessBuild) project(remote, name string) *config.ProjectSelection {
	if hb.projects == nil {
		hb.projects = map[string]*config.ProjectSelection{}
	}
	key := "remote:" + remote + "|name:" + name
	ps, ok := hb.projects[key]
	if !ok {
		ps = &config.ProjectSelection{GitRemote: remote, Name: name}
		hb.projects[key] = ps
		hb.order = append(hb.order, key)
	}
	return ps
}

// fromProjectFirstForest derives the harness-keyed TreeSelection from a
// PROJECT -> BRANCH -> SESSION forest, recovering each session's harness from
// its MetaHarness leaf key. Every provider root Checked -> Mode=all matches the
// standing root-check policy; any narrower selection produces the same
// harness-keyed allowlist the legacy project-first wizard wrote by filtering the
// selected sessions by harness.
func fromProjectFirstForest(roots []*kit.TreeNode) TreeSelection {
	if allRootsChecked(roots) {
		return TreeSelection{Mode: config.SelectionModeAll}
	}
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
			get(h).project(remote, name) // branches nil == whole project
		}
	}
	addBranch := func(scope *kit.TreeNode, remote, name, branch string) {
		for _, h := range harnessesUnder(scope) {
			ps := get(h).project(remote, name)
			if !containsString(ps.Branches, branch) {
				ps.Branches = append(ps.Branches, branch)
			}
		}
	}
	for _, project := range roots {
		if project.State == kit.Unchecked || project.State == kit.Conflict {
			continue
		}
		remote := gitRemoteOf(project)
		name := ""
		if remote == "" {
			name = project.Label
		}
		switch project.State {
		case kit.Checked:
			addProjectAllBranches(project, remote, name)
		case kit.Partial:
			for _, branch := range project.Children {
				switch branch.State {
				case kit.Checked:
					addBranch(branch, remote, name, branchOf(branch))
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
			projects = append(projects, *hb.projects[key])
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

// ApplyExistingSelection pre-checks the nodes of a freshly-scanned forest to
// reflect an already-saved selection, so the user sees what they previously
// chose. It reuses config.CompileSelectionMatcher / ingest.SelectionMatcher's
// MatchDiscovery - the SAME matcher ingest, push, discovery, and prune use -
// rather than reimplementing which sessions a saved selection covers, mirroring
// the legacy kickstart wizard's applyExistingSelection semantics:
//
//   - Mode all pre-checks the whole forest.
//   - A session the matcher admits (BranchMatchYes) is Checked.
//   - A session the matcher withholds as a conflict
//     (BranchMatchWithheldConflict) is marked Conflict (display only): it is
//     ticked in intent but its persisted reality is inconsistent, so it renders
//     distinctly and blocks Commit until resolved.
//   - Interior node states are rolled up from their children afterward.
func ApplyExistingSelection(roots []*kit.TreeNode, sel config.SelectionConfig) {
	if sel.Mode == config.SelectionModeAll {
		for _, r := range roots {
			setSubtreeChecked(r)
		}
		return
	}
	if isProjectFirstForest(roots) {
		applyExistingProjectFirstSelection(roots, sel)
		return
	}
	matcher := config.CompileSelectionMatcher(sel)
	for _, provider := range roots {
		harness := ingest.Harness(provider.ID)
		for _, remote := range provider.Children {
			gitRemote := gitRemoteOf(remote)
			projectName := remote.Label
			for _, worktree := range remote.Children {
				branch := branchOf(worktree)
				for _, session := range worktree.Children {
					match := matcher.MatchDiscovery(
						harness, gitRemote, projectName, branch,
						ingest.SessionID(session.ID), sel.AutoIngestNewBranches)
					switch match {
					case ingest.BranchMatchYes:
						session.State = kit.Checked
					case ingest.BranchMatchWithheldConflict:
						session.State = kit.Conflict
					default:
						session.State = kit.Unchecked
					}
				}
			}
		}
	}
	for _, r := range roots {
		rollup(r)
	}
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

// applyExistingProjectFirstSelection applies sel to the project -> branch ->
// session forest produced by kickstart.ScannerTreeSource. Each session leaf
// carries its harness, while its project and branch identities come from its
// ancestors. This is the same MatchDiscovery boundary used for the older
// harness-first shape above, only traversed according to the detected shape.
func applyExistingProjectFirstSelection(roots []*kit.TreeNode, sel config.SelectionConfig) {
	matcher := config.CompileSelectionMatcher(sel)
	for _, project := range roots {
		remote := gitRemoteOf(project)
		projectName := project.Label
		for _, branch := range project.Children {
			branchName := branchOf(branch)
			var applySession func(*kit.TreeNode)
			applySession = func(session *kit.TreeNode) {
				if harness := harnessOf(session); harness != "" {
					match := matcher.MatchDiscovery(
						ingest.Harness(harness), remote, projectName, branchName,
						ingest.SessionID(session.ID), sel.AutoIngestNewBranches)
					switch match {
					case ingest.BranchMatchYes:
						session.State = kit.Checked
					case ingest.BranchMatchWithheldConflict:
						session.State = kit.Conflict
					default:
						session.State = kit.Unchecked
					}
				}
				for _, child := range session.Children {
					applySession(child)
				}
			}
			for _, session := range branch.Children {
				applySession(session)
			}
		}
		rollup(project)
	}
}

// setSubtreeChecked sets n and every descendant to Checked.
func setSubtreeChecked(n *kit.TreeNode) {
	n.State = kit.Checked
	for _, c := range n.Children {
		setSubtreeChecked(c)
	}
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
