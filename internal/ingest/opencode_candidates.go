package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	openCodeDatabaseOverrideEnv    = "OPENCODE_DB"
	openCodeDisableChannelEnv      = "OPENCODE_DISABLE_CHANNEL_DB"
	openCodeInstallationChannelEnv = "OPENCODE_CHANNEL"
	openCodeSQLiteHeader           = "SQLite format 3\x00"
	openCodeCatalogRowLimit        = 256
	openCodeColumnRowLimit         = 32
	openCodeSessionColumnRowLimit  = 64
	openCodeIndexRowLimit          = 64
)

// OpenCodeSourceKind identifies the physical source without implying whether
// its schema is supported or whether it is eligible for ingestion.
type OpenCodeSourceKind string

const (
	OpenCodeSourceSQLite     OpenCodeSourceKind = "sqlite"
	OpenCodeSourceLegacyJSON OpenCodeSourceKind = "legacy_json"
)

// OpenCodeCanonicalRepresentation identifies one materialized transcript
// representation. The closed set deliberately excludes event, input, context,
// migration, and external-output storage.
type OpenCodeCanonicalRepresentation uint8

const (
	OpenCodeRepresentationLegacyJSON OpenCodeCanonicalRepresentation = iota + 1
	OpenCodeRepresentationLegacySQLite
	OpenCodeRepresentationCurrentSQLite
)

// Validate rejects values outside the selectable transcript representations.
func (r OpenCodeCanonicalRepresentation) Validate() error {
	switch r {
	case OpenCodeRepresentationLegacyJSON, OpenCodeRepresentationLegacySQLite, OpenCodeRepresentationCurrentSQLite:
		return nil
	default:
		return fmt.Errorf("OpenCode canonical representation %d is outside the supported closed set", r)
	}
}

func (r OpenCodeCanonicalRepresentation) precedence() uint8 {
	switch r {
	case OpenCodeRepresentationCurrentSQLite:
		return 3
	case OpenCodeRepresentationLegacySQLite:
		return 2
	case OpenCodeRepresentationLegacyJSON:
		return 1
	default:
		return 0
	}
}

// OpenCodeSelectedSourceIdentity is the complete freshness identity selected
// for one raw OpenCode session. Diffing must use only the matching session's
// freshness evidence from this representation and path.
type OpenCodeSelectedSourceIdentity struct {
	SessionID      SessionID
	Representation OpenCodeCanonicalRepresentation
	Path           ResolvedPath
}

// Validate rejects incomplete selected-source identities before diffing.
func (i OpenCodeSelectedSourceIdentity) Validate() error {
	if i.SessionID == "" {
		return errors.New("selected OpenCode source identity has no session ID")
	}
	if err := i.Representation.Validate(); err != nil {
		return err
	}
	if i.Path == "" {
		return errors.New("selected OpenCode source identity has no attributable path")
	}
	return nil
}

// OpenCodeGraphDiagnosticCode identifies a non-fatal selected-graph repair.
type OpenCodeGraphDiagnosticCode string

const (
	OpenCodeGraphMissingParent     OpenCodeGraphDiagnosticCode = "opencode_missing_parent"
	OpenCodeGraphOrphanPartDropped OpenCodeGraphDiagnosticCode = "opencode_orphan_part_dropped"
	// OpenCodeUnknownPartType records that a well-formed row carried a type
	// outside the known transcript vocabulary. It is not corruption: the session
	// still materializes, and one diagnostic per distinct type names it.
	OpenCodeUnknownPartType OpenCodeGraphDiagnosticCode = "opencode_unknown_part_type"
)

// Validate rejects graph diagnostics outside the supported repair contract.
func (c OpenCodeGraphDiagnosticCode) Validate() error {
	switch c {
	case OpenCodeGraphMissingParent, OpenCodeGraphOrphanPartDropped, OpenCodeUnknownPartType:
		return nil
	default:
		return fmt.Errorf("OpenCode graph diagnostic code %q is outside the supported closed set", c)
	}
}

