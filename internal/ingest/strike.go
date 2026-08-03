package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/salt"
)

// StrikeAdapter discovers and extracts metadata from Strike JSONL sessions.
type StrikeAdapter struct {
	fs   FileSystem
	git  GitResolver
	salt salt.Salt
}

var _ SourceAdapter = (*StrikeAdapter)(nil)

// NewStrikeAdapter creates a StrikeAdapter with injected filesystem, git, and salt dependencies.
func NewStrikeAdapter(fs FileSystem, git GitResolver, s salt.Salt) *StrikeAdapter {
	return &StrikeAdapter{fs: fs, git: git, salt: s}
}

// Harness returns HarnessStrike.
func (a *StrikeAdapter) Harness() Harness { return HarnessStrike }

type strikeEventType string

const (
	strikeEventSessionStarted     strikeEventType = "session.started"
	strikeEventSessionTitled      strikeEventType = "session.titled"
	strikeEventUserMessage        strikeEventType = "user.message"
	strikeEventTurnStarted        strikeEventType = "turn.started"
	strikeEventTurnCompleted      strikeEventType = "turn.completed"
	strikeEventAssistantText      strikeEventType = "assistant.text"
	strikeEventAssistantTextDelta strikeEventType = "assistant.text.delta"
	strikeEventMessageDelta       strikeEventType = "assistant.message.delta"
	strikeEventTextDelta          strikeEventType = "text.delta"
	strikeEventReasoning          strikeEventType = "assistant.reasoning"
	strikeEventReasoningDelta     strikeEventType = "assistant.reasoning.delta"
	strikeEventReasoningDeltaWire strikeEventType = "reasoning.delta"
	strikeEventThinkingDelta      strikeEventType = "assistant.thinking.delta"
	strikeEventToolBegin          strikeEventType = "tool.begin"
	strikeEventToolOutput         strikeEventType = "tool.output"
	strikeEventToolEnd            strikeEventType = "tool.end"
	strikeEventProcessStarted     strikeEventType = "process.started"
	strikeEventProcessOutput      strikeEventType = "process.output"
	strikeEventProcessExited      strikeEventType = "process.exited"
	strikeEventUsageReported      strikeEventType = "usage.reported"
	strikeEventModelSelected      strikeEventType = "model.selected"
)

type strikeEnvelope struct {
	Type strikeEventType `json:"type"`
	Time json.RawMessage `json:"time"`
	Data json.RawMessage `json:"data"`
}

type strikeUsage struct {
	InputTokens      *int `json:"inputTokens"`
	OutputTokens     *int `json:"outputTokens"`
	PromptTokens     *int `json:"promptTokens"`
	CompletionTokens *int `json:"completionTokens"`
}

type strikeEventData struct {
	ProviderRequestID string          `json:"providerRequestId"`
	TurnID            string          `json:"turnId"`
	CallID            string          `json:"callId"`
	ProcessID         string          `json:"processId"`
	Name              string          `json:"name"`
	Title             string          `json:"title"`
	Model             string          `json:"model"`
	Version           string          `json:"version"`
	Delta             string          `json:"delta"`
	Text              string          `json:"text"`
	Reasoning         string          `json:"reasoning"`
	StreamData        string          `json:"data"`
	Content           json.RawMessage `json:"content"`
	Message           json.RawMessage `json:"message"`
	Args              json.RawMessage `json:"args"`
	Input             json.RawMessage `json:"input"`
	Output            json.RawMessage `json:"output"`
	Result            json.RawMessage `json:"result"`
	Error             json.RawMessage `json:"error"`
	StopReason        string          `json:"stopReason"`
	InputTokens       *int            `json:"inputTokens"`
	OutputTokens      *int            `json:"outputTokens"`
	PromptTokens      *int            `json:"promptTokens"`
	CompletionTokens  *int            `json:"completionTokens"`
	Usage             *strikeUsage    `json:"usage,omitempty"`
	ExitCode          *int            `json:"exitCode,omitempty"`
	IsError           bool            `json:"isError"`
	Status            string          `json:"status"`
}

func decodeStrikeEventData(raw json.RawMessage) (strikeEventData, error) {
	var data strikeEventData
	if err := json.Unmarshal(raw, &data); err != nil {
		return strikeEventData{}, err
	}
	return data, nil
}

