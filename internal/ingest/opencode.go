package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/salt"
)

// OpenCodeAdapter discovers every supported materialized representation and
// selects one canonical source per raw OpenCode session ID.
type OpenCodeAdapter struct {
	fs                   FileSystem
	git                  GitResolver
	salt                 salt.Salt
	candidateChannel     string
	candidateEnvironment OpenCodeEnvironmentLookup
	candidateProber      *OpenCodeCandidateProber
	candidateOpener      OpenCodeSQLiteSourceOpener
	candidateOptions     OpenCodeSQLiteSourceOptions
	candidateInitErr     error
	candidateMu          sync.Mutex
	candidateEvidence    []OpenCodeProbeResult
}

var _ SourceAdapter = (*OpenCodeAdapter)(nil)
var _ TranscriptMaterializer = (*OpenCodeAdapter)(nil)

// NewOpenCodeAdapter constructs an OpenCodeAdapter with injected dependencies.
func NewOpenCodeAdapter(fs FileSystem, git GitResolver, s salt.Salt) *OpenCodeAdapter {
	candidateFS, ok := fs.(OpenCodeCandidateFileSystem)
	environment := SystemOpenCodeEnvironment()
	channel := "latest"
	if configuredChannel, exists := environment.LookupEnv(openCodeInstallationChannelEnv); exists && strings.TrimSpace(configuredChannel) != "" {
		channel = configuredChannel
	}
	return newOpenCodeAdapter(fs, git, s, candidateFS, ok, channel, environment, OpenOpenCodeSQLiteSource, DefaultOpenCodeSQLiteSourceOptions())
}

func newOpenCodeAdapter(
	fs FileSystem,
	git GitResolver,
	s salt.Salt,
	candidateFS OpenCodeCandidateFileSystem,
	hasCandidateCapability bool,
	channel string,
	environment OpenCodeEnvironmentLookup,
	opener OpenCodeSQLiteSourceOpener,
	options OpenCodeSQLiteSourceOptions,
) *OpenCodeAdapter {
	adapter := &OpenCodeAdapter{fs: fs, git: git, salt: s}
	if !hasCandidateCapability {
		return adapter
	}
	configured, err := NewOpenCodeAdapterWithCandidateProbe(
		fs,
		git,
		s,
		channel,
		environment,
		candidateFS,
		opener,
		options,
	)
	if err != nil {
		adapter.candidateInitErr = fmt.Errorf("OpenCode discovery cannot initialize SQLite candidate support: candidate-probe construction failed because %w; where: NewOpenCodeAdapter before discovery; impact: legacy JSON and SQLite discovery are stopped rather than silently omitting SQLite sessions; fix: verify the OpenCode adapter dependencies and retry", err)
		return adapter
	}
	return configured
}

// NewOpenCodeAdapterWithCandidateProbe constructs the production adapter with
// explicit candidate-resolution dependencies.
func NewOpenCodeAdapterWithCandidateProbe(
	fs FileSystem,
	git GitResolver,
	s salt.Salt,
	channel string,
	environment OpenCodeEnvironmentLookup,
	candidateFS OpenCodeCandidateFileSystem,
	opener OpenCodeSQLiteSourceOpener,
	options OpenCodeSQLiteSourceOptions,
) (*OpenCodeAdapter, error) {
	if strings.TrimSpace(channel) == "" {
		return nil, fmt.Errorf("construct OpenCode adapter candidate probe failed before discovery: installation channel is empty, so database naming cannot match upstream; no source was accessed; inject the compiled OpenCode channel")
	}
	if environment == nil {
		return nil, fmt.Errorf("construct OpenCode adapter candidate probe failed before discovery: environment lookup is nil, so override and channel-disable behavior cannot be resolved; no source was accessed; inject an environment lookup")
	}
	prober, err := NewOpenCodeCandidateProber(candidateFS, opener, options)
	if err != nil {
		return nil, err
	}
	return &OpenCodeAdapter{
		fs:                   fs,
		git:                  git,
		salt:                 s,
		candidateChannel:     channel,
		candidateEnvironment: environment,
		candidateProber:      prober,
		candidateOpener:      opener,
		candidateOptions:     options,
	}, nil
}

// Harness returns HarnessOpenCode.
func (a *OpenCodeAdapter) Harness() Harness {
	return HarnessOpenCode
}

