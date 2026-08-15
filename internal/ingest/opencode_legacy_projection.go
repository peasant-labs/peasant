package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"time"
)

const (
	openCodeLegacyProjectionFormat  = "peasant.opencode.legacy-sqlite"
	openCodeLegacyProjectionVersion = 1
	openCodeLegacyMaterializePage   = 128
)

type openCodeLegacyProjection struct {
	Format    string                            `json:"format"`
	Version   int                               `json:"version"`
	SessionID string                            `json:"session_id"`
	Messages  []openCodeLegacyProjectionMessage `json:"messages"`
}

type openCodeLegacyProjectionMessage struct {
	ID          string                         `json:"id"`
	SessionID   string                         `json:"session_id"`
	TimeCreated int64                          `json:"time_created"`
	TimeUpdated int64                          `json:"time_updated"`
	Data        json.RawMessage                `json:"data"`
	Parts       []openCodeLegacyProjectionPart `json:"parts"`
}

type openCodeLegacyProjectionPart struct {
	ID          string          `json:"id"`
	MessageID   string          `json:"message_id"`
	SessionID   string          `json:"session_id"`
	TimeCreated int64           `json:"time_created"`
	TimeUpdated int64           `json:"time_updated"`
	Data        json.RawMessage `json:"data"`
}

func (a *OpenCodeAdapter) discoverLegacySQLite(ctx context.Context, candidate OpenCodeCandidate) ([]DiscoveredSession, error) {
	if a.candidateOpener == nil {
		return nil, fmt.Errorf("discover legacy OpenCode SQLite candidate %q failed before row enumeration: source opener is nil, so typed reads cannot run; no session was exposed; construct the adapter with OpenOpenCodeSQLiteSource", candidate.Path)
	}
	path, err := NewOpenCodeSQLiteSourcePath(candidate.Path)
	if err != nil {
		return nil, err
	}
	source, err := a.candidateOpener(ctx, path, a.candidateOptions)
	if err != nil {
		return nil, fmt.Errorf("discover legacy OpenCode SQLite candidate %q failed while opening the restrictive source: %w; no session was exposed; verify source readability and retry without modifying the database", candidate.Path, err)
	}
	defer func() { _ = source.Close(context.Background()) }()

	pageSize, err := NewOpenCodeLegacyPageSize(openCodeLegacyMaterializePage)
	if err != nil {
		return nil, err
	}
	var discovered []DiscoveredSession
	var cursor *OpenCodeLegacySessionCursor
	for {
		page, readErr := source.LegacySessionIDs(ctx, OpenCodeLegacySessionPageRequest{PageSize: pageSize, After: cursor})
		if readErr != nil {
			return nil, fmt.Errorf("discover legacy OpenCode SQLite candidate %q failed while enumerating a bounded session page: %w; no partial discovery result is eligible; verify the database remains a supported legacy message/part store and retry", candidate.Path, readErr)
		}
		for _, legacyID := range page.SessionIDs {
			sessionID, idErr := NewSessionID(legacyID.String())
			if idErr != nil {
				return nil, fmt.Errorf("discover legacy OpenCode SQLite candidate %q found session identifier %q that Peasant cannot store: %w; no partial discovery result is eligible; repair the upstream identifier or use a supported export", candidate.Path, legacyID.String(), idErr)
			}
			session := DiscoveredSession{
				SessionID:        sessionID,
				Harness:          HarnessOpenCode,
				SourcePath:       ResolvedPath(candidate.Path),
				SourceFormat:     SourceFormatJSON,
				OriginalRoot:     ResolvedPath(filepath.Dir(candidate.Path)),
				TranscriptOrigin: TranscriptOriginOpenCodeLegacySQLite,
			}
			discovered = append(discovered, session)
		}
		if page.Next == nil {
			break
		}
		cursor = page.Next
	}
	if len(discovered) > 0 {
		contentModTime, statErr := legacySQLiteContentModTime(a.fs, candidate.Path)
		if statErr != nil {
			return nil, statErr
		}
		for index := range discovered {
			discovered[index].ModTime = contentModTime
		}
	}
	if closeErr := source.Close(ctx); closeErr != nil {
		return nil, fmt.Errorf("discover legacy OpenCode SQLite candidate %q failed while closing its bounded read connection: %w; no partial discovery result is eligible; retry after the source lock clears", candidate.Path, closeErr)
	}
	return discovered, nil
}

