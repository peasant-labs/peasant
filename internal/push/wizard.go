package push

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/schema"
)

// ---------------------------------------------------------------------------
// PushWizardSession - session with metadata and action
// ---------------------------------------------------------------------------

// PushWizardSession pairs a push session row with its metadata and action.
type PushWizardSession struct {
	Row    ingest.PushSessionRow
	Meta   *schema.UnifiedMetadata // loaded for redaction status; may be nil
	Action PushAction
	// Locked marks a session that the branch-aware selection withheld due to a
	// multi-project conflict (BranchMatchWithheldConflict). Locked sessions are
	// shown in the wizard as a conflict row, and they are non-selectable: the
	// selection tree never toggles them and SelectedSessionIDs never returns
	// them.
	Locked bool
}

// WizardCandidates partitions sessions into the wizard's display order: kept
// (unlocked, branch-selected) first, then withheld (Locked=true, branch
// conflict) so the user sees WHY a session is excluded rather than having it
// silently dropped. A nil selection keeps all sessions (none withheld), matching
// the "no selection => push everything otherwise eligible" rule.
//
// This is the wizard-side counterpart to the pipeline's ApplySelection: both
// consume the SAME command-prepared decisions, so the wizard's selectable set
// equals the pipeline's kept set by construction.
func WizardCandidates(sessions []ingest.PushSessionRow, sel *SessionSelection) []PushWizardSession {
	kept, withheld := ApplySelection(sessions, sel)
	out := make([]PushWizardSession, 0, len(kept)+len(withheld))
	for _, s := range kept {
		out = append(out, PushWizardSession{Row: s, Action: PushWithRedaction})
	}
	for _, s := range withheld {
		out = append(out, PushWizardSession{Row: s, Action: PushExclude, Locked: true})
	}
	return out
}

// RedactionState is the closed set of redaction states the wizard reports for
// the STORED copy of one session. It is a named enum rather than a bare string
// so a screen, a fixture, and a test all name the same value.
type RedactionState uint8

const (
	// RedactionStateUnknown is the zero value. The wizard reports it when the
	// session carries no readable metadata.
	RedactionStateUnknown RedactionState = iota
	// RedactionStateCurrent marks a stored copy redacted at the current rule
	// set, for the current content.
	RedactionStateCurrent
	// RedactionStateStale marks a stored copy redacted by an older rule set, or
	// before the content last changed.
	RedactionStateStale
	// RedactionStateRaw marks a stored copy that redaction never ran over.
	RedactionStateRaw
)

// IsValid reports whether s is one of the four known states.
func (s RedactionState) IsValid() bool {
	switch s {
	case RedactionStateUnknown, RedactionStateCurrent, RedactionStateStale, RedactionStateRaw:
		return true
	default:
		return false
	}
}

// String returns a stable, lower-case name for s, or "unknown" for an
// out-of-range value.
func (s RedactionState) String() string {
	switch s {
	case RedactionStateCurrent:
		return "current"
	case RedactionStateStale:
		return "stale"
	case RedactionStateRaw:
		return "raw"
	default:
		return "unknown"
	}
}

// RedactionState reports the state of the session's STORED copy.
func (s PushWizardSession) RedactionState() RedactionState {
	if s.Meta == nil {
		return RedactionStateUnknown
	}
	info := s.Meta.Redaction
	hash := s.Meta.ContentHash
	switch {
	case info.IsCurrent(hash):
		return RedactionStateCurrent
	case info.IsStale(hash):
		return RedactionStateStale
	case info.IsRaw():
		return RedactionStateRaw
	default:
		return RedactionStateUnknown
	}
}

// ---------------------------------------------------------------------------
// PushWizardModel - a four-page wizard composed from the TUI kit
// ---------------------------------------------------------------------------

// wizardPage identifies each page of the push wizard.
type wizardPage uint8

const (
	pageInitialConfirm wizardPage = iota
	pageSessionReview
	pageRedactionPreview
	pageFinalConfirm
	pageCount
)

// IsValid reports whether p names a page the wizard renders.
func (p wizardPage) IsValid() bool { return p < pageCount }

