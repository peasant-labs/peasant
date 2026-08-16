package kit

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// TreeMinSize is the smallest region a Tree draws into: one row of content and
// enough width for a cursor, an expand glyph, a checkbox, and a couple of
// label cells. Below it the Tree renders a single truncation-safe line rather
// than panicking or overlapping chrome.
var TreeMinSize = Size{Width: 6, Height: 1}

// TriState is the selection state of a [TreeNode] in the kit tree. It is a
// closed, strongly-typed enum (never a bare string or bool) so every consumer
// - the settings TreeField's TreeSelection, the render layer, the rollup
// logic - compares against a named constant. Unchecked is the zero value so a
// freshly-constructed node defaults to "not selected".
type TriState int

const (
	// Unchecked is a node the user has not selected. It is the zero value.
	Unchecked TriState = iota
	// Partial is an interior node some but not all of whose descendants are
	// Checked (or one of whose descendants is in Conflict): the parent cannot
	// be a clean Checked/Unchecked, so it rolls up to Partial.
	Partial
	// Checked is a fully-selected node: a leaf the user selected, or an
	// interior node all of whose selectable descendants are Checked.
	Checked
	// Conflict is a DISPLAY-ONLY state a [TreeSource] assigns to a node whose
	// backing reality is inconsistent - e.g. a persisted selection that points
	// at a git worktree that has since been deleted. It renders distinctly and
	// is NEVER produced by user toggling and NEVER persisted: the settings
	// slice's Validate/Commit inspects nodes for it and blocks, but a Conflict
	// never round-trips back into stored selection state.
	Conflict
)

// IsValid reports whether s is one of the four known TriState values.
func (s TriState) IsValid() bool {
	switch s {
	case Unchecked, Partial, Checked, Conflict:
		return true
	default:
		return false
	}
}

// String returns a stable, lower-case name for s, or "unknown" for an
// out-of-range value - mirroring the leading-sentinel String() convention the
// keymap and ingest enums use.
func (s TriState) String() string {
	switch s {
	case Unchecked:
		return "unchecked"
	case Partial:
		return "partial"
	case Checked:
		return "checked"
	case Conflict:
		return "conflict"
	default:
		return "unknown"
	}
}

// TreeFilterMode is the closed lifecycle of hierarchy-wide text search.
type TreeFilterMode uint8

const (
	// TreeFilterModeUnknown is the invalid zero value.
	TreeFilterModeUnknown TreeFilterMode = iota
	// TreeFilterInactive means no text query is being edited or kept.
	TreeFilterInactive
	// TreeFilterEditing means printable input and delete edit the query.
	TreeFilterEditing
	// TreeFilterKept means editing ended while the query remains active.
	TreeFilterKept
)

// IsValid reports whether m is a known filter lifecycle state.
func (m TreeFilterMode) IsValid() bool {
	switch m {
	case TreeFilterInactive, TreeFilterEditing, TreeFilterKept:
		return true
	default:
		return false
	}
}

// String returns a stable lower-case name for m, or "unknown".
func (m TreeFilterMode) String() string {
	switch m {
	case TreeFilterInactive:
		return "inactive"
	case TreeFilterEditing:
		return "editing"
	case TreeFilterKept:
		return "kept"
	default:
		return "unknown"
	}
}

// TreeFilterState is the typed, inspectable text-search state. Query matches
// labels at every hierarchy depth; ancestor rows remain as context. It never
// owns or copies checkbox state: projections retain shared TreeNode identity.
type TreeFilterState struct {
	Mode  TreeFilterMode
	Query string
}

// Active reports whether a non-empty query currently narrows the visible tree.
func (s TreeFilterState) Active() bool { return strings.TrimSpace(s.Query) != "" }

// Editing reports whether text input currently edits the query.
func (s TreeFilterState) Editing() bool { return s.Mode == TreeFilterEditing }

// AvailableActions reports the filter lifecycle actions dispatchable in this
// state. It is itself a keymap.Availability, so search-mode dispatch, footer,
// and help can consume exactly the same typed set.
func (s TreeFilterState) AvailableActions() []keymap.ActionID {
	switch s.Mode {
	case TreeFilterEditing:
		return []keymap.ActionID{
			keymap.ActionDeleteFilter,
			keymap.ActionKeepFilter,
			keymap.ActionClearFilter,
		}
	case TreeFilterKept:
		return []keymap.ActionID{keymap.ActionSearch, keymap.ActionClearFilter}
	case TreeFilterInactive:
		return []keymap.ActionID{keymap.ActionSearch}
	default:
		return nil
	}
}

var _ keymap.Availability = TreeFilterState{}

// TreeOverflow reports whether usable rows exist above or below the current
// viewport. The view renders markers from this state rather than guessing from
// cursor position.
type TreeOverflow struct {
	Top    bool
	Bottom bool
}

// Any reports whether either viewport edge has hidden rows.
func (o TreeOverflow) Any() bool { return o.Top || o.Bottom }

// TreeNode is one node in the ordered hierarchy the kit tree renders. The
// kickstart selection source uses project -> branch -> session. A node owns its
// selection State, ordered Children, and a display-only Meta bag. The tree
// mutates State in place through the Children pointers as the user toggles and
// as interior nodes roll up, so a [TreeSource] hands back the *TreeNode graph it
// wants the tree to own.
type TreeNode struct {
	// ID is a stable identifier for the node (a provider slug, a remote URL, a
	// worktree path, a session id). It is display/lookup data, not rendered
	// chrome; the settings TreeSelection keys off it.
	ID string
	// Label is the human-readable text drawn for the row.
	Label string
	// State is the node's current TriState. For a leaf it is the selection the
	// user set (or a Conflict the source assigned); for an interior node it is
	// recomputed by rollup from its Children.
	State TriState
	// Children are the node's ordered child nodes. A nil/empty slice marks a
	// leaf.
	Children []*TreeNode
	// Meta is a display-only key/value bag (a branch name, a last-scanned
	// timestamp, a conflict reason). It never affects selection, rollup, or
	// dispatch - it exists so a source can attach context a future row
	// renderer or the settings detail pane can surface.
	Meta map[string]string
	// projectionOrigin points from a shallow ancestor copy in a filtered view to
	// the canonical node whose expansion/cursor identity it represents. Sources
	// cannot set it; [ProjectTreeNode] is the one projection constructor.
	projectionOrigin *TreeNode
}

// ProjectTreeNode returns a shallow, view-only copy of node with children as
// its visible descendants. The copy retains node's canonical identity for
// cursor and expansion state, while selectable descendants remain the caller's
// shared pointers. A nil node returns nil.
func ProjectTreeNode(node *TreeNode, children []*TreeNode) *TreeNode {
	if node == nil {
		return nil
	}
	cp := *node
	cp.Children = children
	cp.projectionOrigin = canonicalProjectionOrigin(node)
	return &cp
}

