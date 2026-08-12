package ingest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/schema"
)

const cursorAgentTranscriptsDir = "agent-transcripts"

// CursorAdapter discovers and extracts metadata from Cursor agent JSONL transcripts.
type CursorAdapter struct {
	fs   FileSystem
	git  GitResolver
	salt salt.Salt
}

var _ SourceAdapter = (*CursorAdapter)(nil)

// NewCursorAdapter creates a CursorAdapter with injected dependencies.
func NewCursorAdapter(fs FileSystem, git GitResolver, s salt.Salt) *CursorAdapter {
	return &CursorAdapter{fs: fs, git: git, salt: s}
}

func (a *CursorAdapter) Harness() Harness {
	return HarnessCursor
}

type cursorJSONLLine struct {
	Role       string          `json:"role"`
	UUID       string          `json:"uuid"`
	ParentUUID string          `json:"parentUuid"`
	Timestamp  json.RawMessage `json:"timestamp"`
	StopReason string          `json:"stop_reason"`
	Message    struct {
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage,omitempty"`
	} `json:"message"`
	Content json.RawMessage `json:"content"`
}

func (line cursorJSONLLine) content() json.RawMessage {
	if len(line.Message.Content) > 0 {
		return line.Message.Content
	}
	return line.Content
}

type cursorContentBlock struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Content   json.RawMessage `json:"content"`
	Input     json.RawMessage `json:"input"`
	IsError   bool            `json:"is_error"`
}

// Discover walks configured Cursor IDE project roots looking for transcripts.
// With the default source root ~/.cursor/projects, Cursor IDE stores transcripts at:
//
//	~/.cursor/projects/{workspace}/agent-transcripts/{sessionId}/{sessionId}.jsonl
//	~/.cursor/projects/{workspace}/agent-transcripts/{parentId}/subagents/{sessionId}.jsonl
func (a *CursorAdapter) Discover(ctx context.Context, cfg SourceConfig) ([]DiscoveredSession, error) {
	var sessions []DiscoveredSession

	for _, basePath := range cfg.Paths {
		root := string(basePath)
		type rootEntry struct {
			path      string
			workspace string
			sessionID string
		}
		type subagentEntry struct {
			path       string
			workspace  string
			parentID   string
			subagentID string
		}
		var rootEntries []rootEntry
		var subagentEntries []subagentEntry

		walkErr := a.fs.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 || d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, defaults.ExtJSONL.String()) {
				return nil
			}

			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			parts := strings.Split(rel, string(filepath.Separator))

			// Root session: {workspace}/agent-transcripts/{uuid}/{uuid}.jsonl.
			if len(parts) == 4 && parts[1] == cursorAgentTranscriptsDir {
				sessionIDStr := parts[2]
				filenameID := strings.TrimSuffix(parts[3], defaults.ExtJSONL.String())
				if sessionIDStr != filenameID {
					return nil
				}
				if _, err := NewSessionID(sessionIDStr); err == nil {
					rootEntries = append(rootEntries, rootEntry{path: path, workspace: parts[0], sessionID: sessionIDStr})
				}
				return nil
			}

			// Subagent session: {workspace}/agent-transcripts/{parent}/subagents/{uuid}.jsonl.
			if len(parts) == 5 && parts[1] == cursorAgentTranscriptsDir && parts[3] == defaults.DirSubagents.String() {
				parentIDStr := parts[2]
				subagentIDStr := strings.TrimSuffix(parts[4], defaults.ExtJSONL.String())
				if _, err := NewSessionID(parentIDStr); err == nil {
					if _, err := NewSessionID(subagentIDStr); err == nil {
						subagentEntries = append(subagentEntries, subagentEntry{
							path:       path,
							workspace:  parts[0],
							parentID:   parentIDStr,
							subagentID: subagentIDStr,
						})
					}
				}
				return nil
			}

			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("cursor discover: walk %q: %w", root, walkErr)
		}

		rootIndex := make(map[string]int, len(rootEntries))
		for _, entry := range rootEntries {
			sid, _ := NewSessionID(entry.sessionID)
			info, err := a.fs.Stat(entry.path)
			if err != nil {
				continue
			}
			sourcePath, err := NewResolvedPath(entry.path)
			if err != nil {
				continue
			}
			projectDir, projectName := a.decodeCursorWorkspace(root, entry.workspace)
			hints := a.extractCursorHints(entry.path)
			rootIndex[entry.sessionID] = len(sessions)
			sessions = append(sessions, DiscoveredSession{
				SessionID:     sid,
				Harness:       HarnessCursor,
				SourcePath:    sourcePath,
				SourceFormat:  SourceFormatJSONL,
				OriginalRoot:  basePath,
				SubagentPaths: []ResolvedPath{},
				DebugPaths:    []ResolvedPath{},
				ModTime:       info.ModTime(),
				ProjectName:   projectName,
				Title:         hints.title,
				CWD:           projectDir,
			})
		}

		for _, entry := range subagentEntries {
			parentSID, _ := NewSessionID(entry.parentID)
			subSID, _ := NewSessionID(entry.subagentID)
			info, err := a.fs.Stat(entry.path)
			if err != nil {
				continue
			}
			rp, err := NewResolvedPath(entry.path)
			if err != nil {
				continue
			}
			if idx, ok := rootIndex[entry.parentID]; ok {
				sessions[idx].SubagentPaths = append(sessions[idx].SubagentPaths, rp)
			}
			projectDir, projectName := a.decodeCursorWorkspace(root, entry.workspace)
			sessions = append(sessions, DiscoveredSession{
				SessionID:     subSID,
				Harness:       HarnessCursor,
				SourcePath:    rp,
				SourceFormat:  SourceFormatJSONL,
				OriginalRoot:  basePath,
				ParentUUID:    &parentSID,
				SubagentPaths: []ResolvedPath{},
				DebugPaths:    []ResolvedPath{},
				ModTime:       info.ModTime(),
				ProjectName:   projectName,
				CWD:           projectDir,
			})
		}
	}

	return sessions, nil
}

