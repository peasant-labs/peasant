package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/salt"
)

// OpenCodeAdapter discovers and extracts metadata from OpenCode JSON sessions.
type OpenCodeAdapter struct {
	fs   FileSystem
	git  GitResolver
	salt salt.Salt
}

var _ SourceAdapter = (*OpenCodeAdapter)(nil)

// NewOpenCodeAdapter constructs an OpenCodeAdapter with injected dependencies.
func NewOpenCodeAdapter(fs FileSystem, git GitResolver, s salt.Salt) *OpenCodeAdapter {
	return &OpenCodeAdapter{fs: fs, git: git, salt: s}
}

// Harness returns HarnessOpenCode.
func (a *OpenCodeAdapter) Harness() Harness {
	return HarnessOpenCode
}

// Discover reads project and session JSON files to find all sessions.
//
// For each path in cfg.Paths it expects an OpenCode storage layout:
//
//	{path}/project/{hash}.json
//	{path}/session/{hash}/ses_{id}.json
//
// Sessions with a non-empty parentID are linked via ParentUUID.
func (a *OpenCodeAdapter) Discover(ctx context.Context, cfg SourceConfig) ([]DiscoveredSession, error) {
	var discovered []DiscoveredSession

	for _, root := range cfg.Paths {
		rootStr := root.String()

		// Step 1: walk storage/session/{hash}/ directories
		sessionDir := filepath.Join(rootStr, defaults.OpenCodeDirStorage.String(), defaults.OpenCodeDirSession.String())
		hashEntries, err := a.fs.ReadDir(sessionDir)
		if err != nil {
			// No session directory → no sessions under this root.
			continue
		}

		for _, hashEntry := range hashEntries {
			if !hashEntry.IsDir() {
				continue
			}
			hashDirPath := filepath.Join(sessionDir, hashEntry.Name())

			sesEntries, err := a.fs.ReadDir(hashDirPath)
			if err != nil {
				continue
			}

			for _, sesEntry := range sesEntries {
				name := sesEntry.Name()
				if !strings.HasPrefix(name, defaults.OpenCodeSessionPrefix) || !strings.HasSuffix(name, defaults.ExtJSON.String()) {
					continue
				}
				sesPath := filepath.Join(hashDirPath, name)

				// Read session JSON to extract id and parentID.
				data, err := a.fs.ReadFile(sesPath)
				if err != nil {
					continue
				}
				var ses openCodeSession
				if err := json.Unmarshal(data, &ses); err != nil {
					continue
				}

				sessionID, err := NewSessionID(ses.ID)
				if err != nil {
					continue
				}

				// Resolve ModTime from file stat.
				fi, err := a.fs.Stat(sesPath)
				var modTime time.Time
				if err == nil {
					modTime = fi.ModTime()
				}

				// Build DiscoveredSession.
				var createdAt time.Time
				if ses.Time.Created > 0 {
					createdAt = time.UnixMilli(ses.Time.Created)
				}
				ds := DiscoveredSession{
					SessionID:    sessionID,
					Harness:      HarnessOpenCode,
					SourcePath:   ResolvedPath(sesPath),
					SourceFormat: SourceFormatJSON,
					OriginalRoot: root,
					ModTime:      modTime,
					Title:        ses.Title,
					ProjectName:  filepath.Base(ses.Directory),
					CWD:          ses.Directory,
					CreatedAt:    createdAt,
				}

				// Wire up parent for subagent sessions.
				if ses.ParentID != "" {
					parentID, err := NewSessionID(ses.ParentID)
					if err == nil {
						ds.ParentUUID = &parentID
					}
				}

				discovered = append(discovered, ds)
			}
		}
	}

	return discovered, nil
}

