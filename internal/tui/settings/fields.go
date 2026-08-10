package settings

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// Shared row glyphs for the self-rendered choice fields. They mirror the kit's
// own selection vocabulary so a settings field and a kit component draw the
// same box/radio/cursor marks.
const (
	fieldCursor   = "▸ "
	fieldNoCursor = "  "
	boxChecked    = "[✓]"
	boxUnchecked  = "[ ]"
	radioSelected = "(•)"
	radioEmpty    = "( )"
)

// clip truncates a plain string to at most width display cells using the
// ansi-aware lipgloss.Width, placing an ellipsis when it cuts. It never uses
// len(): byte length miscounts wide runes and escapes.
func clip(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	runes := []rune(s)
	budget := width - 1
	var out []rune
	for _, r := range runes {
		if lipgloss.Width(string(append(out, r))) > budget {
			break
		}
		out = append(out, r)
	}
	res := string(out) + "…"
	if lipgloss.Width(res) > width {
		return string(out)
	}
	return res
}

// cursorPrefix returns the styled cursor/no-cursor prefix for a row.
func cursorPrefix(styles theme.Styles, active bool) string {
	if active {
		return styles.Selected.Render(fieldCursor)
	}
	return styles.Base.Render(fieldNoCursor)
}

// --- Toggle ---------------------------------------------------------------

type toggleField struct {
	baseField
	th      theme.Theme
	acc     Accessor[bool]
	focused bool
	width   int
}

// Toggle builds a boolean field bound to acc.
func Toggle(key, label string, acc Accessor[bool]) Field {
	return &toggleField{baseField: baseField{key: key, label: label}, acc: acc, width: kit.ToggleMinSize.Width}
}

func (f *toggleField) Kind() FieldKind       { return KindToggle }
func (f *toggleField) Validate(*Draft) error { return nil }
func (f *toggleField) mount(t theme.Theme)   { f.th = t }
func (f *toggleField) initCmd() tea.Cmd      { return nil }
func (f *toggleField) focus() tea.Cmd        { f.focused = true; return nil }
func (f *toggleField) blur()                 { f.focused = false }
func (f *toggleField) setSize(w, _ int)      { f.width = w }
func (f *toggleField) sync(*Draft)           {}
func (f *toggleField) reset(d *Draft)        { f.acc.Set(d.Working(), f.acc.Get(d.Baseline())) }
func (f *toggleField) availableActions() []keymap.ActionID {
	return []keymap.ActionID{keymap.ActionToggle}
}

func (f *toggleField) Dirty(d *Draft) bool {
	return f.acc.Get(d.Working()) != f.acc.Get(d.Baseline())
}

func (f *toggleField) build(d *Draft) kit.Toggle {
	t := kit.NewToggle(f.th, f.label, f.acc.Get(d.Working()))
	t.SetSize(f.width, 1)
	if f.focused {
		t.Focus()
	}
	return t
}

func (f *toggleField) handle(d *Draft, msg tea.Msg) tea.Cmd {
	t := f.build(d)
	t, cmd := t.Update(msg)
	f.acc.Set(d.Working(), t.On())
	return cmd
}

func (f *toggleField) render(d *Draft, _ theme.Styles, width int) string {
	f.width = width
	return f.build(d).View()
}

// --- Text -----------------------------------------------------------------

type textField struct {
	baseField
	th      theme.Theme
	acc     Accessor[string]
	inner   kit.TextField
	focused bool
	width   int
}

// Text builds a free-text field bound to acc.
func Text(key, label string, acc Accessor[string]) Field {
	return &textField{baseField: baseField{key: key, label: label}, acc: acc, width: kit.TextFieldMinSize.Width}
}

func (f *textField) Kind() FieldKind              { return KindText }
func (f *textField) Validate(*Draft) error        { return nil }
func (f *textField) initCmd() tea.Cmd             { return nil }
func (f *textField) blur()                        { f.focused = false; f.inner.Blur() }
func (f *textField) reset(d *Draft)               { f.acc.Set(d.Working(), f.acc.Get(d.Baseline())) }
func (f *textField) capturesPrintableInput() bool { return f.focused }
func (f *textField) availableActions() []keymap.ActionID {
	return []keymap.ActionID{keymap.ActionConfirm}
}

func (f *textField) mount(t theme.Theme) {
	f.th = t
	f.inner = kit.NewTextField(t, f.label)
	f.inner.SetSize(f.width, 1)
}

func (f *textField) focus() tea.Cmd { f.focused = true; return f.inner.Focus() }

func (f *textField) setSize(w, _ int) {
	f.width = w
	f.inner.SetSize(w, 1)
}

func (f *textField) sync(d *Draft) { f.inner.SetValue(f.acc.Get(d.Working())) }

func (f *textField) Dirty(d *Draft) bool {
	return f.acc.Get(d.Working()) != f.acc.Get(d.Baseline())
}

func (f *textField) handle(d *Draft, msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	f.inner, cmd = f.inner.Update(msg)
	f.acc.Set(d.Working(), f.inner.Value())
	return cmd
}

