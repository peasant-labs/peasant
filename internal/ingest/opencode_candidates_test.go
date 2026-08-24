package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/build/constraint"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
)

const (
	expectedOpenCodeResolutionCases = 8
	expectedOpenCodeProbeCases      = 13
	expectedContinuationCandidates  = 4
	expectedClosedSetCases          = 7
	expectedAllowedQueryStatements  = 24
	expectedReadableSessionColumns  = 6
	expectedSessionColumnMutations  = 1
	expectedQueryGuardMutations     = 12
	expectedEntryPathMutations      = 41
	expectedEntryPathKindCount      = 41
	expectedIndexEvidenceCases      = 6
	expectedFileInventoryMutations  = 46
	expectedFileMatrixKindCount     = 23
	expectedCoverageGuardMutations  = 9
	expectedBuildTaggedMutations    = 7
	expectedBuildTopologyCases      = 3
	expectedAdapterDiscoveryCases   = 6
)

//go:embed testdata/opencode_candidates.yaml
var openCodeCandidateFixtureYAML []byte

type openCodeCandidateFixture struct {
	DeclaredResolutionCases  int                               `yaml:"declared_resolution_cases"`
	ResolutionCases          []openCodeCandidateResolutionCase `yaml:"resolution_cases"`
	DeclaredProbeCases       int                               `yaml:"declared_probe_cases"`
	ForbiddenQueryTokens     []string                          `yaml:"forbidden_query_tokens"`
	ReadableSessionColumns   []string                          `yaml:"readable_session_columns"`
	DeclaredSessionColumns   int                               `yaml:"declared_session_column_mutations"`
	SessionColumnMutations   []openCodeSessionColumnMutation   `yaml:"session_column_mutations"`
	DeclaredAllowedQueries   int                               `yaml:"declared_allowed_query_statements"`
	AllowedQueryStatements   []openCodeAllowedQueryStatement   `yaml:"allowed_query_statements"`
	DeclaredQueryMutations   int                               `yaml:"declared_query_guard_mutations"`
	QueryGuardMutations      []openCodeQueryGuardMutation      `yaml:"query_guard_mutations"`
	DeclaredEntryMutations   int                               `yaml:"declared_entry_path_mutations"`
	EntryPathMutations       []openCodeEntryPathMutation       `yaml:"entry_path_mutations"`
	DeclaredIndexCases       int                               `yaml:"declared_index_evidence_cases"`
	IndexEvidenceCases       []openCodeIndexEvidenceCase       `yaml:"index_evidence_cases"`
	DeclaredFileMutations    int                               `yaml:"declared_file_inventory_mutations"`
	FileInventoryMutations   []openCodeFileInventoryMutation   `yaml:"file_inventory_mutations"`
	DeclaredCoverageGuards   int                               `yaml:"declared_coverage_guard_mutations"`
	CoverageGuardMutations   []openCodeCoverageGuardMutation   `yaml:"coverage_guard_mutations"`
	DeclaredBuildTagged      int                               `yaml:"declared_build_tagged_mutations"`
	BuildTaggedMutations     []openCodeBuildTaggedMutation     `yaml:"build_tagged_mutations"`
	DeclaredBuildTopology    int                               `yaml:"declared_build_topology_cases"`
	BuildTopologyCases       []openCodeBuildTopologyCase       `yaml:"build_topology_cases"`
	ProbeCases               []openCodeProbeCase               `yaml:"probe_cases"`
	DeclaredContinuation     int                               `yaml:"declared_continuation_candidates"`
	ContinuationCandidates   []openCodeContinuationCandidate   `yaml:"continuation_candidates"`
	DeclaredClosedSetCases   int                               `yaml:"declared_closed_set_cases"`
	ClosedSetCases           []openCodeClosedSetCase           `yaml:"closed_set_cases"`
	DeclaredAdapterDiscovery int                               `yaml:"declared_adapter_discovery_cases"`
	AdapterDiscoveryCases    []openCodeAdapterDiscoveryCase    `yaml:"adapter_discovery_cases"`
}

type openCodeAllowedQueryStatement struct {
	Name      string `yaml:"name"`
	Statement string `yaml:"statement"`
}

type openCodeQueryGuardMutation struct {
	Name                 string `yaml:"name"`
	ReplaceStatement     string `yaml:"replace_statement"`
	ReplacementStatement string `yaml:"replacement_statement"`
}

// openCodeSessionColumnMutation is one statement that reads a forbidden session
// column, so the allowlist proves it can go red: the named column is outside
// readable_session_columns and the statement must be rejected.
type openCodeSessionColumnMutation struct {
	Name            string `yaml:"name"`
	Statement       string `yaml:"statement"`
	ForbiddenColumn string `yaml:"forbidden_column"`
}

type openCodeEntryPathKind string

const (
	openCodeEntrySQLitexExecute           openCodeEntryPathKind = "sqlitex_execute"
	openCodeEntryPrepare                  openCodeEntryPathKind = "prepare"
	openCodeEntryPrepareTransient         openCodeEntryPathKind = "prepare_transient"
	openCodeEntryExecuteScript            openCodeEntryPathKind = "execute_script"
	openCodeEntryStatementStep            openCodeEntryPathKind = "statement_step"
	openCodeEntrySQLitexExec              openCodeEntryPathKind = "sqlitex_exec"
	openCodeEntryConnPrep                 openCodeEntryPathKind = "conn_prep"
	openCodeEntryExecuteFS                openCodeEntryPathKind = "execute_fs"
	openCodeEntryImportAlias              openCodeEntryPathKind = "import_alias"
	openCodeEntryCallableAlias            openCodeEntryPathKind = "callable_alias"
	openCodeEntryReceiverAlias            openCodeEntryPathKind = "receiver_alias"
	openCodeEntryCapturedStep             openCodeEntryPathKind = "captured_step"
	openCodeEntryHelperExecute            openCodeEntryPathKind = "helper_execute"
	openCodeEntryHelperStep               openCodeEntryPathKind = "helper_step"
	openCodeEntryReturnedExecute          openCodeEntryPathKind = "returned_execute"
	openCodeEntryStructField              openCodeEntryPathKind = "struct_field_execute"
	openCodeEntryInterfaceField           openCodeEntryPathKind = "interface_field_execute"
	openCodeEntryInterfacePrepare         openCodeEntryPathKind = "interface_prepare"
	openCodeEntryInterfaceStep            openCodeEntryPathKind = "interface_step"
	openCodeEntryExecutorHistory          openCodeEntryPathKind = "executor_history_execute"
	openCodeEntryExecutorPrepare          openCodeEntryPathKind = "executor_extra_prepare"
	openCodeEntryPrepareArgument          openCodeEntryPathKind = "executor_prepare_argument"
	openCodeEntryExecuteArgument          openCodeEntryPathKind = "executor_execute_argument"
	openCodeEntryExecutorEscape           openCodeEntryPathKind = "executor_callable_escape"
	openCodeEntryInitializerExtra         openCodeEntryPathKind = "initializer_extra_execute"
	openCodeEntryPackageValue             openCodeEntryPathKind = "package_callable_value"
	openCodeEntryExecutorInterface        openCodeEntryPathKind = "executor_interface"
	openCodeEntryCurrentOpenBlob          openCodeEntryPathKind = "current_messages_open_blob"
	openCodeEntryOpenBlobAlias            openCodeEntryPathKind = "open_blob_alias"
	openCodeEntrySerialize                openCodeEntryPathKind = "serialize"
	openCodeEntrySerializeInterface       openCodeEntryPathKind = "serialize_interface"
	openCodeEntryDeserialize              openCodeEntryPathKind = "deserialize"
	openCodeEntryNewBackup                openCodeEntryPathKind = "new_backup"
	openCodeEntryBackupStep               openCodeEntryPathKind = "backup_step"
	openCodeEntryBackupStepAlias          openCodeEntryPathKind = "backup_step_alias"
	openCodeEntryBlobRead                 openCodeEntryPathKind = "blob_read"
	openCodeEntryBlobReadAlias            openCodeEntryPathKind = "blob_read_alias"
	openCodeEntryBlobWriteTo              openCodeEntryPathKind = "blob_write_to"
	openCodeEntrySessionDiff              openCodeEntryPathKind = "session_diff"
	openCodeEntrySessionChangeset         openCodeEntryPathKind = "session_changeset"
	openCodeEntryExecutorPrepareInterface openCodeEntryPathKind = "executor_prepare_interface"
)

type openCodeEntryPathMutation struct {
	Name          string                `yaml:"name"`
	Kind          openCodeEntryPathKind `yaml:"kind"`
	ErrorContains string                `yaml:"error_contains"`
}

type openCodeIndexEvidenceCase struct {
	Name               string                          `yaml:"name"`
	Unique             bool                            `yaml:"unique"`
	Partial            bool                            `yaml:"partial"`
	Keys               []openCodeIndexKeyCase          `yaml:"keys"`
	ExpectedCapability ingest.OpenCodeSchemaCapability `yaml:"expected_capability"`
	ExpectedSupport    ingest.OpenCodeSchemaSupport    `yaml:"expected_support"`
}

type openCodeIndexKeyCase struct {
	Sequence   int64  `yaml:"sequence"`
	ColumnID   int64  `yaml:"column_id"`
	Name       string `yaml:"name"`
	Descending bool   `yaml:"descending"`
	Collation  string `yaml:"collation"`
	Key        bool   `yaml:"key"`
}

type openCodeFileInventoryMutation struct {
	Name          string                `yaml:"name"`
	Filename      string                `yaml:"filename"`
	Kind          openCodeEntryPathKind `yaml:"kind"`
	ErrorContains string                `yaml:"error_contains"`
}

type openCodeCoverageGuardKind string

const (
	openCodeCoverageEntryKindReplacement           openCodeCoverageGuardKind = "entry_kind_replacement"
	openCodeCoverageFileKindSubstitution           openCodeCoverageGuardKind = "file_kind_substitution"
	openCodeCoverageFileClassOmission              openCodeCoverageGuardKind = "file_class_omission"
	openCodeCoverageBuildKindSubstitution          openCodeCoverageGuardKind = "build_tagged_kind_substitution"
	openCodeCoverageBuildClassSubstitution         openCodeCoverageGuardKind = "build_tagged_class_substitution"
	openCodeCoverageBuildNameSubstitution          openCodeCoverageGuardKind = "build_tagged_name_substitution"
	openCodeCoverageTopologyTagSubstitution        openCodeCoverageGuardKind = "topology_tag_substitution"
	openCodeCoverageTopologyExpressionSubstitution openCodeCoverageGuardKind = "topology_expression_substitution"
	openCodeCoverageTopologyOutcomeSubstitution    openCodeCoverageGuardKind = "topology_outcome_substitution"
)

type openCodeCoverageGuardMutation struct {
	Name          string                    `yaml:"name"`
	Kind          openCodeCoverageGuardKind `yaml:"kind"`
	ErrorContains string                    `yaml:"error_contains"`
}

type openCodeBuildTaggedMutationKind string

const (
	openCodeBuildTaggedDirect     openCodeBuildTaggedMutationKind = "direct_execute"
	openCodeBuildTaggedAlias      openCodeBuildTaggedMutationKind = "callable_alias"
	openCodeBuildTaggedHelper     openCodeBuildTaggedMutationKind = "helper_parameter"
	openCodeBuildTaggedInterface  openCodeBuildTaggedMutationKind = "interface_dispatch"
	openCodeBuildTaggedField      openCodeBuildTaggedMutationKind = "struct_field"
	openCodeBuildTaggedReturn     openCodeBuildTaggedMutationKind = "returned_callable"
	openCodeBuildTaggedDirectData openCodeBuildTaggedMutationKind = "blob_callable_aliases"
)

type openCodeBuildTaggedMutationClass string

const (
	openCodeBuildTaggedClassDirect     openCodeBuildTaggedMutationClass = "direct"
	openCodeBuildTaggedClassAlias      openCodeBuildTaggedMutationClass = "alias"
	openCodeBuildTaggedClassHelper     openCodeBuildTaggedMutationClass = "helper"
	openCodeBuildTaggedClassInterface  openCodeBuildTaggedMutationClass = "interface"
	openCodeBuildTaggedClassField      openCodeBuildTaggedMutationClass = "field"
	openCodeBuildTaggedClassReturn     openCodeBuildTaggedMutationClass = "return"
	openCodeBuildTaggedClassDirectData openCodeBuildTaggedMutationClass = "direct_data"
)

type openCodeBuildTaggedMutation struct {
	Name          string                           `yaml:"name"`
	Filename      string                           `yaml:"filename"`
	BuildTags     []string                         `yaml:"build_tags"`
	Kind          openCodeBuildTaggedMutationKind  `yaml:"kind"`
	Class         openCodeBuildTaggedMutationClass `yaml:"class"`
	ErrorContains string                           `yaml:"error_contains"`
}

type openCodeBuildTopologyOutcome string

const (
	openCodeBuildTopologyDiscovery   openCodeBuildTopologyOutcome = "discovery"
	openCodeBuildTopologyForbidden   openCodeBuildTopologyOutcome = "forbidden_callable"
	openCodeBuildTopologyUnreachable openCodeBuildTopologyOutcome = "unreachable_file"
)

type openCodeBuildTopologySourceKind string

const (
	openCodeBuildTopologyAnchor    openCodeBuildTopologySourceKind = "anchor"
	openCodeBuildTopologyExecution openCodeBuildTopologySourceKind = "forbidden_execute"
)

type openCodeBuildTopologySource struct {
	Name                string                          `yaml:"name"`
	Filename            string                          `yaml:"filename"`
	BuildExpression     string                          `yaml:"build_expression"`
	Kind                openCodeBuildTopologySourceKind `yaml:"kind"`
	ExpectedActivations int                             `yaml:"expected_activations"`
}

type openCodeBuildTopologyCase struct {
	Name                            string                        `yaml:"name"`
	Sources                         []openCodeBuildTopologySource `yaml:"sources"`
	ExpectedCustomTags              []string                      `yaml:"expected_custom_tags"`
	ExpectedConfigurations          int                           `yaml:"expected_configurations"`
	ExpectedForbiddenConfigurations int                           `yaml:"expected_forbidden_configurations"`
	ExpectedOutcome                 openCodeBuildTopologyOutcome  `yaml:"expected_outcome"`
	ErrorContains                   string                        `yaml:"error_contains"`
}

type openCodeCandidateResolutionCase struct {
	Name        string                               `yaml:"name"`
	Channel     string                               `yaml:"channel"`
	Environment map[string]string                    `yaml:"environment"`
	Paths       []string                             `yaml:"paths"`
	Provenance  []ingest.OpenCodeCandidateProvenance `yaml:"provenance"`
}

type openCodeProbeCase struct {
	Fixture       string                             `yaml:"fixture"`
	Capability    ingest.OpenCodeSchemaCapability    `yaml:"capability"`
	Support       ingest.OpenCodeSchemaSupport       `yaml:"support"`
	Diagnostic    ingest.OpenCodeProbeDiagnosticCode `yaml:"diagnostic"`
	OverflowScope ingest.OpenCodeCatalogScope        `yaml:"overflow_scope"`
	RetainedRows  int                                `yaml:"retained_rows"`
}

type openCodePathTemplate string

const (
	openCodePathMemory     openCodePathTemplate = "memory"
	openCodePathMissing    openCodePathTemplate = "missing"
	openCodePathFixture    openCodePathTemplate = "fixture"
	openCodePathLegacyRoot openCodePathTemplate = "legacy_root"
)

type openCodeContinuationCandidate struct {
	Name               string                             `yaml:"name"`
	PathTemplate       openCodePathTemplate               `yaml:"path_template"`
	Fixture            string                             `yaml:"fixture"`
	Kind               ingest.OpenCodeSourceKind          `yaml:"kind"`
	Provenance         ingest.OpenCodeCandidateProvenance `yaml:"provenance"`
	ExpectedSupport    ingest.OpenCodeSchemaSupport       `yaml:"expected_support"`
	ExpectedCapability ingest.OpenCodeSchemaCapability    `yaml:"expected_capability"`
}

type openCodeAdapterDiscoveryMode string

const (
	openCodeAdapterCapableProbeInitFailure openCodeAdapterDiscoveryMode = "capable_probe_init_failure"
	openCodeAdapterIncapableLegacyOnly     openCodeAdapterDiscoveryMode = "incapable_legacy_only"
	openCodeAdapterHybridCurrent           openCodeAdapterDiscoveryMode = "hybrid_current_success"
	openCodeAdapterHybridLegacyFallback    openCodeAdapterDiscoveryMode = "hybrid_legacy_fallback"
	openCodeAdapterHybridFailure           openCodeAdapterDiscoveryMode = "hybrid_fallback_error"
	openCodeAdapterSQLiteFailureKeepsJSON  openCodeAdapterDiscoveryMode = "sqlite_failure_keeps_json"
)

type openCodeAdapterDiscoveryCase struct {
	Name             string                        `yaml:"name"`
	SourceFixture    string                        `yaml:"source_fixture"`
	Mode             openCodeAdapterDiscoveryMode  `yaml:"mode"`
	ExpectedOrigin   openCodeAdapterExpectedOrigin `yaml:"expected_origin"`
	ExpectedSessions int                           `yaml:"expected_sessions"`
	ErrorContains    string                        `yaml:"error_contains"`
}

type openCodeAdapterExpectedOrigin string

const (
	openCodeAdapterNoOrigin      openCodeAdapterExpectedOrigin = "none"
	openCodeAdapterLegacyOrigin  openCodeAdapterExpectedOrigin = "legacy_sqlite"
	openCodeAdapterCurrentOrigin openCodeAdapterExpectedOrigin = "current_sqlite"
)

func (origin openCodeAdapterExpectedOrigin) transcriptOrigin() (ingest.TranscriptOrigin, error) {
	switch origin {
	case openCodeAdapterNoOrigin:
		return ingest.TranscriptOriginFile, nil
	case openCodeAdapterLegacyOrigin:
		return ingest.TranscriptOriginOpenCodeLegacySQLite, nil
	case openCodeAdapterCurrentOrigin:
		return ingest.TranscriptOriginOpenCodeCurrentSQLite, nil
	default:
		return ingest.TranscriptOriginFile, fmt.Errorf("unknown expected origin %q", origin)
	}
}

type openCodeClosedSetField string

const (
	openCodeClosedSetKind       openCodeClosedSetField = "kind"
	openCodeClosedSetPath       openCodeClosedSetField = "path"
	openCodeClosedSetProvenance openCodeClosedSetField = "provenance"
	openCodeClosedSetSupport    openCodeClosedSetField = "support"
)

type openCodeClosedSetCase struct {
	Name  string                 `yaml:"name"`
	Field openCodeClosedSetField `yaml:"field"`
	Value string                 `yaml:"value"`
}

type syntheticOpenCodeEnvironment map[string]string

var _ ingest.OpenCodeEnvironmentLookup = syntheticOpenCodeEnvironment{}

func (environment syntheticOpenCodeEnvironment) LookupEnv(key string) (string, bool) {
	value, ok := environment[key]
	return value, ok
}

