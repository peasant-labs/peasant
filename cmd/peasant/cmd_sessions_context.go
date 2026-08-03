package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

// sessionContextOutput is the JSON-serializable envelope for the context command.
type sessionContextOutput struct {
	SessionID     string                `json:"sessionId"`
	CenterIndex   int                   `json:"centerIndex"`
	ContextRadius int                   `json:"contextRadius"`
	MaxEntryIndex int                   `json:"maxEntryIndex"`
	Entries       []schema.SessionEntry `json:"entries"`
}

// buildSessionsContextCommand constructs the `peasant sessions context` subcommand.
// It shows a window of session_entries around a specified entry index (--turn N)
// with -C entries on each side, using direct DB index lookup — NOT dense ordinals.
func buildSessionsContextCommand() *cobra.Command {
	var (
		sessionID   string
		turn        int
		radius      int
		asJSON      bool
		formatTCRaw string
	)

	cmd := &cobra.Command{
		Use:   "context",
		Short: "Show entries around a target entry in a session transcript",
		Long: `Show a window of session transcript entries centered on --turn N.

--turn N uses entry_index directly (raw DB index, not a dense ordinal).
-C K shows K entries before and K entries after the target, clamped to
the session boundaries.

Each entry (depth=0 and depth=1) is displayed on its own line. Depth=1
entries are visually indented. Tool entries use box-drawing characters.

If --session is omitted, recent sessions are listed so you can pick one.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			// Validate --format-tool-calls before opening the DB so invalid values
			// fail fast without any I/O.
			formatTC := defaults.ToolCallFormat(formatTCRaw)
			if !formatTC.IsValid() {
				return fmt.Errorf("invalid --format-tool-calls value %q: must be one of {verbose, compact, quiet}", formatTCRaw)
			}

			// D6: single-DB-open guard.
			// The two branches below are mutually exclusive: the no-session path
			// returns early (error), so the DB is opened exactly ONCE per invocation.

			// R7 fallback: no --session provided → list sessions + hint.
			if sessionID == "" {
				db, cleanup, err := openDB(cmd)
				if err != nil {
					return err
				}
				defer cleanup()

				f := store.SessionListFilter{
					SortField: defaults.SessionSortDate,
					SortDesc:  true,
					Limit:     defaults.SessionListDefaultLimit,
				}
				_ = listSessionsShared(cmd, db, f, false)
				return fmt.Errorf(
					"no session specified — use --session <id> --turn <N> to view context\n" +
						"  (pick a session from the list above)",
				)
			}

			sid, err := schema.NewSessionID(sessionID)
			if err != nil {
				return fmt.Errorf(
					"invalid --session value %q: %w\n"+
						"  what: the session ID format is not recognised\n"+
						"  how to fix: use `peasant sessions list` to find a valid session ID",
					sessionID, err,
				)
			}

			// --session path: the fallback path above returned early, so this is
			// the only remaining openDB call — the DB is opened exactly once.
			db, cleanup, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			maxIdx, err := db.MaxEntryIndex(ctx, sid)
			if err != nil {
				return fmt.Errorf("sessions context: %w", err)
			}
			if maxIdx < 0 {
				return fmt.Errorf(
					"session %s not found or not yet indexed — run `peasant ingest` if the session should exist",
					sid,
				)
			}

			// Validate --turn is within bounds before clamping.
			if turn < 0 || turn > maxIdx {
				return fmt.Errorf(
					"--turn %d is out of range [0, %d] for session %s\n"+
						"  what: the specified entry index does not exist in this session\n"+
						"  how to fix: choose a turn index between 0 and %d",
					turn, maxIdx, sid, maxIdx,
				)
			}

			// Clamp [turn-C, turn+C] to [0, maxIdx].
			fromIndex := turn - radius
			if fromIndex < 0 {
				fromIndex = 0
			}
			toIndex := turn + radius
			if toIndex > maxIdx {
				toIndex = maxIdx
			}

			entries, err := db.ListEntriesRange(ctx, sid, fromIndex, toIndex)
			if err != nil {
				return fmt.Errorf("sessions context: %w", err)
			}

			// Look up the session's project working directory for path relativization.
			var projectDir string
			if row, rErr := db.SessionByID(ctx, string(sid)); rErr == nil && row != nil {
				projectDir = row.ProjectName // COALESCE(canonical_cwd, project_hash)
			}

			if asJSON {
				return renderSessionContextJSON(cmd, sid, turn, radius, maxIdx, entries, projectDir)
			}
			renderSessionContextHuman(cmd, sid, turn, entries, projectDir, formatTC)
			return nil
		},
	}

	cmd.Flags().StringVar(&sessionID, "session", "", "Session ID to inspect")
	// D4: --turn=0 default documented: turn=0 centers the window on entry_index 0
	// (the first indexed entry in the session). Use a higher value to jump deeper
	// into the transcript.
	cmd.Flags().IntVar(&turn, "turn", 0,
		"Target entry_index (center of the context window); 0 = first indexed entry")
	cmd.Flags().IntVarP(&radius, "context", "C", defaults.SessionContextDefaultRadius,
		"Number of entries to show before and after the target")
	cmd.Flags().BoolVar(&asJSON, defaults.JSONFlagName, false, "Output as JSON")
	cmd.Flags().StringVar(&formatTCRaw, "format-tool-calls",
		string(defaults.ToolCallFormatVerbose),
		"How to render tool calls: verbose (full box), compact (one-line), or quiet (hidden)")

	return cmd
}

// renderSessionContextJSON writes the JSON envelope to cmd's output.
// If projectDir is non-empty, paths in content fields are relativized.
func renderSessionContextJSON(
	cmd *cobra.Command,
	sid schema.SessionID,
	turn, radius, maxIdx int,
	entries []schema.SessionEntry,
	projectDir string,
) error {
	if projectDir != "" {
		entries = relativizeEntryPaths(entries, projectDir)
	}
	out := sessionContextOutput{
		SessionID:     string(sid),
		CenterIndex:   turn,
		ContextRadius: radius,
		MaxEntryIndex: maxIdx,
		Entries:       entries,
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// renderSessionContextHuman writes the human-readable transcript window to cmd's output.
// formatTC controls how tool-call entries are rendered (verbose/compact/quiet).
//
// Format:
//
//	[N] role  HH:MM:SS  depth=0  [◀ center]
//	  content preview
//
//	  [M] part_type  HH:MM:SS  depth=1  parent=P
//	    ┌ Tool: name(args)
//	    │ content
//	    └
func renderSessionContextHuman(cmd *cobra.Command, sid schema.SessionID, turn int, entries []schema.SessionEntry, projectDir string, formatTC defaults.ToolCallFormat) {
	w := cmd.OutOrStdout()

	if projectDir != "" {
		entries = relativizeEntryPaths(entries, projectDir)
	}

	for _, e := range entries {
		isCenter := e.EntryIndex == turn
		ts := formatEntryTimestamp(e.TimestampMs)

		if e.Depth == 0 {
			// Depth-0 header.
			header := fmt.Sprintf("[%d] %s  %s  depth=0", e.EntryIndex, e.Role, ts)
			if isCenter {
				header += "  ◀ center"
			}
			fmt.Fprintln(w, header)

			// Content preview (indented 2 spaces).
			if e.ContentPreview != nil && *e.ContentPreview != "" {
				fmt.Fprintf(w, "  %s\n", wrapPreview(*e.ContentPreview))
			}
			fmt.Fprintln(w)
		} else {
			// Depth-1 header (indented 2 spaces).
			parentStr := ""
			if e.ParentIndex != nil {
				parentStr = fmt.Sprintf("  parent=%d", *e.ParentIndex)
			}
			header := fmt.Sprintf("  [%d] %s  %s  depth=1%s", e.EntryIndex, e.EntryType, ts, parentStr)
			if isCenter {
				header += "  ◀ center"
			}
			fmt.Fprintln(w, header)

			// Tool entries are rendered according to formatTC.
			if isToolEntry(e.EntryType) {
				switch formatTC {
				case defaults.ToolCallFormatVerbose:
					renderToolBox(w, e)
				case defaults.ToolCallFormatCompact:
					renderToolCompact(w, e)
				case defaults.ToolCallFormatQuiet:
					// quiet: suppress tool box entirely.
				}
			} else if e.ContentPreview != nil && *e.ContentPreview != "" {
				fmt.Fprintf(w, "    %s\n", wrapPreview(*e.ContentPreview))
			}
			fmt.Fprintln(w)
		}
	}
}

// isToolEntry reports whether the entry type is a tool-related depth-1 entry.
func isToolEntry(t schema.EntryType) bool {
	return t == schema.EntryTypeToolUse || t == schema.EntryTypeToolResult
}

// renderToolBox writes the ┌│└ box-drawing block for a tool entry.
// This is the verbose rendering — byte-identical to the original output.
func renderToolBox(w interface{ Write([]byte) (int, error) }, e schema.SessionEntry) {
	// Determine tool name from ToolNamesCSV or PartType.
	toolName := ""
	if e.ToolNamesCSV != nil && *e.ToolNamesCSV != "" {
		// Use first tool name from CSV.
		parts := strings.SplitN(*e.ToolNamesCSV, ",", 2)
		toolName = strings.TrimSpace(parts[0])
	} else if e.PartType != nil {
		toolName = *e.PartType
	}

	var lines []string
	if e.EntryType == schema.EntryTypeToolUse {
		label := "Tool: " + toolName
		if e.ToolInput != nil && *e.ToolInput != "" {
			label += "(" + truncateJSON(*e.ToolInput, 80) + ")"
		}
		lines = append(lines, "┌ "+label)
		if e.ContentPreview != nil && *e.ContentPreview != "" {
			for _, l := range strings.Split(*e.ContentPreview, "\n") {
				lines = append(lines, "│ "+l)
			}
		}
		lines = append(lines, "└")
	} else {
		// tool_result
		label := "Tool: " + toolName + " (result)"
		lines = append(lines, "┌ "+label)
		if e.ToolOutput != nil && *e.ToolOutput != "" {
			for _, l := range strings.Split(*e.ToolOutput, "\n") {
				lines = append(lines, "│ "+l)
			}
		} else if e.ContentPreview != nil && *e.ContentPreview != "" {
			for _, l := range strings.Split(*e.ContentPreview, "\n") {
				lines = append(lines, "│ "+l)
			}
		}
		lines = append(lines, "└")
	}

	for _, line := range lines {
		fmt.Fprintln(w, "    "+line)
	}
}

// renderToolCompact writes a single-line tool summary (compact format).
func renderToolCompact(w interface{ Write([]byte) (int, error) }, e schema.SessionEntry) {
	toolName := ""
	if e.ToolNamesCSV != nil && *e.ToolNamesCSV != "" {
		parts := strings.SplitN(*e.ToolNamesCSV, ",", 2)
		toolName = strings.TrimSpace(parts[0])
	} else if e.PartType != nil {
		toolName = *e.PartType
	}

	if e.EntryType == schema.EntryTypeToolUse {
		summary := "    tool: " + toolName
		if e.ToolInput != nil && *e.ToolInput != "" {
			summary += "(" + truncateJSON(*e.ToolInput, 40) + ")"
		}
		fmt.Fprintln(w, summary)
	} else {
		// tool_result
		fmt.Fprintln(w, "    tool: "+toolName+" (result)")
	}
}

// relativizeEntryPaths returns a copy of entries with absolute project paths replaced
// by relative paths. Strips "projectDir/" prefix from content_preview, tool_input,
// and tool_output fields.
func relativizeEntryPaths(entries []schema.SessionEntry, projectDir string) []schema.SessionEntry {
	// Ensure trailing slash for prefix replacement.
	prefix := strings.TrimRight(projectDir, "/") + "/"

	out := make([]schema.SessionEntry, len(entries))
	copy(out, entries)
	for i := range out {
		out[i].ContentPreview = relativizeStringPtr(out[i].ContentPreview, prefix)
		out[i].ToolInput = relativizeStringPtr(out[i].ToolInput, prefix)
		out[i].ToolOutput = relativizeStringPtr(out[i].ToolOutput, prefix)
	}
	return out
}

// relativizeStringPtr strips a path prefix from the string pointer's value.
// Returns nil for nil input, and returns the original pointer if no replacement occurs.
func relativizeStringPtr(s *string, prefix string) *string {
	if s == nil {
		return nil
	}
	replaced := strings.ReplaceAll(*s, prefix, "")
	if replaced == *s {
		return s
	}
	return &replaced
}

// formatEntryTimestamp converts a Unix millisecond timestamp pointer to "HH:MM:SS".
// Returns "--:--:--" when the timestamp is nil.
func formatEntryTimestamp(ms *int64) string {
	if ms == nil {
		return "--:--:--"
	}
	return time.Unix(*ms/1000, 0).UTC().Format("15:04:05")
}

// wrapPreview returns the content preview, possibly truncated for display.
// Uses store.TruncateToRunes for rune-safe truncation (no mid-rune cuts on
// multibyte Unicode such as CJK characters or emoji).
func wrapPreview(s string) string {
	// Return first non-empty line, truncated to defaults.SessionContextPreviewMaxChars runes.
	first := strings.SplitN(s, "\n", 2)[0]
	truncated := store.TruncateToRunes(first, defaults.SessionContextPreviewMaxChars)
	if truncated != first {
		return truncated + "..."
	}
	return first
}

// truncateJSON truncates a JSON string for display using rune-safe truncation.
// Uses store.TruncateToRunes to avoid splitting multi-byte UTF-8 sequences.
func truncateJSON(s string, maxLen int) string {
	if len([]rune(s)) <= maxLen {
		return s
	}
	return store.TruncateToRunes(s, maxLen-3) + "..."
}
