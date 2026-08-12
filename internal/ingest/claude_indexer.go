package ingest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// System-injected content prefixes and tags. These are harness-generated
// strings that arrive as role=user in Claude JSONL transcripts but are not
// user-authored. Each constant is the prefix (or exact match) used by
// isSystemInjectedContent to reclassify entries to role=system.
const (
	// CompactionPrefix is the leading text of a context-continuation message
	// injected when a session runs out of context.
	CompactionPrefix = "This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion of the conversation."

	// TagSystemReminder is the opening tag for system-reminder blocks.
	TagSystemReminder = "<system-reminder>"
	// TagSystemReminderClose is the closing tag for system-reminder blocks.
	TagSystemReminderClose = "</system-reminder>"

	// TagTaskNotification is the opening tag for task-notification blocks.
	TagTaskNotification = "<task-notification>"
	// TagTaskNotificationClose is the closing tag for task-notification blocks.
	TagTaskNotificationClose = "</task-notification>"

	// TagTeammateMessage is the opening tag for teammate/subagent messages
	// injected by the harness for multi-agent coordination.
	TagTeammateMessage = "<teammate-message"

	// TagLocalCommand is the opening prefix for local command output
	// (stdout/stderr/caveat from user's shell commands).
	TagLocalCommand = "<local-command-"

	// TagErrorReport is the opening tag for harness-injected error reports.
	TagErrorReport = "<error-report>"
	// TagBashStdout is the opening tag for harness-injected bash output echoes.
	TagBashStdout = "<bash-stdout>"
	// TagBashInput is the opening tag for harness-injected bash input echoes.
	TagBashInput = "<bash-input>"
	// TagFeedback is the opening tag for harness-injected feedback blocks.
	TagFeedback = "<feedback>"

	// PrefixSkillBody is the leading text of a skill body injection emitted
	// when a user invokes a skill (e.g. /aura:epoch).
	PrefixSkillBody = "Base directory for this skill:"

	// ExactToolLoaded is the exact text of a tool-loaded confirmation injected
	// when a deferred tool schema is fetched.
	ExactToolLoaded = "Tool loaded."
)

// ClaudeIndexer parses Claude Code JSONL transcripts into SessionEntry slices.
type ClaudeIndexer struct {
	fs          FileSystem
	fullDepth   bool
	fullContent bool
}

// ClaudeIndexerOption configures a ClaudeIndexer.
type ClaudeIndexerOption func(*ClaudeIndexer)

// WithClaudeFullDepth enables or disables full-depth content block decomposition.
// When enabled, each content block within a message becomes a depth=1 child entry.
func WithClaudeFullDepth(enabled bool) ClaudeIndexerOption {
	return func(idx *ClaudeIndexer) { idx.fullDepth = enabled }
}

// WithClaudeFullContent enables full content extraction when set to true.
// When enabled, ContentPreview is populated with the complete content string
// without truncation. Used by the export pipeline to get full transcript content.
// When disabled (default), content is truncated to defaults.ContentPreviewLimit.
func WithClaudeFullContent(enabled bool) ClaudeIndexerOption {
	return func(idx *ClaudeIndexer) { idx.fullContent = enabled }
}

var _ TranscriptIndexer = (*ClaudeIndexer)(nil)

// SourceKind reports that Claude's entries come from a single JSONL file; every entry is in its bytes.
func (idx *ClaudeIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }

// NewClaudeIndexer creates a ClaudeIndexer with an injected FileSystem.
func NewClaudeIndexer(fs FileSystem, opts ...ClaudeIndexerOption) *ClaudeIndexer {
	idx := &ClaudeIndexer{fs: fs}
	for _, opt := range opts {
		opt(idx)
	}
	return idx
}

// IndexTranscript reads a Claude JSONL transcript from disk and returns SessionEntry rows.
// Malformed lines are skipped (not fatal). Empty files return nil, nil.
func (idx *ClaudeIndexer) IndexTranscript(_ context.Context, session DiscoveredSession) ([]schema.SessionEntry, error) {
	data, err := idx.fs.ReadFile(string(session.SourcePath))
	if err != nil {
		return nil, fmt.Errorf("claude indexer: read %s: %w", session.SourcePath, err)
	}
	return idx.parseJSONL(session.SessionID, data)
}