// legacySQLiteContentModTime returns the newest content-bearing source time.
// SQLite's shared-memory sidecar contains reader coordination state rather than
// committed transcript content and is deliberately never inspected here.
func legacySQLiteContentModTime(filesystem FileSystem, databasePath string) (time.Time, error) {
	databaseInfo, err := filesystem.Stat(databasePath)
	if err != nil {
		return time.Time{}, fmt.Errorf("determine legacy OpenCode SQLite freshness for %q failed while inspecting the main database: %w; discovery cannot classify sessions safely without content evidence; verify the database still exists and is readable, then retry", databasePath, err)
	}
	modified := databaseInfo.ModTime()
	walPath := databasePath + "-wal"
	walInfo, err := filesystem.Stat(walPath)
	if errors.Is(err, fs.ErrNotExist) {
		return modified, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("determine legacy OpenCode SQLite freshness for %q failed while inspecting committed WAL freshness evidence at %q: %w; discovery cannot distinguish a current source from stale managed output; fix sidecar permissions or source accessibility and retry", databasePath, walPath, err)
	}
	// A zero-length or header-only WAL can be benign reader-created residue. It
	// has no transaction frame and therefore is not content freshness evidence.
	const sqliteWALHeaderBytes = 32
	if walInfo.Size() <= sqliteWALHeaderBytes {
		return modified, nil
	}
	if walInfo.ModTime().After(modified) {
		modified = walInfo.ModTime()
	}
	return modified, nil
}

// MaterializeTranscript reads only the selected detached legacy rows and builds
// the versioned JSON transcript Peasant owns. Database, WAL, and SHM bytes never
// enter the returned data.
func (a *OpenCodeAdapter) MaterializeTranscript(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, []byte, error) {
	if session.TranscriptOrigin == TranscriptOriginOpenCodeCurrentSQLite {
		return a.materializeCurrentTranscript(ctx, session)
	}
	if session.TranscriptOrigin != TranscriptOriginOpenCodeLegacySQLite {
		return nil, nil, fmt.Errorf("materialize OpenCode session %q failed before source access: transcript origin %d is not a supported managed OpenCode SQLite origin; no managed state was written; use the file origin for JSON sessions or return a supported typed SQLite origin from discovery", session.SessionID, session.TranscriptOrigin)
	}
	legacyID, err := NewOpenCodeLegacySessionID(string(session.SessionID))
	if err != nil {
		return nil, nil, err
	}
	path, err := NewOpenCodeSQLiteSourcePath(session.SourcePath.String())
	if err != nil {
		return nil, nil, err
	}
	if a.candidateOpener == nil {
		return nil, nil, fmt.Errorf("materialize legacy OpenCode SQLite session %q failed before source access: source opener is nil; raw database bytes were not read and no managed state was written; construct the production adapter with OpenOpenCodeSQLiteSource", session.SessionID)
	}
	source, err := a.candidateOpener(ctx, path, a.candidateOptions)
	if err != nil {
		return nil, nil, fmt.Errorf("materialize legacy OpenCode SQLite session %q failed while opening %q read-only: %w; no managed artifact or store row was written; verify the selected database remains readable and retry", session.SessionID, session.SourcePath, err)
	}
	pageSize, err := NewOpenCodeLegacyPageSize(openCodeLegacyMaterializePage)
	if err != nil {
		_ = source.Close(context.Background())
		return nil, nil, err
	}
	projection, readErr := readOpenCodeLegacyProjection(ctx, source, legacyID, pageSize)
	closeErr := source.Close(ctx)
	if readErr != nil || closeErr != nil {
		return nil, nil, fmt.Errorf("materialize legacy OpenCode SQLite session %q failed while reading selected message/part rows and closing the bounded source: %w; no partial managed artifact or store row was written; fix malformed required row JSON or retry after source locks clear", session.SessionID, errors.Join(readErr, closeErr))
	}
	if len(projection.Messages) == 0 {
		return nil, nil, fmt.Errorf("materialize legacy OpenCode SQLite session %q from %q produced no messages even though discovery enumerated it; no empty managed artifact was written; retry after OpenCode finishes its transaction or remove the stale source row", session.SessionID, session.SourcePath)
	}
	data, err := json.Marshal(projection)
	if err != nil {
		return nil, nil, fmt.Errorf("materialize legacy OpenCode SQLite session %q failed while encoding the versioned managed JSON projection: %w; detached source rows remain unchanged and no managed state was written; report the unsupported row shape", session.SessionID, err)
	}
	data = append(data, '\n')
	metadata, err := a.metadataFromManagedProjection(ctx, session, projection)
	if err != nil {
		return nil, nil, err
	}
	return metadata, data, nil
}

