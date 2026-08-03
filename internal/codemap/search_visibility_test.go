package codemap_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/codegraph"
	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"github.com/peasant-labs/schema/testcase"
	testassert "github.com/peasant-labs/schema/testcase/assert"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/search_visibility.yaml
var searchVisibilityYAML []byte

//go:embed testdata/search_visibility_manifest.yaml
var searchVisibilityManifestYAML []byte

type searchVisibilityInput struct {
	Query     string                 `yaml:"query"`
	Limit     int                    `yaml:"limit"`
	Selection config.SelectionConfig `yaml:"selection"`
	Sessions  []searchSessionFixture `yaml:"sessions"`
}

type searchSessionFixture struct {
	ID          string `yaml:"id"`
	Content     string `yaml:"content"`
	GitRemote   string `yaml:"git_remote"`
	ProjectName string `yaml:"project_name"`
	GitBranch   string `yaml:"git_branch"`
}

type searchVisibilityExpected struct {
	SessionIDs     []string `yaml:"session_ids"`
	ResultsNonNull bool     `yaml:"results_non_null"`
	ErrorContains  string   `yaml:"error_contains"`
}

func decodeSearchVisibilityCorpus(data []byte) (testcase.Corpus[searchVisibilityInput, searchVisibilityExpected], error) {
	manifest, err := testutil.DecodeSemanticManifest(searchVisibilityManifestYAML, "search visibility")
	if err != nil {
		return testcase.Corpus[searchVisibilityInput, searchVisibilityExpected]{}, err
	}
	var corpus testcase.Corpus[searchVisibilityInput, searchVisibilityExpected]
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&corpus); err != nil {
		return testcase.Corpus[searchVisibilityInput, searchVisibilityExpected]{}, fmt.Errorf("decode search visibility fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return testcase.Corpus[searchVisibilityInput, searchVisibilityExpected]{}, fmt.Errorf("search visibility fixture must contain exactly one YAML document: %v", err)
	}
	names := make(map[string]struct{}, len(corpus.Cases))
	actualNames := make([]string, 0, len(corpus.Cases))
	for _, tc := range corpus.Cases {
		if _, duplicate := names[tc.Name]; duplicate {
			return testcase.Corpus[searchVisibilityInput, searchVisibilityExpected]{}, fmt.Errorf("search visibility fixture repeats case name %q", tc.Name)
		}
		names[tc.Name] = struct{}{}
		actualNames = append(actualNames, tc.Name)
		if tc.Input.Query == "" || tc.Input.Limit <= 0 || len(tc.Input.Sessions) == 0 {
			return testcase.Corpus[searchVisibilityInput, searchVisibilityExpected]{}, fmt.Errorf("search visibility fixture case %q has incomplete input", tc.Name)
		}
		if (tc.Classification == testcase.MustFail) != (tc.Expected.ErrorContains != "") {
			return testcase.Corpus[searchVisibilityInput, searchVisibilityExpected]{}, fmt.Errorf("search visibility fixture case %q must pair must-fail with error_contains", tc.Name)
		}
	}
	if err := testutil.ValidateSemanticNames(manifest, actualNames, "search visibility"); err != nil {
		return testcase.Corpus[searchVisibilityInput, searchVisibilityExpected]{}, err
	}
	return corpus, nil
}

func loadSearchVisibilityCorpus(t *testing.T) testcase.Corpus[searchVisibilityInput, searchVisibilityExpected] {
	t.Helper()
	corpus, err := decodeSearchVisibilityCorpus(searchVisibilityYAML)
	if err != nil {
		t.Fatalf("load search visibility fixture: %v", err)
	}
	manifest, err := testutil.DecodeSemanticManifest(searchVisibilityManifestYAML, "search visibility")
	if err != nil {
		t.Fatal(err)
	}
	testassert.RequireMin(t, corpus, manifest.ExpectedCaseCount)
	testassert.RequireValid(t, corpus)
	return corpus
}