func isKnownStrikeEvent(eventType strikeEventType) bool {
	switch eventType {
	case strikeEventSessionStarted, strikeEventSessionTitled, strikeEventUserMessage,
		strikeEventTurnStarted, strikeEventTurnCompleted, strikeEventAssistantText,
		strikeEventAssistantTextDelta, strikeEventMessageDelta, strikeEventTextDelta,
		strikeEventReasoning, strikeEventReasoningDelta, strikeEventReasoningDeltaWire,
		strikeEventThinkingDelta, strikeEventToolBegin, strikeEventToolOutput,
		strikeEventToolEnd, strikeEventProcessStarted, strikeEventProcessOutput,
		strikeEventProcessExited, strikeEventUsageReported, strikeEventModelSelected:
		return true
	default:
		return false
	}
}

type strikeSidecar struct {
	SessionID       string          `json:"sessionId"`
	ParentSessionID string          `json:"parentSessionId"`
	ParentID        string          `json:"parentId"`
	ProjectKey      string          `json:"projectKey"`
	ProjectName     string          `json:"projectName"`
	WorktreePath    string          `json:"worktreePath"`
	WorktreeBranch  string          `json:"worktreeBranch"`
	Worktree        string          `json:"worktree"`
	CWD             string          `json:"cwd"`
	Branch          string          `json:"branch"`
	Remote          string          `json:"remote"`
	CreatedAt       json.RawMessage `json:"createdAt"`
	StartedAt       json.RawMessage `json:"startedAt"`
	Title           string          `json:"title"`
	Model           string          `json:"model"`
	Version         string          `json:"version"`
	Project         struct {
		Key  string `json:"key"`
		Name string `json:"name"`
		Path string `json:"path"`
	} `json:"project"`
	Git struct {
		Worktree string `json:"worktree"`
		Branch   string `json:"branch"`
		Remote   string `json:"remote"`
	} `json:"git"`
}

func (m strikeSidecar) parentID() string {
	if m.ParentSessionID != "" {
		return m.ParentSessionID
	}
	return m.ParentID
}

func (m strikeSidecar) projectName() string {
	for _, candidate := range []string{m.ProjectName, m.Project.Name, m.ProjectKey, m.Project.Key} {
		if candidate != "" {
			if filepath.IsAbs(candidate) {
				return filepath.Base(candidate)
			}
			return candidate
		}
	}
	return ""
}

func (m strikeSidecar) worktreePath() string {
	for _, candidate := range []string{m.WorktreePath, m.Worktree, m.CWD, m.Git.Worktree, m.Project.Path} {
		if candidate != "" {
			return candidate
		}
	}
	if filepath.IsAbs(m.ProjectKey) {
		return m.ProjectKey
	}
	return ""
}

func (m strikeSidecar) branchName() string {
	if m.WorktreeBranch != "" {
		return m.WorktreeBranch
	}
	if m.Branch != "" {
		return m.Branch
	}
	return m.Git.Branch
}

func (m strikeSidecar) remoteURL() string {
	if m.Remote != "" {
		return m.Remote
	}
	return m.Git.Remote
}

func (m strikeSidecar) createdTime() time.Time {
	for _, raw := range []json.RawMessage{m.CreatedAt, m.StartedAt} {
		if ms := parseIndexTimestamp(raw); ms != nil {
			return time.UnixMilli(*ms)
		}
	}
	return time.Time{}
}

type strikeTranscriptHints struct {
	title   string
	model   string
	version string
}