// String returns a stable, lower-case name for p, or "unknown" for an
// out-of-range value.
func (p wizardPage) String() string {
	switch p {
	case pageInitialConfirm:
		return "start"
	case pageSessionReview:
		return "select sessions"
	case pageRedactionPreview:
		return "what leaves your machine"
	case pageFinalConfirm:
		return "confirm push"
	default:
		return "unknown"
	}
}

// Page chrome the frame title carries. The wizard keeps all chrome
// lower-case, like every other kit surface.
const (
	wizardTitle          = "push to village"
	wizardCancelledText  = "push cancelled."
	unknownProjectLabel  = "(unknown project)"
	projectNodePrefix    = "project:"
	wizardSummaryRows    = 1
	wizardReceiptDetails = "sessions in this push"
)

// PushWizardModel is the top-level BubbleTea model for the push wizard. Every
// page composes kit components over one theme: a bordered [kit.Frame], a
// [kit.Panel] body, the [kit.PreviewSplit] session selector, and [kit.Confirm]
// for the two consent pages. The model owns no colors, no padding, and no
// background fill of its own.
type PushWizardModel struct {
	th       theme.Theme
	page     wizardPage
	sessions []PushWizardSession

	// tree is held by pointer because the split drives the SAME forest; both
	// must never work on diverging copies.
	tree   *kit.Tree
	leaves map[string]*kit.TreeNode
	split  kit.PreviewSplit

	confirm kit.Confirm
	overlay kit.Overlay
	helping bool

	noticeScroll int

	width     int
	height    int
	quitting  bool
	confirmed bool
}

// NewPushWizard creates a push wizard model over theme th with the given
// sessions. Every unlocked session starts selected for push, which is the same
// default the pre-kit wizard opened with.
//
// turns is the preview read: it returns the transcript of one session as the
// push will publish it. The selection page draws it beside the tree, loaded
// asynchronously per highlighted row.
func NewPushWizard(th theme.Theme, sessions []PushWizardSession, turns PublishedTurnsFunc) PushWizardModel {
	tree, leaves := newSelectionTree(th, sessions)
	m := PushWizardModel{
		th:       th,
		page:     pageInitialConfirm,
		sessions: sessions,
		tree:     tree,
		leaves:   leaves,
		confirm:  kit.NewConfirm(th, startPrompt(len(sessions))),
		overlay:  kit.NewOverlay(th),
	}
	m.split = kit.NewPreviewSplitWithBodies(th, kit.NewTreeLeftPane(m.tree),
		wizardPreviewSource(sessions, turns, th))
	m.confirm.Focus()
	m.tree.Focus()
	return m
}

// startPrompt is the first page's question. It names the number of sessions so
// the user reads the size of the action before answering it.
func startPrompt(count int) string {
	if count == 1 {
		return "push 1 session to the village?"
	}
	return fmt.Sprintf("push %d sessions to the village?", count)
}

// Confirmed returns true if the user confirmed the push.
func (m PushWizardModel) Confirmed() bool { return m.confirmed }

// Quitting returns true if the user cancelled.
func (m PushWizardModel) Quitting() bool { return m.quitting }

// SelectedSessionIDs returns the session IDs that the user approved for push.
// Locked (withheld branch-conflict) sessions are never returned.
func (m PushWizardModel) SelectedSessionIDs() []string {
	var ids []string
	for _, s := range m.sessions {
		if s.Locked {
			continue
		}
		if s.Action == PushWithRedaction {
			ids = append(ids, s.Row.SessionID)
		}
	}
	return ids
}

// selectedSessions returns the sessions the user approved, in display order.
func (m PushWizardModel) selectedSessions() []PushWizardSession {
	var out []PushWizardSession
	for _, s := range m.sessions {
		if s.Locked {
			continue
		}
		if s.Action == PushWithRedaction {
			out = append(out, s)
		}
	}
	return out
}

func (m PushWizardModel) Init() tea.Cmd { return nil }

// ---------------------------------------------------------------------------
// Selection forest
// ---------------------------------------------------------------------------

// staticTreeSource serves a forest the wizard already holds. The wizard reads
// its candidates before it mounts, so nothing here scans: the source exists so
// the tree keeps one source contract for every caller.
type staticTreeSource struct{ roots []*kit.TreeNode }

