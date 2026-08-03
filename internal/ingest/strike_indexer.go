package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// StrikeIndexer parses Strike's event JSONL into the existing SessionEntry tree.
type StrikeIndexer struct {
	fs          FileSystem
	fullContent bool
}

type StrikeIndexerOption func(*StrikeIndexer)

// WithStrikeFullContent disables preview truncation for detail/export overlays.
func WithStrikeFullContent(enabled bool) StrikeIndexerOption {
	return func(indexer *StrikeIndexer) { indexer.fullContent = enabled }
}

var _ TranscriptIndexer = (*StrikeIndexer)(nil)

// SourceKind reports that Strike's entries come from a single file; every entry is in its bytes.
func (idx *StrikeIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }

// NewStrikeIndexer creates a StrikeIndexer with an injected filesystem.
func NewStrikeIndexer(fs FileSystem, options ...StrikeIndexerOption) *StrikeIndexer {
	indexer := &StrikeIndexer{fs: fs}
	for _, option := range options {
		option(indexer)
	}
	return indexer
}

func (i *StrikeIndexer) IndexTranscript(_ context.Context, session DiscoveredSession) ([]schema.SessionEntry, error) {
	data, err := i.fs.ReadFile(session.SourcePath.String())
	if err != nil {
		return nil, fmt.Errorf("strike indexer: read %s: %w", session.SourcePath, err)
	}
	return i.parse(session.SessionID, data), nil
}

func (i *StrikeIndexer) IndexTranscriptBytes(_ context.Context, session DiscoveredSession, data []byte) ([]schema.SessionEntry, error) {
	return i.parse(session.SessionID, data), nil
}

type strikeContentKind uint8

const (
	strikeContentNone strikeContentKind = iota
	strikeContentReasoning
	strikeContentText
)

type strikeParentState struct {
	position    int
	entryIndex  int
	content     strings.Builder
	contentKind strikeContentKind
}

type strikeCallState struct {
	parentIndex int
	name        string
	outputs     []string
	isError     bool
}

type strikeAssembly struct {
	sessionID       SessionID
	fullContent     bool
	entries         []schema.SessionEntry
	nextIndex       int
	parents         map[string]*strikeParentState
	turnParents     map[string]string
	ordinalParents  map[int]string
	calls           map[string]*strikeCallState
	processCalls    map[string]string
	currentTurnID   string
	currentOrdinal  int
	fallbackCounter int
}