func canonicalProjectionOrigin(node *TreeNode) *TreeNode {
	for node != nil && node.projectionOrigin != nil {
		node = node.projectionOrigin
	}
	return node
}

// Meta keys the tree ROW RENDERER understands. A [TreeSource] attaches them to
// a node to annotate its row; like every other Meta entry they are display-only
// and never affect selection, rollup, or dispatch. They live here, with the
// renderer that reads them, and the settings layer re-exports them so a scanner
// writes exactly the keys this renderer reads.
const (
	// MetaChildCount carries the number of child (subagent) sessions a parent
	// session groups, in base 10. A parent session stays a LEAF row - its
	// children are summarised on the row instead of nested - so the number of
	// rows never depends on how many subagents a session spawned.
	MetaChildCount = "childCount"
	// MetaIngested marks a node whose transcript the local store already holds,
	// so its row reads as already imported.
	MetaIngested = "ingested"
	// MetaIngestedValue is the value MetaIngested carries when set.
	MetaIngestedValue = "true"
	// MetaTracked marks a row included by the selection saved before the current
	// editing run. It is intentionally independent from MetaIngested (local
	// store presence) and the node's mutable checkbox State.
	MetaTracked = "tracked"
	// MetaTrackedValue is the value MetaTracked carries when set.
	MetaTrackedValue = "true"
)

// isLeaf reports whether n has no children.
func (n *TreeNode) isLeaf() bool { return len(n.Children) == 0 }

// meta returns the value n carries for key, or "" when it carries none.
func (n *TreeNode) meta(key string) string {
	if n.Meta == nil {
		return ""
	}
	return n.Meta[key]
}

// TreeSource loads the tree's node forest, potentially slowly (for example, a
// real scan of projects/branches/sessions). It is the ONLY asynchronous
// field-data source in this component vocabulary: the kit tree renders a
// spinner while Load is in flight and drops a late result whose generation no
// longer matches (the stale guard). Load must honor ctx cancellation so a
// re-source or teardown can abandon an in-flight scan.
type TreeSource interface {
	// Load returns the root nodes of the tree, or an error. It should return
	// promptly once ctx is cancelled.
	Load(ctx context.Context) ([]*TreeNode, error)
}

// treeLoadedMsg is the internal message a Tree's load command emits when a
// [TreeSource.Load] returns. owner isolates concrete Tree instances; gen lets
// that owner drop a stale load after it was re-sourced or replaced.
type treeLoadedMsg struct {
	owner asyncOwnerID
	gen   uint64
	nodes []*TreeNode
	err   error
}

// Tree is the tri-state selection tree: it renders an ordered hierarchy with
// tri-state checkboxes, propagates a parent
// toggle down to its descendants, rolls child state back up to Checked/Partial,
// and renders a Conflict node distinctly. While its [TreeSource] is loading it
// shows the kit [Spinner] AND keeps processing input (navigation across known
// chrome, back, help) rather than blocking or drawing a blank frame; the
// loaded forest replaces the spinner. A late load result is dropped by
// generation (the stale guard) and an in-flight load is cancellable via
// context.
type Tree struct {
	theme   theme.Theme
	keymap  keymap.Keymap
	src     TreeSource
	spinner Spinner
	owner   asyncOwnerID

	roots     []*TreeNode
	visible   []*TreeNode
	projected bool
	expanded  map[*TreeNode]bool

	loading bool
	gen     uint64
	cancel  context.CancelFunc
	err     error

	cursor  int
	offset  int
	width   int
	height  int
	focused bool
	filter  TreeFilterState
	// filterFallback remembers the row and viewport under the cursor when search
	// starts, so clearing returns to that exact canonical context even when the
	// projection temporarily showed a same-named row from another project.
	filterFallback cursorAnchor
	// projectionAnchor retains the canonical row and its viewport position for
	// the lifetime of an external roots projection (for example, a harness
	// facet), including an intermediate view that hides that row.
	projectionAnchor      cursorAnchor
	projectionViewportRow int
}

// NewTree builds a Tree over theme t that loads its forest from src. It starts
// empty and not loading; call [Tree.Load] (batched with [Tree.Init] to start
// the spinner) to begin a scan.
func NewTree(t theme.Theme, src TreeSource) Tree {
	return Tree{
		theme:            t,
		keymap:           keymap.Default(),
		src:              src,
		spinner:          NewSpinner(t, "scanning projects"),
		owner:            nextAsyncOwnerID(),
		expanded:         map[*TreeNode]bool{},
		width:            TreeMinSize.Width,
		height:           TreeMinSize.Height,
		filter:           TreeFilterState{Mode: TreeFilterInactive},
		filterFallback:   cursorAnchor{depth: -1},
		projectionAnchor: cursorAnchor{depth: -1},
	}
}

// WithSource returns a copy of t re-pointed at src. It preserves the in-flight
// generation and cancel func so a subsequent [Tree.Load] cancels any prior scan
// and its result is dropped by the stale guard - the re-source path.
func (t Tree) WithSource(src TreeSource) Tree {
	t.src = src
	return t
}

// Init returns the command that starts the spinner animation when the tree is
// loading, or nil otherwise. A caller mounting the tree batches this with the
// command from [Tree.Load] so the spinner ticks while the scan runs.
func (t Tree) Init() tea.Cmd {
	if t.loading {
		return t.spinner.Tick()
	}
	return nil
}

// OwnsAsync reports whether msg is an asynchronous result or active spinner
// tick this exact Tree may consume. Result ownership is instance-specific;
// value copies keep the same immutable owner identity.
func (t Tree) OwnsAsync(msg tea.Msg) bool {
	switch m := msg.(type) {
	case treeLoadedMsg:
		return t.owner.valid() && m.owner.valid() && m.owner == t.owner
	case spinner.TickMsg:
		return t.loading && t.spinner.ownsTick(m)
	default:
		return false
	}
}

