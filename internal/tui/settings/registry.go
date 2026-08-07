package settings

// Registry is the ordered set of [Section]s a settings [Flow] presents. It is
// the declarative contract the kickstart composes and hands to a Flow: the
// kickstart declares which sections and fields exist, and the Flow owns how
// they are stepped, validated, and committed.
type Registry struct {
	// Sections are the flow's steps, in presentation order.
	Sections []Section
}

// visibleSections returns the sections shown for d's current draft, in order.
// The Flow re-derives this on every (re-)entry so a section whose When became
// false after a changed earlier answer disappears from the step sequence, and
// one whose When became true reappears.
func (r Registry) visibleSections(d *Draft) []Section {
	var out []Section
	for _, s := range r.Sections {
		if s.visible(d) {
			out = append(out, s)
		}
	}
	return out
}

// hiddenSections returns the sections NOT shown for d - the ones whose edits a
// commit must drop.
func (r Registry) hiddenSections(d *Draft) []Section {
	var out []Section
	for _, s := range r.Sections {
		if !s.visible(d) {
			out = append(out, s)
		}
	}
	return out
}
