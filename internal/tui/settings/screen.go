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

const (
	screenTitle         = "config"
	screenDiscardPrompt = "discard changes and leave config?"
	screenMinNavWidth   = 16
	screenMaxNavWidth   = 28
)

// SavedMsg reports that Screen completed a real Draft.Commit. The parent mount
// owns any responsibility-specific post-commit effect and can inspect the same
// committed draft through Draft.
type SavedMsg struct {
	draft *Draft
}

// Draft returns the draft whose commit completed successfully.
func (m SavedMsg) Draft() *Draft { return m.draft }

// Screen presents a Registry as a dense settings editor. Its section list is a
// jump target, fields retain buffered Draft state, ctrl+s is the sole save
// action, and exit always requires an explicit discard confirmation.
type Screen struct {
	th    theme.Theme
	reg   Registry
	draft *Draft

	sections    []Section
	section     int
	focusField  int
	navFocused  bool
	confirm     kit.Confirm
	overlay     kit.Overlay
	confirming  bool
	helping     bool
	savePending bool
	ready       bool
	err         error

	width  int
	height int
}

// NewScreen mounts every field in reg over t and opens dense navigation on the
// first visible section. Boundary problems are retained in Err because the
// observable constructor intentionally returns a concrete Screen rather than an
// error pair.
func NewScreen(t theme.Theme, reg Registry, d *Draft) Screen {
	s := Screen{
		th:         t,
		reg:        reg,
		draft:      d,
		navFocused: true,
		focusField: -1,
		confirm:    kit.NewConfirm(t, screenDiscardPrompt),
		overlay:    kit.NewOverlay(t),
	}
	if err := validateScreenBoundary(t, reg, d); err != nil {
		s.err = err
		return s
	}
	for _, section := range reg.Sections {
		for _, fld := range section.Fields {
			fld.mount(t)
			fld.blur()
		}
	}
	s.sections = reg.visibleSections(d)
	s.syncSection()
	s.ready = true
	return s
}