func loadOpenCodeCandidateFixture(t testing.TB) openCodeCandidateFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(openCodeCandidateFixtureYAML))
	decoder.KnownFields(true)
	var fixture openCodeCandidateFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode OpenCode candidate fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("decode OpenCode candidate fixture: expected exactly one YAML document: %v", err)
	}
	if fixture.DeclaredResolutionCases != expectedOpenCodeResolutionCases || len(fixture.ResolutionCases) != expectedOpenCodeResolutionCases {
		t.Fatalf("OpenCode resolution fixture row guard: declared=%d actual=%d required=%d", fixture.DeclaredResolutionCases, len(fixture.ResolutionCases), expectedOpenCodeResolutionCases)
	}
	if fixture.DeclaredProbeCases != expectedOpenCodeProbeCases || len(fixture.ProbeCases) != expectedOpenCodeProbeCases {
		t.Fatalf("OpenCode probe fixture row guard: declared=%d actual=%d required=%d", fixture.DeclaredProbeCases, len(fixture.ProbeCases), expectedOpenCodeProbeCases)
	}
	if fixture.DeclaredAdapterDiscovery != expectedAdapterDiscoveryCases || len(fixture.AdapterDiscoveryCases) != expectedAdapterDiscoveryCases {
		t.Fatalf("OpenCode adapter discovery fixture row guard: declared=%d actual=%d required=%d", fixture.DeclaredAdapterDiscovery, len(fixture.AdapterDiscoveryCases), expectedAdapterDiscoveryCases)
	}
	adapterCaseNames := make(map[string]struct{}, len(fixture.AdapterDiscoveryCases))
	for _, testCase := range fixture.AdapterDiscoveryCases {
		if strings.TrimSpace(testCase.Name) == "" || strings.TrimSpace(testCase.SourceFixture) == "" {
			t.Fatalf("OpenCode adapter discovery fixture has an empty name or source fixture: %+v", testCase)
		}
		if _, duplicate := adapterCaseNames[testCase.Name]; duplicate {
			t.Fatalf("OpenCode adapter discovery fixture has duplicate name %q", testCase.Name)
		}
		adapterCaseNames[testCase.Name] = struct{}{}
		switch testCase.Mode {
		case openCodeAdapterCapableProbeInitFailure, openCodeAdapterIncapableLegacyOnly, openCodeAdapterHybridCurrent, openCodeAdapterHybridLegacyFallback, openCodeAdapterHybridFailure, openCodeAdapterSQLiteFailureKeepsJSON:
		default:
			t.Fatalf("OpenCode adapter discovery fixture %q has unknown mode %q", testCase.Name, testCase.Mode)
		}
		if testCase.Mode == openCodeAdapterHybridCurrent || testCase.Mode == openCodeAdapterHybridLegacyFallback {
			origin, err := testCase.ExpectedOrigin.transcriptOrigin()
			if err != nil || origin == ingest.TranscriptOriginFile {
				t.Fatalf("OpenCode adapter discovery fixture %q has invalid expected origin %q: %v", testCase.Name, testCase.ExpectedOrigin, err)
			}
		}
		if testCase.ErrorContains == "" && (testCase.Mode == openCodeAdapterCapableProbeInitFailure || testCase.Mode == openCodeAdapterHybridFailure || testCase.Mode == openCodeAdapterSQLiteFailureKeepsJSON) {
			t.Fatalf("OpenCode adapter discovery fixture %q must pin its actionable error", testCase.Name)
		}
		if testCase.ExpectedSessions < 0 {
			t.Fatalf("OpenCode adapter discovery fixture %q has a negative expected session count", testCase.Name)
		}
	}
	if fixture.DeclaredContinuation != expectedContinuationCandidates || len(fixture.ContinuationCandidates) != expectedContinuationCandidates {
		t.Fatalf("OpenCode continuation fixture row guard: declared=%d actual=%d required=%d", fixture.DeclaredContinuation, len(fixture.ContinuationCandidates), expectedContinuationCandidates)
	}
	if fixture.DeclaredClosedSetCases != expectedClosedSetCases || len(fixture.ClosedSetCases) != expectedClosedSetCases {
		t.Fatalf("OpenCode closed-set fixture row guard: declared=%d actual=%d required=%d", fixture.DeclaredClosedSetCases, len(fixture.ClosedSetCases), expectedClosedSetCases)
	}
	if len(fixture.ForbiddenQueryTokens) == 0 {
		t.Fatal("OpenCode probe fixture must declare forbidden query-shape tokens")
	}
	seenTokens := make(map[string]bool, len(fixture.ForbiddenQueryTokens))
	for index, token := range fixture.ForbiddenQueryTokens {
		token = strings.ToLower(token)
		fixture.ForbiddenQueryTokens[index] = token
		if strings.TrimSpace(token) == "" || seenTokens[token] {
			t.Fatalf("OpenCode probe fixture has an empty or duplicate forbidden query token %q", token)
		}
		seenTokens[token] = true
	}
	if len(fixture.ReadableSessionColumns) != expectedReadableSessionColumns {
		t.Fatalf("OpenCode readable session-column allowlist row guard: actual=%d required=%d", len(fixture.ReadableSessionColumns), expectedReadableSessionColumns)
	}
	allowlist := make(map[string]bool, len(fixture.ReadableSessionColumns))
	for index, column := range fixture.ReadableSessionColumns {
		column = normalizeOpenCodeQuery(column)
		fixture.ReadableSessionColumns[index] = column
		if column == "" || allowlist[column] {
			t.Fatalf("OpenCode readable session-column allowlist has an empty or duplicate column %q", column)
		}
		allowlist[column] = true
	}
	if fixture.DeclaredSessionColumns != expectedSessionColumnMutations || len(fixture.SessionColumnMutations) != expectedSessionColumnMutations {
		t.Fatalf("OpenCode session-column mutation fixture row guard: declared=%d actual=%d required=%d", fixture.DeclaredSessionColumns, len(fixture.SessionColumnMutations), expectedSessionColumnMutations)
	}
	seenSessionColumnMutations := make(map[string]bool, len(fixture.SessionColumnMutations))
	for index, mutation := range fixture.SessionColumnMutations {
		mutation.Statement = normalizeOpenCodeQuery(mutation.Statement)
		mutation.ForbiddenColumn = normalizeOpenCodeQuery(mutation.ForbiddenColumn)
		fixture.SessionColumnMutations[index] = mutation
		if strings.TrimSpace(mutation.Name) == "" || seenSessionColumnMutations[mutation.Name] || mutation.Statement == "" || mutation.ForbiddenColumn == "" {
			t.Fatalf("OpenCode session-column mutation is incomplete or duplicated: %+v", mutation)
		}
		if allowlist[mutation.ForbiddenColumn] {
			t.Fatalf("OpenCode session-column mutation %q names forbidden column %q that is in the readable allowlist, so the mutation cannot go red", mutation.Name, mutation.ForbiddenColumn)
		}
		columns, targetsSession, recognized := openCodeSessionSelectColumns(mutation.Statement)
		if !recognized || !targetsSession {
			t.Fatalf("OpenCode session-column mutation %q must read from the session table so the allowlist governs it: %+v", mutation.Name, mutation)
		}
		if !slices.Contains(columns, mutation.ForbiddenColumn) {
			t.Fatalf("OpenCode session-column mutation %q must project its forbidden column %q so the allowlist can reject it: columns=%v", mutation.Name, mutation.ForbiddenColumn, columns)
		}
		seenSessionColumnMutations[mutation.Name] = true
	}
	if fixture.DeclaredAllowedQueries != expectedAllowedQueryStatements || len(fixture.AllowedQueryStatements) != expectedAllowedQueryStatements {
		t.Fatalf("OpenCode allowed-query fixture row guard: declared=%d actual=%d required=%d", fixture.DeclaredAllowedQueries, len(fixture.AllowedQueryStatements), expectedAllowedQueryStatements)
	}
	seenQueryNames := make(map[string]bool, len(fixture.AllowedQueryStatements))
	seenQueryStatements := make(map[string]bool, len(fixture.AllowedQueryStatements))
	for index, allowed := range fixture.AllowedQueryStatements {
		allowed.Statement = normalizeOpenCodeQuery(allowed.Statement)
		fixture.AllowedQueryStatements[index] = allowed
		if strings.TrimSpace(allowed.Name) == "" || allowed.Statement == "" || seenQueryNames[allowed.Name] || seenQueryStatements[allowed.Statement] {
			t.Fatalf("OpenCode allowed-query fixture row is empty or duplicated: %+v", allowed)
		}
		if violations := findOpenCodeForbiddenQueryTokens([]string{allowed.Statement}, fixture.ForbiddenQueryTokens); len(violations) != 0 {
			t.Fatalf("OpenCode allowed-query fixture %q contains forbidden tokens %v", allowed.Name, violations)
		}
		seenQueryNames[allowed.Name] = true
		seenQueryStatements[allowed.Statement] = true
	}
	if fixture.DeclaredQueryMutations != expectedQueryGuardMutations || len(fixture.QueryGuardMutations) != expectedQueryGuardMutations {
		t.Fatalf("OpenCode query-guard mutation fixture row guard: declared=%d actual=%d required=%d", fixture.DeclaredQueryMutations, len(fixture.QueryGuardMutations), expectedQueryGuardMutations)
	}
	seenMutationNames := make(map[string]bool, len(fixture.QueryGuardMutations))
	for index, mutation := range fixture.QueryGuardMutations {
		mutation.ReplaceStatement = normalizeOpenCodeQuery(mutation.ReplaceStatement)
		mutation.ReplacementStatement = normalizeOpenCodeQuery(mutation.ReplacementStatement)
		fixture.QueryGuardMutations[index] = mutation
		if mutation.Name == "" || seenMutationNames[mutation.Name] || !seenQueryStatements[mutation.ReplaceStatement] || mutation.ReplacementStatement == "" || seenQueryStatements[mutation.ReplacementStatement] {
			t.Fatalf("OpenCode query-guard mutation must be uniquely named and replace one allowed statement with one disallowed statement: %+v", mutation)
		}
		seenMutationNames[mutation.Name] = true
	}
	if fixture.DeclaredEntryMutations != expectedEntryPathMutations || len(fixture.EntryPathMutations) != expectedEntryPathMutations {
		t.Fatalf("OpenCode entry-path mutation fixture row guard: declared=%d actual=%d required=%d", fixture.DeclaredEntryMutations, len(fixture.EntryPathMutations), expectedEntryPathMutations)
	}
	seenEntryMutationNames := make(map[string]bool, len(fixture.EntryPathMutations))
	for _, mutation := range fixture.EntryPathMutations {
		if strings.TrimSpace(mutation.Name) == "" || strings.TrimSpace(mutation.ErrorContains) == "" || seenEntryMutationNames[mutation.Name] || !knownOpenCodeEntryPathKind(mutation.Kind) {
			t.Fatalf("OpenCode entry-path mutation fixture is incomplete, duplicated, or unknown: %+v", mutation)
		}
		seenEntryMutationNames[mutation.Name] = true
	}
	if fixture.DeclaredIndexCases != expectedIndexEvidenceCases || len(fixture.IndexEvidenceCases) != expectedIndexEvidenceCases {
		t.Fatalf("OpenCode index-evidence fixture row guard: declared=%d actual=%d required=%d", fixture.DeclaredIndexCases, len(fixture.IndexEvidenceCases), expectedIndexEvidenceCases)
	}
	seenIndexCases := make(map[string]bool, len(fixture.IndexEvidenceCases))
	for _, fixtureCase := range fixture.IndexEvidenceCases {
		if strings.TrimSpace(fixtureCase.Name) == "" || seenIndexCases[fixtureCase.Name] || len(fixtureCase.Keys) != 3 || fixtureCase.ExpectedCapability.Validate() != nil || fixtureCase.ExpectedSupport.Validate() != nil {
			t.Fatalf("OpenCode index-evidence fixture is incomplete or duplicated: %+v", fixtureCase)
		}
		seenIndexCases[fixtureCase.Name] = true
	}
	if fixture.DeclaredFileMutations != expectedFileInventoryMutations || len(fixture.FileInventoryMutations) != expectedFileInventoryMutations {
		t.Fatalf("OpenCode file-inventory mutation fixture row guard: declared=%d actual=%d required=%d", fixture.DeclaredFileMutations, len(fixture.FileInventoryMutations), expectedFileInventoryMutations)
	}
	seenFileMutationNames := make(map[string]bool, len(fixture.FileInventoryMutations))
	seenFileMutationSurfaces := make(map[string]bool, len(fixture.FileInventoryMutations))
	for _, mutation := range fixture.FileInventoryMutations {
		surface := mutation.Filename + "\x00" + string(mutation.Kind)
		if strings.TrimSpace(mutation.Name) == "" || strings.TrimSpace(mutation.ErrorContains) == "" || seenFileMutationNames[mutation.Name] || seenFileMutationSurfaces[surface] || !knownOpenCodeEntryPathKind(mutation.Kind) || !strings.HasSuffix(mutation.Filename, ".go") || strings.HasSuffix(mutation.Filename, "_test.go") || filepath.Base(mutation.Filename) != mutation.Filename {
			t.Fatalf("OpenCode file-inventory mutation fixture is incomplete, duplicated, or outside the production inventory: %+v", mutation)
		}
		seenFileMutationNames[mutation.Name] = true
		seenFileMutationSurfaces[surface] = true
	}
	if fixture.DeclaredCoverageGuards != expectedCoverageGuardMutations || len(fixture.CoverageGuardMutations) != expectedCoverageGuardMutations {
		t.Fatalf("OpenCode coverage-guard mutation fixture row guard: declared=%d actual=%d required=%d", fixture.DeclaredCoverageGuards, len(fixture.CoverageGuardMutations), expectedCoverageGuardMutations)
	}
	seenCoverageGuardNames := make(map[string]bool, len(fixture.CoverageGuardMutations))
	for _, mutation := range fixture.CoverageGuardMutations {
		if strings.TrimSpace(mutation.Name) == "" || strings.TrimSpace(mutation.ErrorContains) == "" || seenCoverageGuardNames[mutation.Name] || !knownOpenCodeCoverageGuardKind(mutation.Kind) {
			t.Fatalf("OpenCode coverage-guard mutation fixture is incomplete, duplicated, or unknown: %+v", mutation)
		}
		seenCoverageGuardNames[mutation.Name] = true
	}
	if fixture.DeclaredBuildTagged != expectedBuildTaggedMutations || len(fixture.BuildTaggedMutations) != expectedBuildTaggedMutations {
		t.Fatalf("OpenCode build-tagged mutation fixture row guard: declared=%d actual=%d required=%d", fixture.DeclaredBuildTagged, len(fixture.BuildTaggedMutations), expectedBuildTaggedMutations)
	}
	if err := validateOpenCodeBuildTaggedMutationCoverage(fixture.BuildTaggedMutations); err != nil {
		t.Fatal(err)
	}
	if fixture.DeclaredBuildTopology != expectedBuildTopologyCases || len(fixture.BuildTopologyCases) != expectedBuildTopologyCases {
		t.Fatalf("OpenCode build-topology fixture row guard: declared=%d actual=%d required=%d", fixture.DeclaredBuildTopology, len(fixture.BuildTopologyCases), expectedBuildTopologyCases)
	}
	if err := validateOpenCodeBuildTopologyCoverage(fixture.BuildTopologyCases); err != nil {
		t.Fatal(err)
	}
	if err := validateOpenCodeMutationCoverage(fixture); err != nil {
		t.Fatal(err)
	}
	seenResolution := make(map[string]bool, len(fixture.ResolutionCases))
	for _, fixtureCase := range fixture.ResolutionCases {
		if strings.TrimSpace(fixtureCase.Name) == "" || strings.TrimSpace(fixtureCase.Channel) == "" || len(fixtureCase.Paths) == 0 || len(fixtureCase.Paths) != len(fixtureCase.Provenance) || seenResolution[fixtureCase.Name] {
			t.Fatalf("OpenCode resolution fixture case is incomplete or duplicated: %+v", fixtureCase)
		}
		for _, provenance := range fixtureCase.Provenance {
			if err := provenance.Validate(); err != nil {
				t.Fatalf("OpenCode resolution fixture %q has invalid provenance: %v", fixtureCase.Name, err)
			}
		}
		seenResolution[fixtureCase.Name] = true
	}
	seenProbe := make(map[string]bool, len(fixture.ProbeCases))
	for _, fixtureCase := range fixture.ProbeCases {
		if strings.TrimSpace(fixtureCase.Fixture) == "" || fixtureCase.Capability.Validate() != nil || fixtureCase.Support.Validate() != nil || seenProbe[fixtureCase.Fixture] {
			t.Fatalf("OpenCode probe fixture case is incomplete or duplicated: %+v", fixtureCase)
		}
		if fixtureCase.Diagnostic == ingest.OpenCodeDiagnosticCatalogTruncated {
			if fixtureCase.OverflowScope == "" || fixtureCase.RetainedRows <= 0 {
				t.Fatalf("OpenCode overflow probe fixture %q lacks scope or retained row count", fixtureCase.Fixture)
			}
		} else if fixtureCase.OverflowScope != "" || fixtureCase.RetainedRows != 0 {
			t.Fatalf("OpenCode non-overflow probe fixture %q declares overflow expectations", fixtureCase.Fixture)
		}
		seenProbe[fixtureCase.Fixture] = true
	}
	seenContinuation := make(map[string]bool, len(fixture.ContinuationCandidates))
	for _, candidate := range fixture.ContinuationCandidates {
		if candidate.Name == "" || seenContinuation[candidate.Name] || candidate.Kind.Validate() != nil || candidate.Provenance.Validate() != nil || candidate.ExpectedSupport.Validate() != nil || candidate.ExpectedCapability == "" {
			t.Fatalf("OpenCode continuation fixture candidate is incomplete, duplicated, or invalid: %+v", candidate)
		}
		switch candidate.PathTemplate {
		case openCodePathMemory, openCodePathMissing, openCodePathLegacyRoot:
			if candidate.Fixture != "" {
				t.Fatalf("OpenCode continuation fixture %q declares an unused fixture %q", candidate.Name, candidate.Fixture)
			}
		case openCodePathFixture:
			if candidate.Fixture == "" {
				t.Fatalf("OpenCode continuation fixture %q requires a named source fixture", candidate.Name)
			}
		default:
			t.Fatalf("OpenCode continuation fixture %q has unknown path template %q", candidate.Name, candidate.PathTemplate)
		}
		seenContinuation[candidate.Name] = true
	}
	seenClosed := make(map[string]bool, len(fixture.ClosedSetCases))
	for _, closedCase := range fixture.ClosedSetCases {
		if closedCase.Name == "" || seenClosed[closedCase.Name] {
			t.Fatalf("OpenCode closed-set fixture case is incomplete or duplicated: %+v", closedCase)
		}
		switch closedCase.Field {
		case openCodeClosedSetKind, openCodeClosedSetPath, openCodeClosedSetProvenance, openCodeClosedSetSupport:
		default:
			t.Fatalf("OpenCode closed-set fixture %q has unknown field %q", closedCase.Name, closedCase.Field)
		}
		seenClosed[closedCase.Name] = true
	}
	return fixture
}

func openCodeBuildTaggedKindContract(kind openCodeBuildTaggedMutationKind) (openCodeBuildTaggedMutationClass, string, string, bool) {
	switch kind {
	case openCodeBuildTaggedDirect:
		return openCodeBuildTaggedClassDirect, "reject-build-tagged-direct-executor", "sqlite_tagged_direct.go", true
	case openCodeBuildTaggedAlias:
		return openCodeBuildTaggedClassAlias, "reject-build-tagged-callable-alias", "sqlite_tagged_alias.go", true
	case openCodeBuildTaggedHelper:
		return openCodeBuildTaggedClassHelper, "reject-build-tagged-helper-parameter", "sqlite_tagged_helper.go", true
	case openCodeBuildTaggedInterface:
		return openCodeBuildTaggedClassInterface, "reject-build-tagged-interface-dispatch", "sqlite_tagged_interface.go", true
	case openCodeBuildTaggedField:
		return openCodeBuildTaggedClassField, "reject-build-tagged-struct-field", "sqlite_tagged_field.go", true
	case openCodeBuildTaggedReturn:
		return openCodeBuildTaggedClassReturn, "reject-build-tagged-returned-callable", "sqlite_tagged_return.go", true
	case openCodeBuildTaggedDirectData:
		return openCodeBuildTaggedClassDirectData, "reject-build-tagged-blob-callable-aliases", "sqlite_tagged_blob.go", true
	default:
		return "", "", "", false
	}
}

func validOpenCodeProductionFilename(filename string) bool {
	return strings.HasSuffix(filename, ".go") && !strings.HasSuffix(filename, "_test.go") && filepath.Base(filename) == filename
}

