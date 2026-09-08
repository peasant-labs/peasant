package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
)

// PiIndexer projects the native active path into ordinary durable entry rows.
type PiIndexer struct {
	fs          FileSystem
	fullContent bool
}
type PiIndexerOption func(*PiIndexer)

func WithPiFullContent(enabled bool) PiIndexerOption {
	return func(i *PiIndexer) { i.fullContent = enabled }
}
func NewPiIndexer(fs FileSystem, options ...PiIndexerOption) *PiIndexer {
	i := &PiIndexer{fs: fs}
	for _, option := range options {
		option(i)
	}
	return i
}

var _ TranscriptIndexer = (*PiIndexer)(nil)
var _ VersionedTranscriptIndexer = (*PiIndexer)(nil)

func (i *PiIndexer) IndexTranscriptResult(ctx context.Context, session DiscoveredSession) (indexformat.Result, error) {
	capture, err := i.IndexTranscriptForCapture(ctx, session)
	return indexformat.V1{Entries: capture.Entries}, err
}

func (i *PiIndexer) IndexTranscriptBytesResult(ctx context.Context, session DiscoveredSession, data []byte) (indexformat.Result, error) {
	capture, err := i.IndexTranscriptBytesForCapture(ctx, session, data)
	return indexformat.V1{Entries: capture.Entries}, err
}

var _ AuthoritativeTranscriptIndexer = (*PiIndexer)(nil)

func (i *PiIndexer) IndexTranscriptForCapture(ctx context.Context, session DiscoveredSession) (TranscriptCaptureResult, error) {
	data, err := readPiSource(ctx, i.fs, session.SourcePath.String())
	if err != nil {
		return TranscriptCaptureResult{}, err
	}
	return i.IndexTranscriptBytesForCapture(ctx, session, data)
}

func (i *PiIndexer) IndexTranscriptBytesForCapture(ctx context.Context, session DiscoveredSession, data []byte) (TranscriptCaptureResult, error) {
	if session.ContentOmitted {
		return TranscriptCaptureResult{}, captureFailure(session, 0, fmt.Errorf("retained transcript omitted source content"))
	}
	doc, err := parsePiDocument(ctx, data)
	if err != nil {
		return TranscriptCaptureResult{}, err
	}
	if doc.header.ID != session.SessionID.String() {
		return TranscriptCaptureResult{}, piSourceError("index", 0, fmt.Errorf("header id changed since discovery"))
	}
	// Completeness covers every accepted canonical entry, not an unfinished
	// physical EOF record. Preserve that diagnostic while retaining full text.
	full := *i
	full.fullContent = true
	entries, err := full.project(doc, session.SessionID)
	if err != nil {
		return TranscriptCaptureResult{}, err
	}
	return TranscriptCaptureResult{Entries: entries, Diagnostics: doc.warnings}, nil
}

func (i *PiIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }
func (i *PiIndexer) IndexTranscript(ctx context.Context, session DiscoveredSession) ([]schema.SessionEntry, error) {
	data, err := readPiSource(ctx, i.fs, session.SourcePath.String())
	if err != nil {
		return nil, err
	}
	return i.IndexTranscriptBytes(ctx, session, data)
}
func (i *PiIndexer) IndexTranscriptBytes(ctx context.Context, session DiscoveredSession, data []byte) ([]schema.SessionEntry, error) {
	doc, err := parsePiDocument(ctx, data)
	if err != nil {
		return nil, err
	}
	if doc.header.ID != session.SessionID.String() {
		return nil, piSourceError("index", 0, fmt.Errorf("header id changed since discovery"))
	}
	return i.project(doc, session.SessionID)
}

type piMessageRole string

const (
	piRoleUser       piMessageRole = "user"
	piRoleAssistant  piMessageRole = "assistant"
	piRoleToolResult piMessageRole = "toolResult"
	piRoleBash       piMessageRole = "bashExecution"
)