// IndexTranscriptBytes parses a Claude JSONL transcript from the provided bytes.
// Used by the parallel ingest path to avoid re-reading from disk.
//
// The data must be the bytes of the transcript copy Peasant wrote, which is the
// transcript as recorded: nothing on the ingest path redacts transcript content
// at any level a user can choose.
func (idx *ClaudeIndexer) IndexTranscriptBytes(_ context.Context, session DiscoveredSession, data []byte) ([]schema.SessionEntry, error) {
	return idx.parseJSONL(session.SessionID, data)
}

// parseJSONL is the shared JSONL parsing kernel used by both IndexTranscript and
// IndexTranscriptBytes. Malformed lines are skipped (not fatal).
func (idx *ClaudeIndexer) parseJSONL(sessionID SessionID, data []byte) ([]schema.SessionEntry, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	buf := make([]byte, defaults.ScannerInitBuf)
	scanner.Buffer(buf, defaults.ScannerMaxLine)

	var entries []schema.SessionEntry
	entryIndex := 0
	// askUserCallIDs tracks tool_call_ids of AskUserQuestion tool_use blocks seen
	// in the transcript. tool_result blocks referencing these IDs keep role=user
	// (genuine user answers) instead of being reclassified to role=tool.
	askUserCallIDs := make(map[string]bool)

	for scanner.Scan() {
		raw := scanner.Bytes()
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 {
			continue
		}

		entry, ok := parseClaudeLine(sessionID, entryIndex, trimmed, idx.fullContent)
		if !ok {
			// Malformed line — skip silently.
			entryIndex++
			continue
		}
		entries = append(entries, entry)
		parentIndex := entryIndex
		entryIndex++

		// Full-depth decomposition: emit depth=1 entries for each content block.
		if idx.fullDepth {
			childEntries := decomposeClaudeContentBlocks(sessionID, &entryIndex, parentIndex, trimmed, idx.fullContent, askUserCallIDs)
			// Propagate reclassified role to depth=1 children — if the parent
			// was reclassified (e.g. system-injected skill body), children must
			// inherit the reclassified role, not the original JSONL role.
			if entry.Role == RoleSystem {
				for i := range childEntries {
					childEntries[i].Role = RoleSystem
					childEntries[i].EntryType = EntryTypeSystem
				}
			}

			// R3: Collect AskUserQuestion tool_call_ids from depth=1 tool_use entries
			// so that subsequent tool_result blocks can be kept as role=user.
			for _, child := range childEntries {
				if child.EntryType == EntryTypeToolUse &&
					child.ToolNamesCSV != nil && *child.ToolNamesCSV == "AskUserQuestion" &&
					child.ToolCallID != nil {
					askUserCallIDs[*child.ToolCallID] = true
				}
			}

			// R2 + R6: For tool_result wrappers (depth=0 role=user where all children
			// are tool_result), reclassify the depth-0 entry to role=tool and migrate
			// content_preview from the wrapper down to its depth-1 tool_result children.
			if entry.Role == RoleUser && len(childEntries) > 0 {
				allToolResult := true
				anyAskUser := false
				for _, child := range childEntries {
					if child.EntryType != EntryTypeToolResult {
						allToolResult = false
						break
					}
					if child.Role == RoleUser { // AskUserQuestion tool_result
						anyAskUser = true
					}
				}
				if allToolResult {
					// R6: Move content_preview from depth-0 wrapper to depth-1 children
					// that have no content_preview of their own.
					if entry.ContentPreview != nil {
						for i := range childEntries {
							if childEntries[i].EntryType == EntryTypeToolResult && childEntries[i].ContentPreview == nil {
								childEntries[i].ContentPreview = entry.ContentPreview
							}
						}
						// Clear the parent's preview — it now lives on the children.
						entries[len(entries)-1].ContentPreview = nil
						entry.ContentPreview = nil
					}
					// R2: Reclassify depth-0 wrapper to role=tool unless it is a
					// mixed wrapper (some AskUserQuestion children that kept role=user).
					if !anyAskUser {
						entries[len(entries)-1].Role = RoleTool
						entries[len(entries)-1].EntryType = EntryTypeToolResult
					}
				}
			}

			entries = append(entries, childEntries...)
		}
	}

	if err := scanner.Err(); err != nil {
		return entries, fmt.Errorf("claude indexer: scanner error for %s: %w", sessionID, err)
	}

	return entries, nil
}

