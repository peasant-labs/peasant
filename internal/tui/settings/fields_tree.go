package settings

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

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
	// affordanceRows is the number of status rows reserved by the last render.
	// It is cached so sizing never has to walk the full forest for counts.
	affordanceRows int
	mounted        bool
	// forestReady is true only after the mounted Tree accepts a successful load
	// result. Loading, stale, foreign, and failed messages must never turn the
	// current forest into a persisted selection.
	forestReady bool

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
	previewBody  func(theme.Theme) kit.BodyRenderer
	split        kit.PreviewSplit
	previewRatio float64

	// initializeSelection applies the Draft's saved/current selection when a
	// fresh forest loads. Generic Tree fixtures may instead supply authoritative
	// source states; the canonical kickstart/config registry enables this option.
	initializeSelection bool

	// full is the whole forest the source last loaded; view is the (possibly
	// narrowed) forest the tree currently renders, sharing full's leaf pointers.
	full []*kit.TreeNode
	view []*kit.TreeNode
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

// WithPreviewRatio sets the fraction of the tree/preview region assigned to the
// tree pane. PreviewSplit clamps the value to its safe range.
func WithPreviewRatio(ratio float64) TreeOption {
	return func(f *treeField) { f.previewRatio = ratio }
}

// WithDraftSelectionState makes the Draft baseline the saved tracked source and
// the Draft working value the current checkbox source on every fresh load. It is
// opt-in so a generic Tree can still load authoritative source states.
func WithDraftSelectionState() TreeOption {
	return func(f *treeField) { f.initializeSelection = true }
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
		if f.previewRatio > 0 {
			f.split = f.split.WithRatio(f.previewRatio)
		}
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
	f.forestReady = false
	var loadCmd tea.Cmd
	f.tree, loadCmd = f.tree.Load()
	return tea.Batch(loadCmd, f.tree.Init())
}

func (f *treeField) focus() tea.Cmd { f.focused = true; return f.tree.Focus() }
func (f *treeField) blur()          { f.focused = false; f.tree.Blur() }

func (f *treeField) setSize(w, h int) {
	f.width, f.height = w, h
	f.affordanceRows = clampAffordanceRows(treeAffordanceRows, h)
	f.applySize()
}

// applySize hands the tree the region left after the facet gutter. It is called
// on every render because the gutter's width depends on the loaded forest,
// which arrives after the first size.
func (f *treeField) applySize() {
	inner := f.width - f.gutterWidth()
	height := f.contentHeight()
	if f.hasPreview() {
		// The split owns the divide between the tree and the preview body, and
		// sizes the tree itself.
		f.split.SetSize(inner, height)
		return
	}
	f.tree.SetSize(inner, height)
}

// treeAffordanceRows are the maximum field-local status rows above the tree:
// current scope/search controls, tracked/imported definitions, and canonical
// selected/hidden counts. Very short regions retain at least one tree row and
// show as many status rows as fit. Wide regions combine the definitions and
// count so the adjacent preview keeps another useful row.
const treeAffordanceRows = 3

func clampAffordanceRows(rows, height int) int {
	if maximum := height - 1; rows > maximum {
		rows = maximum
	}
	if rows < 0 {
		return 0
	}
	if rows > treeAffordanceRows {
		return treeAffordanceRows
	}
	return rows
}

func (f *treeField) contentHeight() int {
	height := f.height - f.affordanceRows
	if height < 1 {
		return 1
	}
	return height
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
	if f.facetAvailable() {
		actions = append(actions, keymap.ActionFilter)
	}
	return actions
}

// facetAvailable keeps facet dispatch and its advertised key in lockstep. A
// facet has no effect before a successful non-empty load, while search text is
// being edited, or while the preview pane owns input.
func (f *treeField) facetAvailable() bool {
	if f.facetKey == "" || !f.forestReady || len(f.facetValues()) == 0 || f.tree.FilterState().Editing() {
		return false
	}
	return !f.hasPreview() || f.split.ActivePane() == kit.PaneLeft
}

