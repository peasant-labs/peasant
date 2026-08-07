package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	schema "github.com/peasant-labs/schema"
)

// describedAnnotationType returns a single free-text ("described") annotation
// type so a test can drive straight into the textinput.Model step (step 3) of
// the editor — the only step where the bubbles v2 textinput cursor-split risk
// (flagged during the charm.land v2 re-pin) applies.
func describedAnnotationType() []schema.AnnotationTypeSummary {
	return []schema.AnnotationTypeSummary{
		{
			TypeID:      "note",
			DisplayName: "Note",
			ValueDomain: schema.ValueDomain{Kind: schema.DomainDescribed},
		},
	}
}

// typeRune returns the key event for typing a single printable rune, carrying
// both Code and Text the way a real terminal keypress does — textinput.Model's
// regular-character path reads Text, and String()-based key matching (used for
// the 1-9 annotation-type shortcuts) reads Code.
func typeRune(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

// TestAnnotationEditor_TextEntryAndCursorMovement drives the described-value
// text step through the model's real Update() loop — entering visible text,
// moving the cursor with left/right/home/end, and deleting with backspace —
// asserting the resulting text value AND cursor position after every step.
// This is the one bubbles v2 component ported to the split cursor/rendering
// textinput.Model API, and until this test it had zero coverage proving the
// port preserved editing behavior.
func TestAnnotationEditor_TextEntryAndCursorMovement(t *testing.T) {
	m := NewAnnotationEditor(describedAnnotationType(), nil)

	// Select the one described type (key "1") -> enters the text-input step.
	m, cmd := m.Update(typeRune('1'))
	if m.step != stepTextInput {
		t.Fatalf("after selecting the described type, step = %v, want stepTextInput", m.step)
	}
	if cmd == nil {
		t.Fatal("selecting a described type returned no command; expected textinput.Blink to focus the cursor")
	}
	if !m.textInput.Focused() {
		t.Fatal("text input is not focused after entering the text-input step")
	}

	// Type "hi": two regular-character keys, appended left to right.
	m, _ = m.Update(typeRune('h'))
	m, _ = m.Update(typeRune('i'))
	if got, want := m.textInput.Value(), "hi"; got != want {
		t.Fatalf("value after typing \"hi\" = %q, want %q", got, want)
	}
	if got, want := m.textInput.Position(), 2; got != want {
		t.Fatalf("cursor position after typing \"hi\" = %d, want %d (end of input)", got, want)
	}

	// Left: cursor steps back one rune without changing the value.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	if got, want := m.textInput.Position(), 1; got != want {
		t.Fatalf("cursor position after left = %d, want %d", got, want)
	}

	// Typing at a moved cursor inserts at that position, not at the end.
	m, _ = m.Update(typeRune('X'))
	if got, want := m.textInput.Value(), "hXi"; got != want {
		t.Fatalf("value after inserting mid-string = %q, want %q", got, want)
	}
	if got, want := m.textInput.Position(), 2; got != want {
		t.Fatalf("cursor position after inserting mid-string = %d, want %d", got, want)
	}

	// Home: cursor jumps to the start of the value.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	if got, want := m.textInput.Position(), 0; got != want {
		t.Fatalf("cursor position after home = %d, want %d", got, want)
	}

	// End: cursor jumps to the end of the value.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	if got, want := m.textInput.Position(), len([]rune(m.textInput.Value())); got != want {
		t.Fatalf("cursor position after end = %d, want %d (end of %q)", got, want, m.textInput.Value())
	}

	// Backspace: deletes the rune immediately before the cursor.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if got, want := m.textInput.Value(), "hX"; got != want {
		t.Fatalf("value after backspace = %q, want %q", got, want)
	}
	if got, want := m.textInput.Position(), 2; got != want {
		t.Fatalf("cursor position after backspace = %d, want %d", got, want)
	}

	// Right at the end of the value is a no-op: the cursor cannot move past it.
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if got, want := m.textInput.Position(), 2; got != want {
		t.Fatalf("cursor position after right at end = %d, want %d (should not move past the value)", got, want)
	}

	// Enter confirms the edited value and emits it via the picked command.
	m, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on the text step returned no command; expected an AnnotationPickedMsg to be emitted")
	}
	picked, ok := cmd().(AnnotationPickedMsg)
	if !ok {
		t.Fatalf("enter command produced %T, want AnnotationPickedMsg", cmd())
	}
	if got, want := picked.Value, "hX"; got != want {
		t.Fatalf("emitted annotation value = %q, want %q (the cursor-edited value, not the originally typed \"hi\")", got, want)
	}
	if got, want := picked.TypeID, "note"; got != want {
		t.Fatalf("emitted annotation type = %q, want %q", got, want)
	}
}