// Load returns the prepared forest. It never blocks and never fails.
func (s staticTreeSource) Load(context.Context) ([]*kit.TreeNode, error) { return s.roots, nil }

var _ kit.TreeSource = staticTreeSource{}

// newSelectionTree folds the candidates into a project -> session forest and
// returns the mounted tree plus a lookup from session ID to its leaf, which is
// how the wizard reads the user's selection back out.
func newSelectionTree(th theme.Theme, sessions []PushWizardSession) (*kit.Tree, map[string]*kit.TreeNode) {
	var roots []*kit.TreeNode
	byProject := make(map[string]*kit.TreeNode, len(sessions))
	leaves := make(map[string]*kit.TreeNode, len(sessions))
	for _, s := range sessions {
		project := s.Row.ProjectName
		if strings.TrimSpace(project) == "" {
			project = unknownProjectLabel
		}
		root, ok := byProject[project]
		if !ok {
			root = &kit.TreeNode{ID: projectNodePrefix + project, Label: project}
			byProject[project] = root
			roots = append(roots, root)
		}
		leaf := &kit.TreeNode{
			ID:    s.Row.SessionID,
			Label: sessionRowLabel(s),
			State: leafState(s),
		}
		root.Children = append(root.Children, leaf)
		leaves[s.Row.SessionID] = leaf
	}
	tree := kit.NewTree(th, staticTreeSource{roots: roots}).WithRoots(roots)
	return &tree, leaves
}

// leafState maps a candidate onto the tri-state the tree renders. A withheld
// session is a Conflict: the tree draws it distinctly and refuses to select it,
// which is exactly the "shown but non-selectable" rule the branch-aware
// selection needs.
func leafState(s PushWizardSession) kit.TriState {
	switch {
	case s.Locked:
		return kit.Conflict
	case s.Action == PushWithRedaction:
		return kit.Checked
	default:
		return kit.Unchecked
	}
}

// sessionRowLabel is one selection row: the session, the harness that recorded
// it, and when it started.
func sessionRowLabel(s PushWizardSession) string {
	return fmt.Sprintf("%s  %s  %s",
		shortSessionID(s.Row.SessionID), s.Row.ModelHarness, sessionStartText(s.Row))
}

// shortSessionID clips a session ID to a readable prefix.
func shortSessionID(id string) string {
	const keep = 10
	if len(id) > keep {
		return id[:keep] + ".."
	}
	return id
}

// sessionStartText formats a session's start time for a row or a receipt line.
func sessionStartText(row ingest.PushSessionRow) string {
	return time.UnixMilli(row.StartMs).Format("Jan 02 15:04")
}

// syncSelection copies the tree's checkboxes back onto the candidate actions.
// Locked sessions keep their withheld action: the tree cannot toggle a Conflict
// leaf, so nothing can move them into the push set.
func (m *PushWizardModel) syncSelection() {
	for i := range m.sessions {
		if m.sessions[i].Locked {
			continue
		}
		leaf, ok := m.leaves[m.sessions[i].Row.SessionID]
		if !ok {
			continue
		}
		if leaf.State == kit.Checked {
			m.sessions[i].Action = PushWithRedaction
			continue
		}
		m.sessions[i].Action = PushExclude
	}
}

// ---------------------------------------------------------------------------
// Preview pane
// ---------------------------------------------------------------------------

// projectLabelOf returns the project a candidate is grouped under.
func projectLabelOf(s PushWizardSession) string {
	if strings.TrimSpace(s.Row.ProjectName) == "" {
		return unknownProjectLabel
	}
	return s.Row.ProjectName
}

// ---------------------------------------------------------------------------
// Keys and availability
// ---------------------------------------------------------------------------

// actionKeymap returns the one production keymap, with quit narrowed to the
// interrupt key while the selection tree edits search text. A user typing "q"
// into the search box must get the letter; the interrupt must still quit.
func (m PushWizardModel) actionKeymap() keymap.Keymap {
	km := keymap.Default()
	if !m.filterEditing() {
		return km
	}
	if binding, ok := km[keymap.ActionQuit]; ok {
		binding.SetKeys(interruptKey)
		km[keymap.ActionQuit] = binding
	}
	return km
}