// ExtractMetadata builds UnifiedMetadata from session + message files.
//
// It reads:
//  1. The session JSON at session.SourcePath for id, version, projectID, directory, parentID, time.
//  2. All message files under {root}/message/ses_{id}/ for turn and tool-call counts.
//  3. The corresponding project JSON for the worktree path.
//  4. Git metadata from the session's working directory.
func (a *OpenCodeAdapter) ExtractMetadata(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, error) {
	// ── 1. Read session JSON ──────────────────────────────────────────────────
	sesData, err := a.fs.ReadFile(session.SourcePath.String())
	if err != nil {
		return nil, fmt.Errorf("opencode: read session %s: %w", session.SessionID, err)
	}
	var ses openCodeSession
	if err := json.Unmarshal(sesData, &ses); err != nil {
		return nil, fmt.Errorf("opencode: parse session %s: %w", session.SessionID, err)
	}

	// ── 2. Count messages (turns + tool calls) ────────────────────────────────
	// The message directory lives at {root}/storage/message/ses_{id}/
	storageRoot := resolveStorageRoot(session)
	turnCount, toolCallCount, tokensIn, tokensOut := a.countMessages(ctx, storageRoot, ses.ID)

	// ── 3. Resolve git info ────────────────────────────────────────────────────
	workDir := ses.Directory
	if workDir == "" {
		// Session has no directory; use storage root as a safe fallback for git resolution.
		// Git calls will likely fail, but worktreeForHash will be a valid absolute path
		// rather than "/" which produces invalid HostSlug values.
		workDir = storageRoot
	}

	var gitBranch, gitRemote, gitWorktree *string

	branch, err := a.git.Branch(ctx, workDir)
	if err == nil && branch != "" {
		b := branch
		gitBranch = &b
	}

	remote, err := a.git.RemoteURL(ctx, workDir)
	if err == nil && remote != "" {
		r := remote
		gitRemote = &r
	}

	wt, err := a.git.Worktree(ctx, workDir)
	if err == nil && wt != "" {
		w := wt
		gitWorktree = &w
	}

	var gitTracking *string
	tracking, err := a.git.TrackingBranch(ctx, workDir)
	if err == nil && tracking != "" {
		tr := tracking
		gitTracking = &tr
	}

	// ── 4. Derive ProjectHash and HostSlug ────────────────────────────────────
	// Use git remote if available; otherwise fall back to worktree path.
	var remoteForHash string
	if gitRemote != nil {
		remoteForHash = *gitRemote
	}

	var worktreeForHash string
	if gitWorktree != nil {
		worktreeForHash = *gitWorktree
	} else {
		worktreeForHash = workDir
	}

	projectHash, hostSlug, err := DeriveProjectIdentifiersWithGit(ctx, a.salt, a.git, remoteForHash, worktreeForHash)
	if err != nil {
		return nil, fmt.Errorf("opencode: derive project identifiers for session %s: %w", session.SessionID, err)
	}

	// ── 5. Look up project worktree path from project JSON ────────────────────
	projectFilePath := worktreeForHash
	projectName := filepath.Base(projectFilePath)

	// Try to enrich from project JSON if we have a projectID.
	if ses.ProjectID != "" {
		pw, _ := a.loadProjectWorktrees(storageRoot)
		if wtp, ok := pw[ses.ProjectID]; ok && wtp != "" {
			projectFilePath = wtp
			projectName = filepath.Base(wtp)
		}
	}

	// ── 6. Resolve model ID ───────────────────────────────────────────────────
	// modelID comes from the most recent assistant message; we collect the first one found.
	modelIDStr := a.findModelID(ctx, storageRoot, ses.ID)

	var modelID ModelID
	modelMissing := false
	if modelIDStr != "" {
		modelID, _ = NewModelID(modelIDStr)
	} else {
		modelMissing = true
	}

	// ── 7. Compute duration ────────────────────────────────────────────────────
	var durationMs int64
	if ses.Time.Updated > 0 && ses.Time.Created > 0 {
		durationMs = ses.Time.Updated - ses.Time.Created
	}

	// ── 8. Build UnifiedMetadata ───────────────────────────────────────────────
	md := NewUnifiedMetadata()
	md.SessionID = session.SessionID
	md.ModelHarness = HarnessOpenCode
	md.Model = modelID
	md.Version = ses.Version

	if session.ParentUUID != nil {
		parent := *session.ParentUUID
		md.ParentUUID = &parent
	}

	ingested := time.Now().UnixMilli()
	md.Timestamp = TimestampInfo{
		Start:    ses.Time.Created,
		End:      ses.Time.Updated,
		Ingested: &ingested,
	}

	md.Source = SourceInfo{
		FilePath: session.SourcePath.String(),
		Format:   SourceFormatJSON,
	}

	// Git fields are nil when git is unavailable.
	if gitBranch != nil || gitRemote != nil || gitWorktree != nil || gitTracking != nil {
		md.Git = GitContext{
			Branch:   gitBranch,
			Remote:   gitRemote,
			Worktree: gitWorktree,
			Tracking: gitTracking,
		}
	}

	md.Project = ProjectInfo{
		Hash:     projectHash,
		FilePath: projectFilePath,
		Name:     projectName,
	}

	md.HostSlug = hostSlug

	// Store the real working directory for context-aware slug redaction.
	md.CWD = ses.Directory

	md.Stats = StatsInfo{
		TurnCount:     turnCount,
		ToolCallCount: toolCallCount,
		DurationMs:    durationMs,
		TokensIn:      tokensIn,
		TokensOut:     tokensOut,
	}

	if modelMissing {
		md.Diagnostics.Warnings = append(md.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "missing_model",
			Location:    fmt.Sprintf("session %s", ses.ID),
			Message:     "no model information found in session messages",
			Remediation: "Check if the OpenCode session has assistant messages with model information.",
		})
	}

	return &md, nil
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// openCodeProject represents the JSON structure of project/{hash}.json.
type openCodeProject struct {
	ID       string `json:"id"`
	Worktree string `json:"worktree"`
	VCS      string `json:"vcs"`
	Time     struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
}