// claudeIndexLine is the minimal parsed shape for indexing (subset of claudeJSONLLine).
type claudeIndexLine struct {
	Type       string          `json:"type"`
	UUID       string          `json:"uuid"`
	ParentUUID string          `json:"parentUuid"`
	Timestamp  json.RawMessage `json:"timestamp"`
	StopReason string          `json:"stop_reason"`
	Content    json.RawMessage `json:"content"` // Top-level content (system entries use this, not message.content)
	Message    struct {
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage,omitempty"`
	} `json:"message"`
}

// claudeContentBlock is a content block within an assistant message.
type claudeContentBlock struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"` // Claude puts the thinking trace text here, not in `text`.
	Content  string `json:"content"`
	IsError  bool   `json:"is_error"`
}

// parseClaudeLine parses a single JSONL line into a SessionEntry.
// Returns (entry, true) on success, (zero, false) on parse failure.
// When fullContent is true, ContentPreview is set to the full content string
// without truncation; otherwise it is capped at defaults.ContentPreviewLimit.
func parseClaudeLine(sessionID SessionID, index int, raw []byte, fullContent bool) (schema.SessionEntry, bool) {
	var line claudeIndexLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return schema.SessionEntry{}, false
	}

	entry := schema.SessionEntry{
		SessionID:  sessionID,
		EntryIndex: index,
		Harness:    HarnessClaudeCode,
	}

	// Classify entry type and role.
	entry.EntryType, entry.Role = classifyClaudeEntry(line)
	if entry.Role == RoleAssistant {
		if modelID, err := NewModelID(line.Message.Model); err == nil && strings.TrimSpace(line.Message.Model) == line.Message.Model {
			extra, marshalErr := json.Marshal(map[string]string{"model_id": modelID.String()})
			if marshalErr == nil {
				value := string(extra)
				entry.Extra = &value
			}
		}
	}

	// Timestamp.
	entry.TimestampMs = parseIndexTimestamp(line.Timestamp)

	// Raw byte length.
	rawLen := len(raw)
	entry.RawByteLength = &rawLen

	// Entry ID from UUID field.
	if line.UUID != "" {
		entry.EntryID = &line.UUID
	}
	if line.ParentUUID != "" {
		entry.ParentEntryID = &line.ParentUUID
	}

	// Parse content for tool use, thinking, error detection, and preview.
	var contentBlocks []claudeContentBlock
	var plainText string

	// Determine content source: prefer message.content (assistant/user entries),
	// fall back to top-level content (system entries have no message field).
	content := line.Message.Content
	if len(content) == 0 {
		content = line.Content
	}

	if len(content) > 0 {
		// Try array of content blocks (assistant messages).
		if json.Unmarshal(content, &contentBlocks) != nil {
			// Try plain string (user/system messages).
			json.Unmarshal(content, &plainText)
		}
	}

	var toolNames []string
	var preview strings.Builder
	var thinkingFallback string

	if plainText != "" {
		preview.WriteString(plainText)
	}

	for _, block := range contentBlocks {
		switch block.Type {
		case "tool_use":
			entry.HasToolUse = true
			if block.Name != "" {
				toolNames = append(toolNames, block.Name)
			}
		case "thinking":
			entry.HasThinking = true
			if thinkingFallback == "" && block.Thinking != "" {
				thinkingFallback = block.Thinking
			}
		case "text":
			if preview.Len() == 0 && block.Text != "" {
				preview.WriteString(block.Text)
			}
		case "tool_result":
			if block.IsError {
				entry.IsError = true
			}
			if block.Content != "" && preview.Len() == 0 {
				preview.WriteString(block.Content)
			}
		}
	}

	// If no text/tool_result populated the preview, fall back to the thinking
	// trace so thinking-only assistant messages still surface in the UI.
	if preview.Len() == 0 && thinkingFallback != "" {
		preview.WriteString(thinkingFallback)
	}

	if len(toolNames) > 0 {
		csv := strings.Join(toolNames, ",")
		entry.ToolNamesCSV = &csv
	}

	// Reclassify empty harness entries (progress/direct/queue-operation) to system.
	// These are Claude Code harness artifacts with no content — not user input.
	if entry.Role == RoleUser && plainText == "" && len(contentBlocks) == 0 {
		switch line.Type {
		case "progress", "direct", "queue-operation":
			entry.Role = RoleSystem
			entry.EntryType = EntryTypeSystem
		}
	}

	// Inline reclassification: inspect user-role entries for system-injected content
	// or skill invocations. Uses plainText for string content, or the first text
	// block's Text field for array content (e.g., skill body injections arrive as
	// [{"type":"text","text":"Base directory for this skill: ..."}]).
	textToCheck := plainText
	if textToCheck == "" && len(contentBlocks) > 0 {
		// Extract text for system-injection check. Allow extraction when:
		// (a) ALL blocks are text, OR
		// (b) non-text blocks are all tool_result (e.g., "Tool loaded." + tool results)
		// In case (b), the text is ancillary to tool completion but is still system-injected.
		canCheck := true
		for _, block := range contentBlocks {
			if block.Type != "text" && block.Type != "tool_result" {
				canCheck = false
				break
			}
		}
		if canCheck {
			for _, block := range contentBlocks {
				if block.Type == "text" && block.Text != "" {
					textToCheck = block.Text
					break
				}
			}
		}
	}
	if entry.Role == RoleUser && textToCheck != "" {
		if isSystemInjectedContent(textToCheck) {
			// System-injected entries: reclassify to role=system, entry_type=system.
			// These are <system-reminder>, <task-notification>, and built-in slash commands.
			entry.Role = RoleSystem
			entry.EntryType = EntryTypeSystem
		} else if cmdName, cmdArgs, ok := parseSkillInvocation(textToCheck); ok {
			// Skill invocation: stays role=user but write command metadata into Extra
			// so entries_writer can persist to session_commands.
			extra := `{"command_name":"` + jsonEscapeString(cmdName) + `"`
			if cmdArgs != "" {
				extra += `,"command_args":"` + jsonEscapeString(cmdArgs) + `"`
			}
			extra += `}`
			entry.Extra = &extra
		}
	}

	// Refine entry type based on parsed content.
	if entry.HasToolUse && entry.Role == RoleAssistant {
		entry.EntryType = EntryTypeToolUse
	}
	if entry.HasThinking && entry.EntryType == EntryTypeText && entry.Role == RoleAssistant {
		entry.EntryType = EntryTypeThinking
	}
	if entry.IsError && entry.Role == RoleTool {
		entry.EntryType = EntryTypeToolResult
	}

	// Content preview — truncate to configured limit unless fullContent is set.
	if preview.Len() > 0 {
		var p string
		if fullContent {
			p = preview.String()
		} else {
			p = truncateString(preview.String(), defaults.ContentPreviewLimit)
		}
		entry.ContentPreview = &p
	}

	// Tokens from usage.
	// input_tokens in Claude's JSONL is only the non-cached portion;
	// the bulk of input tokens are in cache_creation and cache_read fields.
	if line.Message.Usage != nil {
		totalIn := line.Message.Usage.InputTokens +
			line.Message.Usage.CacheCreationInputTokens +
			line.Message.Usage.CacheReadInputTokens
		if totalIn > 0 {
			entry.TokensIn = &totalIn
		}
		if line.Message.Usage.OutputTokens > 0 {
			v := line.Message.Usage.OutputTokens
			entry.TokensOut = &v
		}
	}

	// StopReason from stop_reason field (depth=0 only).
	if line.StopReason != "" {
		sr := schema.StopReason(line.StopReason)
		if sr.IsValid() {
			entry.StopReason = &sr
		}
	}

	return entry, true
}

