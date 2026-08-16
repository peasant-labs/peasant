package settings

import (
	"fmt"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// Flow presents a [Registry] as a sequence of steps over a [Draft] and commits
// ONCE, atomically, at a final receipt step. Its ratified semantics:
//
//   - esc ALWAYS prompts a confirm-exit modal, regardless of whether the draft
//     is dirty; a confirmed exit writes NOTHING.
//   - Navigation is free: next/prev move between visible steps with all state
//     retained, and a step's conditional visibility is re-evaluated on
//     (re-)entry.
//   - The receipt-step confirm is the SINGLE commit point - there is no
//     mid-flow save.
//   - A section or field hidden by a changed earlier answer has its edits
//     DROPPED before the receipt, and the receipt reflects that.
//   - A field whose Validate fails (e.g. an unresolved selection Conflict)
//     blocks the receipt commit fail-closed with its actionable error.
//
// Step navigation is bound to next-field/prev-field (Tab / Shift+Tab) so a
// step's own field keeps up/down/left/right/enter for its control; a step
// forwards those to its focused interactive field.
type Flow struct {
	th    theme.Theme
	reg   Registry
	draft *Draft

	consent ConsentSummaryFunc

	steps      []Section // current visible steps, recomputed on navigation
	cur        int       // 0..len(steps); == len(steps) is the receipt step
	focusField int       // index of the focused field within the current step

	overlay        kit.Overlay
	exitConfirm    kit.Confirm
	confirmingExit bool

	noProjectsConfirm    kit.Confirm
	confirmingNoProjects bool
	commitGate           CommitGateEvaluator
	helping              bool

	committed bool
	exited    bool
	err       error

	width, height int
	viewOffset    int
}

// FlowOption configures optional presentation-owned behavior without changing
// existing settings flows, Registry fields, or their persistence contract.
type FlowOption func(*Flow)

// WithConsentSummary installs the final consent summary rendered by the guided
// presentation. The provider reads a typed, read-only context at receipt render
// time, after hidden edits have been dropped, so neither visibility nor values
// can drift from what a confirm will commit. Dense settings presentations do
// not use this option.
func WithConsentSummary(provider ConsentSummaryFunc) FlowOption {
	return func(f *Flow) { f.consent = provider }
}

// exitPrompt is the confirm-exit modal's text.
const exitPrompt = "leave settings without saving?"

// NewFlow mounts every field in reg over theme t and opens the flow on the
// first visible step of draft d.
func NewFlow(t theme.Theme, reg Registry, d *Draft, opts ...FlowOption) Flow {
	for _, s := range reg.Sections {
		for _, fld := range s.Fields {
			fld.mount(t)
		}
	}
	f := Flow{th: t, reg: reg, draft: d, exitConfirm: kit.NewConfirm(t, exitPrompt), overlay: kit.NewOverlay(t)}
	for _, opt := range opts {
		if opt != nil {
			opt(&f)
		}
	}
	f.steps = reg.visibleSections(d)
	f.enterStep()
	return f
}

// Init returns the batched startup commands for every field's asynchronous
// work (the selection tree's scan).
func (f Flow) Init() tea.Cmd {
	var cmds []tea.Cmd
	for _, s := range f.reg.Sections {
		for _, fld := range s.Fields {
			if c := fld.initCmd(); c != nil {
				cmds = append(cmds, c)
			}
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// Committed reports whether the flow committed the draft.
func (f Flow) Committed() bool { return f.committed }

// Exited reports whether the user confirmed a no-save exit.
func (f Flow) Exited() bool { return f.exited }

// Confirming reports whether the exit-confirm modal is currently shown.
func (f Flow) Confirming() bool { return f.confirmingExit }

// ConfirmingNoProjects reports whether the dedicated empty-selection save
// confirmation is shown. It is separate from the no-save exit modal.
func (f Flow) ConfirmingNoProjects() bool { return f.confirmingNoProjects }

// Helping reports whether the keybinding help overlay is currently shown.
func (f Flow) Helping() bool { return f.helping }

// OnReceipt reports whether the flow is on the final receipt step.
func (f Flow) OnReceipt() bool { return f.cur >= len(f.steps) }

// Step reports the current step index (len(steps) is the receipt).
func (f Flow) Step() int { return f.cur }

// Err reports the last validation or commit error shown to the user, if any.
func (f Flow) Err() error { return f.err }

// CurrentSectionKey reports the key of the current step's section, or "" on the
// receipt.
func (f Flow) CurrentSectionKey() string {
	if f.OnReceipt() {
		return ""
	}
	return f.steps[f.cur].Key
}

// OpenSection moves an already-mounted flow to the visible section identified
// by key. It is the narrow transition seam used when kickstart authenticates
// during visibility guidance and reveals a section on the retained registry.
// It does not edit the draft, commit, or alter section visibility.
func (f *Flow) OpenSection(key string) error {
	f.recompute()
	for index := range f.steps {
		if f.steps[index].Key != key {
			continue
		}
		f.cur = index
		f.err = nil
		f.enterStep()
		return nil
	}
	return fmt.Errorf(
		"open guided settings section %q: section is not visible.\n"+
			"what: the retained flow could not open the requested guided section.\n"+
			"why: the canonical registry has no currently visible section with that key.\n"+
			"where: settings.Flow.OpenSection after refreshing guided visibility.\n"+
			"when: restoring the user's position without committing or discarding the draft.\n"+
			"means: the draft is still buffered and no setting was written.\n"+
			"fix: update the registry's runtime visibility state and retry.",
		key)
}

// ResumeNextField completes the same typed next-field transition Flow handles
// for the canonical keymap. A parent presentation uses it after an intentional
// modal detour so it can resume navigation without replaying a terminal key or
// defining a second dispatch path.
func (f *Flow) ResumeNextField() {
	f.advance()
}

// SetSize records the outer render region.
func (f *Flow) SetSize(width, height int) {
	f.width, f.height = width, height
	f.overlay.SetSize(width, height)
	f.viewOffset = 0
}

// enterStep re-syncs the current step's fields from the draft and focuses its
// first interactive field.
func (f *Flow) enterStep() {
	f.viewOffset = 0
	if f.OnReceipt() {
		return
	}
	f.focusField = -1
	for i, fld := range f.steps[f.cur].Fields {
		fld.sync(f.draft)
		fld.blur()
		if f.focusField == -1 && interactive(fld) {
			f.focusField = i
		}
	}
	if f.focusField >= 0 {
		f.steps[f.cur].Fields[f.focusField].focus()
	}
}

// recompute re-derives the visible steps from the current draft (conditional
// re-evaluation) and clamps the cursor.
func (f *Flow) recompute() {
	onReceipt := f.OnReceipt()
	currentKey := f.CurrentSectionKey()
	currentIndex := f.cur
	f.steps = f.reg.visibleSections(f.draft)
	if onReceipt {
		f.cur = len(f.steps)
		return
	}
	for index := range f.steps {
		if f.steps[index].Key == currentKey {
			f.cur = index
			return
		}
	}
	f.cur = currentIndex
	if f.cur >= len(f.steps) {
		f.cur = len(f.steps) - 1
	}
	if f.cur < 0 {
		f.cur = 0
	}
}

// advance moves toward the receipt (or, on a real step, into it), re-evaluating
// conditional visibility first.
func (f *Flow) advance() {
	f.err = nil
	f.recompute()
	if f.cur < len(f.steps) {
		f.cur++
	}
	if f.OnReceipt() {
		// Reaching the receipt applies the Registry's canonical hidden-field
		// convergence so its summary reflects exactly what will be committed.
		f.reg.dropHiddenEdits(f.draft)
	}
	f.enterStep()
}

// retreat moves to the previous step, retaining all state.
func (f *Flow) retreat() {
	f.err = nil
	f.recompute()
	if f.cur > 0 {
		f.cur--
	}
	f.enterStep()
}

// commit drops hidden edits, validates every visible field, evaluates the
// optional save gate, then performs the single atomic commit. Any failure sets
// err and writes nothing.
func (f *Flow) commit() tea.Cmd {
	f.reg.dropHiddenEdits(f.draft)
	if err := f.reg.validateVisible(f.draft); err != nil {
		f.err = err
		return nil
	}
	if f.commitGate != nil {
		switch gate := f.commitGate(f.draft.Working().Selection); gate {
		case CommitGateNone:
			// Continue through the one existing Draft.Commit path.
		case CommitGateConfirmNoProjects:
			f.confirmingNoProjects = true
			f.noProjectsConfirm = kit.NewConfirm(f.th, noProjectsConfirmationQuestion)
			f.noProjectsConfirm.Focus()
			return nil
		default:
			f.err = fmt.Errorf(
				"save settings: the commit gate returned unknown decision %d.\n"+
					"what: the save safety check could not choose a supported action.\n"+
					"where: settings.Flow.commit.\n"+
					"means: nothing was written and ingest did not start.\n"+
					"fix: update Peasant and run kickstart again.", gate)
			return nil
		}
	}
	return f.commitDraft()
}

func (f *Flow) commitDraft() tea.Cmd {
	if err := f.draft.Commit(); err != nil {
		f.err = err
		return nil
	}
	f.committed = true
	return tea.Quit
}

// Update dispatches a message. Component-owned asynchronous work reaches its
// mounted field before modal key handling. Save and exit confirmations then own
// key input; printable editor input precedes global shortcuts.
func (f Flow) Update(msg tea.Msg) (Flow, tea.Cmd) {
	if f.committed || f.exited {
		return f, nil
	}
	keyMsg, isKey := msg.(tea.KeyPressMsg)
	if !isKey {
		// Async results belong to mounted fields, not the current step or modal.
		return f.forwardAsync(msg)
	}
	if f.confirmingNoProjects {
		return f.updateNoProjectsConfirm(msg)
	}
	if f.confirmingExit {
		return f.updateExitConfirm(msg)
	}
	if f.helping {
		// While the help overlay is up, ? or esc closes it and every other key
		// is swallowed so it cannot leak into the underlying step.
		if action, ok := keymap.Match(f.actionKeymap(), keyMsg, helpAvailability{}); ok {
			switch action {
			case keymap.ActionHelp, keymap.ActionBack:
				f.helping = false
			}
		}
		return f, nil
	}
	if keyMsg.Text != "" && f.focusedFieldCapturesPrintableInput() {
		// Text is Bubble Tea's typed signal that this is printable input rather
		// than a lifecycle/control key. Let the focused editor consume it before
		// global bindings such as q (quit), b (back), and ? (help).
		return f.forwardToFields(msg)
	}
	action, ok := keymap.Match(f.actionKeymap(), keyMsg, f.dispatchAvailability(keyMsg))
	if ok {
		switch action {
		case keymap.ActionHelp:
			f.helping = true
			return f, nil
		case keymap.ActionBack, keymap.ActionQuit:
			// esc and q both leave settings, and both prompt the same
			// confirm-exit modal first; a confirmed exit writes nothing.
			f.confirmingExit = true
			f.exitConfirm = kit.NewConfirm(f.th, exitPrompt)
			f.exitConfirm.Focus()
			return f, nil
		case keymap.ActionNextField:
			f.ResumeNextField()
			return f, nil
		case keymap.ActionPrevField:
			f.retreat()
			return f, nil
		case keymap.ActionConfirm:
			if f.OnReceipt() {
				cmd := f.commit()
				return f, cmd
			}
		case keymap.ActionPageUp, keymap.ActionPageDown, keymap.ActionTop, keymap.ActionBottom:
			if !f.focusedFieldOwnsViewport() {
				f.scrollViewport(action)
				return f, nil
			}
		}
	}
	// Not a flow-level action (or ActionConfirm on a non-receipt step): forward
	// to the focused field.
	return f.forwardToFields(msg)
}

// updateExitConfirm drives the exit-confirm modal.
func (f Flow) updateExitConfirm(msg tea.Msg) (Flow, tea.Cmd) {
	var cmd tea.Cmd
	f.exitConfirm, cmd = f.exitConfirm.Update(msg)
	if cmd != nil {
		if res, ok := runResult(cmd).(kit.ConfirmResultMsg); ok {
			f.confirmingExit = false
			if res.OK {
				// Confirmed exit writes NOTHING.
				f.exited = true
				return f, tea.Quit
			}
			return f, nil
		}
	}
	return f, cmd
}

func (f Flow) focusedFieldCapturesPrintableInput() bool {
	if f.OnReceipt() || f.focusField < 0 || f.focusField >= len(f.steps[f.cur].Fields) {
		return false
	}
	return f.steps[f.cur].Fields[f.focusField].capturesPrintableInput()
}

// updateNoProjectsConfirm drives the dedicated save confirmation. No and Back
// return to the receipt. q and ctrl+c cancel the flow immediately. None of
// those paths commit or start ingest. Only Yes reaches commitDraft.
func (f Flow) updateNoProjectsConfirm(msg tea.Msg) (Flow, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		if action, matched := keymap.Match(keymap.Default(), keyMsg, quitOnlyAvailability{}); matched && action == keymap.ActionQuit {
			f.confirmingNoProjects = false
			f.exited = true
			return f, tea.Quit
		}
	}

	var cmd tea.Cmd
	f.noProjectsConfirm, cmd = f.noProjectsConfirm.Update(msg)
	if cmd == nil {
		return f, nil
	}
	result, ok := runResult(cmd).(kit.ConfirmResultMsg)
	if !ok {
		return f, cmd
	}
	f.confirmingNoProjects = false
	if !result.OK {
		return f, nil
	}
	return f, f.commitDraft()
}

// forwardToFields sends msg to the current step's focused interactive field.
func (f Flow) forwardToFields(msg tea.Msg) (Flow, tea.Cmd) {
	if f.OnReceipt() || f.focusField < 0 || f.focusField >= len(f.steps[f.cur].Fields) {
		return f, nil
	}
	cmd := f.steps[f.cur].Fields[f.focusField].handle(f.draft, msg)
	return f, cmd
}

// forwardAsync uses the private async capability rather than the general Field
// handler. Immutable component owner IDs and generation guards select the one
// recipient without invoking synchronous fields on foreign work.
func (f Flow) forwardAsync(msg tea.Msg) (Flow, tea.Cmd) {
	cmds := fieldAsyncCommands(f.reg, f.draft, msg)
	f.recompute()
	if len(cmds) == 0 {
		return f, nil
	}
	return f, tea.Batch(cmds...)
}

// availability reports the flow-level actions for dispatch, footer, and help.
func (f Flow) availability() flowAvailability {
	state := f.viewportState()
	return flowAvailability{flow: &f, viewport: &state}
}

// dispatchAvailability measures the Flow viewport only for one of its paging
// keys. Ordinary field and lifecycle input therefore does not re-render a
// derived guide or consent provider merely to resolve an unrelated action.
func (f Flow) dispatchAvailability(keyMsg tea.KeyPressMsg) flowAvailability {
	availability := flowAvailability{flow: &f}
	if f.focusedFieldOwnsViewport() {
		return availability
	}
	if _, paging := keymap.Match(keymap.Default(), keyMsg, flowViewportKeyAvailability{}); !paging {
		return availability
	}
	state := f.viewportState()
	availability.viewport = &state
	return availability
}

// View renders the current step (or receipt) inside a kit.Frame, overlaying the
// active save or exit confirmation when one is shown.
func (f Flow) View() string {
	layout := kit.NewFrame(f.th).WithTitle(f.title()).WithFooter(" ")
	layout.SetSize(f.width, f.height)
	body := f.renderBody(layout.InnerWidth(), layout.InnerHeight())
	state := viewportStateFor(body, layout.InnerHeight(), f.viewOffset)
	availability := flowAvailability{flow: &f, viewport: &state}
	frame := kit.NewFrame(f.th).WithTitle(f.title()).WithFooter(f.footer(availability))
	frame.SetSize(f.width, f.height)
	frame.SetContent(windowBodyAt(body, frame.InnerHeight(), state.offset))
	base := frame.View()
	if f.helping {
		return f.overlay.Push(helpLayer{th: f.th, entries: keymap.HelpEntries(f.actionKeymap(), availability)}).View(base)
	}
	if f.confirmingNoProjects {
		width := noProjectsConfirmationWidth(f.width)
		f.noProjectsConfirm.SetSize(width, kit.ConfirmMinSize.Height)
		return f.overlay.Push(noProjectsConfirmLayer{th: f.th, c: f.noProjectsConfirm}).View(base)
	}
	if f.confirmingExit {
		f.exitConfirm.SetSize(kit.ConfirmMinSize.Width, kit.ConfirmMinSize.Height)
		return f.overlay.Push(confirmLayer{c: f.exitConfirm}).View(base)
	}
	return base
}

// stepTabs renders the ordered step navigation as a single tab strip with the
// current step highlighted, so a user always sees which step they are on and
// how many remain - the FTUE wizard always showed its step position, and prior
// feedback flagged that changing steps without an indicator was disorienting. The
// synthetic final commit step is shown as a trailing "review & save" tab.
func (f Flow) stepTabs(width int) string {
	styles := f.th.Styles()
	labels := make([]string, 0, len(f.steps)+1)
	for _, s := range f.steps {
		lbl := s.Title
		if lbl == "" {
			lbl = s.Key
		}
		labels = append(labels, lbl)
	}
	labels = append(labels, "review & save")
	var b strings.Builder
	for i, lbl := range labels {
		if i > 0 {
			b.WriteString(styles.Muted.Render("  "))
		}
		cell := " " + lbl + " "
		if i == f.cur {
			b.WriteString(styles.Selected.Render(cell))
		} else {
			b.WriteString(styles.Muted.Render(cell))
		}
	}
	return clip(b.String(), width)
}

func (f Flow) title() string {
	if f.OnReceipt() {
		return "review and save"
	}
	return f.steps[f.cur].Title
}

func (f Flow) actionKeymap() keymap.Keymap {
	if !f.OnReceipt() && f.focusField >= 0 && f.focusField < len(f.steps[f.cur].Fields) {
		return f.steps[f.cur].Fields[f.focusField].actionKeymap()
	}
	return keymap.Default()
}

func (f Flow) footer(availability keymap.Availability) string {
	return keymap.FooterView(f.th, f.actionKeymap(), availability)
}

// body renders the current step's fields, or the receipt, through Flow's
// private viewport. Tree fields keep their own viewport and are already sized
// to the remaining body height; guided prose, choice fields, and receipts use
// this window so every row remains reachable in a fixed terminal.
func (f Flow) body(width, height int) string {
	return f.windowBody(f.renderBody(width, height), height)
}

func (f Flow) renderBody(width, height int) string {
	styles := f.th.Styles()
	tabs := f.stepTabs(width)
	if f.OnReceipt() {
		return joinLines([]string{tabs, "", f.renderReceipt(styles, width)})
	}
	lines := []string{tabs, ""}
	guide := f.guideLines(styles, width)
	guidePending := len(guide) > 0
	visibleFields := f.steps[f.cur].visibleFields(f.draft)
	if len(visibleFields) == 0 && guidePending {
		lines = append(lines, guide...)
		guidePending = false
	}
	// The tab strip and its blank separator already consume rows; a scrolling
	// field (the tree) must be sized to the height that REMAINS, minus the
	// header and description lines it draws above its control. Sizing it to the
	// full height renders more rows than fit and the frame clips its bottom -
	// so the cursor is invisible when it reaches the last row.
	used := len(lines)
	for _, fld := range visibleFields {
		var chrome []string
		if lbl := fld.Label(); lbl != "" && fld.Kind() != KindInfo {
			headerStyle := styles.Header.Background(f.th.Color(f.th.Palette.Canvas))
			chrome = append(chrome, kit.FitLine(headerStyle, lbl, width))
		}
		if guidePending {
			chrome = append(chrome, guide...)
			guidePending = false
		}
		if d := fld.Description(); d != "" && fld.Kind() != KindInfo {
			for _, line := range strings.Split(ansi.Wrap(d, width, ""), "\n") {
				chrome = append(chrome, styles.Muted.Render(line))
			}
		}
		avail := height - used - len(chrome)
		if avail < 1 {
			avail = 1
		}
		fld.setSize(width, avail)
		lines = append(lines, chrome...)
		rendered := f.renderFlowControl(fld, styles, width)
		lines = append(lines, rendered)
		used += len(chrome) + strings.Count(rendered, "\n") + 1
	}
	return joinLines(lines)
}

func (f Flow) renderFlowControl(fld Field, styles theme.Styles, width int) string {
	if toggle, ok := fld.(*toggleField); ok {
		return toggle.renderFlowControl(f.draft, width)
	}
	return fld.render(f.draft, styles, width)
}

type flowViewportState struct {
	offset    int
	maxOffset int
	page      int
}

func (f Flow) viewportState() flowViewportState {
	frame := kit.NewFrame(f.th).WithTitle(f.title()).WithFooter(" ")
	frame.SetSize(f.width, f.height)
	page := frame.InnerHeight()
	if page < 0 {
		page = 0
	}
	body := f.renderBody(frame.InnerWidth(), page)
	return viewportStateFor(body, page, f.viewOffset)
}

func viewportStateFor(body string, page, offset int) flowViewportState {
	total := len(strings.Split(body, "\n"))
	maxOffset := total - page
	if maxOffset < 0 {
		maxOffset = 0
	}
	if offset < 0 {
		offset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}
	return flowViewportState{offset: offset, maxOffset: maxOffset, page: page}
}

func (f Flow) windowBody(body string, height int) string {
	state := viewportStateFor(body, height, f.viewOffset)
	return windowBodyAt(body, height, state.offset)
}

func windowBodyAt(body string, height, offset int) string {
	if height <= 0 {
		return ""
	}
	lines := strings.Split(body, "\n")
	end := offset + height
	if end > len(lines) {
		end = len(lines)
	}
	return joinLines(lines[offset:end])
}

func (f *Flow) scrollViewport(action keymap.ActionID) {
	state := f.viewportState()
	offset := state.offset
	page := state.page
	if page < 1 {
		page = 1
	}
	switch action {
	case keymap.ActionPageUp:
		offset -= page
	case keymap.ActionPageDown:
		offset += page
	case keymap.ActionTop:
		offset = 0
	case keymap.ActionBottom:
		offset = state.maxOffset
	}
	if offset < 0 {
		offset = 0
	}
	if offset > state.maxOffset {
		offset = state.maxOffset
	}
	f.viewOffset = offset
}

func (f Flow) focusedFieldOwnsViewport() bool {
	if f.OnReceipt() || f.focusField < 0 || f.focusField >= len(f.steps[f.cur].Fields) {
		return false
	}
	return f.steps[f.cur].Fields[f.focusField].Kind() == KindTree
}

func (f Flow) viewportActions(state *flowViewportState) []keymap.ActionID {
	if f.focusedFieldOwnsViewport() {
		return nil
	}
	if state == nil || state.maxOffset == 0 {
		return nil
	}
	var actions []keymap.ActionID
	if state.offset > 0 {
		actions = append(actions, keymap.ActionPageUp, keymap.ActionTop)
	}
	if state.offset < state.maxOffset {
		actions = append(actions, keymap.ActionPageDown, keymap.ActionBottom)
	}
	return actions
}

// guideLines renders the current section's optional onboarding metadata. The
// caller places it once before the first field's description/control and after
// its heading when it has one. Dense presentations intentionally do not call
// this helper. Empty guide values consume no space, and a derived example
// receives the same Draft the fields use without gaining control over field
// behavior or terminal styling.
func (f Flow) guideLines(styles theme.Styles, width int) []string {
	if f.OnReceipt() || f.cur < 0 || f.cur >= len(f.steps) {
		return nil
	}
	guide := f.steps[f.cur].Guide
	if guide == nil {
		return nil
	}
	var lines []string
	appendLine := func(value string, prefix string, style lipgloss.Style, full bool) {
		value = strings.Join(strings.Fields(value), " ")
		if value == "" {
			return
		}
		value = prefix + value
		if full {
			lines = append(lines, kit.FitLine(style, value, width))
			return
		}
		lines = append(lines, style.Render(clip(value, width)))
	}
	appendLine(guide.Intro, "", styles.Surface, true)
	for _, hint := range guide.Hints {
		appendLine(hint, "• ", styles.Muted, false)
	}
	if guide.Example != nil {
		example, err := guide.Example(f.draft)
		if err != nil {
			lines = append(lines, f.guideErrorLines(styles, width, err)...)
		} else if rendered, renderErr := f.renderGuideExampleLines(styles, width, example); renderErr != nil {
			lines = append(lines, f.guideErrorLines(styles, width, renderErr)...)
		} else {
			lines = append(lines, rendered...)
		}
	}
	return lines
}

func (f Flow) renderGuideExampleLines(styles theme.Styles, width int, example []GuideExampleLine) ([]string, error) {
	surface := f.th.Color(f.th.Palette.Surface)
	var lines []string
	for index, line := range example {
		if err := validateGuideExampleLine(index, line); err != nil {
			return nil, err
		}
		switch line.Kind {
		case GuideExampleLineText:
			lines = append(lines, kit.FitLine(styles.Surface, line.Text, width))
		case GuideExampleLineLabel:
			lines = append(lines, kit.FitLine(styles.Header.Background(surface), line.Text, width))
		case GuideExampleLineBefore:
			lines = append(lines, kit.FitLine(styles.DiffDel.Background(surface), "- before: "+line.Text, width))
		case GuideExampleLineAfter:
			lines = append(lines, kit.FitLine(styles.DiffAdd.Background(surface), "+ after: "+line.Text, width))
		case GuideExampleLineSpacer:
			lines = append(lines, kit.FitLine(styles.Surface, "", width))
		}
	}
	return lines, nil
}

func validateGuideExampleLine(index int, line GuideExampleLine) error {
	if !line.Kind.IsValid() {
		return guideExampleContractError(
			fmt.Sprintf("example line %d has unknown semantic kind %d", index, line.Kind),
			"the provider returned a line the Flow cannot style or prefix safely",
			"return Text, Label, Before, After, or Spacer from the guide example provider")
	}
	if line.Kind == GuideExampleLineSpacer {
		if line.Text != "" {
			return guideExampleContractError(
				fmt.Sprintf("example spacer line %d carries text", index),
				"a spacer cannot both separate groups and present content",
				"move the text into a separately typed example line")
		}
		return nil
	}
	if strings.TrimSpace(line.Text) == "" {
		return guideExampleContractError(
			fmt.Sprintf("example line %d has no visible text", index),
			"a semantic content line cannot demonstrate an empty value",
			"return non-empty plain text or an explicit Spacer line")
	}
	if strings.ContainsAny(line.Text, "\r\n") {
		return guideExampleContractError(
			fmt.Sprintf("example line %d contains an embedded line break", index),
			"one typed line must map to exactly one terminal row",
			"split multiline content into separately typed lines")
	}
	if ansi.Strip(line.Text) != line.Text {
		return guideExampleContractError(
			fmt.Sprintf("example line %d contains pre-rendered terminal styling", index),
			"guide providers return semantic plain text and Flow applies the canonical theme exactly once",
			"remove ANSI escapes and select the matching GuideExampleLineKind")
	}
	for _, value := range line.Text {
		if unicode.IsControl(value) {
			return guideExampleContractError(
				fmt.Sprintf("example line %d contains terminal control character U+%04X", index, value),
				"control bytes can change terminal state instead of presenting literal example text",
				"remove control bytes and return one printable plain-text terminal line")
		}
	}
	return nil
}

func guideExampleContractError(what, why, fix string) error {
	return fmt.Errorf(
		"guide example unavailable.\n"+
			"what: %s.\n"+
			"why: %s.\n"+
			"where: settings Flow guide-example boundary.\n"+
			"when: before rendering a derived example under its field heading.\n"+
			"means: unverified example output is withheld while the canonical field remains available.\n"+
			"fix: %s.",
		what, why, fix)
}

func (f Flow) guideErrorLines(styles theme.Styles, width int, err error) []string {
	style := styles.Danger.Background(f.th.Color(f.th.Palette.Surface))
	values := append([]string{"example unavailable; unverified output withheld"}, splitLines(err.Error())...)
	lines := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			value = sanitizeTerminalLine(value)
			lines = append(lines, kit.FitLine(style, value, width))
		}
	}
	return lines
}

func sanitizeTerminalLine(value string) string {
	return strings.Map(func(char rune) rune {
		if unicode.IsControl(char) {
			return -1
		}
		return char
	}, ansi.Strip(value))
}

// interactive reports whether a field takes input (everything but Info).
func interactive(fld Field) bool { return fld.Kind() != KindInfo }

// flowAvailability adapts a Flow's current state to keymap.Availability so
// dispatch, the footer, and the help overlay stay in lockstep.
type flowAvailability struct {
	flow     *Flow
	viewport *flowViewportState
}

func (a flowAvailability) AvailableActions() []keymap.ActionID {
	f := a.flow
	var out []keymap.ActionID
	if f.OnReceipt() {
		out = append(out, keymap.ActionConfirm)
		out = append(out, f.viewportActions(a.viewport)...)
		if len(f.steps) > 0 {
			out = append(out, keymap.ActionPrevField)
		}
		out = append(out, keymap.ActionBack, keymap.ActionQuit, keymap.ActionHelp)
		return out
	}
	// A real step: the focused field's actions, then step navigation and exit.
	if f.focusField >= 0 && f.focusField < len(f.steps[f.cur].Fields) {
		out = append(out, f.steps[f.cur].Fields[f.focusField].availableActions()...)
	}
	out = append(out, f.viewportActions(a.viewport)...)
	out = append(out, keymap.ActionNextField)
	if f.cur > 0 {
		out = append(out, keymap.ActionPrevField)
	}
	out = append(out, keymap.ActionBack, keymap.ActionQuit, keymap.ActionHelp)
	return effectiveAvailability(dedupeActions(out), f.focusedFieldCapturesPrintableInput())
}

// footerPruningField is an optional Field refinement: a field that wants its
// step's footer trimmed to a few primary keys, with the rest still reachable
// through help. The keys it returns are advisory - FooterView intersects them
// with what is dispatchable - so a field can name a key that is not currently
// available without ever producing a footer hint no press could match.
type footerPruningField interface {
	primaryFooterActions() []keymap.ActionID
}

// FooterActions lets the footer hint bar show fewer keys than the help overlay
// when the focused field opts into a compact footer. The help overlay keeps
// consuming AvailableActions, so it still lists everything dispatchable.
func (a flowAvailability) FooterActions() []keymap.ActionID {
	f := a.flow
	if f.OnReceipt() || f.focusField < 0 || f.focusField >= len(f.steps[f.cur].Fields) {
		return a.AvailableActions()
	}
	pruner, ok := f.steps[f.cur].Fields[f.focusField].(footerPruningField)
	if !ok {
		return a.AvailableActions()
	}
	primary := pruner.primaryFooterActions()
	if len(primary) == 0 {
		return a.AvailableActions()
	}
	return primary
}

type flowViewportKeyAvailability struct{}

func (flowViewportKeyAvailability) AvailableActions() []keymap.ActionID {
	return []keymap.ActionID{keymap.ActionPageUp, keymap.ActionPageDown, keymap.ActionTop, keymap.ActionBottom}
}

func dedupeActions(in []keymap.ActionID) []keymap.ActionID {
	seen := map[keymap.ActionID]bool{}
	var out []keymap.ActionID
	for _, a := range in {
		if seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

// confirmLayer mounts a kit.Confirm as an overlay layer.
type confirmLayer struct{ c kit.Confirm }

func (l confirmLayer) View() string { return l.c.View() }

// noProjectsConfirmLayer keeps the accepted impact text separate from the exit
// modal while delegating the actual yes/no interaction to kit.Confirm.
type noProjectsConfirmLayer struct {
	th theme.Theme
	c  kit.Confirm
}

func (l noProjectsConfirmLayer) View() string {
	return l.th.Styles().Base.Render(noProjectsConfirmationEffects) + "\n" + l.c.View()
}

func noProjectsConfirmationWidth(maxWidth int) int {
	width := lipgloss.Width(noProjectsConfirmationQuestion)
	for _, line := range strings.Split(noProjectsConfirmationEffects, "\n") {
		if lineWidth := lipgloss.Width(line); lineWidth > width {
			width = lineWidth
		}
	}
	if width < kit.ConfirmMinSize.Width {
		width = kit.ConfirmMinSize.Width
	}
	if maxWidth > 0 && width > maxWidth {
		return maxWidth
	}
	return width
}

type quitOnlyAvailability struct{}

func (quitOnlyAvailability) AvailableActions() []keymap.ActionID {
	return []keymap.ActionID{keymap.ActionQuit}
}

// helpAvailability reports the actions the help overlay itself dispatches -
// only ? (toggle) and esc (back), both of which close the overlay.
type helpAvailability struct{}

func (helpAvailability) AvailableActions() []keymap.ActionID {
	return []keymap.ActionID{keymap.ActionHelp, keymap.ActionBack}
}

// helpLayer renders the full keybinding list for the flow's current step as a
// centered modal panel. Its rows come from keymap.HelpEntries over the SAME
// availability the dispatch and footer use, so the overlay can never advertise
// a key the step cannot actually dispatch - mirroring the FTUE's ? help while
// eliminating the drift the old ftue/help.go carried.
type helpLayer struct {
	th      theme.Theme
	entries []keymap.HelpEntry
}

// helpRow is one rendered line of the overlay, tagged with the style role it
// draws in so grouping headers, entries, the title, and the footer can be
// styled uniformly after the panel width is known.
type helpRow struct {
	text string
	role helpRowRole
}

type helpRowRole int

const (
	helpRowTitle helpRowRole = iota
	helpRowHeader
	helpRowEntry
	helpRowFooter
	helpRowBlank
)

func (l helpLayer) View() string {
	styles := l.th.Styles()
	// Partition the dispatchable entries into the FTUE-faithful "navigation" and
	// "actions" categories, preserving Availability order within each. The rows
	// stay driven by keymap.HelpEntries over the SAME availability the dispatch
	// and footer use - grouping only re-buckets them, so the overlay still cannot
	// advertise a key the step cannot dispatch.
	var nav, act []keymap.HelpEntry
	for _, e := range l.entries {
		if e.Action.IsNavigation() {
			nav = append(nav, e)
		} else {
			act = append(act, e)
		}
	}

	rows := make([]helpRow, 0, len(l.entries)+6)
	rows = append(rows, helpRow{"keyboard shortcuts", helpRowTitle})
	appendCategory := func(name string, entries []keymap.HelpEntry) {
		if len(entries) == 0 {
			return
		}
		rows = append(rows, helpRow{"", helpRowBlank})
		rows = append(rows, helpRow{"  " + name, helpRowHeader})
		for _, e := range entries {
			rows = append(rows, helpRow{fmt.Sprintf("    %-12s %s", e.Key, e.Desc), helpRowEntry})
		}
	}
	appendCategory("navigation", nav)
	appendCategory("actions", act)
	rows = append(rows, helpRow{"", helpRowBlank})
	rows = append(rows, helpRow{"  press ? or esc to close", helpRowFooter})

	// Width the panel to its widest row so every line shares the surface
	// background, reading as one modal card rather than ragged text.
	width := 0
	for _, r := range rows {
		if w := len([]rune(r.text)); w > width {
			width = w
		}
	}
	width += 2
	surfaceBg := l.th.Color(l.th.Palette.Surface)
	pad := func(base lipgloss.Style, s string) string {
		if n := width - len([]rune(s)); n > 0 {
			s += strings.Repeat(" ", n)
		}
		return base.Background(surfaceBg).Render(s)
	}
	styleFor := func(role helpRowRole) lipgloss.Style {
		switch role {
		case helpRowTitle:
			return styles.Header
		case helpRowHeader:
			return styles.Header
		case helpRowFooter:
			return styles.Muted
		default:
			return styles.Surface
		}
	}
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(pad(styleFor(r.role), r.text))
	}
	return b.String()
}

// runResult runs a command far enough to read the message it produced. The
// confirm modal's command emits a single ConfirmResultMsg synchronously.
func runResult(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}
