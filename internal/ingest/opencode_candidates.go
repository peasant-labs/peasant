package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	openCodeDatabaseOverrideEnv = "OPENCODE_DB"
	openCodeDisableChannelEnv   = "OPENCODE_DISABLE_CHANNEL_DB"
	openCodeSQLiteHeader        = "SQLite format 3\x00"
	openCodeCatalogRowLimit     = 256
	openCodeColumnRowLimit      = 32
	openCodeIndexRowLimit       = 64
)

// OpenCodeSourceKind identifies the physical source without implying whether
// its schema is supported or whether it is eligible for ingestion.
type OpenCodeSourceKind string

const (
	OpenCodeSourceSQLite     OpenCodeSourceKind = "sqlite"
	OpenCodeSourceLegacyJSON OpenCodeSourceKind = "legacy_json"
)

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
)

// OpenCodeProbeDiagnosticCode is the machine-readable reason for a diagnostic.
type OpenCodeProbeDiagnosticCode string

const (
	OpenCodeDiagnosticInvalidCandidate  OpenCodeProbeDiagnosticCode = "invalid_candidate"
	OpenCodeDiagnosticPathUnavailable   OpenCodeProbeDiagnosticCode = "path_unavailable"
	OpenCodeDiagnosticInvalidHeader     OpenCodeProbeDiagnosticCode = "invalid_sqlite_header"
	OpenCodeDiagnosticSourceOpenFailed  OpenCodeProbeDiagnosticCode = "source_open_failed"
	OpenCodeDiagnosticCatalogReadFailed OpenCodeProbeDiagnosticCode = "catalog_read_failed"
	OpenCodeDiagnosticSchemaIncomplete  OpenCodeProbeDiagnosticCode = "schema_incomplete"
)

// OpenCodeCandidate is one deduplicated source location. SQLite candidates are
// evidence only and are never converted to DiscoveredSession values.
type OpenCodeCandidate struct {
	Path       string
	Kind       OpenCodeSourceKind
	Provenance OpenCodeCandidateProvenance
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

// OpenCodeIndexEvidence is bounded ordering evidence from one SQLite index.
type OpenCodeIndexEvidence struct {
	Name    string
	Unique  bool
	Columns []string
}

// OpenCodeSchemaEvidence contains catalog shape only. It never contains
// transcript payloads, session rows, migration history, or event data.
type OpenCodeSchemaEvidence struct {
	Tables                []string
	LegacyMessageColumns  []OpenCodeColumnEvidence
	LegacyPartColumns     []OpenCodeColumnEvidence
	CurrentMessageColumns []OpenCodeColumnEvidence
	CurrentIndexes        []OpenCodeIndexEvidence
}

// OpenCodeProbeResult is the complete, non-ingestible observation for one
// candidate. Candidate failures do not prevent later candidates from probing.
type OpenCodeProbeResult struct {
	Candidate   OpenCodeCandidate
	Capability  OpenCodeSchemaCapability
	Support     OpenCodeSchemaSupport
	Evidence    OpenCodeSchemaEvidence
	Diagnostics []OpenCodeProbeDiagnostic
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
		candidates = append(candidates, OpenCodeCandidate{Path: resolved, Kind: OpenCodeSourceSQLite, Provenance: OpenCodeCandidateOverride})
	}

	disableValue, _ := environment.LookupEnv(openCodeDisableChannelEnv)
	databaseName := "opencode.db"
	if channel != "latest" && channel != "beta" && channel != "prod" && disableValue != "1" && disableValue != "true" {
		databaseName = "opencode-" + sanitizeOpenCodeChannel(channel) + ".db"
	}
	candidates = append(candidates,
		OpenCodeCandidate{Path: filepath.Join(root, databaseName), Kind: OpenCodeSourceSQLite, Provenance: OpenCodeCandidateChannel},
		OpenCodeCandidate{Path: root, Kind: OpenCodeSourceLegacyJSON, Provenance: OpenCodeCandidateLegacyJSONRoot},
	)

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
	if candidate.Kind != OpenCodeSourceSQLite && candidate.Kind != OpenCodeSourceLegacyJSON {
		return failedOpenCodeProbe(result, OpenCodeSupportUnsupported, OpenCodeProbeValidate, "candidate source kind is unsupported", fmt.Sprintf("kind %q is not a recognized OpenCode source kind", candidate.Kind), "candidate resolution", "before filesystem inspection", "the candidate cannot provide source evidence", "construct candidates through ResolveOpenCodeCandidates")
	}
	if candidate.Path == "" || candidate.Path == ":memory:" || !filepath.IsAbs(candidate.Path) {
		return failedOpenCodeProbe(result, OpenCodeSupportUnsupported, OpenCodeProbeValidate, "candidate path is not production filesystem evidence", fmt.Sprintf("path %q is empty, relative, or in-memory", candidate.Path), candidate.Path, "before filesystem inspection", "the candidate cannot identify a durable OpenCode-owned file or directory", "use an absolute filesystem path; reserve :memory: only for upstream tests")
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
	evidence, inspectErr := inspectOpenCodeCatalog(ctx, source)
	closeErr := source.Close()
	if inspectErr != nil || closeErr != nil {
		joined := errors.Join(inspectErr, closeErr)
		result.Evidence = evidence
		return failedOpenCodeProbe(result, OpenCodeSupportUnreadable, OpenCodeProbeCatalog, "bounded SQLite catalog inspection failed", joined.Error(), candidate.Path, "while reading explicit schema catalog columns", "schema capability is incomplete and the candidate is not ingestible", "verify source readability and retry; do not migrate, checkpoint, or repair it through Peasant")
	}
	result.Evidence = evidence
	result.Capability, result.Support = classifyOpenCodeEvidence(evidence)
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
	}
	return OpenCodeProbeDiagnostic{Code: code, Stage: stage, What: what, Why: why, Where: where, When: when, Meaning: meaning, Remediation: fix}
}