// openCodeSession represents the JSON structure of session/{hash}/ses_{id}.json.
type openCodeSession struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	Version   string `json:"version"`
	ProjectID string `json:"projectID"`
	Directory string `json:"directory"`
	ParentID  string `json:"parentID"`
	Title     string `json:"title"`
	Time      struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
}

// openCodeMessage represents the JSON structure of message/ses_{id}/msg_{id}.json.
type openCodeMessage struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Role      string `json:"role"`
	ModelID   string `json:"modelID"`
	Time      struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Tokens *struct {
		Input     int `json:"input"`
		Output    int `json:"output"`
		Reasoning int `json:"reasoning"`
		Cache     struct {
			Read  int `json:"read"`
			Write int `json:"write"`
		} `json:"cache"`
	} `json:"tokens,omitempty"`
}

// loadProjectWorktrees reads all project JSON files under {root}/project/ and
// returns a map of projectID → worktree path.
func (a *OpenCodeAdapter) loadProjectWorktrees(storageRoot string) (map[string]string, error) {
	projectDir := filepath.Join(storageRoot, defaults.OpenCodeDirProject.String())
	entries, err := a.fs.ReadDir(projectDir)
	if err != nil {
		return nil, fmt.Errorf("opencode: read project dir %q: %w", projectDir, err)
	}

	result := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), defaults.ExtJSON.String()) {
			continue
		}
		p := filepath.Join(projectDir, e.Name())
		data, err := a.fs.ReadFile(p)
		if err != nil {
			continue
		}
		var proj openCodeProject
		if err := json.Unmarshal(data, &proj); err != nil {
			continue
		}
		if proj.ID != "" {
			result[proj.ID] = proj.Worktree
		}
	}
	return result, nil
}