// interruptKey is the keystroke that always quits, including while a text
// field holds the printable keys.
const interruptKey = "ctrl+c"

// filterEditing reports whether the selection tree currently captures printable
// keys as search text.
func (m PushWizardModel) filterEditing() bool {
	return m.page == pageSessionReview && m.tree != nil && m.tree.FilterState().Editing()
}

// availability reports the actions the current page dispatches, in priority
// order. The component's own actions come FIRST so a component key (the search
// box's enter, for example) is never shadowed by page chrome. Dispatch, the
// footer, and the help overlay all read this one list.
func (m PushWizardModel) availability() keymap.Availability {
	return staticAvailability(m.availableActions())
}

func (m PushWizardModel) availableActions() []keymap.ActionID {
	var actions []keymap.ActionID
	switch m.page {
	case pageInitialConfirm, pageFinalConfirm:
		actions = append(actions, m.confirm.AvailableActions()...)
	case pageSessionReview:
		actions = append(actions, m.split.AvailableActions()...)
		actions = append(actions, keymap.ActionConfirm, keymap.ActionBack)
	case pageRedactionPreview:
		actions = append(actions,
			keymap.ActionUp, keymap.ActionDown,
			keymap.ActionPageUp, keymap.ActionPageDown,
			keymap.ActionTop, keymap.ActionBottom,
			keymap.ActionConfirm, keymap.ActionBack,
		)
	}
	actions = append(actions, keymap.ActionQuit, keymap.ActionHelp)
	return dropPrintable(m.actionKeymap(), dedupeActions(actions), m.filterEditing())
}

// helpAvailability is what the open help overlay dispatches: the help key
// again, or back. Both close it.
func helpAvailability() keymap.Availability {
	return staticAvailability([]keymap.ActionID{keymap.ActionHelp, keymap.ActionBack})
}

// staticAvailability is a fixed action list presented as a keymap.Availability.
type staticAvailability []keymap.ActionID

func (s staticAvailability) AvailableActions() []keymap.ActionID { return []keymap.ActionID(s) }

var _ keymap.Availability = staticAvailability(nil)

// dedupeActions removes repeats while keeping first-seen order, so an action a
// component and the page chrome both advertise is dispatched, hinted, and
// documented exactly once.
func dedupeActions(actions []keymap.ActionID) []keymap.ActionID {
	seen := make(map[keymap.ActionID]bool, len(actions))
	out := make([]keymap.ActionID, 0, len(actions))
	for _, action := range actions {
		if seen[action] {
			continue
		}
		seen[action] = true
		out = append(out, action)
	}
	return out
}

// dropPrintable removes every action whose key a focused text field receives as
// printable input. Dispatch, the footer, and the help overlay consume the
// returned set, so a printable binding can never shadow search text in one of
// them and not the others.
func dropPrintable(km keymap.Keymap, actions []keymap.ActionID, capturing bool) []keymap.ActionID {
	if !capturing {
		return actions
	}
	out := make([]keymap.ActionID, 0, len(actions))
	for _, action := range actions {
		if keymap.HasPrintableBinding(km, action) {
			continue
		}
		out = append(out, action)
	}
	return out
}

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------

func (m PushWizardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.setSize(msg.Width, msg.Height)
		return m, nil

	case kit.ConfirmResultMsg:
		return m.applyConfirmResult(msg)

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	default:
		if m.page == pageSessionReview && m.split.OwnsAsync(msg) {
			var cmd tea.Cmd
			m.split, cmd = m.split.Update(msg)
			return m, cmd
		}
	}
	return m, nil
}

// setSize records the terminal region and hands every child its share of it.
func (m *PushWizardModel) setSize(width, height int) {
	m.width, m.height = width, height
	m.overlay.SetSize(width, height)
	frame := m.frame()
	inner := frame.InnerWidth()
	body := frame.InnerHeight() - wizardSummaryRows
	if body < 1 {
		body = 1
	}
	m.split.SetSize(inner, body)
	m.confirm.SetSize(inner, kit.ConfirmMinSize.Height)
}