// Discover finds Strike transcript files under configured roots. A transcript is
// still discoverable when its sidecar is absent or malformed; ExtractMetadata
// records that condition as a diagnostic rather than discarding valid JSONL.
func (a *StrikeAdapter) Discover(ctx context.Context, cfg SourceConfig) ([]DiscoveredSession, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	var sessions []DiscoveredSession
	for _, basePath := range cfg.Paths {
		root := string(basePath)
		walkErr := a.fs.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if walkErr != nil || d.IsDir() || d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if !strings.HasSuffix(path, defaults.ExtJSONL.String()) {
				return nil
			}

			rawID := strings.TrimSuffix(filepath.Base(path), defaults.ExtJSONL.String())
			sid, err := NewSessionID(rawID)
			if err != nil {
				return nil
			}
			info, err := a.fs.Stat(path)
			if err != nil {
				return nil
			}
			modTime := info.ModTime()
			if sidecarInfo, statErr := a.fs.Stat(strikeSidecarPath(path)); statErr == nil && sidecarInfo.ModTime().After(modTime) {
				modTime = sidecarInfo.ModTime()
			}
			resolved, err := NewResolvedPath(path)
			if err != nil {
				return nil
			}

			sidecar, _ := a.readStrikeSidecar(path)
			var parent *SessionID
			if rawParent := sidecar.parentID(); rawParent != "" {
				if parentID, parentErr := NewSessionID(rawParent); parentErr == nil {
					parent = &parentID
				}
			}
			hints := a.readStrikeTranscriptHints(path)
			title := sidecar.Title
			if hints.title != "" {
				title = hints.title
			}
			worktree := sidecar.worktreePath()
			projectName := sidecar.projectName()
			if projectName == "" && worktree != "" {
				projectName = filepath.Base(worktree)
			}

			sessions = append(sessions, DiscoveredSession{
				SessionID:     sid,
				Harness:       HarnessStrike,
				SourcePath:    resolved,
				SourceFormat:  SourceFormatJSONL,
				OriginalRoot:  basePath,
				ParentUUID:    parent,
				SubagentPaths: []ResolvedPath{},
				DebugPaths:    []ResolvedPath{},
				ModTime:       modTime,
				ProjectName:   projectName,
				Title:         title,
				Branch:        sidecar.branchName(),
				CWD:           worktree,
				CreatedAt:     sidecar.createdTime(),
			})
			return nil
		})
		if walkErr != nil {
			return nil, fmt.Errorf("strike discover: walk %q: %w", root, walkErr)
		}
	}

	return normalizeStrikeSessions(sessions), nil
}

// normalizeStrikeSessions validates parent edges, returns parents before their
// descendants, and propagates descendant mtimes so relationship changes cause
// the parent metadata to be regenerated.
func normalizeStrikeSessions(sessions []DiscoveredSession) []DiscoveredSession {
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].SessionID.String() < sessions[j].SessionID.String()
	})
	byID := make(map[SessionID]int, len(sessions))
	for i := range sessions {
		byID[sessions[i].SessionID] = i
	}

	// Each node has at most one parent. Follow each chain and detach every node
	// participating in a cycle so all transcript content remains ingestible.
	done := make(map[SessionID]bool, len(sessions))
	for _, session := range sessions {
		if done[session.SessionID] {
			continue
		}
		var path []SessionID
		positions := make(map[SessionID]int)
		current := session.SessionID
		for {
			if cycleStart, cyclic := positions[current]; cyclic {
				for _, cycleID := range path[cycleStart:] {
					index := byID[cycleID]
					parentID := sessions[index].ParentUUID
					sessions[index].DiscoveryWarnings = append(sessions[index].DiscoveryWarnings, DiagnosticEntry{
						ErrorType: "invalid_parent_cycle",
						Location:  strikeSidecarPath(sessions[index].SourcePath.String()),
						Message: fmt.Sprintf(
							"Strike session %q declares parent %q in a cyclic relationship; the transcript was retained as a root session",
							cycleID, parentID.String(),
						),
						Remediation: "Correct parentSessionId so the parent graph is acyclic, then rerun peasant ingest to restore the relationship.",
					})
					sessions[index].ParentUUID = nil
				}
				break
			}
			if done[current] {
				break
			}
			index, exists := byID[current]
			if !exists {
				break
			}
			positions[current] = len(path)
			path = append(path, current)
			parentID := sessions[index].ParentUUID
			if parentID == nil {
				break
			}
			if _, exists := byID[*parentID]; !exists {
				break
			}
			current = *parentID
		}
		for _, id := range path {
			done[id] = true
		}
	}

	childrenOf := make(map[SessionID][]int, len(sessions))
	var order []int
	for i := range sessions {
		sessions[i].SubagentPaths = nil
		if sessions[i].ParentUUID != nil {
			if _, exists := byID[*sessions[i].ParentUUID]; exists {
				childrenOf[*sessions[i].ParentUUID] = append(childrenOf[*sessions[i].ParentUUID], i)
				continue
			}
		}
		order = append(order, i)
	}
	for cursor := 0; cursor < len(order); cursor++ {
		parentIndex := order[cursor]
		children := childrenOf[sessions[parentIndex].SessionID]
		order = append(order, children...)
	}

	// Acyclic normalization above guarantees complete ordering. Keep a safe
	// fallback so malformed duplicate IDs cannot silently disappear.
	if len(order) != len(sessions) {
		order = order[:0]
		for i := range sessions {
			order = append(order, i)
		}
	}

	for cursor := len(order) - 1; cursor >= 0; cursor-- {
		childIndex := order[cursor]
		parentID := sessions[childIndex].ParentUUID
		if parentID == nil {
			continue
		}
		parentIndex, exists := byID[*parentID]
		if !exists {
			continue
		}
		sessions[parentIndex].SubagentPaths = append(sessions[parentIndex].SubagentPaths, sessions[childIndex].SourcePath)
		if sessions[childIndex].ModTime.After(sessions[parentIndex].ModTime) {
			sessions[parentIndex].ModTime = sessions[childIndex].ModTime
		}
	}
	for i := range sessions {
		sort.Slice(sessions[i].SubagentPaths, func(left, right int) bool {
			return sessions[i].SubagentPaths[left].String() < sessions[i].SubagentPaths[right].String()
		})
	}

	ordered := make([]DiscoveredSession, 0, len(sessions))
	for _, index := range order {
		ordered = append(ordered, sessions[index])
	}
	return ordered
}

