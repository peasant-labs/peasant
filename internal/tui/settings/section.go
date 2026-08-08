package settings

import "github.com/peasant-labs/peasant/internal/tui/theme"

// Guide is optional presentation-neutral framing for a [Section]. A guided
// presentation may render it before the section's fields, while denser
// presentations may ignore it. Guide metadata never changes field identity,
// visibility, validation, or persistence.
type Guide struct {
	// Intro briefly explains the section's purpose.
	Intro string
	// Hints provide short supporting guidance in display order.
	Hints []string
	// Example optionally derives a themed example from the current draft.
	Example func(theme.Theme, *Draft) string
}

// Section is one step of a settings [Flow]: a titled group of [Field]s, shown
// only when its When predicate holds for the current draft. A nil When means
// the section is always shown. Sections are the unit the Flow steps through and
// the unit whose edits are dropped when a changed earlier answer hides it.
type Section struct {
	// Key is the section's stable identifier, unique within a Registry.
	Key string
	// Title is the step heading.
	Title string
	// Fields are the section's ordered fields.
	Fields []Field
	// When reports whether the section is shown for the current draft. Nil
	// means always shown.
	When func(d *Draft) bool
	// Guide optionally frames this section for guided presentations.
	Guide *Guide
}

// visible reports whether the section is shown for d.
func (s Section) visible(d *Draft) bool {
	if s.When == nil {
		return true
	}
	return s.When(d)
}
