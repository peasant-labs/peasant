package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
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

				candidates = append(candidates, openCodeSessionCandidate{session: ds, identity: OpenCodeSelectedSourceIdentity{SessionID: sessionID, Representation: OpenCodeRepresentationLegacyJSON, Path: ResolvedPath(sesPath)}, provenance: OpenCodeCandidateLegacyJSONRoot})
			}
		}
	}
	for _, result := range evidence {
		if result.Candidate.Kind != OpenCodeSourceSQLite || result.Support != OpenCodeSupportSupported {
			continue
		}
		candidates = append(candidates, a.discoverSQLiteCandidate(ctx, result)...)
	}
	selected, err := selectCanonicalOpenCodeCandidates(candidates)
	if err != nil {
		return nil, err
	}
	return a.hydrateCanonicalOpenCodeFreshness(ctx, selected), nil
}

// discoverSQLiteCandidate enumerates one supported SQLite candidate. A failure
// is local to that candidate: the failure is recorded on its evidence and the
// other candidates still contribute sessions.
func (a *OpenCodeAdapter) discoverSQLiteCandidate(ctx context.Context, result OpenCodeProbeResult) []openCodeSessionCandidate {
	candidates := make([]openCodeSessionCandidate, 0)
	appendRepresentation := func(sessions []DiscoveredSession, representation OpenCodeCanonicalRepresentation) {
		for _, session := range sessions {
			candidates = append(candidates, openCodeSessionCandidate{
				session:    session,
				identity:   OpenCodeSelectedSourceIdentity{SessionID: session.SessionID, Representation: representation, Path: session.SourcePath},
				provenance: result.Candidate.Provenance,
			})
		}
	}
	switch result.Capability {
	case OpenCodeCapabilityLegacy:
		sessions, err := a.discoverLegacySQLite(ctx, result.Candidate)
		if err != nil {
			a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, "legacy SQLite session enumeration failed", err)
			return nil
		}
		appendRepresentation(sessions, OpenCodeRepresentationLegacySQLite)
	case OpenCodeCapabilityCurrent:
		sessions, err := a.discoverCurrentSQLite(ctx, result.Candidate)
		if err != nil {
			a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, "current SQLite session enumeration failed", err)
			return nil
		}
		appendRepresentation(sessions, OpenCodeRepresentationCurrentSQLite)
	case OpenCodeCapabilityHybrid:
		current, currentErr := a.discoverCurrentSQLite(ctx, result.Candidate)
		legacy, legacyErr := a.discoverLegacySQLite(ctx, result.Candidate)
		if currentErr != nil && legacyErr != nil {
			a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, "hybrid SQLite session enumeration failed", fmt.Errorf("current projection is unusable (%w) and legacy fallback also failed (%v)", currentErr, legacyErr))
			return nil
		}
		if currentErr != nil {
			a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, "hybrid SQLite current projection is unusable; legacy rows were used", currentErr)
		}
		if legacyErr != nil {
			a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, "hybrid SQLite legacy projection is unusable; current rows were used", legacyErr)
		}
		appendRepresentation(current, OpenCodeRepresentationCurrentSQLite)
		appendRepresentation(legacy, OpenCodeRepresentationLegacySQLite)
	default:
		return nil
	}
	records, err := a.discoverSQLiteSessionRecords(ctx, result.Candidate)
	if err != nil {
		a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, "session records could not be read; sessions stay discoverable as roots with file-based freshness", err)
		return candidates
	}
	for index := range candidates {
		record, known := records.bySession[candidates[index].session.SessionID]
		if known && record.parent != "" {
			parentID := record.parent
			candidates[index].session.ParentUUID = &parentID
		}
		candidates[index].sessionClock = records.supported
		if known {
			candidates[index].sessionUpdatedAt = record.updatedAt
		}
	}
	return candidates
}

// openCodeSessionClock is one session row's parent link and update clock.
type openCodeSessionClock struct {
	parent    SessionID
	updatedAt time.Time
}

// openCodeSessionRecords holds every session row of one database. supported
// is false when the database has no usable session table.
type openCodeSessionRecords struct {
	supported bool
	bySession map[SessionID]openCodeSessionClock
}