// claudeFullBlock is a content block with full JSON for depth=1 decomposition.
type claudeFullBlock struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"` // tool_result blocks reference their tool_use via this field
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"` // Claude puts the thinking trace text here, not in `text`.
	Content   json.RawMessage `json:"content"`
	Input     json.RawMessage `json:"input"`
	IsError   bool            `json:"is_error"`
}

// decomposeClaudeContentBlocks parses a JSONL line's content blocks into depth=1 entries.
// entryIndex is incremented for each child entry emitted.
// When fullContent is true, ContentPreview is set without truncation.
// askUserCallIDs is the set of tool_call_ids that correspond to AskUserQuestion tool_use blocks
// seen earlier in the transcript; tool_result blocks that reference these IDs keep role=user
// (genuine user responses), while all other tool_result blocks are reclassified to role=tool.
func decomposeClaudeContentBlocks(sessionID SessionID, entryIndex *int, parentIndex int, raw []byte, fullContent bool, askUserCallIDs map[string]bool) []schema.SessionEntry {
	var line struct {
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &line); err != nil {
		return nil
	}

	if len(line.Message.Content) == 0 {
		return nil
	}

	var blocks []claudeFullBlock
	if json.Unmarshal(line.Message.Content, &blocks) != nil {
		return nil // plain string content or unparseable — no depth=1 entries
	}
	if len(blocks) == 0 {
		return nil
	}

	role := Role(line.Message.Role)
	if !role.IsValid() {
		role = RoleAssistant
	}

	var entries []schema.SessionEntry
	for _, block := range blocks {
		entry := schema.SessionEntry{
			SessionID:   sessionID,
			EntryIndex:  *entryIndex,
			Harness:     HarnessClaudeCode,
			Role:        role,
			Depth:       1,
			ParentIndex: &parentIndex,
		}

		switch block.Type {
		case "text":
			pt := block.Type
			entry.PartType = &pt
			entry.EntryType = EntryTypeText
			if block.Text != "" {
				var p string
				if fullContent {
					p = block.Text
				} else {
					p = truncateString(block.Text, defaults.ContentPreviewLimit)
				}
				entry.ContentPreview = &p
				// Reclassify depth=1 text blocks that are system-injected.
				if isSystemInjectedContent(block.Text) {
					entry.Role = RoleSystem
					entry.EntryType = EntryTypeSystem
				}
			}
		case "tool_use":
			pt := block.Type
			entry.PartType = &pt
			entry.EntryType = EntryTypeToolUse
			entry.HasToolUse = true
			if block.Name != "" {
				entry.ToolNamesCSV = &block.Name
				// Classify tool kind from tool name.
				if kind := classifyToolKind(block.Name); kind != "" {
					entry.ToolKind = &kind
				}
			}
			if block.ID != "" {
				entry.ToolCallID = &block.ID
			}
			if len(block.Input) > 0 && string(block.Input) != "null" {
				s := string(block.Input)
				entry.ToolInput = &s
			}
		case "tool_result":
			pt := block.Type
			entry.PartType = &pt
			entry.EntryType = EntryTypeToolResult
			entry.IsError = block.IsError
			if block.ToolUseID != "" {
				entry.ToolCallID = &block.ToolUseID
			}
			// R1/R3: Canonical role — tool_result blocks are reclassified to role=tool,
			// except those that are answers to AskUserQuestion tool_use calls, which
			// are genuine user responses and keep role=user.
			if block.ToolUseID != "" && askUserCallIDs[block.ToolUseID] {
				entry.Role = RoleUser // genuine user answer to AskUserQuestion
			} else {
				entry.Role = RoleTool
			}
			if len(block.Content) > 0 && string(block.Content) != "null" {
				// Content can be a string or structured; store as-is.
				var contentStr string
				if json.Unmarshal(block.Content, &contentStr) == nil {
					entry.ToolOutput = &contentStr
				} else {
					s := string(block.Content)
					entry.ToolOutput = &s
				}
			}
		case "thinking":
			pt := block.Type
			entry.PartType = &pt
			entry.EntryType = EntryTypeThinking
			entry.HasThinking = true
			// Claude transcripts store thinking trace text in `thinking`, not `text`.
			// Fall back to `text` for older fixtures that used the legacy shape.
			text := block.Thinking
			if text == "" {
				text = block.Text
			}
			if text != "" {
				var p string
				if fullContent {
					p = text
				} else {
					p = truncateString(text, defaults.ContentPreviewLimit)
				}
				entry.ContentPreview = &p
			}
			// Track raw thinking length for analytics.
			thinkLen := len(text)
			entry.RawByteLength = &thinkLen
		default:
			pt := block.Type
			entry.PartType = &pt
			entry.EntryType = EntryTypeText
		}

		entries = append(entries, entry)
		*entryIndex++
	}

	return entries
}