func readOpenCodeLegacyProjection(ctx context.Context, source OpenCodeSQLiteSource, sessionID OpenCodeLegacySessionID, pageSize OpenCodeLegacyPageSize) (openCodeLegacyProjection, error) {
	projection := openCodeLegacyProjection{Format: openCodeLegacyProjectionFormat, Version: openCodeLegacyProjectionVersion, SessionID: sessionID.String(), Messages: []openCodeLegacyProjectionMessage{}}
	var messageCursor *OpenCodeLegacyMessageCursor
	for {
		page, err := source.LegacyMessages(ctx, OpenCodeLegacyMessagePageRequest{SessionID: sessionID, PageSize: pageSize, After: messageCursor})
		if err != nil {
			return openCodeLegacyProjection{}, err
		}
		for _, row := range page.Messages {
			message := openCodeLegacyProjectionMessage{ID: row.ID.String(), SessionID: row.SessionID.String(), TimeCreated: row.TimeCreated, TimeUpdated: row.TimeUpdated, Data: json.RawMessage(row.Data), Parts: []openCodeLegacyProjectionPart{}}
			var partCursor *OpenCodeLegacyPartCursor
			for {
				parts, partErr := source.LegacyParts(ctx, OpenCodeLegacyPartPageRequest{SessionID: sessionID, MessageID: row.ID, PageSize: pageSize, After: partCursor})
				if partErr != nil {
					return openCodeLegacyProjection{}, partErr
				}
				for _, part := range parts.Parts {
					message.Parts = append(message.Parts, openCodeLegacyProjectionPart{ID: part.ID.String(), MessageID: part.MessageID.String(), SessionID: part.SessionID.String(), TimeCreated: part.TimeCreated, TimeUpdated: part.TimeUpdated, Data: json.RawMessage(part.Data)})
				}
				if parts.Next == nil {
					break
				}
				partCursor = parts.Next
			}
			projection.Messages = append(projection.Messages, message)
		}
		if page.Next == nil {
			break
		}
		messageCursor = page.Next
	}
	return projection, nil
}