// discoverSQLiteSessionRecords reads session.parent_id and session.time_updated
// from one supported database. A database without those columns yields an
// unsupported, empty result.
func (a *OpenCodeAdapter) discoverSQLiteSessionRecords(ctx context.Context, candidate OpenCodeCandidate) (openCodeSessionRecords, error) {
	records := openCodeSessionRecords{bySession: make(map[SessionID]openCodeSessionClock)}
	path, err := NewOpenCodeSQLiteSourcePath(candidate.Path)
	if err != nil {
		return records, err
	}
	source, err := a.candidateOpener(ctx, path, a.candidateOptions)
	if err != nil {
		return records, fmt.Errorf("read OpenCode session records from %q failed while opening the restrictive source: %w; sessions remain discoverable as roots; verify source readability and retry without modifying the database", candidate.Path, err)
	}
	defer func() { _ = source.Close(context.Background()) }()
	pageSize, err := NewOpenCodeCurrentPageSize(openCodeCurrentMaterializePage)
	if err != nil {
		return records, err
	}
	var cursor *OpenCodeSessionRecordCursor
	for {
		page, readErr := source.SessionRecords(ctx, OpenCodeSessionRecordPageRequest{PageSize: pageSize, After: cursor})
		if readErr != nil {
			return records, fmt.Errorf("read OpenCode session records from %q failed while enumerating a bounded session page: %w; sessions remain discoverable as roots; verify the session table and retry", candidate.Path, readErr)
		}
		records.supported = page.Supported
		for _, row := range page.Records {
			sessionID, sessionErr := NewSessionID(row.SessionID.String())
			if sessionErr != nil {
				continue
			}
			clock := openCodeSessionClock{}
			if row.TimeUpdated > 0 {
				clock.updatedAt = time.UnixMilli(row.TimeUpdated)
			}
			if row.ParentID.String() != "" {
				if parentID, parentErr := NewSessionID(row.ParentID.String()); parentErr == nil && parentID != sessionID {
					clock.parent = parentID
				}
			}
			records.bySession[sessionID] = clock
		}
		if page.Next == nil {
			break
		}
		cursor = page.Next
	}
	return records, nil
}

// recordCandidateFailure attaches an actionable diagnostic to the failing
// candidate's evidence and logs it. Discovery continues for other candidates.
func (a *OpenCodeAdapter) recordCandidateFailure(path string, stage OpenCodeProbeStage, what string, cause error) {
	diagnostic := OpenCodeProbeDiagnostic{
		Code:        OpenCodeDiagnosticDiscoveryFailed,
		Stage:       stage,
		What:        what,
		Why:         cause.Error(),
		Where:       path,
		When:        "during OpenCode discovery after the candidate probe reported a supported schema",
		Meaning:     "sessions from this candidate were skipped for this run; sessions from other candidates and the legacy JSON layout were still discovered",
		Remediation: "retry after OpenCode finishes writing; if the failure persists, verify the database with OpenCode and do not modify it through Peasant",
	}
	a.candidateMu.Lock()
	cleanPath := filepath.Clean(path)
	for index := range a.candidateEvidence {
		if a.candidateEvidence[index].Candidate.Kind == OpenCodeSourceSQLite && filepath.Clean(a.candidateEvidence[index].Candidate.Path) == cleanPath {
			a.candidateEvidence[index].Diagnostics = append(a.candidateEvidence[index].Diagnostics, diagnostic)
		}
	}
	a.candidateMu.Unlock()
	slog.Warn("opencode discovery: candidate skipped",
		"what", diagnostic.What,
		"why", diagnostic.Why,
		"where", diagnostic.Where,
		"when", diagnostic.When,
		"meaning", diagnostic.Meaning,
		"fix", diagnostic.Remediation,
	)
}