func inspectOpenCodeCatalog(ctx context.Context, source OpenCodeSQLiteSource) (OpenCodeSchemaEvidence, error) {
	var evidence OpenCodeSchemaEvidence
	tables := make(map[string]bool)
	query := fmt.Sprintf("SELECT name, type FROM sqlite_schema WHERE type IN ('table','index') ORDER BY type, name LIMIT %d", openCodeCatalogRowLimit)
	rows := 0
	if err := source.Read(ctx, query, nil, func(row OpenCodeSQLiteRow) error {
		rows++
		name, err := openCodeTextColumn(row, "name")
		if err != nil {
			return err
		}
		objectType, err := openCodeTextColumn(row, "type")
		if err != nil {
			return err
		}
		if objectType == "table" {
			tables[name] = true
			evidence.Tables = append(evidence.Tables, name)
		}
		return nil
	}); err != nil {
		return evidence, fmt.Errorf("inspect explicit sqlite_schema name/type columns with a %d-row bound: %w", openCodeCatalogRowLimit, err)
	}
	if rows > openCodeCatalogRowLimit {
		return evidence, fmt.Errorf("inspect sqlite_schema exceeded the declared %d-row bound", openCodeCatalogRowLimit)
	}
	sort.Strings(evidence.Tables)

	var err error
	if tables["message"] {
		evidence.LegacyMessageColumns, err = inspectOpenCodeColumns(ctx, source, "message")
		if err != nil {
			return evidence, err
		}
	}
	if tables["part"] {
		evidence.LegacyPartColumns, err = inspectOpenCodeColumns(ctx, source, "part")
		if err != nil {
			return evidence, err
		}
	}
	if tables["session_message"] {
		evidence.CurrentMessageColumns, err = inspectOpenCodeColumns(ctx, source, "session_message")
		if err != nil {
			return evidence, err
		}
		evidence.CurrentIndexes, err = inspectOpenCodeIndexes(ctx, source, "session_message")
		if err != nil {
			return evidence, err
		}
	}
	return evidence, nil
}

