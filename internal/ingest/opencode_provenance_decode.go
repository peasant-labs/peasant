package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/peasant-labs/schema"
)

// OpenCodeProvenanceScope carries the session-level native facts one row
// decoder needs: the owning session, the shape being decoded, and what the
// session row proved about parentage. ParentNullProven is true only when the
// native session table carries a parent column and the row value is null or
// empty; a missing column leaves it false.
type OpenCodeProvenanceScope struct {
	SessionID        string
	Shape            OpenCodeProvenanceShape
	ParentNullProven bool
	HasParent        bool
}

// OpenCodeProvenanceRow is one native message row awaiting classification: the
// validated identity columns plus the raw data payload. Decoders check the
// redundant payload identities against the columns and fail closed on
// conflict, exactly like the retained row normalizer.
type OpenCodeProvenanceRow struct {
	ID          string
	SessionID   string
	Type        string
	TimeCreated int64
	TimeUpdated int64
	Seq         int64
	HasSeq      bool
	Data        string
}

// DecodeOpenCodeProvenanceRow decodes one current-shape message row into the
// classifier message plus its settled flag. It reuses the pinned row
// validators and payload decoders, so the new path admits exactly what the
// retained normalizer admits: redundant payload identities must agree with
// the SQLite columns, required fields must be present, and newer control rows
// without an upstream id surface the shared skip sentinel for the caller to
// count. A setled row is complete: no running shell, no running or pending
// tool part, no running compaction.
func DecodeOpenCodeProvenanceRow(row OpenCodeProvenanceRow, scope OpenCodeProvenanceScope) (OpenCodeProvenanceMessage, bool, error) {
	msg := OpenCodeProvenanceMessage{
		MessageID:        row.ID,
		SessionID:        scope.SessionID,
		Shape:            scope.Shape,
		NativeType:       row.Type,
		Seq:              row.Seq,
		HasSeq:           row.HasSeq,
		TimeCreated:      row.TimeCreated,
		ParentNullProven: scope.ParentNullProven,
		HasParent:        scope.HasParent,
	}
	if strings.TrimSpace(row.ID) == "" {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message identity is empty; the row cannot be classified; supply the native message id")
	}
	if strings.TrimSpace(scope.SessionID) == "" {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: session scope is empty for message %q; the row cannot be scoped; snapshot the session row first", row.ID)
	}
	data := []byte(row.Data)
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q carries undecodable data: %v; no partial row is eligible; verify the source row and retry", row.ID, err)
	}
	if raw, exists := envelope["id"]; exists {
		var id string
		if err := json.Unmarshal(raw, &id); err != nil {
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q payload id is not a string; the row cannot be identified; verify the source row and retry", row.ID)
		}
		if id != row.ID {
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: upstream message id %q conflicts with row id %q; the row cannot be trusted; verify the source row and retry", id, row.ID)
		}
	}
	if raw, exists := envelope["type"]; exists {
		var rowType string
		if err := json.Unmarshal(raw, &rowType); err != nil {
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q payload type is not a string; the row cannot be classified; verify the source row and retry", row.ID)
		}
		if rowType != row.Type {
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: upstream message type %q conflicts with row type %q; the row cannot be trusted; verify the source row and retry", rowType, row.Type)
		}
	}
	normalized, err := normalizeOpenCodeV2StructuralRow(OpenCodeCurrentMessageRowForDecode(row), data, envelope)
	if err != nil {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q failed structural normalization: %w; no partial row is eligible; verify the source row and retry", row.ID, err)
	}
	data = normalized
	switch row.Type {
	case "user":
		return decodeOpenCodeProvenanceUser(row, msg, data)
	case "shell":
		return decodeOpenCodeProvenanceShell(row, msg, data, envelope)
	case "assistant":
		return decodeOpenCodeProvenanceAssistant(row, msg, data)
	case "synthetic", "system", "skill":
		return decodeOpenCodeProvenanceSystem(row, msg, data)
	case "compaction":
		return decodeOpenCodeProvenanceCompaction(row, msg, data, envelope)
	case "agent-switched", "model-switched":
		return decodeOpenCodeProvenanceControl(row, msg, data)
	default:
		hasID := requireOpenCodeCurrentFields(data, "id") == nil
		hasTime := requireOpenCodeCurrentFields(data, "time") == nil
		if !hasID && hasTime {
			return OpenCodeProvenanceMessage{}, false, errOpenCodeSkipControlRow
		}
		msg.SystemText = strings.TrimSpace(string(data))
		return msg, true, nil
	}
}