// selectCanonicalOpenCodeCandidates applies representation precedence first,
// then candidate provenance, then normalized and raw attributable-path
// tie-breaks. The winners are ordered parents before children, then by raw
// session ID, so both winner choice and output are independent of enumeration.
func selectCanonicalOpenCodeCandidates(candidates []openCodeSessionCandidate) ([]openCodeSessionCandidate, error) {
	selected := make([]openCodeSessionCandidate, 0, len(candidates))
	positions := make(map[SessionID]int, len(candidates))
	for _, candidate := range candidates {
		if err := candidate.identity.Validate(); err != nil {
			return nil, fmt.Errorf("select canonical OpenCode session failed before freshness diffing: %w; the candidate cannot enter the mounted pipeline; fix candidate construction and retry", err)
		}
		// precedence() ranks an unknown provenance as zero, which would silently
		// misrank a winner, so reject an unknown value at the selection boundary.
		if err := candidate.provenance.Validate(); err != nil {
			return nil, fmt.Errorf("select canonical OpenCode session %q failed before freshness diffing: %w; the candidate cannot enter the mounted pipeline; construct provenance through the resolver and retry", candidate.identity.SessionID, err)
		}
		position, exists := positions[candidate.identity.SessionID]
		if !exists {
			positions[candidate.identity.SessionID] = len(selected)
			selected = append(selected, candidate)
			continue
		}
		if canonicalOpenCodeCandidatePrecedes(candidate, selected[position]) {
			selected[position] = candidate
		}
	}
	orderCanonicalOpenCodeCandidatesParentsFirst(selected)
	return selected, nil
}

// orderCanonicalOpenCodeCandidatesParentsFirst sorts winners so a parent
// always precedes its children, then by raw session ID within one depth.
// OpenCode session IDs are time-descending, so a child can sort before its
// parent by raw ID. The pipeline parent gate admits a subagent only after its
// root passed, so a child emitted first is wrongly marked unchanged in
// selected mode. Depth from the nearest selected ancestor gives a stable
// parents-first order; a child whose parent is not selected is treated as a
// root, and a cyclic link stops at the in-progress ancestor.
func orderCanonicalOpenCodeCandidatesParentsFirst(selected []openCodeSessionCandidate) {
	positions := make(map[SessionID]int, len(selected))
	for index := range selected {
		positions[selected[index].session.SessionID] = index
	}
	depthOf := make(map[SessionID]int, len(selected))
	var depth func(id SessionID) int
	depth = func(id SessionID) int {
		if value, computed := depthOf[id]; computed {
			return value
		}
		// Mark in progress as a root so a cycle terminates.
		depthOf[id] = 0
		candidate := selected[positions[id]]
		result := 0
		if candidate.session.ParentUUID != nil {
			parent := *candidate.session.ParentUUID
			if _, present := positions[parent]; present && parent != id {
				result = depth(parent) + 1
			}
		}
		depthOf[id] = result
		return result
	}
	for index := range selected {
		depth(selected[index].session.SessionID)
	}
	sort.SliceStable(selected, func(left, right int) bool {
		leftID := selected[left].session.SessionID
		rightID := selected[right].session.SessionID
		if depthOf[leftID] != depthOf[rightID] {
			return depthOf[leftID] < depthOf[rightID]
		}
		return string(leftID) < string(rightID)
	})
}

func canonicalOpenCodeCandidatePrecedes(candidate, incumbent openCodeSessionCandidate) bool {
	candidateRank := candidate.identity.Representation.precedence()
	incumbentRank := incumbent.identity.Representation.precedence()
	if candidateRank != incumbentRank {
		return candidateRank > incumbentRank
	}
	candidateProvenance := candidate.provenance.precedence()
	incumbentProvenance := incumbent.provenance.precedence()
	if candidateProvenance != incumbentProvenance {
		return candidateProvenance > incumbentProvenance
	}
	candidateClean := filepath.Clean(candidate.identity.Path.String())
	incumbentClean := filepath.Clean(incumbent.identity.Path.String())
	if candidateClean != incumbentClean {
		return candidateClean < incumbentClean
	}
	return candidate.identity.Path.String() < incumbent.identity.Path.String()
}

