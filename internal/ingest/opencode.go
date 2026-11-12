package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/schema"
)

// OpenCodeAdapter discovers every supported materialized representation and
// selects one canonical source per raw OpenCode session ID.
type OpenCodeAdapter struct {
	discoveryProgressAttachment
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
var _ DiscoveryProgressReporting = (*OpenCodeAdapter)(nil)
var _ TranscriptMaterializer = (*OpenCodeAdapter)(nil)
var _ BoundedTranscriptMaterializer = (*OpenCodeAdapter)(nil)
var _ FirstPageTranscriptMaterializer = (*OpenCodeAdapter)(nil)
var _ SessionLocator = (*OpenCodeAdapter)(nil)

// openCodeAncestorProbeKind is the one ancestor-probe kind this adapter
// declares, and it is the same for both storage formats: one row by id per
// candidate database, zero directory reads. A legacy-JSON child names its
// parent in its own file, so it needs no different probe; only the parent's own
// row can say whether a database holds it. A parent that exists only in legacy
// JSON is therefore not located, because that format cannot be searched by id.
const openCodeAncestorProbeKind = ProbeRow

// OpenCodeAdapterOption configures the production OpenCodeAdapter constructor.
type OpenCodeAdapterOption func(*openCodeAdapterOptions)

type openCodeAdapterOptions struct {
	environment OpenCodeEnvironmentLookup
}

// WithOpenCodeEnvironment overrides the environment lookup the adapter uses to
// resolve the installation channel (OPENCODE_CHANNEL) and the database
// overrides (OPENCODE_DB, OPENCODE_DISABLE_CHANNEL_DB). Production leaves it
// unset so SystemOpenCodeEnvironment stays the default; a test injects a fixed
// lookup instead of mutating the process environment, which would serialize
// every other test in the binary under t.Setenv.
func WithOpenCodeEnvironment(environment OpenCodeEnvironmentLookup) OpenCodeAdapterOption {
	return func(o *openCodeAdapterOptions) {
		if environment != nil {
			o.environment = environment
		}
	}
}

// NewOpenCodeAdapter constructs an OpenCodeAdapter with injected dependencies.
func NewOpenCodeAdapter(fs FileSystem, git GitResolver, s salt.Salt, opts ...OpenCodeAdapterOption) *OpenCodeAdapter {
	options := openCodeAdapterOptions{environment: SystemOpenCodeEnvironment()}
	for _, opt := range opts {
		opt(&options)
	}
	environment := options.environment
	candidateFS, ok := fs.(OpenCodeCandidateFileSystem)
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
	var jsonSources []openCodeLegacyJSONPath
	for _, root := range cfg.Paths {
		jsonSources = append(jsonSources, a.enumerateOpenCodeLegacyJSONRoot(ctx, root)...)
	}
	total := len(jsonSources)
	for _, result := range evidence {
		if result.Candidate.Kind == OpenCodeSourceSQLite && result.Support == OpenCodeSupportSupported {
			total++
		}
	}
	discoveryTotal(a.progress, HarnessOpenCode, total)
	// A supported database can contain many sessions, and canonical selection
	// may retain none of them. Keep the file pending until its selected rows'
	// freshness has been read, or until selection proves it has no winners.
	var sqliteComplete func(string)
	if a.progress != nil {
		pending := make(map[string]int)
		for _, result := range evidence {
			if result.Candidate.Kind == OpenCodeSourceSQLite && result.Support == OpenCodeSupportSupported {
				pending[filepath.Clean(result.Candidate.Path)]++
			}
		}
		sqliteComplete = func(path string) {
			if pending[path] > 0 {
				pending[path]--
				discoveryAdvance(a.progress, HarnessOpenCode)
			}
		}
	}
	for _, entry := range jsonSources {
		if source, ok := a.readOpenCodeLegacyJSONSource(entry.path, entry.root); ok {
			candidates = append(candidates, buildOpenCodeSession(source))
		}
		discoveryAdvance(a.progress, HarnessOpenCode)
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
	discovered := a.hydrateCanonicalOpenCodeFreshness(ctx, selected, sqliteSources, sqliteComplete)
	if sqliteComplete != nil {
		for _, result := range evidence {
			if result.Candidate.Kind == OpenCodeSourceSQLite && result.Support == OpenCodeSupportSupported {
				sqliteComplete(filepath.Clean(result.Candidate.Path))
			}
		}
	}
	return discovered, nil
}

// openCodeSessionSource is one enumerated OpenCode session before the shared
// build names it: the identity it carries, the file or database that holds it,
// the representation it belongs to, and every attribute that representation
// supplies. The full walk, the two SQLite projections, the legacy-JSON
// enumeration, and the located build all construct this value and hand it to
// one build, so no two paths can describe one session two ways.
type openCodeSessionSource struct {
	ID             SessionID
	Path           ResolvedPath
	Root           ResolvedPath
	Format         SourceFormat
	Representation OpenCodeCanonicalRepresentation
	Provenance     OpenCodeCandidateProvenance
	Origin         TranscriptOrigin
	Title          string
	Slug           string
	Agent          string
	Version        string
	Directory      string
	ProjectName    string
	CreatedAt      time.Time
	Parent         SessionID
}

// buildOpenCodeSession is the one place an OpenCode session becomes a
// candidate. Every enumeration path calls it, so a new field is added here and
// reaches discovery and the located build together.
func buildOpenCodeSession(source openCodeSessionSource) openCodeSessionCandidate {
	session := DiscoveredSession{
		SessionID:        source.ID,
		Harness:          HarnessOpenCode,
		SourcePath:       source.Path,
		SourceFormat:     source.Format,
		OriginalRoot:     source.Root,
		TranscriptOrigin: source.Origin,
		Title:            source.Title,
		Slug:             source.Slug,
		Agent:            source.Agent,
		Version:          source.Version,
		ProjectName:      source.ProjectName,
		CWD:              source.Directory,
		CreatedAt:        source.CreatedAt,
	}
	if source.Parent != "" {
		parent := source.Parent
		session.ParentUUID = &parent
	}
	return openCodeSessionCandidate{
		session:    session,
		identity:   OpenCodeSelectedSourceIdentity{SessionID: source.ID, Representation: source.Representation, Path: source.Path},
		provenance: source.Provenance,
	}
}

// openCodeLegacyJSONPath is a named session file that has not been read yet.
type openCodeLegacyJSONPath struct {
	root ResolvedPath
	path string
}

// enumerateOpenCodeLegacyJSONRoot lists one configured root's session file
// names without reading their contents. The build reads each named file once.
func (a *OpenCodeAdapter) enumerateOpenCodeLegacyJSONRoot(ctx context.Context, root ResolvedPath) []openCodeLegacyJSONPath {
	sessionDir := filepath.Join(root.String(), defaults.OpenCodeDirStorage.String(), defaults.OpenCodeDirSession.String())
	hashEntries, err := a.fs.ReadDir(sessionDir)
	if err != nil {
		// No session directory means no sessions under this root.
		return nil
	}

	var sources []openCodeLegacyJSONPath
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
			sources = append(sources, openCodeLegacyJSONPath{root: root, path: filepath.Join(hashDirPath, name)})
		}
	}
	return sources
}

// readOpenCodeLegacyJSONSource reads one legacy JSON session file into the
// source the shared build names. A file that cannot be read, does not decode,
// or carries an identifier Peasant cannot store is not a session source and
// reports false, exactly as discovery skips it.
func (a *OpenCodeAdapter) readOpenCodeLegacyJSONSource(path string, root ResolvedPath) (openCodeSessionSource, bool) {
	data, err := a.fs.ReadFile(path)
	if err != nil {
		return openCodeSessionSource{}, false
	}
	var ses openCodeSession
	if err := json.Unmarshal(data, &ses); err != nil {
		return openCodeSessionSource{}, false
	}
	sessionID, err := NewSessionID(ses.ID)
	if err != nil {
		return openCodeSessionSource{}, false
	}
	var createdAt time.Time
	if ses.Time.Created > 0 {
		createdAt = time.UnixMilli(ses.Time.Created)
	}
	title := ses.Title
	if title == "" {
		title = ses.Slug
	}
	source := openCodeSessionSource{
		ID:             sessionID,
		Path:           ResolvedPath(path),
		Root:           root,
		Format:         SourceFormatJSON,
		Representation: OpenCodeRepresentationLegacyJSON,
		Provenance:     OpenCodeCandidateLegacyJSONRoot,
		Origin:         TranscriptOriginFile,
		Title:          title,
		Slug:           ses.Slug,
		Agent:          ses.Agent,
		Version:        ses.Version,
		Directory:      ses.Directory,
		ProjectName:    filepath.Base(ses.Directory),
		CreatedAt:      createdAt,
	}
	if ses.ParentID != "" {
		if parentID, err := NewSessionID(ses.ParentID); err == nil {
			source.Parent = parentID
		}
	}
	return source, true
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
	return a.openOpenCodeSQLiteSourceWithOptions(ctx, candidatePath, a.candidateOptions)
}

func (a *OpenCodeAdapter) openOpenCodeSQLiteSourceWithOptions(ctx context.Context, candidatePath string, options OpenCodeSQLiteSourceOptions) (OpenCodeSQLiteSource, error) {
	if a.candidateOpener == nil {
		return nil, fmt.Errorf("open OpenCode SQLite source for %q failed before source access: source opener is nil, so typed reads cannot run; no database was accessed; construct the adapter with OpenOpenCodeSQLiteSource", candidatePath)
	}
	path, err := NewOpenCodeSQLiteSourcePath(candidatePath)
	if err != nil {
		return nil, err
	}
	source, err := a.candidateOpener(ctx, path, options)
	if err != nil {
		return nil, fmt.Errorf("open OpenCode SQLite source %q failed while opening the restrictive read-only source: %w; no session was exposed; verify source readability and retry without modifying the database", candidatePath, err)
	}
	return source, nil
}

// withOpenCodeSQLiteSource opens one restrictive source for a candidate path,
// runs fn against it, and closes it once. It is the scoped open site used where
// the source does not need to outlive the call.
func (a *OpenCodeAdapter) withOpenCodeSQLiteSource(ctx context.Context, candidatePath string, fn func(OpenCodeSQLiteSource) error) error {
	return a.withOpenCodeMaterializationSource(ctx, candidatePath, false, fn)
}

