package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/schema"
)

// IgnoredRecordReason describes a known record with no conversation payload.
type IgnoredRecordReason string

const (
	IgnoredRecordControl  IgnoredRecordReason = "control"
	IgnoredRecordMetadata IgnoredRecordReason = "metadata"
	IgnoredRecordMirror   IgnoredRecordReason = "represented_mirror"
)

type IgnoredSourceRecord struct {
	Kind   string
	Reason IgnoredRecordReason
}

type TranscriptCaptureResult struct {
	Entries        []schema.SessionEntry
	IgnoredRecords []IgnoredSourceRecord
	Diagnostics    []DiagnosticEntry
}

// AuthoritativeTranscriptIndexer never certifies the surviving subset of a
// malformed transcript. Compatibility indexers remain separately callable.
type AuthoritativeTranscriptIndexer interface {
	IndexTranscriptForCapture(context.Context, DiscoveredSession) (TranscriptCaptureResult, error)
	IndexTranscriptBytesForCapture(context.Context, DiscoveredSession, []byte) (TranscriptCaptureResult, error)
}

const ContentCaptureRevision = "full-source-v1"

func validateCaptureRole(role string) error {
	switch role {
	case "user", "assistant", "system", "tool":
		return nil
	}
	return fmt.Errorf("unsupported conversation role %q", role)
}

func captureFailure(session DiscoveredSession, line int, err error) error {
	return fmt.Errorf("capture %s session %s at source record %d: %w; complete content was not stored; restore a supported intact transcript and rerun harvest index --force", session.Harness, session.SessionID, line, err)
}

func captureTranscriptFile(ctx context.Context, fs FileSystem, idx AuthoritativeTranscriptIndexer, session DiscoveredSession) (TranscriptCaptureResult, error) {
	if err := ctx.Err(); err != nil {
		return TranscriptCaptureResult{}, err
	}
	data, err := fs.ReadFile(session.SourcePath.String())
	if err != nil {
		return TranscriptCaptureResult{}, captureFailure(session, 0, err)
	}
	return idx.IndexTranscriptBytesForCapture(ctx, session, data)
}

// validateCaptureJSONL examines every record before invoking the compatibility
// assembly kernel; no partial parse escapes this boundary.
func validateCaptureJSONL(ctx context.Context, session DiscoveredSession, data []byte, validate func([]byte) (*IgnoredSourceRecord, error)) ([]IgnoredSourceRecord, error) {
	if session.ContentOmitted {
		return nil, captureFailure(session, 0, fmt.Errorf("retained transcript omitted oversized source records; regenerate harvest from a supported complete source before retrying"))
	}
	var ignored []IgnoredSourceRecord
	for line := 1; len(data) > 0; line++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, rest, found := bytes.Cut(data, []byte{'\n'})
		data = rest
		if !found {
			data = nil
		}
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		if !json.Valid(raw) {
			return nil, captureFailure(session, line, fmt.Errorf("malformed JSON"))
		}
		record, err := validate(raw)
		if err != nil {
			return nil, captureFailure(session, line, err)
		}
		if record != nil {
			ignored = append(ignored, *record)
		}
	}
	return ignored, nil
}

func captureContentOmitted(meta *UnifiedMetadata) bool {
	if meta == nil {
		return false
	}
	for _, diagnostic := range meta.Diagnostics.Warnings {
		if diagnostic.ErrorType == "record_too_large" || diagnostic.ErrorType == string(OpenCodeUnknownPartType) || diagnostic.ErrorType == string(OpenCodeGraphOrphanPartDropped) {
			return true
		}
	}
	return false
}

func validateMessageContent(raw json.RawMessage) error {
	return validateCaptureContent(raw, true)
}

// Cursor's recorded tool blocks can omit IDs; their name and complete input
// still carry meaningful traces. Claude's tool IDs remain required.
func validateCaptureContent(raw json.RawMessage, requireToolID bool) error {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return fmt.Errorf("required message content missing")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return nil
	}
	var blocks []cursorContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return fmt.Errorf("malformed message blocks: %w", err)
	}
	var fields []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for i, block := range blocks {
		switch block.Type {
		case "text":
			if fields[i]["text"] == nil {
				return fmt.Errorf("text block requires text")
			}
		case "thinking":
			if fields[i]["thinking"] == nil && fields[i]["text"] == nil {
				return fmt.Errorf("thinking block requires thinking or text")
			}
		case "tool_use":
			if requireToolID && block.ID == "" {
				return fmt.Errorf("tool_use requires id")
			}
			if block.Name == "" || len(block.Input) == 0 {
				return fmt.Errorf("tool_use requires name and input")
			}
		case "tool_result":
			if block.ToolUseID == "" || len(block.Content) == 0 {
				return fmt.Errorf("tool_result requires tool_use_id and content")
			}
			if err := validateCaptureContent(block.Content, requireToolID); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unrepresented content block %q", block.Type)
		}
	}
	return nil
}