func (f *textField) render(d *Draft, _ theme.Styles, width int) string {
	f.setSize(width, 1)
	// Keep the visible text in step with the draft even when focus never
	// touched the inner input.
	if !f.focused {
		f.inner.SetValue(f.acc.Get(d.Working()))
	}
	return f.inner.View()
}

// --- Radio ----------------------------------------------------------------

type radioField[T comparable] struct {
	baseField
	th      theme.Theme
	acc     Accessor[T]
	opts    []Option[T]
	cursor  int
	focused bool
	width   int
}

// Radio builds a single-choice field bound to acc over static opts.
func Radio[T comparable](key, label string, acc Accessor[T], opts ...Option[T]) Field {
	return &radioField[T]{baseField: baseField{key: key, label: label}, acc: acc, opts: opts}
}

func (f *radioField[T]) Kind() FieldKind       { return KindRadio }
func (f *radioField[T]) Validate(*Draft) error { return nil }
func (f *radioField[T]) mount(t theme.Theme)   { f.th = t }
func (f *radioField[T]) initCmd() tea.Cmd      { return nil }
func (f *radioField[T]) focus() tea.Cmd        { f.focused = true; return nil }
func (f *radioField[T]) blur()                 { f.focused = false }
func (f *radioField[T]) setSize(w, _ int)      { f.width = w }
func (f *radioField[T]) reset(d *Draft)        { f.acc.Set(d.Working(), f.acc.Get(d.Baseline())) }
func (f *radioField[T]) availableActions() []keymap.ActionID {
	return []keymap.ActionID{keymap.ActionUp, keymap.ActionDown, keymap.ActionToggle, keymap.ActionConfirm}
}

func (f *radioField[T]) Dirty(d *Draft) bool {
	return f.acc.Get(d.Working()) != f.acc.Get(d.Baseline())
}

// sync moves the cursor onto the currently-selected option so re-entering the
// step (or a dropped edit) lands the cursor on the live value.
func (f *radioField[T]) sync(d *Draft) {
	cur := f.acc.Get(d.Working())
	for i, o := range f.opts {
		if o.Value == cur {
			f.cursor = i
			return
		}
	}
}

func (f *radioField[T]) handle(d *Draft, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	action, ok := keymap.Match(keymap.Default(), keyMsg, availList(f.availableActions()))
	if !ok {
		return nil
	}
	switch action {
	case keymap.ActionUp:
		if f.cursor > 0 {
			f.cursor--
		}
	case keymap.ActionDown:
		if f.cursor < len(f.opts)-1 {
			f.cursor++
		}
	case keymap.ActionToggle, keymap.ActionConfirm:
		if f.cursor >= 0 && f.cursor < len(f.opts) {
			f.acc.Set(d.Working(), f.opts[f.cursor].Value)
		}
	}
	return nil
}

func (f *radioField[T]) render(d *Draft, styles theme.Styles, width int) string {
	f.width = width
	cur := f.acc.Get(d.Working())
	var lines []string
	for i, o := range f.opts {
		mark := radioEmpty
		markStyle := styles.Muted
		if o.Value == cur {
			mark = radioSelected
			markStyle = styles.Success
		}
		active := f.focused && i == f.cursor
		row := cursorPrefix(styles, active) + markStyle.Render(mark) + " " +
			styles.Base.Render(clip(o.Label, width-lipgloss.Width(fieldCursor)-lipgloss.Width(mark)-1))
		lines = append(lines, row)
	}
	// Draw the help for the option in focus - the one under the cursor while the
	// field is focused, else the selected value - so the user reads the meaning
	// of the choice they are about to pick, conditional on the current selection.
	if help := f.highlightedHelp(cur); help != "" {
		lines = append(lines, "", styles.Muted.Render(clip(help, width)))
	}
	return joinLines(lines)
}

// highlightedHelp returns the description of the option the user's attention is
// on: the cursor's option when the field is focused, otherwise the option whose
// value is currently selected (cur). It returns "" when that option has no help.
func (f *radioField[T]) highlightedHelp(cur T) string {
	if f.focused && f.cursor >= 0 && f.cursor < len(f.opts) {
		return f.opts[f.cursor].Description
	}
	for _, o := range f.opts {
		if o.Value == cur {
			return o.Description
		}
	}
	return ""
}

// --- MultiSelect ----------------------------------------------------------

type multiSelectField[T comparable] struct {
	baseField
	th      theme.Theme
	acc     Accessor[[]T]
	opts    []Option[T]
	cursor  int
	focused bool
	width   int
}

// MultiSelect builds a multi-choice field bound to acc over static opts.
func MultiSelect[T comparable](key, label string, acc Accessor[[]T], opts ...Option[T]) Field {
	return &multiSelectField[T]{baseField: baseField{key: key, label: label}, acc: acc, opts: opts}
}

