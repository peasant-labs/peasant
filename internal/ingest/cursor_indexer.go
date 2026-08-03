package ingest

import (
	"strings"

	"github.com/peasant-labs/schema"
)

// classifyCursorToolKind maps Cursor IDE tool names to schema.ToolCallKind.
// Names are matched case-insensitively and cover both PascalCase (agent SDK)
// and snake_case (older Cursor versions) conventions.
func classifyCursorToolKind(toolName string) schema.ToolCallKind {
	switch strings.ToLower(toolName) {
	case "readfile", "read_file", "readfiles", "viewfile", "view_file",
		"openfile", "open_file", "listfiles", "list_files", "listdirectory", "list_dir":
		return schema.ToolCallKindRead
	case "editfile", "edit_file", "writefile", "write_file", "applyedit", "apply_edit",
		"applydiff", "apply_diff", "rewritefile", "rewrite_file", "createfile", "create_file",
		"inserttext", "insert_edit_into_file", "replacetext", "deletetext":
		return schema.ToolCallKindEdit
	case "deletefile", "delete_file":
		return schema.ToolCallKindDelete
	case "movefile", "move_file", "renamefile", "rename_file":
		return schema.ToolCallKindMove
	case "searchcode", "codebase_search", "grepfiles", "grep_search", "searchfiles",
		"file_search", "findfiles", "find_by_name", "searchsymbol", "findsymbol", "getoutline":
		return schema.ToolCallKindSearch
	case "runterminalcommand", "run_terminal_cmd", "runcommand", "run_command",
		"executecommand", "execute_code", "terminal":
		return schema.ToolCallKindExecute
	case "browsersearch", "web_search", "websearch", "fetchurl", "fetch_rules":
		return schema.ToolCallKindFetch
	default:
		return schema.ToolCallKindOther
	}
}

// stripCursorQueryTag extracts the content inside <user_query>...</user_query>
// from Cursor JSONL user messages. Cursor may also prepend a <timestamp> tag
// before <user_query>, so substring extraction is used rather than TrimPrefix.
// If no <user_query> tag is present, the input is returned unchanged.
func stripCursorQueryTag(s string) string {
	const open, close = "<user_query>", "</user_query>"
	start := strings.Index(s, open)
	if start == -1 {
		return s
	}
	inner := s[start+len(open):]
	if end := strings.Index(inner, close); end != -1 {
		inner = inner[:end]
	}
	return strings.TrimSpace(inner)
}
