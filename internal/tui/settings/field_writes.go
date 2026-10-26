package settings

import "github.com/peasant-labs/peasant/internal/config"

// FieldWrites shows what each field of r writes into a configuration, so a
// caller can learn which configuration keys the registry edits from the
// registry itself rather than from a second list.
//
// For every field, in presentation order, it builds a fresh baseline and a
// fresh working configuration, lets the field copy its value from the baseline
// into the working copy (the same write that drops an edit), and calls visit
// with the working copy. A key the field writes now holds the baseline's value;
// every other key still holds the working value. The probe mounts nothing,
// reads no file, and ignores When, so a field a draft would hide still counts.
func (r Registry) FieldWrites(newBaseline, newWorking func() config.Config, visit func(field Field, written *config.Config)) {
	for _, section := range r.Sections {
		for _, field := range section.Fields {
			if field == nil {
				continue
			}
			draft := &Draft{baseline: newBaseline(), working: newWorking()}
			field.reset(draft)
			visit(field, &draft.working)
		}
	}
}