// capturesPrintableInput is the Flow field-input contract: while the tree pane
// is editing a scoped query, printable q, b, ?, and similar characters are text
// before they are global shortcuts. Preview focus remains non-textual.
func (f *treeField) capturesPrintableInput() bool {
	if !f.focused || !f.tree.FilterState().Editing() {
		return false
	}
	return !f.hasPreview() || f.split.ActivePane() == kit.PaneLeft
}

func (f *treeField) sync(d *Draft) {}

// handle accepts focused key input and delegates non-key messages through the
// same owner-checking async capability. Presentations can route non-key work
// directly through asyncField; callers using the general Field seam remain
// isolated because foreign work is rejected before component update.
func (f *treeField) handle(d *Draft, msg tea.Msg) tea.Cmd {
	if _, ok := msg.(tea.KeyPressMsg); ok {
		return f.handleMessage(d, msg, true)
	}
	return f.handleAsync(d, msg)
}

// handleAsync accepts only result/tick messages owned by this field's Tree or
// PreviewSplit. A foreign owner is rejected before either component sees it,
// and Draft changes are derived only when this Tree accepts its own load result.
func (f *treeField) handleAsync(d *Draft, msg tea.Msg) tea.Cmd {
	owned := f.tree.OwnsAsync(msg)
	if f.hasPreview() {
		owned = f.split.OwnsAsync(msg)
	}
	if !owned {
		return nil
	}
	return f.handleMessage(d, msg, false)
}

var _ asyncField = (*treeField)(nil)

// handleMessage advances the owned component. When allowFacet is true it also
// handles the synchronous harness-view key before ordinary Tree dispatch.
func (f *treeField) handleMessage(d *Draft, msg tea.Msg, allowFacet bool) tea.Cmd {
	var cmd tea.Cmd
	wasLoading := f.tree.Loading()
	treeOwned := f.tree.OwnsAsync(msg)
	facetHandled := allowFacet && f.handleFacetKey(msg)
	if !facetHandled {
		if f.hasPreview() {
			// The split routes the message to the tree it holds by pointer, then
			// loads the body of whatever row the cursor ends on.
			f.split, cmd = f.split.Update(msg)
		} else {
			f.tree, cmd = f.tree.Update(msg)
		}
	} else if f.hasPreview() {
		// A facet can move the cursor onto a different row without routing through
		// PreviewSplit.Update, so explicitly refresh the body it now names.
		cmd = f.split.Load()
	}
	acceptedTreeLoad := treeOwned && wasLoading && !f.tree.Loading()
	if acceptedTreeLoad {
		if f.tree.Err() == nil {
			f.captureForest(d)
			f.forestReady = true
		} else {
			f.forestReady = false
		}
	}
	// The selection is re-derived after a facet change too, so what a commit
	// would persist is always read from the whole forest - never left behind at
	// whatever the last narrowed view happened to hold. A successfully-loaded
	// empty forest has no new selection evidence, so it preserves the Draft.
	if f.forestReady && len(f.full) > 0 && (allowFacet || acceptedTreeLoad) {
		f.acc.Set(d.Working(), FromTreeNodes(f.selectionRoots()))
	}
	return cmd
}