func validateOpenCodeBuildTaggedMutationCoverage(mutations []openCodeBuildTaggedMutation) error {
	if len(mutations) != expectedBuildTaggedMutations {
		return fmt.Errorf("OpenCode canonical build-tagged matrix contains %d rows, want exact %d", len(mutations), expectedBuildTaggedMutations)
	}
	seenNames := make(map[string]bool, len(mutations))
	seenKinds := make(map[openCodeBuildTaggedMutationKind]bool, len(mutations))
	seenMatrix := make(map[string]bool, len(mutations))
	for _, mutation := range mutations {
		wantClass, wantName, wantFilename, known := openCodeBuildTaggedKindContract(mutation.Kind)
		matrixKey := string(mutation.Kind) + "\x00" + string(mutation.Class)
		if strings.TrimSpace(mutation.Name) == "" || strings.TrimSpace(mutation.ErrorContains) == "" || seenNames[mutation.Name] || seenKinds[mutation.Kind] || !known || mutation.Name != wantName || mutation.Filename != wantFilename || mutation.Class != wantClass || seenMatrix[matrixKey] || len(mutation.BuildTags) != 1 || mutation.BuildTags[0] != "evidence_negative_indexer" || !validOpenCodeProductionFilename(mutation.Filename) {
			return fmt.Errorf("OpenCode canonical build-tagged matrix has an incomplete, duplicate, unknown, or mismatched row: %+v; restore each exact kind/class pair", mutation)
		}
		seenTags := make(map[string]bool, len(mutation.BuildTags))
		for _, tag := range mutation.BuildTags {
			if strings.TrimSpace(tag) == "" || strings.ContainsAny(tag, " ,") || seenTags[tag] {
				return fmt.Errorf("OpenCode build-tagged mutation %q has an empty, malformed, or duplicate build tag %q", mutation.Name, tag)
			}
			seenTags[tag] = true
		}
		seenNames[mutation.Name] = true
		seenKinds[mutation.Kind] = true
		seenMatrix[matrixKey] = true
	}
	if len(seenKinds) != expectedBuildTaggedMutations || len(seenMatrix) != expectedBuildTaggedMutations {
		return fmt.Errorf("OpenCode canonical build-tagged matrix covers %d kinds and %d kind/class pairs, want exact %d/%d", len(seenKinds), len(seenMatrix), expectedBuildTaggedMutations, expectedBuildTaggedMutations)
	}
	return nil
}

func validateOpenCodeBuildTopologyCoverage(cases []openCodeBuildTopologyCase) error {
	if len(cases) != expectedBuildTopologyCases {
		return fmt.Errorf("OpenCode canonical build topology contains %d cases, want exact %d", len(cases), expectedBuildTopologyCases)
	}
	seenCases := make(map[string]bool, len(cases))
	seenOutcomes := make(map[openCodeBuildTopologyOutcome]bool, len(cases))
	for _, fixtureCase := range cases {
		if strings.TrimSpace(fixtureCase.Name) == "" || seenCases[fixtureCase.Name] {
			return fmt.Errorf("OpenCode canonical build topology has an empty or duplicate case name %q", fixtureCase.Name)
		}
		if seenOutcomes[fixtureCase.ExpectedOutcome] {
			return fmt.Errorf("OpenCode canonical build topology duplicates outcome %q; keep one fixture-owned case for each required topology axis", fixtureCase.ExpectedOutcome)
		}
		if err := validateOpenCodeBuildTopologyCase(fixtureCase); err != nil {
			return err
		}
		seenCases[fixtureCase.Name] = true
		seenOutcomes[fixtureCase.ExpectedOutcome] = true
	}
	for _, required := range []openCodeBuildTopologyOutcome{openCodeBuildTopologyDiscovery, openCodeBuildTopologyForbidden, openCodeBuildTopologyUnreachable} {
		if !seenOutcomes[required] {
			return fmt.Errorf("OpenCode canonical build topology is missing required %q behavior", required)
		}
	}
	return nil
}

func validateOpenCodeBuildTopologyCase(fixtureCase openCodeBuildTopologyCase) error {
	if len(fixtureCase.Sources) == 0 {
		return fmt.Errorf("OpenCode build-topology case %q has no production sources", fixtureCase.Name)
	}
	if len(fixtureCase.ExpectedCustomTags) == 0 || len(fixtureCase.ExpectedCustomTags) > 8 {
		return fmt.Errorf("OpenCode build-topology case %q declares %d custom tags; require 1..8 to preserve the 256-configuration bound", fixtureCase.Name, len(fixtureCase.ExpectedCustomTags))
	}
	seenTags := make(map[string]bool, len(fixtureCase.ExpectedCustomTags))
	for index, tag := range fixtureCase.ExpectedCustomTags {
		if strings.TrimSpace(tag) == "" || strings.ContainsAny(tag, " ,") || seenTags[tag] || (index > 0 && fixtureCase.ExpectedCustomTags[index-1] >= tag) {
			return fmt.Errorf("OpenCode build-topology case %q has empty, malformed, duplicate, or unsorted expected custom tag %q", fixtureCase.Name, tag)
		}
		seenTags[tag] = true
	}
	if fixtureCase.ExpectedConfigurations < 0 || fixtureCase.ExpectedConfigurations > 1<<len(fixtureCase.ExpectedCustomTags) || fixtureCase.ExpectedConfigurations > 256 {
		return fmt.Errorf("OpenCode build-topology case %q configuration count %d exceeds its %d-tag/256-configuration bound", fixtureCase.Name, fixtureCase.ExpectedConfigurations, len(fixtureCase.ExpectedCustomTags))
	}
	if fixtureCase.ExpectedForbiddenConfigurations < 0 || fixtureCase.ExpectedForbiddenConfigurations > fixtureCase.ExpectedConfigurations {
		return fmt.Errorf("OpenCode build-topology case %q forbidden configuration count %d is outside 0..%d", fixtureCase.Name, fixtureCase.ExpectedForbiddenConfigurations, fixtureCase.ExpectedConfigurations)
	}

	seenSourceNames := make(map[string]bool, len(fixtureCase.Sources))
	seenFilenames := make(map[string]bool, len(fixtureCase.Sources))
	independentTags := make(map[string]bool)
	hasConjunctionExecution := false
	hasUnreachableSource := false
	for _, source := range fixtureCase.Sources {
		if strings.TrimSpace(source.Name) == "" || seenSourceNames[source.Name] || !validOpenCodeProductionFilename(source.Filename) || seenFilenames[source.Filename] {
			return fmt.Errorf("OpenCode build-topology case %q has an incomplete or duplicate source name/filename: %+v", fixtureCase.Name, source)
		}
		expression, err := constraint.Parse("//go:build " + source.BuildExpression)
		if err != nil {
			return fmt.Errorf("OpenCode build-topology case %q source %q has invalid build expression %q: %w", fixtureCase.Name, source.Name, source.BuildExpression, err)
		}
		expressionTags := make(map[string]bool)
		openCodeCollectConstraintTags(expression, expressionTags)
		if len(expressionTags) == 0 {
			return fmt.Errorf("OpenCode build-topology case %q source %q has no discoverable custom build tag", fixtureCase.Name, source.Name)
		}
		for tag := range expressionTags {
			if !seenTags[tag] {
				return fmt.Errorf("OpenCode build-topology case %q source %q expression tag %q is absent from expected custom tags", fixtureCase.Name, source.Name, tag)
			}
		}
		minimumEnabled, satisfiable := openCodeMinimumEnabledBuildTags(expression, expressionTags)
		if len(expressionTags) == 1 && satisfiable && minimumEnabled == 1 && source.Kind == openCodeBuildTopologyAnchor {
			for tag := range expressionTags {
				independentTags[tag] = true
			}
		}
		switch source.Kind {
		case openCodeBuildTopologyAnchor:
		case openCodeBuildTopologyExecution:
			if satisfiable && minimumEnabled >= 2 {
				hasConjunctionExecution = true
			}
		default:
			return fmt.Errorf("OpenCode build-topology case %q source %q has unknown kind %q", fixtureCase.Name, source.Name, source.Kind)
		}
		if !satisfiable {
			hasUnreachableSource = true
		}
		if source.ExpectedActivations < 0 || source.ExpectedActivations > fixtureCase.ExpectedConfigurations {
			return fmt.Errorf("OpenCode build-topology case %q source %q activation count %d is outside 0..%d", fixtureCase.Name, source.Name, source.ExpectedActivations, fixtureCase.ExpectedConfigurations)
		}
		seenSourceNames[source.Name] = true
		seenFilenames[source.Filename] = true
	}

	switch fixtureCase.ExpectedOutcome {
	case openCodeBuildTopologyDiscovery:
		if len(independentTags) < 2 || fixtureCase.ExpectedConfigurations == 0 || fixtureCase.ExpectedForbiddenConfigurations != 0 || fixtureCase.ErrorContains != "" {
			return fmt.Errorf("OpenCode build-topology case %q must observably discover at least two independent tags without a failure diagnostic", fixtureCase.Name)
		}
	case openCodeBuildTopologyForbidden:
		if len(independentTags) < 2 || !hasConjunctionExecution || fixtureCase.ExpectedConfigurations == 0 || fixtureCase.ExpectedForbiddenConfigurations == 0 || strings.TrimSpace(fixtureCase.ErrorContains) == "" {
			return fmt.Errorf("OpenCode build-topology case %q must mount a conjunction-only forbidden callable with independent tags and an actionable diagnostic", fixtureCase.Name)
		}
	case openCodeBuildTopologyUnreachable:
		if !hasUnreachableSource || fixtureCase.ExpectedConfigurations != 0 || fixtureCase.ExpectedForbiddenConfigurations != 0 || strings.TrimSpace(fixtureCase.ErrorContains) == "" {
			return fmt.Errorf("OpenCode build-topology case %q must mount an unsatisfiable physical source with an actionable unreachable-file diagnostic", fixtureCase.Name)
		}
	default:
		return fmt.Errorf("OpenCode build-topology case %q has unknown outcome %q", fixtureCase.Name, fixtureCase.ExpectedOutcome)
	}
	return nil
}

func openCodeMinimumEnabledBuildTags(expression constraint.Expr, tags map[string]bool) (int, bool) {
	ordered := make([]string, 0, len(tags))
	for tag := range tags {
		ordered = append(ordered, tag)
	}
	sort.Strings(ordered)
	minimum := len(ordered) + 1
	for mask := 0; mask < 1<<len(ordered); mask++ {
		if !expression.Eval(func(tag string) bool {
			index := sort.SearchStrings(ordered, tag)
			return index < len(ordered) && ordered[index] == tag && mask&(1<<index) != 0
		}) {
			continue
		}
		enabled := 0
		for bits := mask; bits != 0; bits &= bits - 1 {
			enabled++
		}
		if enabled < minimum {
			minimum = enabled
		}
	}
	return minimum, minimum <= len(ordered)
}

func knownOpenCodeEntryPathKind(kind openCodeEntryPathKind) bool {
	switch kind {
	case openCodeEntrySQLitexExecute, openCodeEntryPrepare, openCodeEntryPrepareTransient, openCodeEntryExecuteScript,
		openCodeEntryStatementStep, openCodeEntrySQLitexExec, openCodeEntryConnPrep, openCodeEntryExecuteFS,
		openCodeEntryImportAlias, openCodeEntryCallableAlias, openCodeEntryReceiverAlias, openCodeEntryCapturedStep,
		openCodeEntryHelperExecute, openCodeEntryHelperStep, openCodeEntryReturnedExecute, openCodeEntryStructField,
		openCodeEntryInterfaceField, openCodeEntryInterfacePrepare, openCodeEntryInterfaceStep, openCodeEntryExecutorHistory,
		openCodeEntryExecutorPrepare, openCodeEntryPrepareArgument, openCodeEntryExecuteArgument, openCodeEntryExecutorEscape,
		openCodeEntryInitializerExtra, openCodeEntryPackageValue, openCodeEntryExecutorInterface, openCodeEntryCurrentOpenBlob,
		openCodeEntryOpenBlobAlias, openCodeEntrySerialize, openCodeEntrySerializeInterface, openCodeEntryDeserialize,
		openCodeEntryNewBackup, openCodeEntryBackupStep, openCodeEntryBackupStepAlias, openCodeEntryBlobRead,
		openCodeEntryBlobReadAlias, openCodeEntryBlobWriteTo, openCodeEntrySessionDiff, openCodeEntrySessionChangeset,
		openCodeEntryExecutorPrepareInterface:
		return true
	default:
		return false
	}
}

func knownOpenCodeCoverageGuardKind(kind openCodeCoverageGuardKind) bool {
	switch kind {
	case openCodeCoverageEntryKindReplacement, openCodeCoverageFileKindSubstitution, openCodeCoverageFileClassOmission,
		openCodeCoverageBuildKindSubstitution, openCodeCoverageBuildClassSubstitution, openCodeCoverageBuildNameSubstitution,
		openCodeCoverageTopologyTagSubstitution, openCodeCoverageTopologyExpressionSubstitution, openCodeCoverageTopologyOutcomeSubstitution:
		return true
	default:
		return false
	}
}

func openCodeFileMatrixKind(kind openCodeEntryPathKind) bool {
	switch kind {
	case openCodeEntrySQLitexExec, openCodeEntryConnPrep, openCodeEntryExecuteFS, openCodeEntryImportAlias,
		openCodeEntryCallableAlias, openCodeEntryReceiverAlias, openCodeEntryCapturedStep, openCodeEntryHelperExecute,
		openCodeEntryHelperStep, openCodeEntryReturnedExecute, openCodeEntryStructField, openCodeEntryInterfaceField,
		openCodeEntryInterfacePrepare, openCodeEntryInterfaceStep, openCodeEntryExecutorEscape, openCodeEntryPackageValue,
		openCodeEntryExecutorInterface, openCodeEntryCurrentOpenBlob, openCodeEntryOpenBlobAlias, openCodeEntrySerializeInterface,
		openCodeEntryNewBackup, openCodeEntryBackupStepAlias, openCodeEntryBlobReadAlias:
		return true
	default:
		return false
	}
}

func validateOpenCodeMutationCoverage(fixture openCodeCandidateFixture) error {
	if expectedEntryPathMutations != expectedEntryPathKindCount {
		return fmt.Errorf("OpenCode entry-kind contract is internally inconsistent: rows=%d closed-set=%d", expectedEntryPathMutations, expectedEntryPathKindCount)
	}
	entryKinds := make(map[openCodeEntryPathKind]int, len(fixture.EntryPathMutations))
	for _, mutation := range fixture.EntryPathMutations {
		entryKinds[mutation.Kind]++
		if entryKinds[mutation.Kind] != 1 {
			return fmt.Errorf("OpenCode canonical entry-kind coverage has %d rows for %q, want exactly one; restore the required direct/alias/data-access case instead of substituting another known kind", entryKinds[mutation.Kind], mutation.Kind)
		}
	}
	if len(entryKinds) != expectedEntryPathKindCount {
		return fmt.Errorf("OpenCode canonical entry-kind coverage contains %d distinct kinds, want exact closed set of %d", len(entryKinds), expectedEntryPathKindCount)
	}

	type fileClasses struct {
		existing int
		newFile  int
	}
	matrix := make(map[openCodeEntryPathKind]fileClasses, expectedFileMatrixKindCount)
	for _, mutation := range fixture.FileInventoryMutations {
		if !openCodeFileMatrixKind(mutation.Kind) {
			return fmt.Errorf("OpenCode canonical file matrix contains substituted kind %q; only the typed existing/new mutation families may appear", mutation.Kind)
		}
		classes := matrix[mutation.Kind]
		if mutation.Filename == "opencode.go" {
			classes.existing++
		} else {
			classes.newFile++
		}
		matrix[mutation.Kind] = classes
	}
	if len(matrix) != expectedFileMatrixKindCount {
		return fmt.Errorf("OpenCode canonical file matrix contains %d distinct kinds, want %d exact existing/new families", len(matrix), expectedFileMatrixKindCount)
	}
	for kind, classes := range matrix {
		if classes.existing != 1 || classes.newFile != 1 {
			return fmt.Errorf("OpenCode canonical file matrix kind %q has existing/new multiplicity %d/%d, want exactly 1/1 with opencode.go plus one newly discovered production filename", kind, classes.existing, classes.newFile)
		}
	}
	return nil
}

func TestOpenCodeMutationCoverageRejectsFixtureOwnedDrift(t *testing.T) {
	fixture := loadOpenCodeCandidateFixture(t)
	sourceDirectory, production := openCodeBuildTopologyProduction(t)
	for _, mutation := range fixture.CoverageGuardMutations {
		mutation := mutation
		t.Run(mutation.Name, func(t *testing.T) {
			mutated := fixture
			mutated.EntryPathMutations = append([]openCodeEntryPathMutation(nil), fixture.EntryPathMutations...)
			mutated.FileInventoryMutations = append([]openCodeFileInventoryMutation(nil), fixture.FileInventoryMutations...)
			mutated.BuildTaggedMutations = append([]openCodeBuildTaggedMutation(nil), fixture.BuildTaggedMutations...)
			mutated.BuildTopologyCases = cloneOpenCodeBuildTopologyCases(fixture.BuildTopologyCases)
			var err error
			switch mutation.Kind {
			case openCodeCoverageEntryKindReplacement:
				replaceOpenCodeEntryKind(t, mutated.EntryPathMutations, openCodeEntryOpenBlobAlias, openCodeEntrySerialize)
			case openCodeCoverageFileKindSubstitution:
				replaceOpenCodeFileKind(t, mutated.FileInventoryMutations, openCodeEntryBlobReadAlias, openCodeEntrySerializeInterface)
			case openCodeCoverageFileClassOmission:
				replaceOpenCodeExistingFilename(t, mutated.FileInventoryMutations, openCodeEntryBlobReadAlias, "sqlite_escape.go")
			case openCodeCoverageBuildKindSubstitution:
				mutated.BuildTaggedMutations[0].Kind = openCodeBuildTaggedAlias
				err = validateOpenCodeBuildTaggedMutationCoverage(mutated.BuildTaggedMutations)
			case openCodeCoverageBuildClassSubstitution:
				mutated.BuildTaggedMutations[0].Class = openCodeBuildTaggedClassAlias
				err = validateOpenCodeBuildTaggedMutationCoverage(mutated.BuildTaggedMutations)
			case openCodeCoverageBuildNameSubstitution:
				mutated.BuildTaggedMutations[0].Name = "renamed-build-tagged-direct-executor"
				err = validateOpenCodeBuildTaggedMutationCoverage(mutated.BuildTaggedMutations)
			case openCodeCoverageTopologyTagSubstitution:
				fixtureCase := openCodeBuildTopologyCaseForOutcome(t, mutated.BuildTopologyCases, openCodeBuildTopologyDiscovery)
				fixtureCase.ExpectedCustomTags[1] = "topology_substitute"
				err = runOpenCodeBuildTopologyCase(t, fixture, sourceDirectory, production, fixtureCase)
			case openCodeCoverageTopologyExpressionSubstitution:
				fixtureCase := openCodeBuildTopologyCaseForOutcome(t, mutated.BuildTopologyCases, openCodeBuildTopologyForbidden)
				execution := openCodeBuildTopologySourceForKind(t, fixtureCase.Sources, openCodeBuildTopologyExecution)
				tags := openCodeBuildExpressionTags(t, execution.BuildExpression)
				if len(tags) < 2 {
					t.Fatalf("fixture-owned conjunction mutation requires at least two expression tags, got %v", tags)
				}
				execution.BuildExpression = strings.Join(tags, " || ")
				err = runOpenCodeBuildTopologyCase(t, fixture, sourceDirectory, production, fixtureCase)
			case openCodeCoverageTopologyOutcomeSubstitution:
				fixtureCase := openCodeBuildTopologyCaseForOutcome(t, mutated.BuildTopologyCases, openCodeBuildTopologyUnreachable)
				fixtureCase.ExpectedOutcome = openCodeBuildTopologyDiscovery
				err = runOpenCodeBuildTopologyCase(t, fixture, sourceDirectory, production, fixtureCase)
			default:
				t.Fatalf("unknown fixture-owned coverage mutation %q", mutation.Kind)
			}
			if err == nil {
				err = validateOpenCodeMutationCoverage(mutated)
			}
			if err == nil || !strings.Contains(err.Error(), mutation.ErrorContains) {
				t.Fatalf("OpenCode coverage mutation %q error = %v, want substring %q", mutation.Name, err, mutation.ErrorContains)
			}
		})
	}
}