// countMessages reads all msg_*.json files under {root}/message/{sessionID}/
// and returns (userTurns+assistantTurns, inferredToolCalls).
//
// Directory naming contract: the on-disk layout uses the full session ID string
// as the directory name verbatim — e.g. "ses_3cd91f52effeXd3QAJ54jOyzv5".
// The "ses_" prefix is already part of the session ID value stored in the JSON
// ("id" field), so no prefix is added or stripped here. sessionID is used
// directly as the directory component under {root}/message/.
func (a *OpenCodeAdapter) countMessages(_ context.Context, storageRoot, sessionID string) (turnCount, toolCallCount, tokensIn, tokensOut int) {
	msgDir := filepath.Join(storageRoot, defaults.OpenCodeDirMessage.String(), sessionID)
	entries, err := a.fs.ReadDir(msgDir)
	if err != nil {
		return 0, 0, 0, 0
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), defaults.ExtJSON.String()) {
			continue
		}
		data, err := a.fs.ReadFile(filepath.Join(msgDir, e.Name()))
		if err != nil {
			continue
		}
		var msg openCodeMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		role := Role(msg.Role)
		if role == RoleUser || role == RoleAssistant {
			turnCount++
		}
		if role == RoleAssistant {
			// Count tool call parts as a proxy for tool calls.
			tc := a.countToolCallParts(storageRoot, msg.ID)
			toolCallCount += tc

			// Accumulate token counts from assistant messages.
			if msg.Tokens != nil {
				tokensIn += msg.Tokens.Input
				tokensOut += msg.Tokens.Output
			}
		}
	}
	return turnCount, toolCallCount, tokensIn, tokensOut
}

// countToolCallParts counts JSON files under {root}/part/{msgID}/ as tool call parts.
// ASSUMPTION: The parts directory contains only tool-call part files. OpenCode's storage
// layout guarantees this — each part file corresponds to a single tool call or tool result.
// If the storage format changes, this should add per-file type checking.
func (a *OpenCodeAdapter) countToolCallParts(storageRoot, msgID string) int {
	return len(listPartFilenames(a.fs, storageRoot, msgID))
}

// findModelID returns the modelID from the first assistant message found.
func (a *OpenCodeAdapter) findModelID(_ context.Context, storageRoot, sessionID string) string {
	msgDir := filepath.Join(storageRoot, defaults.OpenCodeDirMessage.String(), sessionID)
	entries, err := a.fs.ReadDir(msgDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), defaults.ExtJSON.String()) {
			continue
		}
		data, err := a.fs.ReadFile(filepath.Join(msgDir, e.Name()))
		if err != nil {
			continue
		}
		var msg openCodeMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.Role == "assistant" && msg.ModelID != "" {
			return msg.ModelID
		}
	}
	return ""
}

// resolveStorageRoot returns the OpenCode storage root directory for a session.
// Uses OriginalRoot if available (appending "storage/"); falls back to
// deriveStorageRoot for sessions discovered before OriginalRoot was introduced.
//
// OriginalRoot is the provider root (e.g. ~/.local/share/opencode),
// so we must append "storage/" to reach the message/part directories.
// deriveStorageRoot already returns the directory containing message/part/.
func resolveStorageRoot(session DiscoveredSession) string {
	if session.OriginalRoot != "" {
		return filepath.Join(string(session.OriginalRoot), defaults.OpenCodeDirStorage.String())
	}
	return deriveStorageRoot(session.SourcePath.String())
}

// deriveStorageRoot derives the OpenCode storage root from a session file path.
//
// The session file lives at: {root}/storage/session/{hash}/ses_{id}.json
// The storage root (containing session/, message/, part/, project/) is three
// directories up from the file, i.e. {root}/storage/.
func deriveStorageRoot(sesFilePath string) string {
	// ses_{id}.json  → parent is {hash} dir
	// {hash} dir     → parent is session/ dir
	// session/ dir   → parent is storage root ({root}/storage/)
	return filepath.Dir(filepath.Dir(filepath.Dir(sesFilePath)))
}