type openCodeSessionCandidate struct {
	session    DiscoveredSession
	identity   OpenCodeSelectedSourceIdentity
	provenance OpenCodeCandidateProvenance
	// sessionUpdatedAt is this one session's usable upstream clock
	// (session.time_updated). It is the single optional clock value per
	// session: a zero value means the session has no usable clock, so the
	// database and WAL mtime floor applies instead. A session whose database
	// has the session table but no row for it, or whose row has a null or zero
	// time_updated, has a zero value here and takes the mtime-floor path.
	sessionUpdatedAt time.Time
	// currentControlOnly marks a current SQLite candidate whose session_message
	// projection holds only control records, such as a model or agent switch,
	// and no substantive user, assistant, or tool row. Such a current candidate
	// loses canonical selection to a legacy sibling that still carries the
	// conversation, so the reader renders the real turns instead of one to three
	// inert control turns. The flag is meaningful only for the current
	// representation; a legacy representation, once discovered, always carries
	// substantive rows, and its zero value keeps the pre-existing preference for
	// a substantive current candidate.
	currentControlOnly bool
}

// OpenCodeCandidateProvenance records why a path was considered. Candidate
// order is override database, channel/default database, then legacy JSON root.
type OpenCodeCandidateProvenance string

const (
	OpenCodeCandidateOverride       OpenCodeCandidateProvenance = "environment_override"
	OpenCodeCandidateChannel        OpenCodeCandidateProvenance = "channel_database"
	OpenCodeCandidateLegacyJSONRoot OpenCodeCandidateProvenance = "legacy_json_root"
)

// OpenCodeSchemaCapability describes only structural SQLite capability.
type OpenCodeSchemaCapability string

const (
	OpenCodeCapabilityNone    OpenCodeSchemaCapability = "none"
	OpenCodeCapabilityLegacy  OpenCodeSchemaCapability = "legacy_message_part"
	OpenCodeCapabilityCurrent OpenCodeSchemaCapability = "current_session_message"
	OpenCodeCapabilityHybrid  OpenCodeSchemaCapability = "hybrid"
)

// OpenCodeSchemaSupport describes the outcome of candidate inspection. It is
// intentionally separate from source kind and structural capability.
type OpenCodeSchemaSupport string

const (
	OpenCodeSupportSupported   OpenCodeSchemaSupport = "supported"
	OpenCodeSupportPartial     OpenCodeSchemaSupport = "partial"
	OpenCodeSupportUnsupported OpenCodeSchemaSupport = "unsupported"
	OpenCodeSupportCorrupt     OpenCodeSchemaSupport = "corrupt"
	OpenCodeSupportUnreadable  OpenCodeSchemaSupport = "unreadable"
)

// OpenCodeProbeStage identifies where a candidate-local diagnostic arose.
type OpenCodeProbeStage string

const (
	OpenCodeProbeValidate OpenCodeProbeStage = "validate_path"
	OpenCodeProbeStat     OpenCodeProbeStage = "stat_path"
	OpenCodeProbeHeader   OpenCodeProbeStage = "sniff_header"
	OpenCodeProbeOpen     OpenCodeProbeStage = "open_source"
	OpenCodeProbeCatalog  OpenCodeProbeStage = "inspect_catalog"
	// OpenCodeProbeDiscover covers session enumeration and parent-link reads.
	OpenCodeProbeDiscover OpenCodeProbeStage = "discover_sessions"
	// OpenCodeProbeFreshness covers selected-session freshness hydration.
	OpenCodeProbeFreshness OpenCodeProbeStage = "hydrate_freshness"
)

// OpenCodeProbeDiagnosticCode is the machine-readable reason for a diagnostic.
type OpenCodeProbeDiagnosticCode string

const (
	OpenCodeDiagnosticInvalidCandidate  OpenCodeProbeDiagnosticCode = "invalid_candidate"
	OpenCodeDiagnosticPathUnavailable   OpenCodeProbeDiagnosticCode = "path_unavailable"
	OpenCodeDiagnosticInvalidHeader     OpenCodeProbeDiagnosticCode = "invalid_sqlite_header"
	OpenCodeDiagnosticSourceOpenFailed  OpenCodeProbeDiagnosticCode = "source_open_failed"
	OpenCodeDiagnosticCatalogReadFailed OpenCodeProbeDiagnosticCode = "catalog_read_failed"
	OpenCodeDiagnosticCatalogTruncated  OpenCodeProbeDiagnosticCode = "catalog_truncated"
	OpenCodeDiagnosticSchemaIncomplete  OpenCodeProbeDiagnosticCode = "schema_incomplete"
	// OpenCodeDiagnosticDiscoveryFailed marks a supported candidate that was
	// skipped during one discovery run.
	OpenCodeDiagnosticDiscoveryFailed OpenCodeProbeDiagnosticCode = "discovery_failed"
)

