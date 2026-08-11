package settings

// GuideExampleLineKind identifies the semantic presentation role of one plain
// guide-example line. The Flow, not the example provider, owns styling so a
// producer cannot bake terminal escapes into content or substitute an
// inaccessible color-only distinction for the before/after vocabulary.
type GuideExampleLineKind uint8

const (
	// GuideExampleLineUnknown is the invalid zero value.
	GuideExampleLineUnknown GuideExampleLineKind = iota
	// GuideExampleLineText is ordinary example prose on the guide surface.
	GuideExampleLineText
	// GuideExampleLineLabel is a short heading such as a canonical privacy
	// category label.
	GuideExampleLineLabel
	// GuideExampleLineBefore is removed input. Flow adds the visible
	// "- before: " prefix and applies the deletion token.
	GuideExampleLineBefore
	// GuideExampleLineAfter is replacement output. Flow adds the visible
	// "+ after: " prefix and applies the addition token.
	GuideExampleLineAfter
	// GuideExampleLineSpacer is one blank guide-surface row between groups.
	GuideExampleLineSpacer
)

// IsValid reports whether k names a supported guide-example role.
func (k GuideExampleLineKind) IsValid() bool {
	switch k {
	case GuideExampleLineText, GuideExampleLineLabel, GuideExampleLineBefore,
		GuideExampleLineAfter, GuideExampleLineSpacer:
		return true
	default:
		return false
	}
}

// String returns the stable lowercase name of k, or "unknown".
func (k GuideExampleLineKind) String() string {
	switch k {
	case GuideExampleLineText:
		return "text"
	case GuideExampleLineLabel:
		return "label"
	case GuideExampleLineBefore:
		return "before"
	case GuideExampleLineAfter:
		return "after"
	case GuideExampleLineSpacer:
		return "spacer"
	default:
		return "unknown"
	}
}

// GuideExampleLine is one plain-text, semantically typed row returned by a
// [GuideExampleFunc]. Text must be one unrendered terminal line. Spacer rows
// carry an empty Text value; every other kind carries non-empty text.
type GuideExampleLine struct {
	Kind GuideExampleLineKind
	Text string
}

// GuideExampleFunc derives one live, presentation-only example from the same
// draft a guided flow is rendering. Returning an error is fail-closed:
// the flow withholds the unverified example and renders the actionable error
// instead of substituting output that did not come from the real behavior being
// explained. The provider returns semantic plain-text lines; Flow owns all
// glyph prefixes, colors, and full-line fitting. The error does not make the
// section's canonical field unavailable.
type GuideExampleFunc func(*Draft) ([]GuideExampleLine, error)

// Guide is optional presentation-neutral framing for a [Section]. A guided
// presentation renders it before the first field's description and control,
// following that field's heading when it has one. Denser presentations may
// ignore it. Guide metadata never changes field identity, visibility,
// validation, or persistence.
type Guide struct {
	// Intro briefly explains the section's purpose.
	Intro string
	// Hints provide short supporting guidance in display order.
	Hints []string
	// Example optionally derives semantically typed example lines from the
	// current draft.
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
