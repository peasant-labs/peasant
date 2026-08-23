package ingest

import (
	"context"
	"encoding/json"
	"errors"
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
	// Each supported SQLite candidate is opened once here and the one source is
	// reused for its enumerations, its session-record read, and freshness
	// hydration, so a single Discover never opens the same database repeatedly
	// and enumeration, records, and freshness observe one consistent snapshot.
	// The sources stay open until hydration finishes and are then closed once.
	sqliteSources := make(map[string]OpenCodeSQLiteSource)
	defer func() {
		for path, source := range sqliteSources {
			if closeErr := closeDeferredOpenCodeSource(ctx, source, a.candidateOptions.queryTimeout); closeErr != nil {
				a.recordCandidateFailure(path, OpenCodeProbeFreshness, "selected SQLite source did not close cleanly after discovery and freshness reads", closeErr)
			}
		}
	}()
	for _, result := range evidence {
		if result.Candidate.Kind != OpenCodeSourceSQLite || result.Support != OpenCodeSupportSupported {
			continue
		}
		sqliteCandidates, source := a.discoverSQLiteCandidate(ctx, result)
		candidates = append(candidates, sqliteCandidates...)
		if source != nil {
			sqliteSources[filepath.Clean(result.Candidate.Path)] = source
		}
	}
	selected, err := selectCanonicalOpenCodeCandidates(candidates)
	if err != nil {
		return nil, err
	}
	return a.hydrateCanonicalOpenCodeFreshness(ctx, selected, sqliteSources), nil
}

