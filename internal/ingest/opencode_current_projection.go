package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
)

const (
	openCodeCurrentProjectionFormat = "peasant.opencode.current-sqlite"
	// openCodeCurrentProjectionVersion is version 2: a control record now
	// carries the typed control field and no fabricated part. The shared minimum
	// readable version stays 1, so a previously persisted version 1 current
	// projection, which never held a control record, still decodes.
	openCodeCurrentProjectionVersion = 2
	openCodeCurrentMaterializePage   = 128
)

type openCodeCurrentProjection struct {
	Format    string                            `json:"format"`
	Version   int                               `json:"version"`
	SessionID string                            `json:"session_id"`
	Messages  []openCodeLegacyProjectionMessage `json:"messages"`
}

// These shapes mirror @opencode-ai/schema/session-message at upstream commit
// 4643e65ad6334de3e4e68dedc201d5fbb828c9fe. The projector stores type and id
// in columns and encodes the remaining complete SessionMessage into data.
type openCodeCurrentBase struct {
	ID       string                     `json:"id"`
	Metadata map[string]json.RawMessage `json:"metadata,omitempty"`
	Time     openCodeCurrentTime        `json:"time"`
}

type openCodeCurrentTime struct {
	Created   int64 `json:"created"`
	Completed int64 `json:"completed,omitempty"`
}

type openCodeCurrentModel struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant,omitempty"`
}

type openCodeCurrentTokens struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
}

type openCodeCurrentSource struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	Text  string `json:"text"`
}

type openCodeCurrentFile struct {
	URI         string                 `json:"uri"`
	MIME        string                 `json:"mime"`
	Name        string                 `json:"name,omitempty"`
	Description string                 `json:"description,omitempty"`
	Source      *openCodeCurrentSource `json:"source,omitempty"`
}

type openCodeCurrentAgent struct {
	Name   string                 `json:"name"`
	Source *openCodeCurrentSource `json:"source,omitempty"`
}

type openCodeCurrentUser struct {
	openCodeCurrentBase
	Text   string                 `json:"text"`
	Files  []openCodeCurrentFile  `json:"files,omitempty"`
	Agents []openCodeCurrentAgent `json:"agents,omitempty"`
}

type openCodeCurrentAssistant struct {
	openCodeCurrentBase
	ParentID string                       `json:"parentID,omitempty"`
	Agent    string                       `json:"agent"`
	Model    openCodeCurrentModel         `json:"model"`
	Content  []json.RawMessage            `json:"content"`
	Snapshot *openCodeCurrentSnapshot     `json:"snapshot,omitempty"`
	Finish   string                       `json:"finish,omitempty"`
	Cost     *float64                     `json:"cost,omitempty"`
	Tokens   *openCodeCurrentTokens       `json:"tokens,omitempty"`
	Error    *openCodeCurrentUnknownError `json:"error,omitempty"`
}

type openCodeCurrentSnapshot struct {
	Start string   `json:"start,omitempty"`
	End   string   `json:"end,omitempty"`
	Files []string `json:"files,omitempty"`
}

type openCodeCurrentUnknownError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type openCodeCurrentAssistantText struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Text string `json:"text"`
}

type openCodeCurrentAssistantReasoning struct {
	Type             string                                `json:"type"`
	ID               string                                `json:"id"`
	Text             string                                `json:"text"`
	ProviderMetadata map[string]map[string]json.RawMessage `json:"providerMetadata,omitempty"`
	Time             *openCodeCurrentTime                  `json:"time,omitempty"`
}

type openCodeCurrentAssistantTool struct {
	Type     string                       `json:"type"`
	ID       string                       `json:"id"`
	Name     string                       `json:"name"`
	Provider *openCodeCurrentToolProvider `json:"provider,omitempty"`
	State    json.RawMessage              `json:"state"`
	Time     openCodeCurrentToolTime      `json:"time"`
}

type openCodeCurrentToolProvider struct {
	Executed       bool                                  `json:"executed"`
	Metadata       map[string]map[string]json.RawMessage `json:"metadata,omitempty"`
	ResultMetadata map[string]map[string]json.RawMessage `json:"resultMetadata,omitempty"`
}

