package settings

import (
	"fmt"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// ConsentSummary is the guided presentation's final explanation of the values
// about to be saved and the effects that confirming will run. Its lines are
// display-only; the Registry remains the sole owner of field behavior and the
// Flow remains the sole commit boundary.
type ConsentSummary struct {
	Values  []string
	Effects []string
}

// VisibleConsentField identifies one field that remains visible after the
// Registry's canonical hidden-edit convergence. Values are copied into the
// context, so a consent provider cannot change Registry visibility.
type VisibleConsentField struct {
	SectionKey string
	FieldKey   string
	Kind       FieldKind
}

// ConsentContext is the typed, read-only input to a guided receipt provider. It
// owns the canonical visible-field identities and exposes the Draft only as a
// detached config snapshot, so consent copy can describe exactly what the Flow
// will validate without gaining mutation authority over the buffered edit.
type ConsentContext struct {
	draft         *Draft
	visibleFields []VisibleConsentField
}

// VisibleFields returns a copy of the fields visible at the receipt.
func (c ConsentContext) VisibleFields() []VisibleConsentField {
	return append([]VisibleConsentField(nil), c.visibleFields...)
}

// HasVisibleField reports whether the canonical receipt contains fieldKey in
// sectionKey. Consent providers use this rather than duplicating When logic.
func (c ConsentContext) HasVisibleField(sectionKey, fieldKey string) bool {
	for _, field := range c.visibleFields {
		if field.SectionKey == sectionKey && field.FieldKey == fieldKey {
			return true
		}
	}
	return false
}

// Config returns a deep copy of the converged working configuration. Mutating
// the returned value cannot alter the Draft or what receipt confirmation saves.
func (c ConsentContext) Config() (config.Config, error) {
	if c.draft == nil {
		return config.Config{}, fmt.Errorf("read consent configuration: draft is nil")
	}
	working := c.draft.Working()
	snapshot, err := cloneConfig(working)
	if err != nil {
		return config.Config{}, err
	}
	// cloneConfig intentionally follows config.yaml and therefore omits the one
	// transient field. Consent must still describe that visible external effect,
	// so copy its scalar value into the otherwise deep-cloned snapshot.
	snapshot.ClaudeRetentionDays = working.ClaudeRetentionDays
	return snapshot, nil
}

// ConsentSummaryFunc derives consent copy from a canonical receipt context.
// Flow invokes it only after hidden-edit convergence, so both the typed field
// identities and detached Draft snapshot describe what a confirm will validate
// and commit.
type ConsentSummaryFunc func(ConsentContext) (ConsentSummary, error)

// receiptContinueCue is the bottom-of-review call to action. It is rendered in
// Styles.Accent - bold amber foreground text, not a full-width background
// fill - so the single available next step - confirm with enter - stays
// visually obvious without opening help, matching the always-on
// ActionConfirm binding, while keeping amber a scarce accent rather than a
// full-bleed bar across the line.
const receiptContinueCue = "▸ press enter to continue"

// renderReceipt draws the final review step: a per-section summary of which
// fields the draft changed, an explicit "no changes" when nothing is dirty, and
// the actionable error from a blocked commit when one is present. It reads the
// CURRENT draft, which has already had any hidden step's edits dropped, so the
// receipt reflects exactly what a confirm will persist.
//
// Content is grouped under bold headings ("your choices", "when you confirm",
// one heading per changed section) with a blank line between groups, rather
// than one flat run of lines, so the review can be scanned instead of read
// line-by-line. A styled continue cue always closes the render so the primary
// action - press enter - stays obvious even when the groups above run long.
//
// The receipt is one kit.Panel. The panel fits every row to one shared width
// and paints the page background behind each cell, so the review reads as a
// single block instead of lines of different background widths.
func (f Flow) renderReceipt(styles theme.Styles, width int) string {
	panel := kit.NewPanel(f.th)
	panel.SetSize(width, 0)
	panel.Line(styles.Header, "review your changes")
	panel.Blank()
	if f.consent != nil {
		summary, err := f.consent(f.reg.consentContext(f.draft))
		if err != nil {
			for _, line := range splitLines(err.Error()) {
				panel.Line(styles.Danger, line)
			}
			panel.Blank()
		} else {
			if len(summary.Values) > 0 {
				panel.Line(styles.Header, "your choices")
				for _, value := range summary.Values {
					panel.Line(styles.Muted, "  • "+value)
				}
				panel.Blank()
			}
			if len(summary.Effects) > 0 {
				panel.Line(styles.Header, "when you confirm")
				for _, effect := range summary.Effects {
					panel.Line(styles.Muted, "  • "+effect)
				}
				panel.Blank()
			}
		}
	}

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
		panel.Line(styles.Header, title)
		for _, fld := range changed {
			lbl := fld.Label()
			if lbl == "" {
				lbl = fld.Key()
			}
			panel.Line(styles.Muted, "  • changed: "+lbl)
		}
		panel.Blank()
	}
	if !anyDirty {
		panel.Line(styles.Muted, "no changes to save")
		panel.Blank()
	}

	if f.err != nil {
		for _, ln := range splitLines(f.err.Error()) {
			panel.Line(styles.Danger, ln)
		}
		panel.Blank()
	}

	panel.Line(styles.Accent, receiptContinueCue)
	return panel.View()
}