// OpenCodeCandidate is one deduplicated source location. SQLite candidates are
// evidence only and are never converted to DiscoveredSession values.
type OpenCodeCandidate struct {
	Path       string
	Kind       OpenCodeSourceKind
	Provenance OpenCodeCandidateProvenance
}

// NewOpenCodeCandidate validates a typed candidate without inspecting it.
func NewOpenCodeCandidate(path string, kind OpenCodeSourceKind, provenance OpenCodeCandidateProvenance) (OpenCodeCandidate, error) {
	candidate := OpenCodeCandidate{Path: path, Kind: kind, Provenance: provenance}
	if err := candidate.Validate(); err != nil {
		return OpenCodeCandidate{}, err
	}
	return candidate, nil
}

// Validate rejects unknown or inconsistent closed-set candidate values.
func (c OpenCodeCandidate) Validate() error {
	if err := c.Kind.Validate(); err != nil {
		return fmt.Errorf("validate OpenCode candidate %q failed before filesystem inspection: %w; no source was accessed; construct the candidate with a supported source kind", c.Path, err)
	}
	if err := c.Provenance.Validate(); err != nil {
		return fmt.Errorf("validate OpenCode candidate %q failed before filesystem inspection: %w; no source was accessed; construct the candidate with resolver-owned provenance", c.Path, err)
	}
	if c.Path == "" {
		return fmt.Errorf("validate OpenCode candidate failed before filesystem inspection: path is empty, so the source cannot be attributed or inspected; no source was accessed; provide the resolved candidate path")
	}
	if c.Kind == OpenCodeSourceLegacyJSON && c.Provenance != OpenCodeCandidateLegacyJSONRoot {
		return fmt.Errorf("validate OpenCode candidate %q failed before filesystem inspection: legacy JSON kind has incompatible provenance %q; no source was accessed; use legacy_json_root provenance for legacy discovery", c.Path, c.Provenance)
	}
	if c.Kind == OpenCodeSourceSQLite && c.Provenance == OpenCodeCandidateLegacyJSONRoot {
		return fmt.Errorf("validate OpenCode candidate %q failed before filesystem inspection: SQLite kind has incompatible legacy-root provenance; no source was accessed; use environment_override or channel_database provenance", c.Path)
	}
	return nil
}

// Validate rejects unknown physical source kinds.
func (k OpenCodeSourceKind) Validate() error {
	switch k {
	case OpenCodeSourceSQLite, OpenCodeSourceLegacyJSON:
		return nil
	default:
		return fmt.Errorf("source kind %q is outside the supported closed set", k)
	}
}

// precedence ranks provenance for equal-representation tie-breaks. The
// environment override outranks the channel database, which outranks the
// legacy JSON root. This mirrors the resolver order.
func (p OpenCodeCandidateProvenance) precedence() uint8 {
	switch p {
	case OpenCodeCandidateOverride:
		return 3
	case OpenCodeCandidateChannel:
		return 2
	case OpenCodeCandidateLegacyJSONRoot:
		return 1
	default:
		return 0
	}
}

// Validate rejects unknown candidate provenance values.
func (p OpenCodeCandidateProvenance) Validate() error {
	switch p {
	case OpenCodeCandidateOverride, OpenCodeCandidateChannel, OpenCodeCandidateLegacyJSONRoot:
		return nil
	default:
		return fmt.Errorf("candidate provenance %q is outside the supported closed set", p)
	}
}

// Validate rejects unknown schema support states at trust boundaries.
func (s OpenCodeSchemaSupport) Validate() error {
	switch s {
	case OpenCodeSupportSupported, OpenCodeSupportPartial, OpenCodeSupportUnsupported, OpenCodeSupportCorrupt, OpenCodeSupportUnreadable:
		return nil
	default:
		return fmt.Errorf("schema support %q is outside the supported closed set", s)
	}
}