// handleKey resolves one key press against the current page.
func (m PushWizardModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	km := m.actionKeymap()
	if m.helping {
		if _, ok := keymap.Match(km, msg, helpAvailability()); ok {
			m.helping = false
		}
		return m, nil
	}
	action, ok := keymap.Match(km, msg, m.availability())
	if !ok {
		return m, nil
	}
	switch action {
	case keymap.ActionHelp:
		m.helping = true
		return m, nil
	case keymap.ActionQuit:
		m.quitting = true
		return m, tea.Quit
	}

	switch m.page {
	case pageInitialConfirm, pageFinalConfirm:
		var cmd tea.Cmd
		m.confirm, cmd = m.confirm.Update(msg)
		return m, cmd
	case pageSessionReview:
		return m.updateSelection(action, msg)
	case pageRedactionPreview:
		return m.updateNotice(action)
	}
	return m, nil
}

// updateSelection advances the selection page. Page chrome owns confirm and
// back; every other key belongs to the split.
func (m PushWizardModel) updateSelection(action keymap.ActionID, msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch action {
	case keymap.ActionConfirm:
		m.page = pageRedactionPreview
		m.noticeScroll = 0
		return m, nil
	case keymap.ActionBack:
		m.page = pageInitialConfirm
		m.confirm = kit.NewConfirm(m.th, startPrompt(len(m.sessions)))
		m.confirm.Focus()
		m.setSize(m.width, m.height)
		return m, nil
	}
	var cmd tea.Cmd
	m.split, cmd = m.split.Update(msg)
	m.syncSelection()
	return m, cmd
}

// updateNotice scrolls the consent page, or moves off it.
func (m PushWizardModel) updateNotice(action keymap.ActionID) (tea.Model, tea.Cmd) {
	page := m.noticeHeight()
	switch action {
	case keymap.ActionConfirm:
		if len(m.selectedSessions()) == 0 {
			// Nothing to confirm. The page keeps saying so rather than
			// advancing to an empty receipt.
			return m, nil
		}
		m.page = pageFinalConfirm
		m.confirm = kit.NewConfirm(m.th, finalPrompt(len(m.selectedSessions())))
		m.confirm.Focus()
		m.setSize(m.width, m.height)
		return m, nil
	case keymap.ActionBack:
		m.page = pageSessionReview
		return m, nil
	case keymap.ActionUp:
		m.noticeScroll--
	case keymap.ActionDown:
		m.noticeScroll++
	case keymap.ActionPageUp:
		m.noticeScroll -= page
	case keymap.ActionPageDown:
		m.noticeScroll += page
	case keymap.ActionTop:
		m.noticeScroll = 0
	case keymap.ActionBottom:
		m.noticeScroll = m.noticeLineCount()
	}
	m.clampNoticeScroll()
	return m, nil
}

// applyConfirmResult acts on the answer a kit.Confirm produced. The first page
// starts or cancels the wizard; the last page pushes or returns to the
// selection.
func (m PushWizardModel) applyConfirmResult(msg kit.ConfirmResultMsg) (tea.Model, tea.Cmd) {
	switch m.page {
	case pageInitialConfirm:
		if !msg.OK {
			m.quitting = true
			return m, tea.Quit
		}
		m.page = pageSessionReview
		m.tree.Focus()
		m.setSize(m.width, m.height)
		return m, m.split.Load()
	case pageFinalConfirm:
		if !msg.OK {
			m.page = pageSessionReview
			return m, nil
		}
		m.confirmed = true
		return m, tea.Quit
	}
	return m, nil
}

// finalPrompt is the last question the user answers before anything uploads.
func finalPrompt(count int) string {
	if count == 1 {
		return "push 1 session now?"
	}
	return fmt.Sprintf("push %d sessions now?", count)
}

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------

func (m PushWizardModel) View() tea.View {
	v := tea.NewView(m.viewString())
	v.AltScreen = true
	return v
}

// frame builds the bordered container for the current page. It is the ONE place
// the wizard's chrome height and width accounting lives.
func (m PushWizardModel) frame() kit.Frame {
	frame := kit.NewFrame(m.th).
		WithTitle(wizardTitle + " - " + m.page.String()).
		WithFooter(keymap.FooterView(m.th, m.actionKeymap(), m.availability()))
	frame.SetSize(m.width, m.height)
	return frame
}

