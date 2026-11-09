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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/schema"
)

// StrikeAdapter discovers and extracts metadata from Strike JSONL sessions.
type StrikeAdapter struct {
	fs   FileSystem
	git  GitResolver
	salt salt.Salt
}

var _ SourceAdapter = (*StrikeAdapter)(nil)
var _ SessionLocator = (*StrikeAdapter)(nil)

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

	// Lifecycle, control and metadata events Strike persists beside the
	// conversation (jonathanung/strike docs/session-format.md). They carry no
	// conversation content except agent.message.
	strikeEventSessionHeader         strikeEventType = "session.header"
	strikeEventAgentSelected         strikeEventType = "agent.selected"
	strikeEventEffortSelected        strikeEventType = "effort.selected"
	strikeEventAutonomySelected      strikeEventType = "autonomy.selected"
	strikeEventPermissionMode        strikeEventType = "permission.mode"
	strikeEventFastSelected          strikeEventType = "fast.selected"
	strikeEventContextControls       strikeEventType = "context.controls"
	strikeEventSessionMeta           strikeEventType = "session.meta"
	strikeEventPromptEffective       strikeEventType = "prompt.effective"
	strikeEventPermissionAsked       strikeEventType = "permission.asked"
	strikeEventPermissionResolved    strikeEventType = "permission.resolved"
	strikeEventPermissionDecided     strikeEventType = "permission.decided"
	strikeEventAdmissionDecided      strikeEventType = "admission.decided"
	strikeEventQuestionAsked         strikeEventType = "question.asked"
	strikeEventQuestionResolved      strikeEventType = "question.resolved"
	strikeEventTurnSteered           strikeEventType = "turn.steered"
	strikeEventVerificationStarted   strikeEventType = "verification.started"
	strikeEventVerificationCompleted strikeEventType = "verification.completed"
	strikeEventHarnessProgress       strikeEventType = "harness.progress"
	strikeEventPhaseChanged          strikeEventType = "phase.changed"
	strikeEventPlanHandoff           strikeEventType = "plan.handoff"
	strikeEventArtifactUpdated       strikeEventType = "artifact.updated"
	strikeEventLedgerUpdated         strikeEventType = "ledger.updated"
	strikeEventPhaseGrantApproved    strikeEventType = "phase.grant_approved"
	strikeEventFilesInvalidated      strikeEventType = "files.invalidated"
	strikeEventPathOverlap           strikeEventType = "path.overlap"
	strikeEventEngineError           strikeEventType = "engine.error"
	strikeEventChildStarted          strikeEventType = "child.started"
	strikeEventChildCompleted        strikeEventType = "child.completed"
	strikeEventChildEscalated        strikeEventType = "child.escalated"
	strikeEventDelegationChanged     strikeEventType = "delegation.changed"
	strikeEventWaitStarted           strikeEventType = "wait.started"
	strikeEventWaitResolved          strikeEventType = "wait.resolved"
	strikeEventAgentContractTimeout  strikeEventType = "agent.contract.timeout"
	strikeEventTeamRoster            strikeEventType = "team.roster"
	strikeEventProviderRetrying      strikeEventType = "provider.retrying"
	strikeEventToolRetrying          strikeEventType = "tool.retrying"
	strikeEventToolLoopDetected      strikeEventType = "tool.loop_detected"
	strikeEventSchedulerQueued       strikeEventType = "scheduler.queued"
	strikeEventSchedulerAdmitted     strikeEventType = "scheduler.admitted"
	strikeEventSchedulerCanceled     strikeEventType = "scheduler.canceled"
	strikeEventCompactionStarted     strikeEventType = "compaction.started"
	strikeEventCompactionCompleted   strikeEventType = "compaction.completed"
	strikeEventSessionRewound        strikeEventType = "session.rewound"
	strikeEventHookMatched           strikeEventType = "hook.matched"
	strikeEventDiagnosticBundle      strikeEventType = "diagnostic.bundle"
	strikeEventContextFitWarning     strikeEventType = "context.fit_warning"
	strikeEventSessionBudgetWarning  strikeEventType = "session.budget_warning"
	strikeEventAgentMessage          strikeEventType = "agent.message"
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
	SessionID         string          `json:"sessionId"`
	ParentSessionID   string          `json:"parentSessionId"`
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
	CacheRead         json.RawMessage `json:"cacheRead"`
	CacheCreation     json.RawMessage `json:"cacheCreation"`
}

// strikeLifecycleData is the narrow shape read from lifecycle, control and
// metadata events. Only the correlation and the conversation text of
// agent.message (body) and turn.steered (text) are read; every other field
// belongs to Strike and is never interpreted, so an additive Strike field
// cannot refuse the capture.
type strikeLifecycleData struct {
	SessionID       string `json:"sessionId"`
	ParentSessionID string `json:"parentSessionId"`
	TurnID          string `json:"turnId"`
	Body            string `json:"body"`
	Text            string `json:"text"`
}