func openCodeBuildTopologyCaseForOutcome(t testing.TB, cases []openCodeBuildTopologyCase, outcome openCodeBuildTopologyOutcome) openCodeBuildTopologyCase {
	t.Helper()
	for _, fixtureCase := range cases {
		if fixtureCase.ExpectedOutcome == outcome {
			return fixtureCase
		}
	}
	t.Fatalf("fixture-owned build topology has no case for outcome %q", outcome)
	return openCodeBuildTopologyCase{}
}

func openCodeBuildTopologySourceForKind(t testing.TB, sources []openCodeBuildTopologySource, kind openCodeBuildTopologySourceKind) *openCodeBuildTopologySource {
	t.Helper()
	for index := range sources {
		if sources[index].Kind == kind {
			return &sources[index]
		}
	}
	t.Fatalf("fixture-owned build topology has no source for kind %q", kind)
	return nil
}

func openCodeBuildExpressionTags(t testing.TB, buildExpression string) []string {
	t.Helper()
	expression, err := constraint.Parse("//go:build " + buildExpression)
	if err != nil {
		t.Fatalf("parse fixture-owned build expression %q: %v", buildExpression, err)
	}
	tagSet := make(map[string]bool)
	openCodeCollectConstraintTags(expression, tagSet)
	tags := make([]string, 0, len(tagSet))
	for tag := range tagSet {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags
}

func cloneOpenCodeBuildTopologyCases(cases []openCodeBuildTopologyCase) []openCodeBuildTopologyCase {
	cloned := append([]openCodeBuildTopologyCase(nil), cases...)
	for index := range cloned {
		cloned[index].Sources = append([]openCodeBuildTopologySource(nil), cases[index].Sources...)
		cloned[index].ExpectedCustomTags = append([]string(nil), cases[index].ExpectedCustomTags...)
	}
	return cloned
}

func replaceOpenCodeEntryKind(t testing.TB, mutations []openCodeEntryPathMutation, oldKind, replacement openCodeEntryPathKind) {
	t.Helper()
	for index := range mutations {
		if mutations[index].Kind == oldKind {
			mutations[index].Kind = replacement
			return
		}
	}
	t.Fatalf("fixture-owned entry-kind mutation anchor %q is absent", oldKind)
}

func replaceOpenCodeFileKind(t testing.TB, mutations []openCodeFileInventoryMutation, oldKind, replacement openCodeEntryPathKind) {
	t.Helper()
	replaced := 0
	for index := range mutations {
		if mutations[index].Kind == oldKind {
			mutations[index].Kind = replacement
			replaced++
		}
	}
	if replaced != 2 {
		t.Fatalf("fixture-owned file-kind mutation replaced %d rows for %q, want exact existing/new pair", replaced, oldKind)
	}
}

func replaceOpenCodeExistingFilename(t testing.TB, mutations []openCodeFileInventoryMutation, kind openCodeEntryPathKind, replacement string) {
	t.Helper()
	for index := range mutations {
		if mutations[index].Kind == kind && mutations[index].Filename == "opencode.go" {
			mutations[index].Filename = replacement
			return
		}
	}
	t.Fatalf("fixture-owned file-class mutation anchor %q/opencode.go is absent", kind)
}

func TestOpenCodePrivateExecutionStatementsMatchFixtureAllowlist(t *testing.T) {
	t.Parallel()
	fixture := loadOpenCodeCandidateFixture(t)
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve OpenCode candidate query guard location")
	}
	directory := filepath.Dir(currentFile)
	configurations, err := openCodePackageProductionConfigurations(directory)
	if err != nil {
		t.Fatalf("discover every ingest build configuration for type-aware OpenCode inventory: %v", err)
	}
	for _, configuration := range configurations {
		statements, extractErr := extractOpenCodePrivateExecutionStatements(configuration.files, configuration.files)
		if extractErr != nil {
			t.Fatalf("resolve private OpenCode SQLite execution statements for build tags %v: %v", configuration.tags, extractErr)
		}
		if validationErr := validateOpenCodePrivateExecutionStatements(statements, fixture); validationErr != nil {
			t.Fatalf("validate private OpenCode SQLite execution statements for build tags %v: %v", configuration.tags, validationErr)
		}

		for _, mutation := range fixture.QueryGuardMutations {
			mutated := append([]string(nil), statements...)
			replaced := 0
			for index, statement := range mutated {
				if statement == mutation.ReplaceStatement {
					mutated[index] = mutation.ReplacementStatement
					replaced++
				}
			}
			if replaced != 1 {
				t.Fatalf("OpenCode query-guard mutation %q replaced %d actual execution statements for build tags %v, want exactly one", mutation.Name, replaced, configuration.tags)
			}
			if validationErr := validateOpenCodePrivateExecutionStatements(mutated, fixture); validationErr == nil {
				t.Fatalf("OpenCode private execution guard accepted fixture-owned mutation %q for build tags %v", mutation.Name, configuration.tags)
			}
		}
	}
}

func TestOpenCodePrivateExecutionGuardDiscoversFixtureOwnedProductionFiles(t *testing.T) {
	fixture := loadOpenCodeCandidateFixture(t)
	for _, mutation := range fixture.FileInventoryMutations {
		mutation := mutation
		t.Run(mutation.Name, func(t *testing.T) {
			directory := t.TempDir()
			filename := filepath.Join(directory, mutation.Filename)
			source, err := openCodeEntryPathMutationSource(mutation.Kind)
			if err != nil {
				t.Fatalf("construct fixture-owned OpenCode production bypass %q: %v", mutation.Filename, err)
			}
			if err := os.WriteFile(filename, []byte(source), 0o600); err != nil {
				t.Fatalf("write fixture-owned compiling OpenCode production bypass %q: %v", mutation.Filename, err)
			}
			production, err := ingestProductionFiles(directory)
			if err != nil {
				t.Fatalf("discover fixture-owned OpenCode production inventory: %v", err)
			}
			if len(production) != 1 || filepath.Base(production[0]) != mutation.Filename {
				t.Fatalf("fixture-owned OpenCode production inventory = %v, want discovered %q", production, mutation.Filename)
			}
			_, err = extractOpenCodePrivateExecutionStatements(production, production)
			if err == nil || !strings.Contains(err.Error(), mutation.ErrorContains) {
				t.Fatalf("discovered production bypass error = %v, want substring %q", err, mutation.ErrorContains)
			}
		})
	}
}

func TestOpenCodePrivateExecutionGuardRejectsFixtureOwnedBuildTaggedBypasses(t *testing.T) {
	fixture := loadOpenCodeCandidateFixture(t)
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve OpenCode build-tagged query guard location")
	}
	configurations, err := openCodePackageProductionConfigurations(filepath.Dir(currentFile))
	if err != nil {
		t.Fatalf("discover build-tagged ingest package configurations: %v", err)
	}
	negativeConfiguration, found := openCodeConfigurationWithTags(configurations, []string{"evidence_negative_indexer"})
	if !found || !openCodeConfigurationContainsFile(negativeConfiguration, "model_observation_capture_negative.go") || openCodeConfigurationContainsFile(negativeConfiguration, "model_observation_capture_default.go") {
		t.Fatalf("evidence_negative_indexer production configuration = tags %v files %v, want the build-tagged negative source and not its default complement", negativeConfiguration.tags, negativeConfiguration.files)
	}
	for _, mutation := range fixture.BuildTaggedMutations {
		mutation := mutation
		t.Run(mutation.Name, func(t *testing.T) {
			configuration, found := openCodeConfigurationWithTags(configurations, mutation.BuildTags)
			if !found {
				t.Fatalf("fixture-owned build-tagged mutation %q requires tags %v, but no production package configuration activates them", mutation.Name, mutation.BuildTags)
			}
			source, sourceErr := openCodeBuildTaggedMutationSource(mutation)
			if sourceErr != nil {
				t.Fatalf("construct fixture-owned build-tagged SQLite bypass: %v", sourceErr)
			}
			filename := filepath.Join(t.TempDir(), mutation.Filename)
			if writeErr := os.WriteFile(filename, []byte(source), 0o600); writeErr != nil {
				t.Fatalf("write fixture-owned build-tagged SQLite bypass: %v", writeErr)
			}
			typeCheckFiles := append(append([]string(nil), configuration.files...), filename)
			if _, typeErr := typeCheckOpenCodeSources(typeCheckFiles); typeErr != nil {
				t.Fatalf("build-tagged mutation %q is not a compiling bypass under tags %v: %v", mutation.Name, configuration.tags, typeErr)
			}
			_, guardErr := extractOpenCodePrivateExecutionStatements(typeCheckFiles, []string{filename})
			if guardErr == nil || !strings.Contains(guardErr.Error(), mutation.ErrorContains) {
				t.Fatalf("build-tagged production bypass error = %v, want substring %q", guardErr, mutation.ErrorContains)
			}
		})
	}
}

func TestOpenCodePrivateExecutionGuardCoversFixtureOwnedBuildTopology(t *testing.T) {
	fixture := loadOpenCodeCandidateFixture(t)
	sourceDirectory, production := openCodeBuildTopologyProduction(t)
	for _, fixtureCase := range fixture.BuildTopologyCases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			if err := runOpenCodeBuildTopologyCase(t, fixture, sourceDirectory, production, fixtureCase); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func openCodeBuildTopologyProduction(t testing.TB) (string, []string) {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve OpenCode build-topology guard location")
	}
	sourceDirectory := filepath.Dir(currentFile)
	production, err := ingestProductionFiles(sourceDirectory)
	if err != nil {
		t.Fatalf("discover production files for build-topology copies: %v", err)
	}
	return sourceDirectory, production
}

func runOpenCodeBuildTopologyCase(t testing.TB, fixture openCodeCandidateFixture, sourceDirectory string, production []string, fixtureCase openCodeBuildTopologyCase) error {
	t.Helper()
	directory, err := os.MkdirTemp(sourceDirectory, ".sqlite-topology-")
	if err != nil {
		return fmt.Errorf("create isolated package for build-topology case %q: %w", fixtureCase.Name, err)
	}
	t.Cleanup(func() {
		if removeErr := os.RemoveAll(directory); removeErr != nil {
			t.Errorf("remove isolated build-topology package %q: %v", directory, removeErr)
		}
	})
	for _, filename := range production {
		data, readErr := os.ReadFile(filename)
		if readErr != nil {
			return fmt.Errorf("read production source %q for build-topology copy: %w", filename, readErr)
		}
		if writeErr := os.WriteFile(filepath.Join(directory, filepath.Base(filename)), data, 0o600); writeErr != nil {
			return fmt.Errorf("copy production source %q into build-topology package: %w", filename, writeErr)
		}
	}
	for _, source := range fixtureCase.Sources {
		data, sourceErr := openCodeBuildTopologySourceText(source)
		if sourceErr != nil {
			return fmt.Errorf("construct build-topology source %q: %w", source.Name, sourceErr)
		}
		if writeErr := os.WriteFile(filepath.Join(directory, source.Filename), []byte(data), 0o600); writeErr != nil {
			return fmt.Errorf("write build-topology source %q: %w", source.Filename, writeErr)
		}
	}

	physical, err := ingestProductionFiles(directory)
	if err != nil {
		return fmt.Errorf("discover copied build-topology production files: %w", err)
	}
	tags, err := openCodeCustomBuildTags(physical)
	if err != nil {
		return fmt.Errorf("discover custom build tags from copied physical sources: %w", err)
	}
	if !reflect.DeepEqual(tags, fixtureCase.ExpectedCustomTags) {
		return fmt.Errorf("build-topology custom tags = %v, want fixture-owned tags %v", tags, fixtureCase.ExpectedCustomTags)
	}

	configurations, configurationErr := openCodePackageProductionConfigurations(directory)
	if fixtureCase.ExpectedOutcome == openCodeBuildTopologyUnreachable {
		if configurationErr == nil || !strings.Contains(configurationErr.Error(), fixtureCase.ErrorContains) {
			return fmt.Errorf("unreachable build-topology configuration error = %v, want fixture-owned diagnostic substring %q", configurationErr, fixtureCase.ErrorContains)
		}
		return nil
	}
	if configurationErr != nil {
		return fmt.Errorf("enumerate build-topology configurations for expected outcome %q: %w", fixtureCase.ExpectedOutcome, configurationErr)
	}
	if len(configurations) != fixtureCase.ExpectedConfigurations {
		return fmt.Errorf("build-topology configuration count = %d, want fixture-owned %d", len(configurations), fixtureCase.ExpectedConfigurations)
	}

	activationCounts := make(map[string]int, len(fixtureCase.Sources))
	forbiddenConfigurations := 0
	for _, configuration := range configurations {
		for _, source := range fixtureCase.Sources {
			if openCodeConfigurationContainsFile(configuration, source.Filename) {
				activationCounts[source.Name]++
			}
		}
		statements, extractErr := extractOpenCodePrivateExecutionStatements(configuration.files, configuration.files)
		if extractErr != nil {
			if fixtureCase.ExpectedOutcome == openCodeBuildTopologyForbidden && strings.Contains(extractErr.Error(), fixtureCase.ErrorContains) {
				forbiddenConfigurations++
				continue
			}
			return fmt.Errorf("extract SQLite callables for build tags %v and expected outcome %q: %w", configuration.tags, fixtureCase.ExpectedOutcome, extractErr)
		}
		if validationErr := validateOpenCodePrivateExecutionStatements(statements, fixture); validationErr != nil {
			return fmt.Errorf("validate exact initializer/executor and 15-statement allowlist for build tags %v: %w", configuration.tags, validationErr)
		}
	}
	for _, source := range fixtureCase.Sources {
		if activationCounts[source.Name] != source.ExpectedActivations {
			return fmt.Errorf("build-topology source %q activated in %d configurations, want fixture-owned %d for expression %q", source.Name, activationCounts[source.Name], source.ExpectedActivations, source.BuildExpression)
		}
	}
	if forbiddenConfigurations != fixtureCase.ExpectedForbiddenConfigurations {
		return fmt.Errorf("build-topology forbidden configuration count = %d, want fixture-owned %d", forbiddenConfigurations, fixtureCase.ExpectedForbiddenConfigurations)
	}
	return nil
}

func openCodeBuildTopologySourceText(source openCodeBuildTopologySource) (string, error) {
	prefix := "//go:build " + source.BuildExpression + "\n\n"
	switch source.Kind {
	case openCodeBuildTopologyAnchor:
		return prefix + "package ingest\n", nil
	case openCodeBuildTopologyExecution:
		return prefix + `package ingest
import (
	"zombiezen.com/go/sqlite"
	zx "zombiezen.com/go/sqlite/sqlitex"
)
func topologyConjunctionExecution(conn *sqlite.Conn) error {
	return zx.Execute(conn, "SELECT data FROM event", nil)
}
`, nil
	default:
		return "", fmt.Errorf("unknown fixture-owned build-topology source kind %q", source.Kind)
	}
}

func openCodeConfigurationContainsFile(configuration openCodePackageConfiguration, basename string) bool {
	for _, filename := range configuration.files {
		if filepath.Base(filename) == basename {
			return true
		}
	}
	return false
}

func openCodeConfigurationWithTags(configurations []openCodePackageConfiguration, required []string) (openCodePackageConfiguration, bool) {
	for _, configuration := range configurations {
		enabled := make(map[string]bool, len(configuration.tags))
		for _, tag := range configuration.tags {
			enabled[tag] = true
		}
		matches := true
		for _, tag := range required {
			if !enabled[tag] {
				matches = false
				break
			}
		}
		if matches {
			return configuration, true
		}
	}
	return openCodePackageConfiguration{}, false
}

func openCodeBuildTaggedMutationSource(mutation openCodeBuildTaggedMutation) (string, error) {
	prefix := "//go:build " + strings.Join(mutation.BuildTags, " && ") + "\n\n"
	var body string
	switch mutation.Kind {
	case openCodeBuildTaggedDirect:
		body = `package ingest
import (
	"zombiezen.com/go/sqlite"
	zx "zombiezen.com/go/sqlite/sqlitex"
)
func buildTaggedDirectBypass(conn *sqlite.Conn) error { return zx.Execute(conn, "SELECT data FROM event", nil) }
`
	case openCodeBuildTaggedAlias:
		body = `package ingest
import (
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)
func buildTaggedAliasBypass(conn *sqlite.Conn) error {
	execute := sqlitex.Execute
	return execute(conn, "SELECT data FROM event", nil)
}
`
	case openCodeBuildTaggedHelper:
		body = `package ingest
import (
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)
func buildTaggedInvoke(fn func(*sqlite.Conn, string, *sqlitex.ExecOptions) error, conn *sqlite.Conn) error {
	return fn(conn, "SELECT data FROM event", nil)
}
func buildTaggedHelperBypass(conn *sqlite.Conn) error { return buildTaggedInvoke(sqlitex.Execute, conn) }
`
	case openCodeBuildTaggedInterface:
		body = `package ingest
import "zombiezen.com/go/sqlite"
type buildTaggedPreparer interface { PrepareTransient(string) (*sqlite.Stmt, int, error) }
func buildTaggedInterfaceBypass(conn *sqlite.Conn) error {
	var preparer buildTaggedPreparer = conn
	_, _, err := preparer.PrepareTransient("SELECT data FROM event")
	return err
}
`
	case openCodeBuildTaggedField:
		body = `package ingest
import (
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)
type buildTaggedHolder struct { execute func(*sqlite.Conn, string, *sqlitex.ExecOptions) error }
func buildTaggedFieldBypass(conn *sqlite.Conn) error {
	holder := buildTaggedHolder{execute: sqlitex.Execute}
	return holder.execute(conn, "SELECT data FROM event", nil)
}
`
	case openCodeBuildTaggedReturn:
		body = `package ingest
import (
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)
func buildTaggedReturnedExecute() func(*sqlite.Conn, string, *sqlitex.ExecOptions) error { return sqlitex.Execute }
func buildTaggedReturnBypass(conn *sqlite.Conn) error { return buildTaggedReturnedExecute()(conn, "SELECT data FROM event", nil) }
`
	case openCodeBuildTaggedDirectData:
		body = `package ingest
import "zombiezen.com/go/sqlite"
func buildTaggedBlobAliases(conn *sqlite.Conn, buffer []byte) error {
	open := conn.OpenBlob
	blob, err := open("main", "event", "data", 1, false)
	if err != nil { return err }
	defer blob.Close()
	read := blob.Read
	_, err = read(buffer)
	return err
}
`
	default:
		return "", fmt.Errorf("unknown fixture-owned build-tagged SQLite mutation kind %q", mutation.Kind)
	}
	return prefix + body, nil
}

func ingestProductionFiles(directory string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(directory, "*.go"))
	if err != nil {
		return nil, fmt.Errorf("resolve ingest production source pattern: %w", err)
	}
	production := make([]string, 0, len(matches))
	for _, filename := range matches {
		if strings.HasSuffix(filename, "_test.go") {
			continue
		}
		info, statErr := os.Stat(filename)
		if statErr != nil {
			return nil, fmt.Errorf("inspect discovered ingest production source %q: %w", filename, statErr)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("discovered ingest production source %q is not a regular file; the SQLite entry-path inventory cannot be trusted; keep production Go sources as regular files", filename)
		}
		production = append(production, filename)
	}
	if len(production) == 0 {
		return nil, fmt.Errorf("discover ingest production sources in %q failed: no regular non-test Go files exist, so SQLite execution entry points cannot be inventoried; restore the production package", directory)
	}
	sort.Strings(production)
	return production, nil
}

type openCodePackageConfiguration struct {
	tags  []string
	files []string
}

