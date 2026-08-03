package api_test

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema/testcase"
	testassert "github.com/peasant-labs/schema/testcase/assert"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/search_limit_handler.yaml
var searchLimitHandlerYAML []byte

//go:embed testdata/search_limit_handler_manifest.yaml
var searchLimitHandlerManifestYAML []byte

type searchLimitHandlerInput struct {
	RawQuery string `yaml:"raw_query"`
}

type searchLimitHandlerExpected struct {
	Status         int    `yaml:"status"`
	ProviderLimit  int    `yaml:"provider_limit"`
	ProviderCalled bool   `yaml:"provider_called"`
	ErrorContains  string `yaml:"error_contains"`
}

func decodeSearchLimitHandlerCorpus(data []byte) (testcase.Corpus[searchLimitHandlerInput, searchLimitHandlerExpected], error) {
	manifest, err := testutil.DecodeSemanticManifest(searchLimitHandlerManifestYAML, "search limit handler")
	if err != nil {
		return testcase.Corpus[searchLimitHandlerInput, searchLimitHandlerExpected]{}, err
	}
	var corpus testcase.Corpus[searchLimitHandlerInput, searchLimitHandlerExpected]
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&corpus); err != nil {
		return testcase.Corpus[searchLimitHandlerInput, searchLimitHandlerExpected]{}, fmt.Errorf("decode search limit handler fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return testcase.Corpus[searchLimitHandlerInput, searchLimitHandlerExpected]{}, fmt.Errorf("search limit handler fixture must contain exactly one YAML document: %v", err)
	}
	names := make(map[string]struct{}, len(corpus.Cases))
	actualNames := make([]string, 0, len(corpus.Cases))
	for _, tc := range corpus.Cases {
		if _, duplicate := names[tc.Name]; duplicate {
			return testcase.Corpus[searchLimitHandlerInput, searchLimitHandlerExpected]{}, fmt.Errorf("search limit handler fixture repeats case name %q", tc.Name)
		}
		names[tc.Name] = struct{}{}
		actualNames = append(actualNames, tc.Name)
		if tc.Input.RawQuery == "" || tc.Expected.Status == 0 {
			return testcase.Corpus[searchLimitHandlerInput, searchLimitHandlerExpected]{}, fmt.Errorf("search limit handler fixture case %q is incomplete", tc.Name)
		}
		if (tc.Classification == testcase.MustFail) != (tc.Expected.ErrorContains != "") {
			return testcase.Corpus[searchLimitHandlerInput, searchLimitHandlerExpected]{}, fmt.Errorf("search limit handler fixture case %q must pair must-fail with error_contains", tc.Name)
		}
	}
	if err := testutil.ValidateSemanticNames(manifest, actualNames, "search limit handler"); err != nil {
		return testcase.Corpus[searchLimitHandlerInput, searchLimitHandlerExpected]{}, err
	}
	return corpus, nil
}

func loadSearchLimitHandlerCorpus(t *testing.T) testcase.Corpus[searchLimitHandlerInput, searchLimitHandlerExpected] {
	t.Helper()
	corpus, err := decodeSearchLimitHandlerCorpus(searchLimitHandlerYAML)
	if err != nil {
		t.Fatalf("load search limit handler fixture: %v", err)
	}
	manifest, err := testutil.DecodeSemanticManifest(searchLimitHandlerManifestYAML, "search limit handler")
	if err != nil {
		t.Fatal(err)
	}
	testassert.RequireMin(t, corpus, manifest.ExpectedCaseCount)
	testassert.RequireValid(t, corpus)
	return corpus
}

func TestSearchLimitHandlerFixtureGuards(t *testing.T) {
	corpus := loadSearchLimitHandlerCorpus(t)
	manifest, err := testutil.DecodeSemanticManifest(searchLimitHandlerManifestYAML, "search limit handler")
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.CheckMin(manifest.ExpectedCaseCount + 1); err == nil {
		t.Fatal("search limit handler CheckMin negative control did not fire")
	}
	mutated := corpus
	mutated.Cases = append([]testcase.Case[searchLimitHandlerInput, searchLimitHandlerExpected](nil), corpus.Cases...)
	mutated.Cases[0].Mutation.Description = ""
	if err := mutated.Validate(); err == nil {
		t.Fatal("search limit handler non-vacuity mutation unexpectedly validated")
	}
	unknown := bytes.Replace(
		searchLimitHandlerYAML,
		[]byte(`input: {raw_query: "q=pipeline"}`),
		[]byte("input:\n      raw_query: q=pipeline\n      unexpected: true"),
		1,
	)
	if _, err := decodeSearchLimitHandlerCorpus(unknown); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("unknown-field mutation error = %v, want strict rejection", err)
	}
	trailing := append(append([]byte{}, searchLimitHandlerYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := decodeSearchLimitHandlerCorpus(trailing); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing-document mutation error = %v, want strict rejection", err)
	}
	unknownManifest := bytes.Replace(searchLimitHandlerManifestYAML, []byte("expectedCaseCount:"), []byte("unexpected: true\nexpectedCaseCount:"), 1)
	if _, err := testutil.DecodeSemanticManifest(unknownManifest, "search limit handler"); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("manifest unknown-field mutation error = %v, want strict rejection", err)
	}
	trailingManifest := append(append([]byte{}, searchLimitHandlerManifestYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := testutil.DecodeSemanticManifest(trailingManifest, "search limit handler"); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("manifest trailing-document mutation error = %v, want strict rejection", err)
	}
	for _, family := range manifest.RequiredNames {
		mutatedBytes := bytes.Replace(searchLimitHandlerYAML, []byte("name: "+family), []byte("name: replacement_family"), 1)
		if _, err := decodeSearchLimitHandlerCorpus(mutatedBytes); err == nil || !strings.Contains(err.Error(), "missing required family") {
			t.Fatalf("family %q replacement error = %v, want count-preserving mutation rejection", family, err)
		}
	}
	if err := testutil.ValidateSemanticNames(manifest, manifest.RequiredNames[1:], "search limit handler"); err == nil {
		t.Fatal("manifest deletion mutation unexpectedly validated")
	}
}

func TestSearchHandler_ValidatesPresentLimit(t *testing.T) {
	corpus := loadSearchLimitHandlerCorpus(t)
	for _, tc := range corpus.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			stub := &mapStubProvider{}
			base := startMapTestServer(t, stub)
			status, _, raw := getJSON(t, base+"/api/v1/search?"+tc.Input.RawQuery)
			if status != tc.Expected.Status {
				t.Fatalf("status = %d, want %d (body: %s)", status, tc.Expected.Status, raw)
			}
			called := stub.gotQuery != ""
			if called != tc.Expected.ProviderCalled || stub.gotLimit != tc.Expected.ProviderLimit {
				t.Fatalf("provider called/limit = %t/%d, want %t/%d", called, stub.gotLimit, tc.Expected.ProviderCalled, tc.Expected.ProviderLimit)
			}
			if tc.Expected.ErrorContains != "" && !strings.Contains(string(raw), tc.Expected.ErrorContains) {
				t.Fatalf("body = %s, want error containing %q", raw, tc.Expected.ErrorContains)
			}
		})
	}
}
