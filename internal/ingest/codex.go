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

// CodexAdapter discovers and extracts metadata from OpenAI Codex CLI JSONL rollouts.
//
// On-disk layout (Codex CLI ≥ 0.133):
//
//	{root}/YYYY/MM/DD/rollout-{ISO-timestamp}-{uuid}.jsonl
//
// Each line is {"timestamp", "type", "payload"}. The first line is always
// "session_meta" and carries id, cwd, model_provider, and an optional git
// context. Subsequent lines mix "turn_context" (model + effort per turn),
// "response_item" (the OpenAI Responses API stream), and "event_msg" (UI
// events including the periodic "token_count" totals).
type CodexAdapter struct {
	fs   FileSystem
	git  GitResolver
	salt salt.Salt
}

var _ SourceAdapter = (*CodexAdapter)(nil)

// NewCodexAdapter creates a CodexAdapter with injected dependencies.
func NewCodexAdapter(fs FileSystem, git GitResolver, s salt.Salt) *CodexAdapter {
	return &CodexAdapter{fs: fs, git: git, salt: s}
}

// Harness returns HarnessCodex.
func (a *CodexAdapter) Harness() Harness {
	return HarnessCodex
}

// codexRolloutFilePrefix identifies rollout files in the Codex sessions tree.
const codexRolloutFilePrefix = "rollout-"

// Discover walks each configured source path looking for rollout-*.jsonl files
// nested four segments deep (YYYY/MM/DD/file). Symlinks and other files are
// silently skipped. The session ID is the trailing UUID embedded in the
// filename; ExtractMetadata cross-checks it against the session_meta payload.
//
// For each rollout we also populate DiscoveredSession.Title/ProjectName/Branch/
// CWD so the FTUE wizard and `peasant ingest --verbose` can render meaningful
// rows. Titles come from sibling session_index.jsonl when present; project,
// branch, and cwd come from the rollout's first line (session_meta).
func (a *CodexAdapter) Discover(ctx context.Context, cfg SourceConfig) ([]DiscoveredSession, error) {
	var sessions []DiscoveredSession

	for _, basePath := range cfg.Paths {
		root := string(basePath)

		// Read sibling session_index.jsonl once per root. Missing/corrupt index
		// = no titles (graceful degradation; wizard falls back to date).
		titles := a.loadCodexSessionTitles(root)

		walkErr := a.fs.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return nil // skip unreadable entries
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, defaults.ExtJSONL.String()) {
				return nil
			}

			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return nil
			}
			parts := strings.Split(rel, string(filepath.Separator))

			// Expect YYYY/MM/DD/rollout-...-{uuid}.jsonl — depth 4.
			if len(parts) != 4 {
				return nil
			}
			filename := parts[3]
			if !strings.HasPrefix(filename, codexRolloutFilePrefix) {
				return nil
			}

			sessionIDStr, ok := codexSessionIDFromFilename(filename)
			if !ok {
				return nil
			}
			sid, sidErr := NewSessionID(sessionIDStr)
			if sidErr != nil {
				return nil
			}

			info, statErr := a.fs.Stat(path)
			if statErr != nil {
				return nil
			}

			hints := a.extractCodexHints(path)

			sessions = append(sessions, DiscoveredSession{
				SessionID:     sid,
				Harness:       HarnessCodex,
				SourcePath:    ResolvedPath(path),
				SourceFormat:  SourceFormatJSONL,
				OriginalRoot:  basePath,
				SubagentPaths: []ResolvedPath{},
				DebugPaths:    []ResolvedPath{},
				ModTime:       info.ModTime(),
				Title:         titles[sessionIDStr],
				ProjectName:   hints.projectName,
				Branch:        hints.branch,
				CWD:           hints.cwd,
			})
			return nil
		})

		if walkErr != nil {
			return nil, fmt.Errorf("CodexAdapter.Discover: walk %q: %w", root, walkErr)
		}
	}

	return sessions, nil
}

// codexSessionIndexFile is the filename of the sidecar index Codex CLI writes
// alongside the sessions tree (one level above the dated YYYY/MM/DD/ folders).
// It carries human-readable thread names per session id and is the cheapest
// source for the wizard's session-list rendering.
const codexSessionIndexFile = "session_index.jsonl"

// codexSessionIndexEntry is one line of session_index.jsonl.
type codexSessionIndexEntry struct {
	ID         string `json:"id"`
	ThreadName string `json:"thread_name"`
}

