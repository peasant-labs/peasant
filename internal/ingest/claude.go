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
)

// ClaudeAdapter discovers and extracts metadata from Claude Code JSONL transcripts.
type ClaudeAdapter struct {
	fs   FileSystem
	git  GitResolver
	salt salt.Salt
}

var _ SourceAdapter = (*ClaudeAdapter)(nil)

// NewClaudeAdapter creates a ClaudeAdapter with injected dependencies.
func NewClaudeAdapter(fs FileSystem, git GitResolver, s salt.Salt) *ClaudeAdapter {
	return &ClaudeAdapter{fs: fs, git: git, salt: s}
}

func (a *ClaudeAdapter) Harness() Harness {
	return HarnessClaudeCode
}

// claudeJSONLLine is the raw shape of each line in a Claude Code JSONL transcript.
// Fields are parsed only as-needed; unmapped fields are silently ignored.
type claudeJSONLLine struct {
	SessionID string `json:"sessionId"`
	Version   string `json:"version"`
	Type      string `json:"type"` // "user" or "assistant"
	CWD       string `json:"cwd"`
	GitBranch string `json:"gitBranch"`
	AgentID   string `json:"agentId"`
	UUID      string `json:"uuid"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"` // string or []contentBlock
		Usage   *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage,omitempty"`
	} `json:"message"`
}

// claudeTeammateIdentity is deliberately private to discovery. Claude's team
// metadata is useful for relating separately stored transcripts, but is not a
// public session contract.
type claudeTeammateIdentity struct {
	team string
	name string
}

// contentBlock is a typed block inside an assistant message content array.
type contentBlock struct {
	Type string `json:"type"`
}

// Discover walks each configured source path looking for *.jsonl files.
// It identifies root sessions (UUID.jsonl at the project slug level) and links
// subagent sessions found under {uuid}/subagents/agent-*.jsonl.
//
// Uses a two-pass approach: first collects all file entries, then processes
// roots before subagents. This ensures parent sessions are registered before
// subagent linking, regardless of filesystem walk order.
//
// Per RFC Section 6.4, symlinks are skipped silently.
func (a *ClaudeAdapter) Discover(ctx context.Context, cfg SourceConfig) ([]DiscoveredSession, error) {
	var sessions []DiscoveredSession

	for _, basePath := range cfg.Paths {
		root := string(basePath)

		// Two-pass approach to handle walk order differences between
		// os.WalkDir (DFS: dirs before sibling files) and MemFS (flat sorted).

		type rootEntry struct {
			path      string
			sessionID string
		}
		type subagentEntry struct {
			path          string
			subagentID    string
			parentUUIDStr string
		}

		var rootEntries []rootEntry
		var subagentEntries []subagentEntry

		// Pass 1: Collection — walk and categorize files.
		walkErr := a.fs.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			if err != nil {
				return nil // skip unreadable entries
			}

			// Skip symlinks per RFC Section 6.4.
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}

			if d.IsDir() {
				return nil
			}

			// Only process .jsonl files.
			if !strings.HasSuffix(path, defaults.ExtJSONL.String()) {
				return nil
			}

			// Determine the session type by relative path structure.
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}

			parts := strings.Split(rel, string(filepath.Separator))

			// Root session: {project-slug}/{uuid}.jsonl (depth 2)
			if len(parts) == 2 {
				sessionIDStr := strings.TrimSuffix(parts[1], defaults.ExtJSONL.String())
				if _, err := NewSessionID(sessionIDStr); err == nil {
					rootEntries = append(rootEntries, rootEntry{path: path, sessionID: sessionIDStr})
				}
				return nil
			}

			// Subagent session: {project-slug}/{uuid}/subagents/agent-{hex}.jsonl (depth 4)
			if len(parts) == 4 && parts[2] == defaults.DirSubagents.String() && strings.HasPrefix(parts[3], defaults.ClaudeSubagentPrefix) {
				parentUUIDStr := parts[1]
				subagentIDStr := strings.TrimSuffix(parts[3], defaults.ExtJSONL.String())
				if _, err := NewSessionID(parentUUIDStr); err == nil {
					if _, err := NewSessionID(subagentIDStr); err == nil {
						subagentEntries = append(subagentEntries, subagentEntry{
							path:          path,
							subagentID:    subagentIDStr,
							parentUUIDStr: parentUUIDStr,
						})
					}
				}
				return nil
			}

			return nil
		})

		if walkErr != nil {
			return nil, fmt.Errorf("Discover: walk %q: %w", root, walkErr)
		}

		// Pass 2: Process roots first, then subagents.
		rootIndex := make(map[string]int)

		for _, entry := range rootEntries {
			sid, _ := NewSessionID(entry.sessionID) // already validated in pass 1

			info, err := a.fs.Stat(entry.path)
			if err != nil {
				continue
			}

			rp := ResolvedPath(entry.path)
			hints := a.extractClaudeHints(entry.path)
			ds := DiscoveredSession{
				SessionID:     sid,
				Harness:       HarnessClaudeCode,
				SourcePath:    rp,
				SourceFormat:  SourceFormatJSONL,
				OriginalRoot:  basePath,
				SubagentPaths: []ResolvedPath{},
				DebugPaths:    []ResolvedPath{},
				ModTime:       info.ModTime(),
				Title:         hints.title,
				Branch:        hints.branch,
				CWD:           hints.cwd,
			}
			rootIndex[entry.sessionID] = len(sessions)
			sessions = append(sessions, ds)
		}

		for _, entry := range subagentEntries {
			parentSID, _ := NewSessionID(entry.parentUUIDStr) // already validated
			subSID, _ := NewSessionID(entry.subagentID)       // already validated

			info, err := a.fs.Stat(entry.path)
			if err != nil {
				continue
			}

			rp := ResolvedPath(entry.path)

			// Link to parent's SubagentPaths — guaranteed parent is already registered.
			if idx, ok := rootIndex[entry.parentUUIDStr]; ok {
				sessions[idx].SubagentPaths = append(sessions[idx].SubagentPaths, rp)
			}

			ds := DiscoveredSession{
				SessionID:     subSID,
				Harness:       HarnessClaudeCode,
				SourcePath:    rp,
				SourceFormat:  SourceFormatJSONL,
				OriginalRoot:  basePath,
				ParentUUID:    &parentSID,
				SubagentPaths: []ResolvedPath{},
				DebugPaths:    []ResolvedPath{},
				ModTime:       info.ModTime(),
			}
			sessions = append(sessions, ds)
		}
	}

	a.linkClaudeTeammates(sessions)
	return sessions, nil
}

