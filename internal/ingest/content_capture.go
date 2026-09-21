package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

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

// Strict vocabularies for the record-kind drift test. Each slice is the
// single source of truth its strict path gates on: the dispatch below refuses
// an unlisted discriminator before reaching any case body, so adding an
// accepted kind means extending the slice, and the drift test fails until the
// registry follows. Case labels may repeat slice literals; the slice governs
// reachability, never the label.
func claudeStrictRecordKinds() []string {
	return []string{"user", "assistant", "human", "system", "summary", "result", "progress", "queue-operation", "file-history-snapshot", "last-prompt"}
}

func claudeStrictSystemSubtypes() []string {
	return []string{"turn_duration", "compact_boundary", "stop_hook_summary", "api_error"}
}

// captureContentBlockKinds names the content blocks the strict capture path
// validates for one harness. Claude alone admits tool_reference control
// blocks; every other harness refuses anything outside the shared four.
func captureContentBlockKinds(harness Harness) []string {
	kinds := []string{"text", "thinking", "tool_use", "tool_result"}
	if harness == HarnessClaudeCode {
		kinds = append(kinds, "tool_reference")
	}
	return kinds
}

// captureRoleKinds names the conversation roles the strict capture path
// accepts across harnesses. Cursor additionally accepts human-role lines,
// which read as user turns.
func captureRoleKinds() []string {
	return []string{"user", "assistant", "system", "tool"}
}

func codexStrictEnvelopeKinds() []string {
	return []string{codexTypeSessionMeta, codexTypeTurnContext, codexTypeEventMsg, codexTypeResponse}
}

func codexStrictEventMsgKinds() []string {
	return []string{"token_count", "task_started", "task_complete", "turn_aborted", "user_message", "agent_message", "agent_reasoning"}
}

func codexStrictResponsePayloadKinds() []string {
	return []string{codexResponseMessage, codexResponseReasoning, codexResponseFunctionCall, codexResponseCustomCall, codexResponseFunctionOut, codexResponseCustomCallOut}
}

func codexStrictMessageBlockKinds() []string {
	return []string{"input_text", "output_text"}
}

func codexStrictReasoningSummaryKinds() []string {
	return []string{"summary_text"}
}

func codexStrictReasoningContentKinds() []string {
	return []string{"reasoning_text", "text"}
}

// cursorStrictRecordKinds names the Cursor record types with dedicated
// handling. Cursor dispatches on role rather than type, so turn_ended is the
// only type-gated branch; every other line is validated by role and blocks.
func cursorStrictRecordKinds() []string {
	return []string{"turn_ended"}
}

func isCursorSpecialRecordKind(kind string) bool {
	return slices.Contains(cursorStrictRecordKinds(), kind)
}

type TranscriptCaptureResult struct {
	Entries        []schema.SessionEntry
	IgnoredRecords []IgnoredSourceRecord
	Diagnostics    []DiagnosticEntry
	// RetainedUnknown records uninterpreted occurrences, not affected sessions.
	// They are also embedded in Entries for transactional local persistence.
	RetainedUnknown []RetainedUnknown
}

// AuthoritativeTranscriptIndexer never certifies the surviving subset of a
// malformed transcript. Compatibility indexers remain separately callable.
type AuthoritativeTranscriptIndexer interface {
	IndexTranscriptForCapture(context.Context, DiscoveredSession) (TranscriptCaptureResult, error)
	IndexTranscriptBytesForCapture(context.Context, DiscoveredSession, []byte) (TranscriptCaptureResult, error)
}