func openCodePackageProductionConfigurations(directory string) ([]openCodePackageConfiguration, error) {
	production, err := ingestProductionFiles(directory)
	if err != nil {
		return nil, err
	}
	tags, err := openCodeCustomBuildTags(production)
	if err != nil {
		return nil, err
	}
	if len(tags) > 8 {
		return nil, fmt.Errorf("discover ingest build configurations found %d custom tags, exceeding the conservative 256-configuration guard bound; split the package or add a bounded configuration policy before adding more SQLite execution surfaces", len(tags))
	}
	seenConfigurations := make(map[string]bool)
	coveredFiles := make(map[string]bool, len(production))
	configurations := make([]openCodePackageConfiguration, 0, 1<<len(tags))
	for mask := 0; mask < 1<<len(tags); mask++ {
		enabled := make([]string, 0, len(tags))
		for index, tag := range tags {
			if mask&(1<<index) != 0 {
				enabled = append(enabled, tag)
			}
		}
		files, listErr := openCodeGoListProductionFiles(directory, enabled)
		if listErr != nil {
			return nil, listErr
		}
		key := strings.Join(files, "\x00")
		if seenConfigurations[key] {
			continue
		}
		seenConfigurations[key] = true
		for _, filename := range files {
			coveredFiles[filepath.Clean(filename)] = true
		}
		configurations = append(configurations, openCodePackageConfiguration{tags: enabled, files: files})
	}
	for _, filename := range production {
		if !coveredFiles[filepath.Clean(filename)] {
			return nil, fmt.Errorf("discover ingest build configurations never activated production source %q; its SQLite callable identity cannot be trusted; use satisfiable package build constraints or extend the bounded configuration policy", filename)
		}
	}
	if len(configurations) == 0 {
		return nil, fmt.Errorf("discover ingest build configurations produced no type-checkable production package; restore at least one valid source configuration")
	}
	return configurations, nil
}

func openCodeGoListProductionFiles(directory string, tags []string) ([]string, error) {
	arguments := []string{"list", "-json"}
	if len(tags) != 0 {
		arguments = append(arguments, "-tags="+strings.Join(tags, ","))
	}
	arguments = append(arguments, ".")
	command := exec.Command("go", arguments...)
	command.Dir = directory
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("resolve non-test ingest package files with go list for build tags %v: %w", tags, err)
	}
	var listed struct {
		GoFiles  []string
		CgoFiles []string
	}
	if err := json.Unmarshal(output, &listed); err != nil {
		return nil, fmt.Errorf("decode non-test ingest package file inventory: %w", err)
	}
	filenames := append(append([]string(nil), listed.GoFiles...), listed.CgoFiles...)
	if len(filenames) == 0 {
		return nil, fmt.Errorf("resolve non-test ingest package files in %q failed: go list returned no production files; type-aware SQLite inventory cannot bind local methods", directory)
	}
	for index := range filenames {
		filenames[index] = filepath.Join(directory, filenames[index])
	}
	sort.Strings(filenames)
	return filenames, nil
}

func openCodeCustomBuildTags(filenames []string) ([]string, error) {
	builtIn, err := openCodeBuiltInBuildTags()
	if err != nil {
		return nil, err
	}
	custom := make(map[string]bool)
	for _, filename := range filenames {
		data, readErr := os.ReadFile(filename)
		if readErr != nil {
			return nil, fmt.Errorf("read ingest production source %q while discovering build constraints: %w", filename, readErr)
		}
		for _, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "package ") {
				break
			}
			if !constraint.IsGoBuild(trimmed) {
				continue
			}
			expression, parseErr := constraint.Parse(trimmed)
			if parseErr != nil {
				return nil, fmt.Errorf("parse build constraint in production source %q: %w", filename, parseErr)
			}
			openCodeCollectConstraintTags(expression, custom)
		}
	}
	for tag := range builtIn {
		delete(custom, tag)
	}
	tags := make([]string, 0, len(custom))
	for tag := range custom {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags, nil
}

func openCodeCollectConstraintTags(expression constraint.Expr, tags map[string]bool) {
	switch expression := expression.(type) {
	case *constraint.TagExpr:
		tags[expression.Tag] = true
	case *constraint.NotExpr:
		openCodeCollectConstraintTags(expression.X, tags)
	case *constraint.AndExpr:
		openCodeCollectConstraintTags(expression.X, tags)
		openCodeCollectConstraintTags(expression.Y, tags)
	case *constraint.OrExpr:
		openCodeCollectConstraintTags(expression.X, tags)
		openCodeCollectConstraintTags(expression.Y, tags)
	}
}

func openCodeBuiltInBuildTags() (map[string]bool, error) {
	builtIn := map[string]bool{
		"cgo": true, "gc": true, "gccgo": true, "unix": true,
	}
	for _, tag := range append(append([]string(nil), build.Default.ReleaseTags...), build.Default.ToolTags...) {
		builtIn[tag] = true
	}
	command := exec.Command("go", "tool", "dist", "list")
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("discover standard GOOS/GOARCH build tags with go tool dist list: %w", err)
	}
	for _, pair := range strings.Fields(string(output)) {
		goos, goarch, ok := strings.Cut(pair, "/")
		if ok {
			builtIn[goos] = true
			builtIn[goarch] = true
		}
	}
	return builtIn, nil
}

func TestOpenCodePrivateExecutionGuardRejectsUnresolvedSQL(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "opencode_dynamic_sql.go")
	source := []byte(`package ingest
import (
	"context"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)
type zombiezenOpenCodeSQLiteSource struct{ conn *sqlite.Conn }
func (s *zombiezenOpenCodeSQLiteSource) executeRowsLocked(ctx context.Context, statement string, args []any, result func(*sqlite.Stmt) error) error {
	_, _, err := s.conn.PrepareTransient(statement)
	if err != nil { return err }
	return sqlitex.ExecuteTransient(s.conn, statement, &sqlitex.ExecOptions{Args: args, ResultFunc: result})
}
func forbiddenDynamicSQL(s *zombiezenOpenCodeSQLiteSource, ctx context.Context, statement string) error {
	return s.executeRowsLocked(ctx, statement, nil, nil)
}
`)
	if err := os.WriteFile(filename, source, 0o600); err != nil {
		t.Fatalf("write synthetic dynamic-SQL source: %v", err)
	}
	_, err := extractOpenCodePrivateExecutionStatements([]string{filename}, []string{filename})
	if err == nil || !strings.Contains(err.Error(), "dynamic, formatted, concatenated, or unresolved SQL expression") {
		t.Fatalf("private execution guard dynamic-SQL error = %v, want unresolved-expression rejection", err)
	}
}

func TestOpenCodePrivateExecutionGuardRejectsFixtureOwnedEntryPathBypasses(t *testing.T) {
	fixture := loadOpenCodeCandidateFixture(t)
	for _, mutation := range fixture.EntryPathMutations {
		mutation := mutation
		t.Run(mutation.Name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "sqlite_entry_bypass.go")
			source, err := openCodeEntryPathMutationSource(mutation.Kind)
			if err != nil {
				t.Fatalf("construct fixture-owned SQLite entry-path mutation: %v", err)
			}
			if err := os.WriteFile(filename, []byte(source), 0o600); err != nil {
				t.Fatalf("write fixture-owned SQLite entry-path mutation: %v", err)
			}
			_, err = extractOpenCodePrivateExecutionStatements([]string{filename}, []string{filename})
			if err == nil || !strings.Contains(err.Error(), mutation.ErrorContains) {
				t.Fatalf("SQLite entry-path mutation error = %v, want substring %q", err, mutation.ErrorContains)
			}
		})
	}
}

func openCodeEntryPathMutationSource(kind openCodeEntryPathKind) (string, error) {
	switch kind {
	case openCodeEntrySQLitexExecute:
		return `package ingest
import (
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)
func bypass(conn *sqlite.Conn) error { return sqlitex.Execute(conn, "SELECT * FROM event", nil) }
`, nil
	case openCodeEntryPrepare:
		return `package ingest
import "zombiezen.com/go/sqlite"
func bypass(conn *sqlite.Conn) { _, _ = conn.Prepare("SELECT * FROM event") }
`, nil
	case openCodeEntryPrepareTransient:
		return `package ingest
import "zombiezen.com/go/sqlite"
func bypass(conn *sqlite.Conn) { _, _, _ = conn.PrepareTransient("SELECT * FROM event") }
`, nil
	case openCodeEntryExecuteScript:
		return `package ingest
import (
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)
func bypass(conn *sqlite.Conn) error { return sqlitex.ExecuteScript(conn, "SELECT * FROM event", nil) }
`, nil
	case openCodeEntryStatementStep:
		return `package ingest
import "zombiezen.com/go/sqlite"
func bypass(stmt *sqlite.Stmt) { _, _ = stmt.Step() }
`, nil
	case openCodeEntrySQLitexExec:
		return `package ingest
import (
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)
func bypass(conn *sqlite.Conn) error { return sqlitex.Exec(conn, "SELECT * FROM event", nil) }
`, nil
	case openCodeEntryConnPrep:
		return `package ingest
import "zombiezen.com/go/sqlite"
func bypass(conn *sqlite.Conn) { _ = conn.Prep("SELECT * FROM event") }
`, nil
	case openCodeEntryExecuteFS:
		return `package ingest
import (
	"testing/fstest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)
func bypass(conn *sqlite.Conn) error {
	return sqlitex.ExecuteFS(conn, fstest.MapFS{"query.sql": {Data: []byte("SELECT * FROM event")}}, "query.sql", nil)
}
`, nil
	case openCodeEntryImportAlias:
		return `package ingest
import (
	"zombiezen.com/go/sqlite"
	zx "zombiezen.com/go/sqlite/sqlitex"
)
func bypass(conn *sqlite.Conn) error { return zx.Execute(conn, "SELECT * FROM event", nil) }
`, nil
	case openCodeEntryCallableAlias:
		return `package ingest
import (
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)
func bypass(conn *sqlite.Conn) error {
	execute := sqlitex.Execute
	return execute(conn, "SELECT * FROM event", nil)
}
`, nil
	case openCodeEntryReceiverAlias:
		return `package ingest
import "zombiezen.com/go/sqlite"
func bypass(conn *sqlite.Conn) {
	receiver := conn
	_ = receiver.Prep("SELECT * FROM event")
}
`, nil
	case openCodeEntryCapturedStep:
		return `package ingest
import "zombiezen.com/go/sqlite"
func bypass(stmt *sqlite.Stmt) {
	step := stmt.Step
	_, _ = step()
}
`, nil
	case openCodeEntryHelperExecute:
		return `package ingest
import (
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)
func invokeExecute(fn func(*sqlite.Conn, string, *sqlitex.ExecOptions) error, conn *sqlite.Conn) error {
	return fn(conn, "SELECT * FROM event", nil)
}
func bypass(conn *sqlite.Conn) error { return invokeExecute(sqlitex.Execute, conn) }
`, nil
	case openCodeEntryHelperStep:
		return `package ingest
import "zombiezen.com/go/sqlite"
func invokeStep(fn func() (bool, error)) { _, _ = fn() }
func bypass(stmt *sqlite.Stmt) { invokeStep(stmt.Step) }
`, nil
	case openCodeEntryReturnedExecute:
		return `package ingest
import (
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)
func returnedExecute() func(*sqlite.Conn, string, *sqlitex.ExecOptions) error { return sqlitex.Execute }
func bypass(conn *sqlite.Conn) error { return returnedExecute()(conn, "SELECT * FROM event", nil) }
`, nil
	case openCodeEntryStructField:
		return `package ingest
import (
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)
type executeHolder struct { execute func(*sqlite.Conn, string, *sqlitex.ExecOptions) error }
func bypass(conn *sqlite.Conn) error {
	holder := executeHolder{execute: sqlitex.Execute}
	return holder.execute(conn, "SELECT * FROM event", nil)
}
`, nil
	case openCodeEntryInterfaceField:
		return `package ingest
import (
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)
type interfaceExecuteHolder struct { execute any }
func bypass(conn *sqlite.Conn) error {
	holder := interfaceExecuteHolder{execute: sqlitex.Execute}
	return holder.execute.(func(*sqlite.Conn, string, *sqlitex.ExecOptions) error)(conn, "SELECT * FROM event", nil)
}
`, nil
	case openCodeEntryInterfacePrepare:
		return `package ingest
import "zombiezen.com/go/sqlite"
type transientPreparer interface { PrepareTransient(string) (*sqlite.Stmt, int, error) }
func bypass(conn *sqlite.Conn) {
	var preparer transientPreparer = conn
	_, _, _ = preparer.PrepareTransient("SELECT * FROM event")
}
`, nil
	case openCodeEntryInterfaceStep:
		return `package ingest
import "zombiezen.com/go/sqlite"
type statementStepper interface { Step() (bool, error) }
func bypass(stmt *sqlite.Stmt) {
	var stepper statementStepper = stmt
	_, _ = stepper.Step()
}
`, nil
	case openCodeEntryExecutorHistory, openCodeEntryExecutorPrepare, openCodeEntryPrepareArgument, openCodeEntryExecuteArgument, openCodeEntryExecutorEscape, openCodeEntryExecutorInterface:
		return openCodeExecutorMutationSource(kind), nil
	case openCodeEntryInitializerExtra:
		return `package ingest
import (
	"context"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)
type zombiezenOpenCodeSQLiteSource struct { conn *sqlite.Conn }
func (s *zombiezenOpenCodeSQLiteSource) initialize(context.Context) error {
	if err := sqlitex.ExecuteTransient(s.conn, "PRAGMA query_only=ON", nil); err != nil { return err }
	if err := sqlitex.ExecuteTransient(s.conn, "PRAGMA query_only", nil); err != nil { return err }
	return sqlitex.ExecuteTransient(s.conn, "SELECT id FROM event", nil)
}
`, nil
	case openCodeEntryPackageValue:
		return `package ingest
import "zombiezen.com/go/sqlite/sqlitex"
var escapedOpenCodeExecute = sqlitex.Execute
`, nil
	case openCodeEntryCurrentOpenBlob:
		return `package ingest
import (
	"context"
	"errors"
	"zombiezen.com/go/sqlite"
)
type zombiezenOpenCodeSQLiteSource struct { conn *sqlite.Conn }
type OpenCodeCurrentPageRequest struct{}
type OpenCodeCurrentPage struct{}
func (s *zombiezenOpenCodeSQLiteSource) CurrentMessages(context.Context, OpenCodeCurrentPageRequest) (OpenCodeCurrentPage, error) {
	blob, err := s.conn.OpenBlob("main", "event", "data", 1, false)
	if err != nil { return OpenCodeCurrentPage{}, err }
	buffer := make([]byte, blob.Size())
	_, readErr := blob.Read(buffer)
	return OpenCodeCurrentPage{}, errors.Join(readErr, blob.Close())
}
`, nil
	case openCodeEntryOpenBlobAlias:
		return `package ingest
import "zombiezen.com/go/sqlite"
func bypass(conn *sqlite.Conn) error {
	openBlob := conn.OpenBlob
	_, err := openBlob("main", "event", "data", 1, false)
	return err
}
`, nil
	case openCodeEntrySerialize:
		return `package ingest
import "zombiezen.com/go/sqlite"
func bypass(conn *sqlite.Conn) error { _, err := conn.Serialize("main"); return err }
`, nil
	case openCodeEntrySerializeInterface:
		return `package ingest
import "zombiezen.com/go/sqlite"
type databaseSerializer interface { Serialize(string) ([]byte, error) }
func bypass(conn *sqlite.Conn) error {
	var serializer databaseSerializer = conn
	_, err := serializer.Serialize("main")
	return err
}
`, nil
	case openCodeEntryDeserialize:
		return `package ingest
import "zombiezen.com/go/sqlite"
func bypass(conn *sqlite.Conn, data []byte) error { return conn.Deserialize("main", data) }
`, nil
	case openCodeEntryNewBackup:
		return `package ingest
import "zombiezen.com/go/sqlite"
func bypass(destination, source *sqlite.Conn) error {
	_, err := sqlite.NewBackup(destination, "main", source, "main")
	return err
}
`, nil
	case openCodeEntryBackupStep:
		return `package ingest
import "zombiezen.com/go/sqlite"
func bypass(backup *sqlite.Backup) error { _, err := backup.Step(-1); return err }
`, nil
	case openCodeEntryBackupStepAlias:
		return `package ingest
import "zombiezen.com/go/sqlite"
func invokeBackupStep(step func(int) (bool, error)) error { _, err := step(-1); return err }
func bypass(backup *sqlite.Backup) error { return invokeBackupStep(backup.Step) }
`, nil
	case openCodeEntryBlobRead:
		return `package ingest
import "zombiezen.com/go/sqlite"
func bypass(blob *sqlite.Blob, buffer []byte) error { _, err := blob.Read(buffer); return err }
`, nil
	case openCodeEntryBlobReadAlias:
		return `package ingest
import "zombiezen.com/go/sqlite"
func invokeBlobRead(read func([]byte) (int, error), buffer []byte) error { _, err := read(buffer); return err }
func bypass(blob *sqlite.Blob, buffer []byte) error { return invokeBlobRead(blob.Read, buffer) }
`, nil
	case openCodeEntryBlobWriteTo:
		return `package ingest
import (
	"bytes"
	"zombiezen.com/go/sqlite"
)
func bypass(blob *sqlite.Blob) error { var output bytes.Buffer; _, err := blob.WriteTo(&output); return err }
`, nil
	case openCodeEntrySessionDiff:
		return `package ingest
import "zombiezen.com/go/sqlite"
func bypass(session *sqlite.Session) error { return session.Diff("main", "event") }
`, nil
	case openCodeEntrySessionChangeset:
		return `package ingest
import (
	"bytes"
	"zombiezen.com/go/sqlite"
)
func bypass(session *sqlite.Session) error { var output bytes.Buffer; return session.WriteChangeset(&output) }
`, nil
	case openCodeEntryExecutorPrepareInterface:
		return `package ingest
import (
	"context"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)
type transientExecutorPreparer interface { PrepareTransient(string) (*sqlite.Stmt, int, error) }
type zombiezenOpenCodeSQLiteSource struct { conn *sqlite.Conn }
func (s *zombiezenOpenCodeSQLiteSource) executeRowsLocked(ctx context.Context, statement string, args []any, result func(*sqlite.Stmt) error) error {
	var preparer transientExecutorPreparer = s.conn
	_, _, err := preparer.PrepareTransient(statement)
	if err != nil { return err }
	return sqlitex.ExecuteTransient(s.conn, statement, &sqlitex.ExecOptions{Args: args, ResultFunc: result})
}
`, nil
	default:
		return "", fmt.Errorf("unknown fixture-owned SQLite entry-path mutation %q", kind)
	}
}

func openCodeExecutorMutationSource(kind openCodeEntryPathKind) string {
	prepareStatement := "statement"
	executeStatement := "statement"
	extraParameter := ""
	extra := ""
	extraMethod := ""
	switch kind {
	case openCodeEntryExecutorHistory:
		extra = `
	if err := sqlitex.ExecuteTransient(s.conn, "SELECT id FROM event", nil); err != nil { return err }`
	case openCodeEntryExecutorPrepare:
		extra = `
	if _, _, err := s.conn.PrepareTransient(statement); err != nil { return err }`
	case openCodeEntryPrepareArgument:
		extraParameter = ", alternate string"
		prepareStatement = "alternate"
	case openCodeEntryExecuteArgument:
		executeStatement = `statement + ""`
	case openCodeEntryExecutorEscape:
		extraMethod = `
func (s *zombiezenOpenCodeSQLiteSource) escapeExecutor() any { return s.executeRowsLocked }
`
	case openCodeEntryExecutorInterface:
		extraMethod = `
type boundedOpenCodeExecutor interface {
	executeRowsLocked(context.Context, string, []any, func(*sqlite.Stmt) error) error
}
func dispatchOpenCodeExecutor(s *zombiezenOpenCodeSQLiteSource, ctx context.Context) error {
	var executor boundedOpenCodeExecutor = s
	return executor.executeRowsLocked(ctx, "SELECT id FROM event", nil, nil)
}
`
	}
	return fmt.Sprintf(`package ingest
import (
	"context"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)
type zombiezenOpenCodeSQLiteSource struct { conn *sqlite.Conn }
func (s *zombiezenOpenCodeSQLiteSource) executeRowsLocked(ctx context.Context, statement string%s, args []any, result func(*sqlite.Stmt) error) error {
	_, _, err := s.conn.PrepareTransient(%s)
	if err != nil { return err }%s
	return sqlitex.ExecuteTransient(s.conn, %s, &sqlitex.ExecOptions{Args: args, ResultFunc: result})
}
%s`, extraParameter, prepareStatement, extra, executeStatement, extraMethod)
}