var _ AuthoritativeTranscriptIndexer = (*ClaudeIndexer)(nil)
var _ AuthoritativeTranscriptIndexer = (*CursorIndexer)(nil)
var _ AuthoritativeTranscriptIndexer = (*CodexIndexer)(nil)
var _ AuthoritativeTranscriptIndexer = (*StrikeIndexer)(nil)

func (idx *ClaudeIndexer) IndexTranscriptForCapture(ctx context.Context, s DiscoveredSession) (TranscriptCaptureResult, error) {
	return captureTranscriptFile(ctx, idx.fs, idx, s)
}
func (idx *ClaudeIndexer) IndexTranscriptBytesForCapture(ctx context.Context, s DiscoveredSession, data []byte) (TranscriptCaptureResult, error) {
	ignored, err := validateCaptureJSONL(ctx, s, data, func(raw []byte) (*IgnoredSourceRecord, error) {
		var line claudeIndexLine
		if err := json.Unmarshal(raw, &line); err != nil {
			return nil, err
		}
		switch line.Type {
		case "user", "assistant", "human":
			if line.Message.Role != "" {
				if err := validateCaptureRole(line.Message.Role); err != nil {
					return nil, err
				}
			}
			return nil, validateMessageContent(line.Message.Content)
		case "system":
			content := line.Content
			if len(content) == 0 {
				content = line.Message.Content
			}
			if len(content) == 0 {
				switch line.Subtype {
				case "turn_duration", "compact_boundary", "stop_hook_summary":
					return &IgnoredSourceRecord{Kind: line.Subtype, Reason: IgnoredRecordControl}, nil
				case "api_error":
					if len(line.Error) == 0 || bytes.Equal(line.Error, []byte("null")) {
						return nil, fmt.Errorf("api_error requires error payload")
					}
					return nil, nil
				}
			}
			return nil, validateMessageContent(content)
		case "summary":
			if line.Summary == nil {
				return nil, fmt.Errorf("summary requires text")
			}
			return nil, nil
		case "result":
			return nil, validateMessageContent(line.Result)
		case "progress", "queue-operation", "file-history-snapshot":
			if len(line.Message.Content) > 0 || len(line.Content) > 0 {
				return nil, fmt.Errorf("control record carries unrepresented content")
			}
			return &IgnoredSourceRecord{Kind: line.Type, Reason: IgnoredRecordControl}, nil
		default:
			return nil, fmt.Errorf("unrepresented Claude record %q", line.Type)
		}
	})
	if err != nil {
		return TranscriptCaptureResult{}, err
	}
	copy := *idx
	copy.fullContent = true
	copy.fullDepth = true
	entries, err := copy.parseJSONL(s.SessionID, data)
	if err != nil {
		return TranscriptCaptureResult{}, captureFailure(s, 0, err)
	}
	return TranscriptCaptureResult{Entries: entries, IgnoredRecords: ignored}, nil
}

func (idx *CursorIndexer) IndexTranscriptForCapture(ctx context.Context, s DiscoveredSession) (TranscriptCaptureResult, error) {
	return captureTranscriptFile(ctx, idx.fs, idx, s)
}
func (idx *CursorIndexer) IndexTranscriptBytesForCapture(ctx context.Context, s DiscoveredSession, data []byte) (TranscriptCaptureResult, error) {
	_, err := validateCaptureJSONL(ctx, s, data, func(raw []byte) (*IgnoredSourceRecord, error) {
		var line cursorJSONLLine
		if err := json.Unmarshal(raw, &line); err != nil {
			return nil, err
		}
		if line.Type == "turn_ended" && line.Status == "aborted" {
			if len(line.content()) != 0 {
				return nil, fmt.Errorf("aborted turn carries unrepresented conversation content")
			}
			if line.Error == nil || *line.Error == "" {
				return nil, fmt.Errorf("aborted turn requires recorded error text")
			}
			return nil, nil
		}
		role := firstNonEmpty(line.Role, line.Message.Role)
		if role != "human" {
			if err := validateCaptureRole(role); err != nil {
				return nil, err
			}
		}
		return nil, validateCaptureContent(line.content(), false)
	})
	if err != nil {
		return TranscriptCaptureResult{}, err
	}
	copy := *idx
	copy.fullContent = true
	copy.fullDepth = true
	entries, err := copy.parseJSONL(s.SessionID, data)
	if err != nil {
		return TranscriptCaptureResult{}, captureFailure(s, 0, err)
	}
	return TranscriptCaptureResult{Entries: entries}, nil
}