// linkClaudeTeammates links independently persisted Claude root transcripts.
// A relationship is accepted only when both sides provide one unambiguous
// complete identity. Files that cannot be read or parsed simply provide no
// evidence and do not make discovery fail.
func (a *ClaudeAdapter) linkClaudeTeammates(sessions []DiscoveredSession) {
	rootByPath := make(map[ResolvedPath]int)
	for i := range sessions {
		if sessions[i].ParentUUID == nil {
			rootByPath[sessions[i].SourcePath] = i
		}
	}

	identities := make(map[claudeTeammateIdentity][]int)
	spawns := make(map[claudeTeammateIdentity]map[int]struct{})
	for i := range sessions {
		if _, ok := rootByPath[sessions[i].SourcePath]; !ok {
			continue
		}
		identity, spawnRecords := a.readClaudeTeammateEvidence(sessions[i].SourcePath)
		if identity != nil {
			identities[*identity] = append(identities[*identity], i)
		}
		for _, spawn := range spawnRecords {
			if spawns[spawn] == nil {
				spawns[spawn] = make(map[int]struct{})
			}
			spawns[spawn][i] = struct{}{}
		}
	}

	for identity, children := range identities {
		parents := spawns[identity]
		if len(children) != 1 || len(parents) != 1 {
			continue
		}
		child := children[0]
		var parent int
		for parent = range parents {
		}
		if child == parent {
			continue
		}
		parentID := sessions[parent].SessionID
		sessions[child].ParentUUID = &parentID
		sessions[parent].SubagentPaths = append(sessions[parent].SubagentPaths, sessions[child].SourcePath)
	}
}

func (a *ClaudeAdapter) readClaudeTeammateEvidence(path ResolvedPath) (*claudeTeammateIdentity, []claudeTeammateIdentity) {
	data, err := a.fs.ReadFile(path.String())
	if err != nil {
		return nil, nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, defaults.ScannerInitBuf), defaults.ScannerMaxLine)
	var identity *claudeTeammateIdentity
	var invalidIdentity bool
	var spawns []claudeTeammateIdentity
	for scanner.Scan() {
		var value map[string]any
		if json.Unmarshal(scanner.Bytes(), &value) != nil {
			continue
		}
		_, hasTeam := value["teamName"]
		_, hasName := value["agentName"]
		if hasTeam || hasName {
			team, teamOK := value["teamName"].(string)
			name, nameOK := value["agentName"].(string)
			if !teamOK || !nameOK || team == "" || name == "" {
				invalidIdentity = true
			} else {
				candidate := claudeTeammateIdentity{team: team, name: name}
				if identity == nil {
					identity = &candidate
				} else if *identity != candidate {
					invalidIdentity = true
				}
			}
		}
		if spawn, ok := claudeTeammateSpawn(value); ok {
			spawns = append(spawns, spawn)
		}
	}
	if invalidIdentity {
		identity = nil
	}
	return identity, spawns
}