func (m PushWizardModel) viewString() string {
	if m.quitting {
		panel := kit.NewPanel(m.th)
		panel.SetSize(m.width, 0)
		panel.Line(m.th.Styles().Muted, wizardCancelledText)
		return panel.View()
	}
	frame := m.frame()
	frame.SetContent(m.body(frame.InnerWidth(), frame.InnerHeight()))
	base := frame.View()
	if m.helping {
		layer := helpLayer{th: m.th, entries: keymap.HelpEntries(m.actionKeymap(), m.availability())}
		return m.overlay.Push(layer).View(base)
	}
	return base
}

// body renders the current page into the frame's inner region.
func (m PushWizardModel) body(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	switch m.page {
	case pageInitialConfirm:
		return m.startBody(width, height)
	case pageSessionReview:
		return m.selectionBody(width, height)
	case pageRedactionPreview:
		return m.noticeBody(width, height)
	case pageFinalConfirm:
		return m.receiptBody(width, height)
	}
	return ""
}

// startBody introduces the push and asks the first question.
func (m PushWizardModel) startBody(width, height int) string {
	styles := m.th.Styles()
	panel := kit.NewPanel(m.th)
	panel.SetSize(width, height)
	panel.Wrapped(styles.Base, fmt.Sprintf("%d session(s) are ready to push.", len(m.sessions)))
	panel.Blank()
	panel.Wrapped(styles.Muted, "the village keeps what you publish. you choose the sessions on the next page.")
	panel.Blank()
	panel.Rendered(m.confirm.View())
	return panel.View()
}

// selectionBody renders the selection count over the tree/preview split.
func (m PushWizardModel) selectionBody(width, height int) string {
	styles := m.th.Styles()
	panel := kit.NewPanel(m.th)
	panel.SetSize(width, height)
	panel.Line(styles.Muted, m.selectionSummary())
	panel.Rendered(m.split.View())
	return panel.View()
}

// selectionSummary is the one-line count over the selector. It reports the
// selected set, the whole candidate set, and how many sessions the branch-aware
// selection withheld.
func (m PushWizardModel) selectionSummary() string {
	withheld := 0
	for _, s := range m.sessions {
		if s.Locked {
			withheld++
		}
	}
	summary := fmt.Sprintf("selected %d of %d sessions", len(m.selectedSessions()), len(m.sessions))
	if withheld > 0 {
		summary += fmt.Sprintf("  -  %d withheld by a branch conflict", withheld)
	}
	return summary
}

// noticeHeight is the number of body rows the consent page scrolls by.
func (m PushWizardModel) noticeHeight() int {
	height := m.frame().InnerHeight()
	if height < 1 {
		return 1
	}
	return height
}

// noticeLineCount is how many lines the consent copy occupies at the current
// width.
func (m PushWizardModel) noticeLineCount() int {
	return len(strings.Split(m.noticePanel(m.frame().InnerWidth()).View(), "\n"))
}

// clampNoticeScroll keeps the scroll offset inside the content.
func (m *PushWizardModel) clampNoticeScroll() {
	max := m.noticeLineCount() - m.noticeHeight()
	if max < 0 {
		max = 0
	}
	if m.noticeScroll > max {
		m.noticeScroll = max
	}
	if m.noticeScroll < 0 {
		m.noticeScroll = 0
	}
}

// noticeBody renders the consent copy, windowed to the current scroll offset.
func (m PushWizardModel) noticeBody(width, height int) string {
	lines := strings.Split(m.noticePanel(width).View(), "\n")
	start := m.noticeScroll
	if start > len(lines)-1 {
		start = len(lines) - 1
	}
	if start < 0 {
		start = 0
	}
	end := start + height
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start:end], "\n")
}