func (idx *CodexIndexer) IndexTranscriptForCapture(ctx context.Context, s DiscoveredSession) (TranscriptCaptureResult, error) {
	return captureTranscriptFile(ctx, idx.fs, idx, s)
}
func (idx *CodexIndexer) IndexTranscriptBytesForCapture(ctx context.Context, s DiscoveredSession, data []byte) (TranscriptCaptureResult, error) {
	var mirrors []string
	ignored, err := validateCaptureJSONL(ctx, s, data, func(raw []byte) (*IgnoredSourceRecord, error) {
		var env codexRolloutLine
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, err
		}
		switch env.Type {
		case "session_meta", "turn_context":
			return &IgnoredSourceRecord{Kind: env.Type, Reason: IgnoredRecordMetadata}, nil
		case "event_msg":
			var event struct {
				Type    string `json:"type"`
				Message string `json:"message"`
				Text    string `json:"text"`
			}
			if err := json.Unmarshal(env.Payload, &event); err != nil {
				return nil, err
			}
			switch event.Type {
			case "token_count", "task_started", "task_complete", "turn_aborted":
				return &IgnoredSourceRecord{Kind: event.Type, Reason: IgnoredRecordControl}, nil
			case "user_message", "agent_message", "agent_reasoning":
				text := firstNonEmpty(event.Message, event.Text)
				if text == "" {
					return nil, fmt.Errorf("conversation mirror requires text")
				}
				mirrors = append(mirrors, text)
				return &IgnoredSourceRecord{Kind: event.Type, Reason: IgnoredRecordMirror}, nil
			default:
				return nil, fmt.Errorf("unrepresented Codex event message %q", event.Type)
			}
		case codexTypeResponse:
			var payload codexResponseItemPayload
			if err := json.Unmarshal(env.Payload, &payload); err != nil {
				return nil, err
			}
			if _, ok := codexResponseItemEntry(s.SessionID, 0, env, len(raw), true, payload, nil); !ok {
				return nil, fmt.Errorf("unrepresented response item")
			}
			switch payload.Type {
			case codexResponseMessage:
				if payload.Role == "" || payload.Content == nil {
					return nil, fmt.Errorf("message requires role and content")
				}
				if payload.Role != "developer" {
					if err := validateCaptureRole(payload.Role); err != nil {
						return nil, err
					}
				}
				for _, block := range payload.Content {
					if block.Type != "input_text" && block.Type != "output_text" {
						return nil, fmt.Errorf("unrepresented message block %q", block.Type)
					}
				}
				var fields struct {
					Content []map[string]json.RawMessage `json:"content"`
				}
				if err := json.Unmarshal(env.Payload, &fields); err != nil {
					return nil, err
				}
				for _, block := range fields.Content {
					if block["text"] == nil {
						return nil, fmt.Errorf("message text block requires text")
					}
				}
			case codexResponseReasoning:
				for _, block := range payload.Summary {
					if block.Type != "summary_text" {
						return nil, fmt.Errorf("unrepresented reasoning summary block %q", block.Type)
					}
				}
				for _, block := range payload.Content {
					if block.Type != "reasoning_text" && block.Type != "text" {
						return nil, fmt.Errorf("unrepresented reasoning block %q", block.Type)
					}
				}
			case codexResponseFunctionCall, codexResponseCustomCall:
				if payload.CallID == "" || payload.Name == "" || (len(payload.Arguments) == 0 && len(payload.Input) == 0) {
					return nil, fmt.Errorf("tool call requires call_id, name and input")
				}
			case codexResponseFunctionOut, codexResponseCustomCallOut:
				if payload.CallID == "" || len(payload.Output) == 0 {
					return nil, fmt.Errorf("tool output requires call_id and output")
				}
			}
			return nil, nil
		default:
			return nil, fmt.Errorf("unrepresented Codex record %q", env.Type)
		}
	})
	if err != nil {
		return TranscriptCaptureResult{}, err
	}
	copy := *idx
	copy.fullContent = true
	entries, err := copy.parseRollout(s.SessionID, data)
	if err != nil {
		return TranscriptCaptureResult{}, captureFailure(s, 0, err)
	}
	for _, mirror := range mirrors {
		found := false
		for _, entry := range entries {
			if entry.ContentPreview != nil && *entry.ContentPreview == mirror {
				found = true
				break
			}
		}
		if !found {
			return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("conversation event has no equivalent response item"))
		}
	}
	return TranscriptCaptureResult{Entries: entries, IgnoredRecords: ignored}, nil
}