type cursorSessionHints struct {
	title string
}

func (a *CursorAdapter) extractCursorHints(path string) cursorSessionHints {
	data, err := a.fs.ReadFile(path)
	if err != nil {
		return cursorSessionHints{}
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	buf := make([]byte, defaults.ScannerInitBuf)
	scanner.Buffer(buf, defaults.ScannerMaxLine)
	for i := 0; i < 10 && scanner.Scan(); i++ {
		var line cursorJSONLLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if cursorLineRole(line) == RoleUser {
			if text := extractCursorText(line.content()); text != "" {
				return cursorSessionHints{title: simpleTitle(text, defaults.HarnessCursor)}
			}
		}
	}
	return cursorSessionHints{}
}

// ExtractMetadata reads the Cursor JSONL transcript and builds UnifiedMetadata.
func (a *CursorAdapter) ExtractMetadata(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, error) {
	data, err := a.fs.ReadFile(string(session.SourcePath))
	if err != nil {
		return nil, fmt.Errorf("cursor: read session %s: %w", session.SessionID, err)
	}

	meta := NewUnifiedMetadata()
	meta.SessionID = session.SessionID
	meta.ParentUUID = session.ParentUUID
	meta.ModelHarness = HarnessCursor
	meta.Source = SourceInfo{
		FilePath: string(session.SourcePath),
		Format:   SourceFormatJSONL,
	}

	var (
		lineNum        int
		firstTimestamp json.RawMessage
		lastTimestamp  json.RawMessage
		turnCount      int
		toolCount      int
		tokensIn       int
		tokensOut      int
	)

	scanner := bufio.NewScanner(bytes.NewReader(data))
	buf := make([]byte, defaults.ScannerInitBuf)
	scanner.Buffer(buf, defaults.ScannerMaxLine)
	for scanner.Scan() {
		lineNum++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var line cursorJSONLLine
		if err := json.Unmarshal(raw, &line); err != nil {
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
				ErrorType:   "parse_error",
				Location:    fmt.Sprintf("line %d", lineNum),
				Message:     fmt.Sprintf("failed to parse Cursor JSONL line: %v", err),
				Remediation: "Check the transcript file for corruption or truncation at the indicated line.",
			})
			continue
		}
		if len(line.Timestamp) > 0 && string(line.Timestamp) != "null" {
			if len(firstTimestamp) == 0 {
				firstTimestamp = append(json.RawMessage(nil), line.Timestamp...)
			}
			lastTimestamp = append(json.RawMessage(nil), line.Timestamp...)
		}
		role := cursorLineRole(line)
		if role == RoleUser || role == RoleAssistant {
			turnCount++
		}
		for _, block := range parseCursorContentBlocks(line.content()) {
			if block.Type == "tool_use" {
				toolCount++
			}
		}
		if meta.Model == "" && line.Message.Model != "" {
			if mid, err := NewModelID(line.Message.Model); err == nil {
				meta.Model = mid
			}
		}
		if line.Message.Usage != nil {
			tokensIn += line.Message.Usage.InputTokens
			tokensOut += line.Message.Usage.OutputTokens
		}
	}
	if err := scanner.Err(); err != nil {
		meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "read_error",
			Location:    fmt.Sprintf("line %d", lineNum),
			Message:     fmt.Sprintf("scanner error reading Cursor transcript: %v", err),
			Remediation: "Verify the transcript file is not corrupted or truncated.",
		})
	}
	if meta.Model == "" {
		meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "missing_model",
			Location:    fmt.Sprintf("session %s", session.SessionID),
			Message:     "Cursor transcript omitted message.model; peasant will hold this session from village push because the village requires a recorded model.",
			Remediation: "Keep the transcript ingested for local analysis, but do not publish it until Cursor records model metadata or the source transcript is corrected.",
		})
	}

	startMs := cursorTimestampMillis(firstTimestamp)
	endMs := cursorTimestampMillis(lastTimestamp)
	if startMs == 0 {
		if !session.CreatedAt.IsZero() {
			startMs = session.CreatedAt.UnixMilli()
		} else if !session.ModTime.IsZero() {
			startMs = session.ModTime.UnixMilli()
		}
	}
	if endMs == 0 && !session.ModTime.IsZero() {
		endMs = session.ModTime.UnixMilli()
	}
	if endMs < startMs {
		endMs = startMs
	}
	ingested := time.Now().UnixMilli()
	meta.Timestamp = TimestampInfo{
		Start:    startMs,
		End:      endMs,
		Ingested: &ingested,
	}
	if startMs > 0 && endMs >= startMs {
		meta.Stats.DurationMs = endMs - startMs
	}
	meta.Stats.TurnCount = turnCount
	meta.Stats.ToolCallCount = toolCount
	meta.Stats.SubagentCount = len(session.SubagentPaths)
	meta.Stats.TokensIn = tokensIn
	meta.Stats.TokensOut = tokensOut
	for _, sp := range session.SubagentPaths {
		subIDStr := strings.TrimSuffix(filepath.Base(string(sp)), defaults.ExtJSONL.String())
		subSID, err := NewSessionID(subIDStr)
		if err != nil {
			continue
		}
		meta.Subagents = append(meta.Subagents, SubagentRef{
			SessionID:  subSID,
			ParentUUID: session.SessionID,
		})
	}

	a.enrichCursorProject(ctx, &meta, session, session.CWD)
	return &meta, nil
}