// noticePanel builds the whole consent copy at width cells. It is what the user
// reads immediately before the push, so every claim on it is measured rather
// than asserted.
func (m PushWizardModel) noticePanel(width int) kit.Panel {
	styles := m.th.Styles()
	panel := kit.NewPanel(m.th)
	panel.SetSize(width, 0)

	selected := m.selectedSessions()
	if len(selected) == 0 {
		panel.Wrapped(styles.Base, "no sessions are selected for push.")
		panel.Blank()
		panel.Wrapped(styles.Muted, "press esc to go back and select sessions.")
		return panel
	}

	// WHAT THIS SCREEN SAYS ABOUT THE SESSIONS, and what it no longer says.
	//
	// It used to classify the selected sessions by the redaction record of their
	// STORED copy: how many had never been redacted, how many carried an older
	// rule set, how many were current. Every one of those counts was true and
	// none of them was about the push. A user read "N session(s) whose stored
	// copy has never been redacted" immediately before consenting to publish and
	// had no action to take on it, because what leaves the machine is redacted on
	// its own way out whatever the stored copy holds. The counts read as a
	// warning about the upload while describing a file on disk.
	//
	// The reassurance below is what the push actually does, and the pointer after
	// it is the one action that answers the question the counts provoked: look at
	// the transcript the push will send.
	panel.Wrapped(styles.Base, fmt.Sprintf(
		"peasant redacts these %d session(s) before it publishes them to the village.", len(selected)))
	panel.Blank()
	panel.Wrapped(styles.Muted,
		"the preview on the selection page shows each transcript as it will be published. go back with esc to review one.")

	// What actually happens on upload.
	//
	// This block has now been wrong in BOTH directions, which is why it is worth
	// the care. It first promised that "no raw data will leave your machine" - an
	// absolute guarantee, printed at the moment of consent, that the push path
	// never delivered. It was then corrected to say only metadata is redacted and
	// content is published as recorded, which was true until push began applying
	// the same content redaction `peasant redact` applies. Understating protection
	// is the safer error and is still an error: this is the screen a user reads
	// while deciding what to share, and a person who believes their content leaves
	// untouched will withhold sessions they could safely publish - or, worse,
	// conclude the control does nothing and stop reading it.
	//
	// What it must not become again is an absolute. Matching finds KNOWN patterns.
	//
	// WHAT REDACTION COVERS IS DERIVED, not written here.
	//
	// This block hand-listed "file paths, the host slug, the project name, the git
	// remote". Measured at standard, the only level a user can select: the project
	// name, the host slug and the git remote are NOT rewritten. The git rules are
	// gated to maximum and maximum is refused, so they cannot fire on any run a
	// user can perform - and the screen promised them at the moment of consent.
	// The list survived a sweep that corrected the sentence about CONTENT beside
	// it and inherited the fields unchecked.
	//
	// The fields now come from running the real redactor, so a claim that stops
	// being true disappears instead of being carried forward.
	covered, coverErr := redactedMetadataFields(config.RecommendedRedactionLevel)
	panel.Blank()
	switch {
	case coverErr != nil || len(covered) == 0:
		// Say nothing rather than guess. A screen that cannot measure what it
		// covers has no business naming fields.
		panel.Wrapped(styles.Base, "before upload, peasant redacts at your configured level, never below standard.")
	default:
		panel.Wrapped(styles.Base, "before upload, peasant redacts at your configured level, never below standard:")
		panel.Wrapped(styles.Base, "metadata - "+joinFieldLabels(covered)+" - and conversation content, including tool arguments and tool results.")
	}
	panel.Blank()
	// THE HEDGE IS THE SHARED CONSTANT, CONSUMED VERBATIM.
	//
	// This screen hand-wrote its own wording of a sentence that exists once, in
	// config.RedactionScopeSentence, which every other surface consumes - the
	// onboarding screen, every push record, both sync refusals and the generated
	// web policy. Rewording it here only ever produced a third phrasing: an earlier
	// edit claimed to match the canonical one and did not, and dropped its scope
	// clause ("in both metadata and transcript content") while doing so.
	//
	// Consuming the constant is what ends that. The panel wraps it at the pane
	// width, so the line count the scroll math uses equals the line count the
	// screen draws.
	panel.Wrapped(styles.Warning, config.RedactionScopeSentence())
	panel.Blank()
	panel.Wrapped(styles.Warning, "source code is published with matched tokens replaced, so a published transcript can differ from what you see locally. read what you share.")
	panel.Blank()
	panel.Wrapped(styles.Base, "if a session holds something you do not want published, deselect it: press esc to go back, then space to toggle it off.")
	return panel
}

