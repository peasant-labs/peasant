package ingest

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// OpenCodeIndexer parses OpenCode JSON message directories into SessionEntry slices.
type OpenCodeIndexer struct {
	fs          FileSystem
	fullDepth   bool
	fullContent bool
}

// OpenCodeIndexerOption configures an OpenCodeIndexer.
type OpenCodeIndexerOption func(*OpenCodeIndexer)

// WithOpenCodeFullDepth enables full-depth part decomposition when true.
// When enabled, the indexer creates depth=1 entries for each content part
// in addition to the depth=0 message entries.
func WithOpenCodeFullDepth(enabled bool) OpenCodeIndexerOption {
	return func(idx *OpenCodeIndexer) { idx.fullDepth = enabled }
}

// WithOpenCodeFullContent enables full content extraction when set to true.
// When enabled, ContentPreview is populated with the complete content string
// without truncation. Used by the export pipeline to get full transcript content.
// When disabled (default), content is truncated to defaults.ContentPreviewLimit.
func WithOpenCodeFullContent(enabled bool) OpenCodeIndexerOption {
	return func(idx *OpenCodeIndexer) { idx.fullContent = enabled }
}

var _ TranscriptIndexer = (*OpenCodeIndexer)(nil)

// SourceKind reports that OpenCode's entries come from a tree of message and part files under the provider storage root, so no single file holds its entries and DiscoveredSession.OriginalRoot must survive to reach here.
func (idx *OpenCodeIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceDirectory }

// NewOpenCodeIndexer creates an OpenCodeIndexer with an injected FileSystem.
func NewOpenCodeIndexer(fs FileSystem, opts ...OpenCodeIndexerOption) *OpenCodeIndexer {
	idx := &OpenCodeIndexer{fs: fs}
	for _, opt := range opts {
		opt(idx)
	}
	return idx
}

// IndexTranscriptBytes for OpenCode falls back to IndexTranscript because
// OpenCode transcripts are directory-based (msg_*.json files) — there are no
// single-file bytes to pass in-memory. The data argument is unused.
func (idx *OpenCodeIndexer) IndexTranscriptBytes(ctx context.Context, session DiscoveredSession, _ []byte) ([]schema.SessionEntry, error) {
	return idx.IndexTranscript(ctx, session)
}

// IndexTranscript reads all msg_*.json files under the message directory for a session
// and returns SessionEntry rows ordered by filename (alphabetical ≈ chronological).
func (idx *OpenCodeIndexer) IndexTranscript(_ context.Context, session DiscoveredSession) ([]schema.SessionEntry, error) {
	storageRoot := resolveStorageRoot(session)
	msgDir := filepath.Join(storageRoot, defaults.OpenCodeDirMessage.String(), string(session.SessionID))

	dirEntries, err := idx.fs.ReadDir(msgDir)
	if err != nil {
		// No message directory → no entries (not an error).
		return nil, nil
	}

	// Collect and sort message file names for deterministic ordering.
	var msgFiles []string
	for _, de := range dirEntries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, defaults.ExtJSON.String()) {
			continue
		}
		msgFiles = append(msgFiles, name)
	}
	sort.Strings(msgFiles)

	var entries []schema.SessionEntry
	entryIndex := 0
	for _, name := range msgFiles {
		data, err := idx.fs.ReadFile(filepath.Join(msgDir, name))
		if err != nil {
			continue
		}

		entry, ok := parseOpenCodeMessage(session.SessionID, entryIndex, data, idx.fullContent)
		if !ok {
			continue
		}

		msgID := strings.TrimSuffix(name, defaults.ExtJSON.String())

		// Inspect parts for structural classification signals and tool-use detection.
		// This is a single pass over part files that:
		//   1. Counts tool-call parts (for HasToolUse / EntryTypeToolUse refinement).
		//   2. Detects system-structural part types (compaction, subtask, agent) that
		//      reclassify the parent message entry to role=system.
		//   3. Detects skill tool calls (type="tool", tool="skill") that stay role=user
		//      but carry command_name metadata in Extra.
		inspection := idx.inspectParts(storageRoot, msgID)

		if inspection.isSystemEntry {
			// Structural system parts: reclassify parent entry regardless of original role.
			entry.Role = RoleSystem
			entry.EntryType = EntryTypeSystem
		} else if inspection.skillName != "" {
			// Skill tool call: stays role=user; write command_name into Extra so the
			// entries writer can persist to session_commands like other command entries.
			extra := `{"command_name":"` + jsonEscapeString(inspection.skillName) + `"}`
			entry.Extra = &extra
		} else if inspection.toolPartCount > 0 && entry.Role == RoleAssistant {
			// Regular tool-call parts: refine entry type.
			entry.HasToolUse = true
			entry.EntryType = EntryTypeToolUse
		}

		// If no inline content preview, try to extract from part files.
		// OpenCode stores content in part files only; msg.Content is always empty JSON.
		if entry.ContentPreview == nil {
			if preview := idx.extractPreviewFromParts(storageRoot, msgID); preview != "" {
				var p string
				if idx.fullContent {
					p = preview
				} else {
					p = truncateString(preview, defaults.ContentPreviewLimit)
				}
				entry.ContentPreview = &p
			}
		}

		parentIndex := entryIndex
		entries = append(entries, entry)
		entryIndex++

		// Full-depth: decompose part files into depth=1 entries.
		// Pass entry.ContentPreview for echo suppression: text parts whose content
		// matches the parent's preview are duplicates and should be suppressed.
		// For OpenCode, both parent and part content come from part files, so
		// the comparison is between extractPreviewFromParts output and the part text.
		if idx.fullDepth {
			partEntries := idx.indexParts(storageRoot, session.SessionID, msgID, parentIndex, &entryIndex, entry.Role, entry.ContentPreview)
			entries = append(entries, partEntries...)
		}
	}

	return entries, nil
}

