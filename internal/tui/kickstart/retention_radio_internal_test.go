package kickstart

import (
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// TestRetentionRadio_SelectionWritesDraft drives the real retention radio and
// proves a selection reaches the draft through retentionAccessor().Set — the
// field->draft write path. It selects "90 days" (index 1 of 30/90/365/never),
// which no fallback or default produces, so a broken accessor cannot pass.
//
// It complements TestProgram_RetentionFieldChoiceReachesWriter, which proves the
// committed-draft value reaches the writer (the draft->writer path) but seeds the
// draft directly and so does not exercise the accessor.
func TestRetentionRadio_SelectionWritesDraft(t *testing.T) {
	// A registry holding ONLY the retention section, so the radio is the single
	// active field and no step-order counting is needed.
	full := BuildRegistry(Options{
		Source:                scannerfix.NewFixtureTreeSource("standard"),
		ClaudeSessionsPresent: true,
	})
	var retentionSection settings.Section
	found := false
	for _, s := range full.Sections {
		if s.Key == SectionRetention {
			retentionSection = s
			found = true
		}
	}
	if !found {
		t.Fatalf("registry has no %q section", SectionRetention)
	}
	reg := settings.Registry{Sections: []settings.Section{retentionSection}}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(path, config.BaseConfig()); err != nil {
		t.Fatalf("seed base config: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	loaded, err := config.Parse(raw)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	d, err := settings.NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open draft: %v", err)
	}
	if d.Working().ClaudeRetentionDays != 0 {
		t.Fatalf("precondition: base ClaudeRetentionDays = %d, want 0", d.Working().ClaudeRetentionDays)
	}

	f := settings.NewFlow(theme.New(theme.ModeDark), reg, d)
	f.SetSize(80, 20)

	up := tea.KeyPressMsg{Code: tea.KeyUp}
	down := tea.KeyPressMsg{Code: tea.KeyDown}
	space := tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}

	// Clamp the radio cursor to the first option, then step down once to "90 days"
	// and commit that option (space = ActionToggle -> accessor.Set).
	for range 3 {
		f, _ = f.Update(up)
	}
	f, _ = f.Update(down)
	f, _ = f.Update(space)

	if got := d.Working().ClaudeRetentionDays; got != 90 {
		t.Fatalf("retention radio selection wrote %d to the draft, want 90 (accessor.Set not exercised?)", got)
	}
}