// classifyClaudeEntry determines EntryType and Role from a Claude JSONL line.
func classifyClaudeEntry(line claudeIndexLine) (EntryType, Role) {
	role := Role(line.Message.Role)
	if !role.IsValid() {
		// Derive from type field.
		switch line.Type {
		case "user", "human", "direct", "progress", "queue-operation":
			role = RoleUser
		case "assistant":
			role = RoleAssistant
		default:
			role = RoleSystem
		}
	}
	if role == "human" {
		role = RoleUser
	}

	// Determine entry type. The _wrt/ reference uses .Type field; but
	// for the production schema we need our typed EntryType enum.
	switch {
	case line.Type == "system":
		return EntryTypeSystem, RoleSystem
	case role == RoleUser:
		return EntryTypeText, RoleUser
	case role == RoleAssistant:
		// Will be refined to EntryTypeToolUse if content has tool_use blocks.
		// That happens in the caller after content parsing. For now, default text.
		return EntryTypeText, RoleAssistant
	case role == RoleTool:
		return EntryTypeToolResult, RoleTool
	default:
		return EntryTypeText, role
	}
}

// jsonEscapeString returns s with JSON-special characters escaped for embedding
// in a manually-constructed JSON string value. Only the minimum set of characters
// required for valid JSON is escaped: backslash, double-quote, and ASCII controls.
func jsonEscapeString(s string) string {
	b, _ := json.Marshal(s)
	// json.Marshal wraps in double quotes; strip them.
	if len(b) >= 2 {
		return string(b[1 : len(b)-1])
	}
	return s
}

