package kit

import (
	"strings"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// PreviewSplitMinSize is the smallest region a PreviewSplit draws its two
// panes into: enough width for a minimal left list, the one-cell divider, and
// a couple of preview cells, and one content row. Below either dimension it
// renders a single truncation-safe line rather than two clipped panes.
var PreviewSplitMinSize = Size{Width: 10, Height: 1}

// defaultPreviewRatio is the fraction of the inner width the left (list) pane
// takes; the right (preview) pane takes the remainder minus the divider.
const defaultPreviewRatio = 0.4

// ContentSource loads the preview body for a highlighted item on demand. The
// width it is handed is the exact cell width the preview pane will render
// into, so a source can pre-wrap or budget its output; PreviewSplit still
// clips defensively. Content is called from a tea.Cmd off the UI goroutine,
// so an implementation may block on IO without freezing list input. A
// returned error is rendered as an actionable in-pane message (never a
// panic). This is the sole seam the kickstart session-contribution step binds
// its session-body loader to.
type ContentSource interface {
	// Content returns the preview body for id, sized for a width-cell pane,
	// or an error describing why it could not be loaded.
	Content(id string, width int) (string, error)
}

// BodyRenderer lays a loaded preview body out for a pane of width cells,
// returning the exact lines to display. It is the seam a caller uses when the
// body is richer than plain text, and it is called at RENDER time, against the
// pane's CURRENT width, for the same reason the default wrap is: a
// ContentSource is handed a width only as a hint, and on mount that hint
// predates the pane having been sized at all. Without a BodyRenderer the pane
// word-wraps the body itself.
type BodyRenderer func(body string, width int) string

// PreviewBody is a loaded preview that lays ITSELF out for a pane of width
// cells. It is what a preview carries when the thing being previewed has
// STRUCTURE a flat string would lose - a session transcript is a sequence of
// role-tagged turns, not one blob of prose - so the structure survives the
// asynchronous load and is turned into text only at draw time, against the
// width the pane actually has.
//
// Every returned line must be at most width display cells wide; the pane clips
// defensively but cannot re-wrap styled text without cutting an escape
// sequence. Render is called on every draw and every scroll, so an
// implementation is expected to cache its own work.
type PreviewBody interface {
	// Render lays the preview out for a pane of width cells.
	Render(width int) string
}

// BodySource loads a STRUCTURED preview for a highlighted item on demand: the
// alternative to [ContentSource] for a preview whose content is not a flat
// string. Mount one with [NewPreviewSplitWithBodies].
//
// It takes NO width, and that is the point. A ContentSource is handed a width
// as a hint and must be trusted not to lay anything out with it; a BodySource
// cannot lay anything out at load time because it is never told a column. The
// width-at-draw-time contract is therefore structural rather than a rule an
// implementation has to remember.
//
// Body is called from a tea.Cmd off the UI goroutine, so an implementation may
// block on IO without freezing list input. A returned error is rendered as an
// actionable in-pane message (never a panic).
type BodySource interface {
	// Body returns the structured preview for id, or an error describing why
	// it could not be loaded.
	Body(id string) (PreviewBody, error)
}

// textBody is the PreviewBody a flat [ContentSource] string becomes. It defers
// exactly what the pane always deferred: the BodyRenderer (or the default word
// wrap) runs at the pane's CURRENT width on every draw, not at the width the
// load was issued with.
type textBody struct {
	text   string
	render BodyRenderer
}

// Render lays the loaded string out for a pane of width cells.
func (b textBody) Render(width int) string {
	if width <= 0 {
		return b.text
	}
	if b.render != nil {
		return b.render(b.text, width)
	}
	// Wrap on word boundaries, breaking a word that is itself wider than the
	// pane, so a long token is folded rather than silently truncated.
	return ansi.Wrap(b.text, width, "")
}

var _ PreviewBody = textBody{}

// IdentifiedItem is the optional capability of a ListItem that carries a
// stable identity distinct from its display label. When a left-pane item
// implements it, PreviewSplit keys previews (and the stale-result guard) on
// ID() rather than the display text, so two rows that render the same label
// still preview independently. A plain StringItem does not implement it and
// falls back to its FilterValue.
type IdentifiedItem interface {
	// ID returns the stable identity used to request and tag a preview.
	ID() string
}

// LeftPane is the narrow highlight seam PreviewSplit binds preview loading to:
// any component that fills a bounded region, takes navigation input, and can
// name the currently-highlighted ID. It is deliberately small - no speculative
// generality - so the kickstart slice can supply its own session-contribution
// left surface, while [NewListLeftPane] wraps the kit [List] as the shipped
// default. PreviewSplit never assumes the left pane is a List.
type LeftPane interface {
	Focusable
	Sizeable
	// Availability reports the actions the pane dispatches in its current
	// state, so the split can advertise the ACTIVE pane's actions rather than
	// a fixed guess at them - which is what keeps the footer hint bar and the
	// help overlay honest as focus moves between the panes.
	keymap.Availability
	// Update advances the pane on a message and returns it as a LeftPane so
	// the split can hold the seam without knowing the concrete type.
	Update(msg tea.Msg) (LeftPane, tea.Cmd)
	// View renders the pane into its current size.
	View() string
	// HighlightedID reports the ID of the currently-highlighted entry, or
	// false when nothing is highlighted (e.g. an empty list).
	HighlightedID() (string, bool)
}

// ListLeftPane adapts the kit [List] to the [LeftPane] seam: it is the default
// left pane a PreviewSplit uses. The highlighted ID is the selected item's
// ID() when the item implements [IdentifiedItem], otherwise its FilterValue.
type ListLeftPane struct {
	list List
}

// NewListLeftPane wraps l as a LeftPane. The returned pointer satisfies
// LeftPane; its focus and size mutate the wrapped list in place.
func NewListLeftPane(l List) *ListLeftPane { return &ListLeftPane{list: l} }

// Focus gives the wrapped list keyboard focus.
func (p *ListLeftPane) Focus() tea.Cmd { return p.list.Focus() }

// Blur removes keyboard focus from the wrapped list.
func (p *ListLeftPane) Blur() { p.list.Blur() }

// Focused reports whether the wrapped list holds focus.
func (p *ListLeftPane) Focused() bool { return p.list.Focused() }

// SetSize sizes the wrapped list to the inner region.
func (p *ListLeftPane) SetSize(width, height int) { p.list.SetSize(width, height) }

// Update advances the wrapped list and returns the pane as a LeftPane.
func (p *ListLeftPane) Update(msg tea.Msg) (LeftPane, tea.Cmd) {
	var cmd tea.Cmd
	p.list, cmd = p.list.Update(msg)
	return p, cmd
}

// View renders the wrapped list.
func (p *ListLeftPane) View() string { return p.list.View() }

// AvailableActions reports the wrapped list's actions.
func (p *ListLeftPane) AvailableActions() []keymap.ActionID {
	return p.list.AvailableActions()
}

// HighlightedID returns the selected item's stable ID (its ID() when it
// implements IdentifiedItem, else its FilterValue), or false when empty.
func (p *ListLeftPane) HighlightedID() (string, bool) {
	item, ok := p.list.Selected()
	if !ok {
		return "", false
	}
	if ided, ok := item.(IdentifiedItem); ok {
		return ided.ID(), true
	}
	return item.FilterValue(), true
}

var (
	_ LeftPane  = (*ListLeftPane)(nil)
	_ Focusable = (*ListLeftPane)(nil)
	_ Sizeable  = (*ListLeftPane)(nil)
)

// TreeLeftPane adapts the kit [Tree] to the [LeftPane] seam, so a selection
// tree can drive a side preview of whatever row the cursor is on. It holds the
// tree by POINTER because the tree is an asynchronous surface a parent also
// owns (it loads, re-sources, and re-points the forest); every update writes
// back through that pointer, so the parent and the split always see one tree,
// never two diverging copies.
type TreeLeftPane struct {
	tree *Tree
}

// NewTreeLeftPane wraps t as a LeftPane. The caller keeps ownership of t.
func NewTreeLeftPane(t *Tree) *TreeLeftPane { return &TreeLeftPane{tree: t} }

// Focus gives the wrapped tree keyboard focus.
func (p *TreeLeftPane) Focus() tea.Cmd { return p.tree.Focus() }

// Blur removes keyboard focus from the wrapped tree.
func (p *TreeLeftPane) Blur() { p.tree.Blur() }

// Focused reports whether the wrapped tree holds focus.
func (p *TreeLeftPane) Focused() bool { return p.tree.Focused() }

// SetSize sizes the wrapped tree to the inner region.
func (p *TreeLeftPane) SetSize(width, height int) { p.tree.SetSize(width, height) }

// Update advances the wrapped tree, writing the result back through the pointer
// so the owner sees it too.
func (p *TreeLeftPane) Update(msg tea.Msg) (LeftPane, tea.Cmd) {
	next, cmd := p.tree.Update(msg)
	*p.tree = next
	return p, cmd
}

// View renders the wrapped tree.
func (p *TreeLeftPane) View() string { return p.tree.View() }

// AvailableActions reports the wrapped tree's actions for its current state
// (a still-loading forest dispatches almost nothing).
func (p *TreeLeftPane) AvailableActions() []keymap.ActionID {
	return p.tree.AvailableActions()
}

// HighlightedID returns the id of the node under the cursor, or false when the
// forest has no visible rows (it is still loading, or empty).
func (p *TreeLeftPane) HighlightedID() (string, bool) {
	node, ok := p.tree.CurrentNode()
	if !ok {
		return "", false
	}
	return node.ID, true
}

var (
	_ LeftPane  = (*TreeLeftPane)(nil)
	_ Focusable = (*TreeLeftPane)(nil)
	_ Sizeable  = (*TreeLeftPane)(nil)
)

// PaneFocus names which pane of a PreviewSplit currently receives input.
type PaneFocus int

const (
	// PaneLeft routes navigation to the left (list) pane; a highlight change
	// there triggers a preview load.
	PaneLeft PaneFocus = iota
	// PaneRight routes scroll input to the right (preview) viewport.
	PaneRight
)

// String returns a kebab-case name for f, or "unknown" for an out-of-range
// value, mirroring the kit's other Stringer conventions.
func (f PaneFocus) String() string {
	switch f {
	case PaneLeft:
		return "left"
	case PaneRight:
		return "right"
	default:
		return "unknown"
	}
}

// previewLoadedMsg is the result of one ContentSource.Content call, tagged
// with the sequence number of the load that requested it. PreviewSplit
// compares seq against its current load sequence on receipt and DROPS any
// result whose seq is stale - a late result for a since-de-highlighted item
// never overwrites the current preview. It is unexported: tests drive it by
// running the real load command the split emits, so the guard is exercised
// end-to-end rather than by hand-built messages.
type previewLoadedMsg struct {
	seq  int
	id   string
	body PreviewBody
	err  error
}

// PreviewSplit is an embedded (non-floating) two-pane surface: a navigable
// list on the left and a live, scrollable preview of the highlighted item on
// the right. Highlighting a new item asynchronously loads its preview via a
// tea.Cmd (list input is never blocked); a kit [Spinner] shows in the right
// pane while the load is in flight; a stale result for a since-de-highlighted
// item is dropped; a ContentSource error renders an actionable in-pane
// message. Focus toggles between the list and the preview viewport via the
// keymap next/prev-field actions. It is keyboard-only and composes the kit's
// theme, keymap, List, and Spinner rather than re-deriving any of them.
type PreviewSplit struct {
	theme  theme.Theme
	keymap keymap.Keymap
	left   LeftPane
	// source and bodies are the two mutually-exclusive load seams: a
	// ContentSource yields a flat string the pane lays out, a BodySource yields
	// a PreviewBody that lays itself out. Exactly one is set by the two
	// constructors.
	source       ContentSource
	bodies       BodySource
	bodyRenderer BodyRenderer
	spinner      Spinner

	ratio  float64
	width  int
	height int

	active PaneFocus

	// currentID is the highlighted ID the split is loading or showing a
	// preview for; seq tags the in-flight load for the stale guard.
	currentID string
	seq       int
	loading   bool
	body      PreviewBody
	loadErr   error
	scroll    int
}

// NewPreviewSplit builds a PreviewSplit over theme t with the given left pane
// seam and content source. Pass [NewListLeftPane] wrapping a kit List for the
// default list-left surface. Start the first preview with [PreviewSplit.Load].
func NewPreviewSplit(t theme.Theme, left LeftPane, source ContentSource) PreviewSplit {
	p := newPreviewSplit(t, left)
	p.source = source
	return p
}

// NewPreviewSplitWithBodies builds a PreviewSplit whose preview is STRUCTURED:
// the asynchronous load fetches a [PreviewBody] from source (width-independent
// by construction), and the pane renders that body at its CURRENT width on
// every draw. Use it when the previewed thing has shape a flat string would
// flatten away - a session transcript's role-tagged turns - and
// [NewPreviewSplit] when the body really is text. Everything else is identical:
// the same stale-result guard, spinner, error pane, focus, and scrolling.
func NewPreviewSplitWithBodies(t theme.Theme, left LeftPane, source BodySource) PreviewSplit {
	p := newPreviewSplit(t, left)
	p.bodies = source
	return p
}

// newPreviewSplit builds the parts both constructors share.
func newPreviewSplit(t theme.Theme, left LeftPane) PreviewSplit {
	return PreviewSplit{
		theme:   t,
		keymap:  keymap.Default(),
		left:    left,
		spinner: NewSpinner(t, "loading preview"),
		ratio:   defaultPreviewRatio,
		width:   PreviewSplitMinSize.Width,
		height:  PreviewSplitMinSize.Height,
		active:  PaneLeft,
	}
}

// WithRatio returns a copy of p whose left pane takes fraction r of the inner
// width (clamped to a sane band so neither pane ever collapses to nothing).
func (p PreviewSplit) WithRatio(r float64) PreviewSplit {
	if r < 0.15 {
		r = 0.15
	}
	if r > 0.85 {
		r = 0.85
	}
	p.ratio = r
	return p
}

// WithBodyRenderer returns a copy of p whose preview body is laid out by fn at
// the pane's current width on every render, instead of being word-wrapped. It
// applies to the [ContentSource] path only: a [BodySource] preview already
// lays itself out.
func (p PreviewSplit) WithBodyRenderer(fn BodyRenderer) PreviewSplit {
	p.bodyRenderer = fn
	return p
}

// Focus gives the split focus, routing input to the left pane, and focuses
// the left pane so its cursor is live.
func (p *PreviewSplit) Focus() tea.Cmd {
	p.active = PaneLeft
	return p.left.Focus()
}

// Blur removes focus from the split and its left pane.
func (p *PreviewSplit) Blur() { p.left.Blur() }

// Focused reports whether the left pane holds focus.
func (p *PreviewSplit) Focused() bool { return p.left.Focused() }

var _ Focusable = (*PreviewSplit)(nil)

// ActivePane reports which pane currently receives input.
func (p PreviewSplit) ActivePane() PaneFocus { return p.active }

// HighlightedID reports the ID currently highlighted in the left pane (the
// item the preview is bound to), or false when nothing is highlighted. It lets
// a parent (e.g. the kickstart session-contribution step) read the current
// selection without reaching through the left-pane seam.
func (p PreviewSplit) HighlightedID() (string, bool) { return p.left.HighlightedID() }

// Loading reports whether a preview load is currently in flight.
func (p PreviewSplit) Loading() bool { return p.loading }

// SetSize sets the inner region the split draws into and re-sizes both panes.
func (p *PreviewSplit) SetSize(width, height int) {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	p.width = width
	p.height = height
	p.resizePanes()
}

var _ Sizeable = (*PreviewSplit)(nil)

// resizePanes hands each pane its share of the current inner region.
func (p *PreviewSplit) resizePanes() {
	lw, rw := p.paneWidths()
	p.left.SetSize(lw, p.height)
	p.spinner.SetSize(rw, p.height)
}

// paneWidths computes the left and right pane content widths for the current
// inner width: left = ratio*width, one divider column, right = the remainder.
// Both are clamped so neither collapses below one cell when the split renders.
func (p PreviewSplit) paneWidths() (left, right int) {
	// The divider consumes one column between the panes.
	avail := p.width - 1
	if avail < 2 {
		if p.width <= 1 {
			return p.width, 0
		}
		return 1, p.width - 1
	}
	left = int(float64(avail)*p.ratio + 0.5)
	if left < 1 {
		left = 1
	}
	if left > avail-1 {
		left = avail - 1
	}
	right = avail - left
	return left, right
}

// AvailableActions reports the actions the split dispatches, in priority
// order: the tab/shift+tab focus toggle first, then everything the ACTIVE pane
// answers to.
func (p PreviewSplit) AvailableActions() []keymap.ActionID {
	return append([]keymap.ActionID{
		keymap.ActionNextField,
		keymap.ActionPrevField,
	}, p.PaneActions()...)
}

var _ keymap.Availability = PreviewSplit{}

// PaneActions reports what the split dispatches WITHOUT its own tab/shift+tab
// focus toggle: the two pane-focus keys, then the active pane's own actions -
// the left pane's when the list/tree has focus, the viewport's scroll set when
// the preview does.
//
// The toggle is split out because a PreviewSplit embedded in a form does not
// own tab: the settings Flow steps between FIELDS with it, and a split is one
// field. Such a host advertises PaneActions and leaves tab to the flow, while a
// standalone split advertises AvailableActions and keeps it.
// The active pane's own keys come FIRST and the two pane-focus keys after
// them, because a hint bar is truncated from the right: leading with pane focus
// would push the keys the user reaches for constantly off the end of the line.
func (p PreviewSplit) PaneActions() []keymap.ActionID {
	var actions []keymap.ActionID
	if p.active == PaneRight {
		actions = previewScrollActions()
	} else {
		actions = p.left.AvailableActions()
	}
	return append(actions, keymap.ActionFocusPaneLeft, keymap.ActionFocusPaneRight)
}

// focusToggleAvailability restricts Match to the two focus-toggle actions so a
// tab / shift+tab press is resolved before any navigation dispatch.
type focusToggleAvailability struct{}

func (focusToggleAvailability) AvailableActions() []keymap.ActionID {
	return []keymap.ActionID{keymap.ActionNextField, keymap.ActionPrevField}
}

// paneFocusAvailability restricts Match to the two pane-focus actions, so
// ctrl+h / ctrl+l are resolved before the active pane sees them as navigation.
type paneFocusAvailability struct{}

func (paneFocusAvailability) AvailableActions() []keymap.ActionID {
	return []keymap.ActionID{keymap.ActionFocusPaneLeft, keymap.ActionFocusPaneRight}
}

// previewScrollActions is the viewport's scroll set: the same movement
// vocabulary the tree uses, so the keys under the user's fingers do not change
// meaning when focus crosses the divider.
func previewScrollActions() []keymap.ActionID {
	return []keymap.ActionID{
		keymap.ActionUp,
		keymap.ActionDown,
		keymap.ActionPageUp,
		keymap.ActionPageDown,
		keymap.ActionTop,
		keymap.ActionBottom,
	}
}

// scrollAvailability restricts Match to the preview viewport's scroll actions.
type scrollAvailability struct{}

func (scrollAvailability) AvailableActions() []keymap.ActionID {
	return previewScrollActions()
}

// Load starts (or refreshes) the preview for the currently-highlighted item
// and returns the command that loads it plus the spinner tick. Call it once
// on mount; Update issues the same load automatically whenever the highlight
// changes. Returns nil when nothing is highlighted.
func (p *PreviewSplit) Load() tea.Cmd { return p.startLoad(true) }

// Update advances the split. It NEVER blocks list input on a preview load:
// key presses route to the active pane immediately, and the load runs in a
// command. A stale previewLoadedMsg (one tagged with a superseded sequence)
// is dropped. Spinner ticks advance the animation only while loading.
func (p PreviewSplit) Update(msg tea.Msg) (PreviewSplit, tea.Cmd) {
	switch m := msg.(type) {
	case previewLoadedMsg:
		if m.seq != p.seq {
			// Stale: a later highlight superseded this request. Drop it so a
			// late result never overwrites the current preview.
			return p, nil
		}
		p.loading = false
		p.body = m.body
		p.loadErr = m.err
		p.scroll = 0
		return p, nil

	case spinner.TickMsg:
		var cmds []tea.Cmd
		if p.loading {
			var cmd tea.Cmd
			p.spinner, cmd = p.spinner.Update(m)
			cmds = append(cmds, cmd)
		}
		// The left pane may be an asynchronous surface with a spinner of its own
		// (a tree still scanning). Each wrapped spinner rejects a tick that is not
		// its own, so forwarding advances only the one the tick belongs to.
		var leftCmd tea.Cmd
		p.left, leftCmd = p.left.Update(m)
		cmds = append(cmds, leftCmd)
		return p, tea.Batch(cmds...)

	case tea.KeyPressMsg:
		// Focus toggle takes precedence over navigation.
		if action, ok := keymap.Match(p.keymap, m, focusToggleAvailability{}); ok {
			switch action {
			case keymap.ActionNextField:
				p.setActive(PaneRight)
			case keymap.ActionPrevField:
				p.setActive(PaneLeft)
			}
			return p, nil
		}
		// Pane focus is resolved before the active pane sees the press, so
		// ctrl+l always means "focus the preview" rather than reaching the tree
		// as an expand.
		if action, ok := keymap.Match(p.keymap, m, paneFocusAvailability{}); ok {
			switch action {
			case keymap.ActionFocusPaneLeft:
				p.setActive(PaneLeft)
			case keymap.ActionFocusPaneRight:
				p.setActive(PaneRight)
			}
			return p, nil
		}
		if p.active == PaneRight {
			if action, ok := keymap.Match(p.keymap, m, scrollAvailability{}); ok {
				p.scrollBy(action)
			}
			return p, nil
		}
		// Left pane focus: route to the list, then load on highlight change.
		var cmd tea.Cmd
		p.left, cmd = p.left.Update(m)
		loadCmd := p.startLoad(false)
		return p, tea.Batch(cmd, loadCmd)

	default:
		// Anything else belongs to the left pane's own work - an asynchronous
		// pane's load result arrives this way - and it can change the highlight
		// (the first row of a just-loaded forest), so a preview load follows.
		var cmd tea.Cmd
		p.left, cmd = p.left.Update(msg)
		return p, tea.Batch(cmd, p.startLoad(false))
	}
}

// setActive switches which pane receives input, keeping the left pane's own
// focus state in sync so its cursor styling reflects whether it is active.
func (p *PreviewSplit) setActive(f PaneFocus) {
	p.active = f
	if f == PaneLeft {
		p.left.Focus()
	} else {
		p.left.Blur()
	}
}

// startLoad begins a preview load for the current highlight when it differs
// from the one already loaded/loading (or when force is set, e.g. the initial
// mount). It bumps the sequence so any in-flight result becomes stale, marks
// the split loading, and returns the load command batched with the spinner
// tick. When nothing is highlighted it clears the preview and returns nil.
func (p *PreviewSplit) startLoad(force bool) tea.Cmd {
	id, ok := p.left.HighlightedID()
	if !ok {
		p.currentID = ""
		p.loading = false
		p.body = nil
		p.loadErr = nil
		return nil
	}
	if !force && id == p.currentID {
		return nil
	}
	p.currentID = id
	p.seq++
	p.loading = true
	p.loadErr = nil
	p.scroll = 0
	seq := p.seq
	return tea.Batch(p.loadCmd(seq, id), p.spinner.Tick())
}

// loadCmd builds the command that fetches one preview off the UI goroutine,
// through whichever of the two seams this split was constructed with. A
// [BodySource] is asked for a structured body; a [ContentSource] is asked for a
// string, which becomes a [textBody] so both paths reach the pane as one type
// and are laid out at draw-time width alike.
func (p PreviewSplit) loadCmd(seq int, id string) tea.Cmd {
	if p.bodies != nil {
		src := p.bodies
		return func() tea.Msg {
			body, err := src.Body(id)
			return previewLoadedMsg{seq: seq, id: id, body: body, err: err}
		}
	}
	// The width handed to a ContentSource stays a HINT: layout happens in
	// textBody.Render at the pane's current width.
	_, rw := p.paneWidths()
	src := p.source
	render := p.bodyRenderer
	return func() tea.Msg {
		content, err := src.Content(id, rw)
		return previewLoadedMsg{seq: seq, id: id, body: textBody{text: content, render: render}, err: err}
	}
}

// scrollBy moves the preview viewport by the given navigation action, clamped
// to the content extent.
func (p *PreviewSplit) scrollBy(action keymap.ActionID) {
	_, rw := p.paneWidths()
	total := len(p.previewBodyLines(rw))
	page := p.height
	if page < 1 {
		page = 1
	}
	switch action {
	case keymap.ActionUp:
		p.scroll--
	case keymap.ActionDown:
		p.scroll++
	case keymap.ActionPageUp:
		p.scroll -= page
	case keymap.ActionPageDown:
		p.scroll += page
	case keymap.ActionTop:
		p.scroll = 0
	case keymap.ActionBottom:
		p.scroll = total
	}
	maxOffset := total - p.height
	if maxOffset < 0 {
		maxOffset = 0
	}
	if p.scroll > maxOffset {
		p.scroll = maxOffset
	}
	if p.scroll < 0 {
		p.scroll = 0
	}
}

// View renders the two panes side by side, separated by a one-cell divider.
// Below PreviewSplitMinSize it renders a single truncation-safe line rather
// than two clipped panes. Every rendered line is exactly p.width cells wide.
func (p PreviewSplit) View() string {
	styles := p.theme.Styles()
	if !PreviewSplitMinSize.fitsWithin(p.width, p.height) {
		label := p.currentID
		if label == "" {
			label = "preview"
		}
		if p.width <= 0 {
			return ""
		}
		return styles.Muted.Render(truncateLine(label, p.width))
	}

	lw, rw := p.paneWidths()
	leftLines := splitToHeight(p.left.View(), p.height)
	rightLines := p.rightLines(styles, rw)
	dividerLines := p.dividerLines(styles)

	rows := make([]string, 0, p.height)
	for i := 0; i < p.height; i++ {
		left := padLine(leftLines[i], lw)
		rows = append(rows, left+dividerLines[i]+rightLines[i])
	}
	return strings.Join(rows, "\n")
}

// The divider column doubles as the focus indicator. Its top cell points at
// whichever pane currently takes input, in the app's focus-ring color; the rest
// of the column is the usual muted rule. It costs no width - a split already
// spends one column on the divider - and it is the ONE thing on screen that
// says which side j/k will move, since neither pane changes shape when it
// loses focus.
const (
	dividerGlyph          = "│"
	focusLeftGlyph        = "<"
	focusRightGlyph       = ">"
	dividerFocusRowHeight = 1
)

// dividerLines renders the divider column: exactly p.height single-cell rows,
// the first of them the focus marker.
func (p PreviewSplit) dividerLines(styles theme.Styles) []string {
	focus := lipgloss.NewStyle().Foreground(p.theme.Color(p.theme.Palette.FocusRing))
	glyph := focusLeftGlyph
	if p.active == PaneRight {
		glyph = focusRightGlyph
	}
	out := make([]string, p.height)
	for i := range out {
		if i < dividerFocusRowHeight {
			out[i] = focus.Render(glyph)
			continue
		}
		out[i] = styles.Muted.Render(dividerGlyph)
	}
	return out
}

// rightLines returns exactly p.height lines, each exactly rw cells wide, for
// the preview pane: the spinner while loading, an actionable message on error,
// otherwise the scrolled content body.
func (p PreviewSplit) rightLines(styles theme.Styles, rw int) []string {
	if rw <= 0 {
		out := make([]string, p.height)
		for i := range out {
			out[i] = ""
		}
		return out
	}
	var body []string
	switch {
	case p.loading:
		body = []string{fitPlain(p.spinner.View(), rw)}
	case p.loadErr != nil:
		body = p.errorLines(styles, rw)
	default:
		body = p.previewBodyLines(rw)
	}
	out := make([]string, p.height)
	for i := 0; i < p.height; i++ {
		idx := i + p.scroll
		if idx >= 0 && idx < len(body) {
			out[i] = fitPlain(body[i+p.scroll], rw)
		} else {
			out[i] = fitLine(styles.Base, "", rw)
		}
	}
	return out
}

// errorLines renders an actionable in-pane failure message answering what
// went wrong, why, where, and how to recover, each clipped to rw.
func (p PreviewSplit) errorLines(styles theme.Styles, rw int) []string {
	where := p.currentID
	if where == "" {
		where = "unknown item"
	}
	return []string{
		styles.Danger.Render(truncateLine("preview unavailable", rw)),
		styles.Base.Render(truncateLine("why: "+p.loadErr.Error(), rw)),
		styles.Muted.Render(truncateLine("where: "+where, rw)),
		styles.Muted.Render(truncateLine("fix: re-select the item to retry", rw)),
	}
}

// previewBodyLines splits the loaded body into physical lines for the viewport
// (no styling; the pane styles them on render). Layout happens HERE, at render
// time, against the pane's CURRENT width - never at load time, where the source
// is handed a width only as a hint (a ContentSource) or no width at all (a
// BodySource), both of which predate the pane having been sized on mount.
// Scrolling and the scroll clamp both measure against this line count.
func (p PreviewSplit) previewBodyLines(rw int) []string {
	if p.body == nil {
		return []string{""}
	}
	return strings.Split(p.body.Render(rw), "\n")
}

// splitToHeight splits s into lines and returns exactly height of them,
// padding with empty lines when short and truncating the slice when long.
// Existing lines are returned verbatim (already styled/sized by the pane) so
// no escape sequence is cut; missing rows are empty strings the caller pads.
func splitToHeight(s string, height int) []string {
	src := strings.Split(s, "\n")
	out := make([]string, height)
	for i := 0; i < height; i++ {
		if i < len(src) {
			out[i] = src[i]
		} else {
			out[i] = ""
		}
	}
	return out
}

// fitPlain clips a plain (unstyled) line to exactly width cells: truncate then
// right-pad with spaces. Used for content the pane has not pre-styled.
func fitPlain(s string, width int) string {
	if width <= 0 {
		return ""
	}
	return padLine(truncateLine(s, width), width)
}
