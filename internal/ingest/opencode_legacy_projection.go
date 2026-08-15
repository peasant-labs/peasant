package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type openCodeLegacyMessageEvidence struct {
	Role       string `json:"role"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Version    string `json:"version"`
	Directory  string `json:"directory"`
	CWD        string `json:"cwd"`
	ParentID   string `json:"parentID"`
	Title      string `json:"title"`
	Model      struct {
		ID      string `json:"id"`
		ModelID string `json:"modelID"`
	} `json:"model"`
	Path struct {
		CWD  string `json:"cwd"`
		Root string `json:"root"`
	} `json:"path"`
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Tokens *struct {
		Input  int `json:"input"`
		Output int `json:"output"`
	} `json:"tokens"`
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
			if info, statErr := a.fs.Stat(candidate.Path); statErr == nil {
				session.ModTime = info.ModTime()
			}
			if projection, projectionErr := readOpenCodeLegacyProjection(ctx, source, legacyID, pageSize); projectionErr == nil {
				applyLegacyDiscoveryEvidence(&session, projection)
			}
			discovered = append(discovered, session)
		}
		if page.Next == nil {
			break
		}
		cursor = page.Next
	}
	if closeErr := source.Close(ctx); closeErr != nil {
		return nil, fmt.Errorf("discover legacy OpenCode SQLite candidate %q failed while closing its bounded read connection: %w; no partial discovery result is eligible; retry after the source lock clears", candidate.Path, closeErr)
	}
	return discovered, nil
}

func applyLegacyDiscoveryEvidence(session *DiscoveredSession, projection openCodeLegacyProjection) {
	for _, message := range projection.Messages {
		var evidence openCodeLegacyMessageEvidence
		if json.Unmarshal(message.Data, &evidence) != nil {
			continue
		}
		cwd := legacyEvidenceCWD(evidence)
		if session.CWD == "" && cwd != "" {
			session.CWD = cwd
			session.ProjectName = filepath.Base(cwd)
		}
		if session.Title == "" {
			session.Title = evidence.Title
		}
		created := message.TimeCreated
		if evidence.Time.Created > 0 {
			created = evidence.Time.Created
		}
		if created > 0 && (session.CreatedAt.IsZero() || created < session.CreatedAt.UnixMilli()) {
			session.CreatedAt = time.UnixMilli(created)
		}
		if session.ParentUUID == nil && evidence.ParentID != "" {
			if parent, err := NewSessionID(evidence.ParentID); err == nil {
				session.ParentUUID = &parent
			}
		}
	}
}

// MaterializeTranscript reads only the selected detached legacy rows and builds
// the versioned JSON transcript Peasant owns. Database, WAL, and SHM bytes never
// enter the returned data.
func (a *OpenCodeAdapter) MaterializeTranscript(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, []byte, error) {
	if session.TranscriptOrigin != TranscriptOriginOpenCodeLegacySQLite {
		return nil, nil, fmt.Errorf("materialize OpenCode session %q failed before source access: transcript origin %d is not legacy SQLite, so this adapter cannot safely replace file-copy behavior; no managed state was written; use ExtractMetadata for JSON sessions", session.SessionID, session.TranscriptOrigin)
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
	metadata, err := a.metadataFromLegacyProjection(ctx, session, projection)
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

func (a *OpenCodeAdapter) metadataFromLegacyProjection(ctx context.Context, session DiscoveredSession, projection openCodeLegacyProjection) (*UnifiedMetadata, error) {
	metadata := NewUnifiedMetadata()
	metadata.SessionID = session.SessionID
	metadata.ModelHarness = HarnessOpenCode
	metadata.Source = SourceInfo{FilePath: session.SourcePath.String(), Format: SourceFormatJSON}
	metadata.CWD = session.CWD
	if session.ParentUUID != nil {
		parent := *session.ParentUUID
		metadata.ParentUUID = &parent
	}

	start, end := int64(0), int64(0)
	workDir := session.CWD
	modelMissing := true
	projectMissing := workDir == ""
	for _, message := range projection.Messages {
		var evidence openCodeLegacyMessageEvidence
		if err := json.Unmarshal(message.Data, &evidence); err != nil {
			return nil, fmt.Errorf("extract metadata for legacy OpenCode SQLite session %q failed while decoding selected message row %q: %w; the managed projection and store state were not committed; fix the malformed required row JSON in OpenCode and retry", session.SessionID, message.ID, err)
		}
		created := message.TimeCreated
		if evidence.Time.Created > 0 {
			created = evidence.Time.Created
		}
		completed := message.TimeUpdated
		if evidence.Time.Completed > 0 {
			completed = evidence.Time.Completed
		}
		if created > 0 && (start == 0 || created < start) {
			start = created
		}
		if completed > end {
			end = completed
		}
		if evidence.Role == RoleUser.String() || evidence.Role == RoleAssistant.String() {
			metadata.Stats.TurnCount++
		}
		if evidence.Role == RoleAssistant.String() {
			if evidence.Tokens != nil {
				metadata.Stats.TokensIn += evidence.Tokens.Input
				metadata.Stats.TokensOut += evidence.Tokens.Output
			}
			if modelMissing {
				model := legacyEvidenceModel(evidence)
				if model != "" {
					if parsed, modelErr := NewModelID(model); modelErr == nil {
						metadata.Model = parsed
						modelMissing = false
					}
				}
			}
		}
		if metadata.Version == "" {
			metadata.Version = evidence.Version
		}
		if workDir == "" {
			workDir = legacyEvidenceCWD(evidence)
			if workDir != "" {
				projectMissing = false
				metadata.CWD = workDir
			}
		}
		if metadata.ParentUUID == nil && evidence.ParentID != "" {
			if parent, parentErr := NewSessionID(evidence.ParentID); parentErr == nil {
				metadata.ParentUUID = &parent
			}
		}
		for _, part := range message.Parts {
			var shape struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(part.Data, &shape); err != nil {
				return nil, fmt.Errorf("extract metadata for legacy OpenCode SQLite session %q failed while decoding selected part row %q for message %q: %w; no partial managed or store state was committed; fix the malformed required row JSON and retry", session.SessionID, part.ID, message.ID, err)
			}
			if shape.Type == "tool" || shape.Type == "tool_use" {
				metadata.Stats.ToolCallCount++
			}
		}
	}
	if end < start {
		end = start
	}
	metadata.Timestamp = TimestampInfo{Start: start, End: end}
	metadata.Stats.DurationMs = end - start

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
		return nil, fmt.Errorf("extract metadata for legacy OpenCode SQLite session %q failed while deriving stable source identity from %q: %w; no managed or store state was committed; verify the selected path and installation salt", session.SessionID, identityPath, err)
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
		metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, DiagnosticEntry{ErrorType: "missing_model", Location: fmt.Sprintf("legacy SQLite session %s", session.SessionID), Message: "selected assistant rows contain no valid model identifier; Peasant left model empty rather than inventing one", Remediation: "Use an OpenCode export that records modelID or retain the session with an unknown model."})
	}
	if projectMissing {
		metadata.Diagnostics.Warnings = append(metadata.Diagnostics.Warnings, DiagnosticEntry{ErrorType: "missing_project", Location: fmt.Sprintf("legacy SQLite session %s in %s", session.SessionID, session.SourcePath), Message: "selected message rows contain no working-directory or project evidence; Peasant left project name and path empty and used only the database path for stable local placement", Remediation: "Use an OpenCode export that records path.cwd, cwd, or directory if project attribution is required."})
	}
	return &metadata, nil
}

func legacyEvidenceCWD(evidence openCodeLegacyMessageEvidence) string {
	if evidence.Path.CWD != "" {
		return evidence.Path.CWD
	}
	if evidence.CWD != "" {
		return evidence.CWD
	}
	return evidence.Directory
}

func legacyEvidenceModel(evidence openCodeLegacyMessageEvidence) string {
	if evidence.ModelID != "" {
		return evidence.ModelID
	}
	if evidence.Model.ModelID != "" {
		return evidence.Model.ModelID
	}
	return evidence.Model.ID
}

func isOpenCodeLegacyProjection(data []byte, sessionID SessionID) bool {
	var identity struct {
		Format    string `json:"format"`
		Version   int    `json:"version"`
		SessionID string `json:"session_id"`
	}
	return json.Unmarshal(data, &identity) == nil && identity.Format == openCodeLegacyProjectionFormat && identity.Version == openCodeLegacyProjectionVersion && identity.SessionID == string(sessionID)
}