// openCodeIndexMsg is the minimal parsed shape for indexing.
type openCodeIndexMsg struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Role      string `json:"role"`
	ModelID   string `json:"modelID"`
	Time      struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Tokens *struct {
		Input      int `json:"input"`
		Output     int `json:"output"`
		Reasoning  int `json:"reasoning"`
		CacheRead  int `json:"cache_read"`
		CacheWrite int `json:"cache_write"`
	} `json:"tokens,omitempty"`
	Content    json.RawMessage `json:"content"`
	CacheRead  int             `json:"cache_read"`
	CacheWrite int             `json:"cache_write"`
}

// parseOpenCodeMessage parses a single message JSON into a SessionEntry.
// When fullContent is true, ContentPreview is set without truncation.
func parseOpenCodeMessage(sessionID SessionID, index int, data []byte, fullContent bool) (schema.SessionEntry, bool) {
	var msg openCodeIndexMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		return schema.SessionEntry{}, false
	}

	role := Role(msg.Role)
	if !role.IsValid() {
		role = RoleSystem
	}

	entry := schema.SessionEntry{
		SessionID:  sessionID,
		EntryIndex: index,
		Harness:    HarnessOpenCode,
		Role:       role,
		EntryType:  classifyOpenCodeEntry(role),
	}

	// Timestamp from msg.Time.Created (unix ms).
	if msg.Time.Created > 0 {
		entry.TimestampMs = &msg.Time.Created
	}

	// Entry ID from msg.ID.
	if msg.ID != "" {
		entry.EntryID = &msg.ID
	}

	// Raw byte length.
	rawLen := len(data)
	entry.RawByteLength = &rawLen

	// Tokens.
	if msg.Tokens != nil {
		if msg.Tokens.Input > 0 {
			v := msg.Tokens.Input
			entry.TokensIn = &v
		}
		if msg.Tokens.Output > 0 {
			v := msg.Tokens.Output
			entry.TokensOut = &v
		}
	}

	// Content preview — try to extract text. Truncate unless fullContent is set.
	preview := extractOpenCodePreview(msg.Content)
	if preview != "" {
		var p string
		if fullContent {
			p = preview
		} else {
			p = truncateString(preview, defaults.ContentPreviewLimit)
		}
		entry.ContentPreview = &p
	}

	// Extra JSON with tokens_reasoning, model_id, cache fields.
	entry.Extra = buildExtraJSON(&msg)

	return entry, true
}