func cursorTimestampMillis(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	if ts := parseIndexTimestamp(raw); ts != nil {
		return *ts
	}
	return 0
}

func (a *CursorAdapter) enrichCursorProject(ctx context.Context, meta *UnifiedMetadata, session DiscoveredSession, projectDir string) {
	if projectDir == "" {
		return
	}
	if a.git == nil {
		return
	}

	remoteURL, remoteErr := a.git.RemoteURL(ctx, projectDir)
	// Walk up parent directories if direct remote lookup fails — handles cases where
	// projectDir decoded to a subdirectory of the actual repo root.
	if remoteErr != nil || remoteURL == "" {
		if walkedRemote, _, walkErr := a.git.WalkUpRemoteURL(ctx, projectDir); walkErr == nil && walkedRemote != "" {
			remoteURL = walkedRemote
			remoteErr = nil
		}
	}
	branchStr, branchErr := a.git.Branch(ctx, projectDir)
	worktreeStr, worktreeErr := a.git.Worktree(ctx, projectDir)
	trackingStr, trackingErr := a.git.TrackingBranch(ctx, projectDir)

	gitInfo := GitContext{}
	if branchErr == nil && branchStr != "" {
		gitInfo.Branch = &branchStr
	}
	if remoteErr == nil && remoteURL != "" {
		gitInfo.Remote = &remoteURL
	}
	if worktreeErr == nil && worktreeStr != "" {
		gitInfo.Worktree = &worktreeStr
	}
	if trackingErr == nil && trackingStr != "" {
		gitInfo.Tracking = &trackingStr
	}
	meta.Git = gitInfo
	meta.CWD = projectDir

	projectPath := worktreeStr
	if projectPath == "" {
		projectPath = projectDir
	}
	// Prefer the repo name from git remote, then the slug-derived name from discovery,
	// then the filesystem basename as a last resort.
	projectName := RepoNameFromRemote(remoteURL)
	if projectName == "" {
		projectName = session.ProjectName
	}
	if projectName == "" {
		projectName = filepath.Base(projectPath)
	}

	projectHash, hostSlug, err := DeriveProjectIdentifiersWithGit(ctx, a.salt, a.git, remoteURL, projectPath)
	if err != nil {
		meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "derive_identity_error",
			Location:    fmt.Sprintf("session %s", session.SessionID),
			Message:     fmt.Sprintf("failed to derive project identifiers: %v", err),
			Remediation: "Check git remote URL or working directory path.",
		})
		meta.Project = ProjectInfo{FilePath: projectPath, Name: projectName}
		return
	}
	meta.HostSlug = hostSlug
	meta.Project = ProjectInfo{
		Hash:     projectHash,
		FilePath: projectPath,
		Name:     projectName,
	}
}