func (a *StrikeAdapter) readStrikeSidecar(transcriptPath string) (strikeSidecar, error) {
	path := strikeSidecarPath(transcriptPath)
	data, err := a.fs.ReadFile(path)
	if err != nil {
		return strikeSidecar{}, err
	}
	var sidecar strikeSidecar
	if err := json.Unmarshal(data, &sidecar); err != nil {
		return strikeSidecar{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return sidecar, nil
}

func strikeSidecarPath(transcriptPath string) string {
	return strings.TrimSuffix(transcriptPath, defaults.ExtJSONL.String()) + ".meta.json"
}

func (a *StrikeAdapter) readStrikeTranscriptHints(path string) strikeTranscriptHints {
	data, err := a.fs.ReadFile(path)
	if err != nil {
		return strikeTranscriptHints{}
	}
	var hints strikeTranscriptHints
	forEachStrikeRecord(data, func(_ int, raw []byte) {
		if strikeRecordTooLarge(raw) {
			return
		}
		var env strikeEnvelope
		if json.Unmarshal(bytes.TrimSpace(raw), &env) != nil {
			return
		}
		switch env.Type {
		case strikeEventSessionTitled:
			event, err := decodeStrikeEventData(env.Data)
			if err != nil {
				return
			}
			if event.Title != "" {
				hints.title = event.Title
			}
		case strikeEventSessionStarted, strikeEventModelSelected:
			event, err := decodeStrikeEventData(env.Data)
			if err != nil {
				return
			}
			if event.Model != "" {
				hints.model = event.Model
			}
			if event.Version != "" {
				hints.version = event.Version
			}
		}
	})
	return hints
}

// ExtractMetadata reads one Strike transcript and its optional sidecar into the
// existing unified metadata contract. Individual malformed records are diagnosed
// and skipped so later valid records still contribute metadata.
func (a *StrikeAdapter) ExtractMetadata(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, error) {
	data, err := a.fs.ReadFile(session.SourcePath.String())
	if err != nil {
		return nil, fmt.Errorf("strike metadata: read %s: %w", session.SourcePath, err)
	}

	meta := NewUnifiedMetadata()
	meta.SessionID = session.SessionID
	meta.ParentUUID = session.ParentUUID
	meta.ModelHarness = HarnessStrike
	meta.Source = SourceInfo{FilePath: session.SourcePath.String(), Format: SourceFormatJSONL}
	meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, session.DiscoveryWarnings...)

	sidecar, sidecarErr := a.readStrikeSidecar(session.SourcePath.String())
	sidecarPath := strikeSidecarPath(session.SourcePath.String())
	if sidecarErr != nil {
		errorType := "sidecar_parse_error"
		message := fmt.Sprintf("failed to parse Strike metadata sidecar: %v", sidecarErr)
		if errors.Is(sidecarErr, os.ErrNotExist) {
			errorType = "missing_sidecar"
			message = "Strike metadata sidecar is missing; transcript content will still be ingested with fallback metadata"
		}
		meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   errorType,
			Location:    sidecarPath,
			Message:     message,
			Remediation: "Restore a valid sibling <session-id>.meta.json file and rerun peasant ingest to recover parent, project, and title metadata.",
		})
	}

	if sidecar.SessionID != "" && sidecar.SessionID != session.SessionID.String() {
		meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "sidecar_session_mismatch",
			Location:    sidecarPath,
			Message:     fmt.Sprintf("Strike sidecar sessionId %q does not match transcript filename session ID %q; the validated filename ID was retained", sidecar.SessionID, session.SessionID),
			Remediation: "Regenerate or correct the sidecar so sessionId matches the sibling JSONL filename, then rerun peasant ingest.",
		})
	}
	if rawParent := sidecar.parentID(); rawParent != "" {
		parent, parentErr := NewSessionID(rawParent)
		if parentErr != nil {
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
				ErrorType:   "invalid_parent_session_id",
				Location:    sidecarPath,
				Message:     fmt.Sprintf("Strike sidecar parent session ID %q is invalid: %v; this session cannot be linked to its parent", rawParent, parentErr),
				Remediation: "Correct parentSessionId to a valid Strike session ID and rerun peasant ingest.",
			})
		} else if session.ParentUUID != nil && *session.ParentUUID != parent {
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
				ErrorType:   "parent_session_mismatch",
				Location:    sidecarPath,
				Message:     fmt.Sprintf("Strike sidecar parent %q did not match the relationship established during discovery; the discovered relationship was retained", parent),
				Remediation: "Keep the sidecar stable while ingest runs, then rerun peasant ingest.",
			})
		}
	}

	hints := a.readStrikeTranscriptHints(session.SourcePath.String())
	model := sidecar.Model
	if hints.model != "" {
		model = hints.model
	}
	if model != "" {
		if modelID, modelErr := NewModelID(model); modelErr == nil {
			meta.Model = modelID
		} else {
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
				ErrorType:   "invalid_model",
				Location:    session.SourcePath.String(),
				Message:     fmt.Sprintf("Strike recorded model %q but it is not a valid model identifier: %v; local ingestion will continue without model enrichment", model, modelErr),
				Remediation: "Update Strike to emit a path-safe model identifier, then rerun peasant ingest.",
			})
		}
	}
	meta.Version = sidecar.Version
	if hints.version != "" {
		meta.Version = hints.version
	}

	var firstTime, lastTime int64
	turnStarts := 0
	userMessages := 0
	callIDs := make(map[string]bool)
	tokensIn, tokensOut := 0, 0
	forEachStrikeRecord(data, func(line int, raw []byte) {
		if strikeRecordTooLarge(raw) {
			return
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 {
			return
		}
		var env strikeEnvelope
		if err := json.Unmarshal(trimmed, &env); err != nil {
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
				ErrorType:   "parse_error",
				Location:    fmt.Sprintf("%s line %d", session.SourcePath, line),
				Message:     fmt.Sprintf("failed to parse Strike JSONL record: %v; later valid records were still processed", err),
				Remediation: "Inspect or regenerate the malformed record at the indicated line, then rerun peasant ingest.",
			})
			return
		}
		if !isKnownStrikeEvent(env.Type) {
			return
		}
		event, dataErr := decodeStrikeEventData(env.Data)
		if dataErr != nil {
			meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
				ErrorType:   "event_data_parse_error",
				Location:    fmt.Sprintf("%s line %d", session.SourcePath, line),
				Message:     fmt.Sprintf("failed to decode data for known Strike event %q: %v; the malformed record was skipped and later valid records were still processed", env.Type, dataErr),
				Remediation: "Inspect the event data types at the indicated line or regenerate the transcript, then rerun peasant ingest.",
			})
			return
		}
		if timestamp := parseIndexTimestamp(env.Time); timestamp != nil {
			if firstTime == 0 {
				firstTime = *timestamp
			}
			lastTime = *timestamp
		}
		switch env.Type {
		case strikeEventTurnStarted:
			turnStarts++
		case strikeEventUserMessage:
			userMessages++
		case strikeEventToolBegin, strikeEventToolEnd:
			key := event.CallID
			if key == "" {
				key = fmt.Sprintf("line-%d", line)
			}
			callIDs[key] = true
		case strikeEventUsageReported:
			in, _, out, _ := strikeUsageCounts(event)
			tokensIn += in
			tokensOut += out
		}
	})

	created := sidecar.createdTime()
	if firstTime == 0 && !created.IsZero() {
		firstTime = created.UnixMilli()
	}
	if firstTime == 0 && !session.CreatedAt.IsZero() {
		firstTime = session.CreatedAt.UnixMilli()
	}
	if firstTime == 0 && !session.ModTime.IsZero() {
		firstTime = session.ModTime.UnixMilli()
	}
	if lastTime == 0 && !session.ModTime.IsZero() {
		lastTime = session.ModTime.UnixMilli()
	}
	if lastTime < firstTime {
		lastTime = firstTime
	}
	meta.Timestamp.Start = firstTime
	meta.Timestamp.End = lastTime
	meta.Stats.DurationMs = lastTime - firstTime
	if turnStarts > 0 {
		meta.Stats.TurnCount = turnStarts
	} else {
		meta.Stats.TurnCount = userMessages
	}
	meta.Stats.ToolCallCount = len(callIDs)
	meta.Stats.SubagentCount = len(session.SubagentPaths)
	meta.Stats.TokensIn = tokensIn
	meta.Stats.TokensOut = tokensOut

	worktree := sidecar.worktreePath()
	if worktree == "" {
		worktree = session.CWD
	}
	if worktree == "" {
		worktree = filepath.Dir(session.SourcePath.String())
	}
	meta.CWD = worktree
	branch := sidecar.branchName()
	if branch == "" {
		branch = session.Branch
	}
	if branch != "" {
		meta.Git.Branch = &branch
	}
	if worktree != "" {
		meta.Git.Worktree = &worktree
	}
	remote := sidecar.remoteURL()
	if remote != "" {
		meta.Git.Remote = &remote
	}

	projectHash, hostSlug, identityErr := DeriveProjectIdentifiersWithGit(ctx, a.salt, a.git, remote, worktree)
	if identityErr != nil {
		return nil, fmt.Errorf("strike metadata: derive project identity for session %s from worktree %q: %w", session.SessionID, worktree, identityErr)
	}
	projectName := sidecar.projectName()
	if projectName == "" {
		projectName = session.ProjectName
	}
	if projectName == "" {
		projectName = filepath.Base(worktree)
	}
	meta.Project = ProjectInfo{Hash: projectHash, FilePath: worktree, Name: projectName}
	meta.HostSlug = hostSlug

	for _, childPath := range session.SubagentPaths {
		rawChildID := strings.TrimSuffix(filepath.Base(childPath.String()), defaults.ExtJSONL.String())
		childID, childErr := NewSessionID(rawChildID)
		if childErr != nil {
			continue
		}
		meta.Subagents = append(meta.Subagents, SubagentRef{SessionID: childID, ParentUUID: session.SessionID})
	}

	return &meta, nil
}