func (a *OpenCodeAdapter) withOpenCodeMaterializationSource(ctx context.Context, candidatePath string, snapshot bool, fn func(OpenCodeSQLiteSource) error) (err error) {
	options := a.candidateOptions
	options.readSnapshot = snapshot
	source, err := a.openOpenCodeSQLiteSourceWithOptions(ctx, candidatePath, options)
	if err != nil {
		return err
	}
	// A snapshot source began its private read transaction when it opened,
	// and Close releases it; beginning a second one inside would nest. A
	// plain source is covered by one read transaction around fn instead.
	var fnErr error
	if sqliteSource, ok := source.(*zombiezenOpenCodeSQLiteSource); ok && !snapshot {
		fnErr = sqliteSource.withReadTransaction(ctx, func() error { return fn(source) })
	} else {
		fnErr = fn(source)
	}
	closeErr := source.Close(ctx)
	return errors.Join(fnErr, closeErr)
}

// applyOpenCodeSessionRecord copies one session row's parent link, changed
// clock and attribution onto a built candidate. The full walk and the located
// build both call it, so both read the same columns into the same fields.
func applyOpenCodeSessionRecord(candidate *openCodeSessionCandidate, record openCodeSessionClock) {
	if record.parent != "" {
		parentID := record.parent
		candidate.session.ParentUUID = &parentID
	}
	// The clock is per session, not per database. A supported database with no
	// row for this session, or a row without a usable time_updated, leaves
	// sessionUpdatedAt zero, so the session takes the mtime-floor path and a row
	// deletion still moves its freshness.
	candidate.sessionUpdatedAt = record.updatedAt
	// Attribute the SQLite-discovered session to its working directory, title,
	// and creation time from the session row. The JSON path sets these from the
	// session file; the SQLite path reads them from the session table so
	// kickstart groups the session under the project its working directory
	// resolves to. An older session table without those columns leaves the
	// fields empty.
	candidate.session.CWD = record.directory
	// Slug is the display-name fallback: a session whose row carries no title
	// shows its harness-generated slug in the listing and metadata.
	title := record.title
	if title == "" {
		title = record.slug
	}
	candidate.session.Title = title
	candidate.session.Slug = record.slug
	candidate.session.CreatedAt = record.createdAt
	// The agent label carries a subagent OpenCode session's agent type so the
	// kickstart listing can show it, mirroring a Claude teammate.
	candidate.session.Agent = record.agent
	// The session row aggregates fill the metadata statistics and version
	// without folding entries.
	candidate.session.Version = record.version
	candidate.session.TokensIn = int(record.tokensIn)
	candidate.session.TokensOut = int(record.tokensOut)
	candidate.session.Cost = record.cost
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
	// controlOnly names, for a current representation, the sessions whose
	// session_message projection holds only control records. A legacy
	// representation passes a nil map because a discovered legacy session always
	// carries substantive rows.
	appendRepresentation := func(sources []openCodeSessionSource, representation OpenCodeCanonicalRepresentation, controlOnly map[SessionID]bool) {
		for _, source := range sources {
			source.Representation = representation
			source.Provenance = result.Candidate.Provenance
			candidate := buildOpenCodeSession(source)
			candidate.currentControlOnly = controlOnly[candidate.session.SessionID]
			candidates = append(candidates, candidate)
		}
	}
	switch result.Capability {
	case OpenCodeCapabilityLegacy:
		sessions, err := a.discoverLegacySQLite(ctx, source, result.Candidate)
		if err != nil {
			a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, "legacy SQLite session enumeration failed", err)
			return nil, source
		}
		appendRepresentation(sessions, OpenCodeRepresentationLegacySQLite, nil)
	case OpenCodeCapabilityCurrent:
		sessions, err := a.discoverCurrentSQLite(ctx, source, result.Candidate)
		if err != nil {
			a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, "current SQLite session enumeration failed", err)
			return nil, source
		}
		appendRepresentation(sessions, OpenCodeRepresentationCurrentSQLite, a.currentControlOnlySessions(ctx, source, result.Candidate, sessions))
	case OpenCodeCapabilityHybrid:
		// A hybrid database carries both projections, so enumerate both and let
		// canonical selection keep one projection per session. Current outranks
		// legacy, so a session in both is a current winner while a session only
		// in the legacy tables is a legacy winner. A session the current
		// projection dropped but the legacy tables still hold is kept when either
		// supported session table still has its live record.
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
		appendRepresentation(current, OpenCodeRepresentationCurrentSQLite, a.currentControlOnlySessions(ctx, source, result.Candidate, current))
		appendRepresentation(legacy, OpenCodeRepresentationLegacySQLite, nil)
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
	a.recordSessionTable(result.Candidate.Path, records.table)
	if err != nil {
		a.recordCandidateOutcome(result.Candidate.Path, OpenCodeProbeDiscover, openCodeOutcomeMetadataUnavailable, "session metadata could not be read completely from supported session tables; sessions stay discoverable as roots and absence could not be verified", err)
		return candidates, source
	}
	if result.V2Layout == OpenCodeSessionV2Unsupported && records.table == OpenCodeSessionTableLegacy {
		a.recordCandidateOutcome(result.Candidate.Path, OpenCodeProbeDiscover, openCodeOutcomeMetadataFallback, "session_v2 lacks id as its sole primary key; session supplies metadata and existence evidence", errors.New("session_v2 is structurally unusable for bounded identity enumeration"))
	}
	if !records.present {
		a.recordCandidateOutcome(result.Candidate.Path, OpenCodeProbeDiscover, openCodeOutcomeMetadataUnavailable, "neither session_v2 nor session supplies usable session identity; sessions stay discoverable as roots and deletion could not be verified", errors.New("no supported metadata table was selected"))
	}
	// A metadata table without parent links and a changed clock degrades those
	// fields, even if v2 still supplies identity. A table with only the parent link or
	// only the clock, is used for whichever it has and is not a failure.
	if records.present && !records.hasParent && !records.hasClock {
		a.recordCandidateOutcome(result.Candidate.Path, OpenCodeProbeDiscover, openCodeOutcomeMetadataDegraded, fmt.Sprintf("%s carries neither parent_id nor time_updated, so its records cannot supply parent links or the changed clock; available identity evidence is retained", records.table), errors.New("selected metadata table lacks both parent_id and time_updated"))
	}
	if len(records.skipped) > 0 {
		// One diagnostic names every dropped row, so the good rows keep their
		// parent link and clock while the bad ones are visible.
		a.recordCandidateOutcome(result.Candidate.Path, OpenCodeProbeDiscover, openCodeOutcomeMetadataDegraded, fmt.Sprintf("%d session metadata row(s) were dropped while retaining valid identity evidence: %s", len(records.skipped), strings.Join(records.skipped, "; ")), errors.New("one or more metadata rows were undecodable"))
	}
	// Only complete successful enumeration of the supported tables can prove
	// absence; message tables may retain orphan history. V2 can prove
	// identity without optional metadata. Preserve legacy clockless compatibility,
	// where old or partial layouts did not supply an authoritative live list.
	if records.canVerifyAbsence {
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
			deleted = append(deleted, fmt.Sprintf("session %q has no live record", sessionID))
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
			a.recordCandidateOutcome(result.Candidate.Path, OpenCodeProbeDiscover, openCodeOutcomeDeleted, fmt.Sprintf("%d session(s) were skipped because they have no live record in the supported session tables (session_v2/session): %s", len(deleted), strings.Join(deleted, "; ")), errors.New("complete supported metadata enumeration verified missing session records, not deletion history"))
		}
	}
	if len(records.danglingParents) > 0 {
		// The children are ingested as roots and the missing links are named, so
		// a parent this run did not discover never silently skips its child.
		a.recordCandidateOutcome(result.Candidate.Path, OpenCodeProbeDiscover, openCodeOutcomeIngestedAsRoots, fmt.Sprintf("%d session(s) were ingested as roots because their parent was not discovered in this run: %s", len(records.danglingParents), strings.Join(records.danglingParents, "; ")), errors.New("one or more parent sessions were absent from discovery"))
	}
	for index := range candidates {
		record, known := records.bySession[candidates[index].session.SessionID]
		if !known {
			continue
		}
		applyOpenCodeSessionRecord(&candidates[index], record)
	}
	// The project tables refine project naming and worktree grouping when they
	// are present. They never change a session's working directory, which stays
	// the session's own directory; git resolution remains the fallback for a
	// database without the project tables.
	if attribution, attrErr := source.ProjectAttribution(ctx); attrErr != nil {
		a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, "project attribution could not be read; sessions keep git-based project resolution", attrErr)
	} else {
		attributeOpenCodeProjects(candidates, attribution)
	}
	// Read the newest event sequence per session so a rescan detects an in-place
	// rewrite that moves no time column. The change cursor is an additional
	// trigger; the session clock stays the primary changed signal.
	a.attributeOpenCodeEventSequences(ctx, source, result.Candidate, candidates)
	return candidates, source
}

// attributeOpenCodeEventSequences sets each discovered session's newest event
// sequence. It reads the event_sequence table once per database; a database
// without that table falls back to the payload-free per-session MAX(seq) seek
// over the event table. The event payload is never read. A read failure leaves
// the sequence zero, so the session keeps its clock-only changed signal.
func (a *OpenCodeAdapter) attributeOpenCodeEventSequences(ctx context.Context, source OpenCodeSQLiteSource, candidate OpenCodeCandidate, candidates []openCodeSessionCandidate) {
	sequence, err := source.EventSequenceBySession(ctx)
	if err != nil {
		a.recordCandidateFailure(candidate.Path, OpenCodeProbeDiscover, "event sequence could not be read; sessions keep the clock-only changed signal", err)
		return
	}
	if sequence.Present {
		for index := range candidates {
			if seq, known := sequence.BySession[string(candidates[index].session.SessionID)]; known {
				candidates[index].session.EventSeq = seq
			}
		}
		return
	}
	// The event_sequence table is absent, so seek each discovered session's newest
	// sequence directly from the event table with the payload-free MAX(seq).
	for index := range candidates {
		linkID, linkErr := NewOpenCodeSessionLinkID(string(candidates[index].session.SessionID))
		if linkErr != nil {
			continue
		}
		maxSeq, seekErr := source.MaxEventSeq(ctx, linkID)
		if seekErr != nil {
			a.recordCandidateFailure(candidate.Path, OpenCodeProbeDiscover, fmt.Sprintf("event sequence seek failed for session %q; it keeps the clock-only changed signal", candidates[index].session.SessionID), seekErr)
			continue
		}
		if maxSeq.Present {
			candidates[index].session.EventSeq = maxSeq.Seq
		}
	}
}