type openCodeCurrentToolTime struct {
	Created   int64 `json:"created"`
	Ran       int64 `json:"ran,omitempty"`
	Completed int64 `json:"completed,omitempty"`
	Pruned    int64 `json:"pruned,omitempty"`
}

type openCodeCurrentToolState struct {
	Status      string                       `json:"status"`
	Input       json.RawMessage              `json:"input"`
	Structured  map[string]json.RawMessage   `json:"structured"`
	Content     []openCodeCurrentToolContent `json:"content"`
	Attachments []openCodeCurrentFile        `json:"attachments,omitempty"`
	OutputPaths []string                     `json:"outputPaths,omitempty"`
	Result      json.RawMessage              `json:"result,omitempty"`
	Error       *openCodeCurrentUnknownError `json:"error,omitempty"`
}

type openCodeCurrentToolContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	URI  string `json:"uri,omitempty"`
	MIME string `json:"mime,omitempty"`
	Name string `json:"name,omitempty"`
}

type openCodeCurrentTextMessage struct {
	openCodeCurrentBase
	Text string `json:"text"`
}

type openCodeCurrentSynthetic struct {
	openCodeCurrentBase
	SessionID string `json:"sessionID"`
	Text      string `json:"text"`
}

type openCodeCurrentShell struct {
	openCodeCurrentBase
	CallID  string `json:"callID"`
	Command string `json:"command"`
	Output  string `json:"output"`
}

type openCodeCurrentCompaction struct {
	openCodeCurrentBase
	Reason  string `json:"reason"`
	Summary string `json:"summary"`
	Recent  string `json:"recent"`
}

type openCodeCurrentAgentSwitched struct {
	openCodeCurrentBase
	Agent string `json:"agent"`
}

type openCodeCurrentModelSwitched struct {
	openCodeCurrentBase
	Model openCodeCurrentModel `json:"model"`
}

type openCodeCurrentIdentityRegistry struct {
	kinds map[string]string
}

func (registry *openCodeCurrentIdentityRegistry) add(id, kind string) error {
	if id == "" {
		return fmt.Errorf("%s identity is empty", kind)
	}
	if previous, exists := registry.kinds[id]; exists {
		return fmt.Errorf("identity %q is already registered as %s and cannot also identify %s", id, previous, kind)
	}
	registry.kinds[id] = kind
	return nil
}

func (base openCodeCurrentBase) validateIdentity(rowID string) error {
	if base.ID == "" {
		return errors.New("upstream message id is required")
	}
	if base.ID != rowID {
		return fmt.Errorf("upstream message id %q conflicts with SQLite row id %q", base.ID, rowID)
	}
	return nil
}

// validateControlIdentity is the identity rule for a control record, such as a
// model switch or an agent switch. A control row legitimately carries no
// upstream message id, so absence is tolerated; the SQLite row id remains the
// stable identity. When an id is present it must still match the row id, so a
// genuine identity conflict stays an error.
func (base openCodeCurrentBase) validateControlIdentity(rowID string) error {
	if base.ID != "" && base.ID != rowID {
		return fmt.Errorf("upstream message id %q conflicts with SQLite row id %q", base.ID, rowID)
	}
	return nil
}

// errOpenCodeSkipControlRow signals that a well-formed current row carried no
// upstream id and a type outside the known set, so it is a newer control record
// the caller skips and counts rather than a fatal decode failure.
var errOpenCodeSkipControlRow = errors.New("skip newer OpenCode control row")

func decodeOpenCodeCurrentJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("expected exactly one JSON value: %w", err)
	}
	return nil
}