func TestSearchVisibilityFixtureGuards(t *testing.T) {
	corpus := loadSearchVisibilityCorpus(t)
	manifest, err := testutil.DecodeSemanticManifest(searchVisibilityManifestYAML, "search visibility")
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.CheckMin(manifest.ExpectedCaseCount + 1); err == nil {
		t.Fatal("search visibility CheckMin negative control did not fire")
	}
	mutated := corpus
	mutated.Cases = append([]testcase.Case[searchVisibilityInput, searchVisibilityExpected](nil), corpus.Cases...)
	mutated.Cases[0].Mutation.Description = ""
	if err := mutated.Validate(); err == nil {
		t.Fatal("search visibility non-vacuity mutation unexpectedly validated")
	}

	unknown := bytes.Replace(searchVisibilityYAML, []byte("query: needle"), []byte("query: needle\n      unexpected: true"), 1)
	if _, err := decodeSearchVisibilityCorpus(unknown); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("unknown-field mutation error = %v, want strict rejection", err)
	}
	trailing := append(append([]byte{}, searchVisibilityYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := decodeSearchVisibilityCorpus(trailing); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing-document mutation error = %v, want strict rejection", err)
	}
	unknownManifest := bytes.Replace(searchVisibilityManifestYAML, []byte("expectedCaseCount:"), []byte("unexpected: true\nexpectedCaseCount:"), 1)
	if _, err := testutil.DecodeSemanticManifest(unknownManifest, "search visibility"); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("manifest unknown-field mutation error = %v, want strict rejection", err)
	}
	trailingManifest := append(append([]byte{}, searchVisibilityManifestYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := testutil.DecodeSemanticManifest(trailingManifest, "search visibility"); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("manifest trailing-document mutation error = %v, want strict rejection", err)
	}
	for _, family := range manifest.RequiredNames {
		mutatedBytes := bytes.Replace(searchVisibilityYAML, []byte("name: "+family), []byte("name: replacement_family"), 1)
		if _, err := decodeSearchVisibilityCorpus(mutatedBytes); err == nil || !strings.Contains(err.Error(), "missing required family") {
			t.Fatalf("family %q replacement error = %v, want count-preserving mutation rejection", family, err)
		}
	}
	if err := testutil.ValidateSemanticNames(manifest, manifest.RequiredNames[1:], "search visibility"); err == nil {
		t.Fatal("manifest deletion mutation unexpectedly validated")
	}
}

func TestSearch_AppliesVisibilityBeforeSemanticLimit(t *testing.T) {
	corpus := loadSearchVisibilityCorpus(t)
	for _, tc := range corpus.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			s := storetest.Open(t)
			for i, session := range tc.Input.Sessions {
				seedSearchVisibilitySession(t, s, session, int64(i+1))
			}
			policy, err := sessionvisibility.New(tc.Input.Selection)
			if err != nil {
				t.Fatalf("selection policy: %v", err)
			}
			svc := codemap.NewService(s, func(string) gitops.Repository { return noRepo() }, codegraph.NewGraphBuilder(), policy)
			payload, err := svc.Search(context.Background(), tc.Input.Query, tc.Input.Limit)
			if tc.Classification == testcase.MustFail {
				if err == nil || !sessionvisibility.IsError(err) || !strings.Contains(err.Error(), tc.Expected.ErrorContains) {
					t.Fatalf("Search error = %v, want visibility error containing %q", err, tc.Expected.ErrorContains)
				}
				if payload != nil {
					t.Fatalf("Search returned partial payload on visibility error: %+v", payload)
				}
				return
			}
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if tc.Expected.ResultsNonNull && payload.Results == nil {
				t.Fatal("Search Results is nil, want a non-null empty array when no visible rows remain")
			}
			gotIDs := make([]string, len(payload.Results))
			for i, result := range payload.Results {
				gotIDs[i] = result.SessionID
				if i > 0 && result.Score > payload.Results[i-1].Score {
					t.Fatalf("visible results lost global rank order: score[%d]=%v > score[%d]=%v", i, result.Score, i-1, payload.Results[i-1].Score)
				}
			}
			if !reflect.DeepEqual(gotIDs, tc.Expected.SessionIDs) {
				t.Fatalf("Search session IDs = %v, want %v", gotIDs, tc.Expected.SessionIDs)
			}
		})
	}
}

func seedSearchVisibilitySession(t *testing.T, s *store.Store, fixture searchSessionFixture, sequence int64) {
	t.Helper()
	projectName := fixture.ProjectName
	if projectName == "" {
		projectName = "repo"
	}
	startMs := fxBase() + sequence*1000
	endMs := startMs + 500
	ingested := endMs + 1
	metadata := &schema.UnifiedMetadata{
		SessionID:    schema.SessionID(fixture.ID),
		ModelHarness: defaults.HarnessClaudeCode,
		Model:        testutil.TestModel,
		HostSlug:     schema.HostSlug(testutil.TestHostSlug),
		Project: schema.ProjectContext{
			Hash:     schema.ProjectHash(fxProjectHash),
			Name:     projectName,
			FilePath: fxCwd,
		},
		Timestamp: schema.TimestampInfo{Start: startMs, End: endMs, Ingested: &ingested},
		Source:    schema.SourceInfo{FilePath: "/search.jsonl", Format: schema.SourceFormatJSONL},
	}
	if fixture.GitRemote != "" {
		metadata.Git.Remote = &fixture.GitRemote
	}
	if fixture.GitBranch != "" {
		metadata.Git.Branch = &fixture.GitBranch
	}
	if err := s.InsertSessions(context.Background(), []ingest.StoreEntry{{Metadata: metadata}}); err != nil {
		t.Fatalf("seed search session %s: %v", fixture.ID, err)
	}
	seedEntries(t, s, fixture.ID, []entrySpec{userTurn(startMs, fixture.Content)})
}