// claudeTeammateSpawn recognizes only the top-level native tool result. Tool
// output may contain arbitrary JSON-shaped text, so recursive matching would
// let an unrelated tool payload forge discovery evidence.
func claudeTeammateSpawn(value map[string]any) (claudeTeammateIdentity, bool) {
	result, ok := value["toolUseResult"].(map[string]any)
	if !ok {
		return claudeTeammateIdentity{}, false
	}
	if status, _ := result["status"].(string); status != "teammate_spawned" {
		return claudeTeammateIdentity{}, false
	}
	team, teamOK := result["team_name"].(string)
	name, nameOK := result["name"].(string)
	if !teamOK || !nameOK || team == "" || name == "" {
		return claudeTeammateIdentity{}, false
	}
	return claudeTeammateIdentity{team: team, name: name}, true
}

// claudeSessionHints holds metadata extracted from the first few JSONL lines.
type claudeSessionHints struct {
	title  string
	branch string
	cwd    string
}

// extractClaudeHints reads the first few lines of a JSONL file to extract
// the session title, git branch, and working directory.
// Best-effort: returns zero values on any error.
func (a *ClaudeAdapter) extractClaudeHints(path string) claudeSessionHints {
	data, err := a.fs.ReadFile(path)
	if err != nil {
		return claudeSessionHints{}
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	buf := make([]byte, defaults.ScannerInitBuf)
	scanner.Buffer(buf, defaults.ScannerMaxLine)

	var hints claudeSessionHints

	// Scan at most 10 lines to avoid reading entire large files.
	for i := 0; i < 10 && scanner.Scan(); i++ {
		var line claudeJSONLLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		// Grab branch and CWD from the first line that has them.
		if hints.branch == "" && line.GitBranch != "" {
			hints.branch = line.GitBranch
		}
		if hints.cwd == "" && line.CWD != "" {
			hints.cwd = line.CWD
		}
		// Derive the display title from the first user message. The shared
		// redaction-free pipeline strips Claude's own markup (system-reminder
		// blocks, command/query wrappers) and caps the length, so the raw markup
		// no longer leaks into the title.
		if hints.title == "" && line.Type == "user" && line.Message.Role == "user" {
			if text := extractTextFromContent(line.Message.Content); text != "" {
				hints.title = simpleTitle(text, defaults.HarnessClaudeCode)
			}
		}
		// Stop early if we have everything.
		if hints.title != "" && hints.branch != "" && hints.cwd != "" {
			break
		}
	}
	return hints
}

// extractTextFromContent extracts plain text from a Claude message content field.
// Content can be a JSON string or an array of content blocks.
func extractTextFromContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	// Try array of content blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return strings.TrimSpace(b.Text)
			}
		}
	}
	return ""
}