// decodeCursorWorkspace resolves a Cursor workspace name to the real project directory
// and a best-effort project name hint. Cursor encodes path separators, underscores,
// and spaces all as dashes (e.g. "/Users/foo/My_Proj" → "Users-foo-My-Proj").
//
// Cursor workspace folder names omit the leading "-" that Claude slugs include, so
// this prepends "-" before calling DecodeCursorSlug.
//
// Returns (projectDir, projectName):
//   - projectDir: the matched filesystem path (or the raw workspace path as fallback)
//   - projectName: repo/directory name for use when git resolution fails
func (a *CursorAdapter) decodeCursorWorkspace(root, workspace string) (projectDir, projectName string) {
	encoded := "-" + workspace
	matched, unmatched := DecodeCursorSlug(encoded, a.dirExists)
	if matched == "" {
		return filepath.Join(root, workspace), workspace
	}
	if unmatched != "" {
		// Partial decode: unmatched trailing segments are the project name
		// (e.g. workspace "Users-foo-Desktop-peasant" → matched "/Users/foo/Desktop", unmatched "peasant")
		return matched, unmatched
	}
	return matched, filepath.Base(matched)
}

// cursorSegmentVariants returns the candidate directory names to try for a given
// dash-encoded segment. Cursor encodes underscores and spaces as dashes, so each
// segment is tried as-is, with dashes replaced by underscores, and with dashes
// replaced by spaces (deduped when replacements produce no change).
func cursorSegmentVariants(segment string) []string {
	seen := make(map[string]bool, 3)
	variants := make([]string, 0, 3)
	for _, v := range []string{
		segment,
		strings.ReplaceAll(segment, "-", "_"),
		strings.ReplaceAll(segment, "-", " "),
	} {
		if !seen[v] {
			seen[v] = true
			variants = append(variants, v)
		}
	}
	return variants
}