// loadCodexSessionTitles reads {root}/../session_index.jsonl and returns a
// session_id → thread_name map. Returns an empty (non-nil) map on any error;
// the index is opt-in and missing/corrupt = no titles, not a discovery
// failure.
func (a *CodexAdapter) loadCodexSessionTitles(root string) map[string]string {
	titles := map[string]string{}
	indexPath := filepath.Join(filepath.Dir(root), codexSessionIndexFile)

	data, err := a.fs.ReadFile(indexPath)
	if err != nil {
		return titles
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	buf := make([]byte, defaults.ScannerInitBuf)
	scanner.Buffer(buf, defaults.ScannerMaxLine)

	for scanner.Scan() {
		raw := scanner.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var entry codexSessionIndexEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue // skip individual bad lines without failing the whole index
		}
		if entry.ID != "" && entry.ThreadName != "" {
			titles[entry.ID] = entry.ThreadName
		}
	}
	return titles
}

// codexSessionHints holds metadata extracted from a rollout's session_meta line.
// All fields are best-effort; zero values are valid (caller renders fallbacks).
type codexSessionHints struct {
	cwd         string
	branch      string
	projectName string
}

// extractCodexHints peeks the first non-empty line of the rollout file and
// parses session_meta for cwd + git.branch. Project name is the basename of
// cwd. Returns zero hints on any I/O or parse error (the wizard will fall
// back to UUID/date display, identical to pre-hint behavior).
func (a *CodexAdapter) extractCodexHints(path string) codexSessionHints {
	data, err := a.fs.ReadFile(path)
	if err != nil {
		return codexSessionHints{}
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	buf := make([]byte, defaults.ScannerInitBuf)
	scanner.Buffer(buf, defaults.ScannerMaxLine)

	for scanner.Scan() {
		raw := scanner.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var env codexRolloutLine
		if err := json.Unmarshal(raw, &env); err != nil {
			return codexSessionHints{}
		}
		if env.Type != codexTypeSessionMeta {
			return codexSessionHints{}
		}
		var sm codexSessionMeta
		if err := json.Unmarshal(env.Payload, &sm); err != nil {
			return codexSessionHints{}
		}
		hints := codexSessionHints{cwd: sm.CWD}
		if sm.CWD != "" {
			hints.projectName = filepath.Base(sm.CWD)
		}
		if sm.Git != nil {
			hints.branch = sm.Git.Branch
		}
		return hints
	}
	return codexSessionHints{}
}

// codexSessionIDFromFilename extracts the trailing UUID from a
// rollout-{ISO-timestamp}-{uuid}.jsonl filename. The ISO timestamp uses
// hyphens for time separators (rollout-2026-05-29T21-14-24-{uuid}.jsonl), so
// we take the last 5 hyphen-delimited segments as the UUID (8-4-4-4-12).
func codexSessionIDFromFilename(filename string) (string, bool) {
	stem := strings.TrimSuffix(filename, defaults.ExtJSONL.String())
	stem = strings.TrimPrefix(stem, codexRolloutFilePrefix)
	segments := strings.Split(stem, "-")
	if len(segments) < 5 {
		return "", false
	}
	tail := segments[len(segments)-5:]
	return strings.Join(tail, "-"), true
}

// ---------------------------------------------------------------------------
// Rollout line shapes
// ---------------------------------------------------------------------------

// codexRolloutLine is the outer envelope every rollout line shares.
type codexRolloutLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexSessionMeta is the payload of a session_meta line. Captured once at
// session start.
//
// Source is intentionally json.RawMessage, not string: on a top-level CLI
// session it is a plain string ("cli"), but on a subagent/thread-spawn
// session (Codex CLI's spawned-agent feature) it is a nested object
// (`{"subagent":{"thread_spawn":{...}}}`). A strictly-typed string field
// here made json.Unmarshal return a *json.UnmarshalTypeError for THIS field
// alone, and the caller treated any non-nil error as "the whole session_meta
// failed to parse" — discarding the otherwise fully-populated struct
// (including a perfectly good CWD) and falling back to the rollout file's
// own containing directory as the project cwd. Subagent-spawned Codex sessions
// would then be attributed to their ~/.codex/sessions/YYYY/MM/DD folder instead
// of the real project.
// Source itself is never read (kept only so the field survives on the
// struct for future use/debugging without reintroducing the type mismatch).
type codexSessionMeta struct {
	ID            string           `json:"id"`
	Timestamp     string           `json:"timestamp"`
	CWD           string           `json:"cwd"`
	Originator    string           `json:"originator"`
	CLIVersion    string           `json:"cli_version"`
	Source        json.RawMessage  `json:"source"`
	ModelProvider string           `json:"model_provider"`
	Git           *codexGitContext `json:"git,omitempty"`
}

// codexGitContext mirrors the session_meta.payload.git block.
type codexGitContext struct {
	CommitHash    string `json:"commit_hash"`
	Branch        string `json:"branch"`
	RepositoryURL string `json:"repository_url"`
}

// codexTurnContext is the payload of a turn_context line. The model and
// effort can differ per turn; we capture them on the first turn_context and
// update if a later turn changes model (rare in practice).
type codexTurnContext struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
	TurnID string `json:"turn_id"`
}