// decodeStrikeEvent decodes one known event's data. The session.header schema
// marker carries no data. Lifecycle kinds use the narrow shape and name the
// event type when their data is malformed.
func decodeStrikeEvent(eventType strikeEventType, raw json.RawMessage) (strikeEventData, error) {
	if eventType == strikeEventSessionHeader {
		return strikeEventData{}, nil
	}
	if !slices.Contains(strikeLifecycleEventKinds(), eventType) {
		return decodeStrikeEventData(raw)
	}
	var data strikeLifecycleData
	if err := json.Unmarshal(raw, &data); err != nil {
		return strikeEventData{}, fmt.Errorf("strike %s event data: %w", eventType, err)
	}
	return strikeEventData{
		SessionID:       data.SessionID,
		ParentSessionID: data.ParentSessionID,
		TurnID:          data.TurnID,
		Text:            firstNonEmpty(data.Body, data.Text),
	}, nil
}

func decodeStrikeEventData(raw json.RawMessage) (strikeEventData, error) {
	var data strikeEventData
	if err := json.Unmarshal(raw, &data); err != nil {
		return strikeEventData{}, err
	}
	return data, nil
}

// knownStrikeEventKinds is the closed set of Strike event types this build
// parses. It is the single source of truth for isKnownStrikeEvent, and the
// record-kind vocabulary completeness check compares it with the declaration.
var knownStrikeEventKinds = []strikeEventType{
	strikeEventSessionStarted,
	strikeEventSessionTitled,
	strikeEventUserMessage,
	strikeEventTurnStarted,
	strikeEventTurnCompleted,
	strikeEventAssistantText,
	strikeEventAssistantTextDelta,
	strikeEventMessageDelta,
	strikeEventTextDelta,
	strikeEventReasoning,
	strikeEventReasoningDelta,
	strikeEventReasoningDeltaWire,
	strikeEventThinkingDelta,
	strikeEventToolBegin,
	strikeEventToolOutput,
	strikeEventToolEnd,
	strikeEventProcessStarted,
	strikeEventProcessOutput,
	strikeEventProcessExited,
	strikeEventUsageReported,
	strikeEventModelSelected,
	strikeEventSessionHeader,
	strikeEventAgentSelected,
	strikeEventEffortSelected,
	strikeEventAutonomySelected,
	strikeEventPermissionMode,
	strikeEventFastSelected,
	strikeEventContextControls,
	strikeEventSessionMeta,
	strikeEventPromptEffective,
	strikeEventPermissionAsked,
	strikeEventPermissionResolved,
	strikeEventPermissionDecided,
	strikeEventAdmissionDecided,
	strikeEventQuestionAsked,
	strikeEventQuestionResolved,
	strikeEventTurnSteered,
	strikeEventVerificationStarted,
	strikeEventVerificationCompleted,
	strikeEventHarnessProgress,
	strikeEventPhaseChanged,
	strikeEventPlanHandoff,
	strikeEventArtifactUpdated,
	strikeEventLedgerUpdated,
	strikeEventPhaseGrantApproved,
	strikeEventFilesInvalidated,
	strikeEventPathOverlap,
	strikeEventEngineError,
	strikeEventChildStarted,
	strikeEventChildCompleted,
	strikeEventChildEscalated,
	strikeEventDelegationChanged,
	strikeEventWaitStarted,
	strikeEventWaitResolved,
	strikeEventAgentContractTimeout,
	strikeEventTeamRoster,
	strikeEventProviderRetrying,
	strikeEventToolRetrying,
	strikeEventToolLoopDetected,
	strikeEventSchedulerQueued,
	strikeEventSchedulerAdmitted,
	strikeEventSchedulerCanceled,
	strikeEventCompactionStarted,
	strikeEventCompactionCompleted,
	strikeEventSessionRewound,
	strikeEventHookMatched,
	strikeEventDiagnosticBundle,
	strikeEventContextFitWarning,
	strikeEventSessionBudgetWarning,
	strikeEventAgentMessage,
}

func isKnownStrikeEvent(eventType strikeEventType) bool {
	_, ok := knownStrikeEventKind(string(eventType))
	return ok
}

func knownStrikeEventKind(raw string) (strikeEventType, bool) {
	for _, known := range knownStrikeEventKinds {
		if raw == string(known) {
			return known, true
		}
	}
	return "", false
}

// strikeMetadataEventKinds names the known Strike events that carry session
// metadata rather than conversation content. The strict capture path records
// them as ignored metadata; keep this set aligned with its metadata branch.
func strikeMetadataEventKinds() []strikeEventType {
	return []strikeEventType{
		strikeEventSessionStarted, strikeEventSessionTitled, strikeEventModelSelected,
		strikeEventSessionHeader,
		strikeEventAgentSelected,
		strikeEventEffortSelected,
		strikeEventAutonomySelected,
		strikeEventPermissionMode,
		strikeEventFastSelected,
		strikeEventContextControls,
		strikeEventSessionMeta,
		strikeEventPromptEffective,
	}
}