func (f *multiSelectField[T]) Kind() FieldKind       { return KindMultiSelect }
func (f *multiSelectField[T]) Validate(*Draft) error { return nil }
func (f *multiSelectField[T]) mount(t theme.Theme)   { f.th = t }
func (f *multiSelectField[T]) initCmd() tea.Cmd      { return nil }
func (f *multiSelectField[T]) focus() tea.Cmd        { f.focused = true; return nil }
func (f *multiSelectField[T]) blur()                 { f.focused = false }
func (f *multiSelectField[T]) setSize(w, _ int)      { f.width = w }
func (f *multiSelectField[T]) sync(*Draft)           {}
func (f *multiSelectField[T]) reset(d *Draft) {
	f.acc.Set(d.Working(), cloneSlice(f.acc.Get(d.Baseline())))
}
func (f *multiSelectField[T]) availableActions() []keymap.ActionID {
	return []keymap.ActionID{keymap.ActionUp, keymap.ActionDown, keymap.ActionToggle, keymap.ActionSelectAll}
}

func (f *multiSelectField[T]) Dirty(d *Draft) bool {
	return !sameSet(f.acc.Get(d.Working()), f.acc.Get(d.Baseline()))
}

func (f *multiSelectField[T]) handle(d *Draft, msg tea.Msg) tea.Cmd {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	action, ok := keymap.Match(keymap.Default(), keyMsg, availList(f.availableActions()))
	if !ok {
		return nil
	}
	switch action {
	case keymap.ActionUp:
		if f.cursor > 0 {
			f.cursor--
		}
	case keymap.ActionDown:
		if f.cursor < len(f.opts)-1 {
			f.cursor++
		}
	case keymap.ActionToggle:
		if f.cursor >= 0 && f.cursor < len(f.opts) {
			f.acc.Set(d.Working(), toggleMember(f.acc.Get(d.Working()), f.opts[f.cursor].Value))
		}
	case keymap.ActionSelectAll:
		f.acc.Set(d.Working(), f.selectAll(f.acc.Get(d.Working())))
	}
	return nil
}

// selectAll checks every option when any is unchecked, otherwise clears all.
func (f *multiSelectField[T]) selectAll(cur []T) []T {
	if len(cur) >= len(f.opts) {
		allIn := true
		for _, o := range f.opts {
			if !contains(cur, o.Value) {
				allIn = false
				break
			}
		}
		if allIn {
			return nil
		}
	}
	out := make([]T, 0, len(f.opts))
	for _, o := range f.opts {
		out = append(out, o.Value)
	}
	return out
}

func (f *multiSelectField[T]) render(d *Draft, styles theme.Styles, width int) string {
	f.width = width
	cur := f.acc.Get(d.Working())
	var lines []string
	for i, o := range f.opts {
		mark := boxUnchecked
		markStyle := styles.Muted
		if contains(cur, o.Value) {
			mark = boxChecked
			markStyle = styles.Success
		}
		active := f.focused && i == f.cursor
		row := cursorPrefix(styles, active) + markStyle.Render(mark) + " " +
			styles.Base.Render(clip(o.Label, width-lipgloss.Width(fieldCursor)-lipgloss.Width(mark)-1))
		lines = append(lines, row)
	}
	return joinLines(lines)
}

// --- Info -----------------------------------------------------------------

type infoField struct {
	baseField
	th       theme.Theme
	renderFn func(*Draft) string
}

// Info builds a read-only field whose body is produced by render.
func Info(key string, render func(*Draft) string) Field {
	return &infoField{baseField: baseField{key: key}, renderFn: render}
}

func (f *infoField) Kind() FieldKind       { return KindInfo }
func (f *infoField) Label() string         { return f.baseField.key }
func (f *infoField) Validate(*Draft) error { return nil }
func (f *infoField) Dirty(*Draft) bool     { return false }
func (f *infoField) mount(t theme.Theme)   { f.th = t }
func (f *infoField) initCmd() tea.Cmd      { return nil }
func (f *infoField) focus() tea.Cmd        { return nil }
func (f *infoField) blur()                 {}
func (f *infoField) setSize(int, int)      {}
func (f *infoField) sync(*Draft)           {}
func (f *infoField) reset(*Draft)          {}
func (f *infoField) handle(*Draft, tea.Msg) tea.Cmd {
	return nil
}
func (f *infoField) availableActions() []keymap.ActionID { return nil }

func (f *infoField) render(d *Draft, styles theme.Styles, width int) string {
	body := ""
	if f.renderFn != nil {
		body = f.renderFn(d)
	}
	var out []string
	for _, ln := range splitLines(body) {
		out = append(out, styles.Muted.Render(clip(ln, width)))
	}
	return joinLines(out)
}

// --- shared helpers -------------------------------------------------------

// availList adapts a static action slice to keymap.Availability.
type availList []keymap.ActionID

func (a availList) AvailableActions() []keymap.ActionID { return a }

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

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	out = append(out, cur)
	return out
}

func contains[T comparable](xs []T, v T) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func toggleMember[T comparable](xs []T, v T) []T {
	if contains(xs, v) {
		out := make([]T, 0, len(xs))
		for _, x := range xs {
			if x != v {
				out = append(out, x)
			}
		}
		return out
	}
	return append(cloneSlice(xs), v)
}

func cloneSlice[T any](xs []T) []T {
	if xs == nil {
		return nil
	}
	out := make([]T, len(xs))
	copy(out, xs)
	return out
}

func sameSet[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !contains(b, x) {
			return false
		}
	}
	return true
}
