package settings

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

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
//   - A step hidden by a changed earlier answer has its edits DROPPED before
//     the receipt, and the receipt reflects that.
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

	steps      []Section // current visible steps, recomputed on navigation
	cur        int       // 0..len(steps); == len(steps) is the receipt step
	focusField int       // index of the focused field within the current step

	overlay    kit.Overlay
	confirm    kit.Confirm
	confirming bool
	helping    bool

	committed bool
	exited    bool
	err       error

	width, height int
}

// exitPrompt is the confirm-exit modal's text.
const exitPrompt = "leave settings without saving?"

// NewFlow mounts every field in reg over theme t and opens the flow on the
// first visible step of draft d.
func NewFlow(t theme.Theme, reg Registry, d *Draft) Flow {
	for _, s := range reg.Sections {
		for _, fld := range s.Fields {
			fld.mount(t)
		}
	}
	f := Flow{th: t, reg: reg, draft: d, confirm: kit.NewConfirm(t, exitPrompt), overlay: kit.NewOverlay(t)}
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
func (f Flow) Confirming() bool { return f.confirming }

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

// SetSize records the outer render region.
func (f *Flow) SetSize(width, height int) {
	f.width, f.height = width, height
	f.overlay.SetSize(width, height)
}

// enterStep re-syncs the current step's fields from the draft and focuses its
// first interactive field.
func (f *Flow) enterStep() {
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
	f.steps = f.reg.visibleSections(f.draft)
	if f.cur > len(f.steps) {
		f.cur = len(f.steps)
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
		// Reaching the receipt drops the edits of any step hidden by a changed
		// answer so the receipt reflects only what will be committed.
		f.dropHiddenEdits()
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

// dropHiddenEdits resets every field of every hidden section to its baseline,
// iterating to a fixpoint because dropping an edit can change which sections
// are hidden. This is what makes a step hidden by a changed answer contribute
// nothing to the commit, and what the receipt reflects.
func (f *Flow) dropHiddenEdits() {
	for i := 0; i < len(f.reg.Sections)+1; i++ {
		changed := false
		for _, s := range f.reg.hiddenSections(f.draft) {
			for _, fld := range s.Fields {
				before := f.draft.Dirty()
				fld.reset(f.draft)
				if before != f.draft.Dirty() {
					changed = true
				}
			}
		}
		if !changed {
			return
		}
	}
}

// commit drops hidden edits, validates every visible field, then performs the
// single atomic commit. Any failure sets err and writes nothing.
func (f *Flow) commit() tea.Cmd {
	f.dropHiddenEdits()
	for _, s := range f.reg.visibleSections(f.draft) {
		for _, fld := range s.Fields {
			if err := fld.Validate(f.draft); err != nil {
				f.err = err
				return nil
			}
		}
	}
	if err := f.draft.Commit(); err != nil {
		f.err = err
		return nil
	}
	f.committed = true
	return tea.Quit
}

// Update dispatches a message. Key handling order enforces the ratified
// semantics: the exit modal is answered first; esc always opens that modal;
// step navigation is next; everything else goes to the focused field.
func (f Flow) Update(msg tea.Msg) (Flow, tea.Cmd) {
	if f.confirming {
		return f.updateConfirm(msg)
	}
	keyMsg, isKey := msg.(tea.KeyPressMsg)
	if !isKey {
		// Forward non-key messages (async tree load results, spinner ticks) to
		// the current step's fields.
		return f.forwardToFields(msg)
	}
	if f.helping {
		// While the help overlay is up, ? or esc closes it and every other key
		// is swallowed so it cannot leak into the underlying step.
		if action, ok := keymap.Match(keymap.Default(), keyMsg, helpAvailability{}); ok {
			switch action {
			case keymap.ActionHelp, keymap.ActionBack:
				f.helping = false
			}
		}
		return f, nil
	}
	action, ok := keymap.Match(keymap.Default(), keyMsg, f.availability())
	if ok {
		switch action {
		case keymap.ActionHelp:
			f.helping = true
			return f, nil
		case keymap.ActionBack, keymap.ActionQuit:
			// esc and q both leave settings, and both prompt the same
			// confirm-exit modal first; a confirmed exit writes nothing.
			f.confirming = true
			f.confirm = kit.NewConfirm(f.th, exitPrompt)
			f.confirm.Focus()
			return f, nil
		case keymap.ActionNextField:
			f.advance()
			return f, nil
		case keymap.ActionPrevField:
			f.retreat()
			return f, nil
		case keymap.ActionConfirm:
			if f.OnReceipt() {
				cmd := f.commit()
				return f, cmd
			}
		}
	}
	// Not a flow-level action (or ActionConfirm on a non-receipt step): forward
	// to the focused field.
	return f.forwardToFields(msg)
}

// updateConfirm drives the exit-confirm modal.
func (f Flow) updateConfirm(msg tea.Msg) (Flow, tea.Cmd) {
	var cmd tea.Cmd
	f.confirm, cmd = f.confirm.Update(msg)
	if cmd != nil {
		if res, ok := runResult(cmd).(kit.ConfirmResultMsg); ok {
			f.confirming = false
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

// forwardToFields sends msg to the current step's focused interactive field.
func (f Flow) forwardToFields(msg tea.Msg) (Flow, tea.Cmd) {
	if f.OnReceipt() || f.focusField < 0 || f.focusField >= len(f.steps[f.cur].Fields) {
		return f, nil
	}
	cmd := f.steps[f.cur].Fields[f.focusField].handle(f.draft, msg)
	return f, cmd
}

// availability reports the flow-level actions for dispatch, footer, and help.
func (f Flow) availability() flowAvailability {
	return flowAvailability{flow: &f}
}

// View renders the current step (or receipt) inside a kit.Frame, overlaying the
// exit-confirm modal when it is shown.
func (f Flow) View() string {
	frame := kit.NewFrame(f.th).WithTitle(f.title()).WithFooter(f.footer())
	frame.SetSize(f.width, f.height)
	frame.SetContent(f.body(frame.InnerWidth(), frame.InnerHeight()))
	base := frame.View()
	if f.helping {
		return f.overlay.Push(helpLayer{th: f.th, entries: keymap.HelpEntries(keymap.Default(), f.availability())}).View(base)
	}
	if f.confirming {
		f.confirm.SetSize(kit.ConfirmMinSize.Width, kit.ConfirmMinSize.Height)
		return f.overlay.Push(confirmLayer{c: f.confirm}).View(base)
	}
	return base
}

// stepTabs renders the ordered step navigation as a single tab strip with the
// current step highlighted, so a user always sees which step they are on and
// how many remain - the FTUE wizard always showed its step position, and the
// UAT flagged that the rebuild changed steps with no on-screen indicator. The
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

func (f Flow) footer() string {
	return keymap.FooterView(f.th, keymap.Default(), f.availability())
}

// body renders the current step's fields, or the receipt.
func (f Flow) body(width, height int) string {
	styles := f.th.Styles()
	tabs := f.stepTabs(width)
	if f.OnReceipt() {
		return joinLines([]string{tabs, "", f.renderReceipt(styles, width)})
	}
	lines := []string{tabs, ""}
	// The tab strip and its blank separator already consume rows; a scrolling
	// field (the tree) must be sized to the height that REMAINS, minus the
	// header and description lines it draws above its control. Sizing it to the
	// full height renders more rows than fit and the frame clips its bottom -
	// so the cursor is invisible when it reaches the last row.
	used := len(lines)
	for _, fld := range f.steps[f.cur].Fields {
		if !fld.When(f.draft) {
			continue
		}
		// A toggle draws its own label alongside its [on]/[off] state, so drawing
		// the label a second time as a section header would double it; the header
		// is drawn only for fields whose control does not render the label itself.
		var header, desc string
		if lbl := fld.Label(); lbl != "" && fld.Kind() != KindInfo && fld.Kind() != KindToggle {
			header = styles.Header.Render(clip(lbl, width))
		}
		if d := fld.Description(); d != "" && fld.Kind() != KindInfo {
			desc = styles.Muted.Render(clip(d, width))
		}
		chrome := 0
		if header != "" {
			chrome++
		}
		if desc != "" {
			chrome++
		}
		avail := height - used - chrome
		if avail < 1 {
			avail = 1
		}
		fld.setSize(width, avail)
		if header != "" {
			lines = append(lines, header)
		}
		if desc != "" {
			lines = append(lines, desc)
		}
		rendered := fld.render(f.draft, styles, width)
		lines = append(lines, rendered)
		used += chrome + strings.Count(rendered, "\n") + 1
	}
	return joinLines(lines)
}

// interactive reports whether a field takes input (everything but Info).
func interactive(fld Field) bool { return fld.Kind() != KindInfo }

// flowAvailability adapts a Flow's current state to keymap.Availability so
// dispatch, the footer, and the help overlay stay in lockstep.
type flowAvailability struct{ flow *Flow }

func (a flowAvailability) AvailableActions() []keymap.ActionID {
	f := a.flow
	var out []keymap.ActionID
	if f.OnReceipt() {
		out = append(out, keymap.ActionConfirm)
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
	out = append(out, keymap.ActionNextField)
	if f.cur > 0 {
		out = append(out, keymap.ActionPrevField)
	}
	out = append(out, keymap.ActionBack, keymap.ActionQuit, keymap.ActionHelp)
	return dedupeActions(out)
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