type openCodeTypedSource struct {
	fileSet *token.FileSet
	files   map[string]*ast.File
	info    *types.Info
	pkg     *types.Package
}

type openCodeSQLiteCallableFamily string

const (
	openCodeCallableNone          openCodeSQLiteCallableFamily = ""
	openCodeCallableSQLitex       openCodeSQLiteCallableFamily = "sqlitex"
	openCodeCallableSQLite        openCodeSQLiteCallableFamily = "sqlite"
	openCodeCallableConn          openCodeSQLiteCallableFamily = "sqlite.Conn"
	openCodeCallableStmt          openCodeSQLiteCallableFamily = "sqlite.Stmt"
	openCodeCallableBlob          openCodeSQLiteCallableFamily = "sqlite.Blob"
	openCodeCallableBackup        openCodeSQLiteCallableFamily = "sqlite.Backup"
	openCodeCallableSession       openCodeSQLiteCallableFamily = "sqlite.Session"
	openCodeCallableLocalExecutor openCodeSQLiteCallableFamily = "zombiezenOpenCodeSQLiteSource"
)

type openCodeSQLiteCallableIdentity struct {
	family openCodeSQLiteCallableFamily
	name   string
	exact  bool
}

type openCodeApprovedCallKind string

const (
	openCodeApprovedNone               openCodeApprovedCallKind = ""
	openCodeApprovedExecutorPrepare    openCodeApprovedCallKind = "executor_prepare"
	openCodeApprovedExecutorExecute    openCodeApprovedCallKind = "executor_execute"
	openCodeApprovedInitializerExecute openCodeApprovedCallKind = "initializer_execute"
)

var forbiddenSQLitexCallables = map[string]struct{}{
	"Exec": {}, "ExecFS": {}, "ExecScript": {}, "ExecScriptFS": {},
	"ExecTransient": {}, "ExecTransientFS": {}, "Execute": {}, "ExecuteFS": {},
	"ExecuteScript": {}, "ExecuteScriptFS": {}, "ExecuteTransient": {}, "ExecuteTransientFS": {},
	"PrepareTransientFS": {}, "Save": {}, "Transaction": {}, "ExclusiveTransaction": {},
	"ImmediateTransaction": {}, "InsertRandID": {}, "ResultBool": {}, "ResultBytes": {},
	"ResultFloat": {}, "ResultInt": {}, "ResultInt64": {}, "ResultText": {},
}

// Pinned zombiezen.com/go/sqlite v1.4.2 direct-data inventory. The package
// exposes SQL execution through sqlitex and Conn preparation/Stmt.Step above;
// these additional APIs can access source content without an inventoried SQL
// call: Conn.OpenBlob (table/column/row payload), Conn.Serialize (whole database
// bytes), sqlite.NewBackup plus Backup.Step (database-page copy), and Blob's
// read/copy methods. Session.Diff and WriteChangeset/WritePatchset can compare
// table data or emit captured row values. Deserialize, ApplyChangeset, inverse
// changesets, and Blob write methods are included because they replace or
// mutate source content and must remain outside Contract A.
// Conn/Stmt Column/Get methods are intentionally not listed: they only decode
// the current row of an already-inventoried statement and cannot advance it
// without forbidden Step. Session setup/Attach only configure later tracking,
// so those non-data helpers are intentionally not blocked.
var forbiddenSQLitePackageCallables = map[string]struct{}{
	"NewBackup": {},
}

var forbiddenSQLiteConnCallables = map[string]struct{}{
	"Prep": {}, "Prepare": {}, "PrepareTransient": {},
	"OpenBlob": {}, "Serialize": {}, "Deserialize": {},
	"ApplyChangeset": {}, "ApplyInverseChangeset": {},
}

var forbiddenSQLiteBlobCallables = map[string]struct{}{
	"Read": {}, "WriteTo": {}, "ReadFrom": {}, "Write": {}, "WriteString": {},
}

var forbiddenSQLiteBackupCallables = map[string]struct{}{
	"Step": {},
}

var forbiddenSQLiteSessionCallables = map[string]struct{}{
	"Diff": {}, "WriteChangeset": {}, "WritePatchset": {},
}

func extractOpenCodePrivateExecutionStatements(typeCheckFiles, inventoryFiles []string) ([]string, error) {
	typed, err := typeCheckOpenCodeSources(typeCheckFiles)
	if err != nil {
		return nil, err
	}
	var statements []string
	for _, filename := range inventoryFiles {
		parsed := typed.files[filepath.Clean(filename)]
		if parsed == nil {
			return nil, fmt.Errorf("inventory OpenCode SQLite entry paths failed for %q: the file was not included in the type-checked package; no execution surface can be trusted; include every inventory file in the type-check input", filename)
		}
		if err := rejectPackageLevelOpenCodeSQLiteCallables(parsed, typed); err != nil {
			return nil, err
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			enclosing, _ := typed.info.Defs[function.Name].(*types.Func)
			if enclosing == nil {
				return nil, fmt.Errorf("inventory OpenCode SQLite entry paths failed at %s: function %q has no resolved go/types object; the guard cannot bind approved receiver-qualified methods; fix package type errors before retrying", typed.fileSet.Position(function.Pos()), function.Name.Name)
			}
			enclosingIdentity := openCodeReceiverIdentity(enclosing) + "." + enclosing.Name()
			executorPrepareCount := 0
			executorExecuteCount := 0
			initializerExecuteCount := 0
			parents := make([]ast.Node, 0, 16)
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if node == nil {
					parents = parents[:len(parents)-1]
					return false
				}
				var parent ast.Node
				if len(parents) != 0 {
					parent = parents[len(parents)-1]
				}
				parents = append(parents, node)
				if err != nil {
					return true
				}
				expression, target := openCodeCallableExpression(node, parent, typed.info)
				if target == nil {
					return true
				}
				identity := identifyOpenCodeSQLiteCallable(target, typed.pkg)
				if identity.family == openCodeCallableNone {
					return true
				}
				if openCodeInsideFunctionLiteral(parents[:len(parents)-1]) {
					err = fmt.Errorf("%s: unapproved SQLite callable %s.%s inside a closure nested in %s; exact initializer/executor approvals apply only to their declared method bodies and forbidden callables cannot be transferred through closures", typed.fileSet.Position(expression.Pos()), identity.family, identity.name, enclosingIdentity)
					return true
				}
				call, direct := parent.(*ast.CallExpr)
				if !direct || call.Fun != expression {
					err = fmt.Errorf("%s: unapproved first-class SQLite callable %s.%s in %s; forbidden execute, prepare, transaction, and step callables may appear only as the direct callee of an exact approved call; call the bounded source executor directly instead of passing, returning, storing, converting, closing over, or aliasing this callable", typed.fileSet.Position(expression.Pos()), identity.family, identity.name, enclosingIdentity)
					return true
				}
				statementExpression, includeStatement, approvedKind, entryErr := classifyOpenCodeSQLiteCall(call, identity, enclosing, typed.info)
				if entryErr != nil {
					err = fmt.Errorf("%s: %w", typed.fileSet.Position(call.Pos()), entryErr)
					return true
				}
				switch approvedKind {
				case openCodeApprovedExecutorPrepare:
					executorPrepareCount++
				case openCodeApprovedExecutorExecute:
					executorExecuteCount++
				case openCodeApprovedInitializerExecute:
					initializerExecuteCount++
				}
				if !includeStatement {
					return true
				}
				statement, resolveErr := resolveTypedOpenCodeStatement(statementExpression, typed.info)
				if resolveErr != nil {
					err = fmt.Errorf("%s uses a dynamic, formatted, concatenated, or unresolved SQL expression: %w", typed.fileSet.Position(statementExpression.Pos()), resolveErr)
					return true
				}
				statements = append(statements, normalizeOpenCodeQuery(statement))
				return true
			})
			if err == nil && enclosingIdentity == "zombiezenOpenCodeSQLiteSource.executeRowsLocked" && (executorPrepareCount != 1 || executorExecuteCount != 1) {
				err = fmt.Errorf("%s: exact bounded executor shape has %d direct sqlite.Conn.PrepareTransient and %d direct sqlitex.ExecuteTransient calls, want exactly one of each bound to the exact statement parameter; remove alternate execution paths and restore the single preflight plus execution pair", typed.fileSet.Position(function.Pos()), executorPrepareCount, executorExecuteCount)
			}
			if err == nil && enclosingIdentity == "zombiezenOpenCodeSQLiteSource.initialize" && initializerExecuteCount != 2 {
				err = fmt.Errorf("%s: exact source initializer has %d direct statically inventoried sqlitex.ExecuteTransient calls, want exactly two query_only setup/verification calls; restore the fixed initializer allowlist", typed.fileSet.Position(function.Pos()), initializerExecuteCount)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return statements, nil
}

func rejectPackageLevelOpenCodeSQLiteCallables(file *ast.File, typed openCodeTypedSource) error {
	for _, declaration := range file.Decls {
		if _, function := declaration.(*ast.FuncDecl); function {
			continue
		}
		parents := make([]ast.Node, 0, 8)
		var found error
		ast.Inspect(declaration, func(node ast.Node) bool {
			if node == nil {
				parents = parents[:len(parents)-1]
				return false
			}
			var parent ast.Node
			if len(parents) != 0 {
				parent = parents[len(parents)-1]
			}
			parents = append(parents, node)
			if found != nil {
				return true
			}
			expression, target := openCodeCallableExpression(node, parent, typed.info)
			if target == nil {
				return true
			}
			identity := identifyOpenCodeSQLiteCallable(target, typed.pkg)
			if identity.family != openCodeCallableNone {
				found = fmt.Errorf("%s: unapproved package-level SQLite callable %s.%s; execution and preparation entry points cannot run or escape during package initialization; move database access behind the bounded source methods", typed.fileSet.Position(expression.Pos()), identity.family, identity.name)
			}
			return true
		})
		if found != nil {
			return found
		}
	}
	return nil
}

func openCodeInsideFunctionLiteral(nodes []ast.Node) bool {
	for _, node := range nodes {
		if _, ok := node.(*ast.FuncLit); ok {
			return true
		}
	}
	return false
}

func typeCheckOpenCodeSources(filenames []string) (openCodeTypedSource, error) {
	if len(filenames) == 0 {
		return openCodeTypedSource{}, fmt.Errorf("type-check OpenCode SQLite entry inventory failed: no source files were supplied; execution entry points cannot be resolved; provide the complete production package")
	}
	fileSet := token.NewFileSet()
	files := make(map[string]*ast.File, len(filenames))
	parsed := make([]*ast.File, 0, len(filenames))
	for _, filename := range filenames {
		clean := filepath.Clean(filename)
		file, err := parser.ParseFile(fileSet, clean, nil, parser.AllErrors)
		if err != nil {
			return openCodeTypedSource{}, fmt.Errorf("type-check OpenCode SQLite entry inventory failed while parsing %q: %w; no callable inventory was accepted; fix source syntax before retrying", clean, err)
		}
		files[clean] = file
		parsed = append(parsed, file)
	}
	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Defs:       make(map[*ast.Ident]types.Object),
		Uses:       make(map[*ast.Ident]types.Object),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	config := types.Config{Importer: openCodeModuleImporter(fileSet), Sizes: types.SizesFor("gc", runtime.GOARCH)}
	checked, err := config.Check("github.com/peasant-labs/peasant/internal/ingest", fileSet, parsed, info)
	if err != nil {
		return openCodeTypedSource{}, fmt.Errorf("type-check OpenCode SQLite entry inventory failed: %w; no callable inventory was accepted; ensure all non-test package files and pinned imports type-check", err)
	}
	return openCodeTypedSource{fileSet: fileSet, files: files, info: info, pkg: checked}, nil
}

func openCodeModuleImporter(fileSet *token.FileSet) types.Importer {
	_, currentFile, _, ok := runtime.Caller(0)
	directory := "."
	if ok {
		directory = filepath.Dir(currentFile)
	}
	lookup := func(importPath string) (io.ReadCloser, error) {
		command := exec.Command("go", "list", "-export", "-f={{.Export}}", importPath)
		command.Dir = directory
		output, err := command.Output()
		if err != nil {
			return nil, fmt.Errorf("resolve export data for %q with module-aware go list: %w", importPath, err)
		}
		exportPath := strings.TrimSpace(string(output))
		if exportPath == "" {
			return nil, fmt.Errorf("resolve export data for %q: go list returned an empty export path", importPath)
		}
		file, err := os.Open(exportPath)
		if err != nil {
			return nil, fmt.Errorf("open export data for %q at %q: %w", importPath, exportPath, err)
		}
		return file, nil
	}
	return importer.ForCompiler(fileSet, "gc", lookup)
}

func openCodeCallableExpression(node, parent ast.Node, info *types.Info) (ast.Expr, *types.Func) {
	switch value := node.(type) {
	case *ast.SelectorExpr:
		if selection := info.Selections[value]; selection != nil {
			function, _ := selection.Obj().(*types.Func)
			return value, function
		}
		function, _ := info.Uses[value.Sel].(*types.Func)
		return value, function
	case *ast.Ident:
		if selector, ok := parent.(*ast.SelectorExpr); ok && selector.Sel == value {
			return nil, nil
		}
		function, _ := info.Uses[value].(*types.Func)
		return value, function
	default:
		return nil, nil
	}
}

func identifyOpenCodeSQLiteCallable(target *types.Func, checked *types.Package) openCodeSQLiteCallableIdentity {
	targetPackage := ""
	if target.Pkg() != nil {
		targetPackage = target.Pkg().Path()
	}
	targetReceiver := openCodeReceiverIdentity(target)
	if targetPackage == "github.com/peasant-labs/peasant/internal/ingest" && targetReceiver == "zombiezenOpenCodeSQLiteSource" && target.Name() == "executeRowsLocked" {
		return openCodeSQLiteCallableIdentity{family: openCodeCallableLocalExecutor, name: target.Name(), exact: true}
	}
	if targetPackage == "zombiezen.com/go/sqlite/sqlitex" {
		if _, forbidden := forbiddenSQLitexCallables[target.Name()]; forbidden {
			return openCodeSQLiteCallableIdentity{family: openCodeCallableSQLitex, name: target.Name(), exact: true}
		}
	}
	if targetPackage == "zombiezen.com/go/sqlite" && targetReceiver == "" {
		if _, forbidden := forbiddenSQLitePackageCallables[target.Name()]; forbidden {
			return openCodeSQLiteCallableIdentity{family: openCodeCallableSQLite, name: target.Name(), exact: true}
		}
	}
	if targetPackage == "zombiezen.com/go/sqlite" && targetReceiver == "Conn" {
		if _, forbidden := forbiddenSQLiteConnCallables[target.Name()]; forbidden {
			return openCodeSQLiteCallableIdentity{family: openCodeCallableConn, name: target.Name(), exact: true}
		}
	}
	if targetPackage == "zombiezen.com/go/sqlite" && targetReceiver == "Stmt" && target.Name() == "Step" {
		return openCodeSQLiteCallableIdentity{family: openCodeCallableStmt, name: target.Name(), exact: true}
	}
	if targetPackage == "zombiezen.com/go/sqlite" && targetReceiver == "Blob" {
		if _, forbidden := forbiddenSQLiteBlobCallables[target.Name()]; forbidden {
			return openCodeSQLiteCallableIdentity{family: openCodeCallableBlob, name: target.Name(), exact: true}
		}
	}
	if targetPackage == "zombiezen.com/go/sqlite" && targetReceiver == "Backup" {
		if _, forbidden := forbiddenSQLiteBackupCallables[target.Name()]; forbidden {
			return openCodeSQLiteCallableIdentity{family: openCodeCallableBackup, name: target.Name(), exact: true}
		}
	}
	if targetPackage == "zombiezen.com/go/sqlite" && targetReceiver == "Session" {
		if _, forbidden := forbiddenSQLiteSessionCallables[target.Name()]; forbidden {
			return openCodeSQLiteCallableIdentity{family: openCodeCallableSession, name: target.Name(), exact: true}
		}
	}
	if _, candidate := forbiddenSQLiteConnCallables[target.Name()]; candidate {
		if canonical := importedOpenCodeSQLiteMethod(checked, "Conn", target.Name()); canonical != nil && openCodeCallableSignaturesIdentical(target, canonical) {
			return openCodeSQLiteCallableIdentity{family: openCodeCallableConn, name: target.Name()}
		}
	}
	if target.Name() == "Step" {
		if canonical := importedOpenCodeSQLiteMethod(checked, "Stmt", target.Name()); canonical != nil && openCodeCallableSignaturesIdentical(target, canonical) {
			return openCodeSQLiteCallableIdentity{family: openCodeCallableStmt, name: target.Name()}
		}
		if canonical := importedOpenCodeSQLiteMethod(checked, "Backup", target.Name()); canonical != nil && openCodeCallableSignaturesIdentical(target, canonical) {
			return openCodeSQLiteCallableIdentity{family: openCodeCallableBackup, name: target.Name()}
		}
	}
	if _, candidate := forbiddenSQLiteSessionCallables[target.Name()]; candidate {
		if canonical := importedOpenCodeSQLiteMethod(checked, "Session", target.Name()); canonical != nil && openCodeCallableSignaturesIdentical(target, canonical) {
			return openCodeSQLiteCallableIdentity{family: openCodeCallableSession, name: target.Name()}
		}
	}
	if target.Name() == "executeRowsLocked" {
		if canonical := localOpenCodeExecutorMethod(checked); canonical != nil && openCodeCallableSignaturesIdentical(target, canonical) {
			return openCodeSQLiteCallableIdentity{family: openCodeCallableLocalExecutor, name: target.Name()}
		}
	}
	return openCodeSQLiteCallableIdentity{}
}

func localOpenCodeExecutorMethod(checked *types.Package) *types.Func {
	typeObject, _ := checked.Scope().Lookup("zombiezenOpenCodeSQLiteSource").(*types.TypeName)
	if typeObject == nil {
		return nil
	}
	methodSet := types.NewMethodSet(types.NewPointer(typeObject.Type()))
	for index := 0; index < methodSet.Len(); index++ {
		method, _ := methodSet.At(index).Obj().(*types.Func)
		if method != nil && method.Name() == "executeRowsLocked" {
			return method
		}
	}
	return nil
}

func importedOpenCodeSQLiteMethod(checked *types.Package, typeName, methodName string) *types.Func {
	for _, imported := range checked.Imports() {
		if imported.Path() != "zombiezen.com/go/sqlite" {
			continue
		}
		typeObject, _ := imported.Scope().Lookup(typeName).(*types.TypeName)
		if typeObject == nil {
			return nil
		}
		methodSet := types.NewMethodSet(types.NewPointer(typeObject.Type()))
		for index := 0; index < methodSet.Len(); index++ {
			method, _ := methodSet.At(index).Obj().(*types.Func)
			if method != nil && method.Name() == methodName {
				return method
			}
		}
	}
	return nil
}

func openCodeCallableSignaturesIdentical(left, right *types.Func) bool {
	leftSignature, _ := left.Type().(*types.Signature)
	rightSignature, _ := right.Type().(*types.Signature)
	return leftSignature != nil && rightSignature != nil &&
		leftSignature.Variadic() == rightSignature.Variadic() &&
		types.Identical(leftSignature.Params(), rightSignature.Params()) &&
		types.Identical(leftSignature.Results(), rightSignature.Results())
}

func classifyOpenCodeSQLiteCall(call *ast.CallExpr, identity openCodeSQLiteCallableIdentity, enclosing *types.Func, info *types.Info) (ast.Expr, bool, openCodeApprovedCallKind, error) {
	enclosingIdentity := openCodeReceiverIdentity(enclosing) + "." + enclosing.Name()

	if identity.family == openCodeCallableLocalExecutor {
		if !identity.exact {
			return nil, false, openCodeApprovedNone, fmt.Errorf("unapproved SQLite entry path through an interface-compatible bounded executor in %s; call the exact receiver-qualified zombiezenOpenCodeSQLiteSource.executeRowsLocked method directly so SQL remains statically attributable", enclosingIdentity)
		}
		if len(call.Args) <= 1 {
			return nil, true, openCodeApprovedNone, nil
		}
		return call.Args[1], true, openCodeApprovedNone, nil
	}

	if identity.family == openCodeCallableSQLitex {
		if identity.name == "ExecuteTransient" {
			switch enclosingIdentity {
			case "zombiezenOpenCodeSQLiteSource.initialize":
				if len(call.Args) <= 1 {
					return nil, true, openCodeApprovedInitializerExecute, nil
				}
				return call.Args[1], true, openCodeApprovedInitializerExecute, nil
			case "zombiezenOpenCodeSQLiteSource.executeRowsLocked":
				if err := requireOpenCodeExecutorStatementArgument(call, 1, enclosing, info); err != nil {
					return nil, false, openCodeApprovedNone, err
				}
				return nil, false, openCodeApprovedExecutorExecute, nil
			}
		}
		return nil, false, openCodeApprovedNone, fmt.Errorf("unapproved SQLite entry path sqlitex.%s in %s; only exact direct ExecuteTransient calls in the source initializer and bounded executor are permitted", identity.name, enclosingIdentity)
	}

	if identity.family == openCodeCallableConn {
		if identity.exact && identity.name == "PrepareTransient" && enclosingIdentity == "zombiezenOpenCodeSQLiteSource.executeRowsLocked" {
			if err := requireOpenCodeExecutorStatementArgument(call, 0, enclosing, info); err != nil {
				return nil, false, openCodeApprovedNone, err
			}
			return nil, false, openCodeApprovedExecutorPrepare, nil
		}
		if identity.name == "Prep" || identity.name == "Prepare" || identity.name == "PrepareTransient" {
			return nil, false, openCodeApprovedNone, fmt.Errorf("unapproved SQLite entry path sqlite.Conn.%s in %s; direct preparation is permitted only once for exact PrepareTransient in the exact bounded executor", identity.name, enclosingIdentity)
		}
		return nil, false, openCodeApprovedNone, fmt.Errorf("unapproved SQLite direct-data entry path sqlite.Conn.%s in %s; blob, serialization, and raw database APIs bypass the fixed SQL inventory and cannot access or replace OpenCode-owned source content", identity.name, enclosingIdentity)
	}
	if identity.family == openCodeCallableStmt {
		return nil, false, openCodeApprovedNone, fmt.Errorf("unapproved SQLite entry path sqlite.Stmt.Step in %s; direct, interface, or captured stepping bypasses the bounded row lifecycle", enclosingIdentity)
	}
	if identity.family == openCodeCallableSQLite || identity.family == openCodeCallableBlob || identity.family == openCodeCallableBackup || identity.family == openCodeCallableSession {
		return nil, false, openCodeApprovedNone, fmt.Errorf("unapproved SQLite direct-data entry path %s.%s in %s; blob, serialization, backup, and raw-copy APIs bypass the fixed SQL inventory and cannot access or mutate OpenCode-owned source content", identity.family, identity.name, enclosingIdentity)
	}
	return nil, false, openCodeApprovedNone, nil
}

func requireOpenCodeExecutorStatementArgument(call *ast.CallExpr, argumentIndex int, enclosing *types.Func, info *types.Info) error {
	signature, _ := enclosing.Type().(*types.Signature)
	if signature == nil || signature.Params().Len() <= 1 || argumentIndex >= len(call.Args) {
		return fmt.Errorf("exact bounded executor call does not expose the required SQL argument and statement parameter; restore the direct statement-bound call")
	}
	statementParameter := signature.Params().At(1)
	if statementParameter.Name() != "statement" {
		return fmt.Errorf("exact bounded executor parameter 1 is named %q, want statement; the structural SQL binding guard requires the declared statement parameter without substitution", statementParameter.Name())
	}
	argument, ok := call.Args[argumentIndex].(*ast.Ident)
	if !ok || info.Uses[argument] != statementParameter {
		return fmt.Errorf("exact bounded executor SQL argument %d is not the go/types object for its statement parameter; local constants, alternate parameters, fields, conversions, and expressions can bypass fixed caller SQL attribution; pass statement directly", argumentIndex)
	}
	return nil
}

func openCodeReceiverIdentity(function *types.Func) string {
	signature, _ := function.Type().(*types.Signature)
	if signature == nil || signature.Recv() == nil {
		return ""
	}
	receiver := signature.Recv().Type()
	if pointer, ok := receiver.(*types.Pointer); ok {
		receiver = pointer.Elem()
	}
	named, _ := receiver.(*types.Named)
	if named == nil || named.Obj() == nil {
		return ""
	}
	return named.Obj().Name()
}

func resolveTypedOpenCodeStatement(expression ast.Expr, info *types.Info) (string, error) {
	value := info.Types[expression].Value
	if value == nil || value.Kind() != constant.String {
		return "", fmt.Errorf("expression type %T is not a compile-time string constant", expression)
	}
	return constant.StringVal(value), nil
}

func validateOpenCodePrivateExecutionStatements(statements []string, fixture openCodeCandidateFixture) error {
	if len(statements) != fixture.DeclaredAllowedQueries {
		return fmt.Errorf("OpenCode private execution statement count = %d, want fixture-declared %d", len(statements), fixture.DeclaredAllowedQueries)
	}
	allowed := make(map[string]bool, len(fixture.AllowedQueryStatements))
	for _, query := range fixture.AllowedQueryStatements {
		allowed[query.Statement] = true
	}
	seen := make(map[string]bool, len(statements))
	for _, statement := range statements {
		statement = normalizeOpenCodeQuery(statement)
		if seen[statement] {
			return fmt.Errorf("OpenCode private execution statement %q is duplicated", statement)
		}
		if !allowed[statement] {
			return fmt.Errorf("OpenCode private execution statement %q is not in the strict fixture allowlist", statement)
		}
		if violations := findOpenCodeForbiddenQueryTokens([]string{statement}, fixture.ForbiddenQueryTokens); len(violations) != 0 {
			return fmt.Errorf("OpenCode private execution statement %q contains forbidden tokens %v", statement, violations)
		}
		seen[statement] = true
	}
	for statement := range allowed {
		if !seen[statement] {
			return fmt.Errorf("fixture-allowed OpenCode private execution statement %q is not wired to an execution call", statement)
		}
	}
	return nil
}

func normalizeOpenCodeQuery(statement string) string {
	return strings.ToLower(strings.TrimSpace(statement))
}

func findOpenCodeForbiddenQueryTokens(statements, forbidden []string) []string {
	var violations []string
	for _, statement := range statements {
		normalized := strings.ToLower(statement)
		for _, token := range forbidden {
			if strings.Contains(normalized, strings.ToLower(token)) {
				violations = append(violations, token)
			}
		}
	}
	return violations
}

// openCodeSessionSelectColumns extracts the projected columns from a simple
// single-table SELECT and reports whether it reads FROM the session table. The
// read-only source statements are fixed compile-time constants of the shape
// "select <columns> from <table> ...", so the first " from " delimits the table
// and the projection. recognized is false for a non-SELECT statement such as a
// pragma; targetsSession is true only for the session table, never for
// session_message or any other table.
func openCodeSessionSelectColumns(statement string) (columns []string, targetsSession, recognized bool) {
	normalized := normalizeOpenCodeQuery(statement)
	const selectPrefix = "select "
	if !strings.HasPrefix(normalized, selectPrefix) {
		return nil, false, false
	}
	fromIndex := strings.Index(normalized, " from ")
	if fromIndex < 0 {
		return nil, false, false
	}
	rest := normalized[fromIndex+len(" from "):]
	table := rest
	if boundary := strings.IndexAny(rest, " \t"); boundary >= 0 {
		table = rest[:boundary]
	}
	if table != "session" {
		return nil, false, true
	}
	projection := strings.TrimSpace(normalized[len(selectPrefix):fromIndex])
	for _, part := range strings.Split(projection, ",") {
		columns = append(columns, strings.TrimSpace(part))
	}
	return columns, true, true
}

func TestOpenCodeSessionStatementsReadOnlyAllowlistedColumns(t *testing.T) {
	t.Parallel()
	fixture := loadOpenCodeCandidateFixture(t)
	allowlist := make(map[string]bool, len(fixture.ReadableSessionColumns))
	for _, column := range fixture.ReadableSessionColumns {
		allowlist[column] = true
	}
	sessionStatements := 0
	for _, allowed := range fixture.AllowedQueryStatements {
		columns, targetsSession, recognized := openCodeSessionSelectColumns(allowed.Statement)
		if !recognized || !targetsSession {
			continue
		}
		sessionStatements++
		for _, column := range columns {
			if !allowlist[column] {
				t.Fatalf("allowed session statement %q reads column %q outside the readable allowlist %v", allowed.Name, column, fixture.ReadableSessionColumns)
			}
		}
	}
	if sessionStatements == 0 {
		t.Fatal("no allowed statement reads the session table, so the readable-column allowlist governs nothing")
	}
}

func TestOpenCodeSessionColumnAllowlistRejectsForbiddenColumns(t *testing.T) {
	t.Parallel()
	fixture := loadOpenCodeCandidateFixture(t)
	allowlist := make(map[string]bool, len(fixture.ReadableSessionColumns))
	for _, column := range fixture.ReadableSessionColumns {
		allowlist[column] = true
	}
	for _, mutation := range fixture.SessionColumnMutations {
		t.Run(mutation.Name, func(t *testing.T) {
			columns, targetsSession, recognized := openCodeSessionSelectColumns(mutation.Statement)
			if !recognized || !targetsSession {
				t.Fatalf("session-column mutation %q does not read the session table", mutation.Name)
			}
			var rejected []string
			for _, column := range columns {
				if !allowlist[column] {
					rejected = append(rejected, column)
				}
			}
			if !slices.Contains(rejected, mutation.ForbiddenColumn) {
				t.Fatalf("session-column allowlist accepted forbidden column %q; rejected columns=%v", mutation.ForbiddenColumn, rejected)
			}
		})
	}
}

func TestResolveOpenCodeCandidatesMatchesUpstreamPrecedence(t *testing.T) {
	t.Parallel()
	fixture := loadOpenCodeCandidateFixture(t)
	for _, fixtureCase := range fixture.ResolutionCases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "opencode-data")
			candidates, err := ingest.ResolveOpenCodeCandidates(root, fixtureCase.Channel, syntheticOpenCodeEnvironment(fixtureCase.Environment))
			if err != nil {
				t.Fatalf("resolve OpenCode candidates: %v", err)
			}
			gotPaths := make([]string, 0, len(candidates))
			gotProvenance := make([]ingest.OpenCodeCandidateProvenance, 0, len(candidates))
			for _, candidate := range candidates {
				path := candidate.Path
				if path == root {
					path = "."
				} else if relative, relativeErr := filepath.Rel(root, path); relativeErr == nil && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
					path = filepath.ToSlash(relative)
				}
				gotPaths = append(gotPaths, path)
				gotProvenance = append(gotProvenance, candidate.Provenance)
			}
			if !reflect.DeepEqual(gotPaths, fixtureCase.Paths) {
				t.Errorf("resolved paths = %v, want fixed precedence %v", gotPaths, fixtureCase.Paths)
			}
			if !reflect.DeepEqual(gotProvenance, fixtureCase.Provenance) {
				t.Errorf("resolved provenance = %v, want %v", gotProvenance, fixtureCase.Provenance)
			}
		})
	}
}