// dirExists checks if path exists as a directory using the adapter's FileSystem.
func (a *CursorAdapter) dirExists(path string) bool {
	info, err := a.fs.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// CursorIndexer parses Cursor JSONL transcripts into SessionEntry slices.
type CursorIndexer struct {
	fs        FileSystem
	fullDepth bool
}

var _ TranscriptIndexer = (*CursorIndexer)(nil)

// SourceKind reports that Cursor's entries come from a single JSONL file; every entry is in its bytes.
func (idx *CursorIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }

// CursorIndexerOption configures a CursorIndexer.
type CursorIndexerOption func(*CursorIndexer)

// WithCursorFullDepth enables or disables full-depth content block decomposition.
func WithCursorFullDepth(enabled bool) CursorIndexerOption {
	return func(idx *CursorIndexer) {
		idx.fullDepth = enabled
	}
}

// NewCursorIndexer creates a CursorIndexer for Cursor JSONL transcripts.
func NewCursorIndexer(fs FileSystem, opts ...CursorIndexerOption) *CursorIndexer {
	idx := &CursorIndexer{fs: fs}
	for _, opt := range opts {
		opt(idx)
	}
	return idx
}

func (idx *CursorIndexer) IndexTranscript(_ context.Context, session DiscoveredSession) ([]schema.SessionEntry, error) {
	data, err := idx.fs.ReadFile(string(session.SourcePath))
	if err != nil {
		return nil, fmt.Errorf("cursor indexer: read %s: %w", session.SourcePath, err)
	}
	return idx.parseJSONL(session.SessionID, data)
}

func (idx *CursorIndexer) IndexTranscriptBytes(_ context.Context, session DiscoveredSession, data []byte) ([]schema.SessionEntry, error) {
	return idx.parseJSONL(session.SessionID, data)
}

func (idx *CursorIndexer) parseJSONL(sessionID SessionID, data []byte) ([]schema.SessionEntry, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	buf := make([]byte, defaults.ScannerInitBuf)
	scanner.Buffer(buf, defaults.ScannerMaxLine)

	var entries []schema.SessionEntry
	entryIndex := 0
	for scanner.Scan() {
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		entry, ok := parseCursorLine(sessionID, entryIndex, raw)
		if !ok {
			entryIndex++
			continue
		}
		parentIndex := entryIndex
		entries = append(entries, entry)
		entryIndex++
		if idx.fullDepth {
			childEntries := decomposeCursorContentBlocks(sessionID, &entryIndex, parentIndex, raw)
			if entry.Role == RoleSystem {
				for i := range childEntries {
					childEntries[i].Role = RoleSystem
					childEntries[i].EntryType = EntryTypeSystem
				}
			}
			entries = append(entries, childEntries...)
		}
	}
	if err := scanner.Err(); err != nil {
		return entries, fmt.Errorf("cursor indexer: scanner error for %s: %w", sessionID, err)
	}
	return entries, nil
}

func parseCursorLine(sessionID SessionID, index int, raw []byte) (schema.SessionEntry, bool) {
	var line cursorJSONLLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return schema.SessionEntry{}, false
	}
	role := cursorLineRole(line)
	entry := schema.SessionEntry{
		SessionID:  sessionID,
		EntryIndex: index,
		Harness:    HarnessCursor,
		Role:       role,
		EntryType:  classifyCursorEntry(role),
	}
	if ts := parseIndexTimestamp(line.Timestamp); ts != nil {
		entry.TimestampMs = ts
	}
	rawLen := len(raw)
	entry.RawByteLength = &rawLen
	if line.UUID != "" {
		entry.EntryID = &line.UUID
	}
	if line.ParentUUID != "" {
		entry.ParentEntryID = &line.ParentUUID
	}

	blocks := parseCursorContentBlocks(line.content())
	plainText := extractCursorPlainText(line.content())
	var toolNames []string
	var preview strings.Builder
	var thinkingFallback string
	if plainText != "" {
		preview.WriteString(stripCursorQueryTag(plainText))
	}
	for _, block := range blocks {
		switch block.Type {
		case "tool_use":
			entry.HasToolUse = true
			if block.Name != "" {
				toolNames = append(toolNames, block.Name)
			}
		case "thinking":
			entry.HasThinking = true
			if thinkingFallback == "" {
				thinkingFallback = firstNonEmpty(block.Thinking, block.Text)
			}
		case "text":
			if preview.Len() == 0 && block.Text != "" {
				preview.WriteString(stripCursorQueryTag(block.Text))
			}
		case "tool_result":
			if block.IsError {
				entry.IsError = true
			}
			if preview.Len() == 0 {
				preview.WriteString(rawMessagePreview(block.Content))
			}
		}
	}
	if preview.Len() == 0 && thinkingFallback != "" {
		preview.WriteString(thinkingFallback)
	}
	if len(toolNames) > 0 {
		csv := strings.Join(toolNames, ",")
		entry.ToolNamesCSV = &csv
	}
	textToCheck := cursorSystemCheckText(plainText, blocks)
	if entry.Role == RoleUser && textToCheck != "" {
		if isSystemInjectedContent(textToCheck) {
			entry.Role = RoleSystem
			entry.EntryType = EntryTypeSystem
		} else if cmdName, cmdArgs, ok := parseSkillInvocation(textToCheck); ok {
			extra := `{"command_name":"` + jsonEscapeString(cmdName) + `"`
			if cmdArgs != "" {
				extra += `,"command_args":"` + jsonEscapeString(cmdArgs) + `"`
			}
			extra += `}`
			entry.Extra = &extra
		}
	}
	if entry.HasToolUse && entry.Role == RoleAssistant {
		entry.EntryType = EntryTypeToolUse
	}
	if entry.HasThinking && entry.EntryType == EntryTypeText && entry.Role == RoleAssistant {
		entry.EntryType = EntryTypeThinking
	}
	if entry.IsError && entry.Role == RoleTool {
		entry.EntryType = EntryTypeToolResult
	}
	if preview.Len() > 0 {
		p := truncateString(preview.String(), defaults.ContentPreviewLimit)
		entry.ContentPreview = &p
	}
	if line.Message.Usage != nil {
		if line.Message.Usage.InputTokens > 0 {
			v := line.Message.Usage.InputTokens
			entry.TokensIn = &v
		}
		if line.Message.Usage.OutputTokens > 0 {
			v := line.Message.Usage.OutputTokens
			entry.TokensOut = &v
		}
	}
	if line.StopReason != "" {
		sr := schema.StopReason(line.StopReason)
		if sr.IsValid() {
			entry.StopReason = &sr
		}
	}
	return entry, true
}