// closeDeferredOpenCodeSource closes a source that stayed open for the whole
// Discover. It derives the close context from context.Background rather than the
// caller context, so an already-cancelled discovery context cannot make Close
// take its cancellation branch and skip releasing the single connection. That
// skip would leak the connection in the long-lived web process. The close stays
// bounded by the source query timeout, so a genuinely stuck cleanup still ends.
// The caller context is intentionally not passed through.
func closeDeferredOpenCodeSource(callerCtx context.Context, source OpenCodeSQLiteSource, bound time.Duration) error {
	_ = callerCtx
	if bound <= 0 {
		bound = defaultOpenCodeSQLiteQueryTimeout
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	return source.Close(closeCtx)
}

// openOpenCodeSQLiteSource opens one restrictive read-only source for a
// candidate path. It centralizes the nil-opener guard and the source path
// validation that every open site needs, so an open never runs without the
// injected opener.
func (a *OpenCodeAdapter) openOpenCodeSQLiteSource(ctx context.Context, candidatePath string) (OpenCodeSQLiteSource, error) {
	if a.candidateOpener == nil {
		return nil, fmt.Errorf("open OpenCode SQLite source for %q failed before source access: source opener is nil, so typed reads cannot run; no database was accessed; construct the adapter with OpenOpenCodeSQLiteSource", candidatePath)
	}
	path, err := NewOpenCodeSQLiteSourcePath(candidatePath)
	if err != nil {
		return nil, err
	}
	source, err := a.candidateOpener(ctx, path, a.candidateOptions)
	if err != nil {
		return nil, fmt.Errorf("open OpenCode SQLite source %q failed while opening the restrictive read-only source: %w; no session was exposed; verify source readability and retry without modifying the database", candidatePath, err)
	}
	return source, nil
}

// withOpenCodeSQLiteSource opens one restrictive source for a candidate path,
// runs fn against it, and closes it once. It is the scoped open site used where
// the source does not need to outlive the call.
func (a *OpenCodeAdapter) withOpenCodeSQLiteSource(ctx context.Context, candidatePath string, fn func(OpenCodeSQLiteSource) error) error {
	source, err := a.openOpenCodeSQLiteSource(ctx, candidatePath)
	if err != nil {
		return err
	}
	fnErr := fn(source)
	closeErr := source.Close(ctx)
	return errors.Join(fnErr, closeErr)
}

// discoverSQLiteCandidate enumerates one supported SQLite candidate against a
// single read-only source that it opens once. The one source is returned to the
// caller so freshness hydration can reuse it and close it once, which removes
// the snapshot skew between enumeration, records, and freshness. A failure is
// local to that candidate: the failure is recorded on its evidence and the
// other candidates still contribute sessions. The returned source is nil only
// when the database could not be opened.
func (a *OpenCodeAdapter) discoverSQLiteCandidate(ctx context.Context, result OpenCodeProbeResult) ([]openCodeSessionCandidate, OpenCodeSQLiteSource) {
	source, err := a.openOpenCodeSQLiteSource(ctx, result.Candidate.Path)
	if err != nil {
		a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, "SQLite session enumeration could not open the restrictive source", err)
		return nil, nil
	}
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
		sessions, err := a.discoverLegacySQLite(ctx, source, result.Candidate)
		if err != nil {
			a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, "legacy SQLite session enumeration failed", err)
			return nil, source
		}
		appendRepresentation(sessions, OpenCodeRepresentationLegacySQLite)
	case OpenCodeCapabilityCurrent:
		sessions, err := a.discoverCurrentSQLite(ctx, source, result.Candidate)
		if err != nil {
			a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, "current SQLite session enumeration failed", err)
			return nil, source
		}
		appendRepresentation(sessions, OpenCodeRepresentationCurrentSQLite)
	case OpenCodeCapabilityHybrid:
		// A hybrid database carries both projections, so enumerate both and let
		// canonical selection keep one projection per session. Current outranks
		// legacy, so a session in both is a current winner while a session only
		// in the legacy tables is a legacy winner. A session the current
		// projection dropped but the legacy tables still hold is handled by the
		// session-table deletion rule below, not by hiding the legacy tables.
		current, currentErr := a.discoverCurrentSQLite(ctx, source, result.Candidate)
		legacy, legacyErr := a.discoverLegacySQLite(ctx, source, result.Candidate)
		if currentErr != nil && legacyErr != nil {
			a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, "hybrid SQLite session enumeration failed", fmt.Errorf("current projection is unusable (%w) and legacy fallback also failed (%v)", currentErr, legacyErr))
			return nil, source
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
		return nil, source
	}
	if len(candidates) == 0 {
		// No session was enumerated from this database, so the session table has
		// no parent link or clock to attach. Skip the whole session-table read.
		return candidates, source
	}
	discoveredIDs := make(map[SessionID]struct{}, len(candidates))
	for index := range candidates {
		discoveredIDs[candidates[index].session.SessionID] = struct{}{}
	}
	records, err := a.discoverSQLiteSessionRecords(ctx, source, result.Candidate, discoveredIDs)
	if err != nil {
		a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, "session records could not be read; sessions stay discoverable as roots with file-based freshness", err)
		return candidates, source
	}
	// A session table that carries neither the parent link nor the changed clock
	// supplies nothing, so name it. A table that has only the parent link, or
	// only the clock, is used for whichever it has and is not a failure.
	if records.present && !records.hasParent && !records.hasClock {
		a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, "session table carries neither parent_id nor time_updated, so parent links and the changed clock are unavailable; sessions stay discoverable as roots with file-based freshness", errors.New("session table lacks both parent_id and time_updated"))
	}
	if len(records.skipped) > 0 {
		// One diagnostic names every dropped row, so the good rows keep their
		// parent link and clock while the bad ones are visible.
		a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, fmt.Sprintf("%d session row(s) were dropped while keeping the others: %s", len(records.skipped), strings.Join(records.skipped, "; ")), errors.New("one or more session rows were undecodable"))
	}
	// OpenCode keeps its authoritative session list in the session table; the
	// message and session_message rows are historical. When the session table
	// carries the changed clock, which is the real OpenCode shape, a discovered
	// session with no row there was deleted from OpenCode, so it is skipped
	// rather than resurrected from stale rows. A session table without the clock
	// column is a degraded or synthetic shape, so the rule fails safe and keeps
	// every discovered session as a root rather than risk skipping a live one.
	if records.present && records.hasClock {
		surviving := candidates[:0]
		survivingIDs := make(map[SessionID]struct{}, len(candidates))
		var deleted []string
		for index := range candidates {
			sessionID := candidates[index].session.SessionID
			if _, kept := records.rowIDs[sessionID]; kept {
				surviving = append(surviving, candidates[index])
				survivingIDs[sessionID] = struct{}{}
				continue
			}
			deleted = append(deleted, fmt.Sprintf("session %q has no row in the session table", sessionID))
		}
		candidates = surviving
		// A parent the deletion rule removed can no longer satisfy the parent
		// link, so clear the link and name the child. The child stays a
		// discovered root instead of pointing at a deleted session.
		for index := range candidates {
			record, known := records.bySession[candidates[index].session.SessionID]
			if !known || record.parent == "" {
				continue
			}
			if _, present := survivingIDs[record.parent]; !present {
				records.danglingParents = append(records.danglingParents, fmt.Sprintf("session %q references parent %q, which this run did not discover", candidates[index].session.SessionID, record.parent))
				record.parent = ""
				records.bySession[candidates[index].session.SessionID] = record
			}
		}
		if len(deleted) > 0 {
			a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, fmt.Sprintf("%d session(s) were skipped because they were deleted from OpenCode and have no row in the session table: %s", len(deleted), strings.Join(deleted, "; ")), errors.New("one or more sessions were deleted from the session table"))
		}
	}
	if len(records.danglingParents) > 0 {
		// The children are ingested as roots and the missing links are named, so
		// a parent this run did not discover never silently skips its child.
		a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, fmt.Sprintf("%d session(s) were ingested as roots because their parent was not discovered in this run: %s", len(records.danglingParents), strings.Join(records.danglingParents, "; ")), errors.New("one or more parent sessions were absent from discovery"))
	}
	for index := range candidates {
		record, known := records.bySession[candidates[index].session.SessionID]
		if known && record.parent != "" {
			parentID := record.parent
			candidates[index].session.ParentUUID = &parentID
		}
		// The clock is per session, not per database. A supported database
		// with no row for this session, or a row without a usable
		// time_updated, leaves sessionUpdatedAt zero, so the session takes the
		// mtime-floor path and a row deletion still moves its freshness.
		if known {
			candidates[index].sessionUpdatedAt = record.updatedAt
		}
	}
	return candidates, source
}