type recordingOpenCodeSource struct {
	ingest.OpenCodeSQLiteSource
	catalogCalls *int
}

type indexEvidenceOpenCodeSource struct {
	ingest.OpenCodeSQLiteSource
	index ingest.OpenCodeIndexEvidence
}

func (source indexEvidenceOpenCodeSource) Catalog(ctx context.Context) (ingest.OpenCodeSchemaEvidence, error) {
	evidence, err := source.OpenCodeSQLiteSource.Catalog(ctx)
	if err != nil {
		return evidence, err
	}
	evidence.CurrentIndexes = []ingest.OpenCodeIndexEvidence{source.index}
	return evidence, nil
}

var _ ingest.OpenCodeSQLiteSource = recordingOpenCodeSource{}

func (source recordingOpenCodeSource) Catalog(ctx context.Context) (ingest.OpenCodeSchemaEvidence, error) {
	*source.catalogCalls++
	return source.OpenCodeSQLiteSource.Catalog(ctx)
}

func TestOpenCodeCandidateProbeClassifiesOnlyBoundedCatalogEvidence(t *testing.T) {
	t.Parallel()
	fixture := loadOpenCodeCandidateFixture(t)
	for _, fixtureCase := range fixture.ProbeCases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Fixture, func(t *testing.T) {
			t.Parallel()
			materialized := testfixture.MaterializeByName(t, fixtureCase.Fixture)
			var before testfixture.SourceSnapshot
			var databaseBefore []byte
			var walBefore walContentState
			var walWriter *sqlite.Conn
			if fixtureCase.Fixture == "wal-capable" {
				walWriter = openWALWriter(t, materialized.Path)
				defer closeSQLiteConnection(t, walWriter, "candidate-probe WAL writer")
				appendWALCatalogIndex(t, walWriter, "candidate_probe_pending_idx")
				databaseBefore = readSyntheticFile(t, materialized.Path)
				walBefore = readWALState(t, materialized.Path)
			} else {
				before = testfixture.SnapshotSource(t, materialized)
			}
			catalogCalls := 0
			opener := func(ctx context.Context, path ingest.OpenCodeSQLiteSourcePath, options ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
				source, err := ingest.OpenOpenCodeSQLiteSource(ctx, path, options)
				if err != nil {
					return nil, err
				}
				return recordingOpenCodeSource{OpenCodeSQLiteSource: source, catalogCalls: &catalogCalls}, nil
			}
			prober, err := ingest.NewOpenCodeCandidateProber(&ingest.OSFileSystem{}, opener, ingest.DefaultOpenCodeSQLiteSourceOptions())
			if err != nil {
				t.Fatalf("construct OpenCode candidate prober: %v", err)
			}
			results := prober.Probe(t.Context(), []ingest.OpenCodeCandidate{{
				Path:       materialized.Path,
				Kind:       ingest.OpenCodeSourceSQLite,
				Provenance: ingest.OpenCodeCandidateChannel,
			}})
			if len(results) != 1 {
				t.Fatalf("probe returned %d results, want one candidate-local result", len(results))
			}
			result := results[0]
			if result.Capability != fixtureCase.Capability || result.Support != fixtureCase.Support {
				t.Errorf("probe classification = capability %q support %q, want %q/%q; diagnostics=%+v", result.Capability, result.Support, fixtureCase.Capability, fixtureCase.Support, result.Diagnostics)
			}
			assertOpenCodeDiagnosticsActionable(t, result.Diagnostics)
			if fixtureCase.Diagnostic != "" && !hasOpenCodeDiagnostic(result.Diagnostics, fixtureCase.Diagnostic) {
				t.Errorf("probe diagnostics = %+v, want typed code %q", result.Diagnostics, fixtureCase.Diagnostic)
			}
			if fixtureCase.Diagnostic == ingest.OpenCodeDiagnosticCatalogTruncated {
				retained := retainedOpenCodeEvidenceRows(result.Evidence, fixtureCase.OverflowScope)
				if retained != fixtureCase.RetainedRows {
					t.Errorf("overflow fixture %q retained %d %s rows, want exactly %d", fixtureCase.Fixture, retained, fixtureCase.OverflowScope, fixtureCase.RetainedRows)
				}
			}
			wantCatalogCalls := 1
			if fixtureCase.Fixture == "corrupt-non-sqlite" {
				wantCatalogCalls = 0
			}
			if catalogCalls != wantCatalogCalls {
				t.Errorf("probe catalog calls = %d, want %d typed bounded operations", catalogCalls, wantCatalogCalls)
			}
			if fixtureCase.Fixture != "wal-capable" {
				testfixture.AssertUnchanged(t, materialized, before)
			} else {
				if !hasIndex(result.Evidence.CurrentIndexes, "candidate_probe_pending_idx") {
					t.Errorf("WAL-aware candidate evidence omitted committed WAL-only index: %+v", result.Evidence.CurrentIndexes)
				}
				assertSyntheticFileEqual(t, materialized.Path, databaseBefore, "main database through candidate probe")
				assertWALStateEqual(t, materialized.Path, walBefore)
				appendWALCatalogIndex(t, walWriter, "candidate_probe_nonvacuous_idx")
				walAfterMutation := readWALState(t, materialized.Path)
				if walAfterMutation.frameCount == walBefore.frameCount && bytes.Equal(walAfterMutation.bytes, walBefore.bytes) {
					t.Fatal("WAL logical-identity assertion is vacuous: a committed synthetic catalog mutation was not detected")
				}
			}
		})
	}
}

func TestOpenCodeCandidateProbeRequiresQueryUsableCurrentOrderingEvidence(t *testing.T) {
	fixture := loadOpenCodeCandidateFixture(t)
	for _, fixtureCase := range fixture.IndexEvidenceCases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, "current-session-message")
			keys := make([]ingest.OpenCodeIndexKeyEvidence, len(fixtureCase.Keys))
			for index, key := range fixtureCase.Keys {
				keys[index] = ingest.OpenCodeIndexKeyEvidence{
					Sequence: key.Sequence, ColumnID: key.ColumnID, Name: key.Name,
					Descending: key.Descending, Collation: key.Collation, Key: key.Key,
				}
			}
			indexEvidence := ingest.OpenCodeIndexEvidence{Name: "fixture_ordering_idx", Unique: fixtureCase.Unique, Partial: fixtureCase.Partial, Keys: keys}
			opener := func(ctx context.Context, path ingest.OpenCodeSQLiteSourcePath, options ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
				source, err := ingest.OpenOpenCodeSQLiteSource(ctx, path, options)
				if err != nil {
					return nil, err
				}
				return indexEvidenceOpenCodeSource{OpenCodeSQLiteSource: source, index: indexEvidence}, nil
			}
			prober, err := ingest.NewOpenCodeCandidateProber(&ingest.OSFileSystem{}, opener, ingest.DefaultOpenCodeSQLiteSourceOptions())
			if err != nil {
				t.Fatalf("construct query-usable index evidence prober: %v", err)
			}
			result := prober.Probe(t.Context(), []ingest.OpenCodeCandidate{{Path: materialized.Path, Kind: ingest.OpenCodeSourceSQLite, Provenance: ingest.OpenCodeCandidateChannel}})[0]
			if result.Capability != fixtureCase.ExpectedCapability || result.Support != fixtureCase.ExpectedSupport {
				t.Fatalf("index evidence classification = %q/%q, want %q/%q; evidence=%+v", result.Capability, result.Support, fixtureCase.ExpectedCapability, fixtureCase.ExpectedSupport, result.Evidence.CurrentIndexes)
			}
		})
	}
}