// Discover enumerates all supported representations, deduplicates by raw
// OpenCode session ID, and selects current SQLite, then legacy SQLite, then
// legacy JSON. Transcript streams are never unioned or interleaved.
//
// For each path in cfg.Paths it expects an OpenCode storage layout:
//
//	{path}/project/{hash}.json
//	{path}/session/{hash}/ses_{id}.json
//
// Sessions with a non-empty parentID are linked via ParentUUID.
func (a *OpenCodeAdapter) Discover(ctx context.Context, cfg SourceConfig) ([]DiscoveredSession, error) {
	if a.candidateInitErr != nil {
		return nil, a.candidateInitErr
	}
	evidence := a.inspectCandidates(ctx, cfg.Paths)
	candidates := make([]openCodeSessionCandidate, 0)

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

				candidates = append(candidates, openCodeSessionCandidate{session: ds, identity: OpenCodeSelectedSourceIdentity{SessionID: sessionID, Representation: OpenCodeRepresentationLegacyJSON, Path: ResolvedPath(sesPath)}})
			}
		}
	}
	for _, result := range evidence {
		if result.Candidate.Kind != OpenCodeSourceSQLite || result.Support != OpenCodeSupportSupported {
			continue
		}
		appendRepresentation := func(sessions []DiscoveredSession, representation OpenCodeCanonicalRepresentation) {
			for _, session := range sessions {
				candidates = append(candidates, openCodeSessionCandidate{session: session, identity: OpenCodeSelectedSourceIdentity{SessionID: session.SessionID, Representation: representation, Path: session.SourcePath}})
			}
		}
		switch result.Capability {
		case OpenCodeCapabilityLegacy:
			sessions, err := a.discoverLegacySQLite(ctx, result.Candidate)
			if err != nil {
				return nil, err
			}
			appendRepresentation(sessions, OpenCodeRepresentationLegacySQLite)
		case OpenCodeCapabilityCurrent:
			sessions, err := a.discoverCurrentSQLite(ctx, result.Candidate)
			if err != nil {
				return nil, err
			}
			appendRepresentation(sessions, OpenCodeRepresentationCurrentSQLite)
		case OpenCodeCapabilityHybrid:
			current, currentErr := a.discoverCurrentSQLite(ctx, result.Candidate)
			legacy, legacyErr := a.discoverLegacySQLite(ctx, result.Candidate)
			switch {
			case currentErr != nil && legacyErr != nil:
				return nil, fmt.Errorf("discover hybrid OpenCode SQLite candidate %q failed: current projection is unusable (%w) and legacy fallback also failed (%v); no partial discovery result is eligible; verify the supported OpenCode database and retry without modifying it", result.Candidate.Path, currentErr, legacyErr)
			case currentErr != nil:
				appendRepresentation(legacy, OpenCodeRepresentationLegacySQLite)
			case legacyErr != nil:
				appendRepresentation(current, OpenCodeRepresentationCurrentSQLite)
			default:
				appendRepresentation(current, OpenCodeRepresentationCurrentSQLite)
				appendRepresentation(legacy, OpenCodeRepresentationLegacySQLite)
			}
		default:
			continue
		}
	}
	return selectCanonicalOpenCodeSessions(candidates)
}

func selectCanonicalOpenCodeSessions(candidates []openCodeSessionCandidate) ([]DiscoveredSession, error) {
	selected := make([]openCodeSessionCandidate, 0, len(candidates))
	positions := make(map[SessionID]int, len(candidates))
	for _, candidate := range candidates {
		if err := candidate.identity.Validate(); err != nil {
			return nil, fmt.Errorf("select canonical OpenCode session failed before freshness diffing: %w; the candidate cannot enter the mounted pipeline; fix candidate construction and retry", err)
		}
		position, exists := positions[candidate.identity.SessionID]
		if !exists {
			positions[candidate.identity.SessionID] = len(selected)
			selected = append(selected, candidate)
			continue
		}
		if candidate.identity.Representation.precedence() > selected[position].identity.Representation.precedence() {
			selected[position] = candidate
		}
	}
	discovered := make([]DiscoveredSession, len(selected))
	for index, candidate := range selected {
		discovered[index] = candidate.session
	}
	return discovered, nil
}

// CandidateEvidence returns a detached snapshot from the most recent discovery.
// The evidence is diagnostic only and carries no ingestion eligibility.
func (a *OpenCodeAdapter) CandidateEvidence() []OpenCodeProbeResult {
	a.candidateMu.Lock()
	defer a.candidateMu.Unlock()
	return cloneOpenCodeProbeResults(a.candidateEvidence)
}