// OpenCodeCurrentMessageRowForDecode maps the plain decode row onto the
// validated row handle the structural normalizer expects. Callers supply rows
// the bounded source read already validated; this mapping never widens what
// the read admitted.
func OpenCodeCurrentMessageRowForDecode(row OpenCodeProvenanceRow) OpenCodeCurrentMessageRow {
	return OpenCodeCurrentMessageRow{
		ID:          OpenCodeCurrentMessageID{value: row.ID},
		SessionID:   OpenCodeCurrentSessionID{value: row.SessionID},
		Type:        OpenCodeCurrentMessageType{value: row.Type},
		TimeCreated: row.TimeCreated,
		TimeUpdated: row.TimeUpdated,
		Data:        row.Data,
	}
}

func decodeOpenCodeProvenanceUser(row OpenCodeProvenanceRow, msg OpenCodeProvenanceMessage, data []byte) (OpenCodeProvenanceMessage, bool, error) {
	var value openCodeCurrentUser
	if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q user payload failed to decode: %v; verify the source row and retry", row.ID, err)
	}
	if err := requireOpenCodeCurrentFields(data, "text", "time"); err != nil {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q user payload misses a required field: %v; verify the source row and retry", row.ID, err)
	}
	msg.Text = value.Text
	for _, agent := range value.Agents {
		if agent.Name == "" {
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q user agent attachment requires a name; verify the source row and retry", row.ID)
		}
		msg.AgentMentions = append(msg.AgentMentions, agent.Name)
	}
	for _, raw := range value.Files {
		file, err := decodeOpenCodeV2File(raw)
		if err != nil {
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q user file attachment failed to decode: %v; verify the source row and retry", row.ID, err)
		}
		msg.Files = append(msg.Files, OpenCodeProvenanceFile{Name: file.Name, URI: file.URI, Description: file.Description})
	}
	for _, skill := range value.Skills {
		if skill.ID == "" || skill.Name == "" {
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q user skill attachment requires id and name; verify the source row and retry", row.ID)
		}
		text := skill.Text
		if text == "" {
			text = skill.Name
		}
		msg.SkillTexts = append(msg.SkillTexts, text)
	}
	return msg, true, nil
}

func decodeOpenCodeProvenanceShell(row OpenCodeProvenanceRow, msg OpenCodeProvenanceMessage, data []byte, envelope map[string]json.RawMessage) (OpenCodeProvenanceMessage, bool, error) {
	var value openCodeCurrentShell
	if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q shell payload failed to decode: %v; verify the source row and retry", row.ID, err)
	}
	if err := requireOpenCodeCurrentFields(data, "callID", "command", "output", "time"); err != nil {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q shell payload misses a required field: %v; verify the source row and retry", row.ID, err)
	}
	msg.ShellCallID = value.CallID
	msg.ShellCommand = value.Command
	msg.ShellOutput = value.Output
	msg.TimeCompleted = value.Time.Completed
	if envelope["shellID"] != nil {
		msg.HasShellID = true
		state, err := normalizeOpenCodeV2ShellState(value, envelope)
		if err != nil {
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q tool-form shell state failed to decode: %v; verify the source row and retry", row.ID, err)
		}
		return msg, state.Status == "completed" || state.Status == "error", nil
	}
	if strings.TrimSpace(value.CallID) == "" {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q shell action requires a call id; the action cannot be correlated; verify the source row and retry", row.ID)
	}
	return msg, value.Time.Completed > 0 || value.Output != "", nil
}