func TestOpenCodeCandidateFailuresRemainLocalAndRejectMemoryEvidence(t *testing.T) {
	t.Parallel()
	fixture := loadOpenCodeCandidateFixture(t)
	root := t.TempDir()
	prober, err := ingest.NewOpenCodeCandidateProber(&ingest.OSFileSystem{}, ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("construct OpenCode candidate prober: %v", err)
	}
	candidates := make([]ingest.OpenCodeCandidate, 0, len(fixture.ContinuationCandidates))
	for _, candidateCase := range fixture.ContinuationCandidates {
		path := ""
		switch candidateCase.PathTemplate {
		case openCodePathMemory:
			path = ":memory:"
		case openCodePathMissing:
			path = filepath.Join(root, "missing.db")
		case openCodePathFixture:
			path = testfixture.MaterializeByName(t, candidateCase.Fixture).Path
		case openCodePathLegacyRoot:
			path = root
		}
		candidate, candidateErr := ingest.NewOpenCodeCandidate(path, candidateCase.Kind, candidateCase.Provenance)
		if candidateErr != nil {
			t.Fatalf("construct continuation candidate %q: %v", candidateCase.Name, candidateErr)
		}
		candidates = append(candidates, candidate)
	}
	results := prober.Probe(t.Context(), candidates)
	if len(results) != len(fixture.ContinuationCandidates) {
		t.Fatalf("probe returned %d results, want one for each of %d fixture candidates", len(results), len(fixture.ContinuationCandidates))
	}
	for index, result := range results {
		want := fixture.ContinuationCandidates[index]
		if result.Support != want.ExpectedSupport || result.Capability != want.ExpectedCapability {
			t.Errorf("continuation result %q = support %q capability %q, want %q/%q", want.Name, result.Support, result.Capability, want.ExpectedSupport, want.ExpectedCapability)
		}
		assertOpenCodeDiagnosticsActionable(t, result.Diagnostics)
	}
}

func TestOpenCodeProductionAdapterDiscoversCurrentOnlySessions(t *testing.T) {
	t.Parallel()
	materialized := testfixture.MaterializeByName(t, "current-session-message")
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic OpenCode data root: %v", err)
	}
	adapter := ingest.NewOpenCodeAdapter(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("run production OpenCode discovery with SQLite evidence: %v", err)
	}
	if len(discovered) != 1 || discovered[0].SessionID != "ses_3cd91f52effeXd3QAJ54jOyzv5" || discovered[0].TranscriptOrigin != ingest.TranscriptOriginOpenCodeCurrentSQLite {
		t.Fatalf("production OpenCode adapter current-only discovery = %+v, want one typed current SQLite session", discovered)
	}
	evidence := adapter.CandidateEvidence()
	if len(evidence) != 2 {
		t.Fatalf("production OpenCode adapter retained %d candidate observations, want channel database and legacy root", len(evidence))
	}
	if evidence[0].Candidate.Path != materialized.Path || evidence[0].Candidate.Kind != ingest.OpenCodeSourceSQLite || evidence[0].Capability != ingest.OpenCodeCapabilityCurrent || evidence[0].Support != ingest.OpenCodeSupportSupported {
		t.Errorf("production SQLite candidate evidence = %+v, want supported current evidence for %q", evidence[0], materialized.Path)
	}
	if evidence[1].Candidate.Kind != ingest.OpenCodeSourceLegacyJSON || evidence[1].Support != ingest.OpenCodeSupportSupported {
		t.Errorf("production legacy-root evidence = %+v, want supported directory evidence", evidence[1])
	}
}

func TestOpenCodeAdapterDiscoveryCapabilities(t *testing.T) {
	t.Parallel()
	fixture := loadOpenCodeCandidateFixture(t)
	for _, fixtureCase := range fixture.AdapterDiscoveryCases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			materialized := testfixture.MaterializeByName(t, fixtureCase.SourceFixture)
			root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
			if err != nil {
				t.Fatalf("resolve synthetic OpenCode root: %v", err)
			}
			filesystem := &ingest.OSFileSystem{}
			environment := fixedCandidateEnvironment{}
			opener := hybridOpenCodeSourceOpener(false, false)
			switch fixtureCase.Mode {
			case openCodeAdapterCapableProbeInitFailure:
				_, err := ingest.NewOpenCodeAdapterWithCandidateProbe(filesystem, testutil.NoGitResolver(), salt.Salt{}, "latest", environment, filesystem, nil, ingest.DefaultOpenCodeSQLiteSourceOptions())
				if err == nil || !strings.Contains(err.Error(), fixtureCase.ErrorContains) {
					t.Fatalf("capable candidate probe construction error = %v, want actionable %q", err, fixtureCase.ErrorContains)
				}
				return
			case openCodeAdapterIncapableLegacyOnly:
				writeLegacyOnlyOpenCodeSession(t, root.String())
				adapter := ingest.NewOpenCodeAdapter(legacyOnlyOpenCodeFileSystem{FileSystem: filesystem}, testutil.NoGitResolver(), salt.Salt{})
				discovered, discoverErr := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
				if discoverErr != nil {
					t.Fatalf("incapable filesystem legacy-only discovery failed: %v", discoverErr)
				}
				if len(discovered) != fixtureCase.ExpectedSessions || len(adapter.CandidateEvidence()) != 0 {
					t.Fatalf("incapable filesystem discovery = %+v evidence=%+v, want %d legacy JSON sessions and no SQLite probe", discovered, adapter.CandidateEvidence(), fixtureCase.ExpectedSessions)
				}
				return
			case openCodeAdapterHybridLegacyFallback:
				opener = hybridOpenCodeSourceOpener(true, false)
			case openCodeAdapterHybridFailure:
				opener = hybridOpenCodeSourceOpener(true, true)
			case openCodeAdapterSQLiteFailureKeepsJSON:
				writeLegacyOnlyOpenCodeSession(t, root.String())
				opener = hybridOpenCodeSourceOpener(true, true)
			}
			adapter, adapterErr := ingest.NewOpenCodeAdapterWithCandidateProbe(filesystem, testutil.NoGitResolver(), salt.Salt{}, "latest", environment, filesystem, opener, ingest.DefaultOpenCodeSQLiteSourceOptions())
			if adapterErr != nil {
				t.Fatalf("construct candidate-capable adapter: %v", adapterErr)
			}
			discovered, discoverErr := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
			if discoverErr != nil {
				t.Fatalf("candidate-capable discovery failed: %v", discoverErr)
			}
			if fixtureCase.Mode == openCodeAdapterHybridFailure || fixtureCase.Mode == openCodeAdapterSQLiteFailureKeepsJSON {
				// A failing supported candidate is skipped and recorded. It never
				// removes the sessions that other candidates discovered.
				assertOpenCodeDiscoveryFailureRecorded(t, adapter.CandidateEvidence(), materialized.Path, fixtureCase.ErrorContains)
				if len(discovered) != fixtureCase.ExpectedSessions {
					t.Fatalf("discovery with a failing SQLite candidate = %+v, want %d sessions from the remaining candidates", discovered, fixtureCase.ExpectedSessions)
				}
				for _, session := range discovered {
					if session.TranscriptOrigin != ingest.TranscriptOriginFile {
						t.Fatalf("discovery with a failing SQLite candidate exposed %+v from the failed candidate", session)
					}
				}
				return
			}
			wantOrigin, originErr := fixtureCase.ExpectedOrigin.transcriptOrigin()
			if originErr != nil {
				t.Fatalf("resolve fixture expected origin: %v", originErr)
			}
			if len(discovered) != fixtureCase.ExpectedSessions || len(discovered) == 0 || discovered[0].TranscriptOrigin != wantOrigin {
				t.Fatalf("hybrid discovery = %+v, want first session with origin %q", discovered, fixtureCase.ExpectedOrigin)
			}
		})
	}
}

type fixedCandidateEnvironment map[string]string

func (environment fixedCandidateEnvironment) LookupEnv(key string) (string, bool) {
	value, ok := environment[key]
	return value, ok
}

type legacyOnlyOpenCodeFileSystem struct{ ingest.FileSystem }

func writeLegacyOnlyOpenCodeSession(t testing.TB, root string) {
	t.Helper()
	path := filepath.Join(root, "storage", "session", "synthetic-project", "ses_3cd91f52effeXd3QAJ54jOyzv5.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create synthetic legacy-only OpenCode session directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"id":"ses_3cd91f52effeXd3QAJ54jOyzv5","directory":"/synthetic/project","time":{"created":1700000000000}}`), 0o600); err != nil {
		t.Fatalf("write synthetic legacy-only OpenCode session: %v", err)
	}
}

type hybridOpenCodeSource struct {
	ingest.OpenCodeSQLiteSource
	failCurrent bool
	failLegacy  bool
}

func hybridOpenCodeSourceOpener(failCurrent, failLegacy bool) ingest.OpenCodeSQLiteSourceOpener {
	return func(ctx context.Context, path ingest.OpenCodeSQLiteSourcePath, options ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
		source, err := ingest.OpenOpenCodeSQLiteSource(ctx, path, options)
		if err != nil {
			return nil, err
		}
		return hybridOpenCodeSource{OpenCodeSQLiteSource: source, failCurrent: failCurrent, failLegacy: failLegacy}, nil
	}
}

func (source hybridOpenCodeSource) CurrentSessionIDs(_ context.Context, request ingest.OpenCodeCurrentSessionPageRequest) (ingest.OpenCodeCurrentSessionPage, error) {
	if source.failCurrent {
		return ingest.OpenCodeCurrentSessionPage{}, errors.New("synthetic current projection unavailable")
	}
	if request.After != nil {
		return ingest.OpenCodeCurrentSessionPage{}, nil
	}
	id, err := ingest.NewOpenCodeCurrentSessionID("ses_3cd91f52effeXd3QAJ54jOyzv5")
	if err != nil {
		return ingest.OpenCodeCurrentSessionPage{}, err
	}
	return ingest.OpenCodeCurrentSessionPage{SessionIDs: []ingest.OpenCodeCurrentSessionID{id}}, nil
}

// SessionRecords keeps the fake session table aligned with the projected
// session id, so the deletion rule that skips a session absent from the session
// table does not drop the mocked session that both projections return.
func (source hybridOpenCodeSource) SessionRecords(_ context.Context, request ingest.OpenCodeSessionRecordPageRequest) (ingest.OpenCodeSessionRecordPage, error) {
	if request.After != nil {
		return ingest.OpenCodeSessionRecordPage{Supported: true, HasParent: true, HasClock: true}, nil
	}
	id, err := ingest.NewOpenCodeSessionLinkID("ses_3cd91f52effeXd3QAJ54jOyzv5")
	if err != nil {
		return ingest.OpenCodeSessionRecordPage{}, err
	}
	return ingest.OpenCodeSessionRecordPage{Supported: true, HasParent: true, HasClock: true, PresentSessionIDs: []ingest.OpenCodeSessionLinkID{id}}, nil
}

func (source hybridOpenCodeSource) LegacySessionIDs(_ context.Context, request ingest.OpenCodeLegacySessionPageRequest) (ingest.OpenCodeLegacySessionPage, error) {
	if source.failLegacy {
		return ingest.OpenCodeLegacySessionPage{}, errors.New("synthetic legacy projection unavailable")
	}
	if request.After != nil {
		return ingest.OpenCodeLegacySessionPage{}, nil
	}
	id, err := ingest.NewOpenCodeLegacySessionID("ses_3cd91f52effeXd3QAJ54jOyzv5")
	if err != nil {
		return ingest.OpenCodeLegacySessionPage{}, err
	}
	return ingest.OpenCodeLegacySessionPage{SessionIDs: []ingest.OpenCodeLegacySessionID{id}}, nil
}

func TestOpenCodeClosedSetsRejectFixtureBackedInvalidValues(t *testing.T) {
	t.Parallel()
	fixture := loadOpenCodeCandidateFixture(t)
	for _, fixtureCase := range fixture.ClosedSetCases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			filesystem := &boundedHeaderFileSystem{info: syntheticRegularFileInfo{}, reader: io.NopCloser(strings.NewReader("unused"))}
			sourceOpenCalls := 0
			prober, err := ingest.NewOpenCodeCandidateProber(filesystem, func(context.Context, ingest.OpenCodeSQLiteSourcePath, ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
				sourceOpenCalls++
				return nil, fmt.Errorf("closed-set validation unexpectedly reached source open")
			}, ingest.DefaultOpenCodeSQLiteSourceOptions())
			if err != nil {
				t.Fatalf("construct closed-set boundary prober: %v", err)
			}
			switch fixtureCase.Field {
			case openCodeClosedSetKind:
				candidate := ingest.OpenCodeCandidate{Path: "/synthetic/opencode.db", Kind: ingest.OpenCodeSourceKind(fixtureCase.Value), Provenance: ingest.OpenCodeCandidateChannel}
				result := prober.Probe(t.Context(), []ingest.OpenCodeCandidate{candidate})[0]
				if result.Support != ingest.OpenCodeSupportUnsupported || !hasOpenCodeDiagnostic(result.Diagnostics, ingest.OpenCodeDiagnosticInvalidCandidate) {
					t.Fatalf("invalid kind probe result = %+v, want typed invalid-candidate diagnostic", result)
				}
				assertOpenCodeDiagnosticsActionable(t, result.Diagnostics)
			case openCodeClosedSetPath:
				candidate := ingest.OpenCodeCandidate{Path: fixtureCase.Value, Kind: ingest.OpenCodeSourceSQLite, Provenance: ingest.OpenCodeCandidateChannel}
				result := prober.Probe(t.Context(), []ingest.OpenCodeCandidate{candidate})[0]
				if result.Support != ingest.OpenCodeSupportUnsupported || len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != ingest.OpenCodeDiagnosticInvalidCandidate {
					t.Fatalf("invalid path probe result = %+v, want one typed invalid-candidate diagnostic", result)
				}
				assertOpenCodeDiagnosticsActionable(t, result.Diagnostics)
				if result.Diagnostics[0].Where != "OpenCode candidate with empty path" {
					t.Errorf("empty-path diagnostic location = %q, want explicit candidate boundary", result.Diagnostics[0].Where)
				}
				if filesystem.statCalls != 0 || filesystem.openCalls != 0 || sourceOpenCalls != 0 {
					t.Fatalf("empty-path validation accessed dependencies: stat=%d file-open=%d source-open=%d", filesystem.statCalls, filesystem.openCalls, sourceOpenCalls)
				}
			case openCodeClosedSetProvenance:
				candidate := ingest.OpenCodeCandidate{Path: "/synthetic/opencode.db", Kind: ingest.OpenCodeSourceSQLite, Provenance: ingest.OpenCodeCandidateProvenance(fixtureCase.Value)}
				result := prober.Probe(t.Context(), []ingest.OpenCodeCandidate{candidate})[0]
				if result.Support != ingest.OpenCodeSupportUnsupported || !hasOpenCodeDiagnostic(result.Diagnostics, ingest.OpenCodeDiagnosticInvalidCandidate) {
					t.Fatalf("invalid provenance probe result = %+v, want typed invalid-candidate diagnostic", result)
				}
				assertOpenCodeDiagnosticsActionable(t, result.Diagnostics)
			case openCodeClosedSetSupport:
				if validateErr := ingest.OpenCodeSchemaSupport(fixtureCase.Value).Validate(); validateErr == nil {
					t.Fatalf("invalid support %q passed closed-set validation", fixtureCase.Value)
				}
			}
		})
	}
}

func hasOpenCodeDiagnostic(diagnostics []ingest.OpenCodeProbeDiagnostic, code ingest.OpenCodeProbeDiagnosticCode) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func retainedOpenCodeEvidenceRows(evidence ingest.OpenCodeSchemaEvidence, scope ingest.OpenCodeCatalogScope) int {
	switch scope {
	case ingest.OpenCodeCatalogTables:
		return len(evidence.Tables)
	case ingest.OpenCodeCatalogColumns:
		return len(evidence.CurrentMessageColumns)
	case ingest.OpenCodeCatalogIndexes:
		rows := 0
		for _, index := range evidence.CurrentIndexes {
			rows += len(index.Keys)
		}
		return rows
	default:
		return 0
	}
}

func assertOpenCodeDiagnosticsActionable(t testing.TB, diagnostics []ingest.OpenCodeProbeDiagnostic) {
	t.Helper()
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "" || diagnostic.Stage == "" || diagnostic.What == "" || diagnostic.Why == "" || diagnostic.Where == "" || diagnostic.When == "" || diagnostic.Meaning == "" || diagnostic.Remediation == "" {
			t.Errorf("candidate diagnostic is not actionable across what/why/where/when/meaning/fix: %+v", diagnostic)
		}
	}
}

func TestOpenCodeCandidateHeaderReadIsBounded(t *testing.T) {
	t.Parallel()
	filesystem := &boundedHeaderFileSystem{info: syntheticRegularFileInfo{}, reader: io.NopCloser(strings.NewReader("SQLite format 3\x00" + strings.Repeat("x", 1024)))}
	prober, err := ingest.NewOpenCodeCandidateProber(filesystem, func(context.Context, ingest.OpenCodeSQLiteSourcePath, ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
		return nil, fmt.Errorf("synthetic open stop")
	}, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("construct bounded-header prober: %v", err)
	}
	result := prober.Probe(t.Context(), []ingest.OpenCodeCandidate{{Path: "/synthetic/opencode.db", Kind: ingest.OpenCodeSourceSQLite, Provenance: ingest.OpenCodeCandidateChannel}})[0]
	if filesystem.bytesRead != len("SQLite format 3\x00") {
		t.Errorf("header probe read %d bytes, want exactly %d", filesystem.bytesRead, len("SQLite format 3\x00"))
	}
	if result.Support != ingest.OpenCodeSupportUnreadable || len(result.Diagnostics) != 1 || result.Diagnostics[0].Stage != ingest.OpenCodeProbeOpen {
		t.Errorf("post-header synthetic open result = %+v, want local open diagnostic", result)
	}
}

type boundedHeaderFileSystem struct {
	info      os.FileInfo
	reader    io.ReadCloser
	bytesRead int
	statCalls int
	openCalls int
}

func (filesystem *boundedHeaderFileSystem) Stat(string) (os.FileInfo, error) {
	filesystem.statCalls++
	return filesystem.info, nil
}
func (filesystem *boundedHeaderFileSystem) Open(string) (io.ReadCloser, error) {
	filesystem.openCalls++
	return &countingReadCloser{ReadCloser: filesystem.reader, count: &filesystem.bytesRead}, nil
}

type countingReadCloser struct {
	io.ReadCloser
	count *int
}

func (reader *countingReadCloser) Read(buffer []byte) (int, error) {
	count, err := reader.ReadCloser.Read(buffer)
	*reader.count += count
	return count, err
}

type syntheticRegularFileInfo struct{}

func (syntheticRegularFileInfo) Name() string       { return "opencode.db" }
func (syntheticRegularFileInfo) Size() int64        { return 1024 }
func (syntheticRegularFileInfo) Mode() os.FileMode  { return 0o600 }
func (syntheticRegularFileInfo) ModTime() time.Time { return time.Time{} }
func (syntheticRegularFileInfo) IsDir() bool        { return false }
func (syntheticRegularFileInfo) Sys() any           { return nil }

// assertOpenCodeDiscoveryFailureRecorded checks that the failing SQLite
// candidate carries an actionable discovery diagnostic on its evidence.
func assertOpenCodeDiscoveryFailureRecorded(t testing.TB, evidence []ingest.OpenCodeProbeResult, databasePath, errorContains string) {
	t.Helper()
	for _, result := range evidence {
		if result.Candidate.Kind != ingest.OpenCodeSourceSQLite || filepath.Clean(result.Candidate.Path) != filepath.Clean(databasePath) {
			continue
		}
		for _, diagnostic := range result.Diagnostics {
			if diagnostic.Code == ingest.OpenCodeDiagnosticDiscoveryFailed && diagnostic.Stage == ingest.OpenCodeProbeDiscover && strings.Contains(diagnostic.Why, errorContains) && diagnostic.What != "" && diagnostic.Where == databasePath && diagnostic.When != "" && diagnostic.Meaning != "" && diagnostic.Remediation != "" {
				return
			}
		}
		t.Fatalf("failing SQLite candidate %q has no actionable discovery diagnostic containing %q: %+v", databasePath, errorContains, result.Diagnostics)
	}
	t.Fatalf("failing SQLite candidate %q is absent from discovery evidence: %+v", databasePath, evidence)
}