// attributeOpenCodeProjects resolves each session's project name and worktree
// root from the OpenCode project tables. A session whose working directory is a
// directory the project_directory table maps to a project groups under that
// project's root, so worktrees of one project share a project even though their
// working directories differ. CWD is never changed. When the tables are absent
// or a session's directory is unmapped, the session keeps its git-based
// resolution.
func attributeOpenCodeProjects(candidates []openCodeSessionCandidate, attribution OpenCodeProjectAttribution) {
	if !attribution.ProjectsPresent || !attribution.DirectoriesPresent {
		return
	}
	projectsByID := make(map[string]OpenCodeProjectRecord, len(attribution.Projects))
	for _, project := range attribution.Projects {
		projectsByID[project.ID.String()] = project
	}
	projectByDirectory := make(map[string]OpenCodeProjectRecord, len(attribution.Directories))
	for _, directory := range attribution.Directories {
		if directory.Directory == "" {
			continue
		}
		if project, known := projectsByID[directory.ProjectID.String()]; known {
			projectByDirectory[directory.Directory] = project
		}
	}
	for index := range candidates {
		directory := candidates[index].session.CWD
		if directory == "" {
			continue
		}
		project, matched := projectByDirectory[directory]
		if !matched || project.Worktree == "" {
			continue
		}
		candidates[index].session.ProjectWorktree = project.Worktree
		name := project.Name
		if name == "" {
			name = filepath.Base(project.Worktree)
		}
		candidates[index].session.ProjectName = name
	}
}

// openCodeSessionClock is one session row's parent link, update clock, and
// attribution. directory, title, and createdAt come from the session row when
// the session table exposes them and stay empty for an older layout, so
// discovery attributes a session to its working directory, title, and creation
// time without a second read.
type openCodeSessionClock struct {
	parent    SessionID
	updatedAt time.Time
	directory string
	title     string
	createdAt time.Time
	agent     string
	slug      string
	version   string
	tokensIn  int64
	tokensOut int64
	cost      float64
}

// openCodeSessionRecords holds every session row of one database. present is
// false when the database has no session table. hasParent and hasClock report
// which of the parent link and changed clock columns the session table carries,
// so parent links are read whether or not the clock column exists.
type openCodeSessionRecords struct {
	canVerifyAbsence bool
	table            OpenCodeSessionTable
	present          bool
	hasParent        bool
	hasClock         bool
	bySession        map[SessionID]openCodeSessionClock
	// rowIDs names every session that still has a row in the session table,
	// including a row whose parent link or clock could not be decoded. A
	// discovered session missing from rowIDs while the table is present and
	// enumerable has no live record, so its historical message rows are skipped.
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
	records, err := a.readSQLiteSessionRecords(ctx, source, candidate, discoveredIDs, OpenCodeSessionRecordsPreferred, nil)
	if err != nil || records.table != OpenCodeSessionTableV2 {
		return records, err
	}
	legacy, err := a.readSQLiteSessionRecords(ctx, source, candidate, discoveredIDs, OpenCodeSessionRecordsLegacy, records.rowIDs)
	if err != nil {
		return records, err
	}
	// A successful V2 read does not prove absence from a supported legacy
	// store. Keep its live identities too, but never replace shared V2 metadata,
	// even when the V2 row's optional fields could not be decoded.
	if legacy.present && !legacy.canVerifyAbsence {
		records.canVerifyAbsence = false
	}
	for id := range legacy.rowIDs {
		if _, shared := records.rowIDs[id]; !shared {
			if row, ok := legacy.bySession[id]; ok {
				records.bySession[id] = row
			}
		}
		records.rowIDs[id] = struct{}{}
	}
	records.skipped = append(records.skipped, legacy.skipped...)
	records.danglingParents = append(records.danglingParents, legacy.danglingParents...)
	return records, nil
}