// openCodeSessionClock is one session row's parent link and update clock.
type openCodeSessionClock struct {
	parent    SessionID
	updatedAt time.Time
}

// openCodeSessionRecords holds every session row of one database. present is
// false when the database has no session table. hasParent and hasClock report
// which of the parent link and changed clock columns the session table carries,
// so parent links are read whether or not the clock column exists.
type openCodeSessionRecords struct {
	present   bool
	hasParent bool
	hasClock  bool
	bySession map[SessionID]openCodeSessionClock
	// rowIDs names every session that still has a row in the session table,
	// including a row whose parent link or clock could not be decoded. A
	// discovered session missing from rowIDs while the table is present and
	// enumerable was deleted from OpenCode, so it is skipped rather than
	// resurrected from its historical message or session_message rows.
	rowIDs map[SessionID]struct{}
	// skipped names the rows the read could not use, so one bad row is dropped
	// with a diagnostic while the others keep their parent link and clock.
	skipped []string
	// danglingParents names discovered sessions whose parent this run did not
	// discover. The child is ingested as a root and the missing link is named,
	// so a dangling parent_id never skips the child at store time.
	danglingParents []string
}

// discoverSQLiteSessionRecords reads session.parent_id and session.time_updated
// from the already-open source for one supported database. The parent link is
// read whether or not the clock column exists. A database without a session
// table yields an empty result with present false.
func (a *OpenCodeAdapter) discoverSQLiteSessionRecords(ctx context.Context, source OpenCodeSQLiteSource, candidate OpenCodeCandidate, discoveredIDs map[SessionID]struct{}) (openCodeSessionRecords, error) {
	records := openCodeSessionRecords{bySession: make(map[SessionID]openCodeSessionClock, len(discoveredIDs)), rowIDs: make(map[SessionID]struct{}, len(discoveredIDs))}
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
		records.present = page.Supported
		records.hasParent = page.HasParent
		records.hasClock = page.HasClock
		for _, skip := range page.Skipped {
			records.skipped = append(records.skipped, skip.Reason)
		}
		for _, rowID := range page.PresentSessionIDs {
			// A row whose stored identifier is valid marks the session present.
			// An identifier Peasant cannot store cannot match a discovered
			// candidate, so it never affects the deletion decision.
			if sessionID, sessionErr := NewSessionID(rowID.String()); sessionErr == nil {
				records.rowIDs[sessionID] = struct{}{}
			}
		}
		for _, row := range page.Records {
			sessionID, sessionErr := NewSessionID(row.SessionID.String())
			if sessionErr != nil {
				// A source-valid identifier that Peasant cannot store is dropped
				// with a diagnostic rather than a silent continue.
				records.skipped = append(records.skipped, fmt.Sprintf("session row %q was dropped because its identifier cannot be stored: %v", row.SessionID.String(), sessionErr))
				continue
			}
			// Retain only the rows for sessions this database actually discovered,
			// so freshness and parent links are read for the discovered ids only.
			if _, discovered := discoveredIDs[sessionID]; !discovered {
				continue
			}
			clock := openCodeSessionClock{}
			if row.TimeUpdated > 0 {
				clock.updatedAt = time.UnixMilli(row.TimeUpdated)
			}
			if row.ParentID.String() != "" {
				if parentID, parentErr := NewSessionID(row.ParentID.String()); parentErr != nil {
					records.skipped = append(records.skipped, fmt.Sprintf("parent link for session %q was dropped because the parent identifier cannot be stored: %v", sessionID, parentErr))
				} else if parentID != sessionID {
					// The parent link is kept only when this run discovered the
					// parent session. A parent the run never discovered cannot
					// satisfy the sessions.parent_id relationship, so the child is
					// left a root and the missing link is named. The store has no
					// role at discovery, so the discovered set is the only
					// authority here.
					if _, discovered := discoveredIDs[parentID]; discovered {
						clock.parent = parentID
					} else {
						records.danglingParents = append(records.danglingParents, fmt.Sprintf("session %q references parent %q, which this run did not discover", sessionID, parentID))
					}
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

// resolveCandidateEvidenceIndex returns the index of the SQLite evidence entry
// for one path, or -1 when none matches. A caller that records several
// diagnostics for the same path resolves the index once instead of scanning the
// evidence slice for every diagnostic.
func (a *OpenCodeAdapter) resolveCandidateEvidenceIndex(path string) int {
	cleanPath := filepath.Clean(path)
	a.candidateMu.Lock()
	defer a.candidateMu.Unlock()
	for index := range a.candidateEvidence {
		if a.candidateEvidence[index].Candidate.Kind == OpenCodeSourceSQLite && filepath.Clean(a.candidateEvidence[index].Candidate.Path) == cleanPath {
			return index
		}
	}
	return -1
}

// recordCandidateFailure attaches an actionable diagnostic to the failing
// candidate's evidence and logs it. Discovery continues for other candidates.
func (a *OpenCodeAdapter) recordCandidateFailure(path string, stage OpenCodeProbeStage, what string, cause error) {
	a.recordCandidateFailureAt(a.resolveCandidateEvidenceIndex(path), path, stage, what, cause)
}

// recordCandidateFailureAt records the diagnostic against a pre-resolved
// evidence index, so a per-session loop does not rescan the evidence slice for
// every session on one path. A negative index records nothing on the evidence
// and only logs.
func (a *OpenCodeAdapter) recordCandidateFailureAt(evidenceIndex int, path string, stage OpenCodeProbeStage, what string, cause error) {
	diagnostic := actionableOpenCodeDiagnostic(
		stage,
		what,
		cause.Error(),
		path,
		"during OpenCode discovery after the candidate probe reported a supported schema",
		"sessions from this candidate were skipped for this run; sessions from other candidates and the legacy JSON layout were still discovered",
		"retry after OpenCode finishes writing; if the failure persists, verify the database with OpenCode and do not modify it through Peasant",
	)
	if evidenceIndex >= 0 {
		a.candidateMu.Lock()
		if evidenceIndex < len(a.candidateEvidence) {
			a.candidateEvidence[evidenceIndex].Diagnostics = append(a.candidateEvidence[evidenceIndex].Diagnostics, diagnostic)
		}
		a.candidateMu.Unlock()
	}
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
// winner combines the newest selected row time with the session's own clock
// (session.time_updated) when that session has a usable clock. OpenCode moves
// that clock on revert and undo, so a row deletion still moves freshness
// without touching sibling sessions. A session with no usable clock falls back
// to the database and WAL mtime as a floor, so a row deletion is still seen.
//
// The changed clock reports content edits and deletions, not the raw file
// mtime. An in-place row rewrite that moves no time column and no session
// clock therefore is not detected as a change for a session that has a usable
// clock; only the floor path, used when a session has no usable clock, tracks
// the file and WAL mtime. A SQLite candidate whose freshness cannot be read is
// skipped and recorded; other winners stay.
func (a *OpenCodeAdapter) hydrateCanonicalOpenCodeFreshness(ctx context.Context, selected []openCodeSessionCandidate, sources map[string]OpenCodeSQLiteSource) []DiscoveredSession {
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
		// The source was opened once during discovery and reused here, so
		// freshness observes the same snapshot as enumeration and no second open
		// is needed. The caller closes every source exactly once.
		source, opened := sources[rawPath]
		if !opened || source == nil {
			a.recordCandidateFailure(rawPath, OpenCodeProbeFreshness, "selected SQLite source was not available for freshness because discovery did not open it", fmt.Errorf("no open source for %q", rawPath))
			continue
		}
		// Read the newest row time of every session on this database with at
		// most one GROUP BY aggregate per table, so freshness reads stay bounded
		// by table count and never grow with the number of sessions.
		rowFreshness := a.batchOpenCodeSQLiteRowFreshness(ctx, source, selected, bySQLitePath[rawPath])
		// The database and WAL mtime is the active (staleness) time for every
		// session on this database, independent of the changed clock. It is also
		// the freshness floor for a session with no usable clock.
		floor, floorErr := sqliteContentModTime(a.fs, rawPath)
		// Resolve the evidence entry for this path once, so a per-session
		// diagnostic does not rescan the evidence slice for every session.
		evidenceIndex := a.resolveCandidateEvidenceIndex(rawPath)
		// Accumulate the affected sessions per outcome, so a database whose reads
		// all fail records one diagnostic and one warn line per outcome naming the
		// sessions, not one per session.
		var freshnessSkipped, floorFallback, clocklessSkipped []string
		var freshnessCause, floorFallbackCause, clocklessCause error
		for _, index := range bySQLitePath[rawPath] {
			candidate := &selected[index]
			newest, hydrationErr := rowFreshness(candidate.identity)
			if hydrationErr != nil {
				// Canonical selection already discarded the losing
				// representations, so dropping the winner here would drop a
				// session that had a readable representation. Fall back to the
				// database and WAL mtime floor and keep the session. Only skip
				// it when the floor is also unreadable, and name it either way.
				if floorErr != nil {
					freshnessSkipped = append(freshnessSkipped, string(candidate.identity.SessionID))
					freshnessCause = hydrationErr
					continue
				}
				floorFallback = append(floorFallback, string(candidate.identity.SessionID))
				floorFallbackCause = hydrationErr
				candidate.session.ModTime = floor
				candidate.session.ActiveModTime = floor
				keep[index] = true
				continue
			}
			if floorErr == nil {
				candidate.session.ActiveModTime = floor
			}
			if !candidate.sessionUpdatedAt.IsZero() {
				if candidate.sessionUpdatedAt.After(newest) {
					newest = candidate.sessionUpdatedAt
				}
			} else {
				if floorErr != nil {
					// The floor is this one session's freshness. Skip only this
					// session, so the other sessions on the same database are still
					// hydrated.
					clocklessSkipped = append(clocklessSkipped, string(candidate.identity.SessionID))
					clocklessCause = floorErr
					continue
				}
				if newest.Before(floor) {
					newest = floor
				}
			}
			candidate.session.ModTime = newest
			keep[index] = true
		}
		if len(freshnessSkipped) > 0 {
			a.recordCandidateFailureAt(evidenceIndex, rawPath, OpenCodeProbeFreshness, fmt.Sprintf("selected freshness could not be read and the mtime floor was unavailable, so %d session(s) were skipped: %s", len(freshnessSkipped), strings.Join(freshnessSkipped, ", ")), freshnessCause)
		}
		if len(floorFallback) > 0 {
			a.recordCandidateFailureAt(evidenceIndex, rawPath, OpenCodeProbeFreshness, fmt.Sprintf("selected freshness could not be read, so %d session(s) fell back to the database and WAL mtime floor: %s", len(floorFallback), strings.Join(floorFallback, ", ")), floorFallbackCause)
		}
		if len(clocklessSkipped) > 0 {
			a.recordCandidateFailureAt(evidenceIndex, rawPath, OpenCodeProbeFreshness, fmt.Sprintf("selected SQLite content freshness could not be read, so %d clockless session(s) were skipped: %s", len(clocklessSkipped), strings.Join(clocklessSkipped, ", ")), clocklessCause)
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

// batchOpenCodeSQLiteRowFreshness reads the newest row time of every session on
// one database with at most one GROUP BY aggregate per table, then returns a
// lookup that resolves a single winner's row freshness from the shared result.
// Only the tables that a winner actually uses are read, so a database of N
// sessions costs at most one statement per present table, never one per session.
// A read failure for a table is returned to every winner of that
// representation, so a caller can fail closed per session without dropping
// winners of the other representation. A session absent from a table's result
// has no rows there and resolves to the zero time, leaving the caller's clock
// or floor to decide freshness.
func (a *OpenCodeAdapter) batchOpenCodeSQLiteRowFreshness(ctx context.Context, source OpenCodeSQLiteSource, selected []openCodeSessionCandidate, indexes []int) func(OpenCodeSelectedSourceIdentity) (time.Time, error) {
	needLegacy, needCurrent := false, false
	for _, index := range indexes {
		switch selected[index].identity.Representation {
		case OpenCodeRepresentationLegacySQLite:
			needLegacy = true
		case OpenCodeRepresentationCurrentSQLite:
			needCurrent = true
		}
	}
	var legacyBySession, currentBySession map[string]time.Time
	var legacyErr, currentErr error
	if needLegacy {
		legacyBySession, legacyErr = source.LegacyFreshnessBySession(ctx)
	}
	if needCurrent {
		currentBySession, currentErr = source.CurrentFreshnessBySession(ctx)
	}
	return func(identity OpenCodeSelectedSourceIdentity) (time.Time, error) {
		switch identity.Representation {
		case OpenCodeRepresentationCurrentSQLite:
			if currentErr != nil {
				return time.Time{}, currentErr
			}
			return currentBySession[string(identity.SessionID)], nil
		case OpenCodeRepresentationLegacySQLite:
			if legacyErr != nil {
				return time.Time{}, legacyErr
			}
			return legacyBySession[string(identity.SessionID)], nil
		default:
			return time.Time{}, fmt.Errorf("selected representation %d cannot use SQLite freshness", identity.Representation)
		}
	}
}

// CandidateEvidence returns a detached snapshot from the most recent discovery.
// The evidence is diagnostic only and carries no ingestion eligibility.
func (a *OpenCodeAdapter) CandidateEvidence() []OpenCodeProbeResult {
	a.candidateMu.Lock()
	defer a.candidateMu.Unlock()
	return cloneOpenCodeProbeResults(a.candidateEvidence)
}

var _ DiscoveryDiagnosticReporter = (*OpenCodeAdapter)(nil)

// DiscoveryDiagnostics flattens the candidate evidence from the most recent
// discovery into provider-agnostic records for the pipeline result. Only the
// failures that skipped or degraded a candidate are surfaced, so a caller sees
// a database that could not be fully enumerated rather than an unexplained
// short session count. A healthy candidate contributes nothing here.
func (a *OpenCodeAdapter) DiscoveryDiagnostics() []DiscoveryDiagnostic {
	a.candidateMu.Lock()
	defer a.candidateMu.Unlock()
	var diagnostics []DiscoveryDiagnostic
	for _, result := range a.candidateEvidence {
		for _, diagnostic := range result.Diagnostics {
			if !openCodeDiscoveryDiagnosticSurfaces(diagnostic.Code) {
				continue
			}
			diagnostics = append(diagnostics, DiscoveryDiagnostic{
				Provider: HarnessOpenCode,
				Code:     string(diagnostic.Code),
				Location: result.Candidate.Path,
				Summary:  diagnostic.What,
				Detail:   diagnostic.Why,
			})
		}
	}
	return diagnostics
}

// openCodeDiscoveryDiagnosticSurfaces reports whether a probe diagnostic names a
// candidate that failed to contribute its sessions, as opposed to a path that
// was simply not an OpenCode database.
func openCodeDiscoveryDiagnosticSurfaces(code OpenCodeProbeDiagnosticCode) bool {
	switch code {
	case OpenCodeDiagnosticSourceOpenFailed, OpenCodeDiagnosticCatalogReadFailed,
		OpenCodeDiagnosticCatalogTruncated, OpenCodeDiagnosticSchemaIncomplete,
		OpenCodeDiagnosticDiscoveryFailed:
		return true
	default:
		return false
	}
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
