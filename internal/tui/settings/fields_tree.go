package settings

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// treeField is the tri-state selection tree field. It wraps the kit.Tree (the
// one asynchronous field-data component), derives a [TreeSelection] from the
// live node forest after every edit, and writes it back through its accessor.
// A Conflict node blocks Validate (and therefore Commit) fail-closed; it is
// never persisted.
//
// A [WithFacet] option adds a side gutter that groups the forest by one Meta
// key and narrows the rendered rows to the chosen value. The gutter is a VIEW:
// the field keeps the whole loaded forest and always derives the selection from
// it, so filtering the rows never drops a selection made under another value.
type treeField struct {
	baseField
	th      theme.Theme
	acc     Accessor[TreeSelection]
	src     kit.TreeSource
	tree    kit.Tree
	focused bool
	width   int
	height  int
	mounted bool

	// facetKey is the Meta key the side gutter groups by (empty: no facet).
	facetKey string
	// facetLabel titles the gutter.
	facetLabel string
	// facetDisplay renders one facet value for the gutter; nil shows the raw
	// value.
	facetDisplay func(value string) string
	// facetState indexes the gutter's cycle: 0 shows every value, 1..n narrows
	// to that value, and the last state hides the gutter (and its filter).
	facetState int

	// previewSource is the seam the body shown beside the tree is loaded
	// through: a structured kit.BodySource for a body with shape a string
	// would flatten (a session transcript's role-tagged turns). split is the
	// two-pane surface hosting the tree and that body.
	previewSource kit.BodySource
	// previewBody builds the layout function the pane renders that body with. It
	// is a factory over the theme rather than a ready-made renderer because a
	// field only learns its theme at mount.
	previewBody func(theme.Theme) kit.BodyRenderer
	split       kit.PreviewSplit
	// restoreSelection enables the kickstart selection editor's one-time saved
	// baseline application. Other generic Tree users keep the source-provided
	// states they already own.
	restoreSelection bool
	// selectAllHelp narrows the generic select-all action's user-facing scope.
	// The key and action ID remain canonical.
	selectAllHelp string

	// full is the whole forest the source last loaded; view is the (possibly
	// narrowed) forest the tree currently renders, sharing full's leaf pointers.
	full []*kit.TreeNode
	view []*kit.TreeNode

	// baselineApplied records the first successful selected-mode source load when
	// restoreSelection is enabled. That load alone restores Draft.Baseline; a
	// later load restores Draft.Working instead, so a refresh cannot overwrite
	// edits already made on screen. Legacy mode all waits for its conversion
	// boundary and does not set this flag.
	baselineApplied bool
	unmatched       UnmatchedBaseline
	reconcileErr    error
}

// TreeOption configures a tree field at construction.
type TreeOption func(*treeField)

// WithFacet groups the tree by the values of one Meta key and renders them as a
// side gutter with per-value counts, narrowing the visible rows to the chosen
// value. It is generic: label titles the gutter and metaKey names the Meta a
// [kit.TreeSource] attached (e.g. [MetaHarness]). The gutter is shown by
// default; the filter key cycles it through "every value", each value in turn,
// and hidden.
func WithFacet(metaKey, label string) TreeOption {
	return func(f *treeField) {
		f.facetKey = metaKey
		f.facetLabel = label
	}
}

// WithFacetDisplay renders each facet value for the gutter, so a raw Meta value
// (a harness slug) can read as its display name. A nil function, or no option
// at all, shows the raw value.
func WithFacetDisplay(display func(value string) string) TreeOption {
	return func(f *treeField) { f.facetDisplay = display }
}

// WithPreviewBodySource shows the body of the highlighted row beside the
// tree, loaded on demand from src, which is asked for a kit.PreviewBody that
// lays ITSELF out at the pane's current width on every draw - the kickstart
// selection tree previews a whole session transcript through it, since a
// sequence of role-tagged turns is not a flat string. The load is
// asynchronous (navigation is never blocked), a stale result for a since-de-
// highlighted row is dropped, and a failed read renders an actionable
// in-pane message rather than a panic - all owned by the kit preview split
// this option mounts.
func WithPreviewBodySource(src kit.BodySource) TreeOption {
	return func(f *treeField) { f.previewSource = src }
}

// WithPreviewBody lays the preview body out with a renderer built from the
// mounted theme, instead of the pane's default word wrap. build is called once
// at mount, so a caller whose renderer needs the palette (rendering markdown in
// the app's colors) gets it without the field having to carry a theme before it
// has one.
func WithPreviewBody(build func(theme.Theme) kit.BodyRenderer) TreeOption {
	return func(f *treeField) { f.previewBody = build }
}

// WithSelectionRestoration applies the saved selected-mode configuration after
// the first successful source load and before deriving the working value. It is
// opt-in because generic Tree sources may own meaningful initial node states;
// kickstart uses it for its selection editor.
func WithSelectionRestoration() TreeOption {
	return func(f *treeField) { f.restoreSelection = true }
}

// WithSelectAllHelp gives this tree a scope-specific description for the
// canonical select-all action. It changes no key binding and leaves unrelated
// trees on the generic "select all" wording.
func WithSelectAllHelp(description string) TreeOption {
	return func(f *treeField) { f.selectAllHelp = description }
}