// codexEventPayload is the payload of an event_msg line, discriminated by
// nested type. We only parse fields we care about (token_count totals and
// task_complete duration).
type codexEventPayload struct {
	Type        string          `json:"type"`
	Info        *codexEventInfo `json:"info,omitempty"`
	CompletedAt *int64          `json:"completed_at,omitempty"`
	DurationMs  *int64          `json:"duration_ms,omitempty"`
}

// codexEventInfo carries token-usage totals on token_count events.
type codexEventInfo struct {
	TotalTokenUsage *codexTokenUsage `json:"total_token_usage,omitempty"`
	LastTokenUsage  *codexTokenUsage `json:"last_token_usage,omitempty"`
}

// codexTokenUsage matches the OpenAI Responses API usage block.
type codexTokenUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
	TotalTokens           int `json:"total_tokens"`
}

// codexResponseItem is the payload of a response_item line, discriminated by
// nested type (message, function_call, function_call_output, reasoning,
// custom_tool_call, custom_tool_call_output).
type codexResponseItem struct {
	Type string `json:"type"`
	Role string `json:"role,omitempty"`
}

// CodexRole is the wire-format role on Codex response_item.message payloads.
type CodexRole string

func (r CodexRole) String() string { return string(r) }

// Recognised payload.type values for response_item / event_msg dispatch.
const (
	codexTypeSessionMeta = "session_meta"
	codexTypeTurnContext = "turn_context"
	codexTypeEventMsg    = "event_msg"
	codexTypeResponse    = "response_item"

	codexResponseMessage       = "message"
	codexResponseFunctionCall  = "function_call"
	codexResponseCustomCall    = "custom_tool_call"
	codexResponseReasoning     = "reasoning"
	codexResponseFunctionOut   = "function_call_output"
	codexResponseCustomCallOut = "custom_tool_call_output"

	codexRoleUser      CodexRole = "user"
	codexRoleAssistant CodexRole = "assistant"
	// codexRoleDeveloper is Codex's system-injected developer prompt (permissions,
	// AGENTS.md context). Distinct from user/assistant on the wire.
	codexRoleDeveloper CodexRole = "developer"

	codexEventTokenCount   = "token_count"
	codexEventTaskComplete = "task_complete"
)