// Validate rejects unknown structural capabilities.
func (c OpenCodeSchemaCapability) Validate() error {
	switch c {
	case OpenCodeCapabilityNone, OpenCodeCapabilityLegacy, OpenCodeCapabilityCurrent, OpenCodeCapabilityHybrid:
		return nil
	default:
		return fmt.Errorf("schema capability %q is outside the supported closed set", c)
	}
}

// OpenCodeProbeDiagnostic is an actionable failure local to one candidate.
type OpenCodeProbeDiagnostic struct {
	Code        OpenCodeProbeDiagnosticCode
	Stage       OpenCodeProbeStage
	What        string
	Why         string
	Where       string
	When        string
	Meaning     string
	Remediation string
}

// OpenCodeColumnEvidence is bounded catalog evidence for one required column.
type OpenCodeColumnEvidence struct {
	Name    string
	NotNull bool
	Primary bool
}

// OpenCodeIndexKeyEvidence records one bounded index_xinfo term.
type OpenCodeIndexKeyEvidence struct {
	Sequence   int64
	ColumnID   int64
	Name       string
	Descending bool
	Collation  string
	Key        bool
}

// OpenCodeIndexEvidence is bounded ordering evidence from one SQLite index.
type OpenCodeIndexEvidence struct {
	Name    string
	Unique  bool
	Partial bool
	Keys    []OpenCodeIndexKeyEvidence
}

// OpenCodeSchemaEvidence contains catalog shape only. It never contains
// transcript payloads, session rows, migration history, or event data.
type OpenCodeSchemaEvidence struct {
	Tables                []string
	SessionColumns        []OpenCodeColumnEvidence
	SessionV2Columns      []OpenCodeColumnEvidence
	LegacyMessageColumns  []OpenCodeColumnEvidence
	LegacyPartColumns     []OpenCodeColumnEvidence
	CurrentMessageColumns []OpenCodeColumnEvidence
	CurrentIndexes        []OpenCodeIndexEvidence
}

// OpenCodeSessionV2Layout reports structural evidence independently of the
// transcript representation. The zero value means catalog inspection failed.
type OpenCodeSessionV2Layout string

const (
	OpenCodeSessionV2Absent      OpenCodeSessionV2Layout = "absent"
	OpenCodeSessionV2Supported   OpenCodeSessionV2Layout = "supported"
	OpenCodeSessionV2Unsupported OpenCodeSessionV2Layout = "unsupported"
)

// OpenCodeProbeResult is the complete, non-ingestible observation for one
// candidate. Candidate failures do not prevent later candidates from probing.
type OpenCodeProbeResult struct {
	V2Layout OpenCodeSessionV2Layout
	// SessionTable is the authority actually read during discovery, including a
	// read that failed after selection. Zero means none was selected.
	SessionTable OpenCodeSessionTable
	Candidate    OpenCodeCandidate
	Capability   OpenCodeSchemaCapability
	Support      OpenCodeSchemaSupport
	Evidence     OpenCodeSchemaEvidence
	Diagnostics  []OpenCodeProbeDiagnostic
}

// OpenCodeEnvironmentLookup keeps environment access injectable.
type OpenCodeEnvironmentLookup interface {
	LookupEnv(string) (string, bool)
}

type systemOpenCodeEnvironment struct{}

func (systemOpenCodeEnvironment) LookupEnv(key string) (string, bool) { return os.LookupEnv(key) }

// SystemOpenCodeEnvironment returns the production environment lookup. Tests
// should inject a synthetic lookup and never resolve HOME or XDG state.
func SystemOpenCodeEnvironment() OpenCodeEnvironmentLookup { return systemOpenCodeEnvironment{} }