func decodeOpenCodeProvenanceAssistant(row OpenCodeProvenanceRow, msg OpenCodeProvenanceMessage, data []byte) (OpenCodeProvenanceMessage, bool, error) {
	var value openCodeCurrentAssistant
	if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q assistant payload failed to decode: %v; verify the source row and retry", row.ID, err)
	}
	if err := requireOpenCodeCurrentFields(data, "agent", "model", "content", "time"); err != nil {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q assistant payload misses a required field: %v; verify the source row and retry", row.ID, err)
	}
	if err := validateOpenCodeV2Error(value.Error); err != nil {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q assistant error failed to decode: %v; verify the source row and retry", row.ID, err)
	}
	collector := &openCodeAssistantPartCollector{messageID: row.ID}
	registry := &openCodeCurrentIdentityRegistry{kinds: map[string]string{}}
	appendPart := func(id string, created int64, part any) error {
		return collector.add(id, created, part)
	}
	for _, content := range value.Content {
		if err := appendOpenCodeCurrentAssistantContent(nil, content, registry, appendPart); err != nil {
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q assistant content failed to decode: %v; verify the source row and retry", row.ID, err)
		}
	}
	msg.Parts = collector.parts
	msg.TimeCompleted = value.Time.Completed
	return msg, collector.settled(), nil
}

// openCodeAssistantPartCollector converts the pinned assistant content shapes
// into classifier parts, preserving native part IDs and recording whether
// every tool part reached a terminal state.
type openCodeAssistantPartCollector struct {
	messageID string
	parts     []OpenCodeProvenanceAssistantPart
	running   bool
}

func (c *openCodeAssistantPartCollector) add(id string, _ int64, part any) error {
	switch value := part.(type) {
	case openCodeCurrentAssistantText:
		c.parts = append(c.parts, OpenCodeProvenanceAssistantPart{ID: value.ID, Kind: "text", Text: value.Text})
	case openCodeCurrentAssistantReasoning:
		c.parts = append(c.parts, OpenCodeProvenanceAssistantPart{ID: value.ID, Kind: "reasoning", Text: value.Text})
	case map[string]any:
		name, _ := value["name"].(string)
		state, _ := value["state"].(openCodeCurrentToolState)
		toolTime, _ := value["time"].(openCodeCurrentToolTime)
		_ = toolTime
		part := OpenCodeProvenanceAssistantPart{ID: id, Kind: "tool", ToolName: name, ToolInput: string(state.Input)}
		switch state.Status {
		case "completed", "error":
			part.ToolCompleted = true
			if len(state.Result) != 0 {
				part.ToolResult = string(state.Result)
			} else if state.Error != nil {
				part.ToolResult = state.Error.Message
			}
		default:
			c.running = true
		}
		c.parts = append(c.parts, part)
	default:
		return fmt.Errorf("assistant content carries an unsupported decoded shape; the part cannot be preserved; verify the source row")
	}
	return nil
}

func (c *openCodeAssistantPartCollector) settled() bool {
	return !c.running
}

func decodeOpenCodeProvenanceSystem(row OpenCodeProvenanceRow, msg OpenCodeProvenanceMessage, data []byte) (OpenCodeProvenanceMessage, bool, error) {
	if row.Type == "synthetic" {
		var value openCodeCurrentSynthetic
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q synthetic payload failed to decode: %v; verify the source row and retry", row.ID, err)
		}
		if err := requireOpenCodeCurrentFields(data, "text", "time"); err != nil {
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q synthetic payload misses a required field: %v; verify the source row and retry", row.ID, err)
		}
		var envelope map[string]json.RawMessage
		_ = json.Unmarshal(data, &envelope)
		if raw, present := envelope["sessionID"]; present && (bytes.Equal(raw, []byte("null")) || value.SessionID != row.SessionID) {
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q synthetic session disagrees with its row session; the row cannot be trusted; verify the source row and retry", row.ID)
		}
		msg.SystemText = value.Text
		return msg, true, nil
	}
	if row.Type == "skill" {
		var value openCodeV2SkillMessage
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q skill payload failed to decode: %v; verify the source row and retry", row.ID, err)
		}
		if err := requireOpenCodeCurrentFields(data, "skill", "name", "text", "time"); err != nil {
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q skill payload misses a required field: %v; verify the source row and retry", row.ID, err)
		}
		if value.Skill == "" || value.Name == "" {
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q skill payload requires skill and name; verify the source row and retry", row.ID)
		}
		msg.SystemText = value.Text
		return msg, true, nil
	}
	var value openCodeCurrentTextMessage
	if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q system payload failed to decode: %v; verify the source row and retry", row.ID, err)
	}
	if err := requireOpenCodeCurrentFields(data, "text", "time"); err != nil {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q system payload misses a required field: %v; verify the source row and retry", row.ID, err)
	}
	msg.SystemText = value.Text
	return msg, true, nil
}