func (a *OpenCodeAdapter) inspectCandidates(ctx context.Context, roots []ResolvedPath) []OpenCodeProbeResult {
	if a.candidateProber == nil {
		return nil
	}
	candidates := make([]OpenCodeCandidate, 0, len(roots)*3)
	seen := make(map[string]struct{}, len(roots)*3)
	for _, root := range roots {
		resolved, err := ResolveOpenCodeCandidates(root.String(), a.candidateChannel, a.candidateEnvironment)
		if err != nil {
			continue
		}
		for _, candidate := range resolved {
			key := string(candidate.Kind) + "\x00" + filepath.Clean(candidate.Path)
			if candidate.Path == ":memory:" {
				key = string(candidate.Kind) + "\x00:memory:"
			}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			candidates = append(candidates, candidate)
		}
	}
	evidence := a.candidateProber.Probe(ctx, candidates)
	a.candidateMu.Lock()
	a.candidateEvidence = cloneOpenCodeProbeResults(evidence)
	a.candidateMu.Unlock()
	return cloneOpenCodeProbeResults(evidence)
}

func cloneOpenCodeProbeResults(results []OpenCodeProbeResult) []OpenCodeProbeResult {
	cloned := make([]OpenCodeProbeResult, len(results))
	for index, result := range results {
		cloned[index] = result
		cloned[index].Diagnostics = append([]OpenCodeProbeDiagnostic(nil), result.Diagnostics...)
		cloned[index].Evidence.Tables = append([]string(nil), result.Evidence.Tables...)
		cloned[index].Evidence.LegacyMessageColumns = append([]OpenCodeColumnEvidence(nil), result.Evidence.LegacyMessageColumns...)
		cloned[index].Evidence.LegacyPartColumns = append([]OpenCodeColumnEvidence(nil), result.Evidence.LegacyPartColumns...)
		cloned[index].Evidence.CurrentMessageColumns = append([]OpenCodeColumnEvidence(nil), result.Evidence.CurrentMessageColumns...)
		cloned[index].Evidence.CurrentIndexes = make([]OpenCodeIndexEvidence, len(result.Evidence.CurrentIndexes))
		for evidenceIndex, indexEvidence := range result.Evidence.CurrentIndexes {
			cloned[index].Evidence.CurrentIndexes[evidenceIndex] = indexEvidence
			cloned[index].Evidence.CurrentIndexes[evidenceIndex].Keys = append([]OpenCodeIndexKeyEvidence(nil), indexEvidence.Keys...)
		}
	}
	return cloned
}

// ExtractMetadata builds UnifiedMetadata from session + message files.
//
// It reads:
//  1. The session JSON at session.SourcePath for id, version, projectID, directory, parentID, time.
//  2. All message files under {root}/message/ses_{id}/ for turn and tool-call counts.
//  3. The corresponding project JSON for the worktree path.
//  4. Git metadata from the session's working directory.
func (a *OpenCodeAdapter) ExtractMetadata(ctx context.Context, session DiscoveredSession) (*UnifiedMetadata, error) {
	if session.TranscriptOrigin == TranscriptOriginOpenCodeLegacySQLite || session.TranscriptOrigin == TranscriptOriginOpenCodeCurrentSQLite {
		metadata, _, err := a.MaterializeTranscript(ctx, session)
		return metadata, err
	}
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
	semanticMessages := loadOpenCodeJSONSemanticMessages(a.fs, session)
	semanticSummary := summarizeOpenCodeSemanticMessages(semanticMessages)

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
	modelIDStr := semanticSummary.modelID

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
		TurnCount:     semanticSummary.turnCount,
		ToolCallCount: semanticSummary.toolCallCount,
		DurationMs:    durationMs,
		TokensIn:      semanticSummary.tokensIn,
		TokensOut:     semanticSummary.tokensOut,
	}

	if modelMissing {
		md.Diagnostics.Warnings = append(md.Diagnostics.Warnings, DiagnosticEntry{
			ErrorType:   "missing_model",
			Location:    fmt.Sprintf("session %s", ses.ID),
			Message:     "no model information found in session messages",
			Remediation: "Check if the OpenCode session has assistant messages with model information.",
		})
	}
	md.Diagnostics.Warnings = append(md.Diagnostics.Warnings, missingOpenCodeParentDiagnostics(session, semanticMessages)...)

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