func decomposeCursorContentBlocks(sessionID SessionID, entryIndex *int, parentIndex int, raw []byte) []schema.SessionEntry {
	var line cursorJSONLLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return nil
	}
	blocks := parseCursorContentBlocks(line.content())
	if len(blocks) == 0 {
		return nil
	}
	role := cursorLineRole(line)
	var entries []schema.SessionEntry
	for _, block := range blocks {
		entry := schema.SessionEntry{
			SessionID:   sessionID,
			EntryIndex:  *entryIndex,
			Harness:     HarnessCursor,
			Role:        role,
			Depth:       1,
			ParentIndex: &parentIndex,
		}
		pt := block.Type
		entry.PartType = &pt
		switch block.Type {
		case "text":
			entry.EntryType = EntryTypeText
			if block.Text != "" {
				cleaned := stripCursorQueryTag(block.Text)
				p := truncateString(cleaned, defaults.ContentPreviewLimit)
				entry.ContentPreview = &p
				if isSystemInjectedContent(block.Text) {
					entry.Role = RoleSystem
					entry.EntryType = EntryTypeSystem
				}
			}
		case "tool_use":
			entry.EntryType = EntryTypeToolUse
			entry.HasToolUse = true
			if block.Name != "" {
				entry.ToolNamesCSV = &block.Name
				if kind := classifyCursorToolKind(block.Name); kind != "" {
					entry.ToolKind = &kind
				}
			}
			if block.ID != "" {
				entry.ToolCallID = &block.ID
			}
			if len(block.Input) > 0 && string(block.Input) != "null" {
				s := string(block.Input)
				entry.ToolInput = &s
			}
		case "tool_result":
			entry.EntryType = EntryTypeToolResult
			entry.IsError = block.IsError
			if block.ToolUseID != "" {
				entry.ToolCallID = &block.ToolUseID
			}
			if output := rawMessagePreview(block.Content); output != "" {
				entry.ToolOutput = &output
			}
		case "thinking":
			entry.EntryType = EntryTypeThinking
			entry.HasThinking = true
			text := firstNonEmpty(block.Thinking, block.Text)
			if text != "" {
				p := truncateString(text, defaults.ContentPreviewLimit)
				entry.ContentPreview = &p
			}
			thinkLen := len(text)
			entry.RawByteLength = &thinkLen
		default:
			entry.EntryType = EntryTypeText
		}
		entries = append(entries, entry)
		*entryIndex++
	}
	return entries
}