func decodeOpenCodeProvenanceCompaction(row OpenCodeProvenanceRow, msg OpenCodeProvenanceMessage, data []byte, envelope map[string]json.RawMessage) (OpenCodeProvenanceMessage, bool, error) {
	var value openCodeCurrentCompaction
	if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q compaction payload failed to decode: %v; verify the source row and retry", row.ID, err)
	}
	if err := requireOpenCodeCurrentFields(data, "reason", "summary", "recent", "time"); err != nil {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q compaction payload misses a required field: %v; verify the source row and retry", row.ID, err)
	}
	if value.Reason != "auto" && value.Reason != "manual" {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q compaction reason %q is outside the auto/manual set; verify the source row and retry", row.ID, value.Reason)
	}
	msg.CompactionSummary = value.Summary
	// A failed native compaction contributes its error as system context but
	// never acquires the completed marker: only a completed compaction changes
	// the model baseline the summary describes. A running compaction is live
	// state: it is unsettled, so the copy proof and the emission both omit it.
	msg.CompactionCompleted = envelope["error"] == nil
	settled := true
	if raw, present := envelope["status"]; present {
		var status string
		if err := json.Unmarshal(raw, &status); err != nil {
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q compaction status is not a string; verify the source row and retry", row.ID)
		}
		switch status {
		case "completed":
			msg.CompactionCompleted = true
		case "failed":
			msg.CompactionCompleted = false
		case "running":
			msg.CompactionCompleted = false
			settled = false
		default:
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q compaction status %q is outside the running/completed/failed set; verify the source row and retry", row.ID, status)
		}
	}
	return msg, settled, nil
}

func decodeOpenCodeProvenanceControl(row OpenCodeProvenanceRow, msg OpenCodeProvenanceMessage, data []byte) (OpenCodeProvenanceMessage, bool, error) {
	if row.Type == "agent-switched" {
		var value openCodeCurrentAgentSwitched
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q agent switch failed to decode: %v; verify the source row and retry", row.ID, err)
		}
		if err := requireOpenCodeCurrentFields(data, "agent", "time"); err != nil {
			return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q agent switch misses a required field: %v; verify the source row and retry", row.ID, err)
		}
		msg.SystemText = value.Agent
		return msg, true, nil
	}
	var value openCodeCurrentModelSwitched
	if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q model switch failed to decode: %v; verify the source row and retry", row.ID, err)
	}
	if err := requireOpenCodeCurrentFields(data, "model", "time"); err != nil {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeProvenanceRow: message %q model switch misses a required field: %v; verify the source row and retry", row.ID, err)
	}
	msg.SystemText = value.Model.ID
	return msg, true, nil
}