// ResolveOpenCodeCandidates mirrors OpenCode's database naming rules while
// retaining the legacy JSON root. The fixed precedence is environment
// override, computed channel/default database, then legacy JSON root. Earlier
// provenance wins when clean paths deduplicate.
func ResolveOpenCodeCandidates(dataRoot, channel string, environment OpenCodeEnvironmentLookup) ([]OpenCodeCandidate, error) {
	if dataRoot == "" || !filepath.IsAbs(dataRoot) {
		return nil, fmt.Errorf("resolve OpenCode candidates failed before environment inspection: data root %q is not an absolute path, so relative database overrides cannot be anchored safely; no filesystem path was inspected; inject the absolute OpenCode data root", dataRoot)
	}
	if strings.TrimSpace(channel) == "" {
		return nil, fmt.Errorf("resolve OpenCode candidates failed before environment inspection: installation channel %q is empty or whitespace, so the upstream database filename cannot be reproduced; no filesystem path was inspected; inject the compiled OpenCode channel", channel)
	}
	if environment == nil {
		return nil, fmt.Errorf("resolve OpenCode candidates failed before environment inspection: environment lookup is nil, so OPENCODE_DB and OPENCODE_DISABLE_CHANNEL_DB cannot be evaluated deterministically; no filesystem path was inspected; inject an environment lookup")
	}

	root := filepath.Clean(dataRoot)
	candidates := make([]OpenCodeCandidate, 0, 3)
	if override, ok := environment.LookupEnv(openCodeDatabaseOverrideEnv); ok && override != "" {
		resolved := override
		if override != ":memory:" && !filepath.IsAbs(override) {
			resolved = filepath.Join(root, override)
		}
		candidate, err := NewOpenCodeCandidate(resolved, OpenCodeSourceSQLite, OpenCodeCandidateOverride)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}

	disableValue, _ := environment.LookupEnv(openCodeDisableChannelEnv)
	databaseName := "opencode.db"
	if channel != "latest" && channel != "beta" && channel != "prod" && disableValue != "1" && disableValue != "true" {
		databaseName = "opencode-" + sanitizeOpenCodeChannel(channel) + ".db"
	}
	channelCandidate, err := NewOpenCodeCandidate(filepath.Join(root, databaseName), OpenCodeSourceSQLite, OpenCodeCandidateChannel)
	if err != nil {
		return nil, err
	}
	legacyCandidate, err := NewOpenCodeCandidate(root, OpenCodeSourceLegacyJSON, OpenCodeCandidateLegacyJSONRoot)
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, channelCandidate, legacyCandidate)

	seen := make(map[string]struct{}, len(candidates))
	deduplicated := candidates[:0]
	for _, candidate := range candidates {
		keyPath := candidate.Path
		if keyPath != ":memory:" {
			keyPath = filepath.Clean(keyPath)
		}
		key := string(candidate.Kind) + "\x00" + keyPath
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		deduplicated = append(deduplicated, candidate)
	}
	return deduplicated, nil
}

func sanitizeOpenCodeChannel(channel string) string {
	var result strings.Builder
	result.Grow(len(channel))
	for _, char := range channel {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '.' || char == '_' || char == '-' {
			result.WriteRune(char)
		} else {
			result.WriteByte('-')
		}
	}
	return result.String()
}

// OpenCodeCandidateFileSystem is the bounded filesystem surface required by
// probing. It deliberately has no write methods.
type OpenCodeCandidateFileSystem interface {
	Stat(string) (os.FileInfo, error)
	Open(string) (io.ReadCloser, error)
}

// OpenCodeSQLiteSourceOpener injects the restrictive source constructor.
type OpenCodeSQLiteSourceOpener func(context.Context, OpenCodeSQLiteSourcePath, OpenCodeSQLiteSourceOptions) (OpenCodeSQLiteSource, error)

// OpenCodeCandidateProber performs bounded, data-free source inspection.
type OpenCodeCandidateProber struct {
	fs      OpenCodeCandidateFileSystem
	open    OpenCodeSQLiteSourceOpener
	options OpenCodeSQLiteSourceOptions
}

