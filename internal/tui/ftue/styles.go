package ftue

import (
	"charm.land/lipgloss/v2"
	"github.com/peasant-labs/peasant/internal/defaults"
)

// baseBg is the shared background color for all FTUE styles.
var baseBg = lipgloss.Color(defaults.ColorBg.String())

// TextBg is a minimal style that only sets the background; use it to wrap
// raw text fragments (spaces, newlines, unstyled values) so they don't
// punch holes in the dark background.
var TextBg = lipgloss.NewStyle().Background(baseBg)

var (
	// Page title: primary text, bold, centered
	PageTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(defaults.ColorFg.String())).
			Background(baseBg).
			Align(lipgloss.Center)

	// Selected option: primary text, bold
	OptionSelected = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(defaults.ColorFg.String())).
			Background(baseBg)

	// Unselected option: dim text foreground
	OptionUnselected = lipgloss.NewStyle().
				Foreground(lipgloss.Color(defaults.ColorDimText.String())).
				Background(baseBg)

	// Cursor indicator style
	OptionCursor = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(defaults.ColorFg.String())).
			Background(baseBg)

	// Checkbox checked: primary text
	CheckboxChecked = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(defaults.ColorFg.String())).
			Background(baseBg)

	// Checkbox unchecked: dim text
	CheckboxUnchecked = lipgloss.NewStyle().
				Foreground(lipgloss.Color(defaults.ColorDimText.String())).
				Background(baseBg)

	// Checkbox conflicted: selected, but the saved configuration disagrees
	// about it. Warm accent so the mark reads as "attention", not "error" —
	// the session IS selected, it just cannot be ingested as configured.
	CheckboxConflict = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color(defaults.ColorPastelLemon.String())).
				Background(baseBg)

	// Help bar: dim text, italic
	HelpBar = lipgloss.NewStyle().
		Foreground(lipgloss.Color(defaults.ColorDimText.String())).
		Background(baseBg).
		Italic(true)

	// Wizard border: rounded border with dark background
	WizardBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(defaults.ColorBorder.String())).
			Background(baseBg).
			Padding(1, 2)

	// Progress indicator
	ProgressBar = lipgloss.NewStyle().
			Foreground(lipgloss.Color(defaults.ColorPastelLilac.String())).
			Background(baseBg).
			Bold(true)

	// Welcome banner: primary text, bold
	WelcomeBanner = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(defaults.ColorFg.String())).
			Background(baseBg)

	// Description style: primary text for readability on dark background
	DescriptionStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color(defaults.ColorFg.String())).
				Background(baseBg)
)
