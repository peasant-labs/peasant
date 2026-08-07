package settings

import (
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// renderReceipt draws the final review step: a per-section summary of which
// fields the draft changed, an explicit "no changes" when nothing is dirty, and
// the actionable error from a blocked commit when one is present. It reads the
// CURRENT draft, which has already had any hidden step's edits dropped, so the
// receipt reflects exactly what a confirm will persist.
func (f Flow) renderReceipt(styles theme.Styles, width int) string {
	var lines []string
	lines = append(lines, styles.Header.Render(clip("review your changes", width)))

	anyDirty := false
	for _, s := range f.reg.visibleSections(f.draft) {
		var changed []Field
		for _, fld := range s.Fields {
			if fld.Dirty(f.draft) {
				changed = append(changed, fld)
			}
		}
		if len(changed) == 0 {
			continue
		}
		anyDirty = true
		title := s.Title
		if title == "" {
			title = s.Key
		}
		lines = append(lines, styles.Base.Render(clip(title, width)))
		for _, fld := range changed {
			lbl := fld.Label()
			if lbl == "" {
				lbl = fld.Key()
			}
			lines = append(lines, styles.Muted.Render(clip("  changed: "+lbl, width)))
		}
	}
	if !anyDirty {
		lines = append(lines, styles.Muted.Render(clip("no changes to save", width)))
	}

	if f.err != nil {
		for _, ln := range splitLines(f.err.Error()) {
			lines = append(lines, styles.Danger.Render(clip(ln, width)))
		}
	}
	return joinLines(lines)
}