// handleFacetKey answers the filter key when a facet is configured, reporting
// whether it consumed the message.
func (f *treeField) handleFacetKey(msg tea.Msg) bool {
	if !f.facetAvailable() {
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

// captureForest records a successfully-loaded forest as the full one and
// re-applies the current facet to it. Its caller has already observed the live
// Tree transition out of loading with no error, so nil roots mean a successful
// empty scan rather than an absent result.
func (f *treeField) captureForest(d *Draft) {
	roots := f.tree.Roots()
	// A config that existed when the Draft opened has a real saved selection to
	// annotate as tracked. WithDraftSelectionState always makes this field the
	// sole owner of applying the current working selection, including a first-run
	// Draft whose default is mode:all and a registry rebuilt after user edits.
	baseline := f.acc.Get(d.Baseline())
	working := f.acc.Get(d.Working())
	if f.initializeSelection && d.expectedExists {
		ApplyTrackedSelection(roots, baseline.ToSelectionConfig(d.Baseline().Selection.AutoIngestNewBranches))
	}
	if f.initializeSelection {
		ApplyExistingSelection(roots, working.ToSelectionConfig(d.Working().Selection.AutoIngestNewBranches))
	}
	f.full = roots
	f.view = roots
	f.tree = f.tree.WithRoots(roots)
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
	value, projected := f.activeFacetValue()
	if projected {
		next = pruneForest(f.full, f.facetKey, value)
	}
	if sameNodes(next, f.tree.Roots()) {
		f.view = next
		return
	}
	f.view = next
	if projected {
		f.tree = f.tree.WithProjectedRoots(next)
	} else {
		f.tree = f.tree.WithUnprojectedRoots(next)
	}
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

func (f *treeField) render(_ *Draft, _ theme.Styles, width int) string {
	if width != f.width {
		f.width = width
	}
	selected := checkedLeafCount(f.full)
	hidden := f.hiddenSelectedCount()
	status := f.affordanceLines(width, selected, hidden)
	f.affordanceRows = clampAffordanceRows(len(status), f.height)
	f.applySize()
	status = status[:f.affordanceRows]
	body := f.treeView()
	gutter := f.gutterLines(f.contentHeight())
	if len(gutter) == 0 {
		return joinLines(append(status, strings.Split(body, "\n")...))
	}
	return joinLines(append(status, strings.Split(joinColumns(gutter, body), "\n")...))
}

// affordanceLines renders field-local state that must remain visible even when
// the shared footer is too narrow to carry every Tree action.
func (f *treeField) affordanceLines(width, selected, hidden int) []string {
	styles := f.th.Styles()
	state := f.tree.FilterState()
	km := keymap.Default()
	interaction := ""
	switch {
	case f.hasPreview() && f.split.ActivePane() == kit.PaneRight:
		focusLeft := actionKey(km, keymap.ActionFocusPaneLeft)
		clearFilter := actionHint(km, keymap.ActionClearFilter)
		if state.Editing() {
			interaction = fmt.Sprintf("preview focused; search %s: %s; %s returns to tree to keep or %s", state.Scope, state.Query, focusLeft, clearFilter)
		} else if state.Active() {
			interaction = fmt.Sprintf("preview focused; filter %s: %s; %s returns to tree, then %s", state.Scope, state.Query, focusLeft, clearFilter)
		} else {
			interaction = fmt.Sprintf("preview focused; %s returns to tree", focusLeft)
		}
	case state.Editing():
		interaction = fmt.Sprintf("search %s: %s    type to filter  %s  %s  %s", state.Scope, state.Query,
			actionHint(km, keymap.ActionDeleteFilter), actionHint(km, keymap.ActionKeepFilter), actionHint(km, keymap.ActionClearFilter))
	case state.Active():
		interaction = fmt.Sprintf("filter %s: %s    %s  %s", state.Scope, state.Query,
			actionHint(km, keymap.ActionSearchScope), actionHint(km, keymap.ActionClearFilter))
	default:
		interaction = f.scopeHint(state.Scope, km)
	}
	definitions := "tracked = included by previous saved selection; imported = already in local store"
	summary := fmt.Sprintf("selected sessions: %d; hidden by filters: %d", selected, hidden)
	compactSummary := fmt.Sprintf("selected %d; hidden by filters: %d", selected, hidden)
	if lipgloss.Width(definitions)+4+lipgloss.Width(compactSummary) <= width {
		return []string{
			styles.Muted.Render(clip(interaction, width)),
			styles.Muted.Render(clip(definitions+"    "+compactSummary, width)),
		}
	}
	return []string{
		styles.Muted.Render(clip(interaction, width)),
		styles.Muted.Render(clip(definitions, width)),
		styles.Muted.Render(clip(summary, width)),
	}
}

func (f *treeField) scopeHint(active kit.TreeScope, km keymap.Keymap) string {
	labels := make([]string, 0, len(kit.AllTreeScopes()))
	for _, scope := range kit.AllTreeScopes() {
		label := scope.String()
		if scope == active {
			label = "[" + label + "]"
		}
		labels = append(labels, label)
	}
	actions := actionSet(f.availableActions())
	hint := "scope: " + strings.Join(labels, "  ")
	if actions[keymap.ActionSearchScope] {
		hint += "    " + actionKey(km, keymap.ActionSearchScope) + ": search this scope"
	}
	if actions[keymap.ActionCollapse] {
		hint += "    " + actionHint(km, keymap.ActionCollapse)
	}
	if actions[keymap.ActionExpand] {
		hint += "  " + actionHint(km, keymap.ActionExpand)
	}
	if actions[keymap.ActionFocusPaneRight] {
		hint += "  " + actionHint(km, keymap.ActionFocusPaneRight)
	}
	return hint
}

func actionHint(km keymap.Keymap, action keymap.ActionID) string {
	entries := keymap.HelpEntries(km, availList{action})
	if len(entries) != 1 {
		return ""
	}
	return entries[0].Key + ": " + entries[0].Desc
}

func actionKey(km keymap.Keymap, action keymap.ActionID) string {
	entries := keymap.HelpEntries(km, availList{action})
	if len(entries) != 1 {
		return ""
	}
	return entries[0].Key
}

func actionSet(actions []keymap.ActionID) map[keymap.ActionID]bool {
	out := make(map[keymap.ActionID]bool, len(actions))
	for _, action := range actions {
		out[action] = true
	}
	return out
}

func checkedLeafCount(roots []*kit.TreeNode) int {
	count := 0
	for _, root := range roots {
		walkNodes(root, func(node *kit.TreeNode) {
			if len(node.Children) == 0 && node.State == kit.Checked {
				count++
			}
		})
	}
	return count
}

func (f *treeField) hiddenSelectedCount() int {
	visible := map[*kit.TreeNode]bool{}
	for _, root := range f.tree.VisibleRoots() {
		walkNodes(root, func(node *kit.TreeNode) {
			if len(node.Children) == 0 {
				visible[node] = true
			}
		})
	}
	hidden := 0
	for _, root := range f.full {
		walkNodes(root, func(node *kit.TreeNode) {
			if len(node.Children) == 0 && node.State == kit.Checked && !visible[node] {
				hidden++
			}
		})
	}
	return hidden
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
func (f *treeField) gutterLines(height int) []string {
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
	for len(lines) < height {
		lines = append(lines, fitCell(styles.Base, "", gw))
	}
	return lines[:height]
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

// Validate blocks commit until a successful scan has reached this mounted
// field, then blocks when the forest contains a display-only Conflict node and
// otherwise checks the derived selection. It inspects the WHOLE forest, so a
// conflict hidden by the current facet still blocks.
func (f *treeField) Validate(d *Draft) error {
	if !f.forestReady {
		if err := f.tree.Err(); err != nil {
			return fmt.Errorf(
				"transcript selection scan failed.\n"+
					"what: the selection tree could not load the current transcript forest.\n"+
					"why: the configured selection source returned: %v.\n"+
					"where: settings.treeField.Validate (field %q).\n"+
					"when: validating the final review before the atomic config commit.\n"+
					"means: the existing buffered selection is preserved and no configuration is written.\n"+
					"fix: resolve the reported scan problem, then restart this settings flow and wait for a successful scan.",
				err, f.key)
		}
		return fmt.Errorf(
			"transcript selection is not ready.\n"+
				"what: the selection tree has not received a successful transcript scan result.\n"+
				"why: the scan is still loading or its result has not reached the mounted field.\n"+
				"where: settings.treeField.Validate (field %q).\n"+
				"when: validating the final review before the atomic config commit.\n"+
				"means: the existing buffered selection is preserved and no configuration is written.\n"+
				"fix: wait for scanning to finish; if it does not, leave without saving and retry.",
			f.key)
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
	return kit.ProjectTreeNode(n, kept)
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
		if len(ac.Projects) != len(bc.Projects) {
			return false
		}
		for _, ap := range ac.Projects {
			found := false
			for _, bp := range bc.Projects {
				if ap.GitRemote == bp.GitRemote && ap.Name == bp.Name && sameSet(ap.Branches, bp.Branches) {
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
