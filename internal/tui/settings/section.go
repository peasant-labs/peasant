package settings

import "github.com/peasant-labs/peasant/internal/tui/theme"

// GuideExampleFunc derives one live, presentation-only example from the same
// theme and draft a guided flow is rendering. Returning an error is fail-closed:
// the flow renders the actionable error instead of omitting the example or
// substituting output that did not come from the real behavior being explained.
type GuideExampleFunc func(theme.Theme, *Draft) (string, error)

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
	Example GuideExampleFunc
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

// visibleFields returns the fields currently shown within s. Field-level When
// predicates are evaluated against the same draft as the section predicate.
func (s Section) visibleFields(d *Draft) []Field {
	var out []Field
	for _, fld := range s.Fields {
		if fld != nil && fld.When(d) {
			out = append(out, fld)
		}
	}
	return out
}

// dirty reports whether any currently visible field in s differs from its
// baseline. It deliberately delegates to Field.Dirty rather than Draft.Dirty
// so settings omitted from config YAML participate in presentation state.
func (s Section) dirty(d *Draft) bool {
	for _, fld := range s.visibleFields(d) {
		if fld.Dirty(d) {
			return true
		}
	}
	return false
}
