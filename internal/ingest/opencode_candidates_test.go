package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"gopkg.in/yaml.v3"
)

const (
	expectedOpenCodeResolutionCases = 8
	expectedOpenCodeProbeCases      = 9
)

//go:embed testdata/opencode_candidates.yaml
var openCodeCandidateFixtureYAML []byte

type openCodeCandidateFixture struct {
	DeclaredResolutionCases int                               `yaml:"declared_resolution_cases"`
	ResolutionCases         []openCodeCandidateResolutionCase `yaml:"resolution_cases"`
	DeclaredProbeCases      int                               `yaml:"declared_probe_cases"`
	ForbiddenQueryTokens    []string                          `yaml:"forbidden_query_tokens"`
	ProbeCases              []openCodeProbeCase               `yaml:"probe_cases"`
}

type openCodeCandidateResolutionCase struct {
	Name        string            `yaml:"name"`
	Channel     string            `yaml:"channel"`
	Environment map[string]string `yaml:"environment"`
	Paths       []string          `yaml:"paths"`
	Provenance  []string          `yaml:"provenance"`
}

type openCodeProbeCase struct {
	Fixture    string `yaml:"fixture"`
	Capability string `yaml:"capability"`
	Support    string `yaml:"support"`
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
	if len(fixture.ForbiddenQueryTokens) == 0 {
		t.Fatal("OpenCode probe fixture must declare forbidden query-shape tokens")
	}
	seenTokens := make(map[string]bool, len(fixture.ForbiddenQueryTokens))
	for _, token := range fixture.ForbiddenQueryTokens {
		if token == "" || seenTokens[token] {
			t.Fatalf("OpenCode probe fixture has an empty or duplicate forbidden query token %q", token)
		}
		seenTokens[token] = true
	}
	seenResolution := make(map[string]bool, len(fixture.ResolutionCases))
	for _, fixtureCase := range fixture.ResolutionCases {
		if strings.TrimSpace(fixtureCase.Name) == "" || strings.TrimSpace(fixtureCase.Channel) == "" || len(fixtureCase.Paths) == 0 || len(fixtureCase.Paths) != len(fixtureCase.Provenance) || seenResolution[fixtureCase.Name] {
			t.Fatalf("OpenCode resolution fixture case is incomplete or duplicated: %+v", fixtureCase)
		}
		seenResolution[fixtureCase.Name] = true
	}
	seenProbe := make(map[string]bool, len(fixture.ProbeCases))
	for _, fixtureCase := range fixture.ProbeCases {
		if strings.TrimSpace(fixtureCase.Fixture) == "" || strings.TrimSpace(fixtureCase.Capability) == "" || strings.TrimSpace(fixtureCase.Support) == "" || seenProbe[fixtureCase.Fixture] {
			t.Fatalf("OpenCode probe fixture case is incomplete or duplicated: %+v", fixtureCase)
		}
		seenProbe[fixtureCase.Fixture] = true
	}
	return fixture
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
			gotProvenance := make([]string, 0, len(candidates))
			for _, candidate := range candidates {
				path := candidate.Path
				if path == root {
					path = "."
				} else if relative, relativeErr := filepath.Rel(root, path); relativeErr == nil && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
					path = filepath.ToSlash(relative)
				}
				gotPaths = append(gotPaths, path)
				gotProvenance = append(gotProvenance, string(candidate.Provenance))
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
	statements *[]string
}

var _ ingest.OpenCodeSQLiteSource = recordingOpenCodeSource{}

func (source recordingOpenCodeSource) Read(ctx context.Context, statement string, args []any, visit func(ingest.OpenCodeSQLiteRow) error) error {
	*source.statements = append(*source.statements, statement)
	return source.OpenCodeSQLiteSource.Read(ctx, statement, args, visit)
}

