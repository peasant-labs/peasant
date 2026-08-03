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

// CodexIndexer parses OpenAI Codex CLI JSONL rollouts into SessionEntry rows.
//
// Each rollout line carries {timestamp, type, payload}. The indexer emits one
// SessionEntry per response_item it recognises (messages, function/tool calls,
// reasoning); session_meta, turn_context, and event_msg lines are
// handled by CodexAdapter.ExtractMetadata and skipped here.
//
// Unlike Claude (where one assistant message embeds an array of content
// blocks that decompose into depth=1 child entries), Codex response_items are
// already atomic — one event = one logical action. We therefore emit depth=0
// entries directly; there is no depth=1 decomposition pass.
type CodexIndexer struct {
	fs          FileSystem
	fullContent bool
}

// CodexIndexerOption configures a CodexIndexer.
type CodexIndexerOption func(*CodexIndexer)

// WithCodexFullContent disables ContentPreview truncation so the full message /
// tool input / tool output text is stored. Off by default — values are capped
// at defaults.ContentPreviewLimit to keep DB rows bounded.
func WithCodexFullContent(enabled bool) CodexIndexerOption {
	return func(idx *CodexIndexer) { idx.fullContent = enabled }
}

var _ TranscriptIndexer = (*CodexIndexer)(nil)

// SourceKind reports that Codex's entries come from a single rollout JSONL file; every entry is in its bytes.
func (idx *CodexIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }

// NewCodexIndexer constructs a CodexIndexer with an injected FileSystem.
func NewCodexIndexer(fs FileSystem, opts ...CodexIndexerOption) *CodexIndexer {
	idx := &CodexIndexer{fs: fs}
	for _, opt := range opts {
		opt(idx)
	}
	return idx
}

// IndexTranscript reads a Codex rollout from disk and returns SessionEntry
// rows. Malformed lines are skipped (not fatal); empty rollouts return nil.
func (idx *CodexIndexer) IndexTranscript(_ context.Context, session DiscoveredSession) ([]schema.SessionEntry, error) {
	data, err := idx.fs.ReadFile(string(session.SourcePath))
	if err != nil {
		return nil, fmt.Errorf("codex indexer: read %s: %w", session.SourcePath, err)
	}
	return idx.parseRollout(session.SessionID, data)
}

// IndexTranscriptBytes parses a Codex rollout from in-memory bytes. Used by
// the parallel ingest path to avoid re-reading the file from disk after the
// EXTRACT+WRITE stage.
func (idx *CodexIndexer) IndexTranscriptBytes(_ context.Context, session DiscoveredSession, data []byte) ([]schema.SessionEntry, error) {
	return idx.parseRollout(session.SessionID, data)
}

// parseRollout is the shared kernel for both Index methods. It scans the JSONL,
// dispatches each envelope on its `type` (and the nested `payload.type` for
// response_item / event_msg), and produces SessionEntry rows.
func (idx *CodexIndexer) parseRollout(sessionID SessionID, data []byte) ([]schema.SessionEntry, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	buf := make([]byte, defaults.ScannerInitBuf)
	scanner.Buffer(buf, defaults.ScannerMaxLine)

	var entries []schema.SessionEntry
	entryIndex := 0

	for scanner.Scan() {
		raw := scanner.Bytes()
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 {
			continue
		}

		var env codexRolloutLine
		if err := json.Unmarshal(trimmed, &env); err != nil {
			continue // malformed line — skip silently
		}

		// Only response_item lines become indexed entries. session_meta /
		// turn_context drive ExtractMetadata; event_msg.{token_count,
		// task_*} are session-level signals or UI mirrors of response_item
		// content and would double-count if indexed.
		if env.Type != codexTypeResponse {
			continue
		}

		entry, ok := parseCodexResponseItem(sessionID, entryIndex, env, len(trimmed), idx.fullContent)
		if !ok {
			continue
		}
		entries = append(entries, entry)
		entryIndex++
	}

	if err := scanner.Err(); err != nil {
		return entries, fmt.Errorf("codex indexer: scanner error for %s: %w", sessionID, err)
	}
	return entries, nil
}

// codexResponseItemPayload is the union of fields the indexer reads from any
// response_item payload. Discriminated by Type; unrelated fields stay zero.
type codexResponseItemPayload struct {
	Type    string                `json:"type"`
	Role    string                `json:"role,omitempty"`
	Content []codexMessageContent `json:"content,omitempty"` // message variant
	Name    string                `json:"name,omitempty"`    // function_call / custom_tool_call
	CallID  string                `json:"call_id,omitempty"` // function_call(_output) / custom_tool_call(_output)
	// Arguments is a JSON string for function_call (e.g., `{"cmd":"pwd"}`),
	// or a raw string for custom_tool_call's `input` field. We read both as
	// json.RawMessage so we can preserve the original form for ToolInput.
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"` // function_call_output / custom_tool_call_output
}