func (a *OpenCodeAdapter) discoverCurrentSQLite(ctx context.Context, source OpenCodeSQLiteSource, candidate OpenCodeCandidate) (discovered []DiscoveredSession, err error) {
	pageSize, err := NewOpenCodeCurrentPageSize(openCodeCurrentMaterializePage)
	if err != nil {
		return nil, err
	}
	var cursor *OpenCodeCurrentSessionCursor
	for {
		page, readErr := source.CurrentSessionIDs(ctx, OpenCodeCurrentSessionPageRequest{PageSize: pageSize, After: cursor})
		if readErr != nil {
			return nil, fmt.Errorf("discover current OpenCode SQLite candidate %q failed while enumerating a bounded session page: %w; no partial discovery result is eligible; verify the database remains a supported current session_message store and retry", candidate.Path, readErr)
		}
		for _, currentID := range page.SessionIDs {
			sessionID, idErr := NewSessionID(currentID.String())
			if idErr != nil {
				return nil, fmt.Errorf("discover current OpenCode SQLite candidate %q found session identifier %q that Peasant cannot store: %w; no partial discovery result is eligible; repair the upstream identifier or use a supported export", candidate.Path, currentID.String(), idErr)
			}
			discovered = append(discovered, DiscoveredSession{SessionID: sessionID, Harness: HarnessOpenCode, SourcePath: ResolvedPath(candidate.Path), SourceFormat: SourceFormatJSON, OriginalRoot: ResolvedPath(filepath.Dir(candidate.Path)), TranscriptOrigin: TranscriptOriginOpenCodeCurrentSQLite})
		}
		if page.Next == nil {
			break
		}
		cursor = page.Next
	}
	return discovered, nil
}

// currentControlOnlySessions probes each discovered current session for a
// substantive row and returns the set that holds only control records. It uses
// the enumeration source already open for the candidate, so no second database
// is opened, and each probe is a single bounded existence read keyed on the
// indexed session_id column rather than a scan. A probe failure fails safe: the
// session is treated as substantive so a transient read error never demotes a
// real current conversation to its legacy sibling, and one diagnostic names the
// affected session.
func (a *OpenCodeAdapter) currentControlOnlySessions(ctx context.Context, source OpenCodeSQLiteSource, candidate OpenCodeCandidate, sessions []DiscoveredSession) map[SessionID]bool {
	controlOnly := make(map[SessionID]bool, len(sessions))
	for _, session := range sessions {
		currentID, err := NewOpenCodeCurrentSessionID(string(session.SessionID))
		if err != nil {
			// Enumeration already validated the identifier, so this is
			// unreachable; treat it as substantive rather than demote it.
			continue
		}
		hasSubstantive, probeErr := source.CurrentSessionHasSubstantive(ctx, currentID)
		if probeErr != nil {
			a.recordCandidateFailure(candidate.Path, OpenCodeProbeDiscover, fmt.Sprintf("substantive-row probe failed for current session %q; the session stays a current winner rather than deferring to a legacy sibling", session.SessionID), probeErr)
			continue
		}
		if !hasSubstantive {
			controlOnly[session.SessionID] = true
		}
	}
	return controlOnly
}

func (a *OpenCodeAdapter) materializeCurrentTranscript(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, []byte, error) {
	currentID, err := NewOpenCodeCurrentSessionID(string(session.SessionID))
	if err != nil {
		return nil, nil, err
	}
	pageSize, err := NewOpenCodeCurrentPageSize(openCodeCurrentMaterializePage)
	if err != nil {
		return nil, nil, err
	}
	var projection openCodeCurrentProjection
	var unknownControlTypes map[string]int
	if err := a.withOpenCodeSQLiteSource(ctx, session.SourcePath.String(), func(source OpenCodeSQLiteSource) error {
		var readErr error
		projection, unknownControlTypes, _, readErr = readOpenCodeCurrentProjectionCore(ctx, source, currentID, pageSize, 0, OpenCodePayloadSize{})
		return readErr
	}); err != nil {
		return nil, nil, fmt.Errorf("materialize current OpenCode SQLite session %q failed while reading selected session_message rows and closing the bounded source: %w; no partial managed artifact or store row was written; fix malformed current rows in OpenCode and retry", session.SessionID, err)
	}
	return a.finishCurrentManagedProjection(ctx, session, projection, unknownControlTypes)
}