// classifyOpenCodeEntry determines EntryType from Role.
func classifyOpenCodeEntry(role Role) EntryType {
	switch role {
	case RoleUser:
		return EntryTypeText
	case RoleAssistant:
		return EntryTypeText // refined to EntryTypeToolUse if parts exist
	case RoleTool:
		return EntryTypeToolResult
	case RoleSystem:
		return EntryTypeSystem
	default:
		return EntryTypeText
	}
}

// extractOpenCodePreview tries to extract preview text from message content.
// Content can be a string, an array of blocks, or absent.
func extractOpenCodePreview(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}

	// Try as plain string.
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return s
	}

	// Try as array of content blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
	}

	return ""
}

// listPartFilenames returns sorted JSON filenames under {storageRoot}/part/{msgID}/.
// Returns nil if the directory cannot be read.
// This is the package-level variant of (*OpenCodeIndexer).listPartFiles for use
// by types that do not embed OpenCodeIndexer.
func listPartFilenames(fs FileSystem, storageRoot, msgID string) []string {
	partDir := filepath.Join(storageRoot, defaults.OpenCodeDirPart.String(), msgID)
	dirEntries, err := fs.ReadDir(partDir)
	if err != nil {
		return nil
	}
	var partFiles []string
	for _, de := range dirEntries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, defaults.ExtJSON.String()) {
			continue
		}
		partFiles = append(partFiles, name)
	}
	sort.Strings(partFiles)
	return partFiles
}

// listPartFiles returns sorted JSON filenames under {storageRoot}/part/{msgID}/.
// Returns nil if the directory cannot be read.
func (idx *OpenCodeIndexer) listPartFiles(storageRoot, msgID string) []string {
	return listPartFilenames(idx.fs, storageRoot, msgID)
}

// countParts counts JSON files under {root}/part/{msgID}/.
func (idx *OpenCodeIndexer) countParts(storageRoot, msgID string) int {
	return len(idx.listPartFiles(storageRoot, msgID))
}

// partInspection summarises structural signals found across a message's part files.
// It is produced by inspectParts and consumed in IndexTranscript to reclassify
// the parent message entry.
type partInspection struct {
	// isSystemEntry is true if any part has a structural system type:
	// "compaction", "subtask", or "agent". These reclassify the parent
	// message to role=system, entry_type=system.
	isSystemEntry bool

	// skillName is non-empty if a part has type="tool" and tool="skill".
	// The value is the skill name extracted from state.input.name, prefixed
	// with "/" (e.g. "/aura:epoch"). Skill entries stay role=user.
	// isSystemEntry takes precedence: if both are set, isSystemEntry wins.
	skillName string

	// toolPartCount is the count of non-system, non-skill part files.
	// Used for HasToolUse / EntryTypeToolUse refinement when neither
	// isSystemEntry nor skillName is set.
	toolPartCount int
}

// openCodePartMinimal is the minimal shape needed for structural classification.
// Only the fields used in inspectParts are captured; the full part shape is
// handled separately by indexParts for full-depth decomposition.
type openCodePartMinimal struct {
	Type  string `json:"type"`
	Tool  string `json:"tool,omitempty"` // present on type="tool" parts
	State *struct {
		Input struct {
			Name string `json:"name"` // skill name for type="tool", tool="skill"
		} `json:"input"`
	} `json:"state,omitempty"`
}