// Init returns the batched startup commands for every mounted field.
func (s Screen) Init() tea.Cmd {
	if s.err != nil {
		return nil
	}
	var cmds []tea.Cmd
	for _, section := range s.reg.Sections {
		for _, fld := range section.Fields {
			if cmd := fld.initCmd(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(cmds...)
}

// SetSize records the outer render region.
func (s *Screen) SetSize(width, height int) {
	if width < 0 {
		width = 0
	}
	if height < 0 {
		height = 0
	}
	s.width, s.height = width, height
	s.overlay.SetSize(width, height)
}

// Dirty reports whether any currently visible field differs from its baseline.
// It deliberately does not delegate to Draft.Dirty, which excludes transient
// values omitted from config YAML.
func (s Screen) Dirty() bool {
	return s.draft != nil && s.reg.dirty(s.draft)
}

// Err reports the latest boundary, validation, discard, or commit failure.
func (s Screen) Err() error { return s.err }

// Update dispatches terminal messages through the shared keymap. Navigation is
// focused first; enter or tab moves into the selected section, and tab returns
// to navigation after the final visible field.
func (s Screen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		s.SetSize(size.Width, size.Height)
		return s, nil
	}
	if !s.ready {
		return s, nil
	}
	if s.savePending {
		if saved, ok := msg.(SavedMsg); ok && saved.Draft() == s.draft && s.draft != nil {
			return s, tea.Quit
		}
		// Draft.Commit has completed and the parent has not yet handled the
		// matching completion. Freeze every field and overlay until then so the
		// committed state is also the state any post-commit effect observes.
		return s, nil
	}
	if _, ok := msg.(SavedMsg); ok {
		return s, nil
	}
	keyMsg, isKey := msg.(tea.KeyPressMsg)
	if !isKey {
		// Save completion stays frozen above, but a discard confirmation owns
		// keyboard input only. Component-owned async results must still reach
		// retained fields while the modal is visible.
		return s.forwardAsync(msg)
	}
	if s.confirming {
		return s.updateConfirm(msg)
	}
	if s.helping {
		if action, ok := keymap.Match(keymap.Default(), keyMsg, helpAvailability{}); ok {
			switch action {
			case keymap.ActionHelp, keymap.ActionBack:
				s.helping = false
			}
		}
		return s, nil
	}
	if keyMsg.Text != "" && s.focusedFieldCapturesPrintableInput() {
		return s.forwardFocused(msg)
	}

	action, matched := keymap.Match(keymap.Default(), keyMsg, s.availability())
	if matched {
		switch action {
		case keymap.ActionHelp:
			s.helping = true
			return s, nil
		case keymap.ActionBack, keymap.ActionQuit:
			s.confirming = true
			s.confirm = kit.NewConfirm(s.th, screenDiscardPrompt)
			s.confirm.Focus()
			return s, nil
		case keymap.ActionSave:
			if s.savePending {
				return s, nil
			}
			return s.save()
		case keymap.ActionUp:
			if s.navFocused {
				s.selectSection(s.section - 1)
				return s, nil
			}
		case keymap.ActionDown:
			if s.navFocused {
				s.selectSection(s.section + 1)
				return s, nil
			}
		case keymap.ActionConfirm:
			if s.navFocused {
				s.focusContent()
				return s, nil
			}
		case keymap.ActionNextField:
			s.nextField()
			return s, nil
		case keymap.ActionPrevField:
			s.previousField()
			return s, nil
		}
	}
	if s.navFocused {
		return s, nil
	}
	return s.forwardFocused(msg)
}

func (s Screen) save() (Screen, tea.Cmd) {
	s.err = nil
	s.reg.dropHiddenEdits(s.draft)
	s.recomputeSections()
	if err := s.reg.validateVisible(s.draft); err != nil {
		s.err = fmt.Errorf(
			"save config settings: visible field validation failed: %w.\n"+
				"what: at least one visible setting cannot be committed in its current state.\n"+
				"why: the field-specific validation reported the error above.\n"+
				"where: settings.Screen save validation.\n"+
				"when: after hidden edits were dropped and before Draft.Commit.\n"+
				"means: no config or external setting was written.\n"+
				"fix: correct the visible field named above, then press ctrl+s again.",
			err)
		return s, nil
	}
	if err := s.draft.Commit(); err != nil {
		s.err = fmt.Errorf(
			"save config settings to %q: %w.\n"+
				"what: the buffered configuration could not be committed.\n"+
				"why: the atomic Draft.Commit step reported the error above.\n"+
				"where: settings.Screen save commit.\n"+
				"when: after validation passed and before SavedMsg could be emitted.\n"+
				"means: no SavedMsg was sent and no post-commit external writer will run.\n"+
				"fix: follow the commit error guidance, reopen current disk state if needed, and retry ctrl+s.",
			s.draft.Path(), err)
		return s, nil
	}
	s.savePending = true
	draft := s.draft
	return s, func() tea.Msg { return SavedMsg{draft: draft} }
}

func (s Screen) updateConfirm(msg tea.Msg) (Screen, tea.Cmd) {
	var cmd tea.Cmd
	s.confirm, cmd = s.confirm.Update(msg)
	if cmd == nil {
		return s, nil
	}
	result, ok := runResult(cmd).(kit.ConfirmResultMsg)
	if !ok {
		return s, cmd
	}
	s.confirming = false
	if !result.OK {
		return s, nil
	}
	if err := s.draft.Discard(); err != nil {
		s.err = fmt.Errorf(
			"discard config edits for %q: %w.\n"+
				"what: the buffered settings could not be restored to their opening values.\n"+
				"why: copying the draft baseline failed.\n"+
				"where: settings.Screen discard confirmation.\n"+
				"when: after the user confirmed leaving without saving.\n"+
				"means: no config file was written, and the screen remains open.\n"+
				"fix: review the reported copy error, then retry discard or restart the config command.",
			s.draft.Path(), err)
		return s, nil
	}
	s.err = nil
	return s, tea.Quit
}

func (s *Screen) selectSection(index int) {
	if len(s.sections) == 0 {
		s.section = 0
		return
	}
	if index < 0 {
		index = 0
	}
	if index >= len(s.sections) {
		index = len(s.sections) - 1
	}
	if index == s.section {
		return
	}
	s.blurFields()
	s.section = index
	s.focusField = -1
	s.syncSection()
}

func (s *Screen) focusContent() {
	if len(s.sections) == 0 {
		return
	}
	s.navFocused = false
	s.focusField = s.firstInteractiveField()
	if s.focusField < 0 {
		s.navFocused = true
		return
	}
	s.sections[s.section].Fields[s.focusField].focus()
}

func (s *Screen) nextField() {
	if s.navFocused {
		s.focusContent()
		return
	}
	fields := s.currentInteractiveFields()
	for i, index := range fields {
		if index != s.focusField {
			continue
		}
		s.sections[s.section].Fields[index].blur()
		if i+1 < len(fields) {
			s.focusField = fields[i+1]
			s.sections[s.section].Fields[s.focusField].focus()
			return
		}
		s.navFocused = true
		s.focusField = -1
		return
	}
	s.navFocused = true
	s.focusField = -1
}

func (s *Screen) previousField() {
	if s.navFocused {
		fields := s.currentInteractiveFields()
		if len(fields) == 0 {
			return
		}
		s.navFocused = false
		s.focusField = fields[len(fields)-1]
		s.sections[s.section].Fields[s.focusField].focus()
		return
	}
	fields := s.currentInteractiveFields()
	for i, index := range fields {
		if index != s.focusField {
			continue
		}
		s.sections[s.section].Fields[index].blur()
		if i > 0 {
			s.focusField = fields[i-1]
			s.sections[s.section].Fields[s.focusField].focus()
			return
		}
		s.navFocused = true
		s.focusField = -1
		return
	}
}

func (s *Screen) syncSection() {
	if len(s.sections) == 0 || s.section < 0 || s.section >= len(s.sections) {
		return
	}
	for _, fld := range s.sections[s.section].Fields {
		fld.sync(s.draft)
		fld.blur()
	}
}

func (s *Screen) blurFields() {
	for _, section := range s.reg.Sections {
		for _, fld := range section.Fields {
			fld.blur()
		}
	}
}

func (s Screen) firstInteractiveField() int {
	fields := s.currentInteractiveFields()
	if len(fields) == 0 {
		return -1
	}
	return fields[0]
}

func (s Screen) currentInteractiveFields() []int {
	if len(s.sections) == 0 || s.section < 0 || s.section >= len(s.sections) {
		return nil
	}
	var out []int
	for i, fld := range s.sections[s.section].Fields {
		if fld.When(s.draft) && interactive(fld) {
			out = append(out, i)
		}
	}
	return out
}

func (s Screen) forwardFocused(msg tea.Msg) (Screen, tea.Cmd) {
	if len(s.sections) == 0 || s.focusField < 0 || s.focusField >= len(s.sections[s.section].Fields) {
		return s, nil
	}
	cmd := s.sections[s.section].Fields[s.focusField].handle(s.draft, msg)
	s.err = nil
	s.recomputeSections()
	return s, cmd
}

func (s Screen) forwardAsync(msg tea.Msg) (Screen, tea.Cmd) {
	cmds := fieldAsyncCommands(s.reg, s.draft, msg)
	s.recomputeSections()
	if len(cmds) == 0 {
		return s, nil
	}
	return s, tea.Batch(cmds...)
}

func (s Screen) focusedFieldCapturesPrintableInput() bool {
	if s.navFocused || len(s.sections) == 0 || s.section < 0 || s.section >= len(s.sections) ||
		s.focusField < 0 || s.focusField >= len(s.sections[s.section].Fields) {
		return false
	}
	return s.sections[s.section].Fields[s.focusField].capturesPrintableInput()
}

func (s *Screen) recomputeSections() {
	if s.draft == nil {
		return
	}
	key := ""
	if s.section >= 0 && s.section < len(s.sections) {
		key = s.sections[s.section].Key
	}
	s.sections = s.reg.visibleSections(s.draft)
	if len(s.sections) == 0 {
		s.section = 0
		s.focusField = -1
		s.navFocused = true
		return
	}
	for i, section := range s.sections {
		if section.Key == key {
			s.section = i
			return
		}
	}
	if s.section >= len(s.sections) {
		s.section = len(s.sections) - 1
	}
	s.focusField = -1
	s.navFocused = true
	s.syncSection()
}

func (s Screen) availability() screenAvailability { return screenAvailability{screen: &s} }

type screenAvailability struct{ screen *Screen }

func (a screenAvailability) AvailableActions() []keymap.ActionID {
	s := a.screen
	if s.confirming {
		return s.confirm.AvailableActions()
	}
	if s.navFocused {
		return []keymap.ActionID{
			keymap.ActionUp,
			keymap.ActionDown,
			keymap.ActionConfirm,
			keymap.ActionNextField,
			keymap.ActionPrevField,
			keymap.ActionSave,
			keymap.ActionBack,
			keymap.ActionQuit,
			keymap.ActionHelp,
		}
	}
	var out []keymap.ActionID
	if len(s.sections) > 0 && s.focusField >= 0 && s.focusField < len(s.sections[s.section].Fields) {
		out = append(out, s.sections[s.section].Fields[s.focusField].availableActions()...)
	}
	out = append(out,
		keymap.ActionNextField,
		keymap.ActionPrevField,
		keymap.ActionSave,
		keymap.ActionBack,
		keymap.ActionQuit,
		keymap.ActionHelp,
	)
	return effectiveAvailability(dedupeActions(out), s.focusedFieldCapturesPrintableInput())
}

// View renders the section jump list and selected section inside one kit Frame.
func (s Screen) View() string {
	if s.err != nil && !s.th.Mode.IsValid() {
		return s.err.Error()
	}
	frame := kit.NewFrame(s.th).WithTitle(screenTitle).WithFooter(
		keymap.FooterView(s.th, keymap.Default(), s.availability()),
	)
	frame.SetSize(s.width, s.height)
	frame.SetContent(s.body(frame.InnerWidth(), frame.InnerHeight()))
	base := frame.View()
	if s.helping {
		return s.overlay.Push(helpLayer{th: s.th, entries: keymap.HelpEntries(keymap.Default(), s.availability())}).View(base)
	}
	if s.confirming {
		s.confirm.SetSize(kit.ConfirmMinSize.Width, kit.ConfirmMinSize.Height)
		return s.overlay.Push(screenConfirmLayer{confirm: s.confirm}).View(base)
	}
	return base
}

func (s Screen) body(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	summary := s.renderSaveSummary(width)
	contentHeight := height - 1
	if contentHeight <= 0 {
		return summary
	}
	var content string
	if len(s.sections) == 0 {
		if s.err != nil {
			content = s.renderError(width)
		} else {
			content = s.th.Styles().Muted.Render(clip("no settings are currently available", width))
		}
	} else if width < screenMinNavWidth*2+1 {
		content = s.renderSection(width, contentHeight)
	} else {
		navWidth := width / 3
		if navWidth < screenMinNavWidth {
			navWidth = screenMinNavWidth
		}
		if navWidth > screenMaxNavWidth {
			navWidth = screenMaxNavWidth
		}
		bodyWidth := width - navWidth - 1
		nav := lipgloss.NewStyle().Width(navWidth).Height(contentHeight).MaxWidth(navWidth).MaxHeight(contentHeight).
			Render(s.renderNavigation(navWidth))
		divider := lipgloss.NewStyle().Width(1).Height(contentHeight).MaxHeight(contentHeight).
			Render(strings.TrimSuffix(strings.Repeat("│\n", contentHeight), "\n"))
		body := lipgloss.NewStyle().Width(bodyWidth).Height(contentHeight).MaxWidth(bodyWidth).MaxHeight(contentHeight).
			Render(s.renderSection(bodyWidth, contentHeight))
		content = lipgloss.JoinHorizontal(lipgloss.Top, nav, s.th.Styles().Muted.Render(divider), body)
	}
	return lipgloss.JoinVertical(lipgloss.Left, summary, content)
}

// renderSaveSummary is Screen-owned chrome, not a canonical registry field.
// It states the one save action the dense presentation actually dispatches.
func (s Screen) renderSaveSummary(width int) string {
	text := "ctrl+s saves visible settings"
	style := s.th.Styles().Muted
	if s.savePending {
		text = "saving settings; input is paused"
		style = s.th.Styles().Header
	} else if s.Dirty() {
		text = "visible changes are buffered; " + text
		style = s.th.Styles().Warning
	}
	return style.Render(clip(text, width))
}

func (s Screen) renderNavigation(width int) string {
	styles := s.th.Styles()
	lines := []string{styles.Header.Render(clip("sections", width))}
	for i, section := range s.sections {
		label := section.Title
		if label == "" {
			label = section.Key
		}
		if section.dirty(s.draft) {
			label += " *"
		}
		prefix := "  "
		style := styles.Muted
		if i == s.section {
			prefix = "▸ "
			if s.navFocused {
				style = styles.Selected
			} else {
				style = styles.Header
			}
		}
		lines = append(lines, style.Render(screenPad(prefix+clip(label, width-2), width)))
	}
	return strings.Join(lines, "\n")
}

func (s Screen) renderSection(width, height int) string {
	styles := s.th.Styles()
	section := s.sections[s.section]
	title := section.Title
	if title == "" {
		title = section.Key
	}
	if section.dirty(s.draft) {
		title += " [modified]"
	}
	lines := []string{styles.Header.Render(clip(title, width))}
	if s.err != nil {
		lines = append(lines, s.renderError(width), "")
	}
	used := len(lines)
	for _, fld := range section.visibleFields(s.draft) {
		if label := fld.Label(); label != "" && fld.Kind() != KindInfo && fld.Kind() != KindToggle {
			if fld.Dirty(s.draft) {
				label += " [modified]"
			}
			lines = append(lines, styles.Header.Render(clip(label, width)))
			used++
		}
		if desc := fld.Description(); desc != "" && fld.Kind() != KindInfo {
			lines = append(lines, styles.Muted.Render(clip(desc, width)))
			used++
		}
		available := height - used
		if available < 1 {
			available = 1
		}
		fld.setSize(width, available)
		rendered := fld.render(s.draft, styles, width)
		lines = append(lines, rendered)
		used += strings.Count(rendered, "\n") + 1
		if fld.Kind() == KindToggle && fld.Dirty(s.draft) {
			lines = append(lines, styles.Warning.Render(clip("[modified]", width)))
			used++
		}
	}
	return strings.Join(lines, "\n")
}

func (s Screen) renderError(width int) string {
	if s.err == nil {
		return ""
	}
	styles := s.th.Styles()
	lines := splitLines(s.err.Error())
	for i := range lines {
		lines[i] = styles.Danger.Render(clip(lines[i], width))
	}
	return strings.Join(lines, "\n")
}

type screenConfirmLayer struct{ confirm kit.Confirm }

func (l screenConfirmLayer) View() string { return l.confirm.View() }

func screenPad(value string, width int) string {
	if width <= 0 {
		return ""
	}
	value = clip(value, width)
	if padding := width - lipgloss.Width(value); padding > 0 {
		value += strings.Repeat(" ", padding)
	}
	return value
}

func validateScreenBoundary(t theme.Theme, reg Registry, d *Draft) error {
	if !t.Mode.IsValid() {
		return fmt.Errorf(
			"open config screen: rendering theme mode %q is invalid.\n"+
				"what: the screen cannot resolve token colors for this mode.\n"+
				"why: only theme.ModeDark and theme.ModeLight are renderable.\n"+
				"where: settings.NewScreen.\n"+
				"when: validating the screen before mounting fields.\n"+
				"means: no field was mounted and no draft state was changed.\n"+
				"fix: resolve the configured theme to dark or light, then construct the screen again.",
			t.Mode)
	}
	if d == nil {
		return fmt.Errorf(
			"open config screen: settings draft is nil.\n" +
				"what: the screen has no buffered configuration to display or save.\n" +
				"why: settings.NewDraft did not provide a draft.\n" +
				"where: settings.NewScreen.\n" +
				"when: validating the screen before mounting fields.\n" +
				"means: no field was mounted and no configuration can be committed.\n" +
				"fix: load the config, open a Draft, seed transient values, and retry.")
	}
	if d.Path() == "" {
		return fmt.Errorf(
			"open config screen: settings draft has no destination path.\n" +
				"what: the supplied draft is not a valid editing session.\n" +
				"why: it was not opened through settings.NewDraft.\n" +
				"where: settings.NewScreen.\n" +
				"when: validating the screen before mounting fields.\n" +
				"means: no field was mounted and no configuration can be committed.\n" +
				"fix: create the draft with a resolved config path and retry.")
	}
	if len(reg.Sections) == 0 {
		return fmt.Errorf(
			"open config screen for %q: registry has no sections.\n"+
				"what: there are no settings for the screen to present.\n"+
				"why: the canonical registry producer returned an empty registry.\n"+
				"where: settings.NewScreen.\n"+
				"when: validating the screen before mounting fields.\n"+
				"means: no field was mounted and no configuration can be committed.\n"+
				"fix: build the canonical settings registry and pass it unchanged to NewScreen.",
			d.Path())
	}
	sectionKeys := make(map[string]struct{}, len(reg.Sections))
	for sectionIndex, section := range reg.Sections {
		if section.Key == "" {
			return fmt.Errorf(
				"open config screen for %q: section %d has an empty key.\n"+
					"what: a settings section has no stable identity.\n"+
					"why: the registry producer omitted its Key.\n"+
					"where: settings.NewScreen registry validation.\n"+
					"when: validating the screen before mounting fields.\n"+
					"means: no field was mounted and navigation cannot address that section.\n"+
					"fix: assign every canonical section a non-empty unique key.",
				d.Path(), sectionIndex)
		}
		if _, duplicate := sectionKeys[section.Key]; duplicate {
			return fmt.Errorf(
				"open config screen for %q: section key %q is duplicated.\n"+
					"what: two settings sections share one navigation identity.\n"+
					"why: the registry producer reused a section Key.\n"+
					"where: settings.NewScreen registry validation.\n"+
					"when: validating the screen before mounting fields.\n"+
					"means: no field was mounted because navigation would be ambiguous.\n"+
					"fix: give every canonical section a unique key.",
				d.Path(), section.Key)
		}
		sectionKeys[section.Key] = struct{}{}
		fieldKeys := make(map[string]struct{}, len(section.Fields))
		for fieldIndex, fld := range section.Fields {
			if fld == nil {
				return fmt.Errorf(
					"open config screen for %q: section %q field %d is nil.\n"+
						"what: the registry contains a field with no implementation.\n"+
						"why: the registry producer appended a nil Field.\n"+
						"where: settings.NewScreen registry validation.\n"+
						"when: validating the screen before mounting fields.\n"+
						"means: no field was mounted because driving the nil field would panic.\n"+
						"fix: construct every field through a settings field constructor.",
					d.Path(), section.Key, fieldIndex)
			}
			if fld.Key() == "" || !fld.Kind().IsValid() {
				return fmt.Errorf(
					"open config screen for %q: section %q has invalid field key %q or kind %q.\n"+
						"what: a canonical field lacks a stable identity or supported shape.\n"+
						"why: the registry producer supplied an empty key or unknown FieldKind.\n"+
						"where: settings.NewScreen registry validation.\n"+
						"when: validating the screen before mounting fields.\n"+
						"means: no field was mounted because the screen cannot drive it safely.\n"+
						"fix: construct the field with a supported settings constructor and a non-empty key.",
					d.Path(), section.Key, fld.Key(), fld.Kind())
			}
			if _, duplicate := fieldKeys[fld.Key()]; duplicate {
				return fmt.Errorf(
					"open config screen for %q: field key %q is duplicated in section %q.\n"+
						"what: two fields share one stable identity within a section.\n"+
						"why: the registry producer reused a field Key.\n"+
						"where: settings.NewScreen registry validation.\n"+
						"when: validating the screen before mounting fields.\n"+
						"means: no field was mounted because dirty and focus state would be ambiguous.\n"+
						"fix: give every field in the section a unique key.",
					d.Path(), fld.Key(), section.Key)
			}
			fieldKeys[fld.Key()] = struct{}{}
		}
	}
	return nil
}
