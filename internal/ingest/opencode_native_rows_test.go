package ingest

import (
	_ "embed"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/opencode_native_rows.yaml
var openCodeNativeRowsYAML []byte

//go:embed testdata/opencode_beta_rows.yaml
var openCodeBetaRowsYAML []byte

type openCodeNativeRowsFixture struct {
	RequiredNames      []string                            `yaml:"required_names"`
	NormalizationCases []openCodeNativeNormalizationCase   `yaml:"normalization_cases"`
	Cases              []openCodeNativeMaterializationCase `yaml:"cases"`
}

type openCodeNativeMaterializationCase struct {
	Name          string   `yaml:"name"`
	SourceFixture string   `yaml:"source_fixture"`
	SessionID     string   `yaml:"session_id"`
	ExpectedText  []string `yaml:"expected_text"`
	ExpectedModel string   `yaml:"expected_model"`
}

type openCodeNativeNormalizationCase struct {
	Name               string                       `yaml:"name"`
	Rows               []openCodeSemanticCurrentRow `yaml:"rows"`
	ExpectedText       string                       `yaml:"expected_text"`
	ErrorContains      string                       `yaml:"error_contains"`
	ExpectedManaged    []string                     `yaml:"expected_managed"`
	ForbiddenManaged   []string                     `yaml:"forbidden_managed"`
	ExpectedToolOutput *string                      `yaml:"expected_tool_output"`
}

func loadOpenCodeNativeRowsFixtures(t testing.TB, data []byte) openCodeNativeRowsFixture {
	t.Helper()
	var fixture openCodeNativeRowsFixture
	if err := yaml.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	add := func(name string) {
		if names[name] || name == "" {
			t.Fatalf("duplicate or empty native fixture name %q", name)
		}
		names[name] = true
	}
	for _, testCase := range fixture.Cases {
		add(testCase.Name)
		if testCase.SourceFixture == "" || testCase.SessionID == "" || len(testCase.ExpectedText) == 0 || testCase.ExpectedModel == "" {
			t.Fatalf("native materialization fixture %q is incomplete", testCase.Name)
		}
	}
	for _, testCase := range fixture.NormalizationCases {
		add(testCase.Name)
		if len(testCase.Rows) == 0 || (testCase.ExpectedText == "") == (testCase.ErrorContains == "") {
			t.Fatalf("native normalization fixture %q needs rows and exactly one success or failure expectation", testCase.Name)
		}
	}
	for _, required := range fixture.RequiredNames {
		if !names[required] {
			t.Fatalf("required native fixture %q missing", required)
		}
	}
	return fixture
}

type nativeRowsEnvironment map[string]string

func (environment nativeRowsEnvironment) LookupEnv(key string) (string, bool) {
	value, ok := environment[key]
	return value, ok
}

func TestOpenCodeNativeRowsProductionMaterializationAndIndexing(t *testing.T) {
	fixture := loadOpenCodeNativeRowsFixtures(t, openCodeNativeRowsYAML)
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			source := testfixture.MaterializeByName(t, testCase.SourceFixture)
			before := testfixture.SnapshotSource(t, source)
			defer testfixture.AssertUnchanged(t, source, before)
			fs := &OSFileSystem{}
			adapter, err := NewOpenCodeAdapterWithCandidateProbe(fs, semanticNoGit{}, salt.Salt{}, "latest", nativeRowsEnvironment{"OPENCODE_DB": source.Path}, fs, OpenOpenCodeSQLiteSource, DefaultOpenCodeSQLiteSourceOptions())
			if err != nil {
				t.Fatal(err)
			}
			root, err := NewResolvedPath(filepath.Dir(source.Path))
			if err != nil {
				t.Fatal(err)
			}
			sessions, err := adapter.Discover(t.Context(), SourceConfig{Enabled: true, Paths: []ResolvedPath{root}})
			if err != nil || len(sessions) != 1 || string(sessions[0].SessionID) != testCase.SessionID {
				t.Fatalf("native discovery: sessions=%+v error=%v", sessions, err)
			}
			metadata, data, err := adapter.MaterializeTranscript(t.Context(), sessions[0])
			if err != nil {
				t.Fatalf("native materialization: %v", err)
			}
			if string(metadata.Model) != testCase.ExpectedModel {
				t.Errorf("model=%q want %q", metadata.Model, testCase.ExpectedModel)
			}
			indexer := NewOpenCodeIndexer(fs, WithOpenCodeFullDepth(true), WithOpenCodeFullContent(true))
			entries, err := indexer.IndexTranscriptBytes(t.Context(), sessions[0], data)
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range testCase.ExpectedText {
				found := false
				for _, entry := range entries {
					if entry.ContentPreview != nil && strings.Contains(*entry.ContentPreview, text) {
						found = true
					}
					if entry.ToolInput != nil && strings.Contains(*entry.ToolInput, text) {
						found = true
					}
					if entry.ToolOutput != nil && strings.Contains(*entry.ToolOutput, text) {
						found = true
					}
				}
				if !found {
					t.Errorf("native text %q missing from indexed entries: %+v", text, entries)
				}
			}
		})
	}
}