// NewOpenCodeCandidateProber constructs a prober with no environment or path
// defaults. All dependencies are explicit and injectable.
func NewOpenCodeCandidateProber(fs OpenCodeCandidateFileSystem, opener OpenCodeSQLiteSourceOpener, options OpenCodeSQLiteSourceOptions) (*OpenCodeCandidateProber, error) {
	if fs == nil {
		return nil, fmt.Errorf("construct OpenCode candidate prober failed before filesystem inspection: filesystem is nil, so candidate type and header cannot be validated; inject a read-only stat/open filesystem")
	}
	if opener == nil {
		return nil, fmt.Errorf("construct OpenCode candidate prober failed before filesystem inspection: SQLite source opener is nil, so restrictive catalog reads cannot run; inject OpenOpenCodeSQLiteSource or a contract-compatible test opener")
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	return &OpenCodeCandidateProber{fs: fs, open: opener, options: options}, nil
}

// Probe inspects every candidate in order. A failed candidate produces a typed
// local result and never suppresses inspection of the remaining candidates.
func (p *OpenCodeCandidateProber) Probe(ctx context.Context, candidates []OpenCodeCandidate) []OpenCodeProbeResult {
	results := make([]OpenCodeProbeResult, 0, len(candidates))
	for _, candidate := range candidates {
		results = append(results, p.probeCandidate(ctx, candidate))
	}
	return results
}

func (p *OpenCodeCandidateProber) probeCandidate(ctx context.Context, candidate OpenCodeCandidate) OpenCodeProbeResult {
	result := OpenCodeProbeResult{Candidate: candidate, Capability: OpenCodeCapabilityNone}
	diagnosticLocation := candidate.Path
	if diagnosticLocation == "" {
		diagnosticLocation = "OpenCode candidate with empty path"
	}
	if err := candidate.Validate(); err != nil {
		return failedOpenCodeProbe(result, OpenCodeSupportUnsupported, OpenCodeProbeValidate, "candidate typed contract is invalid", err.Error(), diagnosticLocation, "before filesystem inspection", "the candidate cannot provide attributable source evidence", "construct candidates through ResolveOpenCodeCandidates or NewOpenCodeCandidate")
	}
	if candidate.Path == "" || candidate.Path == ":memory:" || !filepath.IsAbs(candidate.Path) {
		return failedOpenCodeProbe(result, OpenCodeSupportUnsupported, OpenCodeProbeValidate, "candidate path is not production filesystem evidence", fmt.Sprintf("path %q is empty, relative, or in-memory", candidate.Path), diagnosticLocation, "before filesystem inspection", "the candidate cannot identify a durable OpenCode-owned file or directory", "use an absolute filesystem path; reserve :memory: only for upstream tests")
	}

	info, err := p.fs.Stat(candidate.Path)
	if err != nil {
		why := err.Error()
		if errors.Is(err, os.ErrNotExist) {
			why = "the candidate does not exist"
		}
		return failedOpenCodeProbe(result, OpenCodeSupportUnreadable, OpenCodeProbeStat, "candidate path could not be validated", why, candidate.Path, "while statting the resolved candidate", "no source capability was inferred and other candidates remain eligible", "verify the configured OpenCode data root, override, channel, and filesystem permissions")
	}
	if candidate.Kind == OpenCodeSourceLegacyJSON {
		if !info.IsDir() {
			return failedOpenCodeProbe(result, OpenCodeSupportUnsupported, OpenCodeProbeStat, "legacy JSON candidate is not a directory", fmt.Sprintf("filesystem mode is %s", info.Mode()), candidate.Path, "while validating the legacy OpenCode root", "the existing JSON adapter cannot traverse storage/session beneath this candidate", "point the legacy candidate at the OpenCode data directory")
		}
		result.Support = OpenCodeSupportSupported
		return result
	}
	if !info.Mode().IsRegular() {
		return failedOpenCodeProbe(result, OpenCodeSupportUnsupported, OpenCodeProbeStat, "SQLite candidate is not a regular file", fmt.Sprintf("filesystem mode is %s", info.Mode()), candidate.Path, "while validating the database candidate", "the path is not acceptable SQLite file evidence", "configure OPENCODE_DB or the data root to name a regular database file")
	}
	if err := p.sniffSQLiteHeader(candidate.Path); err != nil {
		return failedOpenCodeProbe(result, OpenCodeSupportCorrupt, OpenCodeProbeHeader, "SQLite header validation failed", err.Error(), candidate.Path, "before opening the restrictive source", "the file is not trusted as SQLite evidence and no catalog query ran", "select a valid OpenCode SQLite database or repair the upstream installation without using Peasant")
	}

	path, err := NewOpenCodeSQLiteSourcePath(candidate.Path)
	if err != nil {
		return failedOpenCodeProbe(result, OpenCodeSupportUnsupported, OpenCodeProbeValidate, "SQLite source path validation failed", err.Error(), candidate.Path, "after header validation and before source open", "catalog capability cannot be inspected", "supply an absolute regular-file path")
	}
	source, err := p.open(ctx, path, p.options)
	if err != nil {
		return failedOpenCodeProbe(result, OpenCodeSupportUnreadable, OpenCodeProbeOpen, "restrictive SQLite source open failed", err.Error(), candidate.Path, "after header validation and before catalog inspection", "the file may be SQLite but no schema support claim can be made", "verify read permission and source health, then retry while OpenCode owns all repair or migration")
	}
	evidence, inspectErr := source.Catalog(ctx)
	closeErr := source.Close(context.Background())
	if inspectErr != nil || closeErr != nil {
		joined := errors.Join(inspectErr, closeErr)
		result.Evidence = evidence
		var overflow *OpenCodeCatalogOverflowError
		if errors.As(joined, &overflow) {
			result.Support = OpenCodeSupportPartial
			result.Diagnostics = append(result.Diagnostics, OpenCodeProbeDiagnostic{
				Code:        OpenCodeDiagnosticCatalogTruncated,
				Stage:       OpenCodeProbeCatalog,
				What:        "bounded SQLite catalog evidence is incomplete",
				Why:         overflow.Error(),
				Where:       candidate.Path,
				When:        "while retaining explicit structural catalog rows",
				Meaning:     "the candidate cannot be classified as supported and remains ineligible for ingestion",
				Remediation: "reduce unrelated upstream schema objects or use a supported OpenCode database; do not modify, copy, checkpoint, migrate, or repair it through Peasant",
			})
			return result
		}
		return failedOpenCodeProbe(result, OpenCodeSupportUnreadable, OpenCodeProbeCatalog, "bounded SQLite catalog inspection failed", joined.Error(), candidate.Path, "while reading explicit schema catalog columns", "schema capability is incomplete and the candidate is not ingestible", "verify source readability and retry; do not migrate, checkpoint, or repair it through Peasant")
	}
	result.Evidence = evidence
	result.V2Layout = OpenCodeSessionV2Absent
	if len(evidence.SessionV2Columns) > 0 {
		result.V2Layout = OpenCodeSessionV2Unsupported
		if openCodeSessionColumns(OpenCodeSessionTableV2, evidence.SessionV2Columns).hasID {
			result.V2Layout = OpenCodeSessionV2Supported
		}
	}
	result.Capability, result.Support = classifyOpenCodeEvidence(evidence)
	if err := result.Support.Validate(); err != nil {
		return failedOpenCodeProbe(result, OpenCodeSupportUnsupported, OpenCodeProbeCatalog, "schema support classification is invalid", err.Error(), candidate.Path, "after bounded catalog inspection", "the candidate cannot be trusted or ingested", "report the unsupported classification and update the typed implementation")
	}
	if result.Support != OpenCodeSupportSupported {
		diagnostic := actionableOpenCodeDiagnostic(OpenCodeProbeCatalog, "OpenCode SQLite schema is not fully supported", "required legacy message/part columns or current session_message ordering evidence are incomplete", candidate.Path, "after bounded catalog inspection", "SQLite presence remains evidence only and cannot enter ingestion", "upgrade OpenCode to a supported schema or retain the legacy JSON layout; do not modify the source through Peasant")
		diagnostic.Code = OpenCodeDiagnosticSchemaIncomplete
		result.Diagnostics = append(result.Diagnostics, diagnostic)
	}
	return result
}

func (p *OpenCodeCandidateProber) sniffSQLiteHeader(path string) error {
	file, err := p.fs.Open(path)
	if err != nil {
		return fmt.Errorf("open the candidate for a %d-byte header read: %w", len(openCodeSQLiteHeader), err)
	}
	header := make([]byte, len(openCodeSQLiteHeader))
	_, readErr := io.ReadFull(file, header)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return fmt.Errorf("read and close the bounded SQLite header: %w", errors.Join(readErr, closeErr))
	}
	if string(header) != openCodeSQLiteHeader {
		return fmt.Errorf("first %d bytes do not match the SQLite format header", len(openCodeSQLiteHeader))
	}
	return nil
}

