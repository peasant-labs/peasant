package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
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
	expectedOpenCodeProbeCases      = 12
	expectedContinuationCandidates  = 4
	expectedClosedSetCases          = 7
	expectedAllowedQueryStatements  = 5
)

//go:embed testdata/opencode_candidates.yaml
var openCodeCandidateFixtureYAML []byte

type openCodeCandidateFixture struct {
	DeclaredResolutionCases int                               `yaml:"declared_resolution_cases"`
	ResolutionCases         []openCodeCandidateResolutionCase `yaml:"resolution_cases"`
	DeclaredProbeCases      int                               `yaml:"declared_probe_cases"`
	ForbiddenQueryTokens    []string                          `yaml:"forbidden_query_tokens"`
	DeclaredAllowedQueries  int                               `yaml:"declared_allowed_query_statements"`
	AllowedQueryStatements  []openCodeAllowedQueryStatement   `yaml:"allowed_query_statements"`
	QueryGuardMutation      openCodeQueryGuardMutation        `yaml:"query_guard_mutation"`
	ProbeCases              []openCodeProbeCase               `yaml:"probe_cases"`
	DeclaredContinuation    int                               `yaml:"declared_continuation_candidates"`
	ContinuationCandidates  []openCodeContinuationCandidate   `yaml:"continuation_candidates"`
	DeclaredClosedSetCases  int                               `yaml:"declared_closed_set_cases"`
	ClosedSetCases          []openCodeClosedSetCase           `yaml:"closed_set_cases"`
}

type openCodeAllowedQueryStatement struct {
	Name      string `yaml:"name"`
	Statement string `yaml:"statement"`
}

type openCodeQueryGuardMutation struct {
	ReplaceStatement     string `yaml:"replace_statement"`
	ReplacementStatement string `yaml:"replacement_statement"`
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
	fixture.QueryGuardMutation.ReplaceStatement = normalizeOpenCodeQuery(fixture.QueryGuardMutation.ReplaceStatement)
	fixture.QueryGuardMutation.ReplacementStatement = normalizeOpenCodeQuery(fixture.QueryGuardMutation.ReplacementStatement)
	if !seenQueryStatements[fixture.QueryGuardMutation.ReplaceStatement] || fixture.QueryGuardMutation.ReplacementStatement == "" || seenQueryStatements[fixture.QueryGuardMutation.ReplacementStatement] {
		t.Fatalf("OpenCode query-guard mutation must replace one allowed statement with one disallowed statement: %+v", fixture.QueryGuardMutation)
	}
	if violations := findOpenCodeForbiddenQueryTokens([]string{fixture.QueryGuardMutation.ReplacementStatement}, fixture.ForbiddenQueryTokens); len(violations) == 0 {
		t.Fatalf("OpenCode query-guard mutation replacement must exercise the forbidden-token boundary: %+v", fixture.QueryGuardMutation)
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

func TestOpenCodePrivateExecutionStatementsMatchFixtureAllowlist(t *testing.T) {
	t.Parallel()
	fixture := loadOpenCodeCandidateFixture(t)
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve OpenCode candidate query guard location")
	}
	directory := filepath.Dir(currentFile)
	production := []string{
		filepath.Join(directory, "opencode_candidates.go"),
		filepath.Join(directory, "opencode_sqlite_source_zombiezen.go"),
	}
	statements, err := extractOpenCodePrivateExecutionStatements(production)
	if err != nil {
		t.Fatalf("resolve private OpenCode SQLite execution statements: %v", err)
	}
	if err := validateOpenCodePrivateExecutionStatements(statements, fixture); err != nil {
		t.Fatal(err)
	}

	mutated := append([]string(nil), statements...)
	replaced := 0
	for index, statement := range mutated {
		if statement == fixture.QueryGuardMutation.ReplaceStatement {
			mutated[index] = fixture.QueryGuardMutation.ReplacementStatement
			replaced++
		}
	}
	if replaced != 1 {
		t.Fatalf("OpenCode query-guard mutation replaced %d actual execution statements, want exactly one", replaced)
	}
	if err := validateOpenCodePrivateExecutionStatements(mutated, fixture); err == nil {
		t.Fatal("OpenCode private execution guard accepted a fixture-owned payload-query mutation")
	}
}

func extractOpenCodePrivateExecutionStatements(filenames []string) ([]string, error) {
	fileSet := token.NewFileSet()
	var statements []string
	for _, filename := range filenames {
		parsed, err := parser.ParseFile(fileSet, filename, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse production source %q: %w", filename, err)
		}
		constants, err := staticOpenCodeStringConstants(parsed)
		if err != nil {
			return nil, fmt.Errorf("resolve production constants in %q: %w", filename, err)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if err != nil {
					return false
				}
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				statementExpression, executionCall := openCodePrivateStatementExpression(call, function.Name.Name)
				if !executionCall {
					return true
				}
				statement, resolveErr := resolveStaticOpenCodeStatement(statementExpression, constants)
				if resolveErr != nil {
					position := fileSet.Position(call.Pos())
					if statementExpression != nil {
						position = fileSet.Position(statementExpression.Pos())
					}
					err = fmt.Errorf("%s uses a dynamic, formatted, concatenated, or unresolved SQL expression: %w", position, resolveErr)
					return false
				}
				statements = append(statements, normalizeOpenCodeQuery(statement))
				return true
			})
		}
		if err != nil {
			return nil, err
		}
	}
	return statements, nil
}

func staticOpenCodeStringConstants(file *ast.File) (map[string]string, error) {
	constants := make(map[string]string)
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, specification := range general.Specs {
			values := specification.(*ast.ValueSpec)
			if len(values.Names) != len(values.Values) {
				continue
			}
			for index, name := range values.Names {
				literal, ok := values.Values[index].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil {
					return nil, fmt.Errorf("unquote constant %s: %w", name.Name, err)
				}
				constants[name.Name] = value
			}
		}
	}
	return constants, nil
}

func openCodePrivateStatementExpression(call *ast.CallExpr, functionName string) (ast.Expr, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	if selector.Sel.Name == "executeRowsLocked" {
		if len(call.Args) <= 1 {
			return nil, true
		}
		return call.Args[1], true
	}
	packageName, packageCall := selector.X.(*ast.Ident)
	if !packageCall || packageName.Name != "sqlitex" || selector.Sel.Name != "ExecuteTransient" || functionName == "executeRowsLocked" {
		return nil, false
	}
	if len(call.Args) <= 1 {
		return nil, true
	}
	return call.Args[1], true
}

func resolveStaticOpenCodeStatement(expression ast.Expr, constants map[string]string) (string, error) {
	switch value := expression.(type) {
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return "", fmt.Errorf("literal kind is %s, not string", value.Kind)
		}
		return strconv.Unquote(value.Value)
	case *ast.Ident:
		statement, ok := constants[value.Name]
		if !ok {
			return "", fmt.Errorf("identifier %q is not a file-scope static string constant", value.Name)
		}
		return statement, nil
	case *ast.ParenExpr:
		return resolveStaticOpenCodeStatement(value.X, constants)
	default:
		return "", fmt.Errorf("expression type is %T", expression)
	}
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

func TestOpenCodeProductionAdapterMountsTypedCandidateEvidenceWithoutSessions(t *testing.T) {
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
	if len(discovered) != 0 {
		t.Fatalf("production OpenCode adapter converted SQLite evidence into %d discovered sessions", len(discovered))
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
			rows += len(index.Columns)
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