// classifyToolKind maps a tool name (from any provider) to a schema.ToolCallKind.
// Returns an empty ToolCallKind if the tool name is not recognized.
// Tool names are matched case-insensitively for robustness.
func classifyToolKind(toolName string) schema.ToolCallKind {
	lower := strings.ToLower(strings.TrimSpace(toolName))
	switch lower {
	// Read tools
	case "read", "cat", "view", "task_read", "todoread", "memory_read", "issue_read":
		return schema.ToolCallKindRead
	// Edit tools
	case "edit", "write", "patch", "multieditstool", "multiedit", "apply_patch", "todowrite", "memory_write", "issue_write", "notebook_edit":
		return schema.ToolCallKindEdit
	// Delete tools
	case "delete", "remove", "rm":
		return schema.ToolCallKindDelete
	// Move tools
	case "move", "rename", "mv":
		return schema.ToolCallKindMove
	// Search tools
	case "grep", "glob", "search", "find", "ripgrep", "rg", "list", "ls", "list_files", "toolsearch":
		return schema.ToolCallKindSearch
	// Execute tools
	case "bash", "execute", "run", "shell", "terminal", "command", "exec_command", "exec", "sleep":
		return schema.ToolCallKindExecute
	// Think tools
	case "think", "reasoning":
		return schema.ToolCallKindThink
	// Fetch tools
	case "fetch", "webfetch", "curl", "http", "web_search", "websearch":
		return schema.ToolCallKindFetch
	default:
		return schema.ToolCallKindOther
	}
}