// finishCurrentManagedProjection encodes a read current projection into the
// managed JSON bytes and derives its metadata. The full-session and preview
// prefix reads share it.
func (a *OpenCodeAdapter) finishCurrentManagedProjection(ctx context.Context, session DiscoveredSession, projection openCodeCurrentProjection, unknownControlTypes map[string]int) (*UnifiedMetadata, []byte, error) {
	if len(projection.Messages) == 0 {
		return nil, nil, fmt.Errorf("materialize current OpenCode SQLite session %q from %q produced no semantic messages even though discovery enumerated it; no empty managed artifact was written; retry after OpenCode finishes its transaction or remove the stale source row", session.SessionID, session.SourcePath)
	}
	data, err := json.Marshal(projection)
	if err != nil {
		return nil, nil, fmt.Errorf("materialize current OpenCode SQLite session %q failed while encoding the deterministic managed JSON projection: %w; detached source rows remain unchanged and no managed state was written; report the unsupported row shape", session.SessionID, err)
	}
	data = append(data, '\n')
	managed := openCodeLegacyProjection{Format: projection.Format, Version: projection.Version, SessionID: projection.SessionID, Messages: projection.Messages}
	metadata, err := a.metadataFromManagedProjection(ctx, session, managed)
	if err != nil {
		return nil, nil, err
	}
	metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, openCodeUnknownTypeDiagnostics(session, unknownControlTypes, "control message row")...)
	return metadata, data, nil
}

func (a *OpenCodeAdapter) materializeCurrentTranscriptBounded(ctx context.Context, session DiscoveredSession, budgetBytes int64) (*UnifiedMetadata, []byte, MaterializeTruncation, error) {
	currentID, err := NewOpenCodeCurrentSessionID(string(session.SessionID))
	if err != nil {
		return nil, nil, MaterializeTruncation{}, err
	}
	pageSize, err := NewOpenCodeCurrentPageSize(openCodeCurrentMaterializePage)
	if err != nil {
		return nil, nil, MaterializeTruncation{}, err
	}
	var projection openCodeCurrentProjection
	var unknownControlTypes map[string]int
	var truncation MaterializeTruncation
	if err := a.withOpenCodeSQLiteSource(ctx, session.SourcePath.String(), func(source OpenCodeSQLiteSource) error {
		size, sizeErr := source.CurrentSessionPayloadSize(ctx, currentID)
		if sizeErr != nil {
			return sizeErr
		}
		budget := int64(0)
		if budgetBytes > 0 && size.Bytes > budgetBytes {
			budget = budgetBytes
		}
		var readErr error
		projection, unknownControlTypes, truncation, readErr = readOpenCodeCurrentProjectionCore(ctx, source, currentID, pageSize, budget, size)
		return readErr
	}); err != nil {
		return nil, nil, MaterializeTruncation{}, fmt.Errorf("materialize bounded current OpenCode SQLite session %q failed while reading selected session_message rows and closing the bounded source: %w; no partial managed artifact or store row was written; fix malformed current rows in OpenCode and retry", session.SessionID, err)
	}
	metadata, data, err := a.finishCurrentManagedProjection(ctx, session, projection, unknownControlTypes)
	if err != nil {
		return nil, nil, MaterializeTruncation{}, err
	}
	return metadata, data, truncation, nil
}

func readOpenCodeCurrentProjection(ctx context.Context, source OpenCodeSQLiteSource, sessionID OpenCodeCurrentSessionID, pageSize OpenCodeCurrentPageSize) (openCodeCurrentProjection, map[string]int, error) {
	projection, unknownControlTypes, _, err := readOpenCodeCurrentProjectionCore(ctx, source, sessionID, pageSize, 0, OpenCodePayloadSize{})
	return projection, unknownControlTypes, err
}