// strikeControlEventKinds names the known Strike lifecycle and control events:
// permissions, questions, phases, children, team, scheduling, retries,
// verification and engine state. None carries conversation content; each is
// recognized and entryless, and the strict capture records it as ignored
// control so it never blocks certification.
func strikeControlEventKinds() []strikeEventType {
	return []strikeEventType{
		strikeEventPermissionAsked,
		strikeEventPermissionResolved,
		strikeEventPermissionDecided,
		strikeEventAdmissionDecided,
		strikeEventQuestionAsked,
		strikeEventQuestionResolved,
		strikeEventVerificationStarted,
		strikeEventVerificationCompleted,
		strikeEventHarnessProgress,
		strikeEventPhaseChanged,
		strikeEventPlanHandoff,
		strikeEventArtifactUpdated,
		strikeEventLedgerUpdated,
		strikeEventPhaseGrantApproved,
		strikeEventFilesInvalidated,
		strikeEventPathOverlap,
		strikeEventEngineError,
		strikeEventChildStarted,
		strikeEventChildCompleted,
		strikeEventChildEscalated,
		strikeEventDelegationChanged,
		strikeEventWaitStarted,
		strikeEventWaitResolved,
		strikeEventAgentContractTimeout,
		strikeEventTeamRoster,
		strikeEventProviderRetrying,
		strikeEventToolRetrying,
		strikeEventToolLoopDetected,
		strikeEventSchedulerQueued,
		strikeEventSchedulerAdmitted,
		strikeEventSchedulerCanceled,
		strikeEventCompactionStarted,
		strikeEventCompactionCompleted,
		strikeEventSessionRewound,
		strikeEventHookMatched,
		strikeEventDiagnosticBundle,
		strikeEventContextFitWarning,
		strikeEventSessionBudgetWarning,
	}
}

// strikeEntrylessEventKinds is every known Strike event whose payload is
// never projected into an entry. Their payloads are not scanned for nested
// content blocks: a lifecycle "message" string is not a conversation block.
func strikeEntrylessEventKinds() []strikeEventType {
	return append(strikeMetadataEventKinds(), strikeControlEventKinds()...)
}

// strikeLifecycleEventKinds are the event kinds this build added for #449.
// Their correlation may name another session (a child.* event names the child,
// team.roster names the lead), so they do not take part in the transcript
// identity check and are decoded through the narrow lifecycle shape.
func strikeLifecycleEventKinds() []strikeEventType {
	return []strikeEventType{
		strikeEventSessionHeader,
		strikeEventAgentSelected,
		strikeEventEffortSelected,
		strikeEventAutonomySelected,
		strikeEventPermissionMode,
		strikeEventFastSelected,
		strikeEventContextControls,
		strikeEventSessionMeta,
		strikeEventPromptEffective,
		strikeEventPermissionAsked,
		strikeEventPermissionResolved,
		strikeEventPermissionDecided,
		strikeEventAdmissionDecided,
		strikeEventQuestionAsked,
		strikeEventQuestionResolved,
		strikeEventTurnSteered,
		strikeEventVerificationStarted,
		strikeEventVerificationCompleted,
		strikeEventHarnessProgress,
		strikeEventPhaseChanged,
		strikeEventPlanHandoff,
		strikeEventArtifactUpdated,
		strikeEventLedgerUpdated,
		strikeEventPhaseGrantApproved,
		strikeEventFilesInvalidated,
		strikeEventPathOverlap,
		strikeEventEngineError,
		strikeEventChildStarted,
		strikeEventChildCompleted,
		strikeEventChildEscalated,
		strikeEventDelegationChanged,
		strikeEventWaitStarted,
		strikeEventWaitResolved,
		strikeEventAgentContractTimeout,
		strikeEventTeamRoster,
		strikeEventProviderRetrying,
		strikeEventToolRetrying,
		strikeEventToolLoopDetected,
		strikeEventSchedulerQueued,
		strikeEventSchedulerAdmitted,
		strikeEventSchedulerCanceled,
		strikeEventCompactionStarted,
		strikeEventCompactionCompleted,
		strikeEventSessionRewound,
		strikeEventHookMatched,
		strikeEventDiagnosticBundle,
		strikeEventContextFitWarning,
		strikeEventSessionBudgetWarning,
		strikeEventAgentMessage,
	}
}

type strikeSidecar struct {
	SessionID       string          `json:"sessionId"`
	ParentSessionID string          `json:"parentSessionId"`
	ParentID        string          `json:"parentId"`
	LeadSessionID   string          `json:"leadSessionId"`
	ForkedFrom      string          `json:"forkedFrom"`
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
	// children are the sessions this transcript's child.* events name as
	// its own foreground children, in first-seen order.
	children []SessionID
}

// strikeSource is one enumerated Strike transcript before the shared build
// reads it. info and loaded are filled by a caller that already observed the
// file (a located source must read its own stream to learn the children it
// names), so one transcript is never read twice.
type strikeSource struct {
	root     ResolvedPath
	id       SessionID
	path     string
	resolved ResolvedPath
	info     os.FileInfo
	loaded   bool
	modTime  time.Time
	sidecar  strikeSidecar
	hints    strikeTranscriptHints
}