// ExtractMetadata reads the rollout JSONL and builds UnifiedMetadata. The
// first line must be session_meta; later lines are scanned for the model
// (turn_context), turn/tool counts (response_item.message / function_call /
// custom_tool_call), and the running token-usage total (event_msg.token_count).
// The session's end timestamp is the last line's timestamp.
func (a *CodexAdapter) ExtractMetadata(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, error) {
	data, err := a.fs.ReadFile(string(session.SourcePath))
	if err != nil {
		return nil, fmt.Errorf("CodexAdapter.ExtractMetadata: read %q: %w", session.SourcePath, err)
	}

	meta := NewUnifiedMetadata()
	meta.SessionID = session.SessionID
	meta.ParentUUID = nil // Codex has no native subagent model.
	meta.ModelHarness = HarnessCodex
	meta.Source = SourceInfo{
		FilePath: string(session.SourcePath),
		Format:   SourceFormatJSONL,
	}

	var (
		sessionMeta    *codexSessionMeta
		lastTimestamp  string
		lastTotalUsage *codexTokenUsage
		turnCount      int
		toolCount      int
		lineNum        int
		gotSessionMeta bool
	)

	scanner := bufio.NewScanner(bytes.NewReader(data))
	buf := make([]byte, defaults.ScannerInitBuf)
	scanner.Buffer(buf, defaults.ScannerMaxLine)

	for scanner.Scan() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lineNum++
		raw := scanner.Bytes()
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}

		var env codexRolloutLine
		if err := json.Unmarshal(raw, &env); err != nil {
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
				ErrorType:   "parse_error",
				Location:    fmt.Sprintf("line %d", lineNum),
				Message:     fmt.Sprintf("failed to parse rollout line: %v", err),
				Remediation: "Check the rollout file for corruption or truncation at the indicated line.",
			})
			continue
		}

		if env.Timestamp != "" {
			lastTimestamp = env.Timestamp
		}

		switch env.Type {
		case codexTypeSessionMeta:
			var sm codexSessionMeta
			if err := json.Unmarshal(env.Payload, &sm); err == nil {
				sessionMeta = &sm
				gotSessionMeta = true
			} else {
				meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
					ErrorType:   "parse_error",
					Location:    fmt.Sprintf("line %d", lineNum),
					Message:     fmt.Sprintf("failed to parse session_meta payload: %v", err),
					Remediation: "Verify the Codex CLI version that produced this rollout.",
				})
			}

		case codexTypeTurnContext:
			var tc codexTurnContext
			if err := json.Unmarshal(env.Payload, &tc); err == nil && tc.Model != "" {
				if mid, mErr := NewModelID(tc.Model); mErr == nil {
					meta.Model = mid
				}
			}

		case codexTypeResponse:
			var ri codexResponseItem
			if err := json.Unmarshal(env.Payload, &ri); err != nil {
				continue
			}
			switch ri.Type {
			case codexResponseMessage:
				if role := CodexRole(ri.Role); role == codexRoleUser || role == codexRoleAssistant {
					turnCount++
				}
			case codexResponseFunctionCall, codexResponseCustomCall:
				toolCount++
			}

		case codexTypeEventMsg:
			var ev codexEventPayload
			if err := json.Unmarshal(env.Payload, &ev); err != nil {
				continue
			}
			if ev.Type == codexEventTokenCount && ev.Info != nil && ev.Info.TotalTokenUsage != nil {
				lastTotalUsage = ev.Info.TotalTokenUsage
			}
		}
	}

	if err := scanner.Err(); err != nil {
		meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "read_error",
			Location:    fmt.Sprintf("line %d", lineNum),
			Message:     fmt.Sprintf("scanner error reading rollout: %v", err),
			Remediation: "Verify the rollout file is not corrupted or truncated.",
		})
	}

	if !gotSessionMeta {
		meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "missing_session_meta",
			Location:    fmt.Sprintf("session %s", session.SessionID),
			Message:     "rollout did not begin with a session_meta line",
			Remediation: "Rollout may be truncated or from an incompatible Codex CLI version.",
		})
	}

	if sessionMeta != nil {
		meta.Version = sessionMeta.CLIVersion
	}

	// Timestamps: prefer session_meta.timestamp for start; fall back to the
	// first line's envelope timestamp if session_meta is missing.
	startStr := ""
	if sessionMeta != nil {
		startStr = sessionMeta.Timestamp
	}
	startMs := parseTimestampMillis(startStr)
	endMs := parseTimestampMillis(lastTimestamp)
	ingested := time.Now().UnixMilli()
	meta.Timestamp = TimestampInfo{
		Start:    startMs,
		End:      endMs,
		Ingested: &ingested,
	}
	if startMs > 0 && endMs >= startMs {
		meta.Stats.DurationMs = endMs - startMs
	}

	// Git context — Codex captures it once in session_meta. When present, we
	// skip the GitResolver subprocess and derive identifiers directly from the
	// recorded remote. cwd from session_meta is the real project path (no slug
	// decoding needed, unlike Claude).
	cwd := ""
	var remoteURL string
	if sessionMeta != nil {
		cwd = sessionMeta.CWD
		if sessionMeta.Git != nil {
			remoteURL = sessionMeta.Git.RepositoryURL
			if sessionMeta.Git.Branch != "" {
				b := sessionMeta.Git.Branch
				meta.Git.Branch = &b
			}
			if remoteURL != "" {
				r := remoteURL
				meta.Git.Remote = &r
			}
		}
	}
	if cwd == "" {
		cwd = filepath.Dir(string(session.SourcePath))
	}
	meta.CWD = cwd

	projectHash, hostSlug, derErr := DeriveProjectIdentifiers(a.salt, remoteURL, cwd)
	if derErr != nil {
		meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "derive_identity_error",
			Location:    fmt.Sprintf("session %s", session.SessionID),
			Message:     fmt.Sprintf("failed to derive project identifiers: %v", derErr),
			Remediation: "Check the recorded git remote URL or working directory path.",
		})
	} else {
		meta.HostSlug = hostSlug
	}

	meta.Project = ProjectInfo{
		Hash:     projectHash,
		FilePath: cwd,
		Name:     filepath.Base(cwd),
	}

	// Stats.
	meta.Stats.TurnCount = turnCount
	meta.Stats.ToolCallCount = toolCount
	if lastTotalUsage != nil {
		meta.Stats.TokensIn = lastTotalUsage.InputTokens
		meta.Stats.TokensOut = lastTotalUsage.OutputTokens
		if lastTotalUsage.CachedInputTokens > 0 {
			v := lastTotalUsage.CachedInputTokens
			meta.Stats.CachedReadTokens = &v
		}
		if lastTotalUsage.ReasoningOutputTokens > 0 {
			v := lastTotalUsage.ReasoningOutputTokens
			meta.Stats.ThoughtTokens = &v
		}
	}

	return &meta, nil
}