// readOpenCodeCurrentProjectionCore reads the session's current message rows in
// seq order and normalizes each. When budget is positive it stops accumulating
// once the summed message payload byte length reaches the budget and reports the
// truncation; a non-positive budget reads the whole session and reports no
// truncation. size carries the whole-session totals so a truncated result can
// name how much it left out. The full-session read and the preview prefix read
// share this one path.
func readOpenCodeCurrentProjectionCore(ctx context.Context, source OpenCodeSQLiteSource, sessionID OpenCodeCurrentSessionID, pageSize OpenCodeCurrentPageSize, budget int64, size OpenCodePayloadSize) (openCodeCurrentProjection, map[string]int, MaterializeTruncation, error) {
	projection := openCodeCurrentProjection{Format: openCodeCurrentProjectionFormat, Version: openCodeCurrentProjectionVersion, SessionID: sessionID.String(), Messages: []openCodeLegacyProjectionMessage{}}
	registry := openCodeCurrentIdentityRegistry{kinds: make(map[string]string)}
	unknownControlTypes := make(map[string]int)
	var cursor *OpenCodeCurrentCursor
	// readBytes is what the read pulled out of the source and is what the budget
	// governs. includedRows and includedBytes are what reached the projection: a
	// control row is read and paid for but never shown.
	var readBytes, includedBytes, includedRows int64
	truncated := false
rowLoop:
	for {
		page, err := source.CurrentMessages(ctx, OpenCodeCurrentPageRequest{SessionID: sessionID, PageSize: pageSize, After: cursor})
		if err != nil {
			return openCodeCurrentProjection{}, nil, MaterializeTruncation{}, err
		}
		for _, row := range page.Messages {
			// Every read row, control or substantive, counts toward the budget: the
			// budget bounds how much source payload the read pulls into memory, not
			// how many rows survive normalization. The budget is checked BEFORE the
			// row is taken, so a read that spent its budget exactly on the last row
			// of the session reports no truncation: nothing was left out.
			if budget > 0 && readBytes >= budget {
				truncated = true
				break rowLoop
			}
			readBytes += int64(len(row.Data))
			if err := registry.add(row.ID.String(), "message row"); err != nil {
				return openCodeCurrentProjection{}, nil, MaterializeTruncation{}, currentNormalizationError(row, "registering stable identities", err)
			}
			message, err := normalizeOpenCodeCurrentRow(row, &registry)
			if errors.Is(err, errOpenCodeSkipControlRow) {
				// A newer id-less control record: keep the session, drop the row,
				// and count its type for one diagnostic per type.
				unknownControlTypes[row.Type.String()]++
				continue
			}
			if err != nil {
				return openCodeCurrentProjection{}, nil, MaterializeTruncation{}, currentNormalizationError(row, "decoding the pinned SessionMessage shape", err)
			}
			projection.Messages = append(projection.Messages, message)
			includedRows++
			includedBytes += int64(len(row.Data))
		}
		if page.Next == nil {
			break
		}
		cursor = page.Next
	}
	truncation := MaterializeTruncation{}
	if truncated {
		truncation = MaterializeTruncation{Truncated: true, Unit: MaterializeUnitMessages, BudgetBytes: budget, IncludedBytes: includedBytes, TotalBytes: size.Bytes, IncludedRows: includedRows, TotalRows: size.Rows}
	}
	return projection, unknownControlTypes, truncation, nil
}

func currentNormalizationError(row OpenCodeCurrentMessageRow, operation string, cause error) error {
	return fmt.Errorf("normalize current OpenCode session_message row %q type %q failed while %s: %w; the row does not satisfy the pinned upstream SessionMessage materialized schema, so no projection or caller-visible partial state was emitted; repair the upstream row or upgrade Peasant for a newly supported schema and retry", row.ID.String(), row.Type.String(), operation, cause)
}

