package codemap

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/peasant-labs/schema"
)

// Tool names recognized by the Touch rule: an edit is a
// depth-1 tool_use whose tool name is Write/Edit/NotebookEdit and whose
// tool_input carries file_path/notebook_path. Reads (Read with file_path)
// count only toward ReadCount — never toward activity edges, coverage, or
// binding.
const (
	toolNameWrite        = "Write"
	toolNameEdit         = "Edit"
	toolNameNotebookEdit = "NotebookEdit"
	toolNameRead         = "Read"
)

// errorAdjacencyWindow: an edit counts as "error-adjacent" (effort signal)
// when an is_error entry occurs within this many entries after it in the
// same session.
const errorAdjacencyWindow = 2

// taskData is one task: a depth-0 user turn and everything until the next
// depth-0 user turn. Entries before the first user turn
// belong to no task and are skipped.
type taskData struct {
	sessionID  string
	entryIndex int // the depth-0 user entry's index (task identity)
	title      string
	startMs    *int64

	editedFiles []string    // repo-relative, sorted distinct
	editEvents  []editEvent // every edit occurrence (for effort + coverage)
	readCount   int
	retryLoop   bool
}

// editEvent is a single recorded edit of a file inside a task.
type editEvent struct {
	file          string // repo-relative
	ms            *int64
	errorAdjacent bool // an is_error entry follows within errorAdjacencyWindow
}

// touchesPath reports whether the task edited the given file, or any file
// under the given directory path.
func (t *taskData) touchesPath(p string) bool {
	for _, f := range t.editedFiles {
		if f == p || strings.HasPrefix(f, p+"/") {
			return true
		}
	}
	return false
}

// rollupTasks groups one session's entries into tasks.
//
//   - Grain: each depth-0 role=user entry opens a task; its range runs to
//     the next depth-0 user entry (exclusive) — all depths in between belong
//     to the task.
//   - Title: the user entry's content_preview truncated at a word boundary
//     to <=80 chars (same approach as the title metric in internal/metrics).
//   - Edits/reads: per the Touch rule (see tool name constants above). Tool
//     paths are recorded absolute by the harness; they are relativized
//     against the project's canonical_cwd, and paths outside it are skipped.
//   - retryLoop: true when >=2 consecutive depth-0 assistant/tool entries
//     with is_error occur inside the task's range.
func rollupTasks(sessionID, cwd string, entries []schema.SessionEntry) []taskData {
	// Error positions for the error-adjacency lookups.
	errorAt := make(map[int]bool)
	for i := range entries {
		if entries[i].IsError {
			errorAt[entries[i].EntryIndex] = true
		}
	}

	var tasks []taskData
	var cur *taskData
	flush := func() {
		if cur == nil {
			return
		}
		sort.Strings(cur.editedFiles)
		cur.editedFiles = dedupeSorted(cur.editedFiles)
		tasks = append(tasks, *cur)
		cur = nil
	}

	errStreak := 0
	for i := range entries {
		e := &entries[i]

		if e.Depth == 0 && e.Role == schema.RoleUser {
			flush()
			cur = &taskData{
				sessionID:   sessionID,
				entryIndex:  e.EntryIndex,
				title:       titleFromPreview(e.ContentPreview),
				startMs:     e.TimestampMs,
				editedFiles: []string{},
				editEvents:  []editEvent{},
			}
			errStreak = 0
			continue
		}
		if cur == nil {
			continue // before the first user turn: no task to attach to
		}

		// Retry detection: consecutive depth-0 assistant/tool error entries.
		if e.Depth == 0 && (e.Role == schema.RoleAssistant || e.Role == schema.RoleTool) {
			if e.IsError {
				errStreak++
				if errStreak >= 2 {
					cur.retryLoop = true
				}
			} else {
				errStreak = 0
			}
		}

		// Touch extraction: depth-1 tool_use with a recognized tool name and
		// a file path in tool_input.
		if e.Depth != 1 || e.EntryType != schema.EntryTypeToolUse || e.ToolInput == nil || e.ToolNamesCSV == nil {
			continue
		}
		filePath := toolInputFilePath(*e.ToolInput)
		if filePath == "" {
			continue
		}
		rel, ok := relativizePath(cwd, filePath)
		if !ok {
			continue
		}
		switch {
		case csvContainsAny(*e.ToolNamesCSV, toolNameWrite, toolNameEdit, toolNameNotebookEdit):
			cur.editedFiles = append(cur.editedFiles, rel)
			cur.editEvents = append(cur.editEvents, editEvent{
				file:          rel,
				ms:            e.TimestampMs,
				errorAdjacent: errorWithin(errorAt, e.EntryIndex, errorAdjacencyWindow),
			})
		case csvContainsAny(*e.ToolNamesCSV, toolNameRead):
			cur.readCount++
		}
	}
	flush()
	return tasks
}