func failedOpenCodeProbe(result OpenCodeProbeResult, support OpenCodeSchemaSupport, stage OpenCodeProbeStage, what, why, where, when, meaning, fix string) OpenCodeProbeResult {
	result.Support = support
	result.Diagnostics = append(result.Diagnostics, actionableOpenCodeDiagnostic(stage, what, why, where, when, meaning, fix))
	return result
}

func actionableOpenCodeDiagnostic(stage OpenCodeProbeStage, what, why, where, when, meaning, fix string) OpenCodeProbeDiagnostic {
	code := OpenCodeDiagnosticInvalidCandidate
	switch stage {
	case OpenCodeProbeStat:
		code = OpenCodeDiagnosticPathUnavailable
	case OpenCodeProbeHeader:
		code = OpenCodeDiagnosticInvalidHeader
	case OpenCodeProbeOpen:
		code = OpenCodeDiagnosticSourceOpenFailed
	case OpenCodeProbeCatalog:
		code = OpenCodeDiagnosticCatalogReadFailed
	case OpenCodeProbeDiscover, OpenCodeProbeFreshness:
		code = OpenCodeDiagnosticDiscoveryFailed
	}
	return OpenCodeProbeDiagnostic{Code: code, Stage: stage, What: what, Why: why, Where: where, When: when, Meaning: meaning, Remediation: fix}
}