// Discover finds Strike transcript files under configured roots. A transcript is
// still discoverable when its sidecar is absent or malformed; ExtractMetadata
// records that condition as a diagnostic rather than discarding valid JSONL.
//
// The walk only names files. Every root is enumerated first and the whole set is
// handed to the same build DiscoverSessionAt uses, so a full walk and a located
// source cannot describe one session two ways.
func (a *StrikeAdapter) Discover(ctx context.Context, cfg SourceConfig) ([]DiscoveredSession, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	var sources []strikeSource
	for _, basePath := range cfg.Paths {
		enumerated, err := a.enumerateStrikeRoot(ctx, basePath)
		if err != nil {
			return nil, err
		}
		sources = append(sources, enumerated...)
	}
	return a.buildStrikeSessions(sources), nil
}

// enumerateStrikeRoot walks one configured root and returns the transcript files
// it holds. It reads names only: a file that disappears before the build reads
// it is dropped by the build, exactly as the walk's own stat used to drop it.
func (a *StrikeAdapter) enumerateStrikeRoot(ctx context.Context, basePath ResolvedPath) ([]strikeSource, error) {
	var sources []strikeSource
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
		sources = append(sources, strikeSource{root: basePath, id: sid, path: path})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("strike discover: walk %q: %w", root, walkErr)
	}
	return sources, nil
}

// loadStrikeSource reads one enumerated transcript's stat, sidecar and stream
// hints. A missing or malformed sidecar is tolerated here: ExtractMetadata
// records it as a diagnostic, and the transcript still discovers with fallback
// metadata.
func (a *StrikeAdapter) loadStrikeSource(source strikeSource) (strikeSource, error) {
	if source.loaded {
		return source, nil
	}
	if source.info == nil {
		info, err := a.fs.Stat(source.path)
		if err != nil {
			return source, err
		}
		source.info = info
	}
	resolved, err := NewResolvedPath(source.path)
	if err != nil {
		return source, err
	}
	source.resolved = resolved
	source.modTime = source.info.ModTime()
	if sidecarInfo, statErr := a.fs.Stat(strikeSidecarPath(source.path)); statErr == nil && sidecarInfo.ModTime().After(source.modTime) {
		source.modTime = sidecarInfo.ModTime()
	}
	source.sidecar, _ = a.readStrikeSidecar(source.path)
	source.hints = a.readStrikeTranscriptHints(source.path, source.id)
	source.loaded = true
	return source, nil
}

// buildStrikeSession turns one loaded source into a discovered session. It is
// the one place the DiscoveredSession literal is written, so the full walk and
// the located-source path cannot drift apart.
func (a *StrikeAdapter) buildStrikeSession(source strikeSource) (DiscoveredSession, bool) {
	if source.resolved == "" {
		return DiscoveredSession{}, false
	}
	title := source.sidecar.Title
	if source.hints.title != "" {
		title = source.hints.title
	}
	worktree := source.sidecar.worktreePath()
	projectName := source.sidecar.projectName()
	if projectName == "" && worktree != "" {
		projectName = filepath.Base(worktree)
	}
	return DiscoveredSession{
		SessionID:     source.id,
		Harness:       HarnessStrike,
		SourcePath:    source.resolved,
		SourceFormat:  SourceFormatJSONL,
		OriginalRoot:  source.root,
		ParentUUID:    strikeParent(source.sidecar),
		SubagentPaths: []ResolvedPath{},
		DebugPaths:    []ResolvedPath{},
		ModTime:       source.modTime,
		ProjectName:   projectName,
		Title:         title,
		Branch:        source.sidecar.branchName(),
		CWD:           worktree,
		CreatedAt:     source.sidecar.createdTime(),
	}, true
}

// strikeParent is the parent the sidecar's own parent field names. forkedFrom
// is deliberately absent: it records a context_from relationship, not a parent.
func strikeParent(sidecar strikeSidecar) *SessionID {
	rawParent := sidecar.parentID()
	if rawParent == "" {
		return nil
	}
	parent, err := NewSessionID(rawParent)
	if err != nil {
		return nil
	}
	return &parent
}

// buildStrikeSessions is the one construction path for Discover and
// DiscoverSessionAt. The sidecar parent stays authoritative; a child whose own
// sidecar names no parent takes the parent whose child.* events name it, so a
// full walk and a located source link the same sessions.
func (a *StrikeAdapter) buildStrikeSessions(sources []strikeSource) []DiscoveredSession {
	// childParents maps a child to the session whose child.* events name it.
	childParents := make(map[SessionID]SessionID)
	loaded := make([]strikeSource, 0, len(sources))
	sessions := make([]DiscoveredSession, 0, len(sources))
	for _, source := range sources {
		filled, err := a.loadStrikeSource(source)
		if err != nil {
			continue
		}
		session, ok := a.buildStrikeSession(filled)
		if !ok {
			continue
		}
		loaded = append(loaded, filled)
		sessions = append(sessions, session)
	}
	for _, source := range loaded {
		for _, child := range source.hints.children {
			if _, claimed := childParents[child]; !claimed {
				childParents[child] = source.id
			}
		}
	}
	for i := range sessions {
		if sessions[i].ParentUUID != nil {
			continue
		}
		if parent, ok := childParents[sessions[i].SessionID]; ok {
			parentCopy := parent
			sessions[i].ParentUUID = &parentCopy
		}
	}
	return normalizeStrikeSessions(sessions)
}