// inspectParts reads all part files for a message and returns a partInspection
// describing the structural signals found. This is a single-pass scan that
// replaces the earlier countParts call and adds system/skill detection.
func (idx *OpenCodeIndexer) inspectParts(storageRoot, msgID string) partInspection {
	partFiles := idx.listPartFiles(storageRoot, msgID)
	if len(partFiles) == 0 {
		return partInspection{}
	}

	partDir := filepath.Join(storageRoot, defaults.OpenCodeDirPart.String(), msgID)
	var result partInspection
	for _, name := range partFiles {
		data, err := idx.fs.ReadFile(filepath.Join(partDir, name))
		if err != nil {
			continue
		}

		var part openCodePartMinimal
		if err := json.Unmarshal(data, &part); err != nil {
			continue
		}

		switch part.Type {
		case "compaction", "subtask", "agent":
			// Structural system signals take precedence over everything else.
			result.isSystemEntry = true
		case "tool":
			if part.Tool == "skill" {
				// Skill invocation: extract name from state.input.name.
				var skillName string
				if part.State != nil {
					skillName = part.State.Input.Name
				}
				if skillName == "" {
					skillName = "unknown-skill"
				}
				if !strings.HasPrefix(skillName, "/") {
					skillName = "/" + skillName
				}
				// Only set if not already a system entry and not yet found a skill.
				if result.skillName == "" {
					result.skillName = skillName
				}
			} else {
				result.toolPartCount++
			}
		default:
			// text, tool_use, tool_result, etc. — contribute to toolPartCount.
			result.toolPartCount++
		}
	}
	return result
}

// indexParts reads part files for a message and creates depth=1 SessionEntry rows.
// Part files are sorted by filename for deterministic ordering.
// entryIndex is a pointer that is incremented for each part entry created.
// parentRole is the role of the depth=0 parent entry; parentContent is the parent's
// ContentPreview (may be nil/empty). Text parts whose content exactly matches
// parentContent are suppressed to avoid echo entries.
func (idx *OpenCodeIndexer) indexParts(storageRoot string, sessionID SessionID, msgID string, parentIndex int, entryIndex *int, parentRole schema.Role, parentContent *string) []schema.SessionEntry {
	partFiles := idx.listPartFiles(storageRoot, msgID)
	if partFiles == nil {
		return nil
	}

	partDir := filepath.Join(storageRoot, defaults.OpenCodeDirPart.String(), msgID)
	var entries []schema.SessionEntry
	for _, name := range partFiles {
		data, err := idx.fs.ReadFile(filepath.Join(partDir, name))
		if err != nil {
			continue
		}

		var partMap map[string]any
		if err := json.Unmarshal(data, &partMap); err != nil {
			continue
		}

		entry := schema.SessionEntry{
			SessionID:   sessionID,
			EntryIndex:  *entryIndex,
			Harness:     HarnessOpenCode,
			Depth:       1,
			ParentIndex: &parentIndex,
		}

		// Raw byte length of the part file.
		rawLen := len(data)
		entry.RawByteLength = &rawLen

		// Detect type from the "type" field.
		partType, _ := partMap["type"].(string)
		switch partType {
		case "tool_use":
			pt := partType
			entry.PartType = &pt
			entry.EntryType = EntryTypeToolUse
			entry.Role = RoleAssistant
			entry.HasToolUse = true
			// Set ToolInput from the part JSON (the entire part is the tool invocation).
			inputJSON := marshalPartInput(partMap)
			if inputJSON != "" {
				entry.ToolInput = &inputJSON
			}
		case "tool_result":
			pt := partType
			entry.PartType = &pt
			entry.EntryType = EntryTypeToolResult
			entry.Role = RoleTool
			// Set ToolOutput from the part JSON.
			outputJSON := marshalPartOutput(partMap)
			if outputJSON != "" {
				entry.ToolOutput = &outputJSON
			}
		case "text":
			entry.EntryType = EntryTypeText
			// Extract text for echo-suppression check and preview population.
			text, _ := partMap["text"].(string)
			// Suppress echo: for user messages only, if the text part exactly matches
			// the parent's ContentPreview, this is a duplicate echo — skip it.
			// Assistant text parts are real content blocks, not echoes.
			if parentRole == RoleUser && parentContent != nil && *parentContent != "" && text == *parentContent {
				continue
			}
			pt := partType
			entry.PartType = &pt
			entry.Role = parentRole
			// Populate preview if text is non-empty.
			if text != "" {
				var p string
				if idx.fullContent {
					p = text
				} else {
					p = truncateString(text, defaults.ContentPreviewLimit)
				}
				entry.ContentPreview = &p
			}
		default:
			pt := partType
			entry.PartType = &pt
			entry.EntryType = EntryTypeText
			entry.Role = parentRole
			// Semantic override: OpenCode "reasoning" maps to thinking entry type.
			if partType == "reasoning" {
				entry.EntryType = EntryTypeThinking
				entry.HasThinking = true
			}
			text, _ := partMap["text"].(string)
			if text != "" {
				var p string
				if idx.fullContent {
					p = text
				} else {
					p = truncateString(text, defaults.ContentPreviewLimit)
				}
				entry.ContentPreview = &p
			}
		}

		// Extract "id" field as EntryID and ToolCallID for tool_use/tool_result.
		if id, ok := partMap["id"].(string); ok && id != "" {
			entry.EntryID = &id
			if partType == "tool_use" || partType == "tool_result" {
				entry.ToolCallID = &id
			}
		}

		// Extract tool name for tool_use parts.
		if partType == "tool_use" {
			if toolName, ok := partMap["name"].(string); ok && toolName != "" {
				entry.ToolNamesCSV = &toolName
				// Classify tool kind from tool name.
				if kind := classifyToolKind(toolName); kind != "" {
					entry.ToolKind = &kind
				}
			}
		}

		entries = append(entries, entry)
		*entryIndex++
	}

	return entries
}