func strikeUsageCounts(event strikeEventData) (int, bool, int, bool) {
	in, inKnown := strikeFirstCount(event.InputTokens, event.PromptTokens)
	out, outKnown := strikeFirstCount(event.OutputTokens, event.CompletionTokens)
	if event.Usage != nil {
		if !inKnown {
			in, inKnown = strikeFirstCount(event.Usage.InputTokens, event.Usage.PromptTokens)
		}
		if !outKnown {
			out, outKnown = strikeFirstCount(event.Usage.OutputTokens, event.Usage.CompletionTokens)
		}
	}
	if !inKnown {
		in, inKnown = strikeKnownTokenCount(event.Input)
	}
	if !outKnown {
		out, outKnown = strikeKnownTokenCount(event.Output)
	}
	return in, inKnown, out, outKnown
}

func strikeFirstCount(counts ...*int) (int, bool) {
	for _, count := range counts {
		if count != nil {
			return *count, true
		}
	}
	return 0, false
}

func strikeKnownTokenCount(raw json.RawMessage) (int, bool) {
	var count struct {
		N     int  `json:"n"`
		Known bool `json:"known"`
	}
	if json.Unmarshal(raw, &count) != nil || !count.Known {
		return 0, false
	}
	return count.N, true
}

// forEachStrikeRecord walks JSONL records using newline indexes rather than a
// scanner, so one oversized record cannot prevent later records from being seen.
func forEachStrikeRecord(data []byte, visit func(line int, record []byte)) {
	line := 1
	for start := 0; start < len(data); line++ {
		relEnd := bytes.IndexByte(data[start:], '\n')
		if relEnd < 0 {
			visit(line, data[start:])
			return
		}
		end := start + relEnd
		visit(line, data[start:end])
		start = end + 1
	}
}