// decodeOpenCodeSemanticProvenance decodes one legacy or semantic message with
// its parts into the classifier message plus its settled flag. It reuses the
// pinned semantic parsers, so the new path sees exactly what the retained
// reader sees. Roles come from the message payload; delivery stays unknown
// because these shapes prove no admission route, and a message whose every
// part is natively synthetic stays harness context even when its role reads
// user.
func decodeOpenCodeSemanticProvenance(semantic openCodeSemanticMessage, scope OpenCodeProvenanceScope) (OpenCodeProvenanceMessage, bool, error) {
	msg := OpenCodeProvenanceMessage{
		MessageID:        semantic.EntryID,
		SessionID:        scope.SessionID,
		Shape:            scope.Shape,
		TimeCreated:      semantic.TimeCreated,
		TimeCompleted:    semantic.TimeCompleted,
		ParentNullProven: false,
		HasParent:        scope.HasParent,
	}
	if strings.TrimSpace(semantic.EntryID) == "" {
		return OpenCodeProvenanceMessage{}, false, fmt.Errorf("ingest.DecodeOpenCodeSemanticProvenance: message identity is empty; the row cannot be classified; supply the native message id")
	}
	role := Role(semantic.Data.Role)
	if !role.IsValid() {
		msg.SystemText = firstOpenCodeSemanticText(semantic.Parts)
		if msg.SystemText == "" {
			msg.SystemText = strings.TrimSpace(string(semantic.Raw))
		}
		return msg, true, nil
	}
	msg.Role = schema.Role(role)
	synthetic, counted := 0, 0
	for _, part := range semantic.Parts {
		if part.UnknownType {
			continue
		}
		counted++
		if part.Data.Synthetic {
			synthetic++
		}
	}
	if counted > 0 && synthetic == counted {
		msg.AllPartsSynthetic = true
		msg.SystemText = joinOpenCodeSemanticText(semantic.Parts)
		return msg, true, nil
	}
	switch role {
	case RoleUser:
		msg.Text = joinOpenCodeSemanticText(semantic.Parts)
		return msg, true, nil
	case RoleAssistant:
		settled := true
		for _, part := range semantic.Parts {
			partMsg, partRunning := decodeOpenCodeSemanticPart(part)
			if partRunning {
				settled = false
			}
			msg.Parts = append(msg.Parts, partMsg)
		}
		return msg, settled, nil
	default:
		msg.SystemText = joinOpenCodeSemanticText(semantic.Parts)
		return msg, true, nil
	}
}

// decodeOpenCodeSemanticPart maps one semantic part onto a classifier part. A
// tool_result part without a matching tool_use in the same message keeps its
// native identity; the classifier pairs it by that identity and never by text.
func decodeOpenCodeSemanticPart(part openCodeSemanticPart) (OpenCodeProvenanceAssistantPart, bool) {
	partID := part.Data.ID
	if partID == "" {
		partID = part.EntryID
	}
	switch part.Data.Type {
	case "reasoning":
		return OpenCodeProvenanceAssistantPart{ID: partID, Kind: "reasoning", Text: part.Data.Text}, false
	case "tool", "tool_use":
		out := OpenCodeProvenanceAssistantPart{ID: partID, Kind: "tool", ToolName: openCodeSemanticToolName(part.Data), ToolInput: string(part.Data.Input)}
		status := ""
		if part.Data.State != nil {
			status = part.Data.State.Status
		}
		switch status {
		case "completed", "error":
			out.ToolCompleted = true
		case "running", "pending":
			return out, true
		}
		switch {
		case len(part.Data.Output) != 0:
			out.ToolResult = string(part.Data.Output)
		case part.Data.State != nil && len(part.Data.State.Result) != 0:
			out.ToolResult = string(part.Data.State.Result)
		case part.Data.State != nil && len(part.Data.State.Output) != 0:
			out.ToolResult = string(part.Data.State.Output)
		case len(part.Data.Content) != 0:
			out.ToolResult = string(part.Data.Content)
		}
		return out, false
	case "tool_result":
		out := OpenCodeProvenanceAssistantPart{ID: partID, Kind: "tool", ToolResult: string(part.Data.Output)}
		if out.ToolResult == "" {
			out.ToolResult = string(part.Data.Content)
		}
		return out, false
	default:
		return OpenCodeProvenanceAssistantPart{ID: partID, Kind: "text", Text: part.Data.Text}, false
	}
}

func joinOpenCodeSemanticText(parts []openCodeSemanticPart) string {
	var texts []string
	for _, part := range parts {
		if part.UnknownType {
			continue
		}
		if (part.Data.Type == "text" || part.Data.Type == "") && part.Data.Text != "" {
			texts = append(texts, part.Data.Text)
		}
	}
	return strings.Join(texts, "\n")
}
