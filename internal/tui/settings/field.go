package settings

import (
	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// FieldKind is the closed set of settings field shapes. It is an enum, never a
// stringly-typed tag: a presentation switches on it to draw the right chrome,
// and the leading Unknown sentinel makes a zero-valued kind fail IsValid
// rather than silently aliasing a real kind - the same leading-Unknown
// convention keymap.ActionID and the ingest enums use.
type FieldKind int

const (
	// KindUnknown is the zero value and never names a real field.
	KindUnknown FieldKind = iota
	// KindToggle is a single boolean control.
	KindToggle
	// KindRadio is a single-choice control over static options.
	KindRadio
	// KindMultiSelect is a multi-choice control over static options.
	KindMultiSelect
	// KindText is a free-text control.
	KindText
	// KindTree is the tri-state selection tree, fed by a kit.TreeSource - the
	// one asynchronous field-data source in the vocabulary.
	KindTree
	// KindInfo is a read-only, non-interactive rendered panel.
	KindInfo
)

// IsValid reports whether k names a real field kind.
func (k FieldKind) IsValid() bool {
	switch k {
	case KindToggle, KindRadio, KindMultiSelect, KindText, KindTree, KindInfo:
		return true
	}
	return false
}

// String returns a stable lower-case name for k, or "unknown".
func (k FieldKind) String() string {
	switch k {
	case KindToggle:
		return "toggle"
	case KindRadio:
		return "radio"
	case KindMultiSelect:
		return "multiselect"
	case KindText:
		return "text"
	case KindTree:
		return "tree"
	case KindInfo:
		return "info"
	default:
		return "unknown"
	}
}

// Field is one authored setting. The exported surface is what a [Section],
// [Registry], receipt, and validation read: identity (Key), shape (Kind),
// display (Label), whether the user has changed it (Dirty), whether its value
// is committable (Validate), and whether it should be shown for the current
// draft (When). The lower-case hooks are how a presentation ([Flow]) drives
// the field's live component - they are unexported because only a presentation
// in this package calls them; a caller only ever composes Fields into a
// Registry.
type Field interface {
	// Key is the field's stable identifier, unique within its Section.
	Key() string
	// Kind is the field's shape.
	Kind() FieldKind
	// Label is the human-readable prompt drawn beside the control.
	Label() string
	// Description is an optional plain-language line drawn under the label to
	// explain what the field controls. An empty string draws no line.
	Description() string
	// Validate reports whether the field's current value in d is committable,
	// returning an actionable error (what/why/where/fix) that blocks the
	// atomic commit when it is not.
	Validate(d *Draft) error
	// Dirty reports whether the field's value in d's working copy differs from
	// d's baseline.
	Dirty(d *Draft) bool
	// When reports whether the field should be shown for d's current draft.
	When(d *Draft) bool

	// mount injects the theme (and builds any live component) before the field
	// is first driven. Constructors take no theme so a Registry is
	// theme-agnostic; the Flow mounts every field at NewFlow.
	mount(t theme.Theme)
	// initCmd is the command to start any asynchronous work the field needs
	// (the selection tree's scan); nil for a field with none.
	initCmd() tea.Cmd
	// focus/blur/setSize/availableActions/handle/render/reset are the
	// presentation hooks a Flow uses to mount and drive the field.
	focus() tea.Cmd
	blur()
	setSize(width, height int)
	availableActions() []keymap.ActionID
	// capturesPrintableInput reports whether the currently-focused component is
	// editing free text. Flow consults it only for key messages whose Text is
	// non-empty, before resolving global printable actions such as q, b, and ?.
	// Lifecycle keys (escape, enter, tab, arrows) remain typed keymap actions.
	capturesPrintableInput() bool
	// sync pulls d's current value into the field's live component (used on
	// (re-)entry to a step so the component reflects the draft, including after
	// an edit was dropped).
	sync(d *Draft)
	// handle dispatches one synchronous key message to the field's live
	// component and writes any resulting value change back into d. Flow and
	// Screen route every non-key message exclusively through the private
	// asyncField capability instead of this general field hook.
	handle(d *Draft, msg tea.Msg) tea.Cmd
	// render draws the field's control at the given inner width.
	render(d *Draft, styles theme.Styles, width int) string
	// reset drops the field's edits by writing d's baseline value back into its
	// working copy.
	reset(d *Draft)
}

// baseField carries the identity every concrete field shares. Concrete fields
// embed it and add their accessor + live component.
type baseField struct {
	key   string
	label string
	desc  string
	when  func(d *Draft) bool
}

func (b baseField) Key() string                  { return b.key }
func (b baseField) Label() string                { return b.label }
func (b baseField) Description() string          { return b.desc }
func (b baseField) capturesPrintableInput() bool { return false }

func (b baseField) When(d *Draft) bool {
	if b.when == nil {
		return true
	}
	return b.when(d)
}

// setDesc writes the field's description. The pointer receiver makes every
// concrete field (which embeds baseField and is always used as a pointer)
// satisfy [describable], so [WithDescription] can set the line after
// construction without widening every field constructor's signature.
func (b *baseField) setDesc(s string) { b.desc = s }

// describable is the private hook [WithDescription] sets a field's line
// through. Every concrete field satisfies it via its embedded baseField.
type describable interface {
	setDesc(string)
}

// WithDescription sets f's always-shown description line and returns f, so a
// registry can annotate a field inline:
//
//	settings.WithDescription(
//		settings.Toggle(key, label, acc),
//		"one short line on what turning it on does.")
func WithDescription(f Field, desc string) Field {
	if d, ok := f.(describable); ok {
		d.setDesc(desc)
	}
	return f
}