func TestOpenCodeNativeRowsNormalizationBoundaries(t *testing.T) {
	runOpenCodeNativeNormalizationCases(t, loadOpenCodeNativeRowsFixtures(t, openCodeNativeRowsYAML))
}

func TestOpenCodeBetaRowsNormalizationBoundaries(t *testing.T) {
	runOpenCodeNativeNormalizationCases(t, loadOpenCodeNativeRowsFixtures(t, openCodeBetaRowsYAML))
}

func runOpenCodeNativeNormalizationCases(t *testing.T, fixture openCodeNativeRowsFixture) {
	for _, testCase := range fixture.NormalizationCases {
		t.Run(testCase.Name, func(t *testing.T) {
			rows := semanticCurrentRows(t, testCase.Rows)
			pageSize, err := NewOpenCodeCurrentPageSize(32)
			if err != nil {
				t.Fatal(err)
			}
			projection, _, err := readOpenCodeCurrentProjection(t.Context(), semanticNegativeSource{rows: rows}, rows[0].SessionID, pageSize)
			if testCase.ErrorContains != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.ErrorContains) {
					t.Fatalf("error=%v want %q", err, testCase.ErrorContains)
				}
				if len(projection.Messages) != 0 {
					t.Fatal("failed normalization emitted a partial projection")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(projection)
			if err != nil {
				t.Fatal(err)
			}
			for _, marker := range testCase.ExpectedManaged {
				if !strings.Contains(string(data), marker) {
					t.Errorf("managed projection lacks %q", marker)
				}
			}
			for _, marker := range testCase.ForbiddenManaged {
				if strings.Contains(string(data), marker) {
					t.Errorf("managed projection contains forbidden %q", marker)
				}
			}
			id, err := NewSessionID(rows[0].SessionID.String())
			if err != nil {
				t.Fatal(err)
			}
			indexer := NewOpenCodeIndexer(&OSFileSystem{}, WithOpenCodeFullDepth(true), WithOpenCodeFullContent(true))
			entries, err := indexer.IndexTranscriptBytes(t.Context(), DiscoveredSession{SessionID: id, TranscriptOrigin: TranscriptOriginOpenCodeCurrentSQLite}, data)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.ExpectedToolOutput != nil {
				found := false
				for _, entry := range entries {
					if entry.ToolOutput != nil && *entry.ToolOutput == *testCase.ExpectedToolOutput {
						found = true
					}
				}
				if !found {
					t.Errorf("indexed tool output does not equal %q", *testCase.ExpectedToolOutput)
				}
			}
			for _, entry := range entries {
				if entry.ContentPreview != nil && strings.Contains(*entry.ContentPreview, testCase.ExpectedText) {
					return
				}
				if entry.ToolOutput != nil && strings.Contains(*entry.ToolOutput, testCase.ExpectedText) {
					return
				}
				if entry.ToolInput != nil && strings.Contains(*entry.ToolInput, testCase.ExpectedText) {
					return
				}
			}
			t.Fatalf("expected native text %q missing from indexed entries: %+v", testCase.ExpectedText, entries)
		})
	}
}