func (a *OpenCodeAdapter) metadataFromManagedProjection(ctx context.Context, session DiscoveredSession, projection openCodeLegacyProjection) (*UnifiedMetadata, error) {
	metadata := NewUnifiedMetadata()
	metadata.SessionID = session.SessionID
	metadata.ModelHarness = HarnessOpenCode
	metadata.Source = SourceInfo{FilePath: session.SourcePath.String(), Format: SourceFormatJSON}
	metadata.CWD = session.CWD
	if session.ParentUUID != nil {
		parent := *session.ParentUUID
		metadata.ParentUUID = &parent
	}

	kind := managedOpenCodeProjectionKind(session.TranscriptOrigin)
	messages, err := parseManagedOpenCodeSemanticMessages(projection, kind)
	if err != nil {
		return nil, fmt.Errorf("extract metadata from %s projection for session %q failed while decoding the shared semantic corpus: %w; no managed artifact or store state was committed, so repair the malformed normalized row and retry harvest", kind, session.SessionID, err)
	}
	summary := summarizeOpenCodeSemanticMessages(messages)
	metadata.Stats = StatsInfo{TurnCount: summary.turnCount, ToolCallCount: summary.toolCallCount, TokensIn: summary.tokensIn, TokensOut: summary.tokensOut, DurationMs: summary.endMS - summary.startMS}
	metadata.Timestamp = TimestampInfo{Start: summary.startMS, End: summary.endMS}
	metadata.Version = summary.version
	workDir := session.CWD
	if workDir == "" {
		workDir = summary.cwd
		metadata.CWD = workDir
	}
	projectMissing := workDir == ""
	modelMissing := summary.modelID == ""
	if !modelMissing {
		if parsed, modelErr := NewModelID(summary.modelID); modelErr == nil {
			metadata.Model = parsed
		} else {
			modelMissing = true
		}
	}
	if metadata.ParentUUID == nil && summary.parentID != "" {
		if parent, parentErr := NewSessionID(summary.parentID); parentErr == nil {
			metadata.ParentUUID = &parent
		}
	}

	var gitBranch, gitRemote, gitWorktree, gitTracking *string
	if workDir != "" {
		if value, gitErr := a.git.Branch(ctx, workDir); gitErr == nil && value != "" {
			gitBranch = &value
		}
		if value, gitErr := a.git.RemoteURL(ctx, workDir); gitErr == nil && value != "" {
			gitRemote = &value
		}
		if value, gitErr := a.git.Worktree(ctx, workDir); gitErr == nil && value != "" {
			gitWorktree = &value
		}
		if value, gitErr := a.git.TrackingBranch(ctx, workDir); gitErr == nil && value != "" {
			gitTracking = &value
		}
	}
	identityPath := workDir
	if gitWorktree != nil {
		identityPath = *gitWorktree
	}
	if identityPath == "" {
		identityPath = session.SourcePath.String()
	}
	remote := ""
	if gitRemote != nil {
		remote = *gitRemote
	}
	projectHash, hostSlug, err := DeriveProjectIdentifiersWithGit(ctx, a.salt, a.git, remote, identityPath)
	if err != nil {
		return nil, fmt.Errorf("extract metadata from %s projection for session %q failed while deriving stable source identity from %q: %w; no managed artifact or store state was committed, so verify the selected path and installation salt and retry", kind, session.SessionID, identityPath, err)
	}
	metadata.HostSlug = hostSlug
	metadata.Project.Hash = projectHash
	if workDir != "" {
		metadata.Project.FilePath = workDir
		metadata.Project.Name = filepath.Base(workDir)
	}
	if gitBranch != nil || gitRemote != nil || gitWorktree != nil || gitTracking != nil {
		metadata.Git = GitContext{Branch: gitBranch, Remote: gitRemote, Worktree: gitWorktree, Tracking: gitTracking}
	}
	if modelMissing {
		metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, DiagnosticEntry{ErrorType: "missing_model", Location: fmt.Sprintf("%s session %s", kind, session.SessionID), Message: "selected assistant messages contain no valid model identifier; Peasant left model empty rather than inventing one", Remediation: "Use an OpenCode source that records a supported assistant model or retain the session with an unknown model."})
	}
	if projectMissing {
		metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, DiagnosticEntry{ErrorType: "missing_project", Location: fmt.Sprintf("%s session %s in %s", kind, session.SessionID, session.SourcePath), Message: "selected messages contain no working-directory or project evidence; Peasant left project name and path empty and used only the source path for stable local placement", Remediation: "Use an OpenCode source that records path.cwd, cwd, or directory if project attribution is required."})
	}
	return &metadata, nil
}

// recognizeManagedOpenCodeProjection distinguishes ordinary legacy JSON from a
// Peasant-owned envelope. A recognizable managed marker with invalid bytes is
// an error, not a file-origin fallback, so recovery cannot erase an index by
// treating corruption as an empty legacy directory.
func recognizeManagedOpenCodeProjection(data []byte, sessionID SessionID) (TranscriptOrigin, error) {
	formatMarker := managedOpenCodeFormatMarker(data)
	for _, candidate := range []struct {
		origin  TranscriptOrigin
		format  string
		version int
	}{
		{TranscriptOriginOpenCodeLegacySQLite, openCodeLegacyProjectionFormat, openCodeLegacyProjectionVersion},
		{TranscriptOriginOpenCodeCurrentSQLite, openCodeCurrentProjectionFormat, openCodeCurrentProjectionVersion},
	} {
		if formatMarker != candidate.format {
			continue
		}
		if _, err := decodeManagedOpenCodeProjection(data, candidate.format, candidate.version, sessionID); err != nil {
			return TranscriptOriginFile, fmt.Errorf("recognize managed OpenCode projection for session %q failed: found managed format marker %q but its envelope is corrupt: %w; recovery stopped before legacy fallback, so re-run harvest to regenerate the managed transcript", sessionID, candidate.format, err)
		}
		return candidate.origin, nil
	}
	return TranscriptOriginFile, nil
}