func TestOpenCodeCandidateProbeClassifiesOnlyBoundedCatalogEvidence(t *testing.T) {
	t.Parallel()
	fixture := loadOpenCodeCandidateFixture(t)
	for _, fixtureCase := range fixture.ProbeCases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Fixture, func(t *testing.T) {
			t.Parallel()
			materialized := testfixture.Materialize(t, testfixture.CaseByName(t, fixtureCase.Fixture))
			before := testfixture.SnapshotSource(t, materialized)
			var statements []string
			opener := func(ctx context.Context, path ingest.OpenCodeSQLiteSourcePath, options ingest.OpenCodeSQLiteSourceOptions) (ingest.OpenCodeSQLiteSource, error) {
				source, err := ingest.OpenOpenCodeSQLiteSource(ctx, path, options)
				if err != nil {
					return nil, err
				}
				return recordingOpenCodeSource{OpenCodeSQLiteSource: source, statements: &statements}, nil
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
			if string(result.Capability) != fixtureCase.Capability || string(result.Support) != fixtureCase.Support {
				t.Errorf("probe classification = capability %q support %q, want %q/%q; diagnostics=%+v", result.Capability, result.Support, fixtureCase.Capability, fixtureCase.Support, result.Diagnostics)
			}
			assertOpenCodeDiagnosticsActionable(t, result.Diagnostics)
			assertOpenCodeProbeStatementsAreCatalogOnly(t, statements, fixture.ForbiddenQueryTokens)
			if fixtureCase.Fixture != "wal-capable" {
				testfixture.AssertUnchanged(t, materialized, before)
			}
		})
	}
}

func TestOpenCodeCandidateFailuresRemainLocalAndRejectMemoryEvidence(t *testing.T) {
	t.Parallel()
	valid := testfixture.Materialize(t, testfixture.CaseByName(t, "legacy-message-part"))
	root := t.TempDir()
	prober, err := ingest.NewOpenCodeCandidateProber(&ingest.OSFileSystem{}, ingest.OpenOpenCodeSQLiteSource, ingest.DefaultOpenCodeSQLiteSourceOptions())
	if err != nil {
		t.Fatalf("construct OpenCode candidate prober: %v", err)
	}
	results := prober.Probe(t.Context(), []ingest.OpenCodeCandidate{
		{Path: ":memory:", Kind: ingest.OpenCodeSourceSQLite, Provenance: ingest.OpenCodeCandidateOverride},
		{Path: filepath.Join(root, "missing.db"), Kind: ingest.OpenCodeSourceSQLite, Provenance: ingest.OpenCodeCandidateChannel},
		{Path: valid.Path, Kind: ingest.OpenCodeSourceSQLite, Provenance: ingest.OpenCodeCandidateChannel},
		{Path: root, Kind: ingest.OpenCodeSourceLegacyJSON, Provenance: ingest.OpenCodeCandidateLegacyJSONRoot},
	})
	if len(results) != 4 {
		t.Fatalf("probe returned %d results, want one per candidate despite failures", len(results))
	}
	if results[0].Support != ingest.OpenCodeSupportUnsupported || results[1].Support != ingest.OpenCodeSupportUnreadable || results[2].Support != ingest.OpenCodeSupportSupported || results[3].Support != ingest.OpenCodeSupportSupported {
		t.Errorf("candidate-local support sequence = [%q %q %q %q], want [unsupported unreadable supported supported]", results[0].Support, results[1].Support, results[2].Support, results[3].Support)
	}
	for _, result := range results {
		assertOpenCodeDiagnosticsActionable(t, result.Diagnostics)
	}
	if results[2].Capability != ingest.OpenCodeCapabilityLegacy {
		t.Errorf("valid candidate after failures capability = %q, want legacy", results[2].Capability)
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

func assertOpenCodeProbeStatementsAreCatalogOnly(t testing.TB, statements, forbidden []string) {
	t.Helper()
	for _, statement := range statements {
		lower := strings.ToLower(" " + strings.Join(strings.Fields(statement), " ") + " ")
		if !strings.Contains(lower, " limit ") {
			t.Errorf("probe statement is not explicitly bounded: %s", statement)
		}
		for _, token := range forbidden {
			if strings.Contains(lower, token) {
				t.Errorf("probe statement reads forbidden payload/history shape %q: %s", token, statement)
			}
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
	result := prober.Probe(t.Context(), []ingest.OpenCodeCandidate{{Path: "/synthetic/opencode.db", Kind: ingest.OpenCodeSourceSQLite}})[0]
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
}

func (filesystem *boundedHeaderFileSystem) Stat(string) (os.FileInfo, error) {
	return filesystem.info, nil
}
func (filesystem *boundedHeaderFileSystem) Open(string) (io.ReadCloser, error) {
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