// Tree builds the selection-tree field bound to acc, loading its forest from
// src (a kit.TreeSource - in tests, scannerfix.FixtureTreeSource).
func Tree(key, label string, acc Accessor[TreeSelection], src kit.TreeSource, opts ...TreeOption) Field {
	f := &treeField{baseField: baseField{key: key, label: label}, acc: acc, src: src}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

func (f *treeField) Kind() FieldKind { return KindTree }

func (f *treeField) mount(t theme.Theme) {
	f.th = t
	f.tree = kit.NewTree(t, f.src)
	// The split drives the SAME tree by pointer, so the field, the facet, and
	// the preview never work on diverging copies of the forest.
	if f.previewSource != nil {
		f.split = kit.NewPreviewSplitWithBodies(t, kit.NewTreeLeftPane(&f.tree), f.previewSource)
	}
	f.mounted = true
}

// hasPreview reports whether a preview pane is mounted beside the tree.
func (f *treeField) hasPreview() bool { return f.previewSource != nil }

// initCmd starts the tree's scan and spinner. It is batched into Flow.Init.
func (f *treeField) initCmd() tea.Cmd {
	if !f.mounted {
		return nil
	}
	var loadCmd tea.Cmd
	f.tree, loadCmd = f.tree.Load()
	return tea.Batch(loadCmd, f.tree.Init())
}

func (f *treeField) focus() tea.Cmd { f.focused = true; return f.tree.Focus() }
func (f *treeField) blur()          { f.focused = false; f.tree.Blur() }

func (f *treeField) setSize(w, h int) {
	f.width, f.height = w, h
	f.applySize()
}

// applySize hands the tree the region left after the facet gutter. It is called
// on every render because the gutter's width depends on the loaded forest,
// which arrives after the first size.
func (f *treeField) applySize() {
	inner := f.width - f.gutterWidth()
	if f.hasPreview() {
		// The split owns the divide between the tree and the preview body, and
		// sizes the tree itself.
		f.split.SetSize(inner, f.height)
		return
	}
	f.tree.SetSize(inner, f.height)
}

// availableActions reports what the field dispatches right now, plus the filter
// key when a facet is configured, so the footer and help overlay advertise
// exactly that.
//
// With a preview mounted the answer comes from the split, and it CHANGES with
// pane focus: the tree's navigate/expand/select set while the tree is active,
// the viewport's scroll set while the preview is. The split's own tab/shift+tab
// toggle is deliberately excluded - the flow owns those keys for stepping
// between fields, and a split is one field.
func (f *treeField) availableActions() []keymap.ActionID {
	var actions []keymap.ActionID
	if f.hasPreview() {
		actions = f.split.PaneActions()
	} else {
		actions = f.tree.AvailableActions()
	}
	if f.facetKey != "" {
		actions = append(actions, keymap.ActionFilter)
	}
	return actions
}

func (f *treeField) actionKeymap() keymap.Keymap {
	km := keymap.Default()
	if f.selectAllHelp == "" {
		return km
	}
	binding, ok := km[keymap.ActionSelectAll]
	if !ok {
		return km
	}
	help := binding.Help()
	binding.SetHelp(help.Key, f.selectAllHelp)
	km[keymap.ActionSelectAll] = binding
	return km
}

func (f *treeField) sync(d *Draft) {}

// handle forwards a message to the live tree, then re-derives the selection
// from the current forest and writes it back into the draft. The filter key is
// answered first: it re-points the tree at a narrowed VIEW of the loaded forest
// rather than reaching the tree as navigation.
func (f *treeField) handle(d *Draft, msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	wasLoading := f.tree.Loading()
	intent := f.captureSelectionIntent(msg)
	var before selectableState
	exactAction := f.restoreSelection && f.baselineApplied && intent.action != keymap.ActionUnknown && isProjectFirstForest(f.tree.Roots())
	if exactAction {
		var err error
		before, err = f.snapshotSelectableState(intent)
		if err != nil {
			f.reconcileErr = f.actionableReconcileError(intent, err)
			return nil
		}
	}
	if !f.handleFacetKey(msg) {
		if f.hasPreview() {
			// The split routes the message to the tree it holds by pointer, then
			// loads the body of whatever row the cursor ends on.
			f.split, cmd = f.split.Update(msg)
		} else {
			f.tree, cmd = f.tree.Update(msg)
		}
		loadSucceeded := wasLoading && !f.tree.Loading() && f.tree.Err() == nil
		f.captureForest(loadSucceeded)
	}

	// A spinner tick, an in-flight load, or a failed load must not replace the
	// draft with an empty derived selection. Restoration waits for the first
	// successful result.
	if f.tree.Loading() || f.tree.Err() != nil {
		return cmd
	}

	loadSucceeded := wasLoading && !f.tree.Loading()
	if f.restoreSelection && loadSucceeded {
		selection := d.Working().Selection
		firstSuccessfulLoad := !f.baselineApplied
		if firstSuccessfulLoad {
			selection = d.Baseline().Selection
		}
		if selection.Mode != config.SelectionModeSelected {
			// Legacy mode all is converted by the kickstart conversion boundary.
			// Until then, keep it intact and never compile it as a selected-mode
			// matcher merely because a tree finished loading.
			return cmd
		} else {
			f.unmatched = PrepopulateSelection(f.selectionRoots(), selection)
			if firstSuccessfulLoad {
				f.baselineApplied = true
			}
			f.reconcileErr = nil
		}
		return cmd
	}
	if f.restoreSelection && !f.baselineApplied {
		return cmd
	}
	if f.restoreSelection && isProjectFirstForest(f.selectionRoots()) && intent.action == keymap.ActionUnknown {
		// Draft.Working is authoritative for the kickstart selection editor.
		// Navigation, preview, facet, spinner, and expansion messages change only
		// the view and must never round-trip the complete forest into the draft.
		return cmd
	}
	if exactAction {
		after, err := f.snapshotSelectableState(intent)
		if err != nil {
			f.restoreSelectableState(before)
			f.reconcileErr = f.actionableReconcileError(intent, err)
			return cmd
		}
		scopes, err := changedSelectionScopes(intent, before, after, f.selectionRoots())
		if err != nil {
			f.restoreSelectableState(before)
			f.reconcileErr = f.actionableReconcileError(intent, err)
			return cmd
		}
		if len(scopes) == 0 {
			return cmd
		}
		current := f.acc.Get(d.Working())
		next, changed, err := reconcileTouchedSelection(current, f.unmatched, scopes, d.Working().Selection.AutoIngestNewBranches)
		if err != nil {
			f.restoreSelectableState(before)
			f.reconcileErr = f.actionableReconcileError(intent, err)
			return cmd
		}
		if changed {
			f.acc.Set(d.Working(), next)
		}
		// Reapply the canonical matcher after the reducer succeeds. This updates
		// the private branch/session provenance markers from the value that would
		// actually be saved, never from a merely recognized key action.
		f.unmatched = PrepopulateSelection(f.selectionRoots(), next.ToSelectionConfig(d.Working().Selection.AutoIngestNewBranches))
		f.reconcileErr = nil
		return cmd
	}
	f.applySelectionIntent(intent)
	// The selection is re-derived after a facet change too, so what a commit
	// would persist is always read from the whole forest - never left behind at
	// whatever the last narrowed view happened to hold.
	derived := FromTreeNodes(f.selectionRoots())
	f.acc.Set(d.Working(), MergeSelection(derived, f.unmatched))
	return cmd
}

type selectableNodeKey struct {
	projectIdentity string
	harness         string
	clonePath       string
	branch          string
	sessionID       string
}

type selectableNodeState struct {
	node                   *kit.TreeNode
	state                  kit.TriState
	explicitBranchValue    string
	explicitBranchPresent  bool
	explicitSessionValue   string
	explicitSessionPresent bool
}

type selectableState map[selectableNodeKey]selectableNodeState

// snapshotSelectableState captures only the semantic subtree the recognized
// action can mutate. Display labels, cursor state, expansion, and facet state
// are deliberately outside this transient rollback snapshot.
func (f *treeField) snapshotSelectableState(intent treeSelectionIntent) (selectableState, error) {
	state := selectableState{}
	roots := f.tree.Roots()
	switch intent.action {
	case keymap.ActionToggle:
		if intent.scope == nil {
			return nil, fmt.Errorf("toggle has no current tree node")
		}
		root := rootContaining(roots, intent.scope)
		if root == nil {
			return nil, fmt.Errorf("toggle node %q is not contained by a visible project root", intent.scope.ID)
		}
		branch := branchContaining(root, intent.scope)
		if intent.scope == root {
			return snapshotProjectState(state, root)
		}
		if branch == nil {
			return nil, fmt.Errorf("toggle node %q has no branch ancestor", intent.scope.ID)
		}
		if intent.scope == branch {
			return snapshotBranchState(state, root, branch)
		}
		return snapshotSessionState(state, root, branch, intent.scope)
	case keymap.ActionSelectUnderProject:
		if intent.scope == nil {
			return nil, fmt.Errorf("select-under-project has no visible project root")
		}
		return snapshotProjectState(state, intent.scope)
	case keymap.ActionSelectAll:
		if len(roots) == 0 {
			return nil, fmt.Errorf("select-all has no visible project roots")
		}
		for _, root := range roots {
			if _, err := snapshotProjectState(state, root); err != nil {
				return nil, err
			}
		}
		return state, nil
	default:
		return nil, fmt.Errorf("action %q is not a selectable tree action", intent.action)
	}
}

func snapshotProjectState(state selectableState, root *kit.TreeNode) (selectableState, error) {
	identity, harness, clonePath, err := exactProjectNodeIdentity(root)
	if err != nil {
		return nil, err
	}
	if err := addSelectableState(state, selectableNodeKey{projectIdentity: identity, harness: harness, clonePath: clonePath}, root); err != nil {
		return nil, err
	}
	for _, branch := range root.Children {
		if _, err := snapshotBranchState(state, root, branch); err != nil {
			return nil, err
		}
	}
	return state, nil
}

func snapshotBranchState(state selectableState, root, branch *kit.TreeNode) (selectableState, error) {
	identity, harness, clonePath, err := exactProjectNodeIdentity(root)
	if err != nil {
		return nil, err
	}
	branchName, err := exactBranchName(branch)
	if err != nil {
		return nil, err
	}
	if err := addSelectableState(state, selectableNodeKey{projectIdentity: identity, harness: harness, clonePath: clonePath}, root); err != nil {
		return nil, err
	}
	if err := addSelectableState(state, selectableNodeKey{projectIdentity: identity, harness: harness, clonePath: clonePath, branch: branchName}, branch); err != nil {
		return nil, err
	}
	for _, session := range branch.Children {
		if err := snapshotSessionTree(state, root, branchName, session); err != nil {
			return nil, err
		}
	}
	return state, nil
}

func snapshotSessionState(state selectableState, root, branch, session *kit.TreeNode) (selectableState, error) {
	identity, harness, clonePath, err := exactProjectNodeIdentity(root)
	if err != nil {
		return nil, err
	}
	branchName, err := exactBranchName(branch)
	if err != nil {
		return nil, err
	}
	if err := addSelectableState(state, selectableNodeKey{projectIdentity: identity, harness: harness, clonePath: clonePath}, root); err != nil {
		return nil, err
	}
	if err := addSelectableState(state, selectableNodeKey{projectIdentity: identity, harness: harness, clonePath: clonePath, branch: branchName}, branch); err != nil {
		return nil, err
	}
	if err := snapshotOneSession(state, identity, harness, clonePath, branchName, session); err != nil {
		return nil, err
	}
	return state, nil
}

func snapshotSessionTree(state selectableState, root *kit.TreeNode, branch string, session *kit.TreeNode) error {
	identity, harness, clonePath, err := exactProjectNodeIdentity(root)
	if err != nil {
		return err
	}
	if err := snapshotOneSession(state, identity, harness, clonePath, branch, session); err != nil {
		return err
	}
	for _, child := range session.Children {
		if err := snapshotSessionTree(state, root, branch, child); err != nil {
			return err
		}
	}
	return nil
}

func snapshotOneSession(state selectableState, identity, harness, clonePath, branch string, session *kit.TreeNode) error {
	if sessionHarness := harnessOf(session); sessionHarness == "" || sessionHarness != harness {
		return fmt.Errorf("session %q carries harness %q, want project harness %q", session.ID, sessionHarness, harness)
	}
	if sessionIdentity := metaOf(session, MetaProjectIdentity); sessionIdentity != "" && sessionIdentity != identity {
		return fmt.Errorf("session %q carries project identity %q, want %q", session.ID, sessionIdentity, identity)
	}
	if sessionPath := metaOf(session, MetaClonePath); sessionPath != "" && sessionPath != clonePath {
		return fmt.Errorf("session %q carries clone path %q, want %q", session.ID, sessionPath, clonePath)
	}
	if session.ID == "" || strings.TrimSpace(session.ID) != session.ID {
		return fmt.Errorf("session node %q has no normalized session identity", session.ID)
	}
	return addSelectableState(state, selectableNodeKey{
		projectIdentity: identity,
		harness:         harness,
		clonePath:       clonePath,
		branch:          branch,
		sessionID:       session.ID,
	}, session)
}

func addSelectableState(state selectableState, key selectableNodeKey, node *kit.TreeNode) error {
	if prior, duplicate := state[key]; duplicate && prior.node != node {
		return fmt.Errorf("duplicate selectable identity %+v is carried by nodes %q and %q", key, prior.node.ID, node.ID)
	}
	branchValue, branchPresent := markerValue(node, metaExplicitBranchSelection)
	sessionValue, sessionPresent := markerValue(node, metaExplicitSessionSelection)
	state[key] = selectableNodeState{
		node:                   node,
		state:                  node.State,
		explicitBranchValue:    branchValue,
		explicitBranchPresent:  branchPresent,
		explicitSessionValue:   sessionValue,
		explicitSessionPresent: sessionPresent,
	}
	return nil
}

func markerValue(node *kit.TreeNode, key string) (string, bool) {
	if node.Meta == nil {
		return "", false
	}
	value, present := node.Meta[key]
	return value, present
}

func exactProjectNodeIdentity(root *kit.TreeNode) (string, string, string, error) {
	identity := metaOf(root, MetaProjectIdentity)
	harness := metaOf(root, MetaProjectHarness)
	clonePath := metaOf(root, MetaClonePath)
	switch {
	case identity == "":
		return "", "", "", fmt.Errorf("project node %q has no stable project identity", root.ID)
	case root.ID != identity:
		return "", "", "", fmt.Errorf("project node ID %q does not match identity %q", root.ID, identity)
	case harness == "" || !ingest.Harness(harness).IsKnown():
		return "", "", "", fmt.Errorf("project %q carries unknown harness %q", identity, harness)
	case clonePath == "":
		return "", "", "", fmt.Errorf("project %q has no exact physical clone path", identity)
	case strings.TrimSpace(clonePath) != clonePath || !filepath.IsAbs(clonePath) || filepath.Clean(clonePath) != clonePath:
		return "", "", "", fmt.Errorf("project %q carries non-exact clone path %q", identity, clonePath)
	}
	return identity, harness, clonePath, nil
}

func exactBranchName(branch *kit.TreeNode) (string, error) {
	name := metaOf(branch, MetaBranch)
	if name == "" {
		return "", fmt.Errorf("branch node %q has no exact branch identity", branch.ID)
	}
	if name == "(unknown branch)" {
		return "", fmt.Errorf("branch node %q is the unknown-branch display placeholder and cannot form an exact denial", branch.ID)
	}
	if strings.TrimSpace(name) != name {
		return "", fmt.Errorf("branch node %q carries non-normalized branch %q", branch.ID, name)
	}
	return name, nil
}

func branchContaining(root, target *kit.TreeNode) *kit.TreeNode {
	for _, branch := range root.Children {
		if nodeContains(branch, target) {
			return branch
		}
	}
	return nil
}

func changedSelectionScopes(intent treeSelectionIntent, before, after selectableState, fullRoots []*kit.TreeNode) ([]selectionScope, error) {
	if len(before) != len(after) {
		return nil, fmt.Errorf("selectable identity set changed size from %d to %d during %s", len(before), len(after), intent.action)
	}
	changed := map[selectableNodeKey]bool{}
	for key, old := range before {
		current, ok := after[key]
		if !ok {
			return nil, fmt.Errorf("selectable identity %+v disappeared during %s", key, intent.action)
		}
		if old.state != kit.Conflict && current.state != kit.Conflict && old.state != current.state {
			changed[key] = true
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}

	rootByIdentity := map[string]*kit.TreeNode{}
	for _, root := range fullRoots {
		identity := metaOf(root, MetaProjectIdentity)
		if identity == "" {
			continue
		}
		if _, duplicate := rootByIdentity[identity]; duplicate {
			return nil, fmt.Errorf("full forest repeats project identity %q", identity)
		}
		rootByIdentity[identity] = root
	}

	makeScope := func(key selectableNodeKey, kind selectionScopeKind, selected bool) (selectionScope, error) {
		root := rootByIdentity[key.projectIdentity]
		if root == nil {
			return selectionScope{}, fmt.Errorf("changed project identity %q is absent from the complete forest", key.projectIdentity)
		}
		return selectionScope{
			kind:            kind,
			root:            root,
			projectIdentity: key.projectIdentity,
			harness:         key.harness,
			clonePath:       ingest.ClonePath(key.clonePath),
			branch:          key.branch,
			sessionID:       key.sessionID,
			selected:        selected,
		}, nil
	}

	switch intent.action {
	case keymap.ActionToggle:
		var targetKey selectableNodeKey
		found := false
		for key, old := range before {
			if old.node == intent.scope {
				targetKey, found = key, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("toggle target %q has no selectable semantic identity", intent.scope.ID)
		}
		kind := selectionScopeProject
		if targetKey.sessionID != "" {
			kind = selectionScopeSession
			if _, err := ingest.NewSessionID(targetKey.sessionID); err != nil {
				return nil, fmt.Errorf("session node %q cannot form an exact session exclusion: %w", targetKey.sessionID, err)
			}
		} else if targetKey.branch != "" {
			kind = selectionScopeBranch
		}
		scope, err := makeScope(targetKey, kind, intent.checked)
		if err != nil {
			return nil, err
		}
		return []selectionScope{scope}, nil
	case keymap.ActionSelectUnderProject:
		for key := range before {
			if key.branch == "" && key.sessionID == "" {
				scope, err := makeScope(key, selectionScopeProject, true)
				if err != nil {
					return nil, err
				}
				return []selectionScope{scope}, nil
			}
		}
		return nil, fmt.Errorf("select-under-project has no exact project scope")
	case keymap.ActionSelectAll:
		changedProjects := map[string]bool{}
		for key := range changed {
			changedProjects[key.projectIdentity] = true
		}
		var scopes []selectionScope
		for _, root := range fullRoots {
			identity := metaOf(root, MetaProjectIdentity)
			if !changedProjects[identity] {
				continue
			}
			var projectKey selectableNodeKey
			for key := range before {
				if key.projectIdentity == identity && key.branch == "" && key.sessionID == "" {
					projectKey = key
					break
				}
			}
			selected, err := changedProjectTarget(identity, changed, after)
			if err != nil {
				return nil, err
			}
			scope, err := makeScope(projectKey, selectionScopeProject, selected)
			if err != nil {
				return nil, err
			}
			scopes = append(scopes, scope)
		}
		return scopes, nil
	default:
		return nil, fmt.Errorf("action %q cannot produce selection scopes", intent.action)
	}
}

func changedProjectTarget(identity string, changed map[selectableNodeKey]bool, after selectableState) (bool, error) {
	found := false
	selected := false
	for key := range changed {
		if key.projectIdentity != identity || key.sessionID == "" {
			continue
		}
		state := after[key].state
		if state != kit.Checked && state != kit.Unchecked {
			return false, fmt.Errorf("changed session %q ended in non-binary state %s", key.sessionID, state)
		}
		value := state == kit.Checked
		if found && value != selected {
			return false, fmt.Errorf("select-all produced mixed targets under project %q", identity)
		}
		found, selected = true, value
	}
	if !found {
		return false, fmt.Errorf("select-all changed project %q without changing a selectable session", identity)
	}
	return selected, nil
}

func (f *treeField) restoreSelectableState(before selectableState) {
	for _, old := range before {
		old.node.State = old.state
		restoreMarker(old.node, metaExplicitBranchSelection, old.explicitBranchValue, old.explicitBranchPresent)
		restoreMarker(old.node, metaExplicitSessionSelection, old.explicitSessionValue, old.explicitSessionPresent)
	}
	for _, root := range f.full {
		rollup(root)
	}
}

func restoreMarker(node *kit.TreeNode, key, value string, present bool) {
	if present {
		if node.Meta == nil {
			node.Meta = map[string]string{}
		}
		node.Meta[key] = value
		return
	}
	if node.Meta != nil {
		delete(node.Meta, key)
	}
}

func (f *treeField) actionableReconcileError(intent treeSelectionIntent, cause error) error {
	return fmt.Errorf(
		"transcript selection change was rolled back.\n"+
			"what: Peasant could not apply the %q action to selection %q.\n"+
			"why: the exact project, branch, or session scope could not be reconciled safely: %v.\n"+
			"where: settings.treeField.handle and reconcileTouchedSelection (field %q).\n"+
			"when: while applying the tree action before updating the settings draft.\n"+
			"meaning: the tree state and private markers were restored, and the saved draft was not changed.\n"+
			"fix: refresh the selection and retry; if the same scope fails again, rerun kickstart after updating Peasant.",
		intent.action, f.label, cause, f.key,
	)
}

type treeSelectionIntent struct {
	action  keymap.ActionID
	scope   *kit.TreeNode
	checked bool
}

// captureSelectionIntent records the user-selected grain before the kit tree
// mutates node state. A session action remains an explicit session rule. A
// branch, project, or select-all action deliberately promotes that scope to a
// project rule and clears restored session-only provenance beneath it.
func (f *treeField) captureSelectionIntent(msg tea.Msg) treeSelectionIntent {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok || f.tree.Loading() {
		return treeSelectionIntent{}
	}
	action, matched := keymap.Match(keymap.Default(), keyMsg, availList(f.availableActions()))
	if !matched {
		return treeSelectionIntent{}
	}
	intent := treeSelectionIntent{action: action}
	switch action {
	case keymap.ActionToggle:
		if node, found := f.tree.CurrentNode(); found {
			intent.scope = node
			intent.checked = node.State != kit.Checked
		}
	case keymap.ActionSelectAll:
		// applySelectionIntent uses the same current tree view the kit action
		// mutates, so a narrowed facet does not change hidden session provenance.
	case keymap.ActionSelectUnderProject:
		if node, found := f.tree.CurrentNode(); found {
			intent.scope = rootContaining(f.tree.Roots(), node)
		}
	default:
		return treeSelectionIntent{}
	}
	return intent
}

func (f *treeField) applySelectionIntent(intent treeSelectionIntent) {
	switch intent.action {
	case keymap.ActionToggle:
		if intent.scope == nil {
			return
		}
		if harnessOf(intent.scope) != "" {
			setExplicitSessionIntent(intent.scope, intent.checked)
			return
		}
		setExplicitSessionIntent(intent.scope, false)
	case keymap.ActionSelectAll:
		for _, root := range f.tree.Roots() {
			setExplicitSessionIntent(root, false)
		}
	case keymap.ActionSelectUnderProject:
		setExplicitSessionIntent(intent.scope, false)
	}
}

func rootContaining(roots []*kit.TreeNode, target *kit.TreeNode) *kit.TreeNode {
	for _, root := range roots {
		if nodeContains(root, target) {
			return root
		}
	}
	return nil
}

func nodeContains(root, target *kit.TreeNode) bool {
	if root == target {
		return true
	}
	for _, child := range root.Children {
		if nodeContains(child, target) {
			return true
		}
	}
	return false
}

// handleFacetKey answers the filter key when a facet is configured, reporting
// whether it consumed the message.
func (f *treeField) handleFacetKey(msg tea.Msg) bool {
	if f.facetKey == "" {
		return false
	}
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return false
	}
	action, matched := keymap.Match(keymap.Default(), keyMsg, availList{keymap.ActionFilter})
	if !matched || action != keymap.ActionFilter {
		return false
	}
	f.facetState = (f.facetState + 1) % (len(f.facetValues()) + 2)
	f.applyFacet()
	return true
}

// captureForest records a freshly-loaded forest as the full one and re-applies
// the current facet to it. A load result is recognised by the tree's roots no
// longer being the view the field last installed.
func (f *treeField) captureForest(force bool) {
	roots := f.tree.Roots()
	if force {
		f.full = roots
		f.view = roots
		f.applyFacet()
		return
	}
	if len(roots) == 0 || sameNodes(roots, f.view) {
		return
	}
	f.full = roots
	f.view = roots
	f.applyFacet()
}

// selectionRoots returns the forest a selection is derived from: always the
// WHOLE loaded forest, never the narrowed view, so a filtered-out session keeps
// whatever the user chose for it. Interior states are rolled up first because a
// toggle made through a narrowed view only rolled up that view's own interior
// copies.
func (f *treeField) selectionRoots() []*kit.TreeNode {
	if len(f.full) == 0 {
		return f.tree.Roots()
	}
	for _, r := range f.full {
		rollup(r)
	}
	return f.full
}

// applyFacet re-points the tree at the view the current facet state selects,
// leaving the cursor alone when the view did not change.
func (f *treeField) applyFacet() {
	next := f.full
	if value, ok := f.activeFacetValue(); ok {
		next = pruneForest(f.full, f.facetKey, value)
	}
	if sameNodes(next, f.tree.Roots()) {
		f.view = next
		return
	}
	f.view = next
	f.tree = f.tree.WithRoots(next)
	f.applySize()
}

// activeFacetValue reports the value the facet currently narrows to, or false
// when it shows every value (or is hidden, or not configured).
func (f *treeField) activeFacetValue() (string, bool) {
	values := f.facetValues()
	if f.facetKey == "" || f.facetState <= 0 || f.facetState > len(values) {
		return "", false
	}
	return values[f.facetState-1], true
}

// facetHidden reports whether the gutter is in its hidden state - the last step
// of the cycle, which also clears the filter.
func (f *treeField) facetHidden() bool {
	return f.facetKey != "" && f.facetState == len(f.facetValues())+1
}

// facetValues returns the distinct values of the facet key across the whole
// loaded forest, sorted so the gutter is stable across runs.
func (f *treeField) facetValues() []string {
	if f.facetKey == "" {
		return nil
	}
	seen := map[string]bool{}
	var values []string
	for _, r := range f.full {
		walkNodes(r, func(n *kit.TreeNode) {
			v := metaOf(n, f.facetKey)
			if v == "" || seen[v] {
				return
			}
			seen[v] = true
			values = append(values, v)
		})
	}
	sort.Strings(values)
	return values
}

// facetCount reports how many nodes of the whole forest carry the given facet
// value; an empty value counts every node that carries the key at all.
func (f *treeField) facetCount(value string) int {
	n := 0
	for _, r := range f.full {
		walkNodes(r, func(node *kit.TreeNode) {
			v := metaOf(node, f.facetKey)
			if v == "" {
				return
			}
			if value == "" || v == value {
				n++
			}
		})
	}
	return n
}

func (f *treeField) render(d *Draft, _ theme.Styles, width int) string {
	if width != f.width {
		f.width = width
	}
	f.applySize()
	body := f.treeView()
	gutter := f.gutterLines()
	if len(gutter) == 0 {
		return body
	}
	return joinColumns(gutter, body)
}

// treeView renders the tree, beside its preview body when one is mounted.
func (f *treeField) treeView() string {
	if f.hasPreview() {
		return f.split.View()
	}
	return f.tree.View()
}

// facetGutterMinCells is the narrowest gutter worth drawing, and the padding it
// keeps between a value and its count.
const (
	facetGutterMinCells = 12
	facetGutterPadding  = 3
)

// gutterWidth reports the cells the facet gutter occupies, or 0 when there is
// no facet, it is hidden, nothing has loaded yet, or the region is too narrow
// to leave the tree a usable width.
func (f *treeField) gutterWidth() int {
	if f.facetKey == "" || f.facetHidden() {
		return 0
	}
	values := f.facetValues()
	if len(values) == 0 {
		return 0
	}
	want := lipgloss.Width(f.facetLabel)
	for _, row := range f.gutterRowTexts(values) {
		if w := lipgloss.Width(row); w > want {
			want = w
		}
	}
	want += facetGutterPadding
	if want < facetGutterMinCells {
		want = facetGutterMinCells
	}
	if f.width-want < kit.TreeMinSize.Width {
		// Too narrow to carry both: the tree keeps the whole region and the
		// gutter collapses rather than squeezing the rows it exists to explain.
		return 0
	}
	return want
}

// gutterRowTexts renders the plain (unstyled, unpadded) text of every gutter
// row: the "every value" row first, then one row per value, each with its
// count.
func (f *treeField) gutterRowTexts(values []string) []string {
	rows := []string{f.gutterRowText("all", f.facetCount(""))}
	for _, v := range values {
		rows = append(rows, f.gutterRowText(f.displayValue(v), f.facetCount(v)))
	}
	return rows
}

func (f *treeField) gutterRowText(label string, count int) string {
	return label + " " + strconv.Itoa(count)
}

// displayValue renders one facet value for the gutter.
func (f *treeField) displayValue(value string) string {
	if f.facetDisplay == nil {
		return value
	}
	if shown := f.facetDisplay(value); shown != "" {
		return shown
	}
	return value
}

// gutterLines renders the facet gutter as exactly f.height lines of exactly
// gutterWidth cells: the label, then the "all" row and one row per value, with
// the active row marked. It returns nil when no gutter is drawn.
func (f *treeField) gutterLines() []string {
	gw := f.gutterWidth()
	if gw == 0 {
		return nil
	}
	styles := f.th.Styles()
	values := f.facetValues()
	lines := []string{fitCell(styles.Header, f.facetLabel, gw)}
	for i, text := range f.gutterRowTexts(values) {
		style := styles.Muted
		marker := "  "
		if i == f.facetState {
			style = styles.Selected
			marker = "> "
		}
		lines = append(lines, fitCell(style, marker+text, gw))
	}
	for len(lines) < f.height {
		lines = append(lines, fitCell(styles.Base, "", gw))
	}
	return lines[:f.height]
}

// reset restores the draft's baseline selection value. The live tree forest is
// left as-is (its display is rebuilt from the source on the next scan); what a
// commit persists is the accessor value, which reset returns to baseline.
func (f *treeField) reset(d *Draft) { f.acc.Set(d.Working(), f.acc.Get(d.Baseline())) }

func (f *treeField) Dirty(d *Draft) bool {
	w := f.acc.Get(d.Working())
	b := f.acc.Get(d.Baseline())
	return !selectionsEqual(w, b)
}

// Validate blocks the commit when the forest still contains a display-only
// Conflict node, and otherwise checks the derived selection is internally
// consistent. It inspects the WHOLE forest, so a conflict hidden by the current
// facet still blocks.
func (f *treeField) Validate(d *Draft) error {
	if f.reconcileErr != nil {
		return f.reconcileErr
	}
	if HasConflict(f.selectionRoots()) {
		return fmt.Errorf(
			"unresolved transcript selection conflict.\n"+
				"what: the selection %q still contains an entry whose backing project or worktree no longer exists.\n"+
				"why: a saved selection points at something the current scan cannot reconcile, so its true state is ambiguous.\n"+
				"where: settings.treeField.Validate (field %q).\n"+
				"means: the settings flow will not persist an ambiguous selection.\n"+
				"fix: resolve the highlighted conflicting entry (re-check or clear it) and confirm again.",
			f.label, f.key)
	}
	return f.acc.Get(d.Working()).Validate()
}

// pruneForest returns a view of roots holding only the nodes that carry value
// for key, plus the ancestors that still have one. A matching node is the
// ORIGINAL pointer, so a selection made through the view mutates the real node;
// an ancestor is a shallow copy carrying only its retained children, so
// narrowing the view never rewrites the full forest's shape.
func pruneForest(roots []*kit.TreeNode, key, value string) []*kit.TreeNode {
	var out []*kit.TreeNode
	for _, r := range roots {
		if kept := pruneNode(r, key, value); kept != nil {
			out = append(out, kept)
		}
	}
	return out
}

func pruneNode(n *kit.TreeNode, key, value string) *kit.TreeNode {
	if v := metaOf(n, key); v != "" {
		if v == value {
			return n
		}
		return nil
	}
	var kept []*kit.TreeNode
	for _, c := range n.Children {
		if pc := pruneNode(c, key, value); pc != nil {
			kept = append(kept, pc)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	cp := *n
	cp.Children = kept
	return &cp
}

// metaOf returns the value n carries for key, or "".
func metaOf(n *kit.TreeNode, key string) string {
	if n.Meta == nil {
		return ""
	}
	return n.Meta[key]
}

// walkNodes visits n and every descendant in pre-order.
func walkNodes(n *kit.TreeNode, fn func(*kit.TreeNode)) {
	fn(n)
	for _, c := range n.Children {
		walkNodes(c, fn)
	}
}

// sameNodes reports whether two forests are the same nodes in the same order,
// by pointer identity - the test for "the tree is already rendering this view".
func sameNodes(a, b []*kit.TreeNode) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fitCell truncates then pads a plain string to exactly width cells and styles
// the whole run, so a column's background is unbroken.
func fitCell(style lipgloss.Style, s string, width int) string {
	if width <= 0 {
		return ""
	}
	clipped := clip(s, width)
	if pad := width - lipgloss.Width(clipped); pad > 0 {
		clipped += spaceRun(pad)
	}
	return style.Render(clipped)
}

// spaceRun returns n spaces (n<=0 yields "").
func spaceRun(n int) string {
	if n <= 0 {
		return ""
	}
	out := make([]byte, n)
	for i := range out {
		out[i] = ' '
	}
	return string(out)
}

// joinColumns lays a fixed-width left column beside a body, line for line. Each
// output row is one left cell followed by the body's matching line, so the two
// stay aligned even when the body renders fewer lines than the column has.
func joinColumns(left []string, body string) string {
	bodyLines := strings.Split(body, "\n")
	rows := make([]string, 0, len(left))
	for i, cell := range left {
		line := ""
		if i < len(bodyLines) {
			line = bodyLines[i]
		}
		rows = append(rows, cell+line)
	}
	// A body taller than the column keeps its remaining lines, indented past the
	// column so nothing is silently dropped.
	for i := len(left); i < len(bodyLines); i++ {
		rows = append(rows, bodyLines[i])
	}
	return joinLines(rows)
}

// selectionsEqual compares two derived selections by value (mode plus the
// per-harness allowlist), tolerant of nil-vs-empty maps.
func selectionsEqual(a, b TreeSelection) bool {
	if a.Mode != b.Mode {
		return false
	}
	if len(a.Harnesses) != len(b.Harnesses) {
		return false
	}
	for h, ac := range a.Harnesses {
		bc, ok := b.Harnesses[h]
		if !ok {
			return false
		}
		if !sameSet(ac.Sessions, bc.Sessions) {
			return false
		}
		if !sameSet(ac.Exclusions.Sessions, bc.Exclusions.Sessions) || len(ac.Exclusions.Branches) != len(bc.Exclusions.Branches) {
			return false
		}
		for _, exclusion := range ac.Exclusions.Branches {
			found := false
			for _, other := range bc.Exclusions.Branches {
				if exclusion.ClonePath == other.ClonePath && sameSet(exclusion.Branches, other.Branches) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		if len(ac.Projects) != len(bc.Projects) {
			return false
		}
		for _, ap := range ac.Projects {
			found := false
			for _, bp := range bc.Projects {
				if ap.GitRemote == bp.GitRemote && ap.Name == bp.Name &&
					sameSet(ap.ClonePaths, bp.ClonePaths) && sameSet(ap.Branches, bp.Branches) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
	}
	return true
}
