package codemap_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/codegraph"
	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"github.com/peasant-labs/schema/testcase"
	testassert "github.com/peasant-labs/schema/testcase/assert"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/project_resolution.yaml
var projectResolutionYAML []byte

//go:embed testdata/project_resolution_manifest.yaml
var projectResolutionManifestYAML []byte

type projectResolutionInput struct {
	Identity  string                     `yaml:"identity"`
	Selection config.SelectionConfig     `yaml:"selection"`
	Projects  []projectResolutionProject `yaml:"projects"`
}

type projectResolutionProject struct {
	Hash      string `yaml:"hash"`
	CWD       string `yaml:"cwd"`
	Remote    string `yaml:"remote,omitempty"`
	SessionID string `yaml:"session_id"`
}

type projectResolutionExpected struct {
	Project       string `yaml:"project"`
	Hash          string `yaml:"hash"`
	ErrorKind     string `yaml:"error_kind"`
	ErrorContains string `yaml:"error_contains"`
}

func decodeProjectResolutionCorpus(data []byte) (testcase.Corpus[projectResolutionInput, projectResolutionExpected], error) {
	manifest, err := testutil.DecodeSemanticManifest(projectResolutionManifestYAML, "project resolution")
	if err != nil {
		return testcase.Corpus[projectResolutionInput, projectResolutionExpected]{}, err
	}
	var corpus testcase.Corpus[projectResolutionInput, projectResolutionExpected]
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&corpus); err != nil {
		return testcase.Corpus[projectResolutionInput, projectResolutionExpected]{}, fmt.Errorf("decode project resolution fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return testcase.Corpus[projectResolutionInput, projectResolutionExpected]{}, fmt.Errorf("project resolution fixture must contain exactly one YAML document: %v", err)
	}
	names := make(map[string]struct{}, len(corpus.Cases))
	actualNames := make([]string, 0, len(corpus.Cases))
	for _, fixtureCase := range corpus.Cases {
		if _, duplicate := names[fixtureCase.Name]; duplicate {
			return testcase.Corpus[projectResolutionInput, projectResolutionExpected]{}, fmt.Errorf("project resolution fixture repeats case name %q", fixtureCase.Name)
		}
		names[fixtureCase.Name] = struct{}{}
		actualNames = append(actualNames, fixtureCase.Name)
		if fixtureCase.Input.Identity == "" || len(fixtureCase.Input.Projects) == 0 {
			return testcase.Corpus[projectResolutionInput, projectResolutionExpected]{}, fmt.Errorf("project resolution fixture case %q has incomplete input", fixtureCase.Name)
		}
		if (fixtureCase.Classification == testcase.MustFail) != (fixtureCase.Expected.ErrorKind != "" && fixtureCase.Expected.ErrorContains != "") {
			return testcase.Corpus[projectResolutionInput, projectResolutionExpected]{}, fmt.Errorf("project resolution fixture case %q must pair must-fail with error_kind and error_contains", fixtureCase.Name)
		}
	}
	if err := testutil.ValidateSemanticNames(manifest, actualNames, "project resolution"); err != nil {
		return testcase.Corpus[projectResolutionInput, projectResolutionExpected]{}, err
	}
	return corpus, nil
}

func loadProjectResolutionCorpus(t *testing.T) testcase.Corpus[projectResolutionInput, projectResolutionExpected] {
	t.Helper()
	corpus, err := decodeProjectResolutionCorpus(projectResolutionYAML)
	if err != nil {
		t.Fatalf("load project resolution fixture: %v", err)
	}
	manifest, err := testutil.DecodeSemanticManifest(projectResolutionManifestYAML, "project resolution")
	if err != nil {
		t.Fatal(err)
	}
	testassert.RequireMin(t, corpus, manifest.ExpectedCaseCount)
	testassert.RequireValid(t, corpus)
	return corpus
}