func normalizeOpenCodeCurrentRow(row OpenCodeCurrentMessageRow, registry *openCodeCurrentIdentityRegistry) (openCodeLegacyProjectionMessage, error) {
	message := openCodeLegacyProjectionMessage{ID: row.ID.String(), SessionID: row.SessionID.String(), TimeCreated: row.TimeCreated, TimeUpdated: row.TimeUpdated, Parts: []openCodeLegacyProjectionPart{}}
	appendPart := func(id string, created int64, data any) error {
		if id != "" {
			if err := registry.add(id, "nested message content"); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		message.Parts = append(message.Parts, openCodeLegacyProjectionPart{ID: id, MessageID: message.ID, SessionID: message.SessionID, TimeCreated: created, TimeUpdated: created, Data: raw})
		return nil
	}
	messageData := func(role, text, model, agent, parentID string, metadata map[string]json.RawMessage, time openCodeCurrentTime, tokens *openCodeCurrentTokens) error {
		cwd, err := currentMetadataString(metadata, "cwd")
		if err != nil {
			return err
		}
		value := map[string]any{"id": message.ID, "sessionID": message.SessionID, "role": role, "time": time}
		if text != "" {
			value["content"] = text
		}
		if model != "" {
			value["modelID"] = model
		}
		if agent != "" {
			value["agent"] = agent
		}
		if parentID != "" {
			value["parentID"] = parentID
		}
		if cwd != "" {
			value["cwd"] = cwd
		}
		if tokens != nil {
			value["tokens"] = map[string]any{"input": tokens.Input, "output": tokens.Output, "reasoning": tokens.Reasoning, "cache": tokens.Cache}
		}
		raw, err := json.Marshal(value)
		message.Data = raw
		return err
	}
	data := []byte(row.Data)
	switch row.Type.String() {
	case "user":
		var value openCodeCurrentUser
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return message, err
		}
		if err := value.validateIdentity(message.ID); err != nil {
			return message, err
		}
		if err := requireOpenCodeCurrentFields(data, "id", "text", "files", "agents", "time"); err != nil {
			return message, err
		}
		if err := messageData(RoleUser.String(), value.Text, "", "", "", value.Metadata, value.Time, nil); err != nil {
			return message, err
		}
		for _, agent := range value.Agents {
			if agent.Name == "" {
				return message, errors.New("user agent attachment requires name")
			}
			if err := appendPart("", value.Time.Created, map[string]any{"type": "agent", "name": agent.Name, "text": agent.Name}); err != nil {
				return message, err
			}
		}
	case "assistant":
		var value openCodeCurrentAssistant
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return message, err
		}
		if err := value.validateIdentity(message.ID); err != nil {
			return message, err
		}
		if err := requireOpenCodeCurrentFields(data, "id", "agent", "model", "content", "time"); err != nil {
			return message, err
		}
		if err := messageData(RoleAssistant.String(), "", value.Model.ID, value.Agent, value.ParentID, value.Metadata, value.Time, value.Tokens); err != nil {
			return message, err
		}
		for _, content := range value.Content {
			if err := appendOpenCodeCurrentAssistantContent(&message, content, registry, appendPart); err != nil {
				return message, err
			}
		}
	case "shell":
		var value openCodeCurrentShell
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return message, err
		}
		if err := value.validateIdentity(message.ID); err != nil {
			return message, err
		}
		if err := requireOpenCodeCurrentFields(data, "id", "callID", "command", "output", "time"); err != nil {
			return message, err
		}
		if err := registry.add(value.CallID, "shell tool call"); err != nil {
			return message, err
		}
		if err := messageData(RoleAssistant.String(), "", "", "", "", value.Metadata, value.Time, nil); err != nil {
			return message, err
		}
		if err := appendPart("", value.Time.Created, map[string]any{"id": value.CallID, "type": "tool_use", "name": "shell", "input": map[string]any{"command": value.Command}, "time": map[string]any{"created": value.Time.Created}}); err != nil {
			return message, err
		}
		if value.Time.Completed > 0 || value.Output != "" {
			if err := appendPart("", value.Time.Completed, map[string]any{"id": value.CallID, "type": "tool_result", "output": value.Output, "time": map[string]any{"created": value.Time.Completed}}); err != nil {
				return message, err
			}
		}
	case "synthetic":
		var value openCodeCurrentSynthetic
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return message, err
		}
		if err := value.validateIdentity(message.ID); err != nil {
			return message, err
		}
		if err := requireOpenCodeCurrentFields(data, "id", "sessionID", "text", "time"); err != nil {
			return message, err
		}
		if value.SessionID != message.SessionID {
			return message, errors.New("synthetic sessionID must match SQLite row session")
		}
		if err := messageData(RoleSystem.String(), value.Text, "", "", "", value.Metadata, value.Time, nil); err != nil {
			return message, err
		}
	case "system":
		var value openCodeCurrentTextMessage
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return message, err
		}
		if err := value.validateIdentity(message.ID); err != nil {
			return message, err
		}
		if err := requireOpenCodeCurrentFields(data, "id", "text", "time"); err != nil {
			return message, err
		}
		if err := messageData(RoleSystem.String(), value.Text, "", "", "", value.Metadata, value.Time, nil); err != nil {
			return message, err
		}
	case "compaction":
		var value openCodeCurrentCompaction
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return message, err
		}
		if err := value.validateIdentity(message.ID); err != nil {
			return message, err
		}
		if err := requireOpenCodeCurrentFields(data, "id", "reason", "summary", "recent", "time"); err != nil {
			return message, err
		}
		if value.Reason != "auto" && value.Reason != "manual" {
			return message, errors.New("compaction reason must be auto or manual")
		}
		if err := messageData(RoleSystem.String(), value.Summary, "", "", "", value.Metadata, value.Time, nil); err != nil {
			return message, err
		}
		if err := appendPart("", value.Time.Created, map[string]any{"type": "compaction", "text": value.Summary, "content": value.Recent, "time": map[string]any{"created": value.Time.Created}}); err != nil {
			return message, err
		}
	case "agent-switched":
		// A control record legitimately carries no upstream id, so absence is
		// tolerated rather than fatal. It renders as a system record naming the
		// new agent, which is inert transcript context, not a user or assistant
		// turn.
		var value openCodeCurrentAgentSwitched
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return message, err
		}
		if err := value.validateControlIdentity(message.ID); err != nil {
			return message, err
		}
		if err := requireOpenCodeCurrentFields(data, "agent", "time"); err != nil {
			return message, err
		}
		message.Control = "agent-switched"
		if err := messageData(RoleSystem.String(), value.Agent, "", value.Agent, "", value.Metadata, value.Time, nil); err != nil {
			return message, err
		}
		if err := appendPart("", value.Time.Created, map[string]any{"type": "agent", "name": value.Agent, "text": value.Agent}); err != nil {
			return message, err
		}
	case "model-switched":
		// A control record legitimately carries no upstream id, so absence is
		// tolerated rather than fatal. A model switch is the same signal the
		// indexer records as a per-turn model observation, so it carries the
		// model id and the indexer applies it to the following assistant turn.
		var value openCodeCurrentModelSwitched
		if err := decodeOpenCodeCurrentJSON(data, &value); err != nil {
			return message, err
		}
		if err := value.validateControlIdentity(message.ID); err != nil {
			return message, err
		}
		if err := requireOpenCodeCurrentFields(data, "model", "time"); err != nil {
			return message, err
		}
		message.Control = "model-switched"
		if err := messageData(RoleSystem.String(), value.Model.ID, value.Model.ID, "", "", value.Metadata, value.Time, nil); err != nil {
			return message, err
		}
		if err := appendPart("", value.Time.Created, map[string]any{"type": "subtask", "name": value.Model.ID, "text": value.Model.ID}); err != nil {
			return message, err
		}
	default:
		// A well-formed row that carries no upstream id but is control-shaped (it
		// has a time) is a newer control record: skip it and let the caller count
		// its type. A row that should carry an id, or is not control-shaped,
		// stays an error so genuinely malformed content still fails the session.
		hasID := requireOpenCodeCurrentFields(data, "id") == nil
		hasTime := requireOpenCodeCurrentFields(data, "time") == nil
		if !hasID && hasTime {
			return message, errOpenCodeSkipControlRow
		}
		return message, fmt.Errorf("type %q is outside the supported pinned SessionMessage closed set", row.Type.String())
	}
	return message, nil
}