func cursorLineRole(line cursorJSONLLine) Role {
	raw := line.Role
	if raw == "" {
		raw = line.Message.Role
	}
	role := Role(raw)
	if role.IsValid() {
		return role
	}
	switch raw {
	case "human":
		return RoleUser
	default:
		return RoleSystem
	}
}

func classifyCursorEntry(role Role) EntryType {
	switch role {
	case RoleUser, RoleAssistant:
		return EntryTypeText
	case RoleTool:
		return EntryTypeToolResult
	case RoleSystem:
		return EntryTypeSystem
	default:
		return EntryTypeText
	}
}

func parseCursorContentBlocks(raw json.RawMessage) []cursorContentBlock {
	if len(raw) == 0 {
		return nil
	}
	var blocks []cursorContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks
	}
	return nil
}

func extractCursorText(raw json.RawMessage) string {
	var candidate string
	text := extractCursorPlainText(raw)
	if text != "" {
		candidate = stripCursorQueryTag(strings.TrimSpace(text))
	} else {
		for _, block := range parseCursorContentBlocks(raw) {
			if block.Type == "text" && block.Text != "" {
				candidate = stripCursorQueryTag(strings.TrimSpace(block.Text))
				break
			}
		}
	}
	if candidate == "" {
		return ""
	}
	// Use only the first non-empty line so the picker shows a tidy one-liner.
	for _, line := range strings.Split(candidate, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			if len(line) > 80 {
				line = line[:77] + "..."
			}
			return line
		}
	}
	return ""
}

func extractCursorPlainText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

func cursorSystemCheckText(plainText string, blocks []cursorContentBlock) string {
	if plainText != "" {
		return plainText
	}
	if len(blocks) == 0 {
		return ""
	}
	for _, block := range blocks {
		if block.Type != "text" {
			return ""
		}
	}
	for _, block := range blocks {
		if block.Text != "" {
			return block.Text
		}
	}
	return ""
}

func rawMessagePreview(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