func validateCaptureRole(role string) error {
	if slices.Contains(captureRoleKinds(), role) {
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
//
// A well-formed record kind this build does not represent is a REFUSAL that a
// later build might lift; any other validation failure is corruption the user
// can act on. One refusal must not MASK corruption later in the file, so
// validation continues past it: an ordinary error wins wherever it appears,
// and the first refusal is returned only when the whole transcript is
// otherwise valid. A transcript whose records are all unrepresented still
// refuses, so nothing is silently certified from an empty projection.
func validateCaptureJSONL(ctx context.Context, session DiscoveredSession, data []byte, validate func([]byte) (*IgnoredSourceRecord, error)) ([]IgnoredSourceRecord, error) {
	if session.ContentOmitted {
		return nil, captureFailure(session, 0, fmt.Errorf("the retained transcript omits source records that ingest left out before writing it, so a strict capture cannot certify it as the source's own"))
	}
	var ignored []IgnoredSourceRecord
	var refusal *UnrepresentedRecordError
	refusalLine := 0
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
			var unrepresented *UnrepresentedRecordError
			if errors.As(err, &unrepresented) {
				if refusal == nil {
					refusal, refusalLine = unrepresented, line
				}
				continue
			}
			return nil, captureFailure(session, line, err)
		}
		if record != nil {
			ignored = append(ignored, *record)
		}
	}
	if refusal != nil {
		return nil, captureFailure(session, refusalLine, refusal)
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

func validateMessageContent(harness Harness, raw json.RawMessage) error {
	return validateCaptureContent(harness, raw, true)
}

// Cursor's recorded tool blocks can omit IDs; their name and complete input
// still carry meaningful traces. Claude's tool IDs remain required.
//
// A content block of a kind this build does not represent is a REFUSAL this
// build cannot lift (UnrepresentedRecordError), not corruption: the tolerant
// projection still stores the rest of the session, and the refusal records why
// the capture stays incomplete so the session settles until a newer build can
// represent the block.
func validateCaptureContent(harness Harness, raw json.RawMessage, requireToolID bool) error {
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
	// The same corruption-wins rule as validateCaptureJSONL applies WITHIN a
	// content-block array: one unrepresented block must not mask a malformed
	// known block after it. The first refusal is remembered, every remaining
	// block is still validated (including recursive tool_result content), an
	// ordinary error is returned wherever it appears, and the refusal is
	// returned only when the whole array is otherwise valid.
	var refusal *UnrepresentedRecordError
	for i, block := range blocks {
		if block.Type == "" {
			return fmt.Errorf("content block lacks its type")
		}
		if !slices.Contains(captureContentBlockKinds(harness), block.Type) {
			if refusal == nil {
				refusal = &UnrepresentedRecordError{Harness: harness, Kind: block.Type}
			}
			continue
		}
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
			err := validateCaptureContent(harness, block.Content, requireToolID)
			if err == nil {
				continue
			}
			var unrepresented *UnrepresentedRecordError
			if !errors.As(err, &unrepresented) {
				return err
			}
			if refusal == nil {
				refusal = unrepresented
			}
		case "tool_reference":
			// Claude attaches a tool_reference block when it loads a deferred
			// tool's schema. It is a control block with no conversation
			// content, so it is accepted as long as it names its tool. The
			// kinds gate above admits it for Claude only.
			if block.ToolName == "" && block.Name == "" {
				return fmt.Errorf("tool_reference requires tool_name")
			}
		}
	}
	if refusal != nil {
		return refusal
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
	ignored, err := validateRetainingJSONL(ctx, s, data, func(raw []byte) (*IgnoredSourceRecord, error) {
		var line claudeIndexLine
		if err := json.Unmarshal(raw, &line); err != nil {
			return nil, err
		}
		if line.Type == "" {
			// A missing discriminator is corruption, not vocabulary:
			// settling it would hide an actionable malformed record.
			return nil, fmt.Errorf("record lacks its type")
		}
		// The kinds slice governs reachability: a case body below cannot run
		// for an unlisted kind, so extending the vocabulary means extending
		// the slice the drift test walks.
		if !slices.Contains(claudeStrictRecordKinds(), line.Type) && !isClaudeControlRecordType(line.Type) {
			return nil, &UnrepresentedRecordError{Harness: HarnessClaudeCode, Kind: line.Type}
		}
		switch line.Type {
		case "user", "assistant", "human":
			if line.Message.Role != "" {
				if err := validateCaptureRole(line.Message.Role); err != nil {
					return nil, err
				}
			}
			return nil, validateMessageContent(HarnessClaudeCode, line.Message.Content)
		case "system":
			content := line.Content
			if len(content) == 0 {
				content = line.Message.Content
			}
			if len(content) == 0 && !slices.Contains(claudeStrictSystemSubtypes(), line.Subtype) {
				return nil, validateMessageContent(HarnessClaudeCode, content)
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
			return nil, validateMessageContent(HarnessClaudeCode, content)
		case "summary":
			if line.Summary == nil {
				return nil, fmt.Errorf("summary requires text")
			}
			return nil, nil
		case "result":
			return nil, validateMessageContent(HarnessClaudeCode, line.Result)
		case "progress", "queue-operation", "file-history-snapshot":
			if len(line.Message.Content) > 0 || len(line.Content) > 0 {
				return nil, &UnrepresentedRecordError{Harness: HarnessClaudeCode, Kind: line.Type}
			}
			return &IgnoredSourceRecord{Kind: line.Type, Reason: IgnoredRecordControl}, nil
		case "last-prompt":
			// A mirror of the prompt already represented by the user turn. It
			// carries no payload of its own, so it is recorded as metadata.
			return &IgnoredSourceRecord{Kind: line.Type, Reason: IgnoredRecordMetadata}, nil
		default:
			// A represented control record. The indexer retains its kind and
			// payload as a depth=0 row, so the capture can certify. The gate
			// above refused every other unlisted kind; the refusal below is
			// defense in depth.
			if isClaudeControlRecordType(line.Type) {
				return nil, nil
			}
			// A well-formed record kind this build does not represent. The
			// refusal is typed so the ordinary index path can store the
			// represented entries as an incomplete capture with a terminal
			// code, bump the producer and settle (see permanentRefusalCode).
			return nil, &UnrepresentedRecordError{Harness: HarnessClaudeCode, Kind: line.Type}
		}
	})
	if err != nil {
		return TranscriptCaptureResult{}, err
	}
	copy := *idx
	copy.fullContent = true
	copy.fullDepth = true
	copy.retainUnknown = true
	entries, err := copy.parseJSONL(s.SessionID, data)
	if err != nil {
		return TranscriptCaptureResult{}, captureFailure(s, 0, err)
	}
	unknown, err := retainedUnknownEntries(entries)
	return TranscriptCaptureResult{Entries: entries, IgnoredRecords: ignored, RetainedUnknown: unknown}, err
}

func (idx *CursorIndexer) IndexTranscriptForCapture(ctx context.Context, s DiscoveredSession) (TranscriptCaptureResult, error) {
	return captureTranscriptFile(ctx, idx.fs, idx, s)
}
func (idx *CursorIndexer) IndexTranscriptBytesForCapture(ctx context.Context, s DiscoveredSession, data []byte) (TranscriptCaptureResult, error) {
	_, err := validateRetainingJSONL(ctx, s, data, func(raw []byte) (*IgnoredSourceRecord, error) {
		var line cursorJSONLLine
		if err := json.Unmarshal(raw, &line); err != nil {
			return nil, err
		}
		if isCursorSpecialRecordKind(line.Type) && line.Status == "aborted" {
			if len(line.content()) != 0 {
				return nil, fmt.Errorf("aborted turn carries unrepresented conversation content")
			}
			if line.Error == nil || *line.Error == "" {
				return nil, fmt.Errorf("aborted turn requires recorded error text")
			}
			return nil, nil
		}
		role := firstNonEmpty(line.Role, line.Message.Role)
		if !slices.Contains(cursorCaptureRoleKinds(), role) {
			return nil, fmt.Errorf("unsupported conversation role %q", role)
		}
		return nil, validateCaptureContent(HarnessCursor, line.content(), false)
	})
	if err != nil {
		return TranscriptCaptureResult{}, err
	}
	copy := *idx
	copy.fullContent = true
	copy.fullDepth = true
	copy.retainUnknown = true
	entries, err := copy.parseJSONL(s.SessionID, data)
	if err != nil {
		return TranscriptCaptureResult{}, captureFailure(s, 0, err)
	}
	unknown, err := retainedUnknownEntries(entries)
	return TranscriptCaptureResult{Entries: entries, RetainedUnknown: unknown}, err
}

func (idx *CodexIndexer) IndexTranscriptForCapture(ctx context.Context, s DiscoveredSession) (TranscriptCaptureResult, error) {
	return captureTranscriptFile(ctx, idx.fs, idx, s)
}
func (idx *CodexIndexer) IndexTranscriptBytesForCapture(ctx context.Context, s DiscoveredSession, data []byte) (TranscriptCaptureResult, error) {
	var mirrors []string
	ignored, err := validateRetainingJSONL(ctx, s, data, func(raw []byte) (*IgnoredSourceRecord, error) {
		prepared, _, err := prepareCodexRecord(raw, UnknownSourcePosition{Line: 1}, false)
		if err != nil {
			return nil, err
		}
		if prepared == nil {
			return nil, nil
		}
		raw = prepared
		var env codexRolloutLine
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, err
		}
		if env.Type == "" {
			return nil, fmt.Errorf("rollout record lacks its type")
		}
		// The envelope slice governs reachability, as with the Claude record
		// gate above: extending the vocabulary means extending the slice.
		if !slices.Contains(codexStrictEnvelopeKinds(), env.Type) {
			return nil, &UnrepresentedRecordError{Harness: HarnessCodex, Kind: env.Type}
		}
		switch env.Type {
		case codexTypeSessionMeta, codexTypeTurnContext:
			return &IgnoredSourceRecord{Kind: env.Type, Reason: IgnoredRecordMetadata}, nil
		case codexTypeEventMsg:
			var event struct {
				Type    string `json:"type"`
				Message string `json:"message"`
				Text    string `json:"text"`
			}
			if err := json.Unmarshal(env.Payload, &event); err != nil {
				return nil, err
			}
			if event.Type == "" {
				return nil, fmt.Errorf("event message lacks its type")
			}
			// The event slice governs reachability: a case body below cannot
			// run for an unlisted event type.
			if !slices.Contains(codexStrictEventMsgKinds(), event.Type) {
				return nil, &UnrepresentedRecordError{Harness: HarnessCodex, Kind: event.Type}
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
				return nil, &UnrepresentedRecordError{Harness: HarnessCodex, Kind: event.Type}
			}
		case codexTypeResponse:
			var payload codexResponseItemPayload
			if err := json.Unmarshal(env.Payload, &payload); err != nil {
				return nil, err
			}
			if _, ok := codexResponseItemEntry(s.SessionID, 0, env, len(raw), true, payload, nil); !ok {
				return nil, &UnrepresentedRecordError{Harness: HarnessCodex, Kind: "response_item"}
			}
			// The payload slice governs reachability: entry-ok holds exactly
			// for these six shapes, so the gate below is behavior-identical
			// and a new shape must join the walked slice to validate further.
			if !slices.Contains(codexStrictResponsePayloadKinds(), payload.Type) {
				return nil, &UnrepresentedRecordError{Harness: HarnessCodex, Kind: "response_item"}
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
				var fields struct {
					Content []map[string]json.RawMessage `json:"content"`
				}
				if err := json.Unmarshal(env.Payload, &fields); err != nil {
					return nil, err
				}
				// Corruption wins inside the block array too: an unknown block
				// must not mask a known text block missing its text later.
				var refusal *UnrepresentedRecordError
				for i, block := range payload.Content {
					if block.Type == "" {
						return nil, fmt.Errorf("message block lacks its type")
					}
					if !slices.Contains(codexStrictMessageBlockKinds(), block.Type) {
						if refusal == nil {
							refusal = &UnrepresentedRecordError{Harness: HarnessCodex, Kind: block.Type}
						}
						continue
					}
					if i >= len(fields.Content) || fields.Content[i]["text"] == nil {
						return nil, fmt.Errorf("message text block requires text")
					}
				}
				if refusal != nil {
					return nil, refusal
				}
			case codexResponseReasoning:
				var refusal *UnrepresentedRecordError
				for _, block := range payload.Summary {
					if block.Type == "" {
						return nil, fmt.Errorf("reasoning summary block lacks its type")
					}
					if !slices.Contains(codexStrictReasoningSummaryKinds(), block.Type) && refusal == nil {
						refusal = &UnrepresentedRecordError{Harness: HarnessCodex, Kind: block.Type}
					}
				}
				for _, block := range payload.Content {
					if block.Type == "" {
						return nil, fmt.Errorf("reasoning block lacks its type")
					}
					if !slices.Contains(codexStrictReasoningContentKinds(), block.Type) && refusal == nil {
						refusal = &UnrepresentedRecordError{Harness: HarnessCodex, Kind: block.Type}
					}
				}
				if refusal != nil {
					return nil, refusal
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
			// Unreachable: the envelope gate above refused every unlisted
			// type. Kept refuse-closed rather than open.
			return nil, &UnrepresentedRecordError{Harness: HarnessCodex, Kind: env.Type}
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
	unknown, err := retainedUnknownEntries(entries)
	return TranscriptCaptureResult{Entries: entries, IgnoredRecords: ignored, RetainedUnknown: unknown}, err
}

func (idx *StrikeIndexer) IndexTranscriptForCapture(ctx context.Context, s DiscoveredSession) (TranscriptCaptureResult, error) {
	return captureTranscriptFile(ctx, idx.fs, idx, s)
}
func (idx *StrikeIndexer) IndexTranscriptBytesForCapture(ctx context.Context, s DiscoveredSession, data []byte) (TranscriptCaptureResult, error) {
	calls := make(map[string]bool)
	processes := make(map[string]string)
	pending := make(map[string]bool)
	ignored, err := validateRetainingJSONL(ctx, s, data, func(raw []byte) (*IgnoredSourceRecord, error) {
		if strikeRecordTooLarge(raw, 0) {
			return nil, fmt.Errorf("record exceeds Strike format limit")
		}
		var env strikeEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, err
		}
		if !isKnownStrikeEvent(env.Type) {
			return nil, fmt.Errorf("event lacks its type")
		}
		if len(env.Data) == 0 || bytes.Equal(bytes.TrimSpace(env.Data), []byte("null")) {
			return nil, fmt.Errorf("event requires data object")
		}
		event, err := decodeStrikeEventData(env.Data)
		if err != nil {
			return nil, err
		}
		if slices.Contains(strikeMetadataEventKinds(), env.Type) {
			return &IgnoredSourceRecord{Kind: string(env.Type), Reason: IgnoredRecordMetadata}, nil
		}
		switch env.Type {
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
			if strikeReasoningText(event) == "" && !bytes.Equal(bytes.TrimSpace(event.Content), []byte("[]")) && !bytes.Equal(bytes.TrimSpace(event.Message), []byte("[]")) {
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
	copy.retainUnknown = true
	entries, err := copy.parseWithCompletion(s.SessionID, data, nil)
	if err != nil {
		return TranscriptCaptureResult{}, err
	}
	unknown, err := retainedUnknownEntries(entries)
	return TranscriptCaptureResult{Entries: entries, IgnoredRecords: ignored, RetainedUnknown: unknown}, err
}