// ExtractMetadata reads the JSONL transcript and builds UnifiedMetadata.
func (a *ClaudeAdapter) ExtractMetadata(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, error) {
	data, err := a.fs.ReadFile(string(session.SourcePath))
	if err != nil {
		return nil, fmt.Errorf("ExtractMetadata: read %q: %w", session.SourcePath, err)
	}

	meta := NewUnifiedMetadata()
	meta.SessionID = session.SessionID
	meta.ParentUUID = session.ParentUUID
	meta.ModelHarness = HarnessClaudeCode
	meta.Source = SourceInfo{
		FilePath: string(session.SourcePath),
		Format:   SourceFormatJSONL,
	}

	var (
		firstLine      claudeJSONLLine
		hasFirst       bool
		firstTimestamp string // earliest non-empty timestamp across all lines
		lastTimestamp  string // latest non-empty timestamp across all lines
		lineNum        int
		turnCount      int
		toolCount      int
		tokensIn       int
		tokensOut      int
	)

	scanner := bufio.NewScanner(bytes.NewReader(data))
	// Set scanner buffer to handle large lines. While the entire file is already
	// in memory via ReadFile(), bufio.Scanner has an internal line-length limit
	// that defaults to 64KiB. The 10 MiB limit prevents token scan errors on
	// assistant messages with very large tool outputs.
	buf := make([]byte, defaults.ScannerInitBuf)
	scanner.Buffer(buf, defaults.ScannerMaxLine)

	for scanner.Scan() {
		lineNum++
		raw := scanner.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}

		var line claudeJSONLLine
		if err := json.Unmarshal(raw, &line); err != nil {
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
				ErrorType:   "parse_error",
				Location:    fmt.Sprintf("line %d", lineNum),
				Message:     fmt.Sprintf("failed to parse JSONL line: %v", err),
				Remediation: "Check the transcript file for corruption or truncation at the indicated line.",
			})
			continue
		}

		if !hasFirst {
			firstLine = line
			hasFirst = true
		}

		// Track earliest and latest non-empty timestamps across all lines.
		if line.Timestamp != "" {
			if firstTimestamp == "" {
				firstTimestamp = line.Timestamp
			}
			lastTimestamp = line.Timestamp
		}

		// Count turns.
		if line.Type == "user" || line.Type == "assistant" {
			turnCount++
		}

		// Count tool calls and accumulate tokens from assistant messages.
		if line.Type == "assistant" && len(line.Message.Content) > 0 {
			// content may be a string or an array of blocks.
			// Only count tool_use blocks from arrays.
			var blocks []contentBlock
			if err := json.Unmarshal(line.Message.Content, &blocks); err == nil {
				for _, b := range blocks {
					if b.Type == "tool_use" {
						toolCount++
					}
				}
			}

			// Capture model from first assistant message seen.
			if line.Message.Model != "" && meta.Model == "" {
				mid, merr := NewModelID(line.Message.Model)
				if merr == nil {
					meta.Model = mid
				}
			}

			// Accumulate token usage from assistant messages.
			// input_tokens in Claude's JSONL is only the non-cached portion;
			// the bulk of input tokens are in cache_creation and cache_read fields.
			if line.Message.Usage != nil {
				tokensIn += line.Message.Usage.InputTokens +
					line.Message.Usage.CacheCreationInputTokens +
					line.Message.Usage.CacheReadInputTokens
				tokensOut += line.Message.Usage.OutputTokens
			}
		}
	}

	if err := scanner.Err(); err != nil {
		meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "read_error",
			Location:    fmt.Sprintf("line %d", lineNum),
			Message:     fmt.Sprintf("scanner error reading transcript: %v", err),
			Remediation: "Verify the transcript file is not corrupted or truncated.",
		})
	}

	// Populate fields from parsed lines.
	if hasFirst {
		meta.Version = firstLine.Version

		// Timestamps: use earliest/latest non-empty timestamps found across
		// all lines, not just the first/last line positions. Some sessions
		// have a first line (e.g. init/system) without a timestamp field.
		startMs := parseTimestampMillis(firstTimestamp)
		endMs := parseTimestampMillis(lastTimestamp)
		ingested := time.Now().UnixMilli()
		meta.Timestamp = TimestampInfo{
			Start:    startMs,
			End:      endMs,
			Ingested: &ingested,
		}

		// Duration.
		if startMs > 0 && endMs >= startMs {
			meta.Stats.DurationMs = endMs - startMs
		}

		// Resolve git metadata using the cwd from the first line.
		cwd := firstLine.CWD
		if cwd == "" {
			cwd = filepath.Dir(string(session.SourcePath))
		}

		// If cwd is under ~/.claude/projects/, try to decode the actual project path.
		// Claude project dirs encode the real path by replacing "/" with "-".
		if decoded := a.decodeClaudeProjectDir(cwd); decoded != "" {
			cwd = decoded
		}

		remoteURL, remoteErr := a.git.RemoteURL(ctx, cwd)
		// If direct remote check fails, walk up parent directories to find one.
		// This ensures sessions from decoded slug paths (which may point to a
		// subdirectory) still resolve the correct git remote for project grouping.
		if (remoteErr != nil || remoteURL == "") && a.git != nil {
			if walkedRemote, _, walkErr := a.git.WalkUpRemoteURL(ctx, cwd); walkErr == nil && walkedRemote != "" {
				remoteURL = walkedRemote
				remoteErr = nil
			}
		}
		branchStr, branchErr := a.git.Branch(ctx, cwd)
		worktreeStr, worktreeErr := a.git.Worktree(ctx, cwd)
		trackingStr, trackingErr := a.git.TrackingBranch(ctx, cwd)

		// Build GitContext — all fields are nullable.
		gitInfo := GitContext{}

		// Prefer gitBranch from JSONL over resolved branch.
		branch := firstLine.GitBranch
		if branch == "" && branchErr == nil && branchStr != "" {
			branch = branchStr
		}
		if branch != "" {
			b := branch
			gitInfo.Branch = &b
		}

		if remoteErr == nil && remoteURL != "" {
			r := remoteURL
			gitInfo.Remote = &r
		}

		if worktreeErr == nil && worktreeStr != "" {
			w := worktreeStr
			gitInfo.Worktree = &w
		}

		if trackingErr == nil && trackingStr != "" {
			tr := trackingStr
			gitInfo.Tracking = &tr
		}

		meta.Git = gitInfo

		// Store the real working directory for context-aware slug redaction.
		meta.CWD = cwd

		// ProjectInfo.
		projectPath := worktreeStr
		if projectPath == "" {
			projectPath = cwd
		}

		projectHash, hostSlug, err := DeriveProjectIdentifiersWithGit(ctx, a.salt, a.git, remoteURL, projectPath)
		if err != nil {
			// DeriveProjectIdentifiersWithGit should not fail for valid paths,
			// but fall back to zero-value hash if it does.
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
				ErrorType:   "derive_identity_error",
				Location:    fmt.Sprintf("session %s", session.SessionID),
				Message:     fmt.Sprintf("failed to derive project identifiers: %v", err),
				Remediation: "Check git remote URL or working directory path.",
			})
		} else {
			meta.HostSlug = hostSlug
		}

		// Derive project name from git remote (repo name) when available,
		// so that worktrees and subdirectories show the repository name
		// (e.g., "widget-service") rather than the worktree/branch
		// name (e.g., "feature-push").
		projectName := RepoNameFromRemote(remoteURL)
		if projectName == "" {
			projectName = filepath.Base(projectPath)
		}

		meta.Project = ProjectInfo{
			Hash:     projectHash,
			FilePath: projectPath,
			Name:     projectName,
		}
	}

	meta.Stats.TurnCount = turnCount
	meta.Stats.ToolCallCount = toolCount
	meta.Stats.SubagentCount = len(session.SubagentPaths)
	meta.Stats.TokensIn = tokensIn
	meta.Stats.TokensOut = tokensOut

	// Build subagent refs from SubagentPaths.
	for _, sp := range session.SubagentPaths {
		filename := filepath.Base(string(sp))
		subIDStr := strings.TrimSuffix(filename, defaults.ExtJSONL.String())
		subSID, err := NewSessionID(subIDStr)
		if err != nil {
			continue
		}
		meta.Subagents = append(meta.Subagents, SubagentRef{
			SessionID:  subSID,
			ParentUUID: session.SessionID,
		})
	}

	return &meta, nil
}