// hydrateCanonicalOpenCodeFreshness sets ModTime for every winner. A JSON
// winner uses its file mtime and degrades to zero when stat fails. A SQLite
// winner uses the newest selected row time combined with the upstream session
// clock (session.time_updated), which OpenCode moves on revert and undo, so a
// row deletion still moves freshness without touching sibling sessions. A
// database without that clock falls back to the database and WAL mtime as a
// floor. A SQLite candidate whose freshness cannot be read is skipped and
// recorded; other winners stay.
func (a *OpenCodeAdapter) hydrateCanonicalOpenCodeFreshness(ctx context.Context, selected []openCodeSessionCandidate) []DiscoveredSession {
	bySQLitePath := make(map[string][]int)
	keep := make([]bool, len(selected))
	for index := range selected {
		candidate := &selected[index]
		if candidate.identity.Representation == OpenCodeRepresentationLegacyJSON {
			keep[index] = true
			if info, err := a.fs.Stat(candidate.identity.Path.String()); err == nil {
				candidate.session.ModTime = info.ModTime()
			}
			continue
		}
		path := filepath.Clean(candidate.identity.Path.String())
		bySQLitePath[path] = append(bySQLitePath[path], index)
	}

	paths := make([]string, 0, len(bySQLitePath))
	for path := range bySQLitePath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, rawPath := range paths {
		path, err := NewOpenCodeSQLiteSourcePath(rawPath)
		if err != nil {
			a.recordCandidateFailure(rawPath, OpenCodeProbeFreshness, "selected SQLite path is not a valid source path", err)
			continue
		}
		source, err := a.candidateOpener(ctx, path, a.candidateOptions)
		if err != nil {
			a.recordCandidateFailure(rawPath, OpenCodeProbeFreshness, "selected SQLite source could not be opened read-only for freshness", err)
			continue
		}
		var floor time.Time
		floorKnown := false
		for _, index := range bySQLitePath[rawPath] {
			candidate := &selected[index]
			newest, hydrationErr := selectedOpenCodeSQLiteFreshness(ctx, source, candidate.identity)
			if hydrationErr != nil {
				a.recordCandidateFailure(rawPath, OpenCodeProbeFreshness, fmt.Sprintf("selected freshness for session %q could not be read; that session was skipped", candidate.identity.SessionID), hydrationErr)
				continue
			}
			if candidate.sessionClock {
				if candidate.sessionUpdatedAt.After(newest) {
					newest = candidate.sessionUpdatedAt
				}
			} else {
				if !floorKnown {
					floor, err = sqliteContentModTime(a.fs, rawPath)
					if err != nil {
						a.recordCandidateFailure(rawPath, OpenCodeProbeFreshness, "selected SQLite content freshness could not be read", err)
						break
					}
					floorKnown = true
				}
				if newest.Before(floor) {
					newest = floor
				}
			}
			candidate.session.ModTime = newest
			keep[index] = true
		}
		if closeErr := source.Close(ctx); closeErr != nil {
			a.recordCandidateFailure(rawPath, OpenCodeProbeFreshness, "selected SQLite source did not close cleanly after freshness reads", closeErr)
		}
	}

	discovered := make([]DiscoveredSession, 0, len(selected))
	for index, candidate := range selected {
		if keep[index] {
			discovered = append(discovered, candidate.session)
		}
	}
	return discovered
}

func selectedOpenCodeSQLiteFreshness(ctx context.Context, source OpenCodeSQLiteSource, identity OpenCodeSelectedSourceIdentity) (time.Time, error) {
	switch identity.Representation {
	case OpenCodeRepresentationCurrentSQLite:
		id, err := NewOpenCodeCurrentSessionID(string(identity.SessionID))
		if err != nil {
			return time.Time{}, err
		}
		return source.CurrentSessionFreshness(ctx, id)
	case OpenCodeRepresentationLegacySQLite:
		id, err := NewOpenCodeLegacySessionID(string(identity.SessionID))
		if err != nil {
			return time.Time{}, err
		}
		return source.LegacySessionFreshness(ctx, id)
	default:
		return time.Time{}, fmt.Errorf("selected representation %d cannot use SQLite freshness", identity.Representation)
	}
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