func classifyOpenCodeEvidence(evidence OpenCodeSchemaEvidence) (OpenCodeSchemaCapability, OpenCodeSchemaSupport) {
	legacyMessage := hasOpenCodeColumns(evidence.LegacyMessageColumns, []string{"id", "session_id", "time_created", "time_updated", "data"}, "")
	legacyPart := hasOpenCodeColumns(evidence.LegacyPartColumns, []string{"id", "message_id", "session_id", "time_created", "time_updated", "data"}, "")
	legacy := legacyMessage && legacyPart
	currentColumns := hasOpenCodeColumns(evidence.CurrentMessageColumns, []string{"id", "session_id", "type", "time_created", "time_updated", "data", "seq"}, "seq")
	currentOrder := false
	for _, index := range evidence.CurrentIndexes {
		if isOpenCodeCurrentOrderingIndex(index) {
			currentOrder = true
			break
		}
	}
	current := currentColumns && currentOrder

	switch {
	case legacy && current:
		return OpenCodeCapabilityHybrid, OpenCodeSupportSupported
	case legacy:
		return OpenCodeCapabilityLegacy, OpenCodeSupportSupported
	case current:
		return OpenCodeCapabilityCurrent, OpenCodeSupportSupported
	case len(evidence.LegacyMessageColumns) != 0 || len(evidence.LegacyPartColumns) != 0 || len(evidence.CurrentMessageColumns) != 0:
		return OpenCodeCapabilityNone, OpenCodeSupportPartial
	default:
		return OpenCodeCapabilityNone, OpenCodeSupportUnsupported
	}
}

func isOpenCodeCurrentOrderingIndex(index OpenCodeIndexEvidence) bool {
	if !index.Unique || index.Partial || len(index.Keys) < 2 {
		return false
	}
	keyPosition := 0
	for position, key := range index.Keys {
		if key.Sequence != int64(position) {
			return false
		}
		if !key.Key {
			continue
		}
		if key.ColumnID < 0 || key.Collation != "BINARY" {
			return false
		}
		switch keyPosition {
		case 0:
			if key.Name != "session_id" {
				return false
			}
		case 1:
			if key.Name != "seq" {
				return false
			}
		default:
			return false
		}
		keyPosition++
	}
	return keyPosition == 2
}

func hasOpenCodeColumns(columns []OpenCodeColumnEvidence, required []string, requiredNotNull string) bool {
	byName := make(map[string]OpenCodeColumnEvidence, len(columns))
	for _, column := range columns {
		byName[column.Name] = column
	}
	for _, name := range required {
		column, ok := byName[name]
		if !ok {
			return false
		}
		if name == requiredNotNull && !column.NotNull {
			return false
		}
	}
	return true
}

var _ OpenCodeCandidateFileSystem = (*OSFileSystem)(nil)
var _ OpenCodeEnvironmentLookup = systemOpenCodeEnvironment{}