func inspectOpenCodeColumns(ctx context.Context, source OpenCodeSQLiteSource, table string) ([]OpenCodeColumnEvidence, error) {
	query := fmt.Sprintf("SELECT name, \"notnull\", pk FROM pragma_table_info(?) ORDER BY cid LIMIT %d", openCodeColumnRowLimit)
	columns := make([]OpenCodeColumnEvidence, 0)
	if err := source.Read(ctx, query, []any{table}, func(row OpenCodeSQLiteRow) error {
		name, err := openCodeTextColumn(row, "name")
		if err != nil {
			return err
		}
		notNull, err := openCodeIntegerColumn(row, "notnull")
		if err != nil {
			return err
		}
		primary, err := openCodeIntegerColumn(row, "pk")
		if err != nil {
			return err
		}
		columns = append(columns, OpenCodeColumnEvidence{Name: name, NotNull: notNull != 0, Primary: primary != 0})
		return nil
	}); err != nil {
		return columns, fmt.Errorf("inspect explicit pragma_table_info columns for %q with a %d-row bound: %w", table, openCodeColumnRowLimit, err)
	}
	if len(columns) > openCodeColumnRowLimit {
		return columns, fmt.Errorf("inspect columns for %q exceeded the declared %d-row bound", table, openCodeColumnRowLimit)
	}
	return columns, nil
}

func inspectOpenCodeIndexes(ctx context.Context, source OpenCodeSQLiteSource, table string) ([]OpenCodeIndexEvidence, error) {
	query := fmt.Sprintf("SELECT il.name AS index_name, il.\"unique\" AS is_unique, ii.seqno AS seqno, ii.name AS column_name FROM pragma_index_list(?) AS il JOIN pragma_index_info(il.name) AS ii ORDER BY il.name, ii.seqno LIMIT %d", openCodeIndexRowLimit)
	indexes := make([]OpenCodeIndexEvidence, 0)
	byName := make(map[string]int)
	if err := source.Read(ctx, query, []any{table}, func(row OpenCodeSQLiteRow) error {
		name, err := openCodeTextColumn(row, "index_name")
		if err != nil {
			return err
		}
		unique, err := openCodeIntegerColumn(row, "is_unique")
		if err != nil {
			return err
		}
		column, err := openCodeTextColumn(row, "column_name")
		if err != nil {
			return err
		}
		index, ok := byName[name]
		if !ok {
			index = len(indexes)
			byName[name] = index
			indexes = append(indexes, OpenCodeIndexEvidence{Name: name, Unique: unique != 0})
		}
		indexes[index].Columns = append(indexes[index].Columns, column)
		return nil
	}); err != nil {
		return indexes, fmt.Errorf("inspect explicit index-list/index-info columns for %q with a %d-row bound: %w", table, openCodeIndexRowLimit, err)
	}
	rows := 0
	for _, index := range indexes {
		rows += len(index.Columns)
	}
	if rows > openCodeIndexRowLimit {
		return indexes, fmt.Errorf("inspect indexes for %q exceeded the declared %d-row bound", table, openCodeIndexRowLimit)
	}
	return indexes, nil
}

func openCodeTextColumn(row OpenCodeSQLiteRow, name string) (string, error) {
	for _, column := range row.Columns() {
		if column.Name == name && column.Value.Kind == OpenCodeSQLiteValueText {
			return column.Value.Text, nil
		}
	}
	return "", fmt.Errorf("catalog result omitted required text column %q", name)
}

func openCodeIntegerColumn(row OpenCodeSQLiteRow, name string) (int64, error) {
	for _, column := range row.Columns() {
		if column.Name == name && column.Value.Kind == OpenCodeSQLiteValueInteger {
			return column.Value.Integer, nil
		}
	}
	return 0, fmt.Errorf("catalog result omitted required integer column %q", name)
}

func classifyOpenCodeEvidence(evidence OpenCodeSchemaEvidence) (OpenCodeSchemaCapability, OpenCodeSchemaSupport) {
	legacyMessage := hasOpenCodeColumns(evidence.LegacyMessageColumns, []string{"id", "session_id", "time_created", "time_updated", "data"}, "")
	legacyPart := hasOpenCodeColumns(evidence.LegacyPartColumns, []string{"id", "message_id", "session_id", "time_created", "time_updated", "data"}, "")
	legacy := legacyMessage && legacyPart
	currentColumns := hasOpenCodeColumns(evidence.CurrentMessageColumns, []string{"id", "session_id", "type", "time_created", "time_updated", "data", "seq"}, "seq")
	currentOrder := false
	for _, index := range evidence.CurrentIndexes {
		if index.Unique && len(index.Columns) == 2 && index.Columns[0] == "session_id" && index.Columns[1] == "seq" {
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