// receiptBody renders the last page: a short summary, then the details it
// summarises, then the question.
func (m PushWizardModel) receiptBody(width, height int) string {
	styles := m.th.Styles()
	selected := m.selectedSessions()
	panel := kit.NewPanel(m.th)
	// The panel measures its own lines, so the receipt can count the rows it has
	// already used before it decides how many details fit. The frame pads the
	// body to the page height.
	panel.SetSize(width, 0)
	// The receipt counts the push, and it counts nothing else.
	//
	// It used to open "N session(s) leave this machine." and follow with "M of N
	// candidate sessions stay on it." A push copies; it moves nothing and it
	// deletes nothing. Read at the moment of confirmation, that pair says the
	// selected sessions go away and the rest are what remain - which is what a
	// maintainer read on a real store. The lines below name the action, name the
	// sessions the action skips, and say outright that the machine keeps
	// everything.
	panel.Wrapped(styles.Header, fmt.Sprintf("push %d session(s) to the village.", len(selected)))
	if skipped := len(m.sessions) - len(selected); skipped > 0 {
		panel.Wrapped(styles.Muted, fmt.Sprintf("%d session(s) are not selected and are not pushed.", skipped))
	}
	panel.Wrapped(styles.Muted, "nothing is removed from this machine.")
	panel.Blank()
	panel.Line(styles.Header, wizardReceiptDetails)

	// The receipt lists what it can and says how much it left out, rather than
	// running past the bottom of the box where the reader cannot see it.
	rows := height - panel.LineCount() - kit.ConfirmMinSize.Height - 1
	if rows < 0 {
		rows = 0
	}
	shown := len(selected)
	if shown > rows {
		shown = rows
		if shown > 0 {
			shown--
		}
	}
	for _, s := range selected[:shown] {
		panel.Line(styles.Base, fmt.Sprintf("  %s  %s  %s  %s",
			shortSessionID(s.Row.SessionID), s.Row.ModelHarness, projectLabelOf(s), sessionStartText(s.Row)))
	}
	if remaining := len(selected) - shown; remaining > 0 {
		panel.Line(styles.Muted, fmt.Sprintf("  and %d more.", remaining))
	}
	panel.Blank()
	panel.Rendered(m.confirm.View())
	return panel.View()
}

// ---------------------------------------------------------------------------
// Help overlay
// ---------------------------------------------------------------------------

// helpLayer renders the full keybinding list for the current page as a raised
// card. Its rows come from keymap.HelpEntries over the SAME availability the
// dispatch and the footer use, so the card cannot advertise a key the page
// cannot dispatch.
type helpLayer struct {
	th      theme.Theme
	entries []keymap.HelpEntry
}

var _ kit.OverlayLayer = helpLayer{}

// helpPanelMargin is the trailing space the card keeps past its widest row.
const helpPanelMargin = 2

func (l helpLayer) View() string {
	styles := l.th.Styles()
	var nav, act []keymap.HelpEntry
	for _, entry := range l.entries {
		if entry.Action.IsNavigation() {
			nav = append(nav, entry)
			continue
		}
		act = append(act, entry)
	}

	type row struct {
		text   string
		header bool
	}
	rows := make([]row, 0, len(l.entries)+6)
	rows = append(rows, row{text: "keyboard shortcuts", header: true})
	appendCategory := func(name string, entries []keymap.HelpEntry) {
		if len(entries) == 0 {
			return
		}
		rows = append(rows, row{})
		rows = append(rows, row{text: "  " + name, header: true})
		for _, entry := range entries {
			rows = append(rows, row{text: fmt.Sprintf("    %-12s %s", entry.Key, entry.Desc)})
		}
	}
	appendCategory("navigation", nav)
	appendCategory("actions", act)
	rows = append(rows, row{})
	rows = append(rows, row{text: "  press ? or esc to close"})

	widest := 0
	for _, r := range rows {
		if len(r.text) > widest {
			widest = len(r.text)
		}
	}
	panel := kit.NewPanel(l.th).WithBackground(l.th.Palette.Surface)
	panel.SetSize(widest+helpPanelMargin, 0)
	for _, r := range rows {
		style := styles.Surface
		if r.header {
			style = styles.Header
		}
		panel.Line(style, r.text)
	}
	return panel.View()
}
