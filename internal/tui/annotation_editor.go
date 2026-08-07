package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/peasant-labs/peasant/internal/defaults"
	schema "github.com/peasant-labs/schema"
)

// AnnotationPickedMsg is emitted when the user selects an annotation type and value.
// EntryIdx is non-nil for entry-level annotations (turn-specific); nil for session-level.
type AnnotationPickedMsg struct {
	TypeID   string
	TypeName string
	Value    string
	EntryIdx *int
}

// AnnotationEditorCancelMsg is emitted when the user presses Esc to close the editor.
type AnnotationEditorCancelMsg struct{}

// editorStep tracks which step of the editor the user is on.
type editorStep int

const (
	stepTypeSelection  editorStep = iota // picking annotation type (1-9)
	stepValueSelection                   // picking enumerated value (1-9)
	stepTextInput                        // typing described value (free text)
)

// AnnotationEditorModel is the Bubble Tea sub-model for the annotation type picker modal.
// Triggered by pressing 'a' in the session detail view. Two steps:
//  1. Type selection: displays up to 9 annotation types with numbered shortcuts (1–9).
//  2. Value selection: for enumerated types, shows allowed values (1–9). For described, shows text input.
type AnnotationEditorModel struct {
	types    []schema.AnnotationTypeSummary
	entryIdx *int // nil for session-level; set when a turn is marked
	width    int

	step         editorStep
	selectedType *schema.AnnotationTypeSummary // set after step 1
	textInput    textinput.Model               // for described value domains
}

// NewAnnotationEditor creates an annotation type picker modal.
// entryIdx should be nil for session-level annotations and non-nil for entry-level.
func NewAnnotationEditor(types []schema.AnnotationTypeSummary, entryIdx *int) AnnotationEditorModel {
	ti := textinput.New()
	ti.Placeholder = "type value and press Enter"
	ti.CharLimit = 256

	return AnnotationEditorModel{
		types:     types,
		entryIdx:  entryIdx,
		step:      stepTypeSelection,
		textInput: ti,
	}
}

// Init satisfies the tea.Model interface (sub-model pattern).
func (m AnnotationEditorModel) Init() tea.Cmd { return nil }

// Update handles key events in the annotation editor.
func (m AnnotationEditorModel) Update(msg tea.Msg) (AnnotationEditorModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width

	case tea.KeyPressMsg:
		switch msg.String() {
		case defaults.KeyEscape.String():
			return m, func() tea.Msg { return AnnotationEditorCancelMsg{} }
		}

		switch m.step {
		case stepTypeSelection:
			return m.updateTypeSelection(msg)
		case stepValueSelection:
			return m.updateValueSelection(msg)
		case stepTextInput:
			return m.updateTextInput(msg)
		}
	}
	return m, nil
}

// updateTypeSelection handles 1-9 key presses to select an annotation type.
func (m AnnotationEditorModel) updateTypeSelection(msg tea.KeyPressMsg) (AnnotationEditorModel, tea.Cmd) {
	if len(msg.String()) == 1 {
		ch := msg.String()[0]
		if ch >= '1' && ch <= '9' {
			idx := int(ch - '1')
			if idx < len(m.types) {
				t := m.types[idx]
				m.selectedType = &t

				switch t.ValueDomain.Kind {
				case schema.DomainEnumerated:
					vals := t.ValueDomain.PermissibleValues
					if len(vals) == 1 {
						// Auto-select the only value.
						return m, m.emitPicked(vals[0])
					}
					m.step = stepValueSelection
					return m, nil

				case schema.DomainDescribed:
					m.step = stepTextInput
					m.textInput.Focus()
					return m, textinput.Blink

				default:
					// Unknown domain kind — emit with empty value (backend will validate).
					return m, m.emitPicked("")
				}
			}
		}
	}
	return m, nil
}

// updateValueSelection handles 1-9 key presses to select an enumerated value.
func (m AnnotationEditorModel) updateValueSelection(msg tea.KeyPressMsg) (AnnotationEditorModel, tea.Cmd) {
	if m.selectedType == nil {
		return m, nil
	}
	vals := m.selectedType.ValueDomain.PermissibleValues
	if len(msg.String()) == 1 {
		ch := msg.String()[0]
		if ch >= '1' && ch <= '9' {
			idx := int(ch - '1')
			if idx < len(vals) {
				return m, m.emitPicked(vals[idx])
			}
		}
	}
	return m, nil
}

// updateTextInput handles text input for described value domains.
func (m AnnotationEditorModel) updateTextInput(msg tea.KeyPressMsg) (AnnotationEditorModel, tea.Cmd) {
	if msg.Code == tea.KeyEnter {
		value := strings.TrimSpace(m.textInput.Value())
		if value != "" {
			return m, m.emitPicked(value)
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.textInput, cmd = m.textInput.Update(msg)
	return m, cmd
}

// emitPicked returns a Cmd that emits AnnotationPickedMsg with the selected type and value.
func (m AnnotationEditorModel) emitPicked(value string) tea.Cmd {
	t := m.selectedType
	entryIdx := m.entryIdx
	return func() tea.Msg {
		return AnnotationPickedMsg{
			TypeID:   t.TypeID,
			TypeName: t.DisplayName,
			Value:    value,
			EntryIdx: entryIdx,
		}
	}
}

// View renders the annotation editor modal.
func (m AnnotationEditorModel) View() string {
	modalStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(defaults.ColorPastelLilac.String())).
		Background(baseBg).
		Padding(1, 2)

	var lines []string

	switch m.step {
	case stepTypeSelection:
		lines = append(lines, AccentStyle.Render("Annotation Type Picker (1–9: select, Esc: cancel)"))
		lines = append(lines, "")

		if len(m.types) == 0 {
			lines = append(lines, DimStyle.Render("No annotation types available"))
		} else {
			for i, t := range m.types {
				if i >= 9 {
					break // only 9 numbered shortcuts
				}
				num := fmt.Sprintf("%d", i+1)
				label := fmt.Sprintf("[%s] %s", AccentStyle.Render(num), t.DisplayName)
				if t.Description != "" {
					label += "  " + DimStyle.Render("— "+t.Description)
				}
				lines = append(lines, label)
			}
			if len(m.types) > 9 {
				lines = append(lines, DimStyle.Render(fmt.Sprintf("  … and %d more (use Web UI for full list)", len(m.types)-9)))
			}
		}

	case stepValueSelection:
		if m.selectedType != nil {
			lines = append(lines, AccentStyle.Render(
				fmt.Sprintf("Select value for %s (1–9: select, Esc: cancel)", m.selectedType.DisplayName),
			))
			lines = append(lines, "")
			for i, v := range m.selectedType.ValueDomain.PermissibleValues {
				if i >= 9 {
					break
				}
				num := fmt.Sprintf("%d", i+1)
				lines = append(lines, fmt.Sprintf("[%s] %s", AccentStyle.Render(num), v))
			}
		}

	case stepTextInput:
		if m.selectedType != nil {
			lines = append(lines, AccentStyle.Render(
				fmt.Sprintf("Enter value for %s (Enter: confirm, Esc: cancel)", m.selectedType.DisplayName),
			))
			lines = append(lines, "")
			lines = append(lines, m.textInput.View())
		}
	}

	return modalStyle.Render(strings.Join(lines, "\n"))
}