// LookupSessionSources finds the transcript that can hold id BY NAME. Strike's
// writer places every session directly in its source root as <id>.jsonl, so the
// lookup tries exactly {root}/{id}.jsonl under each configured root and nothing
// else: one stat per root, never a directory read, never a walk.
//
// A failed search is recorded ONCE, by the core: on a returned error the core
// appends the SearchFailed entry and carries the miss, so this method never
// appends one itself. It returns the entries it gathered for the roots it
// completed, so the miss still names where else the id was searched. Every
// adapter's lookup obeys the same convention.
func (a *StrikeAdapter) LookupSessionSources(ctx context.Context, cfg SourceConfig, id SessionID) (SourceLookup, error) {
	if err := ctx.Err(); err != nil {
		return SourceLookup{}, err
	}
	if _, err := NewSessionID(string(id)); err != nil {
		return SourceLookup{}, fmt.Errorf("ingest: StrikeAdapter.LookupSessionSources was given %q, which is not a session id; no Strike source can be searched under a name no session can hold; where: ingest.StrikeAdapter.LookupSessionSources in internal/ingest/strike.go; when: locating a named session by name; impact: no Strike source is searched; fix: pass the session id the user named: %w", id, err)
	}

	var lookup SourceLookup
	listed := make(map[ResolvedPath]struct{}, len(cfg.Paths))
	for _, basePath := range cfg.Paths {
		if _, duplicate := listed[basePath]; duplicate {
			// One root named twice would claim the same transcript twice, read
			// downstream as a duplicate session, and report the same search
			// twice.
			continue
		}
		listed[basePath] = struct{}{}
		if err := ctx.Err(); err != nil {
			return lookup, err
		}
		candidate := filepath.Join(string(basePath), string(id)+defaults.ExtJSONL.String())
		hit, err := a.strikeSourceExists(candidate)
		if err != nil {
			return lookup, strikeLookupFailure(id, "stat", candidate, err)
		}
		if hit {
			resolved, err := NewResolvedPath(candidate)
			if err != nil {
				return lookup, strikeLookupFailure(id, "resolve", candidate, err)
			}
			lookup.Sources = append(lookup.Sources, resolved)
		}
		lookup.Searched = append(lookup.Searched, SearchedSource{
			Harness:  HarnessStrike,
			Method:   SearchNamePattern,
			Location: strikeSearchLocation(basePath, id),
		})
	}
	return lookup, nil
}

// strikeSearchLocation is the pattern one configured root was searched with,
// for the miss error's searched block: the exact file the writer lays a session
// down as. The block composes one line per source from this value alone, so it
// carries no resolved folder or file that happens to exist.
func strikeSearchLocation(root ResolvedPath, id SessionID) string {
	return filepath.Join(strikeDisplayPath(root), string(id)+defaults.ExtJSONL.String())
}

// strikeLookupFailure is the one statement of "the by-name lookup could not
// cover this source". The core records the failure from the returned error, and
// the detail says why the id cannot be reported absent from this source.
func strikeLookupFailure(id SessionID, action, path string, err error) error {
	return fmt.Errorf("ingest: the Strike lookup for session %s could not %s %q; the search stopped before this source was fully covered, so the id cannot be reported absent from it; where: ingest.StrikeAdapter.LookupSessionSources in internal/ingest/strike.go; when: locating the named session by name; impact: this source was not fully searched, and the miss text names the failure; fix: make the path readable and retry: %w", id, action, path, err)
}

// strikeSourceExists reports whether path is a readable file. A missing path is
// not a failure: it is simply not a hit.
func (a *StrikeAdapter) strikeSourceExists(path string) (bool, error) {
	info, err := a.fs.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return !info.IsDir(), nil
}

