package ingest

import (
	"context"
	"encoding/json"
	"fmt"
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
var _ SessionTranscriptSourceResolver = (*OpenCodeIndexer)(nil)

// SourceKind reports that OpenCode's entries come from a tree of message and part files under the provider storage root, so no single file holds its entries and DiscoveredSession.OriginalRoot must survive to reach here.
func (idx *OpenCodeIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceDirectory }

// TranscriptSourceKindFor selects the physical representation for one session.
// The pipeline remains provider-agnostic while OpenCode owns its two source
// shapes.
func (idx *OpenCodeIndexer) TranscriptSourceKindFor(session DiscoveredSession) TranscriptSourceKind {
	if session.TranscriptOrigin == TranscriptOriginOpenCodeLegacySQLite {
		return TranscriptSourceFile
	}
	return TranscriptSourceDirectory
}

// NewOpenCodeIndexer creates an OpenCodeIndexer with an injected FileSystem.
func NewOpenCodeIndexer(fs FileSystem, opts ...OpenCodeIndexerOption) *OpenCodeIndexer {
	idx := &OpenCodeIndexer{fs: fs}
	for _, opt := range opts {
		opt(idx)
	}
	return idx
}

// IndexTranscriptBytes indexes Peasant's managed legacy SQLite projection.
// Legacy JSON sessions retain the directory-based path because their entries
// remain spread across provider-owned message and part files.
func (idx *OpenCodeIndexer) IndexTranscriptBytes(ctx context.Context, session DiscoveredSession, data []byte) ([]schema.SessionEntry, error) {
	if session.TranscriptOrigin != TranscriptOriginOpenCodeLegacySQLite {
		return idx.IndexTranscript(ctx, session)
	}
	return idx.indexLegacyProjection(session, data)
}

func (idx *OpenCodeIndexer) indexLegacyProjection(session DiscoveredSession, data []byte) ([]schema.SessionEntry, error) {
	var projection openCodeLegacyProjection
	if err := json.Unmarshal(data, &projection); err != nil {
		return nil, fmt.Errorf("index managed legacy OpenCode projection for session %q failed while decoding Peasant-owned JSON: %w; no entry rows were stored; re-run harvest to regenerate the projection", session.SessionID, err)
	}
	if projection.Format != openCodeLegacyProjectionFormat || projection.Version != openCodeLegacyProjectionVersion || projection.SessionID != string(session.SessionID) {
		return nil, fmt.Errorf("index managed legacy OpenCode projection for session %q rejected identity format=%q version=%d session_id=%q; expected format=%q version=%d and the selected session ID; no entry rows were stored; re-run harvest with a compatible Peasant version", session.SessionID, projection.Format, projection.Version, projection.SessionID, openCodeLegacyProjectionFormat, openCodeLegacyProjectionVersion)
	}
	messages := make([]openCodeSemanticMessage, 0, len(projection.Messages))
	for _, message := range projection.Messages {
		semantic, err := parseOpenCodeSemanticMessage(message.ID, message.TimeCreated, message.Data)
		if err != nil {
			return nil, fmt.Errorf("index managed legacy OpenCode projection for session %q failed while decoding message row %q; no partial entry set was stored; fix malformed row JSON in the selected database and re-run harvest", session.SessionID, message.ID)
		}
		for _, part := range message.Parts {
			semanticPart, partErr := parseOpenCodeSemanticPart(part.ID, part.TimeCreated, part.Data)
			if partErr != nil {
				return nil, fmt.Errorf("index managed legacy OpenCode projection for session %q part %q failed while decoding selected row JSON: %w; no partial entry set was stored; fix malformed part JSON and re-run harvest", session.SessionID, part.ID, partErr)
			}
			semantic.Parts = append(semantic.Parts, semanticPart)
		}
		messages = append(messages, semantic)
	}
	return idx.indexSemanticMessages(session.SessionID, messages), nil
}

// IndexTranscript reads all msg_*.json files under the message directory for a session
// and returns SessionEntry rows ordered by filename (alphabetical ≈ chronological).
func (idx *OpenCodeIndexer) IndexTranscript(_ context.Context, session DiscoveredSession) ([]schema.SessionEntry, error) {
	if session.TranscriptOrigin == TranscriptOriginOpenCodeLegacySQLite {
		data, err := idx.fs.ReadFile(session.SourcePath.String())
		if err != nil {
			return nil, fmt.Errorf("index managed legacy OpenCode projection for session %q failed while reading %q: %w; no entry rows were stored; re-run harvest to restore the managed artifact", session.SessionID, session.SourcePath, err)
		}
		return idx.indexLegacyProjection(session, data)
	}
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

	messages := make([]openCodeSemanticMessage, 0, len(msgFiles))
	for _, name := range msgFiles {
		data, err := idx.fs.ReadFile(filepath.Join(msgDir, name))
		if err != nil {
			continue
		}
		messageID := strings.TrimSuffix(name, defaults.ExtJSON.String())
		semantic, err := parseOpenCodeSemanticMessage(messageID, 0, data)
		if err != nil {
			continue
		}
		partDir := filepath.Join(storageRoot, defaults.OpenCodeDirPart.String(), messageID)
		for _, partName := range idx.listPartFiles(storageRoot, messageID) {
			partData, readErr := idx.fs.ReadFile(filepath.Join(partDir, partName))
			if readErr != nil {
				continue
			}
			partID := strings.TrimSuffix(partName, defaults.ExtJSON.String())
			part, parseErr := parseOpenCodeSemanticPart(partID, 0, partData)
			if parseErr != nil {
				continue
			}
			semantic.Parts = append(semantic.Parts, part)
		}
		messages = append(messages, semantic)
	}
	return idx.indexSemanticMessages(session.SessionID, messages), nil
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

// openCodeSemanticMessage is the private representation shared by every
// OpenCode source loader. Outer storage formats supply row identity, ordering,
// and raw bytes; indexing consumes only this model.
type openCodeSemanticMessage struct {
	EntryID     string
	TimeCreated int64
	Raw         []byte
	Data        openCodeIndexMsg
	Parts       []openCodeSemanticPart
}

type openCodeSemanticPart struct {
	EntryID     string
	TimeCreated int64
	Raw         []byte
	Data        openCodeIndexPart
}

type openCodeIndexPart struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Name    string          `json:"name"`
	Tool    string          `json:"tool"`
	Text    string          `json:"text"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	Output  json.RawMessage `json:"output"`
	Time    struct {
		Created int64 `json:"created"`
		Start   int64 `json:"start"`
	} `json:"time"`
	State *struct {
		Input struct {
			Name string `json:"name"`
		} `json:"input"`
	} `json:"state"`
}

func parseOpenCodeSemanticMessage(entryID string, outerTimeCreated int64, raw []byte) (openCodeSemanticMessage, error) {
	var data openCodeIndexMsg
	if err := json.Unmarshal(raw, &data); err != nil {
		return openCodeSemanticMessage{}, err
	}
	timestamp := outerTimeCreated
	if timestamp <= 0 {
		timestamp = data.Time.Created
	}
	if entryID == "" {
		entryID = data.ID
	}
	return openCodeSemanticMessage{EntryID: entryID, TimeCreated: timestamp, Raw: append([]byte(nil), raw...), Data: data}, nil
}

func parseOpenCodeSemanticPart(entryID string, outerTimeCreated int64, raw []byte) (openCodeSemanticPart, error) {
	var data openCodeIndexPart
	if err := json.Unmarshal(raw, &data); err != nil {
		return openCodeSemanticPart{}, err
	}
	timestamp := outerTimeCreated
	if timestamp <= 0 {
		timestamp = data.Time.Created
		if timestamp <= 0 {
			timestamp = data.Time.Start
		}
	}
	if entryID == "" {
		entryID = data.ID
	}
	return openCodeSemanticPart{EntryID: entryID, TimeCreated: timestamp, Raw: append([]byte(nil), raw...), Data: data}, nil
}

func (idx *OpenCodeIndexer) indexSemanticMessages(sessionID SessionID, messages []openCodeSemanticMessage) []schema.SessionEntry {
	entries := make([]schema.SessionEntry, 0, len(messages))
	entryIndex := 0
	for _, message := range messages {
		entry := openCodeMessageEntry(sessionID, entryIndex, message, idx.fullContent)
		inspection := inspectOpenCodeSemanticParts(message.Parts)
		if inspection.isSystemEntry {
			entry.Role = RoleSystem
			entry.EntryType = EntryTypeSystem
		} else if inspection.skillName != "" {
			extra := `{"command_name":"` + jsonEscapeString(inspection.skillName) + `"}`
			entry.Extra = &extra
		} else if inspection.toolPartCount > 0 && entry.Role == RoleAssistant {
			entry.HasToolUse = true
			entry.EntryType = EntryTypeToolUse
		}
		if entry.Role != RoleAssistant {
			entry.Extra = removeModelObservation(entry.Extra)
		}
		if entry.ContentPreview == nil {
			if preview := firstOpenCodeSemanticText(message.Parts); preview != "" {
				if !idx.fullContent {
					preview = truncateString(preview, defaults.ContentPreviewLimit)
				}
				entry.ContentPreview = &preview
			}
		}
		parentIndex := entryIndex
		entries = append(entries, entry)
		entryIndex++
		if !idx.fullDepth {
			continue
		}
		for _, part := range message.Parts {
			partEntry, include := idx.openCodePartEntry(sessionID, part, parentIndex, entryIndex, entry.Role, entry.ContentPreview)
			if !include {
				continue
			}
			entries = append(entries, partEntry)
			entryIndex++
		}
	}
	return entries
}

func openCodeMessageEntry(sessionID SessionID, index int, message openCodeSemanticMessage, fullContent bool) schema.SessionEntry {
	role := Role(message.Data.Role)
	if !role.IsValid() {
		role = RoleSystem
	}
	entry := schema.SessionEntry{SessionID: sessionID, EntryIndex: index, Harness: HarnessOpenCode, Role: role, EntryType: classifyOpenCodeEntry(role)}
	if message.TimeCreated > 0 {
		timestamp := message.TimeCreated
		entry.TimestampMs = &timestamp
	}
	if message.EntryID != "" {
		entryID := message.EntryID
		entry.EntryID = &entryID
	}
	rawLength := len(message.Raw)
	entry.RawByteLength = &rawLength
	if message.Data.Tokens != nil {
		if message.Data.Tokens.Input > 0 {
			value := message.Data.Tokens.Input
			entry.TokensIn = &value
		}
		if message.Data.Tokens.Output > 0 {
			value := message.Data.Tokens.Output
			entry.TokensOut = &value
		}
	}
	if preview := extractOpenCodePreview(message.Data.Content); preview != "" {
		if !fullContent {
			preview = truncateString(preview, defaults.ContentPreviewLimit)
		}
		entry.ContentPreview = &preview
	}
	entry.Extra = buildExtraJSON(&message.Data)
	return entry
}

func inspectOpenCodeSemanticParts(parts []openCodeSemanticPart) partInspection {
	var result partInspection
	for _, part := range parts {
		switch part.Data.Type {
		case "compaction", "subtask", "agent":
			result.isSystemEntry = true
		case "tool":
			if part.Data.Tool == "skill" {
				name := ""
				if part.Data.State != nil {
					name = part.Data.State.Input.Name
				}
				if name == "" {
					name = "unknown-skill"
				}
				if !strings.HasPrefix(name, "/") {
					name = "/" + name
				}
				if result.skillName == "" {
					result.skillName = name
				}
			} else {
				result.toolPartCount++
			}
		default:
			result.toolPartCount++
		}
	}
	return result
}

func firstOpenCodeSemanticText(parts []openCodeSemanticPart) string {
	for _, part := range parts {
		if part.Data.Type == "text" && part.Data.Text != "" {
			return part.Data.Text
		}
	}
	return ""
}

func (idx *OpenCodeIndexer) openCodePartEntry(sessionID SessionID, part openCodeSemanticPart, parentIndex, entryIndex int, parentRole schema.Role, parentContent *string) (schema.SessionEntry, bool) {
	entry := schema.SessionEntry{SessionID: sessionID, EntryIndex: entryIndex, Harness: HarnessOpenCode, Depth: 1, ParentIndex: &parentIndex}
	rawLength := len(part.Raw)
	entry.RawByteLength = &rawLength
	partType := part.Data.Type
	entry.PartType = &partType
	if part.TimeCreated > 0 {
		timestamp := part.TimeCreated
		entry.TimestampMs = &timestamp
	}
	switch partType {
	case "tool_use":
		entry.EntryType, entry.Role, entry.HasToolUse = EntryTypeToolUse, RoleAssistant, true
		if value := rawOpenCodeJSON(part.Data.Input); value != "" {
			entry.ToolInput = &value
		}
	case "tool_result":
		entry.EntryType, entry.Role = EntryTypeToolResult, RoleTool
		if value := firstRawOpenCodeJSON(part.Data.Content, part.Data.Output); value != "" {
			entry.ToolOutput = &value
		}
	case "text":
		entry.EntryType, entry.Role = EntryTypeText, parentRole
		if parentRole == RoleUser && parentContent != nil && *parentContent != "" && part.Data.Text == *parentContent {
			return schema.SessionEntry{}, false
		}
		if part.Data.Text != "" {
			text := part.Data.Text
			if !idx.fullContent {
				text = truncateString(text, defaults.ContentPreviewLimit)
			}
			entry.ContentPreview = &text
		}
	default:
		entry.EntryType, entry.Role = EntryTypeText, parentRole
		if partType == "reasoning" {
			entry.EntryType, entry.HasThinking = EntryTypeThinking, true
		}
		if part.Data.Text != "" {
			text := part.Data.Text
			if !idx.fullContent {
				text = truncateString(text, defaults.ContentPreviewLimit)
			}
			entry.ContentPreview = &text
		}
	}
	if part.EntryID != "" {
		entryID := part.EntryID
		entry.EntryID = &entryID
	}
	if partType == "tool_use" || partType == "tool_result" {
		callID := part.Data.ID
		if callID == "" {
			callID = part.EntryID
		}
		if callID != "" {
			entry.ToolCallID = &callID
		}
	}
	if partType == "tool_use" && part.Data.Name != "" {
		toolName := part.Data.Name
		entry.ToolNamesCSV = &toolName
		if kind := classifyToolKind(toolName); kind != "" {
			entry.ToolKind = &kind
		}
	}
	return entry, true
}

func rawOpenCodeJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	compacted, err := json.Marshal(raw)
	if err != nil {
		return ""
	}
	return string(compacted)
}

func firstRawOpenCodeJSON(values ...json.RawMessage) string {
	for _, value := range values {
		if encoded := rawOpenCodeJSON(value); encoded != "" {
			return encoded
		}
	}
	return ""
}

// parseOpenCodeMessage parses a single message JSON into a SessionEntry.
// When fullContent is true, ContentPreview is set without truncation.
func parseOpenCodeMessage(sessionID SessionID, index int, data []byte, fullContent bool) (schema.SessionEntry, bool) {
	message, err := parseOpenCodeSemanticMessage("", 0, data)
	if err != nil {
		return schema.SessionEntry{}, false
	}
	return openCodeMessageEntry(sessionID, index, message, fullContent), true
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

// buildExtraJSON builds the Extra JSON string from message fields.
// Returns nil if no extra fields are present.
func buildExtraJSON(msg *openCodeIndexMsg) *string {
	extra := make(map[string]any)

	if msg.Tokens != nil && msg.Tokens.Reasoning > 0 {
		extra["tokens_reasoning"] = msg.Tokens.Reasoning
	}
	if msg.Role == RoleAssistant.String() && ValidObservedModel(msg.ModelID) {
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