// claudeProjectsDirSegment is the path segment that identifies a directory as a
// Claude project memory directory. Used to detect when the CWD fallback points
// at a Claude internal directory rather than the actual project.
const claudeProjectsDirSegment = "/.claude/projects/"

// decodeClaudeProjectDir checks if cwd is under a Claude project memory directory
// and attempts to decode the encoded directory name back to the real filesystem path.
//
// Claude encodes project paths by replacing "/" with "-" and prepending "-":
//
//	/home/user/dev/my-project → -home-user-dev-my-project
//
// The encoding is ambiguous (dashes in directory names vs path separators), so this
// method uses a greedy filesystem-based decoder: starting from root, it tries each
// segment and checks if a directory exists. If not, it merges segments with dashes.
//
// Returns the decoded path if it exists on the filesystem, or empty string if
// decoding fails or the path does not exist.
func (a *ClaudeAdapter) decodeClaudeProjectDir(cwd string) string {
	idx := strings.Index(cwd, claudeProjectsDirSegment)
	if idx < 0 {
		return ""
	}

	// Extract the portion after "/.claude/projects/".
	// cwd may be the project dir itself or a parent of the JSONL file.
	after := cwd[idx+len(claudeProjectsDirSegment):]
	if after == "" {
		return ""
	}

	// The basename may have a trailing slash or additional path components
	// (e.g., if cwd is the dir containing the JSONL). Take only the first
	// path component after "/.claude/projects/".
	encoded := strings.SplitN(after, "/", 2)[0]

	return DecodeClaudeSlug(encoded, a.dirExists)
}

// dirExists checks if path exists as a directory using the adapter's FileSystem.
func (a *ClaudeAdapter) dirExists(path string) bool {
	info, err := a.fs.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// parseTimestampMillis parses an ISO 8601 / RFC 3339 timestamp and returns Unix milliseconds.
// Returns 0 on parse failure.
func parseTimestampMillis(ts string) int64 {
	if ts == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		// Try RFC3339Nano as fallback.
		t, err = time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return 0
		}
	}
	return t.UnixMilli()
}