// DiscoverSessionAt builds the members for one located Strike source: the
// source itself, the children its own stream names, and the stored children the
// core passed in. It reads each transcript once (the located source's stream is
// read to learn its children, and the build reuses that read) and writes
// nothing, so a refused run has nothing to undo.
//
// Children are found by name beside the located transcript because the writer
// places every session directly in its source root. A parent is never read
// here: the sidecar names it, and the returned AncestorProbe is a pure value the
// core executes. The role and the outcome of every member are the core's, so
// the adapter leaves both at their zero value.
func (a *StrikeAdapter) DiscoverSessionAt(ctx context.Context, cfg SourceConfig, at LocatedSource) (LocatedBuild, error) {
	if err := ctx.Err(); err != nil {
		return LocatedBuild{}, err
	}
	if at.ID == "" {
		return LocatedBuild{}, fmt.Errorf("ingest: StrikeAdapter.DiscoverSessionAt was given a located source with no session id at %q; a located source must name the session it holds so the member can be matched to its store row; where: ingest.StrikeAdapter.DiscoverSessionAt in internal/ingest/strike.go; when: building a located source; impact: nothing was read and no member was built; fix: pass the id the store or the lookup resolved", at.Path)
	}
	if nameID, ok := strikeSessionIDFromPath(at.Path.String()); !ok || nameID != at.ID {
		return LocatedBuild{}, strikeSourceGoneError(at.Path, at.ID, "the file name is not <session-id>.jsonl or names another session")
	}
	root, ok := strikeEnumeratedRoot(cfg, at.Path)
	if !ok {
		return LocatedBuild{}, fmt.Errorf("ingest: the located Strike path %q lies outside every configured Strike source root; a located source must be attributed to the root the full walk would have found it under, or the member would carry a different OriginalRoot than discovery gives it; where: ingest.StrikeAdapter.DiscoverSessionAt in internal/ingest/strike.go; when: resolving the source root of the located path; impact: nothing was read and no member was built; fix: restore the transcript under a configured Strike source path, or add the path's root to the configured source paths", at.Path)
	}
	info, err := a.fs.Stat(at.Path.String())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return LocatedBuild{}, strikeSourceGoneError(at.Path, at.ID, "the file no longer exists")
		}
		return LocatedBuild{}, fmt.Errorf("ingest: the Strike transcript %q for session %s could not be re-read; the path exists but its state cannot be observed, so the member cannot be built from it; where: ingest.StrikeAdapter.DiscoverSessionAt in internal/ingest/strike.go; when: re-reading the path a previous discovery recorded; impact: no member was built from this path, and the core records the read failure; fix: make the file readable and retry: %w", at.Path, at.ID, err)
	}
	if info.IsDir() {
		return LocatedBuild{}, strikeSourceGoneError(at.Path, at.ID, "the path is a directory, not a transcript file")
	}
	located, err := a.loadStrikeSource(strikeSource{root: root, id: at.ID, path: at.Path.String(), info: info})
	if err != nil {
		return LocatedBuild{}, strikeSourceGoneError(at.Path, at.ID, "the transcript could not be read again")
	}
	sources := []strikeSource{located}
	named, err := a.enumerateStrikeNamedChildren(located)
	if err != nil {
		return LocatedBuild{}, err
	}
	sources = append(sources, named...)
	unreadable := a.appendStrikeStoredChildren(cfg, &sources, at)

	sessions := a.buildStrikeSessions(sources)
	byID := make(map[SessionID]int, len(sessions))
	for index := range sessions {
		if _, duplicate := byID[sessions[index].SessionID]; !duplicate {
			byID[sessions[index].SessionID] = index
		}
	}
	if _, ok := byID[at.ID]; !ok {
		return LocatedBuild{}, strikeSourceGoneError(at.Path, at.ID, "the transcript could not be built from its own stream")
	}

	members := make([]LocatedMember, 0, len(sources)+len(unreadable))
	emitted := make(map[SessionID]struct{}, len(sources))
	for _, source := range sources {
		if _, duplicate := emitted[source.id]; duplicate {
			continue
		}
		index, built := byID[source.id]
		if !built {
			continue
		}
		emitted[source.id] = struct{}{}
		member := LocatedMember{ID: source.id, Harness: HarnessStrike, Source: &sessions[index]}
		if source.id == at.ID && at.Stored.Parent == nil {
			member.ParentProbe = strikeAncestorProbe(located)
		}
		members = append(members, member)
	}
	members = append(members, unreadable...)
	return LocatedBuild{Members: members, Deferred: NoDeferredWrites{}}, nil
}

// enumerateStrikeNamedChildren reads the child ids the located transcript's own
// stream names (child.* events) and stats each child's transcript beside it. The
// writer places every session directly in its source root, so a child's file is
// <dir of the located transcript>/<child>.jsonl; a named child whose file is not
// there is not a session yet. No directory is listed.
func (a *StrikeAdapter) enumerateStrikeNamedChildren(located strikeSource) ([]strikeSource, error) {
	if len(located.hints.children) == 0 {
		return nil, nil
	}
	dir := filepath.Dir(located.path)
	sources := make([]strikeSource, 0, len(located.hints.children))
	for _, child := range located.hints.children {
		candidate := filepath.Join(dir, string(child)+defaults.ExtJSONL.String())
		info, err := a.fs.Stat(candidate)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("ingest: the Strike child transcript %q named by session %s could not be observed; a child whose state cannot be read might exist, so the build cannot claim this session has none; where: ingest.StrikeAdapter.DiscoverSessionAt in internal/ingest/strike.go; when: enumerating a located source's named children; impact: nothing was built for this located source; fix: make the path readable and retry: %w", candidate, located.id, err)
		}
		if info.IsDir() {
			continue
		}
		sources = append(sources, strikeSource{root: located.root, id: child, path: candidate, info: info})
	}
	return sources, nil
}

// appendStrikeStoredChildren adds the stored children the build must re-read. A
// child whose file is gone, belongs to another harness, or cannot be observed
// comes back as a member with no source and its reason, because the store still
// records it and the run's upkeep set must see it.
func (a *StrikeAdapter) appendStrikeStoredChildren(cfg SourceConfig, sources *[]strikeSource, at LocatedSource) []LocatedMember {
	seen := make(map[SessionID]struct{}, len(*sources))
	for _, source := range *sources {
		seen[source.id] = struct{}{}
	}
	var unreadable []LocatedMember
	for _, child := range at.Stored.Children {
		if _, duplicate := seen[child.ID]; duplicate {
			continue
		}
		seen[child.ID] = struct{}{}
		member, source, ok := a.buildStrikeStoredChild(cfg, child)
		if ok {
			*sources = append(*sources, source)
			continue
		}
		unreadable = append(unreadable, member)
	}
	return unreadable
}