func (r *piMessageRole) UnmarshalJSON(raw []byte) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	switch value {
	case string(piRoleUser):
		*r = piRoleUser
	case string(piRoleAssistant):
		*r = piRoleAssistant
	case string(piRoleToolResult):
		*r = piRoleToolResult
	case string(piRoleBash):
		*r = piRoleBash
	default:
		return fmt.Errorf("unknown Pi message role")
	}
	return nil
}

type piBlockType string

const (
	piBlockText     piBlockType = "text"
	piBlockThinking piBlockType = "thinking"
	piBlockImage    piBlockType = "image"
	piBlockToolCall piBlockType = "toolCall"
)

func (t *piBlockType) UnmarshalJSON(raw []byte) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	switch value {
	case string(piBlockText):
		*t = piBlockText
	case string(piBlockThinking):
		*t = piBlockThinking
	case string(piBlockImage):
		*t = piBlockImage
	case string(piBlockToolCall):
		*t = piBlockToolCall
	default:
		return fmt.Errorf("unknown Pi content block type")
	}
	return nil
}

type piMessagePayload struct {
	Role          piMessageRole   `json:"role"`
	Content       json.RawMessage `json:"content"`
	Model         string          `json:"model"`
	ResponseModel json.RawMessage `json:"responseModel"`
	Usage         json.RawMessage `json:"usage"`
	ToolCallID    string          `json:"toolCallId"`
	ToolName      string          `json:"toolName"`
	IsError       bool            `json:"isError"`
	Details       json.RawMessage `json:"details"`
	Command       string          `json:"command"`
	Output        string          `json:"output"`
	ExitCode      *int            `json:"exitCode"`
	Cancelled     bool            `json:"cancelled"`
}
type piContentBlock struct {
	Type      piBlockType     `json:"type"`
	Text      *string         `json:"text"`
	Thinking  *string         `json:"thinking"`
	ImageData *string         `json:"data"`
	MimeType  *string         `json:"mimeType"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Namespace *string         `json:"namespace"`
	Arguments json.RawMessage `json:"arguments"`
}
type piToolOwner struct {
	parent   int
	name     string
	finished bool
}

var _ json.Unmarshaler = (*piContentBlock)(nil)

// UnmarshalJSON retains optional empty namespace evidence, rejecting explicit
// null before a pointer decode could make it indistinguishable from omission.
func (b *piContentBlock) UnmarshalJSON(raw []byte) error {
	type block piContentBlock
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if namespace, present := fields["namespace"]; present && bytes.Equal(bytes.TrimSpace(namespace), []byte("null")) {
		return fmt.Errorf("namespace must be a string when present, not null")
	}
	return json.Unmarshal(raw, (*block)(b))
}

func piContent(raw json.RawMessage, assistant bool) (string, []piContentBlock, error) {
	if len(raw) == 0 {
		return "", nil, fmt.Errorf("content is missing")
	}
	if raw[0] == '"' {
		var text string
		err := json.Unmarshal(raw, &text)
		return text, nil, err
	}
	if raw[0] != '[' {
		return "", nil, fmt.Errorf("content must be text or a content-block array")
	}
	var blocks []piContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", nil, err
	}
	var text []string
	var thinking []string
	var tools []piContentBlock
	for _, block := range blocks {
		switch block.Type {
		case piBlockText:
			if block.Text == nil {
				return "", nil, fmt.Errorf("text block requires string text")
			}
			text = append(text, *block.Text)
		case piBlockThinking:
			if !assistant || block.Thinking == nil {
				return "", nil, fmt.Errorf("thinking requires an assistant and string thinking text")
			}
			thinking = append(thinking, *block.Thinking)
		case piBlockImage:
			if block.ImageData == nil || block.MimeType == nil {
				return "", nil, fmt.Errorf("image block requires string data and mimeType")
			}
			text = append(text, "[image omitted]")
		case piBlockToolCall:
			if !assistant || block.ID == "" || block.Name == "" || !bytes.HasPrefix(bytes.TrimSpace(block.Arguments), []byte{'{'}) {
				return "", nil, fmt.Errorf("toolCall requires id, name, and object arguments")
			}
			tools = append(tools, block)
		default:
			return "", nil, fmt.Errorf("unknown content block type")
		}
	}
	content := strings.Join(text, "\n")
	if len(thinking) > 0 {
		// Canonical mixed-thinking representation: the adapter extracts only
		// this leading disclosure and leaves the answer/images visible.
		content = "<thinking>" + strings.Join(thinking, "\n") + "</thinking>\n" + content
	}
	return content, tools, nil
}

func (i *PiIndexer) project(doc piDocument, sessionID SessionID) ([]schema.SessionEntry, error) {
	var rows []schema.SessionEntry
	metadataCount, metadataBytes := 0, 0
	calls := make(map[string]*piToolOwner)
	appendRow := func(row schema.SessionEntry, extra PiExtra) error {
		row.SessionID, row.EntryIndex, row.Harness = sessionID, len(rows), schema.HarnessPi
		if !i.fullContent {
			if row.ContentPreview != nil {
				text := truncateString(*row.ContentPreview, defaults.ContentPreviewLimit)
				row.ContentPreview = &text
			}
			if row.ToolOutput != nil {
				text := truncateString(*row.ToolOutput, defaults.ContentPreviewLimit)
				row.ToolOutput = &text
			}
		}
		encoded, err := EncodePiExtra(extra)
		if err != nil {
			return err
		}
		row.Extra = encoded
		rows = append(rows, row)
		return nil
	}
	for _, entry := range doc.active {
		extra := PiExtra{Kind: PiExtraState, Harness: schema.HarnessPi, SourceRef: PiPublicRef(sessionID.String(), "entry", entry.ID)}
		row := schema.SessionEntry{Role: RoleSystem, EntryType: EntryTypeSystem, TimestampMs: parseIndexTimestamp(entry.Timestamp)}
		var scope schema.UsageScope
		var rawUsage json.RawMessage
		var metadataKind schema.NativeMetadataKind
		var sourceType schema.NativeMetadataSourceType
		var data json.RawMessage
		var content string
		var tools []piContentBlock
		switch entry.Type {
		case piMessage:
			var message piMessagePayload
			if err := json.Unmarshal(entry.Message, &message); err != nil {
				return nil, piSourceError("message", 0, err)
			}
			if message.Role == piRoleBash {
				// A user shell execution has a real execute-style result, but is not
				// an assistant response and must not fabricate assistant usage.
				content = "Shell execution"
				row.Role = RoleSystem
				row.ContentPreview = &content
				parent := len(rows)
				if err := appendRow(row, extra); err != nil {
					return nil, err
				}
				toolID := PiPublicRef(sessionID.String(), "tool", entry.ID)
				name := "bash"
				kind := classifyToolKind(name)
				args, _ := json.Marshal(map[string]string{"command": message.Command})
				input := string(args)
				call := schema.SessionEntry{Role: RoleAssistant, EntryType: EntryTypeToolUse, Depth: 1, ParentIndex: &parent, HasToolUse: true, ToolCallID: &toolID, ToolKind: &kind, ToolNamesCSV: &name, ToolInput: &input}
				if err := appendRow(call, extra); err != nil {
					return nil, err
				}
				result := schema.SessionEntry{Role: RoleTool, EntryType: EntryTypeToolResult, Depth: 1, ParentIndex: &parent, ToolCallID: &toolID, ToolOutput: &message.Output, ContentPreview: &message.Output, IsError: message.Cancelled || message.ExitCode != nil && *message.ExitCode != 0}
				extra.SourceRef = PiPublicRef(sessionID.String(), "entry", entry.ID+":result")
				usage, err := PiUsageFromRaw(sessionID.String(), entry.ID+":result", schema.UsageScopeTool, message.Usage)
				if err != nil {
					return nil, err
				}
				extra.Usage = &usage
				if err := appendRow(result, extra); err != nil {
					return nil, err
				}
				continue
			}
			var err error
			content, tools, err = piContent(message.Content, message.Role == piRoleAssistant)
			if err != nil {
				return nil, piSourceError("message content", 0, err)
			}
			switch message.Role {
			case piRoleUser:
				row.Role, row.EntryType = RoleUser, EntryTypeText
			case piRoleAssistant:
				row.Role, row.EntryType = RoleAssistant, EntryTypeText
				var blocks []piContentBlock
				if json.Unmarshal(message.Content, &blocks) == nil {
					for _, block := range blocks {
						if block.Type == piBlockThinking {
							row.HasThinking = true
						}
					}
				}
				scope, rawUsage = schema.UsageScopeAssistant, message.Usage
				model := message.Model
				if len(message.ResponseModel) > 0 {
					if bytes.Equal(bytes.TrimSpace(message.ResponseModel), []byte("null")) {
						return nil, piSourceError("response model", 0, fmt.Errorf("responseModel must be a valid observed model string when present, not null"))
					}
					if err := json.Unmarshal(message.ResponseModel, &model); err != nil {
						return nil, piSourceError("response model", 0, err)
					}
				}
				if model != "" || len(message.ResponseModel) > 0 {
					observed, modelErr := schema.NewObservedModelID(model)
					if modelErr != nil {
						return nil, piSourceError("response model", 0, modelErr)
					}
					extra.ModelID = observed
				}
			case piRoleToolResult:
				owner := calls[message.ToolCallID]
				if owner == nil || owner.finished || owner.name != message.ToolName {
					return nil, piSourceError("tool pairing", 0, fmt.Errorf("result has no unique preceding matching call"))
				}
				owner.finished = true
				id := PiPublicRef(sessionID.String(), "tool", message.ToolCallID)
				row.Role, row.EntryType, row.Depth, row.ParentIndex, row.ToolCallID = RoleTool, EntryTypeToolResult, 1, &owner.parent, &id
				row.IsError, row.ToolOutput = message.IsError, &content
				scope, rawUsage = schema.UsageScopeTool, message.Usage
				metadataKind, sourceType, data = schema.NativeMetadataPiToolResultDetails, schema.NativeSourcePiMessage, message.Details
			default:
				return nil, piSourceError("message role", 0, fmt.Errorf("unsupported message role"))
			}
			if message.Role != piRoleAssistant && len(tools) > 0 {
				return nil, piSourceError("tool pairing", 0, fmt.Errorf("only assistant messages may declare tool calls"))
			}
		case piCustomMessage:
			var err error
			content, tools, err = piContent(entry.Content, false)
			if err != nil || len(tools) > 0 {
				return nil, piSourceError("custom message", 0, fmt.Errorf("expected text/image context content"))
			}
			metadataKind, sourceType, data = schema.NativeMetadataPiCustomMessageDetails, schema.NativeSourcePiCustomMessage, entry.Details
		case piCompaction, piBranchSummary:
			content, scope, rawUsage, data = entry.Summary, schema.UsageScopeSummary, entry.Usage, entry.Details
			if entry.Type == piCompaction {
				metadataKind, sourceType = schema.NativeMetadataPiCompactionDetails, schema.NativeSourcePiCompaction
			} else {
				metadataKind, sourceType = schema.NativeMetadataPiBranchSummaryDetails, schema.NativeSourcePiBranchSummary
			}
		case piCustom:
			metadataKind, sourceType, data = schema.NativeMetadataPiCustomData, schema.NativeSourcePiCustom, entry.Data
		}
		if scope != "" {
			usage, err := PiUsageFromRaw(sessionID.String(), entry.ID, scope, rawUsage)
			if err != nil {
				return nil, err
			}
			extra.Usage, extra.Kind = &usage, PiExtraUsage
			if scope == schema.UsageScopeAssistant && usage.Tokens != nil {
				if usage.Tokens.Input != nil {
					value := int(*usage.Tokens.Input)
					row.TokensIn = &value
				}
				if usage.Tokens.Output != nil {
					value := int(*usage.Tokens.Output)
					row.TokensOut = &value
				}
			}
		}
		if len(data) > 0 {
			if (entry.Type == piCustom || entry.Type == piCustomMessage) && strings.TrimSpace(entry.CustomType) == "" {
				return nil, piSourceError("metadata", 0, fmt.Errorf("customType is required for custom metadata"))
			}
			metadataCount++
			metadataBytes += len(data)
			if metadataCount > 256 || metadataBytes > 1<<20 {
				return nil, piSourceError("metadata", 0, fmt.Errorf("metadata exceeds session record or byte budget"))
			}
			safe, err := SanitizePiMetadataData(data, func(s string) (string, error) { return s, nil })
			if err != nil {
				return nil, err
			}
			record := schema.NativeMetadataRecord{ID: PiPublicRef(sessionID.String(), "metadata", entry.ID), Kind: metadataKind, Source: schema.NativeSourceRef{EntryRef: extra.SourceRef, SourceType: sourceType}, Data: safe}
			if entry.Type == piCustom || entry.Type == piCustomMessage {
				record.CustomType = entry.CustomType
			}
			if metadataKind == schema.NativeMetadataPiToolResultDetails {
				record.Source.MessageRole = schema.NativePiMessageRoleToolResult
			}
			extra.Metadata, extra.Kind = []schema.NativeMetadataRecord{record}, PiExtraNativeMetadata
		}
		if entry.Type != piMessage && entry.Type != piCustomMessage && entry.Type != piCompaction && entry.Type != piBranchSummary {
			carrier, err := NewPiCarrier(sessionID, len(rows), extra)
			if err != nil {
				return nil, err
			}
			rows = append(rows, carrier)
			continue
		}
		row.ContentPreview, row.HasToolUse = &content, len(tools) > 0
		parent := len(rows)
		if err := appendRow(row, extra); err != nil {
			return nil, err
		}
		for _, tool := range tools {
			if _, exists := calls[tool.ID]; exists {
				return nil, piSourceError("tool pairing", 0, fmt.Errorf("duplicate tool call id"))
			}
			calls[tool.ID] = &piToolOwner{parent: parent, name: tool.Name}
			id := PiPublicRef(sessionID.String(), "tool", tool.ID)
			kind, input := classifyToolKind(tool.Name), string(tool.Arguments)
			call := schema.SessionEntry{Role: RoleAssistant, EntryType: EntryTypeToolUse, TimestampMs: row.TimestampMs, Depth: 1, ParentIndex: &parent, HasToolUse: true, ToolNamesCSV: &tool.Name, ToolKind: &kind, ToolCallID: &id, ToolInput: &input}
			callExtra := PiExtra{Kind: PiExtraState, Harness: schema.HarnessPi, SourceRef: extra.SourceRef, Namespace: tool.Namespace}
			if err := appendRow(call, callExtra); err != nil {
				return nil, err
			}
		}
	}
	if len(rows) == 0 {
		carrier, err := NewPiCarrier(sessionID, 0, PiExtra{Kind: PiExtraCarrier, Harness: schema.HarnessPi})
		if err != nil {
			return nil, err
		}
		rows = append(rows, carrier)
	}
	if doc.hasTitle {
		extra, _, err := DecodePiExtra(rows[0].Extra)
		if err != nil {
			return nil, err
		}
		extra.SessionName = &doc.title
		rows[0].Extra, err = EncodePiExtra(extra)
		if err != nil {
			return nil, err
		}
	}
	return rows, nil
}