// extractPreviewFromParts reads the part directory for a message and returns the
// text of the first text-type part, truncated to ContentPreviewLimit.
// Returns empty string if no text parts exist or the directory cannot be read.
func (idx *OpenCodeIndexer) extractPreviewFromParts(storageRoot, msgID string) string {
	partFiles := idx.listPartFiles(storageRoot, msgID)
	if partFiles == nil {
		return ""
	}

	partDir := filepath.Join(storageRoot, defaults.OpenCodeDirPart.String(), msgID)
	for _, name := range partFiles {
		data, err := idx.fs.ReadFile(filepath.Join(partDir, name))
		if err != nil {
			continue
		}
		var part map[string]any
		if err := json.Unmarshal(data, &part); err != nil {
			continue
		}
		if typ, _ := part["type"].(string); typ != "text" {
			continue
		}
		if text, _ := part["text"].(string); text != "" {
			return text
		}
	}
	return ""
}

// marshalPartInput extracts the "input" field from a tool_use part and marshals it to JSON.
func marshalPartInput(partMap map[string]any) string {
	input, ok := partMap["input"]
	if !ok {
		return ""
	}
	b, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	return string(b)
}

// marshalPartOutput extracts the "content" or "output" field from a tool_result part
// and marshals it to JSON.
func marshalPartOutput(partMap map[string]any) string {
	// Try "content" first (standard Anthropic tool_result shape), then "output".
	for _, key := range []string{"content", "output"} {
		if v, ok := partMap[key]; ok {
			b, err := json.Marshal(v)
			if err != nil {
				continue
			}
			return string(b)
		}
	}
	return ""
}

// buildExtraJSON builds the Extra JSON string from message fields.
// Returns nil if no extra fields are present.
func buildExtraJSON(msg *openCodeIndexMsg) *string {
	extra := make(map[string]any)

	if msg.Tokens != nil && msg.Tokens.Reasoning > 0 {
		extra["tokens_reasoning"] = msg.Tokens.Reasoning
	}
	if msg.Role == RoleAssistant.String() && validObservedModel(msg.ModelID) {
		extra["model_id"] = msg.ModelID
	}

	// Cache fields: prefer tokens-level, fall back to top-level message fields.
	cacheRead := 0
	cacheWrite := 0
	if msg.Tokens != nil {
		cacheRead = msg.Tokens.CacheRead
		cacheWrite = msg.Tokens.CacheWrite
	}
	if cacheRead == 0 {
		cacheRead = msg.CacheRead
	}
	if cacheWrite == 0 {
		cacheWrite = msg.CacheWrite
	}
	if cacheRead > 0 {
		extra["cache_read"] = cacheRead
	}
	if cacheWrite > 0 {
		extra["cache_write"] = cacheWrite
	}

	if len(extra) == 0 {
		return nil
	}

	b, err := json.Marshal(extra)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}