func (a *OpenCodeAdapter) readSQLiteSessionRecords(ctx context.Context, source OpenCodeSQLiteSource, candidate OpenCodeCandidate, discoveredIDs map[SessionID]struct{}, selection OpenCodeSessionRecordSelection, preferredIDs map[SessionID]struct{}) (openCodeSessionRecords, error) {
	records := openCodeSessionRecords{bySession: make(map[SessionID]openCodeSessionClock, len(discoveredIDs)), rowIDs: make(map[SessionID]struct{}, len(discoveredIDs))}
	pageSize, err := NewOpenCodeCurrentPageSize(openCodeCurrentMaterializePage)
	if err != nil {
		return records, err
	}
	var cursor *OpenCodeSessionRecordCursor
	for {
		page, readErr := source.SessionRecords(ctx, OpenCodeSessionRecordPageRequest{Selection: selection, PageSize: pageSize, After: cursor})
		if page.Table != "" {
			records.table = page.Table
		}
		if readErr != nil {
			return records, fmt.Errorf("read OpenCode session records from %q failed while enumerating a bounded session page: %w; sessions remain discoverable as roots; verify the session table and retry", candidate.Path, readErr)
		}
		if cursor != nil && !page.Supported {
			return records, fmt.Errorf("read OpenCode session records from %q lost supported metadata during pagination; absence cannot be verified; retry after OpenCode finishes changing its schema", candidate.Path)
		}
		records.present = page.Supported
		records.hasParent = page.HasParent
		records.hasClock = page.HasClock
		for _, skip := range page.Skipped {
			records.skipped = append(records.skipped, fmt.Sprintf("%s: %s", page.Table, skip.Reason))
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
			if _, preferred := preferredIDs[sessionID]; preferred {
				continue
			}
			if _, discovered := discoveredIDs[sessionID]; !discovered {
				continue
			}
			clock := openCodeSessionClock{directory: row.Directory, title: row.Title, agent: row.Agent, slug: row.Slug, version: row.Version, tokensIn: row.TokensInput, tokensOut: row.TokensOutput, cost: row.Cost}
			if row.TimeUpdated > 0 {
				clock.updatedAt = time.UnixMilli(row.TimeUpdated)
			}
			if row.TimeCreated > 0 {
				clock.createdAt = time.UnixMilli(row.TimeCreated)
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
	records.canVerifyAbsence = records.present && (records.hasClock || records.table == OpenCodeSessionTableV2)
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

// openCodeDiagnosticOutcome names what happened to the sessions a discovery
// diagnostic describes, so the diagnostic's meaning and remediation match the
// event rather than assuming every diagnostic skipped its sessions.
type openCodeDiagnosticOutcome int

const (
	// openCodeOutcomeSkipped is the default: the candidate could not contribute
	// some or all of its sessions this run.
	openCodeOutcomeSkipped openCodeDiagnosticOutcome = iota
	// openCodeOutcomeIngestedAsRoots names sessions kept as roots because this
	// run did not discover their parent. Nothing was skipped.
	openCodeOutcomeIngestedAsRoots
	// openCodeOutcomeFloorFallback names sessions whose freshness fell back to
	// the database and write-ahead-log file time. Nothing was skipped.
	openCodeOutcomeFloorFallback
	// openCodeOutcomeDeleted names historical rows without a live session record.
	openCodeOutcomeDeleted
	openCodeOutcomeMetadataUnavailable
	openCodeOutcomeMetadataDegraded
	openCodeOutcomeMetadataFallback
)

// meaningAndRemediation returns the Meaning and Remediation text for the
// outcome. Keeping them beside the outcome constant means one diagnostic can no
// longer report that sessions were skipped when they were ingested as roots or
// only fell back for freshness.
func (o openCodeDiagnosticOutcome) meaningAndRemediation() (string, string) {
	switch o {
	case openCodeOutcomeMetadataUnavailable:
		return "transcript candidates remain discoverable as roots with file-based freshness; incomplete metadata enumeration cannot prove deletion and no candidate was removed by this metadata failure",
			"retry after the source is readable; if the failure persists, report its schema and error without transcript content; do not modify the OpenCode database through Peasant"
	case openCodeOutcomeMetadataDegraded:
		return "available metadata and valid session identities are retained; missing or malformed metadata does not itself mean a session was deleted",
			"inspect the selected table's supported column shape and retry; report persistent metadata incompatibility without transcript content"
	case openCodeOutcomeMetadataFallback:
		return "the legacy session table is the metadata authority because session_v2 is structurally unusable; transcript representation selection remains independent",
			"report the unsupported session_v2 column shape so compatibility can be extended; do not migrate or repair the source through Peasant"
	case openCodeOutcomeIngestedAsRoots:
		return "the named sessions were ingested as roots because this run did not discover their parent; no session was skipped and every other session was still discovered",
			"discover the parent to restore the link, for example by widening the selection or ingesting the parent's source; if the parent no longer exists in OpenCode, the root attachment is correct and no action is needed"
	case openCodeOutcomeFloorFallback:
		return "the named sessions used the database and write-ahead-log file time as a freshness floor; no session was skipped and every session was still discovered",
			"no action is needed; the file time still moves a session when its rows change"
	case openCodeOutcomeDeleted:
		return "the named sessions have historical message rows but no live record in the supported session tables; those candidates were skipped, but their deletion history is unknown",
			"if a named session should be live, verify its session record in OpenCode and retry; Peasant does not modify the source database"
	default:
		return "sessions from this candidate were skipped for this run; sessions from other candidates and the legacy JSON layout were still discovered",
			"retry after OpenCode finishes writing; if the failure persists, verify the database with OpenCode and do not modify it through Peasant"
	}
}

// logSummary is the one-line slog message for the outcome.
func (o openCodeDiagnosticOutcome) logSummary() string {
	switch o {
	case openCodeOutcomeMetadataUnavailable, openCodeOutcomeMetadataDegraded:
		return "opencode discovery: session metadata degraded"
	case openCodeOutcomeMetadataFallback:
		return "opencode discovery: legacy session metadata selected"
	case openCodeOutcomeIngestedAsRoots:
		return "opencode discovery: sessions ingested as roots"
	case openCodeOutcomeFloorFallback:
		return "opencode discovery: freshness used the file-time floor"
	case openCodeOutcomeDeleted:
		return "opencode discovery: sessions without live records skipped"
	default:
		return "opencode discovery: candidate skipped"
	}
}

// recordCandidateFailure attaches a skip diagnostic to the failing candidate's
// evidence and logs it. Discovery continues for other candidates.
func (a *OpenCodeAdapter) recordCandidateFailure(path string, stage OpenCodeProbeStage, what string, cause error) {
	a.recordCandidateOutcomeAt(a.resolveCandidateEvidenceIndex(path), path, stage, openCodeOutcomeSkipped, what, cause)
}

func (a *OpenCodeAdapter) recordSessionTable(path string, table OpenCodeSessionTable) {
	index := a.resolveCandidateEvidenceIndex(path)
	if index < 0 {
		return
	}
	a.candidateMu.Lock()
	defer a.candidateMu.Unlock()
	if index < len(a.candidateEvidence) {
		a.candidateEvidence[index].SessionTable = table
	}
}

// recordCandidateOutcome attaches a diagnostic whose meaning matches the given
// outcome, so a non-skip event such as a root-attached session or a freshness
// floor fallback no longer reports that sessions were skipped.
func (a *OpenCodeAdapter) recordCandidateOutcome(path string, stage OpenCodeProbeStage, outcome openCodeDiagnosticOutcome, what string, cause error) {
	a.recordCandidateOutcomeAt(a.resolveCandidateEvidenceIndex(path), path, stage, outcome, what, cause)
}

// recordCandidateFailureAt records a skip diagnostic against a pre-resolved
// evidence index.
func (a *OpenCodeAdapter) recordCandidateFailureAt(evidenceIndex int, path string, stage OpenCodeProbeStage, what string, cause error) {
	a.recordCandidateOutcomeAt(evidenceIndex, path, stage, openCodeOutcomeSkipped, what, cause)
}

// recordCandidateOutcomeAt records the outcome-specific diagnostic against a
// pre-resolved evidence index, so a per-session loop does not rescan the
// evidence slice for every session on one path. A negative index records
// nothing on the evidence and only logs.
func (a *OpenCodeAdapter) recordCandidateOutcomeAt(evidenceIndex int, path string, stage OpenCodeProbeStage, outcome openCodeDiagnosticOutcome, what string, cause error) {
	meaning, remediation := outcome.meaningAndRemediation()
	diagnostic := actionableOpenCodeDiagnostic(
		stage,
		what,
		cause.Error(),
		path,
		"during OpenCode discovery after the candidate probe reported a supported schema",
		meaning,
		remediation,
	)
	if evidenceIndex >= 0 {
		a.candidateMu.Lock()
		if evidenceIndex < len(a.candidateEvidence) {
			a.candidateEvidence[evidenceIndex].Diagnostics = append(a.candidateEvidence[evidenceIndex].Diagnostics, diagnostic)
		}
		a.candidateMu.Unlock()
	}
	slog.Warn(outcome.logSummary(),
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

// effectiveOpenCodeCanonicalRank ranks one candidate for canonical selection.
// It is the representation precedence, except that a current candidate whose
// projection is control-only drops below every representation that carries
// substantive content. The result is that a control-only current candidate
// loses to a legacy sibling, so the legacy conversation renders instead of a
// handful of inert control turns, while a control-only current candidate with
// no sibling is still the sole candidate and materializes as before. A
// substantive current candidate keeps the top rank, so the pre-existing
// current-over-legacy preference is unchanged for real conversations.
func effectiveOpenCodeCanonicalRank(candidate openCodeSessionCandidate) uint8 {
	switch candidate.identity.Representation {
	case OpenCodeRepresentationCurrentSQLite:
		if candidate.currentControlOnly {
			return 1
		}
		return 4
	case OpenCodeRepresentationLegacySQLite:
		return 3
	case OpenCodeRepresentationLegacyJSON:
		return 2
	default:
		return 0
	}
}

func canonicalOpenCodeCandidatePrecedes(candidate, incumbent openCodeSessionCandidate) bool {
	candidateRank := effectiveOpenCodeCanonicalRank(candidate)
	incumbentRank := effectiveOpenCodeCanonicalRank(incumbent)
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
// winner with a usable clock (session.time_updated) uses that clock as its
// changed time and reads no row aggregate. OpenCode moves that clock on revert
// and undo, so a row deletion still moves freshness without touching sibling
// sessions. A session with no usable clock reads the per-table row aggregate
// and falls back to the database and WAL mtime as a floor, so a row deletion is
// still seen. The active (staleness) time is always the database and WAL mtime.
//
// The row aggregate is read only for the clockless sessions of a database, so a
// database whose sessions all carry a clock reads no freshness aggregate at all
// and a very large legacy table is never scanned for a session that already has
// a clock. A failed aggregate read therefore demotes only the clockless
// sessions to the mtime floor; a clock-bearing session keeps its clock.
//
// The changed clock reports content edits and deletions, not the raw file
// mtime. An in-place row rewrite that moves no time column and no session
// clock therefore is not detected as a change for a session that has a usable
// clock; only the floor path, used when a session has no usable clock, tracks
// the file and WAL mtime. A clockless SQLite candidate whose freshness cannot
// be read is skipped and recorded; other winners stay.
func (a *OpenCodeAdapter) hydrateCanonicalOpenCodeFreshness(ctx context.Context, selected []openCodeSessionCandidate, sources map[string]OpenCodeSQLiteSource, sqliteComplete func(string)) []DiscoveredSession {
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
	for index, rawPath := range paths {
		if index > 0 && sqliteComplete != nil {
			sqliteComplete(paths[index-1])
		}
		// The source was opened once during discovery and reused here, so
		// freshness observes the same snapshot as enumeration and no second open
		// is needed. The caller closes every source exactly once.
		source, opened := sources[rawPath]
		if !opened || source == nil {
			a.recordCandidateFailure(rawPath, OpenCodeProbeFreshness, "selected SQLite source was not available for freshness because discovery did not open it", fmt.Errorf("no open source for %q", rawPath))
			continue
		}
		// A session with a usable clock (session.time_updated) uses that clock as
		// its changed time and reads no row aggregate, so only the clockless
		// sessions on this database need the per-table freshness read. Collect
		// them first. A database whose sessions all carry a clock reads no
		// freshness aggregate at all, so a very large legacy table is never
		// scanned for a session that already has a clock.
		var clockless []int
		for _, index := range bySQLitePath[rawPath] {
			if selected[index].sessionUpdatedAt.IsZero() {
				clockless = append(clockless, index)
			}
		}
		// Read the newest row time of every clockless session on this database
		// with at most one GROUP BY aggregate per table they use, so freshness
		// reads stay bounded by table count and never grow with the number of
		// sessions. An empty clockless set reads nothing.
		rowFreshness := a.batchOpenCodeSQLiteRowFreshness(ctx, source, selected, clockless)
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
			if !candidate.sessionUpdatedAt.IsZero() {
				// Clock-first: the session clock is the changed time and the
				// database and WAL mtime is the active time. No row aggregate is
				// read, so a failed aggregate read never demotes this session.
				candidate.session.ModTime = candidate.sessionUpdatedAt
				if floorErr == nil {
					candidate.session.ActiveModTime = floor
				}
				keep[index] = true
				continue
			}
			// Clockless: the row aggregate and the mtime floor decide freshness.
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
			candidate.session.ModTime = newest
			keep[index] = true
		}
		if len(freshnessSkipped) > 0 {
			a.recordCandidateFailureAt(evidenceIndex, rawPath, OpenCodeProbeFreshness, fmt.Sprintf("selected freshness could not be read and the mtime floor was unavailable, so %d session(s) were skipped: %s", len(freshnessSkipped), strings.Join(freshnessSkipped, ", ")), freshnessCause)
		}
		if len(floorFallback) > 0 {
			a.recordCandidateOutcomeAt(evidenceIndex, rawPath, OpenCodeProbeFreshness, openCodeOutcomeFloorFallback, fmt.Sprintf("selected freshness could not be read, so %d session(s) fell back to the database and WAL mtime floor: %s", len(floorFallback), strings.Join(floorFallback, ", ")), floorFallbackCause)
		}
		if len(clocklessSkipped) > 0 {
			a.recordCandidateFailureAt(evidenceIndex, rawPath, OpenCodeProbeFreshness, fmt.Sprintf("selected SQLite content freshness could not be read, so %d clockless session(s) were skipped: %s", len(clocklessSkipped), strings.Join(clocklessSkipped, ", ")), clocklessCause)
		}
	}
	if len(paths) > 0 && sqliteComplete != nil {
		sqliteComplete(paths[len(paths)-1])
	}

	discovered := make([]DiscoveredSession, 0, len(selected))
	for index, candidate := range selected {
		if keep[index] {
			discovered = append(discovered, candidate.session)
		}
	}
	return discovered
}

// batchOpenCodeSQLiteRowFreshness reads the newest row time of the passed
// sessions on one database with at most one GROUP BY aggregate per table, then
// returns a lookup that resolves a single winner's row freshness from the
// shared result. Only the tables that a passed session actually uses are read,
// so an empty index set reads nothing and a set of clockless sessions costs at
// most one statement per present table, never one per session.
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
	evidence := a.candidateProber.Probe(ctx, a.resolveOpenCodeCandidateSources(roots))
	a.candidateMu.Lock()
	a.candidateEvidence = cloneOpenCodeProbeResults(evidence)
	a.candidateMu.Unlock()
	return cloneOpenCodeProbeResults(evidence)
}

// resolveOpenCodeCandidateSources names every source the configured roots can
// hold, in the order a full discovery considers them: for each root, the
// environment override database, then the channel database, then the legacy
// JSON root. A source named twice is kept once, so one file is never claimed
// twice and one lookup never reports the same search twice. The database and
// the chosen channel live in the same value the prober receives, so the lookup
// and the walk cannot disagree about which databases exist.
func (a *OpenCodeAdapter) resolveOpenCodeCandidateSources(roots []ResolvedPath) []OpenCodeCandidate {
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
	return candidates
}

func cloneOpenCodeProbeResults(results []OpenCodeProbeResult) []OpenCodeProbeResult {
	cloned := make([]OpenCodeProbeResult, len(results))
	for index, result := range results {
		cloned[index] = result
		cloned[index].Diagnostics = append([]OpenCodeProbeDiagnostic(nil), result.Diagnostics...)
		cloned[index].Evidence.Tables = append([]string(nil), result.Evidence.Tables...)
		cloned[index].Evidence.SessionColumns = append([]OpenCodeColumnEvidence(nil), result.Evidence.SessionColumns...)
		cloned[index].Evidence.SessionV2Columns = append([]OpenCodeColumnEvidence(nil), result.Evidence.SessionV2Columns...)
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

// openCodeStartedBy is the durable started_by evidence for a session whose
// authoritative OpenCode row names a parent. Both metadata-assembly paths emit
// it whenever the session carries a parent, whether or not this run discovered
// or stored that parent: the target may name an unstored session, and the
// listing groups or hides the child by this evidence rather than by discovery.
func openCodeStartedBy(parent SessionID) schema.SessionRelationship {
	target := parent
	return schema.SessionRelationship{Kind: schema.SessionRelationshipStartedBy, TargetState: schema.RelationshipTargetKnown, TargetLocalID: &target, Evidence: schema.EvidenceExistingAdapter}
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
		captured, err := a.MaterializeTranscript(ctx, session)
		return captured.Metadata, err
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
	if ses.ID != session.SessionID.String() {
		return nil, fmt.Errorf("OpenCode metadata capture for %s: source session id changed after discovery; no capture was written; restore the matching source and rerun peasant ingest", session.SessionID)
	}

	// ── 2. Count messages (turns + tool calls) ────────────────────────────────
	// The message directory lives at {root}/storage/message/ses_{id}/
	storageRoot := resolveStorageRoot(session)
	semanticMessages := loadOpenCodeJSONSemanticMessages(a.fs, session)
	semanticSummary := summarizeOpenCodeSemanticMessages(semanticMessages)

	// ── 3. Resolve git info ────────────────────────────────────────────────────
	workDir := ses.Directory
	if workDir == "" {
		// Session has no directory; use the storage root as the path the identity
		// hash is derived from, so worktreeForHash is a valid absolute path rather
		// than "/" (which produces invalid HostSlug values). Git is not asked about
		// it: see gitDir below.
		workDir = storageRoot
	}

	var gitBranch, gitRemote, gitWorktree *string

	// Git is asked about the session's project directory and nothing else, the
	// directory the tree and the import filter ask about; workDir above may be
	// the storage root, which is only a path for the identity hash.
	gitDir := ExtractionDirectory(session, ses.Directory, osDirectoryExists)
	if branch := ResolveGitBranch(ctx, a.git, gitDir, session.Branch); branch != "" {
		gitBranch = &branch
	}

	remote, tracking := ResolveGitRemote(ctx, a.git, gitDir, session.Branch, "")
	if remote != "" {
		r := remote
		gitRemote = &r
	}

	if gitDir != "" {
		if wt, err := a.git.Worktree(ctx, gitDir); err == nil && wt != "" {
			w := wt
			gitWorktree = &w
		}
	}

	var gitTracking *string
	if tracking != "" {
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

	projectHash, hostSlug, err := DeriveProjectIdentifiers(a.salt, remoteForHash, worktreeForHash)
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
	applyOpenCodeThinkingSeed(&md, semanticMessages, decodeOpenCodeModelVariant(ses.Model))

	if session.ParentUUID != nil {
		parent := *session.ParentUUID
		md.ParentUUID = &parent
		md.Relationships = append(md.Relationships, openCodeStartedBy(parent))
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
	ID        string          `json:"id"`
	Slug      string          `json:"slug"`
	Version   string          `json:"version"`
	Agent     string          `json:"agent"`
	ProjectID string          `json:"projectID"`
	Directory string          `json:"directory"`
	ParentID  string          `json:"parentID"`
	Title     string          `json:"title"`
	Model     json.RawMessage `json:"model"`
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

// ---------- scoped lookup and located build ----------

// LookupSessionSources finds the OpenCode sources that can hold id by name. The
// two storage formats answer differently, and the difference is stated, never
// mistaken for a miss:
//
//   - SQLite: one row by id per candidate database, in the order Discover
//     considers them (environment override database, then channel database).
//     Each candidate database is opened once and asked for its one row; a
//     database that does not open is never queried for an id at all, because
//     the opener takes a path.
//   - legacy JSON: the id appears only in the file body, so this format cannot
//     be searched by id. That is reported as unsupported for every configured
//     root instead of being passed over in silence.
//
// The searched block follows that same candidate order, one entry per source: for
// each configured root, the override database, the channel database, then the
// legacy root. A candidate database that does not exist still contributes its
// entry — it is a place the id was looked for — and is never opened.
//
// It reads no transcript and lists no directory. A failed lookup is recorded
// ONCE, by the core: on a returned error the core appends the SearchFailed
// entry and carries the miss, so this method never appends one itself. It
// returns the entries it gathered before the failure, so the miss still names
// where else the id was looked for.
func (a *OpenCodeAdapter) LookupSessionSources(ctx context.Context, cfg SourceConfig, id SessionID) (SourceLookup, error) {
	if err := ctx.Err(); err != nil {
		return SourceLookup{}, err
	}
	if a.candidateInitErr != nil {
		return SourceLookup{}, a.candidateInitErr
	}
	if id == "" {
		return SourceLookup{}, fmt.Errorf(
			"ingest: OpenCodeAdapter.LookupSessionSources was given an empty session id; an empty id can never match an OpenCode row or session file, so the lookup could only ever report a miss; where: ingest.OpenCodeAdapter.LookupSessionSources in internal/ingest/opencode.go; when: locating a named session by name; impact: no OpenCode source is searched; fix: pass the session id the user named")
	}
	if _, err := NewSessionID(string(id)); err != nil {
		return SourceLookup{}, fmt.Errorf(
			"ingest: OpenCodeAdapter.LookupSessionSources was given %q, which is not a session id; no OpenCode source can be searched under a name no session can hold; where: ingest.OpenCodeAdapter.LookupSessionSources in internal/ingest/opencode.go; when: locating a named session by name; impact: no OpenCode source is searched; fix: pass the session id the user named: %w", id, err)
	}

	candidates := a.resolveOpenCodeCandidateSources(cfg.Paths)
	var lookup SourceLookup
	// The searched block follows the same candidate order Discover considers, so
	// the miss reads in the order the sources are actually visited: for each
	// configured root, the environment override database, the channel database,
	// then the legacy JSON root.
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return lookup, err
		}
		switch candidate.Kind {
		case OpenCodeSourceLegacyJSON:
			// The id appears only in the file body, so this format cannot be
			// searched by id. The limitation belongs to the format, not to the
			// directory, so the entry is recorded whether or not the root exists
			// today.
			lookup.Searched = append(lookup.Searched, SearchedSource{
				Harness:  HarnessOpenCode,
				Method:   SearchUnsupported,
				Location: openCodeLegacyJSONSearchPattern(ResolvedPath(candidate.Path), id),
				Reason:   IDOnlyInFileBody,
			})
		case OpenCodeSourceSQLite:
			location := openCodeDisplayPath(candidate.Path)
			// A candidate database that does not exist holds no rows. It is a
			// place the id was looked for, so the miss names it, and it is never
			// opened.
			if _, statErr := a.fs.Stat(candidate.Path); statErr != nil {
				if errors.Is(statErr, fs.ErrNotExist) {
					lookup.Searched = append(lookup.Searched, SearchedSource{
						Harness:  HarnessOpenCode,
						Method:   SearchSQLiteRow,
						Location: location,
					})
					continue
				}
				return lookup, openCodeLookupFailure(id, location, statErr)
			}
			source, err := a.openOpenCodeSQLiteSource(ctx, candidate.Path)
			if err != nil {
				// No row is read from a database that did not open: the opener
				// takes a path, and the database was never in a state to answer
				// for an id.
				return lookup, openCodeLookupFailure(id, location, err)
			}
			present, queryErr := a.openCodeSessionRowPresent(ctx, source, id)
			a.closeScopedOpenCodeSource(ctx, source, candidate.Path)
			if queryErr != nil {
				return lookup, openCodeLookupFailure(id, location, queryErr)
			}
			lookup.Searched = append(lookup.Searched, SearchedSource{
				Harness:  HarnessOpenCode,
				Method:   SearchSQLiteRow,
				Location: location,
			})
			if present {
				lookup.Sources = append(lookup.Sources, ResolvedPath(candidate.Path))
			}
		}
	}
	return lookup, nil
}

// openCodeLookupFailure is the one statement of "the OpenCode lookup could not
// cover this database". It names the database and carries the underlying error
// so the miss can print its first line; the core records it as SearchFailed.
func openCodeLookupFailure(id SessionID, location string, err error) error {
	return &SourceLookupFailure{Location: location, Err: fmt.Errorf(
		"ingest: the OpenCode lookup for session %s could not read database %q; the database did not answer, so the id cannot be reported absent from it; where: ingest.OpenCodeAdapter.LookupSessionSources in internal/ingest/opencode.go; when: locating the named session by name; impact: this database was not searched, and the miss text names the failure; fix: make the database readable and retry: %w", id, location, err)}
}

// openCodeSessionRowPresent reports whether one database holds a session-table
// row for id. The preferred read covers session_v2 when it is usable and the
// legacy session table otherwise; a hybrid database whose preferred table is V2
// is asked for the legacy row too, so a legacy-only session is not passed over.
// One bounded row is read, never a page of the table.
func (a *OpenCodeAdapter) openCodeSessionRowPresent(ctx context.Context, source OpenCodeSQLiteSource, id SessionID) (bool, error) {
	pageSize, err := NewOpenCodeCurrentPageSize(1)
	if err != nil {
		return false, err
	}
	linkID, err := NewOpenCodeSessionLinkID(string(id))
	if err != nil {
		return false, err
	}
	page, err := source.SessionRecords(ctx, OpenCodeSessionRecordPageRequest{Selection: OpenCodeSessionRecordsPreferred, PageSize: pageSize, SessionID: &linkID})
	if err != nil {
		return false, fmt.Errorf("read the session row for %s from the preferred OpenCode session table: %w", id, err)
	}
	if openCodeSessionRecordPageHasID(page, linkID) {
		return true, nil
	}
	if page.Table != OpenCodeSessionTableV2 {
		return false, nil
	}
	legacy, err := source.SessionRecords(ctx, OpenCodeSessionRecordPageRequest{Selection: OpenCodeSessionRecordsLegacy, PageSize: pageSize, SessionID: &linkID})
	if err != nil {
		return false, fmt.Errorf("read the legacy session row for %s from the OpenCode session table: %w", id, err)
	}
	return openCodeSessionRecordPageHasID(legacy, linkID), nil
}

// openCodeSessionRecordPageHasID reports whether one bounded row read observed
// the requested identifier. PresentSessionIDs counts a row whose optional
// columns could not be decoded, so a damaged row still proves the session
// exists.
func openCodeSessionRecordPageHasID(page OpenCodeSessionRecordPage, id OpenCodeSessionLinkID) bool {
	for _, present := range page.PresentSessionIDs {
		if present == id {
			return true
		}
	}
	return false
}

// openCodeSessionRecordByID reads the one session row the located build
// attributes a session from, preferring the V2 table exactly as the full walk
// does. hasRecord is false when no supported table holds a row for the id; the
// session is still built then, with no parent link and no attribution.
func (a *OpenCodeAdapter) openCodeSessionRecordByID(ctx context.Context, source OpenCodeSQLiteSource, id SessionID) (openCodeSessionClock, bool, error) {
	pageSize, err := NewOpenCodeCurrentPageSize(1)
	if err != nil {
		return openCodeSessionClock{}, false, err
	}
	linkID, err := NewOpenCodeSessionLinkID(string(id))
	if err != nil {
		return openCodeSessionClock{}, false, err
	}
	page, err := source.SessionRecords(ctx, OpenCodeSessionRecordPageRequest{Selection: OpenCodeSessionRecordsPreferred, PageSize: pageSize, SessionID: &linkID})
	if err != nil {
		return openCodeSessionClock{}, false, fmt.Errorf("read the session row for %s from the preferred OpenCode session table: %w", id, err)
	}
	for index := range page.Records {
		if page.Records[index].SessionID == linkID {
			record := openCodeSessionClockFromRecord(page.Records[index])
			return record, true, nil
		}
	}
	if page.Table != OpenCodeSessionTableV2 {
		return openCodeSessionClock{}, false, nil
	}
	legacy, err := source.SessionRecords(ctx, OpenCodeSessionRecordPageRequest{Selection: OpenCodeSessionRecordsLegacy, PageSize: pageSize, SessionID: &linkID})
	if err != nil {
		return openCodeSessionClock{}, false, fmt.Errorf("read the legacy session row for %s from the OpenCode session table: %w", id, err)
	}
	for index := range legacy.Records {
		if legacy.Records[index].SessionID == linkID {
			record := openCodeSessionClockFromRecord(legacy.Records[index])
			return record, true, nil
		}
	}
	return openCodeSessionClock{}, false, nil
}

// openCodeSessionClockFromRecord copies one detached session row into the
// internal parent-link and attribution shape the shared build consumes.
func openCodeSessionClockFromRecord(record OpenCodeSessionRecord) openCodeSessionClock {
	clock := openCodeSessionClock{
		directory: record.Directory,
		title:     record.Title,
		agent:     record.Agent,
		slug:      record.Slug,
		version:   record.Version,
		tokensIn:  record.TokensInput,
		tokensOut: record.TokensOutput,
		cost:      record.Cost,
	}
	// The full walk keeps a parent link only when it names another storable
	// session; a self-link or an unstorable identifier is dropped.
	if parentText := record.ParentID.String(); parentText != "" {
		if parentID, err := NewSessionID(parentText); err == nil && parentID != SessionID(record.SessionID.String()) {
			clock.parent = parentID
		}
	}
	if record.TimeUpdated > 0 {
		clock.updatedAt = time.UnixMilli(record.TimeUpdated)
	}
	if record.TimeCreated > 0 {
		clock.createdAt = time.UnixMilli(record.TimeCreated)
	}
	return clock
}

// discoverOpenCodeDatabaseMember builds the members one located database
// carries: the session itself and every stored child the core passed in. It
// reads only the one row per session it must, plus the same attribution and
// freshness reads the full walk makes for that session.
func (a *OpenCodeAdapter) discoverOpenCodeDatabaseMember(ctx context.Context, cfg SourceConfig, candidate OpenCodeCandidate, at LocatedSource) (LocatedBuild, error) {
	result := a.probeOpenCodeCandidate(ctx, candidate)
	switch result.Support {
	case OpenCodeSupportSupported:
	case OpenCodeSupportCorrupt, OpenCodeSupportUnreadable:
		// The file exists but cannot be read as this session's source. A stored
		// session keeps its transcript and takes the store-side upkeep a full run
		// gives it, so this is a member with its reason, never a refused run. No
		// id was queried: the failure happened before any row read.
		return LocatedBuild{
			Members: []LocatedMember{{
				ID:         at.ID,
				Harness:    HarnessOpenCode,
				Unreadable: UnreadableSourceFailed,
				Detail:     firstLineOr(openCodeProbeDetail(result), "the located source could not be read"),
			}},
			Deferred: NoDeferredWrites{},
		}, nil
	default:
		return LocatedBuild{}, openCodeUnsupportedSourceError(at.Path, at.ID, result)
	}
	build, err := a.openCodeDatabaseBuild(ctx, result, at.ID, true)
	if err != nil {
		// The database opened but the session could not be read from it. That is
		// the same stored-but-unreadable outcome a corrupt file gives: the stored
		// transcript stays, the store-side upkeep still runs, and the run
		// continues. Detail carries the first line so the warning and the
		// one-session open can say why.
		return LocatedBuild{
			Members: []LocatedMember{{
				ID:         at.ID,
				Harness:    HarnessOpenCode,
				Unreadable: UnreadableSourceFailed,
				Detail:     firstLineOr(err.Error(), "the located source could not be read"),
			}},
			Deferred: NoDeferredWrites{},
		}, nil
	}
	if !build.Found {
		return LocatedBuild{}, openCodeSourceGoneError(at.Path, at.ID, "the database holds no live row for this session")
	}
	parent := build.Located.session.ParentUUID
	if at.Stored.Parent != nil {
		parent = nil
	}
	seen := make(map[SessionID]struct{}, 1+len(build.Children))
	seen[at.ID] = struct{}{}
	members := make([]LocatedMember, 0, 1+len(build.Children)+len(at.Stored.Children))
	members = append(members, LocatedMember{
		ID:          at.ID,
		Harness:     HarnessOpenCode,
		Source:      &build.Located.session,
		ParentProbe: openCodeAncestorProbe(at.ID, parent),
	})
	for index := range build.Children {
		child := &build.Children[index]
		seen[child.identity.SessionID] = struct{}{}
		members = append(members, LocatedMember{
			ID:      child.identity.SessionID,
			Harness: HarnessOpenCode,
			Source:  &child.session,
		})
	}
	members = append(members, a.openCodeStoredChildMembers(ctx, cfg, at, seen)...)
	return LocatedBuild{Members: members, Deferred: NoDeferredWrites{}}, nil
}

// openCodeStoredChildMembers builds every stored child of a located source,
// skipping one already built for the located id. A child whose source is gone
// or unreadable comes back as a member with no source and its reason, never
// dropped: the stored transcript stays, and the store-side upkeep a full run
// does for the row still runs.
func (a *OpenCodeAdapter) openCodeStoredChildMembers(ctx context.Context, cfg SourceConfig, at LocatedSource, built map[SessionID]struct{}) []LocatedMember {
	members := make([]LocatedMember, 0, len(at.Stored.Children))
	for _, child := range at.Stored.Children {
		if _, duplicate := built[child.ID]; duplicate {
			continue
		}
		built[child.ID] = struct{}{}
		members = append(members, a.openCodeStoredChildMember(ctx, cfg, child))
	}
	return members
}

// openCodeStoredChildMember resolves one stored child either to the source the
// shared build reads or to an unreadable member that names why. The child's own
// recorded format decides which read applies; the store's link never does.
func (a *OpenCodeAdapter) openCodeStoredChildMember(ctx context.Context, cfg SourceConfig, child StoredSessionSource) LocatedMember {
	member := LocatedMember{ID: child.ID, Harness: child.Harness}
	if child.Harness != HarnessOpenCode {
		// Another harness owns this source, or none was recorded. The path may be
		// exactly where the store says, so this is a read failure, not a gone
		// source.
		member.Unreadable = UnreadableSourceFailed
		if child.Harness == "" {
			member.Detail = "the store records no harness for this child, so the opencode adapter cannot attribute its source"
		} else {
			member.Detail = "a " + string(child.Harness) + " source cannot be read by the opencode adapter"
		}
		return member
	}
	if candidate, ok := a.openCodeCandidateDatabaseFor(cfg, child.Path); ok {
		result := a.probeOpenCodeCandidate(ctx, candidate)
		if result.Support != OpenCodeSupportSupported {
			member.Unreadable = UnreadableSourceFailed
			member.Detail = firstLineOr(openCodeProbeDetail(result), "the recorded source could not be read")
			return member
		}
		build, err := a.openCodeDatabaseBuild(ctx, result, child.ID, false)
		if err != nil {
			member.Unreadable = UnreadableSourceFailed
			member.Detail = firstLineOr(err.Error(), "the recorded source could not be read")
			return member
		}
		if !build.Found {
			member.Unreadable = UnreadableSourceGone
			return member
		}
		member.Source = &build.Located.session
		return member
	}
	root, ok := openCodeLegacyJSONRootFor(cfg, child.Path)
	if !ok {
		member.Unreadable = UnreadableSourceFailed
		member.Detail = "the recorded path is outside every configured OpenCode source root"
		return member
	}
	source, ok := a.readOpenCodeLegacyJSONSource(string(child.Path), root)
	if !ok {
		if _, err := a.fs.Stat(string(child.Path)); err != nil {
			member.Unreadable = UnreadableSourceGone
			return member
		}
		member.Unreadable = UnreadableSourceFailed
		member.Detail = "the recorded file does not decode as an OpenCode session"
		return member
	}
	if source.ID != child.ID {
		member.Unreadable = UnreadableSourceGone
		return member
	}
	candidate := buildOpenCodeSession(source)
	a.hydrateOpenCodeJSONFreshness(&candidate)
	member.Source = &candidate.session
	return member
}

// DiscoverSessionAt builds the members one located OpenCode source carries. A
// database source builds the session from its one row; a legacy-JSON source
// builds it from its file. Both formats call the same build, the same
// attribution and the same freshness step the full walk calls, so a located
// member equals the one a full discovery of the same tree produces.
//
// The role and the outcome of every member are the core's: only the core knows
// whether a member was requested, related-only, or an ancestor, so this adapter
// leaves both at their zero value. Nothing is written here.
func (a *OpenCodeAdapter) DiscoverSessionAt(ctx context.Context, cfg SourceConfig, at LocatedSource) (LocatedBuild, error) {
	if err := ctx.Err(); err != nil {
		return LocatedBuild{}, err
	}
	if at.ID == "" {
		return LocatedBuild{}, fmt.Errorf(
			"ingest: OpenCodeAdapter.DiscoverSessionAt was given a located source with no session id at %q; a located source must name the session it holds so the member can be matched to the store row; where: ingest.OpenCodeAdapter.DiscoverSessionAt in internal/ingest/opencode.go; when: building a located source; impact: nothing was read and no member was built; fix: pass the id the store or the lookup resolved",
			at.Path)
	}
	if a.candidateInitErr != nil {
		return LocatedBuild{}, a.candidateInitErr
	}
	if candidate, ok := a.openCodeCandidateDatabaseFor(cfg, at.Path); ok {
		return a.discoverOpenCodeDatabaseMember(ctx, cfg, candidate, at)
	}
	root, ok := openCodeLegacyJSONRootFor(cfg, at.Path)
	if !ok {
		return LocatedBuild{}, fmt.Errorf(
			"ingest: the located OpenCode path %q is neither a candidate database nor a session file under a configured OpenCode root; a located source must be attributed to the root the full walk would have found it under, or the member would carry a different OriginalRoot than discovery gives it; where: ingest.OpenCodeAdapter.DiscoverSessionAt in internal/ingest/opencode.go; when: resolving the source root of the located path; impact: nothing was read and no member was built; fix: restore the recorded source under a configured OpenCode data root, or update the configured paths so the recorded location is inside one",
			at.Path)
	}
	return a.discoverOpenCodeLegacyJSONMember(ctx, cfg, root, at)
}

// discoverOpenCodeLegacyJSONMember builds the members one located legacy-JSON
// session file carries: the session itself and every stored child. A missing
// file, or one that names another session id, is ErrSourceGone, so the core
// takes the by-name lookup path for the id. A file that is there but does not
// decode is a read failure with the same outcome a database that does not open
// gives: a member with no source and UnreadableSourceFailed.
func (a *OpenCodeAdapter) discoverOpenCodeLegacyJSONMember(ctx context.Context, cfg SourceConfig, root ResolvedPath, at LocatedSource) (LocatedBuild, error) {
	// A path that is not there no longer holds the session. A path that is there
	// but cannot be read or decoded is a read failure, the same outcome a
	// database that does not open gives: the stored transcript stays and the
	// run continues.
	if _, err := a.fs.Stat(string(at.Path)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return LocatedBuild{}, openCodeSourceGoneError(at.Path, at.ID, "the file is gone")
		}
		return LocatedBuild{}, fmt.Errorf("ingest: the located OpenCode session file %q exists but its state cannot be observed, so the member cannot be built from it; where: ingest.OpenCodeAdapter.DiscoverSessionAt in internal/ingest/opencode.go; when: re-reading the path a previous discovery recorded; impact: no member was built from this path, and the core records the read failure; fix: make the file readable and retry: %w", at.Path, err)
	}
	source, ok := a.readOpenCodeLegacyJSONSource(string(at.Path), root)
	if !ok {
		return LocatedBuild{
			Members: []LocatedMember{{
				ID:         at.ID,
				Harness:    HarnessOpenCode,
				Unreadable: UnreadableSourceFailed,
				Detail:     "the recorded OpenCode session file does not decode as a session",
			}},
			Deferred: NoDeferredWrites{},
		}, nil
	}
	if source.ID != at.ID {
		return LocatedBuild{}, openCodeSourceGoneError(at.Path, at.ID, "the file names another session id")
	}
	session := buildOpenCodeSession(source)
	a.hydrateOpenCodeJSONFreshness(&session)
	located := LocatedMember{
		ID:      at.ID,
		Harness: HarnessOpenCode,
		Source:  &session.session,
	}
	parent := session.session.ParentUUID
	if at.Stored.Parent != nil {
		parent = nil
	}
	located.ParentProbe = openCodeAncestorProbe(at.ID, parent)

	members := make([]LocatedMember, 0, 1+len(at.Stored.Children))
	members = append(members, located)
	members = append(members, a.openCodeStoredChildMembers(ctx, cfg, at, map[SessionID]struct{}{at.ID: {}})...)
	return LocatedBuild{Members: members, Deferred: NoDeferredWrites{}}, nil
}

// openCodeLocatedDatabaseBuild is one database's located build: the session the
// caller asked for, whether the database still holds it, and every other session
// the database records as its child.
type openCodeLocatedDatabaseBuild struct {
	Located  openCodeSessionCandidate
	Found    bool
	Children []openCodeSessionCandidate
}

// openCodeDatabaseBuild builds the located session and the new children one
// database holds, from a single open source, with the same steps the full walk
// runs for them: enumerate the representations that carry each id, build through
// the shared build, apply the session row, attribute the project, read the event
// sequence, select one canonical representation per session, then hydrate
// freshness once for the whole set.
//
// A session the database no longer holds (no message row in a supported
// projection, or a usable session table without a live row) reports Found false
// with no error: that is the one-id form of the deletion rule the full walk
// applies.
//
// withChildren asks for the session's own children. The caller that is already
// merging stored children asks once for the located session; a stored child is
// built without it, so one relation step never fans out into the next.
func (a *OpenCodeAdapter) openCodeDatabaseBuild(ctx context.Context, result OpenCodeProbeResult, id SessionID, withChildren bool) (openCodeLocatedDatabaseBuild, error) {
	source, err := a.openOpenCodeSQLiteSource(ctx, result.Candidate.Path)
	if err != nil {
		return openCodeLocatedDatabaseBuild{}, err
	}
	defer a.closeScopedOpenCodeSource(ctx, source, result.Candidate.Path)

	record, hasRecord, err := a.openCodeSessionRecordByID(ctx, source, id)
	if err != nil {
		return openCodeLocatedDatabaseBuild{}, err
	}
	candidates, err := a.openCodeDatabaseCandidatesForID(ctx, source, result, id, record, hasRecord)
	if err != nil {
		return openCodeLocatedDatabaseBuild{}, err
	}
	if len(candidates) == 0 {
		return openCodeLocatedDatabaseBuild{}, nil
	}

	// The located session's children come from its own database. A child the
	// parent page does not name is not a child this source can reveal, so a
	// stored child the caller passed in is merged by the caller instead.
	var children []OpenCodeSessionRecord
	if withChildren {
		children, err = a.openCodeChildRecords(ctx, source, result.Candidate.Path, id)
		if err != nil {
			return openCodeLocatedDatabaseBuild{}, err
		}
	}
	seen := map[SessionID]struct{}{id: {}}
	for _, child := range children {
		childID, idErr := NewSessionID(child.SessionID.String())
		if idErr != nil {
			continue
		}
		if _, duplicate := seen[childID]; duplicate {
			continue
		}
		seen[childID] = struct{}{}
		built, err := a.openCodeDatabaseCandidatesForID(ctx, source, result, childID, openCodeSessionClockFromRecord(child), true)
		if err != nil {
			return openCodeLocatedDatabaseBuild{}, err
		}
		candidates = append(candidates, built...)
	}

	if attribution, attrErr := source.ProjectAttribution(ctx); attrErr != nil {
		a.recordCandidateFailure(result.Candidate.Path, OpenCodeProbeDiscover, "project attribution could not be read; the located sessions keep git-based project resolution", attrErr)
	} else {
		attributeOpenCodeProjects(candidates, attribution)
	}
	a.attributeOpenCodeEventSequences(ctx, source, result.Candidate, candidates)

	selected, err := selectCanonicalOpenCodeCandidates(candidates)
	if err != nil {
		return openCodeLocatedDatabaseBuild{}, err
	}
	hydrated := a.hydrateCanonicalOpenCodeFreshness(ctx, selected, map[string]OpenCodeSQLiteSource{filepath.Clean(result.Candidate.Path): source}, nil)

	kept := make(map[SessionID]struct{}, len(hydrated))
	for _, session := range hydrated {
		kept[session.SessionID] = struct{}{}
	}
	build := openCodeLocatedDatabaseBuild{Children: make([]openCodeSessionCandidate, 0, len(hydrated))}
	for _, candidate := range selected {
		if _, ok := kept[candidate.identity.SessionID]; !ok {
			// Hydration dropped this candidate: its freshness was unreadable.
			continue
		}
		if candidate.identity.SessionID == id {
			build.Located = candidate
			build.Found = true
			continue
		}
		build.Children = append(build.Children, candidate)
	}
	if !build.Found {
		// The session exists, but its changed time and the database mtime floor
		// were both unreadable, so the full walk would skip it and record the
		// reason. Reading the source failed; the path is not gone.
		return openCodeLocatedDatabaseBuild{}, fmt.Errorf("read the freshness of session %s from %q: neither the session clock nor the database mtime floor could be read", id, result.Candidate.Path)
	}
	return build, nil
}

// openCodeDatabaseCandidatesForID builds every representation this database can
// carry for one session id. The projection presence decides which
// representations exist, and the shared build names each one.
func (a *OpenCodeAdapter) openCodeDatabaseCandidatesForID(ctx context.Context, source OpenCodeSQLiteSource, result OpenCodeProbeResult, id SessionID, record openCodeSessionClock, hasRecord bool) ([]openCodeSessionCandidate, error) {
	current, legacy, err := a.openCodeRepresentationPresence(ctx, source, result.Candidate.Path, result.Capability, id)
	if err != nil {
		return nil, err
	}
	if !current && !legacy {
		return nil, nil
	}
	if !hasRecord && !openCodeSessionTableAbsent(result) {
		// The supported session table is usable and holds no live row for this
		// id, so its remaining message rows are history, exactly as the full
		// walk's deletion rule treats them.
		return nil, nil
	}
	var candidates []openCodeSessionCandidate
	if current {
		enumerated := openCodeSQLiteSessionSource(result.Candidate, id, TranscriptOriginOpenCodeCurrentSQLite)
		enumerated.Representation = OpenCodeRepresentationCurrentSQLite
		enumerated.Provenance = result.Candidate.Provenance
		candidate := buildOpenCodeSession(enumerated)
		controlOnly := a.currentControlOnlySessions(ctx, source, result.Candidate, []openCodeSessionSource{enumerated})
		candidate.currentControlOnly = controlOnly[id]
		candidates = append(candidates, candidate)
	}
	if legacy {
		enumerated := openCodeSQLiteSessionSource(result.Candidate, id, TranscriptOriginOpenCodeLegacySQLite)
		enumerated.Representation = OpenCodeRepresentationLegacySQLite
		enumerated.Provenance = result.Candidate.Provenance
		candidates = append(candidates, buildOpenCodeSession(enumerated))
	}
	if hasRecord {
		for index := range candidates {
			applyOpenCodeSessionRecord(&candidates[index], record)
		}
	}
	return candidates, nil
}

// openCodeChildRecords pages the located database for the rows whose parent link
// names id. The preferred table is read first and, when it is session_v2, the
// legacy session table is read too: a child created before OpenCode's migration
// lives there, and an id present in both keeps its V2 record, exactly as the
// full walk's record merge decides.
//
// Cost: the harness database is external and read-only and parent_id carries no
// index upstream, so this is not a cheap read. A parent with N children costs
// ceil(N/128) scans of that database's session table per supported session
// table (two for a mixed layout), plus one bounded presence read per child for
// each projection the schema declares and one substantive-row probe per current
// child. What bounds it: the 128-row page size, the keyset cursor that makes the
// loop terminate, and the fact that only the located database is read. None of
// it scans the peasant store.
func (a *OpenCodeAdapter) openCodeChildRecords(ctx context.Context, source OpenCodeSQLiteSource, database string, parent SessionID) ([]OpenCodeSessionRecord, error) {
	preferred, table, err := a.openCodeChildRecordPages(ctx, source, database, parent, OpenCodeSessionRecordsPreferred)
	if err != nil {
		return nil, err
	}
	if table != OpenCodeSessionTableV2 {
		return preferred, nil
	}
	legacy, _, err := a.openCodeChildRecordPages(ctx, source, database, parent, OpenCodeSessionRecordsLegacy)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(preferred))
	for _, record := range preferred {
		seen[record.SessionID.String()] = struct{}{}
	}
	records := preferred
	for _, record := range legacy {
		if _, duplicate := seen[record.SessionID.String()]; duplicate {
			continue
		}
		seen[record.SessionID.String()] = struct{}{}
		records = append(records, record)
	}
	return records, nil
}

// openCodeChildRecordPages pages one table's children of parent to completion,
// one bounded page at a time. Each page is bounded by the page size; the keyset
// cursor is the last observed session id, so a page never re-reads a row and the
// read always advances. The page count is the child count divided by the page
// size, and each page may scan the harness session table because parent_id is
// unindexed upstream.
func (a *OpenCodeAdapter) openCodeChildRecordPages(ctx context.Context, source OpenCodeSQLiteSource, database string, parent SessionID, selection OpenCodeSessionRecordSelection) ([]OpenCodeSessionRecord, OpenCodeSessionTable, error) {
	pageSize, err := NewOpenCodeCurrentPageSize(openCodeCurrentMaterializePage)
	if err != nil {
		return nil, "", err
	}
	linkID, err := NewOpenCodeSessionLinkID(string(parent))
	if err != nil {
		return nil, "", err
	}
	var records []OpenCodeSessionRecord
	var table OpenCodeSessionTable
	var cursor *OpenCodeSessionRecordCursor
	for {
		page, readErr := source.SessionRecords(ctx, OpenCodeSessionRecordPageRequest{Selection: selection, PageSize: pageSize, ParentID: &linkID, After: cursor})
		if readErr != nil {
			return nil, "", fmt.Errorf("read the children of session %s from %q: %w", parent, database, readErr)
		}
		if page.Table != "" {
			table = page.Table
		}
		records = append(records, page.Records...)
		if page.Next == nil {
			return records, table, nil
		}
		if cursor != nil && page.Next.sessionID == cursor.sessionID {
			return nil, "", fmt.Errorf("read the children of session %s from %q: the page cursor did not advance past session %s, so a bounded read could repeat forever; no partial child list is eligible; inspect the source session table and retry", parent, database, cursor.sessionID.String())
		}
		cursor = page.Next
	}
}

// openCodeRepresentationPresence reports which supported projection carries the
// id. One bounded page of one row per projection is enough: the projections the
// capability names are exactly the ones a full walk enumerates, so nothing is
// read from a table the schema does not declare.
func (a *OpenCodeAdapter) openCodeRepresentationPresence(ctx context.Context, source OpenCodeSQLiteSource, database string, capability OpenCodeSchemaCapability, id SessionID) (bool, bool, error) {
	currentPageSize, err := NewOpenCodeCurrentPageSize(1)
	if err != nil {
		return false, false, err
	}
	legacyPageSize, err := NewOpenCodeLegacyPageSize(1)
	if err != nil {
		return false, false, err
	}
	var current, legacy bool
	switch capability {
	case OpenCodeCapabilityCurrent, OpenCodeCapabilityHybrid:
		currentID, idErr := NewOpenCodeCurrentSessionID(string(id))
		if idErr != nil {
			return false, false, idErr
		}
		page, readErr := source.CurrentMessages(ctx, OpenCodeCurrentPageRequest{SessionID: currentID, PageSize: currentPageSize})
		if readErr != nil {
			return false, false, fmt.Errorf("read the current projection of session %s from %q: %w", id, database, readErr)
		}
		current = len(page.Messages) > 0
	}
	switch capability {
	case OpenCodeCapabilityLegacy, OpenCodeCapabilityHybrid:
		legacyID, idErr := NewOpenCodeLegacySessionID(string(id))
		if idErr != nil {
			return false, false, idErr
		}
		page, readErr := source.LegacyMessages(ctx, OpenCodeLegacyMessagePageRequest{SessionID: legacyID, PageSize: legacyPageSize})
		if readErr != nil {
			return false, false, fmt.Errorf("read the legacy projection of session %s from %q: %w", id, database, readErr)
		}
		legacy = len(page.Messages) > 0
	}
	return current, legacy, nil
}

// openCodeSessionTableAbsent reports whether the probed schema has no usable
// session metadata table, so a missing row proves nothing about deletion.
func openCodeSessionTableAbsent(result OpenCodeProbeResult) bool {
	return result.Evidence.SessionColumns == nil && result.Evidence.SessionV2Columns == nil
}

// hydrateOpenCodeJSONFreshness sets a legacy-JSON winner's changed time from its
// own file mtime, the way the full walk's hydration step does.
func (a *OpenCodeAdapter) hydrateOpenCodeJSONFreshness(candidate *openCodeSessionCandidate) {
	if info, err := a.fs.Stat(candidate.identity.Path.String()); err == nil {
		candidate.session.ModTime = info.ModTime()
	}
}

// openCodeAncestorProbe is the one probe a located OpenCode source declares for
// its unstored parent, or nil when there is nothing to locate. The probe is a
// pure value: it names the parent id and performs no I/O.
func openCodeAncestorProbe(id SessionID, parent *SessionID) *AncestorProbe {
	if parent == nil || *parent == "" || *parent == id {
		return nil
	}
	return &AncestorProbe{Kind: openCodeAncestorProbeKind, Harness: HarnessOpenCode, ID: *parent}
}

// probeOpenCodeCandidate inspects one candidate in isolation, so a located
// build decides the schema capability from the same evidence the walk reads,
// and never opens a database the candidate list does not name.
func (a *OpenCodeAdapter) probeOpenCodeCandidate(ctx context.Context, candidate OpenCodeCandidate) OpenCodeProbeResult {
	if a.candidateProber == nil {
		return OpenCodeProbeResult{Candidate: candidate}
	}
	results := a.candidateProber.Probe(ctx, []OpenCodeCandidate{candidate})
	if len(results) != 1 {
		return OpenCodeProbeResult{Candidate: candidate}
	}
	return results[0]
}

// openCodeCandidateDatabaseFor returns the candidate database a located path
// names, or false when the configured roots do not name it. A database the
// candidate list does not name is never opened.
func (a *OpenCodeAdapter) openCodeCandidateDatabaseFor(cfg SourceConfig, path ResolvedPath) (OpenCodeCandidate, bool) {
	clean := filepath.Clean(string(path))
	if clean == ":memory:" {
		return OpenCodeCandidate{}, false
	}
	for _, candidate := range a.resolveOpenCodeCandidateSources(cfg.Paths) {
		if candidate.Kind != OpenCodeSourceSQLite {
			continue
		}
		if filepath.Clean(candidate.Path) == clean {
			return candidate, true
		}
	}
	return OpenCodeCandidate{}, false
}

// openCodeLegacyJSONRootFor returns the configured OpenCode root a legacy-JSON
// session path hangs from. Its layout is
// {root}/storage/session/{hash}/ses_{id}.json, the only shape the full walk
// admits, so that is the only shape a located build accepts.
func openCodeLegacyJSONRootFor(cfg SourceConfig, path ResolvedPath) (ResolvedPath, bool) {
	clean := filepath.Clean(string(path))
	base := filepath.Base(clean)
	if !strings.HasPrefix(base, defaults.OpenCodeSessionPrefix) || !strings.HasSuffix(base, defaults.ExtJSON.String()) {
		return "", false
	}
	sessionDir := filepath.Dir(filepath.Dir(clean))
	if filepath.Base(sessionDir) != defaults.OpenCodeDirSession.String() {
		return "", false
	}
	storageDir := filepath.Dir(sessionDir)
	if filepath.Base(storageDir) != defaults.OpenCodeDirStorage.String() {
		return "", false
	}
	root := filepath.Dir(storageDir)
	for _, configured := range cfg.Paths {
		if filepath.Clean(string(configured)) == root {
			return configured, true
		}
	}
	return "", false
}

// openCodeLegacyJSONSearchPattern is the shape the legacy-JSON lookup was asked
// for, for the miss error's searched block. The id sits in the file body, so the
// pattern names where a session file would be; it never names a file that
// happens to exist.
func openCodeLegacyJSONSearchPattern(root ResolvedPath, id SessionID) string {
	return filepath.Join(
		openCodeDisplayPath(string(root)),
		defaults.OpenCodeDirStorage.String(),
		defaults.OpenCodeDirSession.String(),
		"*",
		string(id)+defaults.ExtJSON.String(),
	)
}

// openCodeDisplayPath renders a source path with the home directory shortened,
// the way the searched block shows every location.
func openCodeDisplayPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}

// openCodeSourceGoneError is the one statement of "this recorded path no longer
// holds the session it was recorded for". It wraps ErrSourceGone, so the core
// takes the by-name lookup path for the id.
func openCodeSourceGoneError(path ResolvedPath, id SessionID, because string) error {
	return fmt.Errorf(
		"ingest: the located OpenCode path %q no longer holds session %s because %s; an OpenCode source is named by the file or database the session is read from, so this path is not that session's source; where: ingest.OpenCodeAdapter.DiscoverSessionAt in internal/ingest/opencode.go; when: re-reading the path a previous discovery recorded; impact: no member was built from this path; fix: let the by-name lookup find the source the session is in now: %w", path, id, because, ErrSourceGone)
}

// openCodeUnsupportedSourceError states that a candidate database cannot supply
// a session because its schema is outside the supported set. The full walk skips
// such a database entirely; a located build says why instead of reporting a
// miss.
func openCodeUnsupportedSourceError(path ResolvedPath, id SessionID, result OpenCodeProbeResult) error {
	detail := "its schema is outside the supported OpenCode set"
	if len(result.Diagnostics) > 0 {
		detail = result.Diagnostics[0].Why
	}
	return fmt.Errorf(
		"ingest: the located OpenCode source %q cannot supply session %s because %s; a database whose schema is unsupported is not enumerated by a full harvest, so no member can be built from it; where: ingest.OpenCodeAdapter.DiscoverSessionAt in internal/ingest/opencode.go; when: building a located database source; impact: no member was built from this database; fix: let OpenCode upgrade or repair the database, or run a full harvest once it is supported", path, id, detail)
}

// openCodeProbeDetail names why a candidate could not provide a source, in one
// line, for a member's Detail.
func openCodeProbeDetail(result OpenCodeProbeResult) string {
	if len(result.Diagnostics) > 0 {
		return result.Diagnostics[0].Why
	}
	return "the source could not be read as a supported OpenCode database"
}

// closeScopedOpenCodeSource releases one source opened for a located build. The
// close context is derived from context.Background, so a cancelled build still
// releases the single connection, bounded by the source query timeout.
func (a *OpenCodeAdapter) closeScopedOpenCodeSource(ctx context.Context, source OpenCodeSQLiteSource, path string) {
	if err := closeDeferredOpenCodeSource(ctx, source, a.candidateOptions.queryTimeout); err != nil {
		a.recordCandidateFailure(path, OpenCodeProbeFreshness, "located SQLite source did not close cleanly after the scoped read", err)
	}
}
