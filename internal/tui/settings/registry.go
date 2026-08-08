package settings

// Registry is the ordered set of [Section]s a settings presentation mounts. It
// owns canonical visibility, hidden-edit convergence, and visible validation;
// Flow and Screen own only how users navigate and when they request a commit.
type Registry struct {
	// Sections are mounted in presentation order.
	Sections []Section
}

// visibleSections returns the sections shown for d's current draft, in order.
// A presentation re-derives this on every (re-)entry so a section whose When
// became false after a changed earlier answer disappears, and one whose When
// became true reappears.
func (r Registry) visibleSections(d *Draft) []Section {
	var out []Section
	for _, s := range r.Sections {
		if s.visible(d) {
			out = append(out, s)
		}
	}
	return out
}

// dirty reports whether any currently visible section has a visible dirty
// field. Draft.Dirty is intentionally not consulted because transient fields
// can be omitted from config YAML while still representing a real user edit.
func (r Registry) dirty(d *Draft) bool {
	for _, s := range r.visibleSections(d) {
		if s.dirty(d) {
			return true
		}
	}
	return false
}

// hiddenFields returns every field that must be reset before commit: all
// fields in hidden sections plus field-level conditional controls that are
// hidden inside an otherwise visible section.
func (r Registry) hiddenFields(d *Draft) []Field {
	var out []Field
	for _, s := range r.Sections {
		if !s.visible(d) {
			for _, fld := range s.Fields {
				if fld != nil {
					out = append(out, fld)
				}
			}
			continue
		}
		for _, fld := range s.Fields {
			if fld != nil && !fld.When(d) {
				out = append(out, fld)
			}
		}
	}
	return out
}

// dropHiddenEdits restores hidden fields to baseline. It iterates to a
// fixpoint because resetting one conditional value can hide another field.
// Field.Dirty is the change detector so transient, YAML-omitted values are not
// accidentally retained.
func (r Registry) dropHiddenEdits(d *Draft) {
	for i := 0; i < r.fieldCount(); i++ {
		changed := false
		for _, fld := range r.hiddenFields(d) {
			if fld.Dirty(d) {
				changed = true
			}
			fld.reset(d)
		}
		if !changed {
			return
		}
	}
}

// fieldCount returns the maximum number of distinct field resets needed to
// converge hidden edits. Visibility can cascade between fields within one
// section, so a section-count bound is too short for the field-level contract.
func (r Registry) fieldCount() int {
	count := 0
	for _, section := range r.Sections {
		for _, fld := range section.Fields {
			if fld != nil {
				count++
			}
		}
	}
	return count
}

// validateVisible validates exactly the fields the user can currently see.
func (r Registry) validateVisible(d *Draft) error {
	for _, s := range r.visibleSections(d) {
		for _, fld := range s.visibleFields(d) {
			if err := fld.Validate(d); err != nil {
				return err
			}
		}
	}
	return nil
}
