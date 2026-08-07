package defaults

// Color is a typed TUI color value (hex true-color).
type Color string

func (c Color) String() string { return string(c) }

// TUI color palette — Tokyo Night dark theme.
const (
	// Base palette — Tokyo Night Storm
	ColorBg     Color = "#1a1b26" // Storm background
	ColorBorder Color = "#3b4261" // Border / subtle accent
	ColorBright Color = "#c0caf5" // Primary foreground (light blue-white)
	ColorDim    Color = "#565f89" // Comment / muted
	ColorDimmer Color = "#414868" // Even more muted
	ColorDark   Color = "#16161e" // Night background (darkest)

	// Accent palette — Tokyo Night vibrant
	ColorPastelPink  Color = "#f7768e" // Red
	ColorPastelPeach Color = "#ff9e64" // Orange
	ColorPastelMint  Color = "#9ece6a" // Green
	ColorPastelLilac Color = "#bb9af7" // Magenta / purple
	ColorPastelSky   Color = "#7aa2f7" // Blue
	ColorPastelLemon Color = "#e0af68" // Yellow
	ColorPastelCoral Color = "#f7768e" // Red (alias)

	// Text colors for dark background
	ColorFg      Color = "#a9b1d6" // Primary text
	ColorDimText Color = "#565f89" // Secondary / dim text
)

// TUI layout constants.
const (
	TrendsDaysLookback   = 7
	SessionIDDisplayLen  = 8
	ContentTruncLen      = 100
	ToolArgsTruncLen     = 50
	WideLayoutBreakpoint = 100
	MinCardWidth         = 20
)

// Column widths for the sessions table.
const (
	ColWidthID       = 10
	ColWidthProvider = 10
	ColWidthOutcome  = 10
	ColWidthDate     = 14
	ColWidthDuration = 10
	ColWidthTokens   = 10
	ColWidthTurns    = 6
)

// Key is a typed keyboard shortcut identifier.
type Key string

func (k Key) String() string { return string(k) }

// Keyboard shortcuts.
const (
	KeyQuit      Key = "q"
	KeyInterrupt Key = "ctrl+c"
	KeyTab       Key = "tab"
	KeyShiftTab  Key = "shift+tab"
	KeyEnter     Key = "enter"
	KeyEscape    Key = "esc"
	KeyBackspace Key = "backspace"
	KeySpace     Key = "space"
	KeyUp        Key = "up"
	KeyDown      Key = "down"
	KeyVimUp     Key = "k"
	KeyVimDown   Key = "j"
	KeyBack      Key = "b"
	KeyRestart   Key = "r"
)

// Push wizard keys.
const (
	KeyPushApprove Key = "w" // set session to push
	KeyExclude     Key = "x" // set session to exclude / exclude all
)

// Navigation keys used by TUI app and FTUE.
const (
	KeyRight Key = "right"
	KeyLeft  Key = "left"
	Key1     Key = "1"
	Key2     Key = "2"
	Key3     Key = "3"
)

// Tree navigation keys for FTUE session selection are defined in
// internal/tui/ftue.DefaultTreeKeyMap using charmbracelet/bubbles/key bindings.

// Annotation navigation keys (vim-inspired, session detail view).
const (
	KeyVimLeft       Key = "h"           // jump to previous depth=0 turn
	KeyVimRight      Key = "l"           // jump to next depth=0 turn
	KeyAnnotate      Key = "a"           // open annotation type picker
	KeyDeletePending Key = "d"           // delete pending annotation from SQLite
	KeyFind          Key = "f"           // find / filter turns
	KeyCommitPending Key = "c"           // commit pending annotations via HTTP POST
	KeyShiftSpace    Key = "shift+space" // begin/end range selection
	KeyPageDown      Key = "ctrl+d"      // page down
	KeyPageUp        Key = "ctrl+u"      // page up
	// KeyAnnotateS ('s' key): unbound, reserved for future use
)

// AnnotationBadgeColors is the Okabe-Ito colorblind-safe palette for annotation type badges.
// Index maps to a distinct color; cycle with modulo for more than 7 annotation types.
// Source: https://jfly.uni-koeln.de/color/ (Okabe & Ito, 2002).
var AnnotationBadgeColors = [7]Color{
	"#E69F00", // orange
	"#56B4E9", // sky blue
	"#009E73", // green
	"#F0E442", // yellow
	"#0072B2", // blue
	"#D55E00", // vermilion
	"#CC79A7", // reddish purple
}
