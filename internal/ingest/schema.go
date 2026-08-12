package ingest

import (
	"time"

	schema "github.com/peasant-labs/schema"
)

// Session represents a normalized agent coding session.
type Session struct {
	ID        SessionID
	Project   string
	Harness   Harness
	StartTime time.Time
	EndTime   time.Time
	Turns     []Turn
	Metadata  SessionMetadata

	// Detail fields — populated by SessionByID for the detail view.
	Model       string // from sessions.model_id
	GitBranch   string // from sessions.git_branch
	GitRemote   string // from host_slugs.git_remote
	ProjectPath string // from projects.project_path (working directory)
	PushedAt    *int64 // from sessions.pushed_at (nil = not pushed)
}

// Turn represents a single interaction turn within a session.
type Turn struct {
	Index       int
	Role        Role
	Content     string
	ToolCalls   []ToolCall
	Timestamp   time.Time
	Depth       int  // 0 = main agent, 1+ = subagent nesting levels
	ParentIndex *int // entry_index of parent turn (nil for depth=0)
	// ObservedModel is exact source evidence. Omission remains omission; readers
	// derive sticky state without mutating this raw observation.
	ObservedModel ObservedModelID

	// Enrichment fields — propagated from session_entries for the detail view.
	EntryType   schema.EntryType   // text, tool_use, tool_result, thinking, system, error, result
	HasThinking bool               // whether the turn contains thinking/reasoning blocks
	StopReason  *schema.StopReason // why the turn ended (end_turn, max_tokens, cancelled, etc.)
	TokensIn    *int               // input tokens for this turn
	TokensOut   *int               // output tokens for this turn
	PartType    *string            // provider's original part type label (nil for depth=0)
}

// ToolCall represents a tool invocation within a turn.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
	Result    string

	// Enrichment fields — computed from session_entries data.
	DurationMs *int                // wall-clock duration from tool_use to tool_result
	ExitCode   *int                // exit code for Bash tool calls
	FilePath   string              // extracted file path for Read/Write/Edit/Glob
	IsError    bool                // true when tool_result had is_error flag set
	ToolKind   schema.ToolCallKind // classified tool type: read, edit, execute, search, etc.
}

// SessionMetadata holds aggregate metrics for a session.
type SessionMetadata struct {
	TokensIn      int
	TokensOut     int
	TotalTokens   int
	Duration      time.Duration
	TurnCount     int
	ToolCallCount int
	Quality       *schema.QualityMetrics // nil when quality data not available
}