func (i *StrikeIndexer) parse(sessionID SessionID, data []byte) []schema.SessionEntry {
	a := &strikeAssembly{
		sessionID:      sessionID,
		fullContent:    i.fullContent,
		parents:        make(map[string]*strikeParentState),
		turnParents:    make(map[string]string),
		ordinalParents: make(map[int]string),
		calls:          make(map[string]*strikeCallState),
		processCalls:   make(map[string]string),
	}

	forEachStrikeRecord(data, func(_ int, raw []byte) {
		if strikeRecordTooLarge(raw) {
			return
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 {
			return
		}
		var envelope strikeEnvelope
		if json.Unmarshal(trimmed, &envelope) != nil {
			return
		}
		if !isKnownStrikeEvent(envelope.Type) {
			return
		}
		event, err := decodeStrikeEventData(envelope.Data)
		if err != nil {
			return
		}
		timestamp := parseIndexTimestamp(envelope.Time)

		switch envelope.Type {
		case strikeEventUserMessage:
			a.beginTurn(event.TurnID)
			content := strikeEventText(event)
			if content != "" {
				a.appendUser(content, timestamp, len(trimmed))
			}

		case strikeEventTurnStarted:
			a.beginTurn(event.TurnID)

		case strikeEventAssistantText, strikeEventAssistantTextDelta, strikeEventMessageDelta, strikeEventTextDelta:
			content := strikeEventText(event)
			if content == "" {
				return
			}
			key := a.eventKey(event)
			parent := a.ensureParent(key, event, timestamp, len(trimmed), EntryTypeText)
			a.appendParentContent(parent, strikeContentText, content)

		case strikeEventReasoning, strikeEventReasoningDelta, strikeEventReasoningDeltaWire, strikeEventThinkingDelta:
			content := strikeReasoningText(event)
			if content == "" {
				return
			}
			key := a.eventKey(event)
			parent := a.ensureParent(key, event, timestamp, len(trimmed), EntryTypeThinking)
			a.appendParentContent(parent, strikeContentReasoning, content)

		case strikeEventToolBegin:
			a.beginTool(event, timestamp, len(trimmed))

		case strikeEventToolOutput:
			a.appendCallOutput(event.CallID, strikeOutputText(event))

		case strikeEventProcessStarted:
			if event.ProcessID != "" && event.CallID != "" {
				a.processCalls[event.ProcessID] = event.CallID
			}

		case strikeEventProcessOutput:
			callID := event.CallID
			if callID == "" {
				callID = a.processCalls[event.ProcessID]
			}
			a.appendCallOutput(callID, strikeOutputText(event))

		case strikeEventProcessExited:
			callID := event.CallID
			if callID == "" {
				callID = a.processCalls[event.ProcessID]
			}
			a.finishProcess(callID, event)

		case strikeEventToolEnd:
			a.endTool(event, timestamp, len(trimmed))

		case strikeEventUsageReported:
			if parent := a.lookupParent(event); parent != nil {
				in, inKnown, out, outKnown := strikeUsageCounts(event)
				if inKnown {
					a.entries[parent.position].TokensIn = &in
				}
				if outKnown {
					a.entries[parent.position].TokensOut = &out
				}
			}

		case strikeEventTurnCompleted:
			if parent := a.lookupParent(event); parent != nil {
				if reason, ok := strikeStopReason(event.StopReason); ok {
					a.entries[parent.position].StopReason = &reason
				}
			}
		}
	})

	return a.entries
}

func (a *strikeAssembly) beginTurn(turnID string) {
	if turnID != "" {
		if turnID == a.currentTurnID {
			return
		}
		if _, exists := a.turnParents[turnID]; exists {
			a.currentTurnID = turnID
			return
		}
	}
	a.currentOrdinal++
	a.currentTurnID = turnID
}

func (a *strikeAssembly) eventKey(event strikeEventData) string {
	if event.ProviderRequestID != "" {
		return "request:" + event.ProviderRequestID
	}
	if event.TurnID != "" {
		return "turn:" + event.TurnID
	}
	if a.currentOrdinal > 0 {
		return fmt.Sprintf("ordinal:%d", a.currentOrdinal)
	}
	a.fallbackCounter++
	return fmt.Sprintf("event:%d", a.fallbackCounter)
}

func (a *strikeAssembly) ensureParent(key string, event strikeEventData, timestamp *int64, rawLength int, entryType EntryType) *strikeParentState {
	if parent := a.parents[key]; parent != nil {
		return parent
	}
	entry := schema.SessionEntry{
		SessionID:     a.sessionID,
		EntryIndex:    a.nextIndex,
		Harness:       HarnessStrike,
		EntryType:     entryType,
		Role:          RoleAssistant,
		TimestampMs:   timestamp,
		Depth:         0,
		RawByteLength: intPointer(rawLength),
	}
	a.entries = append(a.entries, entry)
	parent := &strikeParentState{position: len(a.entries) - 1, entryIndex: a.nextIndex}
	a.parents[key] = parent
	a.nextIndex++
	if event.TurnID != "" {
		a.turnParents[event.TurnID] = key
	}
	if a.currentOrdinal > 0 {
		a.ordinalParents[a.currentOrdinal] = key
	}
	return parent
}

func (a *strikeAssembly) lookupParent(event strikeEventData) *strikeParentState {
	if event.ProviderRequestID != "" {
		if parent := a.parents["request:"+event.ProviderRequestID]; parent != nil {
			return parent
		}
	}
	if event.TurnID != "" {
		if key := a.turnParents[event.TurnID]; key != "" {
			return a.parents[key]
		}
		if parent := a.parents["turn:"+event.TurnID]; parent != nil {
			return parent
		}
	}
	if a.currentOrdinal > 0 {
		if key := a.ordinalParents[a.currentOrdinal]; key != "" {
			return a.parents[key]
		}
	}
	return nil
}

func (a *strikeAssembly) appendUser(content string, timestamp *int64, rawLength int) {
	preview := a.preview(content)
	a.entries = append(a.entries, schema.SessionEntry{
		SessionID:      a.sessionID,
		EntryIndex:     a.nextIndex,
		Harness:        HarnessStrike,
		EntryType:      EntryTypeText,
		Role:           RoleUser,
		TimestampMs:    timestamp,
		ContentPreview: &preview,
		Depth:          0,
		RawByteLength:  intPointer(rawLength),
	})
	a.nextIndex++
}

func (a *strikeAssembly) appendParentContent(parent *strikeParentState, kind strikeContentKind, content string) {
	if parent.content.Len() > 0 && parent.contentKind != kind {
		parent.content.WriteByte('\n')
	}
	parent.content.WriteString(content)
	parent.contentKind = kind
	preview := a.preview(parent.content.String())
	entry := &a.entries[parent.position]
	entry.ContentPreview = &preview
	if kind == strikeContentReasoning {
		entry.HasThinking = true
		if entry.EntryType != EntryTypeText {
			entry.EntryType = EntryTypeThinking
		}
	} else {
		entry.EntryType = EntryTypeText
	}
}

func (a *strikeAssembly) beginTool(event strikeEventData, timestamp *int64, rawLength int) {
	key := a.eventKey(event)
	parent := a.ensureParent(key, event, timestamp, rawLength, EntryTypeText)
	a.entries[parent.position].HasToolUse = true

	callID := event.CallID
	if callID == "" {
		a.fallbackCounter++
		callID = fmt.Sprintf("strike-call-%d", a.fallbackCounter)
	}
	name := event.Name
	if name == "" {
		name = event.Title
	}
	if name == "" {
		name = "unknown"
	}
	input := strikeToolInput(event.Args)
	if input == nil {
		input = strikeToolInput(event.Input)
	}
	kind := classifyToolKind(name)
	partType := string(strikeEventToolBegin)
	parentIndex := parent.entryIndex
	a.entries = append(a.entries, schema.SessionEntry{
		SessionID:     a.sessionID,
		EntryIndex:    a.nextIndex,
		Harness:       HarnessStrike,
		EntryType:     EntryTypeToolUse,
		Role:          RoleAssistant,
		TimestampMs:   timestamp,
		HasToolUse:    true,
		ToolKind:      &kind,
		ToolNamesCSV:  &name,
		ToolCallID:    &callID,
		Depth:         1,
		ParentIndex:   &parentIndex,
		ToolInput:     input,
		RawByteLength: intPointer(rawLength),
		PartType:      &partType,
	})
	a.nextIndex++
	a.calls[callID] = &strikeCallState{parentIndex: parent.entryIndex, name: name}
}

func (a *strikeAssembly) appendCallOutput(callID, output string) {
	if callID == "" || output == "" {
		return
	}
	if call := a.calls[callID]; call != nil {
		call.outputs = append(call.outputs, output)
	}
}

func (a *strikeAssembly) finishProcess(callID string, event strikeEventData) {
	call := a.calls[callID]
	if call == nil {
		return
	}
	if event.ExitCode != nil && *event.ExitCode != 0 {
		call.isError = true
		call.outputs = append(call.outputs, fmt.Sprintf("Exit code %d", *event.ExitCode))
	}
	if event.Status != "" && event.Status != "exited" {
		call.isError = true
	}
	if errorText := strikeRawValue(event.Error); errorText != nil && *errorText != "" {
		call.isError = true
		call.outputs = append(call.outputs, *errorText)
	}
}

func (a *strikeAssembly) endTool(event strikeEventData, timestamp *int64, rawLength int) {
	callID := event.CallID
	call := a.calls[callID]
	if call == nil {
		key := a.eventKey(event)
		parent := a.ensureParent(key, event, timestamp, rawLength, EntryTypeText)
		a.entries[parent.position].HasToolUse = true
		name := event.Name
		if name == "" {
			name = event.Title
		}
		if name == "" {
			name = "unknown"
		}
		if callID == "" {
			a.fallbackCounter++
			callID = fmt.Sprintf("strike-call-%d", a.fallbackCounter)
		}
		input := strikeToolInput(event.Args)
		if input == nil {
			input = strikeToolInput(event.Input)
		}
		kind := classifyToolKind(name)
		partType := string(strikeEventToolBegin)
		parentIndex := parent.entryIndex
		a.entries = append(a.entries, schema.SessionEntry{
			SessionID:     a.sessionID,
			EntryIndex:    a.nextIndex,
			Harness:       HarnessStrike,
			EntryType:     EntryTypeToolUse,
			Role:          RoleAssistant,
			TimestampMs:   timestamp,
			HasToolUse:    true,
			ToolKind:      &kind,
			ToolNamesCSV:  &name,
			ToolCallID:    &callID,
			Depth:         1,
			ParentIndex:   &parentIndex,
			ToolInput:     input,
			RawByteLength: intPointer(rawLength),
			PartType:      &partType,
		})
		a.nextIndex++
		call = &strikeCallState{parentIndex: parent.entryIndex, name: name}
		a.calls[callID] = call
	}

	if terminal := strikeOutputText(event); terminal != "" {
		call.outputs = append(call.outputs, terminal)
	}
	if event.IsError {
		call.isError = true
	}
	if errorText := strikeRawValue(event.Error); errorText != nil && *errorText != "" {
		call.isError = true
		call.outputs = append(call.outputs, *errorText)
	}
	output := joinStrikeOutput(call.outputs)
	preview := a.preview(output)
	partType := string(strikeEventToolEnd)
	parentIndex := call.parentIndex
	a.entries = append(a.entries, schema.SessionEntry{
		SessionID:      a.sessionID,
		EntryIndex:     a.nextIndex,
		Harness:        HarnessStrike,
		EntryType:      EntryTypeToolResult,
		Role:           RoleTool,
		TimestampMs:    timestamp,
		ContentPreview: &preview,
		IsError:        call.isError,
		ToolCallID:     &callID,
		Depth:          1,
		ParentIndex:    &parentIndex,
		ToolOutput:     &preview,
		RawByteLength:  intPointer(rawLength),
		PartType:       &partType,
	})
	a.nextIndex++
}

func (a *strikeAssembly) preview(content string) string {
	if a.fullContent {
		return content
	}
	return truncateString(content, defaults.ContentPreviewLimit)
}

func strikeEventText(event strikeEventData) string {
	for _, candidate := range []string{event.Delta, event.Text} {
		if candidate != "" {
			return candidate
		}
	}
	for _, raw := range []json.RawMessage{event.Content, event.Message} {
		if value := strikeRawValue(raw); value != nil {
			return *value
		}
	}
	return ""
}

func strikeReasoningText(event strikeEventData) string {
	if event.Delta != "" {
		return event.Delta
	}
	if event.Reasoning != "" {
		return event.Reasoning
	}
	return strikeEventText(event)
}

func strikeOutputText(event strikeEventData) string {
	if event.StreamData != "" {
		return event.StreamData
	}
	for _, raw := range []json.RawMessage{event.Output, event.Result} {
		if value := strikeRawValue(raw); value != nil {
			return *value
		}
	}
	return ""
}

func strikeRawValue(raw json.RawMessage) *string {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return &text
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil {
		for _, key := range []string{"text", "content", "message", "delta", "output", "result"} {
			if nested, ok := object[key]; ok {
				if value := strikeRawValue(nested); value != nil {
					return value
				}
			}
		}
	}
	compact := bytes.Buffer{}
	if json.Compact(&compact, raw) == nil {
		value := compact.String()
		return &value
	}
	value := string(raw)
	return &value
}

func strikeToolInput(raw json.RawMessage) *string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		return &text
	}
	var compact bytes.Buffer
	if json.Compact(&compact, trimmed) == nil {
		value := compact.String()
		return &value
	}
	value := string(trimmed)
	return &value
}

func joinStrikeOutput(parts []string) string {
	var output strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		if output.Len() > 0 {
			current := output.String()
			if current[len(current)-1] != '\n' && part[0] != '\n' {
				output.WriteByte('\n')
			}
		}
		output.WriteString(part)
	}
	return output.String()
}

func strikeStopReason(raw string) (schema.StopReason, bool) {
	normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(raw), "-", "_"))
	var reason schema.StopReason
	switch normalized {
	case "end_turn", "stop", "completed", "complete":
		reason = schema.StopReasonEndTurn
	case "cancelled", "canceled", "abort", "aborted":
		reason = schema.StopReasonCancelled
	case "max_tokens", "length":
		reason = schema.StopReasonMaxTokens
	case "max_turn_requests", "max_turns":
		reason = schema.StopReasonMaxTurnRequests
	case "refusal", "refused":
		reason = schema.StopReasonRefusal
	default:
		return "", false
	}
	return reason, true
}

func intPointer(value int) *int { return &value }