func TestProjectResolutionFixtureGuards(t *testing.T) {
	corpus := loadProjectResolutionCorpus(t)
	manifest, err := testutil.DecodeSemanticManifest(projectResolutionManifestYAML, "project resolution")
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.CheckMin(manifest.ExpectedCaseCount + 1); err == nil {
		t.Fatal("project resolution CheckMin negative control did not fire")
	}
	mutated := corpus
	mutated.Cases = append([]testcase.Case[projectResolutionInput, projectResolutionExpected](nil), corpus.Cases...)
	mutated.Cases[0].Mutation.Description = ""
	if err := mutated.Validate(); err == nil {
		t.Fatal("project resolution non-vacuity mutation unexpectedly validated")
	}
	unknown := bytes.Replace(projectResolutionYAML, []byte("identity: /work/legacy label"), []byte("identity: /work/legacy label\n      unexpected: true"), 1)
	if _, err := decodeProjectResolutionCorpus(unknown); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("unknown-field mutation error = %v, want strict rejection", err)
	}
	trailing := append(append([]byte{}, projectResolutionYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := decodeProjectResolutionCorpus(trailing); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing-document mutation error = %v, want strict rejection", err)
	}
	unknownManifest := bytes.Replace(projectResolutionManifestYAML, []byte("expectedCaseCount:"), []byte("unexpected: true\nexpectedCaseCount:"), 1)
	if _, err := testutil.DecodeSemanticManifest(unknownManifest, "project resolution"); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("manifest unknown-field mutation error = %v, want strict rejection", err)
	}
	trailingManifest := append(append([]byte{}, projectResolutionManifestYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := testutil.DecodeSemanticManifest(trailingManifest, "project resolution"); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("manifest trailing-document mutation error = %v, want strict rejection", err)
	}
	for _, family := range manifest.RequiredNames {
		mutatedBytes := bytes.Replace(projectResolutionYAML, []byte("name: "+family), []byte("name: replacement_family"), 1)
		if _, err := decodeProjectResolutionCorpus(mutatedBytes); err == nil || !strings.Contains(err.Error(), "missing required family") {
			t.Fatalf("family %q replacement error = %v, want count-preserving mutation rejection", family, err)
		}
	}
	if err := testutil.ValidateSemanticNames(manifest, manifest.RequiredNames[1:], "project resolution"); err == nil {
		t.Fatal("manifest deletion mutation unexpectedly validated")
	}
}

func TestResolveProject_CanonicalAndLegacyIdentity(t *testing.T) {
	corpus := loadProjectResolutionCorpus(t)
	for _, fixtureCase := range corpus.Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			database := storetest.Open(t)
			for index, project := range fixtureCase.Input.Projects {
				projectHash, err := schema.NewProjectHash(project.Hash)
				if err != nil {
					t.Fatalf("fixture project hash: %v", err)
				}
				sessionID, err := schema.NewSessionID(project.SessionID)
				if err != nil {
					t.Fatalf("fixture session ID: %v", err)
				}
				startMs := fxBase() + int64(index+1)*1000
				endMs := startMs + 500
				ingestedMs := endMs + 1
				metadata := &schema.UnifiedMetadata{
					SessionID:    sessionID,
					ModelHarness: defaults.HarnessClaudeCode,
					Model:        testutil.TestModel,
					HostSlug:     schema.HostSlug(testutil.TestHostSlug),
					Project:      schema.ProjectContext{Hash: projectHash, Name: "project", FilePath: project.CWD},
					Timestamp:    schema.TimestampInfo{Start: startMs, End: endMs, Ingested: &ingestedMs},
					Source:       schema.SourceInfo{FilePath: "/resolution.jsonl", Format: schema.SourceFormatJSONL},
				}
				if project.Remote != "" {
					remote := project.Remote
					metadata.Git.Remote = &remote
				}
				if err := database.InsertSessions(context.Background(), []ingest.StoreEntry{{Metadata: metadata}}); err != nil {
					t.Fatalf("seed project %s: %v", project.Hash, err)
				}
			}
			visibility, err := sessionvisibility.New(fixtureCase.Input.Selection)
			if err != nil {
				t.Fatalf("selection policy: %v", err)
			}
			service := codemap.NewService(database, func(string) gitops.Repository { return noRepo() }, codegraph.NewGraphBuilder(), visibility)
			payload, err := service.ResolveProject(context.Background(), fixtureCase.Input.Identity)
			if fixtureCase.Classification == testcase.MustFail {
				assertProjectResolutionError(t, err, fixtureCase.Expected)
				if payload != nil {
					t.Fatalf("ResolveProject returned partial payload on failure: %+v", payload)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveProject: %v", err)
			}
			if payload.Project != fixtureCase.Expected.Project || payload.ProjectHash.String() != fixtureCase.Expected.Hash {
				t.Fatalf("ResolveProject payload = %+v, want project %q hash %q", payload, fixtureCase.Expected.Project, fixtureCase.Expected.Hash)
			}
		})
	}
}

func assertProjectResolutionError(t *testing.T, err error, expected projectResolutionExpected) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), expected.ErrorContains) {
		t.Fatalf("ResolveProject error = %v, want %q", err, expected.ErrorContains)
	}
	var target error
	switch expected.ErrorKind {
	case "missing":
		target = codemap.ErrProjectNotFound
	case "ambiguous":
		target = codemap.ErrProjectAmbiguous
	case "malformed":
		target = codemap.ErrProjectIdentityInvalid
	default:
		t.Fatalf("fixture has unknown error kind %q", expected.ErrorKind)
	}
	if !errors.Is(err, target) {
		t.Fatalf("ResolveProject error = %v, want sentinel %v", err, target)
	}
}
