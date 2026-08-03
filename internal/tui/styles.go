package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/peasant-labs/peasant/internal/defaults"
)

// bgANSISeq is the pre-computed ANSI SGR sequence that sets the base TUI
// background color.  It is derived from defaults.ColorBg at init time so it
// stays in sync with the palette.
var bgANSISeq = func() string {
	hex := defaults.ColorBg.String() // e.g. "#1a1b26"
	r, _ := strconv.ParseInt(hex[1:3], 16, 0)
	g, _ := strconv.ParseInt(hex[3:5], 16, 0)
	b, _ := strconv.ParseInt(hex[5:7], 16, 0)
	return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", r, g, b)
}()

// FillBg post-processes a fully-rendered string so that the base background
// color is re-established after every ANSI SGR reset (\x1b[0m).  Without
// this, gaps appear between styled fragments where the terminal's own
// background bleeds through.
func FillBg(s string) string {
	return strings.ReplaceAll(s, "\x1b[0m", "\x1b[0m"+bgANSISeq)
}

// baseBg is the shared background color for all TUI styles.
var baseBg = lipgloss.Color(defaults.ColorBg.String())

// TextBg is a minimal style that only sets the background; use it to wrap
// raw text fragments (spaces, newlines, unstyled values) so they don't
// punch holes in the dark background.
var TextBg = lipgloss.NewStyle().Background(baseBg)

var (
	// Base background style (for filling empty space in containers)
	ContentBg = lipgloss.NewStyle().
			Background(baseBg)

	// Tab bar styles
	ActiveTab = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(defaults.ColorBright.String())).
			Background(lipgloss.Color(defaults.ColorDark.String())).
			Padding(0, 2)

	InactiveTab = lipgloss.NewStyle().
			Foreground(lipgloss.Color(defaults.ColorDimText.String())).
			Background(baseBg).
			Padding(0, 2)

	// Card/box styles
	CardStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(defaults.ColorBorder.String())).
			Background(baseBg).
			Padding(1, 2)

	// Section container with rounded border
	SectionStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(defaults.ColorBorder.String())).
			Background(baseBg).
			Padding(0, 1)

	// Header style
	HeaderStyle = lipgloss.NewStyle().
			Bold(true).
			Background(baseBg).
			BorderStyle(lipgloss.NormalBorder()).
			BorderBottom(true)

	// Help text style
	HelpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color(defaults.ColorDimText.String())).
			Background(baseBg).
			Italic(true)

	// Status bar style
	StatusBarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color(defaults.ColorDimmer.String())).
			Background(lipgloss.Color(defaults.ColorDark.String())).
			Padding(0, 1)

	// Accent style for values
	AccentStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color(defaults.ColorFg.String())).
			Background(baseBg).
			Bold(true)

	// Dimmed label style
	DimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color(defaults.ColorDimText.String())).
			Background(baseBg)

	// Bright value style
	BrightStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color(defaults.ColorFg.String())).
			Background(baseBg).
			Bold(true)

	// Section header for trends
	SectionHeader = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(defaults.ColorFg.String())).
			Background(baseBg)

	// Section headers per metric group
	SectionHeaderCost = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color(defaults.ColorPastelPink.String())).
				Background(baseBg)

	SectionHeaderBehavioral = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color(defaults.ColorPastelLilac.String())).
				Background(baseBg)

	SectionHeaderQuality = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color(defaults.ColorPastelMint.String())).
				Background(baseBg)

	// Trend indicator styles
	TrendUpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color(defaults.ColorPastelMint.String())).
			Background(baseBg).
			Bold(true)

	TrendDownStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color(defaults.ColorPastelCoral.String())).
			Background(baseBg).
			Bold(true)

	TrendNeutralStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color(defaults.ColorDimText.String())).
				Background(baseBg)

	// Outcome badge styles
	OutcomeResolvedStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color(defaults.ColorPastelMint.String())).
				Background(baseBg).
				Bold(true)

	OutcomePartialStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color(defaults.ColorPastelLemon.String())).
				Background(baseBg).
				Bold(true)

	OutcomeFailedStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color(defaults.ColorPastelCoral.String())).
				Background(baseBg).
				Bold(true)
)