func (idx *StrikeIndexer) IndexTranscriptForCapture(ctx context.Context, s DiscoveredSession) (TranscriptCaptureResult, error) {
	return captureTranscriptFile(ctx, idx.fs, idx, s)
}
func (idx *StrikeIndexer) IndexTranscriptBytesForCapture(ctx context.Context, s DiscoveredSession, data []byte) (TranscriptCaptureResult, error) {
	calls := make(map[string]bool)
	processes := make(map[string]string)
	pending := make(map[string]bool)
	ignored, err := validateCaptureJSONL(ctx, s, data, func(raw []byte) (*IgnoredSourceRecord, error) {
		if strikeRecordTooLarge(raw) {
			return nil, fmt.Errorf("record exceeds Strike format limit")
		}
		var env strikeEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, err
		}
		if !isKnownStrikeEvent(env.Type) {
			return nil, fmt.Errorf("unrepresented Strike event %q", env.Type)
		}
		if len(env.Data) == 0 || bytes.Equal(bytes.TrimSpace(env.Data), []byte("null")) {
			return nil, fmt.Errorf("event requires data object")
		}
		event, err := decodeStrikeEventData(env.Data)
		if err != nil {
			return nil, err
		}
		switch env.Type {
		case strikeEventSessionStarted, strikeEventSessionTitled, strikeEventModelSelected:
			return &IgnoredSourceRecord{Kind: string(env.Type), Reason: IgnoredRecordMetadata}, nil
		case strikeEventToolBegin:
			if event.CallID == "" || firstNonEmpty(event.Name, event.Title) == "" || (len(event.Args) == 0 && len(event.Input) == 0) {
				return nil, fmt.Errorf("tool begin requires callId, name and input")
			}
			calls[event.CallID] = true
		case strikeEventToolEnd:
			if event.CallID == "" {
				return nil, fmt.Errorf("tool end requires callId")
			}
			delete(pending, event.CallID)
		case strikeEventToolOutput:
			if !calls[event.CallID] {
				return nil, fmt.Errorf("tool output has no represented call")
			}
			pending[event.CallID] = true
		case strikeEventProcessStarted:
			if event.ProcessID == "" || !calls[event.CallID] {
				return nil, fmt.Errorf("process requires processId and represented callId")
			}
			processes[event.ProcessID] = event.CallID
		case strikeEventProcessOutput, strikeEventProcessExited:
			call := firstNonEmpty(event.CallID, processes[event.ProcessID])
			if !calls[call] {
				return nil, fmt.Errorf("process output has no represented call")
			}
			pending[call] = true
		case strikeEventUserMessage, strikeEventAssistantText, strikeEventAssistantTextDelta, strikeEventMessageDelta, strikeEventTextDelta:
			if event.Text == "" && event.Delta == "" && len(event.Content) == 0 && len(event.Message) == 0 {
				return nil, fmt.Errorf("conversation event requires text payload")
			}
		case strikeEventReasoning, strikeEventReasoningDelta, strikeEventReasoningDeltaWire, strikeEventThinkingDelta:
			if strikeReasoningText(event) == "" {
				return nil, fmt.Errorf("reasoning event requires text payload")
			}
		}
		return nil, nil
	})
	if err != nil {
		return TranscriptCaptureResult{}, err
	}
	if len(pending) != 0 {
		return TranscriptCaptureResult{}, captureFailure(s, 0, fmt.Errorf("tool output has no terminal event; restore complete tool trace"))
	}
	copy := *idx
	copy.fullContent = true
	return TranscriptCaptureResult{Entries: copy.parse(s.SessionID, data), IgnoredRecords: ignored}, nil
}