func managedOpenCodeFormatMarker(data []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return ""
	}
	for decoder.More() {
		keyToken, keyErr := decoder.Token()
		key, ok := keyToken.(string)
		if keyErr != nil || !ok {
			return ""
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return ""
		}
		if key != "format" {
			continue
		}
		var format string
		if err := json.Unmarshal(value, &format); err != nil {
			return ""
		}
		return format
	}
	return ""
}

func decodeManagedOpenCodeProjection(data []byte, expectedFormat string, expectedVersion int, sessionID SessionID) (openCodeLegacyProjection, error) {
	fields, err := decodeOpenCodeProjectionObject(data, "managed envelope", []string{"format", "version", "session_id", "messages"})
	if err != nil {
		return openCodeLegacyProjection{}, err
	}
	var projection openCodeLegacyProjection
	if err := json.Unmarshal(fields["format"], &projection.Format); err != nil {
		return projection, fmt.Errorf("decode managed envelope format: %w", err)
	}
	if err := json.Unmarshal(fields["version"], &projection.Version); err != nil {
		return projection, fmt.Errorf("decode managed envelope version: %w", err)
	}
	if err := json.Unmarshal(fields["session_id"], &projection.SessionID); err != nil {
		return projection, fmt.Errorf("decode managed envelope session_id: %w", err)
	}
	if projection.Format != expectedFormat || projection.Version != expectedVersion || projection.SessionID != string(sessionID) {
		return projection, fmt.Errorf("managed envelope identity format=%q version=%d session_id=%q does not match expected format=%q version=%d selected_session_id=%q", projection.Format, projection.Version, projection.SessionID, expectedFormat, expectedVersion, sessionID)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(fields["messages"], &rows); err != nil {
		return projection, fmt.Errorf("decode managed envelope messages: %w", err)
	}
	if len(rows) == 0 {
		return projection, errors.New("managed envelope messages must be present and non-empty")
	}
	projection.Messages = make([]openCodeLegacyProjectionMessage, 0, len(rows))
	identities := make(map[string]string, len(rows))
	for index, raw := range rows {
		message, messageErr := decodeManagedOpenCodeProjectionMessage(raw, projection.SessionID, identities)
		if messageErr != nil {
			return projection, fmt.Errorf("decode managed envelope message %d: %w", index, messageErr)
		}
		projection.Messages = append(projection.Messages, message)
	}
	if _, err := parseManagedOpenCodeSemanticMessages(projection, "managed OpenCode"); err != nil {
		return projection, fmt.Errorf("validate managed envelope semantic corpus: %w", err)
	}
	return projection, nil
}

func decodeManagedOpenCodeProjectionMessage(raw json.RawMessage, sessionID string, identities map[string]string) (openCodeLegacyProjectionMessage, error) {
	fields, err := decodeOpenCodeProjectionObject(raw, "managed message", []string{"id", "session_id", "time_created", "time_updated", "data", "parts"})
	if err != nil {
		return openCodeLegacyProjectionMessage{}, err
	}
	var message openCodeLegacyProjectionMessage
	if err := json.Unmarshal(fields["id"], &message.ID); err != nil {
		return message, fmt.Errorf("decode id: %w", err)
	}
	if message.ID == "" {
		return message, errors.New("id must be non-empty")
	}
	if err := json.Unmarshal(fields["session_id"], &message.SessionID); err != nil {
		return message, fmt.Errorf("decode session_id: %w", err)
	}
	if message.SessionID != sessionID {
		return message, fmt.Errorf("session_id %q does not match envelope session_id %q", message.SessionID, sessionID)
	}
	if err := json.Unmarshal(fields["time_created"], &message.TimeCreated); err != nil {
		return message, fmt.Errorf("decode time_created: %w", err)
	}
	if err := json.Unmarshal(fields["time_updated"], &message.TimeUpdated); err != nil {
		return message, fmt.Errorf("decode time_updated: %w", err)
	}
	if err := requireJSONObject(fields["data"]); err != nil {
		return message, fmt.Errorf("decode data: %w", err)
	}
	message.Data = append(json.RawMessage(nil), fields["data"]...)
	if prior, exists := identities[message.ID]; exists {
		return message, fmt.Errorf("id %q duplicates %s", message.ID, prior)
	}
	identities[message.ID] = "a message"
	var parts []json.RawMessage
	if err := json.Unmarshal(fields["parts"], &parts); err != nil {
		return message, fmt.Errorf("decode parts: %w", err)
	}
	message.Parts = make([]openCodeLegacyProjectionPart, 0, len(parts))
	for index, partRaw := range parts {
		part, partErr := decodeManagedOpenCodeProjectionPart(partRaw, message.ID, sessionID, identities)
		if partErr != nil {
			return message, fmt.Errorf("decode part %d: %w", index, partErr)
		}
		message.Parts = append(message.Parts, part)
	}
	return message, nil
}

func decodeManagedOpenCodeProjectionPart(raw json.RawMessage, messageID, sessionID string, identities map[string]string) (openCodeLegacyProjectionPart, error) {
	fields, err := decodeOpenCodeProjectionObject(raw, "managed part", []string{"id", "message_id", "session_id", "time_created", "time_updated", "data"})
	if err != nil {
		return openCodeLegacyProjectionPart{}, err
	}
	var part openCodeLegacyProjectionPart
	if err := json.Unmarshal(fields["id"], &part.ID); err != nil {
		return part, fmt.Errorf("decode id: %w", err)
	}
	if part.ID != "" {
		if prior, exists := identities[part.ID]; exists {
			return part, fmt.Errorf("id %q duplicates %s", part.ID, prior)
		}
	}
	if err := json.Unmarshal(fields["message_id"], &part.MessageID); err != nil {
		return part, fmt.Errorf("decode message_id: %w", err)
	}
	if part.MessageID != messageID {
		return part, fmt.Errorf("message_id %q does not match containing message %q", part.MessageID, messageID)
	}
	if err := json.Unmarshal(fields["session_id"], &part.SessionID); err != nil {
		return part, fmt.Errorf("decode session_id: %w", err)
	}
	if part.SessionID != sessionID {
		return part, fmt.Errorf("session_id %q does not match envelope session_id %q", part.SessionID, sessionID)
	}
	if err := json.Unmarshal(fields["time_created"], &part.TimeCreated); err != nil {
		return part, fmt.Errorf("decode time_created: %w", err)
	}
	if err := json.Unmarshal(fields["time_updated"], &part.TimeUpdated); err != nil {
		return part, fmt.Errorf("decode time_updated: %w", err)
	}
	if err := requireJSONObject(fields["data"]); err != nil {
		return part, fmt.Errorf("decode data: %w", err)
	}
	part.Data = append(json.RawMessage(nil), fields["data"]...)
	if part.ID != "" {
		identities[part.ID] = "a part"
	}
	return part, nil
}

func decodeOpenCodeProjectionObject(data []byte, location string, expected []string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		if err != nil {
			return nil, fmt.Errorf("decode %s opening object: %w", location, managedProjectionJSONError(data, err))
		}
		return nil, fmt.Errorf("decode %s: expected JSON object", location)
	}
	allowed := make(map[string]struct{}, len(expected))
	for _, key := range expected {
		allowed[key] = struct{}{}
	}
	fields := make(map[string]json.RawMessage, len(expected))
	for decoder.More() {
		keyToken, tokenErr := decoder.Token()
		key, ok := keyToken.(string)
		if tokenErr != nil || !ok {
			return nil, fmt.Errorf("decode %s field name: %w", location, tokenErr)
		}
		if _, ok := allowed[key]; !ok {
			return nil, fmt.Errorf("decode %s: unknown field %q", location, key)
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, fmt.Errorf("decode %s: duplicate field %q", location, key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode %s field %q: %w", location, key, err)
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("decode %s closing object: %w", location, managedProjectionJSONError(data, err))
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("decode %s: %w", location, err)
	}
	for _, key := range expected {
		if _, present := fields[key]; !present {
			return nil, fmt.Errorf("decode %s: required field %q is missing", location, key)
		}
	}
	return fields, nil
}

func managedProjectionJSONError(data []byte, fallback error) error {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	return fallback
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func requireJSONObject(data json.RawMessage) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	if object == nil {
		return errors.New("must be a JSON object")
	}
	return nil
}