// titleFromPreview derives the task title from a content preview: first
// words, truncated at the last word boundary at or before 80 characters
// (mirrors the session title metric).
func titleFromPreview(preview *string) string {
	if preview == nil {
		return ""
	}
	return truncateTitle(*preview)
}

// truncateTitle truncates at the last word boundary at or before 80
// characters (mirrors the session title metric).
func truncateTitle(title string) string {
	if len(title) > 80 {
		if idx := strings.LastIndex(title[:81], " "); idx > 0 {
			title = title[:idx]
		} else {
			title = title[:80]
		}
	}
	return title
}

// Title-signal floor: a user-turn preview below this many words or
// characters is a sentence fragment ("The file"), not a task label. The
// wire title falls back to the session title, then the first edited
// filename, then "task @ <entryIndex>" (see projectData.taskTitle).
const (
	minTitleWords = 3
	minTitleChars = 12
)

// titleHasSignal reports whether a candidate title carries enough signal to
// serve as a primary label.
func titleHasSignal(title string) bool {
	return len(title) >= minTitleChars && len(strings.Fields(title)) >= minTitleWords
}

// toolInputFilePath extracts file_path (or notebook_path) from a tool_use
// input JSON. Malformed JSON or absent keys return "" and are skipped.
func toolInputFilePath(toolInput string) string {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(toolInput), &parsed); err != nil {
		return ""
	}
	filePath, _ := parsed["file_path"].(string)
	if filePath == "" {
		filePath, _ = parsed["notebook_path"].(string)
	}
	return filePath
}

// relativizePath converts a recorded tool path to a repo-relative path.
// Already-relative paths pass through cleaned; absolute paths must sit under
// the project's canonical_cwd or they are skipped (ok=false) — recorded
// edits outside the repo cannot be placed on the map. Agent-worktree
// segments (.claude/worktrees/<name>/, .claire/worktrees/<name>/) are
// stripped so the remainder remaps onto the real tree.
func relativizePath(cwd, p string) (string, bool) {
	if p == "" {
		return "", false
	}
	if !strings.HasPrefix(p, "/") {
		clean := cleanRelPath(stripWorktreePrefix(p))
		return clean, clean != ""
	}
	if cwd == "" {
		return "", false
	}
	cwd = strings.TrimSuffix(cwd, "/")
	if !strings.HasPrefix(p, cwd+"/") {
		return "", false
	}
	clean := cleanRelPath(stripWorktreePrefix(p[len(cwd)+1:]))
	return clean, clean != ""
}

// Agent-harness worktree path components: tool paths recorded inside an
// agent worktree (<cwd>/.claude/worktrees/<name>/rest, also seen with
// .claire) would otherwise become phantom repo-relative node IDs that
// duplicate whole subtrees and steal activity attribution from the real
// modules.
const worktreesDirName = "worktrees"

var worktreeMetaDirs = []string{".claude", ".claire"}

// stripWorktreePrefix remaps a repo-relative path recorded inside an agent
// worktree onto the real repo tree by stripping everything through the
// worktree name segment (liberally: any …/.claude/worktrees/<name>/ or
// …/.claire/worktrees/<name>/ run, nested runs included). Paths without a
// worktree run pass through unchanged, so non-worktree dotpaths (e.g.
// .claude/settings.json) keep their own nodes.
func stripWorktreePrefix(p string) string {
	segs := strings.Split(p, "/")
	for i := 0; i+3 < len(segs); i++ {
		if segs[i+1] != worktreesDirName || segs[i+2] == "" {
			continue
		}
		for _, meta := range worktreeMetaDirs {
			if segs[i] == meta {
				return stripWorktreePrefix(strings.Join(segs[i+3:], "/"))
			}
		}
	}
	return p
}

// cleanRelPath normalizes a relative path; rejects empty/escaping results.
func cleanRelPath(p string) string {
	p = strings.TrimPrefix(p, "./")
	if p == "" || p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return ""
	}
	return p
}

// csvContainsAny reports whether the comma-separated tool name list contains
// any of the given names (tool_names_csv has no spaces).
func csvContainsAny(csv string, names ...string) bool {
	for _, part := range strings.Split(csv, ",") {
		for _, name := range names {
			if part == name {
				return true
			}
		}
	}
	return false
}

// errorWithin reports whether an is_error entry exists in
// (index, index+window].
func errorWithin(errorAt map[int]bool, index, window int) bool {
	for i := index + 1; i <= index+window; i++ {
		if errorAt[i] {
			return true
		}
	}
	return false
}

// dedupeSorted removes adjacent duplicates from a sorted slice in place.
func dedupeSorted(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// loadTasks rolls up tasks for every session of the project, in the
// sessions' (newest-first) order.
func (s *Service) loadTasks(ctx context.Context, cwd string, sessions []sessionRow) ([]taskData, error) {
	var tasks []taskData
	for _, sess := range sessions {
		entries, err := s.listEntries(ctx, sess.id)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, rollupTasks(sess.id, cwd, entries)...)
	}
	return tasks, nil
}