// buildStrikeStoredChild resolves one stored child either to a source the shared
// build reads or to an unreadable member that names why. A readable child is
// built from its file, exactly as the full walk builds it, so its OriginalRoot,
// title and parent come from the source and not from the store's link.
func (a *StrikeAdapter) buildStrikeStoredChild(cfg SourceConfig, child StoredSessionSource) (LocatedMember, strikeSource, bool) {
	member := LocatedMember{ID: child.ID, Harness: child.Harness}
	if child.Harness != HarnessStrike {
		// Another harness owns this source, or none was recorded. That is the
		// cross-harness read failure UnreadableSourceFailed states; the source is
		// not gone.
		member.Unreadable = UnreadableSourceFailed
		if child.Harness == "" {
			member.Detail = "the store records no harness for this child, so the strike adapter cannot attribute its source"
		} else {
			member.Detail = "a " + string(child.Harness) + " source cannot be read by the strike adapter"
		}
		return member, strikeSource{}, false
	}
	nameID, ok := strikeSessionIDFromPath(child.Path.String())
	if !ok || nameID != child.ID {
		member.Unreadable = UnreadableSourceGone
		return member, strikeSource{}, false
	}
	root, ok := strikeEnumeratedRoot(cfg, child.Path)
	if !ok {
		member.Unreadable = UnreadableSourceFailed
		member.Detail = "the recorded path is outside every configured Strike source root"
		return member, strikeSource{}, false
	}
	info, err := a.fs.Stat(child.Path.String())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			member.Unreadable = UnreadableSourceGone
			return member, strikeSource{}, false
		}
		member.Unreadable = UnreadableSourceFailed
		member.Detail = firstLine(err.Error())
		return member, strikeSource{}, false
	}
	if info.IsDir() {
		member.Unreadable = UnreadableSourceFailed
		member.Detail = "the recorded path is a directory, not a Strike transcript file"
		return member, strikeSource{}, false
	}
	return LocatedMember{}, strikeSource{root: root, id: child.ID, path: child.Path.String(), info: info}, true
}

// strikeAncestorProbe derives one unstored parent's exact source file from the
// child's own sidecar and path. It is a pure value: no stat, no directory read.
func strikeAncestorProbe(located strikeSource) *AncestorProbe {
	parent := strikeParent(located.sidecar)
	if parent == nil || *parent == located.id {
		return nil
	}
	parentPath := filepath.Join(filepath.Dir(located.path), string(*parent)+defaults.ExtJSONL.String())
	path, err := NewResolvedPath(parentPath)
	if err != nil {
		return nil
	}
	return &AncestorProbe{Kind: ProbePath, Harness: HarnessStrike, Path: path}
}

// strikeSourceGoneError is the one statement of "this recorded path no longer
// holds the session it was recorded for". It wraps ErrSourceGone, so the core
// takes the by-name lookup path for the id.
func strikeSourceGoneError(path ResolvedPath, id SessionID, because string) error {
	return fmt.Errorf("ingest: the located Strike path %q no longer holds session %s because %s; a Strike transcript carries its session id in its file name, so this path is not that session's source; where: ingest.StrikeAdapter.DiscoverSessionAt in internal/ingest/strike.go; when: re-reading the path a previous discovery recorded; impact: no member was built from this path; fix: let the by-name lookup find the source the session is in now: %w", path, id, because, ErrSourceGone)
}

// strikeSessionIDFromPath reads the session id a transcript's file name holds.
func strikeSessionIDFromPath(path string) (SessionID, bool) {
	base := filepath.Base(path)
	if !strings.HasSuffix(base, defaults.ExtJSONL.String()) {
		return "", false
	}
	id, err := NewSessionID(strings.TrimSuffix(base, defaults.ExtJSONL.String()))
	if err != nil {
		return "", false
	}
	return id, true
}

// strikeEnumeratedRoot returns the configured source root a located path hangs
// from. Strike's writer lays every session directly in its root, but Discover
// still accepts a transcript at any depth, so the root is the configured one
// that contains the path; that is the only OriginalRoot a full walk records.
func strikeEnumeratedRoot(cfg SourceConfig, path ResolvedPath) (ResolvedPath, bool) {
	for _, configured := range cfg.Paths {
		if strikePathWithin(string(configured), string(path)) {
			return configured, true
		}
	}
	return "", false
}