func currentMetadataString(metadata map[string]json.RawMessage, key string) (string, error) {
	var value string
	if raw := metadata[key]; len(raw) != 0 {
		if !json.Valid(raw) {
			return "", fmt.Errorf("current metadata %q contains malformed JSON; correct the metadata value in OpenCode and retry", key)
		}
		// Metadata is an upstream open record. A valid non-string value is not a
		// CWD, but must remain harmlessly optional as it was before normalization.
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", nil
		}
	}
	return value, nil
}

func requireOpenCodeCurrentFields(raw []byte, fields ...string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	for _, field := range fields {
		if _, ok := object[field]; !ok {
			return fmt.Errorf("required upstream field %q is absent", field)
		}
	}
	return nil
}

func appendOpenCodeCurrentAssistantContent(message *openCodeLegacyProjectionMessage, raw json.RawMessage, registry *openCodeCurrentIdentityRegistry, appendPart func(string, int64, any) error) error {
	var discriminator struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &discriminator); err != nil {
		return err
	}
	switch discriminator.Type {
	case "text":
		var value openCodeCurrentAssistantText
		if err := decodeOpenCodeCurrentJSON(raw, &value); err != nil {
			return err
		}
		if value.ID == "" {
			return errors.New("assistant text requires id")
		}
		return appendPart(value.ID, 0, value)
	case "reasoning":
		var value openCodeCurrentAssistantReasoning
		if err := decodeOpenCodeCurrentJSON(raw, &value); err != nil {
			return err
		}
		if value.ID == "" {
			return errors.New("assistant reasoning requires id")
		}
		created := int64(0)
		if value.Time != nil {
			created = value.Time.Created
		}
		return appendPart(value.ID, created, value)
	case "tool":
		var value openCodeCurrentAssistantTool
		if err := decodeOpenCodeCurrentJSON(raw, &value); err != nil {
			return err
		}
		if err := requireOpenCodeCurrentFields(raw, "type", "id", "name", "state", "time"); err != nil {
			return err
		}
		var state openCodeCurrentToolState
		if err := decodeOpenCodeCurrentJSON(value.State, &state); err != nil {
			return fmt.Errorf("decode tool %q state: %w", value.ID, err)
		}
		if state.Status != "pending" && state.Status != "running" && state.Status != "completed" && state.Status != "error" {
			return fmt.Errorf("tool %q state status %q is unsupported", value.ID, state.Status)
		}
		if len(state.Input) == 0 {
			return fmt.Errorf("tool %q state requires input", value.ID)
		}
		if state.Status == "pending" {
			var pending string
			if err := json.Unmarshal(state.Input, &pending); err != nil || state.Structured != nil || state.Content != nil || len(state.Result) != 0 || state.Error != nil {
				return fmt.Errorf("tool %q pending state requires string input", value.ID)
			}
		} else {
			var input map[string]json.RawMessage
			if err := json.Unmarshal(state.Input, &input); err != nil || state.Structured == nil || state.Content == nil {
				return fmt.Errorf("tool %q %s state requires object input", value.ID, state.Status)
			}
			for _, content := range state.Content {
				if content.Type == "text" && content.Text == "" {
					return fmt.Errorf("tool %q text content requires text", value.ID)
				}
				if content.Type == "file" && (content.URI == "" || content.MIME == "") {
					return fmt.Errorf("tool %q file content requires uri and mime", value.ID)
				}
				if content.Type != "text" && content.Type != "file" {
					return fmt.Errorf("tool %q content type %q is unsupported", value.ID, content.Type)
				}
			}
			if state.Status == "running" && (len(state.Result) != 0 || state.Error != nil || len(state.Attachments) != 0 || len(state.OutputPaths) != 0) {
				return fmt.Errorf("tool %q running state contains completed/error-only fields", value.ID)
			}
			if state.Status == "completed" && state.Error != nil {
				return fmt.Errorf("tool %q completed state contains error-only fields", value.ID)
			}
			if state.Status == "error" && (state.Error == nil || state.Error.Type != "unknown" || state.Error.Message == "") {
				return fmt.Errorf("tool %q error state requires an unknown error with message", value.ID)
			}
		}
		return appendPart(value.ID, value.Time.Created, map[string]any{"id": value.ID, "type": "tool", "name": value.Name, "state": state, "time": value.Time})
	default:
		return fmt.Errorf("assistant content type %q is outside the supported text/reasoning/tool closed set", discriminator.Type)
	}
}