// codexMessageContent is one element of response_item.message.content[]. Codex
// uses {"type":"input_text","text":...} for user/developer messages and
// {"type":"output_text","text":...} for assistant messages.
type codexMessageContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// parseCodexResponseItem converts one response_item envelope into a
// SessionEntry. Returns (zero, false) for response_item variants the indexer
// does not surface (e.g. unknown future payload types).
func parseCodexResponseItem(sessionID SessionID, index int, env codexRolloutLine, rawLen int, fullContent bool) (schema.SessionEntry, bool) {
	var p codexResponseItemPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return schema.SessionEntry{}, false
	}

	entry := schema.SessionEntry{
		SessionID:     sessionID,
		EntryIndex:    index,
		Harness:       HarnessCodex,
		Depth:         0,
		RawByteLength: &rawLen,
	}

	if ms := parseTimestampMillis(env.Timestamp); ms > 0 {
		entry.TimestampMs = &ms
	}

	switch p.Type {
	case codexResponseMessage:
		entry.EntryType = EntryTypeText
		switch CodexRole(p.Role) {
		case codexRoleAssistant:
			entry.Role = RoleAssistant
		case codexRoleUser:
			entry.Role = RoleUser
		case codexRoleDeveloper:
			// Developer-role messages are Codex's system-injected prompts
			// (permissions instructions, AGENTS.md context, etc.). Match
			// Claude's treatment of system-injected user content.
			entry.Role = RoleSystem
			entry.EntryType = EntryTypeSystem
		default:
			entry.Role = RoleSystem
			entry.EntryType = EntryTypeSystem
		}
		if text := joinCodexContentText(p.Content); text != "" {
			preview := text
			if !fullContent {
				preview = truncateString(preview, defaults.ContentPreviewLimit)
			}
			entry.ContentPreview = &preview
		}

	case codexResponseReasoning:
		// Reasoning content is encrypted/opaque; record that thinking
		// happened without storing the body, mirroring how Claude indexes
		// `thinking` blocks when content is hidden.
		entry.EntryType = EntryTypeThinking
		entry.Role = RoleAssistant
		entry.HasThinking = true

	case codexResponseFunctionCall, codexResponseCustomCall:
		entry.EntryType = EntryTypeToolUse
		entry.Role = RoleAssistant
		entry.HasToolUse = true
		if p.Name != "" {
			name := p.Name
			entry.ToolNamesCSV = &name
			if kind := classifyToolKind(p.Name); kind != "" {
				entry.ToolKind = &kind
			}
		}
		if p.CallID != "" {
			callID := p.CallID
			entry.ToolCallID = &callID
		}
		// function_call carries arguments as a JSON string; custom_tool_call
		// carries `input` as a raw string. Either way, preserve the source
		// bytes verbatim as ToolInput.
		switch {
		case len(p.Arguments) > 0:
			entry.ToolInput = codexRawJSONToString(p.Arguments)
		case len(p.Input) > 0:
			entry.ToolInput = codexRawJSONToString(p.Input)
		}

	case codexResponseFunctionOut, codexResponseCustomCallOut:
		entry.EntryType = EntryTypeToolResult
		entry.Role = RoleTool
		if p.CallID != "" {
			callID := p.CallID
			entry.ToolCallID = &callID
		}
		if len(p.Output) > 0 {
			out := codexRawJSONToString(p.Output)
			if out != nil {
				// Truncate to the preview limit so a single huge tool
				// output can't bloat one DB row. Full output is preserved
				// in ToolOutput when fullContent is set.
				preview := *out
				if !fullContent {
					preview = truncateString(preview, defaults.ContentPreviewLimit)
				}
				entry.ToolOutput = &preview
				entry.ContentPreview = &preview
			}
		}

	default:
		// Unknown response_item type (likely a Codex CLI version drift).
		// Skip rather than emit a half-classified row.
		return schema.SessionEntry{}, false
	}

	return entry, true
}

// joinCodexContentText flattens response_item.message.content[] text blocks
// into a single string. Codex uses input_text for user/developer messages and
// output_text for assistant messages; we treat both as plain text.
func joinCodexContentText(blocks []codexMessageContent) string {
	if len(blocks) == 0 {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// codexRawJSONToString unwraps a json.RawMessage to a string. If the raw bytes
// are a JSON string literal, the unquoted string is returned. Otherwise the
// raw bytes are returned as-is (so structured tool outputs survive). Returns
// nil for empty/null input so callers can distinguish "absent" from "empty".
func codexRawJSONToString(raw json.RawMessage) *string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return &s
	}
	out := string(raw)
	return &out
}