// strikePathWithin reports whether path sits strictly below root.
func strikePathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// strikeDisplayPath renders a source path with the home directory shortened, the
// way the searched block shows every location.
func strikeDisplayPath(path ResolvedPath) string {
	raw := string(path)
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(raw, home+string(filepath.Separator)) {
		return "~" + strings.TrimPrefix(raw, home)
	}
	return raw
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

func (a *StrikeAdapter) readStrikeTranscriptHints(path string, self SessionID) strikeTranscriptHints {
	data, err := a.fs.ReadFile(path)
	if err != nil {
		return strikeTranscriptHints{}
	}
	var hints strikeTranscriptHints
	seen := make(map[SessionID]bool)
	forEachStrikeRecord(data, func(_ int, raw []byte) {
		if strikeRecordTooLarge(raw, 0) {
			return
		}
		var env strikeEnvelope
		if json.Unmarshal(bytes.TrimSpace(raw), &env) != nil {
			return
		}
		switch env.Type {
		case strikeEventChildStarted, strikeEventChildCompleted, strikeEventChildEscalated:
			event, err := decodeStrikeEvent(env.Type, env.Data)
			if err != nil {
				return
			}
			// The parent engine emits child.* with the child's correlation.
			// A record naming another parent is not this session's child.
			if event.ParentSessionID != "" && event.ParentSessionID != self.String() {
				return
			}
			child, err := NewSessionID(event.SessionID)
			if err != nil || child == self || seen[child] {
				return
			}
			seen[child] = true
			hints.children = append(hints.children, child)
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

	hints := a.readStrikeTranscriptHints(session.SourcePath.String(), session.SessionID)
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
	identityMismatch := false
	callIDs := make(map[string]bool)
	tokensIn, tokensOut := 0, 0
	cacheRead, cacheWrite := 0, 0
	cacheReadKnown, cacheWriteKnown := false, false
	forEachStrikeRecord(data, func(line int, raw []byte) {
		if strikeRecordTooLarge(raw, 0) {
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
		event, dataErr := decodeStrikeEvent(env.Type, env.Data)
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
		// Lifecycle events may carry another session's correlation: child.*
		// names the child and team.roster names the lead.
		if event.SessionID != "" && event.SessionID != session.SessionID.String() && !slices.Contains(strikeLifecycleEventKinds(), env.Type) {
			identityMismatch = true
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
			if n, known := strikeKnownTokenCount(event.CacheRead); known {
				cacheRead += n
				cacheReadKnown = true
			}
			if n, known := strikeKnownTokenCount(event.CacheCreation); known {
				cacheWrite += n
				cacheWriteKnown = true
			}
		}
	})

	if identityMismatch {
		return nil, fmt.Errorf("Strike metadata capture for %s: transcript event sessionId disagrees with the discovered filename; no capture was written; restore the matching transcript and rerun peasant ingest", session.SessionID)
	}
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
	if cacheReadKnown {
		meta.Stats.CachedReadTokens = &cacheRead
	}
	if cacheWriteKnown {
		meta.Stats.CachedWriteTokens = &cacheWrite
	}
	a.applyStrikeLineage(&meta, session, sidecar, sidecarPath)

	worktree := sidecar.worktreePath()
	if worktree == "" {
		worktree = session.CWD
	}
	if worktree == "" {
		worktree = filepath.Dir(session.SourcePath.String())
	}
	// The sidecar records a worktree, not the session's exact working directory.
	meta.CWD = ""
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
	remote, tracking := ResolveGitRemote(ctx, a.git, worktree, branch, sidecar.remoteURL())
	if remote != "" {
		meta.Git.Remote = &remote
	}
	if tracking != "" {
		meta.Git.Tracking = &tracking
	}

	projectHash, hostSlug, identityErr := DeriveProjectIdentifiers(a.salt, remote, worktree)
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

// applyStrikeLineage maps the sidecar team lead and fork source onto the
// existing session graph fields. leadSessionId names the root of a child's
// agent team, so it is the child's root session; Strike roots are their own
// lead and record none. forkedFrom names the session whose history a fork
// copied: the fork stays a root and gains a context_from relationship.
func (a *StrikeAdapter) applyStrikeLineage(meta *UnifiedMetadata, session DiscoveredSession, sidecar strikeSidecar, sidecarPath string) {
	invalid := func(field, raw string, err error) {
		meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "invalid_" + field,
			Location:    sidecarPath,
			Message:     fmt.Sprintf("Strike sidecar %s %q is not a valid session ID: %v; the relationship was not recorded", field, raw, err),
			Remediation: fmt.Sprintf("Correct %s to a valid Strike session ID and rerun peasant ingest.", field),
		})
	}
	if raw := strings.TrimSpace(sidecar.LeadSessionID); raw != "" && session.ParentUUID != nil {
		lead, err := NewSessionID(raw)
		switch {
		case err != nil:
			invalid("leadSessionId", raw, err)
		case lead != session.SessionID:
			meta.RootSessionID = &lead
		}
	}
	if raw := strings.TrimSpace(sidecar.ForkedFrom); raw != "" {
		source, err := NewSessionID(raw)
		switch {
		case err != nil:
			invalid("forkedFrom", raw, err)
		case source != session.SessionID:
			target := source
			meta.Relationships = append(meta.Relationships, schema.SessionRelationship{
				Kind:          schema.SessionRelationshipContextFrom,
				TargetState:   schema.RelationshipTargetKnown,
				TargetLocalID: &target,
				Evidence:      schema.EvidenceNativeTyped,
			})
		}
	}
}
