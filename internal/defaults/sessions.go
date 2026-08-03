package defaults

// SessionSortField represents the field by which sessions are sorted in listings.
type SessionSortField string

func (f SessionSortField) String() string { return string(f) }

// IsValid reports whether f is a known sort field.
func (f SessionSortField) IsValid() bool {
	for _, v := range AllSessionSortFields {
		if f == v {
			return true
		}
	}
	return false
}

const (
	// SessionSortDate sorts sessions by start date (default).
	SessionSortDate SessionSortField = "date"
	// SessionSortTurns sorts sessions by turn count.
	SessionSortTurns SessionSortField = "turns"
	// SessionSortTokens sorts sessions by total token count.
	SessionSortTokens SessionSortField = "tokens"
	// SessionSortProject sorts sessions by project name.
	SessionSortProject SessionSortField = "project"
)

// AllSessionSortFields is the canonical list of all known sort fields.
var AllSessionSortFields = []SessionSortField{
	SessionSortDate,
	SessionSortTurns,
	SessionSortTokens,
	SessionSortProject,
}

// ToolCallFormat controls how tool calls are rendered in session output.
type ToolCallFormat string

func (f ToolCallFormat) String() string { return string(f) }

// IsValid reports whether f is a known tool call format.
func (f ToolCallFormat) IsValid() bool {
	for _, v := range AllToolCallFormats {
		if f == v {
			return true
		}
	}
	return false
}

const (
	// ToolCallFormatVerbose shows full tool call details (default).
	ToolCallFormatVerbose ToolCallFormat = "verbose"
	// ToolCallFormatCompact shows a one-line summary per tool call.
	ToolCallFormatCompact ToolCallFormat = "compact"
	// ToolCallFormatQuiet suppresses tool call output entirely.
	ToolCallFormatQuiet ToolCallFormat = "quiet"
)

// AllToolCallFormats is the canonical list of all known tool call formats.
var AllToolCallFormats = []ToolCallFormat{
	ToolCallFormatVerbose,
	ToolCallFormatCompact,
	ToolCallFormatQuiet,
}

const (
	// SessionListDefaultLimit is the default maximum number of sessions returned by
	// `peasant sessions list`.
	SessionListDefaultLimit = 20
	// SessionsBareDefaultLimit is the maximum number of sessions shown by bare
	// `peasant sessions` (no subcommand). Smaller than SessionListDefaultLimit
	// because the listing is a discovery aid, not the user's primary request.
	SessionsBareDefaultLimit = 5
	// SessionContextDefaultRadius is the default number of turns shown before/after
	// a target turn in context view.
	SessionContextDefaultRadius = 3
	// SessionPreviewMaxChars is the maximum number of Unicode characters in a
	// first-message preview (used by FirstUserMessage in the LIST view).
	SessionPreviewMaxChars = 40
	// SessionContextPreviewMaxChars is the per-line preview width for the
	// `sessions context` DETAIL view. It is distinct from the 40-rune
	// SessionPreviewMaxChars list cap: the detail view shows fuller content
	// (content_preview is stored at ContentPreviewLimit=2000), so its per-line
	// truncation width is wider to avoid shortening detail lines.
	SessionContextPreviewMaxChars = 117
)