// Load begins (or restarts) a scan: it cancels any in-flight load, opens a
// fresh cancellable context, bumps the load generation, marks the tree
// loading, and returns the command that runs [TreeSource.Load] and emits its
// result tagged with immutable owner identity and this generation. A foreign
// owner or stale generation is dropped by [Tree.Update]. Batch the returned
// command with [Tree.Init] to also start the spinner.
func (t Tree) Load() (Tree, tea.Cmd) {
	if t.cancel != nil {
		t.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	t.gen++
	gen := t.gen
	owner := t.owner
	t.loading = true
	t.err = nil
	t.projectionAnchor = cursorAnchor{depth: -1}
	t.projectionViewportRow = 0
	src := t.src
	cmd := func() tea.Msg {
		nodes, err := src.Load(ctx)
		return treeLoadedMsg{owner: owner, gen: gen, nodes: nodes, err: err}
	}
	return t, cmd
}

// WithRoots returns a copy of t rendering the given forest instead of the one
// its source last loaded, rolls the new forest's interior states up, and puts
// the cursor back at the top. It is how a caller renders a FILTERED VIEW of a
// loaded forest: the view shares the leaf pointers of the full forest, so a
// selection made through it is a selection of the real nodes, and the caller
// keeps the full forest for deriving what to persist. It does not re-run a
// scan; use [Tree.Load] for that.
func (t Tree) WithRoots(roots []*TreeNode) Tree {
	t.projectionAnchor = cursorAnchor{depth: -1}
	t.projectionViewportRow = 0
	return t.replaceRoots(roots)
}

func (t Tree) replaceRoots(roots []*TreeNode) Tree {
	t.roots = roots
	t.cursor = 0
	t.offset = 0
	t.applyTextProjection(false)
	return t
}

// WithProjectedRoots replaces the installed view while retaining the current
// canonical row and its viewport position when that row remains present. It is
// for view-only projections such as a harness facet; source loads continue to
// reset to the first row through the normal load path. When the anchored row is
// absent, the new projection falls back to its first row predictably.
func (t Tree) WithProjectedRoots(roots []*TreeNode) Tree {
	t = t.captureProjectionAnchor()
	return t.replaceRootsAt(roots, t.projectionAnchor, t.projectionViewportRow)
}

// WithUnprojectedRoots restores the row captured when external projection
// began, when it exists in roots, then ends that projection lifecycle. An
// intermediate projection may hide the row without losing the eventual return
// target.
func (t Tree) WithUnprojectedRoots(roots []*TreeNode) Tree {
	t = t.captureProjectionAnchor()
	anchor := t.projectionAnchor
	viewportRow := t.projectionViewportRow
	t = t.replaceRootsAt(roots, anchor, viewportRow)
	t.projectionAnchor = cursorAnchor{depth: -1}
	t.projectionViewportRow = 0
	return t
}

// CollapseInitial records each given interior node as collapsed, so its
// children start hidden until the user expands it, without changing any other
// node's expansion. It is how a source seeds a folded-by-default root - a
// project with no already-imported or tracked sessions, say - while leaving the
// expand/collapse keys fully in control afterward. Leaves and nil nodes are
// ignored. Because expansion is keyed by canonical node identity, a node
// collapsed here stays collapsed under a later facet projection of the same
// forest.
func (t Tree) CollapseInitial(nodes []*TreeNode) Tree {
	for _, node := range nodes {
		if node == nil || node.isLeaf() {
			continue
		}
		t.expanded[t.canonicalNode(node)] = false
	}
	t.clampWindow()
	return t
}

func (t Tree) captureProjectionAnchor() Tree {
	current := t.currentAnchor()
	if current.depth < 0 {
		return t
	}
	if t.projectionAnchor.depth < 0 || t.containsAnchor(t.projectionAnchor) {
		t.projectionAnchor = current
		t.projectionViewportRow = t.cursor - t.offset
	}
	return t
}

func (t Tree) replaceRootsAt(roots []*TreeNode, anchor cursorAnchor, viewportRow int) Tree {
	t = t.replaceRoots(roots)
	if anchor.depth < 0 {
		return t
	}
	for i, row := range t.visibleRows() {
		if !t.rowMatchesAnchor(row, anchor) {
			continue
		}
		t.cursor = i
		t.offset = i - viewportRow
		t.clampWindow()
		return t
	}
	return t
}

func (t Tree) containsAnchor(anchor cursorAnchor) bool {
	for _, row := range t.visibleRows() {
		if t.rowMatchesAnchor(row, anchor) {
			return true
		}
	}
	return false
}

func (t Tree) rowMatchesAnchor(row treeRow, anchor cursorAnchor) bool {
	return anchor.node != nil && t.canonicalNode(row.node) == anchor.node
}

// Focus gives the Tree keyboard focus.
func (t *Tree) Focus() tea.Cmd { t.focused = true; return nil }

// Blur removes keyboard focus.
func (t *Tree) Blur() { t.focused = false }

// Focused reports whether the Tree holds focus.
func (t Tree) Focused() bool { return t.focused }

var _ Focusable = (*Tree)(nil)

// SetSize sets the inner region the Tree draws into (and the width the spinner
// clips to), re-clamping the scroll window so the cursor stays visible.
func (t *Tree) SetSize(width, height int) {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	t.width = width
	t.height = height
	t.spinner.SetSize(width, 1)
	t.clampWindow()
}

var _ Sizeable = (*Tree)(nil)

// Loading reports whether a scan is currently in flight.
func (t Tree) Loading() bool { return t.loading }

// Err reports the error from the last completed load, or nil.
func (t Tree) Err() error { return t.err }

// Roots returns the tree's current root nodes (nil while loading or before the
// first load completes). These are the unfiltered roots most recently loaded or
// installed with [Tree.WithRoots]; [Tree.VisibleRoots] reports the current text
// projection.
func (t Tree) Roots() []*TreeNode { return t.roots }

// VisibleRoots returns the hierarchy-wide text projection. It shares selectable
// node identity with Roots and contains shallow ancestor copies only where
// needed to retain context. With no active query it is exactly Roots.
func (t Tree) VisibleRoots() []*TreeNode { return t.renderRoots() }

// Cursor reports the current cursor index into the visible (expanded) rows.
func (t Tree) Cursor() int { return t.cursor }

// ViewportOffset reports the first visible row index in the current projection.
// It is observable scroll state used by hosts that preserve a canonical cursor
// across view-only projections.
func (t Tree) ViewportOffset() int { return t.offset }

// FilterState reports a copy of the hierarchy-wide text-search state.
func (t Tree) FilterState() TreeFilterState { return t.filter }

// Overflow reports whether the current visible viewport has rows above or
// below it. Loading and empty trees report no overflow.
func (t Tree) Overflow() TreeOverflow {
	if t.loading || t.height < 1 {
		return TreeOverflow{}
	}
	count := len(t.visibleRows())
	if count == 0 {
		return TreeOverflow{}
	}
	return TreeOverflow{
		Top:    t.offset > 0,
		Bottom: t.offset+t.height < count,
	}
}

// CurrentNode returns the node under the cursor and true, or nil and false
// when there are no visible rows.
func (t Tree) CurrentNode() (*TreeNode, bool) {
	rows := t.visibleRows()
	if t.cursor < 0 || t.cursor >= len(rows) {
		return nil, false
	}
	return rows[t.cursor].node, true
}

// HasConflict reports whether any node in the forest is in Conflict - the
// signal the settings slice's Validate/Commit consults to block on a
// display-only conflict without persisting it.
func (t Tree) HasConflict() bool {
	var any bool
	for _, r := range t.roots {
		walk(r, func(n *TreeNode) {
			if n.State == Conflict {
				any = true
			}
		})
	}
	return any
}

// AvailableActions reports only actions that can change the Tree in its current
// state. Loading/failed/empty forests expose no row operations; navigation is
// edge-aware; and expand, collapse, and selection follow the current row and
// projected forest shape. Dispatch, footer, and help consume this same set.
func (t Tree) AvailableActions() []keymap.ActionID {
	if t.loading || t.err != nil {
		return []keymap.ActionID{keymap.ActionHelp, keymap.ActionBack}
	}
	if t.filter.Editing() {
		return t.filter.AvailableActions()
	}
	actions := make([]keymap.ActionID, 0, 18)
	rows := t.visibleRows()
	if t.cursor > 0 && len(rows) > 0 {
		actions = append(actions, keymap.ActionUp, keymap.ActionPageUp, keymap.ActionTop)
	}
	if t.cursor >= 0 && t.cursor+1 < len(rows) {
		actions = append(actions, keymap.ActionDown, keymap.ActionPageDown, keymap.ActionBottom)
	}
	if len(t.roots) > 0 {
		actions = append(actions, keymap.ActionSearch)
	}
	if t.filter.Active() {
		actions = append(actions, keymap.ActionClearFilter)
	}
	if previous, next := t.projectDirections(); previous {
		actions = append(actions, keymap.ActionPrevProject)
		if next {
			actions = append(actions, keymap.ActionNextProject)
		}
	} else if next {
		actions = append(actions, keymap.ActionNextProject)
	}
	if node, ok := t.CurrentNode(); ok {
		expansion := t.visibleExpansion(node)
		if expansion.controllable {
			if expansion.expanded {
				actions = append(actions, keymap.ActionCollapse)
			} else {
				actions = append(actions, keymap.ActionExpand)
			}
		}
		if t.subtreeHasSelectable(node) {
			actions = append(actions, keymap.ActionToggle)
		}
		if t.levelCanExpand(node) {
			actions = append(actions, keymap.ActionExpandLevel)
		}
		if t.levelCanCollapse(node) {
			actions = append(actions, keymap.ActionCollapseLevel)
		}
		if t.projectHasUnselected(node) {
			actions = append(actions, keymap.ActionSelectUnderProject)
		}
	}
	if t.anyInteriorCanExpand() {
		actions = append(actions, keymap.ActionExpandAll)
	}
	if t.anyInteriorCanCollapse() {
		actions = append(actions, keymap.ActionCollapseAll)
	}
	if t.hasSelectableNode() {
		actions = append(actions, keymap.ActionSelectAll)
	}
	return append(actions, keymap.ActionHelp, keymap.ActionBack)
}

func (t Tree) nodeExpanded(node *TreeNode) bool {
	if node == nil || node.isLeaf() {
		return false
	}
	expanded, recorded := t.expanded[t.canonicalNode(node)]
	return !recorded || expanded
}

type treeVisibleExpansion struct {
	expanded     bool
	controllable bool
}

// visibleExpansion is the single interpretation of expansion in the rendered
// projection. Shallow ancestors retained as search context are force-open so
// their matching descendants stay visible, but they are not controllable: an
// expand/collapse command would only mutate hidden canonical state without
// changing the projected view.
func (t Tree) visibleExpansion(node *TreeNode) treeVisibleExpansion {
	if node == nil || node.isLeaf() {
		return treeVisibleExpansion{}
	}
	if t.projected && node.projectionOrigin != nil {
		return treeVisibleExpansion{expanded: true}
	}
	return treeVisibleExpansion{expanded: t.nodeExpanded(node), controllable: true}
}

func (t Tree) projectDirections() (previous, next bool) {
	node, ok := t.CurrentNode()
	if !ok {
		return false, false
	}
	root := t.rootAncestor(node)
	roots := t.renderRoots()
	for i, candidate := range roots {
		if candidate != root {
			continue
		}
		return i > 0, i+1 < len(roots)
	}
	return false, false
}

func (t Tree) subtreeHasSelectable(node *TreeNode) bool {
	selectable := false
	walk(node, func(candidate *TreeNode) {
		if candidate.isLeaf() && candidate.State != Conflict {
			selectable = true
		}
	})
	return selectable
}

// HasUnselected reports whether the visible projection still holds a selectable
// node that is not Checked. The selection field reads it to label the select/
// clear-all toggle - "select all" while something is unselected, "clear all"
// once everything is - so the key's advertised action always matches what it does.
func (t Tree) HasUnselected() bool { return t.hasUnselectedSelectableNode() }

// hasSelectableNode reports whether the visible projection holds any selectable
// leaf at all, Checked or not. The select/clear-all toggle is offered whenever
// one exists, so the key is never inert on a forest that starts fully selected.
func (t Tree) hasSelectableNode() bool {
	for _, root := range t.renderRoots() {
		found := false
		walk(root, func(c *TreeNode) {
			if c.isLeaf() && c.State != Conflict {
				found = true
			}
		})
		if found {
			return true
		}
	}
	return false
}

// hasUnselectedSelectableNode reports whether the current visible projection
// holds at least one selectable leaf that is not already Checked. It steers the
// select/clear-all toggle: while it is true the key selects the projection, and
// once it is false the key clears it.
func (t Tree) hasUnselectedSelectableNode() bool {
	for _, root := range t.renderRoots() {
		unselected := false
		walk(root, func(candidate *TreeNode) {
			if candidate.isLeaf() && candidate.State != Conflict && candidate.State != Checked {
				unselected = true
			}
		})
		if unselected {
			return true
		}
	}
	return false
}

func (t Tree) projectHasUnselected(node *TreeNode) bool {
	root := t.rootAncestor(node)
	unselected := false
	walk(root, func(candidate *TreeNode) {
		if candidate.isLeaf() && candidate.State != Conflict && candidate.State != Checked {
			unselected = true
		}
	})
	return unselected
}

func (t Tree) levelCanExpand(node *TreeNode) bool {
	root := t.rootAncestor(node)
	for _, child := range root.Children {
		expansion := t.visibleExpansion(child)
		if expansion.controllable && !expansion.expanded {
			return true
		}
	}
	return false
}

func (t Tree) levelCanCollapse(node *TreeNode) bool {
	root := t.rootAncestor(node)
	for _, child := range root.Children {
		expansion := t.visibleExpansion(child)
		if expansion.controllable && expansion.expanded {
			return true
		}
	}
	return false
}

func (t Tree) anyInteriorCanExpand() bool {
	canExpand := false
	for _, root := range t.renderRoots() {
		walk(root, func(node *TreeNode) {
			expansion := t.visibleExpansion(node)
			if expansion.controllable && !expansion.expanded {
				canExpand = true
			}
		})
	}
	return canExpand
}

func (t Tree) anyInteriorCanCollapse() bool {
	canCollapse := false
	for _, root := range t.renderRoots() {
		walk(root, func(node *TreeNode) {
			expansion := t.visibleExpansion(node)
			if expansion.controllable && expansion.expanded {
				canCollapse = true
			}
		})
	}
	return canCollapse
}

var _ keymap.Availability = Tree{}

// Update advances the tree on a load result, a spinner tick, or a dispatched
// key action, and returns the concrete Tree plus any follow-up command.
func (t Tree) Update(msg tea.Msg) (Tree, tea.Cmd) {
	switch m := msg.(type) {
	case treeLoadedMsg:
		// Instance ownership is checked before generation. Two newly-mounted
		// trees both begin at generation one, so generation alone cannot
		// distinguish their first completions.
		if !t.owner.valid() || !m.owner.valid() || m.owner != t.owner {
			return t, nil
		}
		// Stale guard: a result whose generation no longer matches was
		// re-sourced or replaced before it returned - drop it.
		if m.gen != t.gen {
			return t, nil
		}
		t.loading = false
		if m.err != nil {
			t.err = m.err
			t.roots = nil
			return t, nil
		}
		t.roots = m.nodes
		t.cursor = 0
		t.offset = 0
		t.applyTextProjection(false)
		return t, nil
	case spinner.TickMsg:
		if !t.loading {
			return t, nil
		}
		var cmd tea.Cmd
		t.spinner, cmd = t.spinner.Update(msg)
		return t, cmd
	case tea.KeyPressMsg:
		return t.handleKey(m)
	default:
		return t, nil
	}
}

// handleKey dispatches a key press through the keymap. Unmatched keys, and any
// navigation while loading, leave the tree unchanged (input stays live but
// there is nothing yet to move over).
func (t Tree) handleKey(msg tea.KeyPressMsg) (Tree, tea.Cmd) {
	if t.filter.Editing() {
		return t.handleFilterKey(msg)
	}
	action, ok := keymap.Match(t.keymap, msg, t)
	if !ok {
		return t, nil
	}
	if t.loading {
		// Input is processed (never dropped/blocked) but there is no forest to
		// navigate yet; Back/Help are handled by the parent.
		return t, nil
	}
	rows := t.visibleRows()
	page := t.height
	if page < 1 {
		page = 1
	}
	switch action {
	case keymap.ActionUp:
		t.moveCursor(-1, len(rows))
	case keymap.ActionDown:
		t.moveCursor(1, len(rows))
	case keymap.ActionPageUp:
		t.moveCursor(-page, len(rows))
	case keymap.ActionPageDown:
		t.moveCursor(page, len(rows))
	case keymap.ActionTop:
		t.moveCursor(-len(rows), len(rows))
	case keymap.ActionBottom:
		t.moveCursor(len(rows), len(rows))
	case keymap.ActionNextProject:
		t.moveToProject(1)
	case keymap.ActionPrevProject:
		t.moveToProject(-1)
	case keymap.ActionSearch:
		if !t.filter.Active() || t.filterFallback.depth < 0 {
			t.filterFallback = t.currentAnchor()
		}
		t.filter.Mode = TreeFilterEditing
	case keymap.ActionClearFilter:
		t.clearFilter()
	case keymap.ActionExpand:
		if node, okc := t.CurrentNode(); okc && !node.isLeaf() {
			t.expanded[t.canonicalNode(node)] = true
			t.clampWindow()
		}
	case keymap.ActionCollapse:
		if node, okc := t.CurrentNode(); okc && !node.isLeaf() {
			t.expanded[t.canonicalNode(node)] = false
			t.clampWindow()
		}
	case keymap.ActionExpandLevel:
		t.setLevelExpanded(true)
	case keymap.ActionCollapseLevel:
		t.setLevelExpanded(false)
	case keymap.ActionExpandAll:
		t.setAllExpanded(true)
	case keymap.ActionCollapseAll:
		t.setAllExpanded(false)
	case keymap.ActionToggle:
		if node, okc := t.CurrentNode(); okc {
			t.toggle(node)
		}
	case keymap.ActionSelectAll:
		t.toggleSelectAll()
	case keymap.ActionSelectUnderProject:
		t.selectUnderProject()
	}
	return t, nil
}

// handleFilterKey owns search-text editing. Lifecycle keys dispatch through the
// typed keymap; printable text is content forwarded by Bubble Tea rather than a
// second keybinding definition.
func (t Tree) handleFilterKey(msg tea.KeyPressMsg) (Tree, tea.Cmd) {
	if action, ok := keymap.Match(t.keymap, msg, t.filter); ok {
		switch action {
		case keymap.ActionDeleteFilter:
			if t.filter.Query != "" {
				t.filter.Query = trimLastGraphemeCluster(t.filter.Query)
				t.applyTextProjection(true)
			}
		case keymap.ActionKeepFilter:
			t.filter.Query = strings.TrimSpace(t.filter.Query)
			if t.filter.Query == "" {
				t.filter.Mode = TreeFilterInactive
			} else {
				t.filter.Mode = TreeFilterKept
			}
			t.applyTextProjection(true)
			if t.filter.Mode == TreeFilterInactive {
				t.filterFallback = cursorAnchor{depth: -1}
			}
		case keymap.ActionClearFilter:
			t.clearFilter()
		}
		return t, nil
	}
	if msg.Text == "" || (msg.Mod != 0 && msg.Mod != tea.ModShift) {
		return t, nil
	}
	t.filter.Query += msg.Text
	t.applyTextProjection(true)
	return t, nil
}

// clearFilter returns to the current unfiltered roots without changing
// expansion or any node's checkbox state.
func (t *Tree) clearFilter() {
	anchor := t.filterFallback
	if anchor.depth < 0 {
		anchor = t.currentAnchor()
	}
	t.filter.Query = ""
	t.filter.Mode = TreeFilterInactive
	t.applyTextProjectionAt(anchor)
	t.filterFallback = cursorAnchor{depth: -1}
}

// moveCursor moves the cursor by delta over count rows, clamped, then
// re-clamps the scroll window.
func (t *Tree) moveCursor(delta, count int) {
	if count == 0 {
		t.cursor = 0
		t.offset = 0
		return
	}
	t.cursor += delta
	if t.cursor < 0 {
		t.cursor = 0
	}
	if t.cursor >= count {
		t.cursor = count - 1
	}
	t.clampWindow()
}

// toggle flips a node's selection and propagates: a node currently Checked
// becomes Unchecked, anything else becomes Checked; the new state is pushed
// down to every selectable descendant (Conflict leaves are left as-is - a
// deleted-worktree selection cannot be selected), then interior states are
// rolled back up.
func (t *Tree) toggle(node *TreeNode) {
	if node.isLeaf() && node.State == Conflict {
		return
	}
	target := Checked
	if node.State == Checked {
		target = Unchecked
	}
	setSubtree(node, target)
	t.recompute()
}

// toggleSelectAll makes the select-all key a single, always-meaningful action:
// when any selectable node in the visible projection is unselected it selects
// the whole projection; when everything is already selected it clears it. The
// help label reflects which of the two the key will do (see fields_tree's
// WithSelectAllHelp), so it never reads as a hidden clear.
func (t *Tree) toggleSelectAll() {
	target := Checked
	if !t.hasUnselectedSelectableNode() {
		target = Unchecked
	}
	for _, r := range t.renderRoots() {
		setSubtree(r, target)
	}
	t.recompute()
}

// setAllExpanded expands (or collapses) every controllable interior node
// represented by the current visible projection, then re-clamps the scroll
// window so the cursor stays visible.
func (t *Tree) setAllExpanded(expand bool) {
	for _, r := range t.renderRoots() {
		walk(r, func(n *TreeNode) {
			if t.visibleExpansion(n).controllable {
				t.expanded[t.canonicalNode(n)] = expand
			}
		})
	}
	t.clampWindow()
}

// setLevelExpanded expands (or collapses) every controllable branch in the
// current projected project containing the cursor. Whether the cursor is on
// the project itself or one of its branches, its controllable branch level
// moves together.
func (t *Tree) setLevelExpanded(expand bool) {
	node, ok := t.CurrentNode()
	if !ok {
		return
	}
	root := t.rootAncestor(node)
	for _, c := range root.Children {
		if t.visibleExpansion(c).controllable {
			t.expanded[t.canonicalNode(c)] = expand
		}
	}
	t.clampWindow()
}

// selectUnderProject checks every selectable node represented under the current
// projected project containing the cursor, then rolls interior state back up.
// It always selects; it is not a clear toggle.
func (t *Tree) selectUnderProject() {
	node, ok := t.CurrentNode()
	if !ok {
		return
	}
	root := t.rootAncestor(node)
	setSubtree(root, Checked)
	t.recompute()
}

// moveToProject moves the cursor to the next (delta>0) or previous (delta<0)
// top-level project root, then re-clamps the scroll window. It clamps at the
// first and last project, so it is a no-op past either end.
func (t *Tree) moveToProject(delta int) {
	node, ok := t.CurrentNode()
	if !ok {
		return
	}
	root := t.rootAncestor(node)
	idx := -1
	roots := t.renderRoots()
	for i, r := range roots {
		if r == root {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	target := idx + delta
	if target < 0 {
		target = 0
	}
	if target >= len(roots) {
		target = len(roots) - 1
	}
	targetRoot := roots[target]
	for i, r := range t.visibleRows() {
		if r.node == targetRoot {
			t.cursor = i
			break
		}
	}
	t.clampWindow()
}

// rootAncestor returns the top-level forest root that node descends from
// (node itself when it is already a root).
func (t Tree) rootAncestor(node *TreeNode) *TreeNode {
	parent := t.parentIndex()
	for {
		p, ok := parent[node]
		if !ok || p == nil {
			return node
		}
		node = p
	}
}

// parentIndex maps each node to its parent across the whole forest. Roots are
// absent from the map. The tree stores only child pointers, so this is rebuilt
// on demand for the ancestor lookups the level and project operations need.
func (t Tree) parentIndex() map[*TreeNode]*TreeNode {
	parent := map[*TreeNode]*TreeNode{}
	var visit func(n *TreeNode)
	visit = func(n *TreeNode) {
		for _, c := range n.Children {
			parent[c] = n
			visit(c)
		}
	}
	for _, r := range t.renderRoots() {
		visit(r)
	}
	return parent
}

// renderRoots returns the current text projection, or the installed roots when
// no query is active.
func (t Tree) renderRoots() []*TreeNode {
	if t.projected {
		return t.visible
	}
	return t.roots
}

// canonicalNode resolves a shallow projection ancestor to the corresponding
// node in the installed roots. Matching nodes and descendants are already the
// original pointers and pass through unchanged.
func (t Tree) canonicalNode(node *TreeNode) *TreeNode {
	return canonicalProjectionOrigin(node)
}

// cursorAnchor records canonical pointer identity and depth so projection
// changes can keep the same row. Shallow projection ancestors resolve through
// projectionOrigin; display IDs are deliberately excluded because common
// branch names such as main and develop repeat under different projects.
type cursorAnchor struct {
	node   *TreeNode
	depth  int
	offset int
}

func (t Tree) currentAnchor() cursorAnchor {
	rows := t.visibleRows()
	if t.cursor < 0 || t.cursor >= len(rows) {
		return cursorAnchor{depth: -1}
	}
	row := rows[t.cursor]
	return cursorAnchor{node: t.canonicalNode(row.node), depth: row.depth, offset: t.offset}
}

// applyTextProjection rebuilds the hierarchy-wide text view from the installed
// roots. Matching subtrees retain original pointers; only ancestor context is
// shallow-copied and recorded in origins.
func (t *Tree) applyTextProjection(preserveCursor bool) {
	anchor := cursorAnchor{depth: -1}
	if preserveCursor {
		anchor = t.currentAnchor()
		if anchor.depth < 0 && t.filterFallback.depth >= 0 {
			anchor = t.filterFallback
		}
	}
	t.applyTextProjectionAt(anchor)
}

func (t *Tree) applyTextProjectionAt(anchor cursorAnchor) {
	t.recompute()
	query := strings.TrimSpace(t.filter.Query)
	if query == "" {
		t.visible = nil
		t.projected = false
	} else {
		t.visible = projectTree(t.roots, query)
		t.projected = true
	}
	t.recompute()
	rows := t.visibleRows()
	t.cursor = 0
	t.offset = 0
	if anchor.depth >= 0 {
		for i, row := range rows {
			if t.canonicalNode(row.node) == anchor.node {
				t.cursor = i
				t.offset = anchor.offset
				break
			}
		}
	}
	t.clampWindow()
}

// projectTree narrows roots to nodes whose label contains query at any depth,
// retaining ancestors for context. A matched node is the original pointer with
// its whole subtree; retained ancestors are shallow copies whose children are
// only matching branches. Query normalization happens once before the linear
// walk, and no ID or metadata participates in user-visible label search.
func projectTree(roots []*TreeNode, query string) []*TreeNode {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return roots
	}
	var project func(*TreeNode) *TreeNode
	project = func(node *TreeNode) *TreeNode {
		if strings.Contains(strings.ToLower(flattenLine(node.Label)), needle) {
			return node
		}
		children := make([]*TreeNode, 0, len(node.Children))
		for _, child := range node.Children {
			if kept := project(child); kept != nil {
				children = append(children, kept)
			}
		}
		if len(children) == 0 {
			return nil
		}
		return ProjectTreeNode(node, children)
	}
	visible := make([]*TreeNode, 0, len(roots))
	for _, root := range roots {
		if kept := project(root); kept != nil {
			visible = append(visible, kept)
		}
	}
	return visible
}

// recompute rolls every interior node's state up from its children across the
// whole forest.
func (t *Tree) recompute() {
	for _, r := range t.roots {
		rollup(r)
	}
	if len(t.visible) > 0 {
		for _, r := range t.visible {
			rollup(r)
		}
	}
}

// setSubtree sets state on node and every descendant, skipping Conflict leaves
// (a display-only conflict is not user-selectable).
func setSubtree(node *TreeNode, state TriState) {
	if node.isLeaf() {
		if node.State == Conflict {
			return
		}
		node.State = state
		return
	}
	for _, c := range node.Children {
		setSubtree(c, state)
	}
}

// rollup recomputes node.State from its children, bottom-up. A leaf keeps its
// own state (including a Conflict the source assigned). An interior node is
// Checked when every child is Checked, Unchecked when every child is
// Unchecked, and Partial otherwise - and any Conflict or Partial among the
// children forces at least Partial, so a conflict is never hidden behind a
// clean parent.
func rollup(node *TreeNode) TriState {
	if node.isLeaf() {
		return node.State
	}
	allChecked, allUnchecked := true, true
	forcePartial := false
	for _, c := range node.Children {
		cs := rollup(c)
		switch cs {
		case Checked:
			allUnchecked = false
		case Unchecked:
			allChecked = false
		default: // Partial or Conflict
			allChecked, allUnchecked = false, false
			forcePartial = true
		}
	}
	switch {
	case forcePartial:
		node.State = Partial
	case allChecked:
		node.State = Checked
	case allUnchecked:
		node.State = Unchecked
	default:
		node.State = Partial
	}
	return node.State
}

// walk visits node and every descendant in pre-order.
func walk(node *TreeNode, fn func(*TreeNode)) {
	fn(node)
	for _, c := range node.Children {
		walk(c, fn)
	}
}

// treeRow is one visible (expanded-into) row: the node and its depth.
type treeRow struct {
	node  *TreeNode
	depth int
}

// visibleRows flattens the forest into the ordered rows currently visible,
// descending into a node's children only when it is expanded. A node is
// expanded by default (absent from the expanded map); collapse records false.
// Shallow ancestors retained only to explain a text-search match open
// transiently even when their canonical node is collapsed, without changing
// that canonical expansion entry.
func (t Tree) visibleRows() []treeRow {
	var rows []treeRow
	var visit func(n *TreeNode, depth int)
	visit = func(n *TreeNode, depth int) {
		rows = append(rows, treeRow{node: n, depth: depth})
		if n.isLeaf() {
			return
		}
		if !t.visibleExpansion(n).expanded {
			return
		}
		for _, c := range n.Children {
			visit(c, depth+1)
		}
	}
	for _, r := range t.renderRoots() {
		visit(r, 0)
	}
	return rows
}

// treeScrollMargin keeps this many rows visible above and below the cursor when
// the forest is taller than the viewport, so the list scrolls before the cursor
// reaches the very edge (like a "scrolloff"). It shrinks automatically when the
// viewport is too short to honour it at both ends.
const treeScrollMargin = 5

// flattenLine collapses any newlines, carriage returns, and tabs in s into
// single spaces, so a value that spans multiple lines still renders as exactly
// one display line.
func flattenLine(s string) string {
	if !strings.ContainsAny(s, "\n\r\t") {
		return s
	}
	replacer := strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ", "\t", " ")
	return replacer.Replace(s)
}

// clampWindow scrolls the visible window so the cursor stays inside it, keeping a
// treeScrollMargin of context above and below where the forest allows.
func (t *Tree) clampWindow() {
	count := len(t.visibleRows())
	if t.cursor >= count {
		t.cursor = count - 1
	}
	if t.cursor < 0 {
		t.cursor = 0
	}
	if t.height < 1 || count == 0 {
		t.offset = 0
		return
	}
	margin := treeScrollMargin
	if half := t.height / 2; margin > half {
		margin = half
	}
	if t.cursor < t.offset+margin {
		t.offset = t.cursor - margin
	}
	if t.cursor+margin+1 > t.offset+t.height {
		t.offset = t.cursor - t.height + margin + 1
	}
	// Never scroll past the ends: the window is [0, count-height].
	maxOffset := count - t.height
	if maxOffset < 0 {
		maxOffset = 0
	}
	if t.offset > maxOffset {
		t.offset = maxOffset
	}
	if t.offset < 0 {
		t.offset = 0
	}
}

// View renders the tree. While loading it renders the spinner. Once loaded it
// renders exactly height rows of the visible window; an empty forest renders a
// muted placeholder; a load error renders it muted. Below TreeMinSize it
// renders a single truncation-safe line.
func (t Tree) View() string {
	styles := t.theme.Styles()
	if t.loading {
		return t.spinner.View()
	}
	if !TreeMinSize.fitsWithin(t.width, t.height) {
		return t.minFallback(styles)
	}
	if t.err != nil {
		return fitLine(styles.Muted, "scan failed", t.width)
	}
	rows := t.visibleRows()
	if len(rows) == 0 {
		empty := "no projects"
		if t.filter.Active() {
			empty = "no matching rows"
		}
		return fitLine(styles.Muted, empty, t.width)
	}
	end := t.offset + t.height
	if end > len(rows) {
		end = len(rows)
	}
	overflow := t.Overflow()
	out := make([]string, 0, t.height)
	for i := t.offset; i < end; i++ {
		edge := ""
		if i == t.offset && overflow.Top {
			edge = treeOverflowTopGlyph
		}
		if i == end-1 && overflow.Bottom {
			if edge == "" {
				edge = treeOverflowBottomGlyph
			} else {
				edge = treeOverflowBothGlyph
			}
		}
		out = append(out, t.renderRow(styles, rows[i], i == t.cursor, edge))
	}
	for len(out) < t.height {
		out = append(out, fitLine(styles.Base, "", t.width))
	}
	return joinLines(out)
}

// minFallback renders one truncation-safe line when the region is below the
// declared minimum: the cursor row's node if there is one, else a placeholder.
func (t Tree) minFallback(styles theme.Styles) string {
	if node, ok := t.CurrentNode(); ok {
		return styles.Muted.Render(truncateLine(flattenLine(node.Label), t.width))
	}
	if t.loading {
		return truncateLine(t.spinner.View(), t.width)
	}
	return truncateLine(styles.Muted.Render("no projects"), t.width)
}

// renderRow renders one visible row to exactly t.width cells: cursor, indent,
// expand glyph, tri-state box, the label, then the row's muted annotations.
// Segments are styled individually and the label is fit into the remaining
// budget so the row fills the width exactly; when the fixed prefix cannot fit
// (deep indent in a narrow region) it falls back to a single hard-truncated
// line so the width invariant always holds.
func (t Tree) renderRow(styles theme.Styles, row treeRow, active bool, edge string) string {
	node := row.node
	// A label MUST render as exactly one display line: a newline in the label
	// (e.g. a multi-line first-turn title) would otherwise split one row across
	// several lines, breaking the row background and the cursor/scroll math that
	// counts one row per line. The annotations are appended on the SAME line for
	// the same reason.
	label := flattenLine(node.Label)
	cur := treeRowLead(styles, active, edge)
	indent := spaces(row.depth * 2)
	expand := expandGlyph(node, t.visibleExpansion(node))
	box := stateBox(styles, node.State)
	annotation := rowAnnotation(node)

	prefixPlain := noCursorGlyph + indent + expand + " " + box + " "
	prefixWidth := lipgloss.Width(prefixPlain)
	labelBudget := t.width - prefixWidth
	if labelBudget < 1 {
		// Narrow region: styled segments would not fit, so hard-truncate the
		// whole plain row and style it as a unit - never overflow the width.
		plain := treeRowLeadPlain(active, edge) + indent + expand + " " + box + " " + label + annotation
		style := styles.Base
		if active {
			style = styles.Selected
		}
		return style.Render(truncateLine(plain, t.width))
	}

	labelStyle := styles.Base
	switch {
	case active:
		labelStyle = styles.Selected
	case node.State == Conflict:
		labelStyle = styles.Danger
	}
	return cur +
		styles.Base.Render(indent) +
		styles.Muted.Render(expand+" ") +
		box +
		styles.Base.Render(" ") +
		t.renderLabel(styles, labelStyle, label, annotation, labelBudget, active)
}

// minAnnotatedLabelCells is the smallest label remainder an annotation may
// leave. Below it the annotation is dropped so a narrow pane always shows the
// session title rather than only its trailing count.
const minAnnotatedLabelCells = 8

// renderLabel fits the label and its trailing annotation into exactly budget
// cells: the label first, the muted annotation right after it, and the leftover
// padded in the label's own style so an active row's highlight still spans the
// full width. An annotation that would squeeze the label below
// minAnnotatedLabelCells is dropped entirely rather than crowding out the title.
func (t Tree) renderLabel(styles theme.Styles, labelStyle lipgloss.Style, label, annotation string, budget int, active bool) string {
	annWidth := lipgloss.Width(annotation)
	if annWidth > 0 && budget-annWidth < minAnnotatedLabelCells {
		annotation, annWidth = "", 0
	}
	if annWidth == 0 {
		return fitLine(labelStyle, label, budget)
	}
	clipped := truncateLine(label, budget-annWidth)
	// On the active row the annotation shares the highlight so the row reads as
	// one selected band; elsewhere it stays muted next to the title.
	annStyle := styles.Muted
	if active {
		annStyle = labelStyle
	}
	pad := budget - lipgloss.Width(clipped) - annWidth
	return labelStyle.Render(clipped) + annStyle.Render(annotation) + labelStyle.Render(spaces(pad))
}

// rowAnnotation returns the muted text a node appends to its label: how many
// child sessions it groups (a parent session summarises its subagents instead
// of nesting them) and whether the local store already holds it. Both come from
// display-only Meta a TreeSource attached; a node carrying neither annotates
// nothing.
func rowAnnotation(node *TreeNode) string {
	var b strings.Builder
	if n := childCountOf(node); n > 0 {
		if n == 1 {
			b.WriteString(" + 1 child session")
		} else {
			fmt.Fprintf(&b, " + %d child sessions", n)
		}
	}
	if node.meta(MetaTracked) == MetaTrackedValue {
		b.WriteString("  tracked")
	}
	if node.meta(MetaIngested) == MetaIngestedValue {
		b.WriteString("  already imported")
	}
	return b.String()
}

// childCountOf reports the child-session count a node carries, or 0 when it
// carries none or an unreadable one (a malformed count annotates nothing rather
// than failing the render).
func childCountOf(node *TreeNode) int {
	raw := node.meta(MetaChildCount)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// treeRowLead renders the fixed two-cell cursor/overflow prefix. Overflow uses
// the second cell, so it never changes row width or steals the cursor.
func treeRowLead(styles theme.Styles, active bool, edge string) string {
	plain := treeRowLeadPlain(active, edge)
	if active {
		return styles.Selected.Render(plain)
	}
	if edge != "" {
		return styles.Muted.Render(plain)
	}
	return styles.Base.Render(plain)
}

func treeRowLeadPlain(active bool, edge string) string {
	first := " "
	if active {
		first = "▸"
	}
	second := " "
	if edge != "" {
		second = edge
	}
	return first + second
}

// expandGlyph returns the plain expand/collapse indicator for a node: a
// down-pointing triangle when expanded, a right-pointing one when collapsed,
// and a space for a leaf. Each is one cell wide.
func expandGlyph(node *TreeNode, expansion treeVisibleExpansion) string {
	if node.isLeaf() {
		return " "
	}
	if expansion.expanded {
		return treeExpandedGlyph
	}
	return treeCollapsedGlyph
}

// stateBox returns the tri-state checkbox for a node, styled distinctly per
// state so a Conflict reads differently from an Unchecked box at a glance.
func stateBox(styles theme.Styles, state TriState) string {
	switch state {
	case Checked:
		return styles.Success.Render(checkedBox)
	case Partial:
		return styles.Warning.Render(partialBox)
	case Conflict:
		return styles.Danger.Render(conflictBox)
	default:
		return styles.Muted.Render(uncheckedBox)
	}
}

// joinLines joins rendered rows with newlines without importing strings for a
// one-liner (kept local to the render helpers).
func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}

// Tree row glyph vocabulary. The boxes are all three cells wide so a column of
// mixed states stays aligned; the expand glyphs are one cell wide.
const (
	treeExpandedGlyph       = "▾"
	treeCollapsedGlyph      = "▸"
	treeOverflowTopGlyph    = "↑"
	treeOverflowBottomGlyph = "↓"
	treeOverflowBothGlyph   = "↕"
	partialBox              = "[~]"
	conflictBox             = "[!]"
)
