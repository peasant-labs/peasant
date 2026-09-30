package codemap

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema/testcase"
	testassert "github.com/peasant-labs/schema/testcase/assert"
)

//go:embed testdata/search_direct.yaml
var searchDirectYAML []byte

//go:embed testdata/search_direct_manifest.yaml
var searchDirectManifestYAML []byte

type searchDirectInput struct {
	Query string `yaml:"query"`
}

type searchDirectExpected struct {
	Kind string `yaml:"kind"`
}

func decodeSearchDirectCorpus(data []byte) (testcase.Corpus[searchDirectInput, searchDirectExpected], error) {
	corpus, err := testcase.LoadCorpus[searchDirectInput, searchDirectExpected](data)
	if err != nil {
		return testcase.Corpus[searchDirectInput, searchDirectExpected]{}, fmt.Errorf("decode search direct fixture: %w", err)
	}
	for _, tc := range corpus.Cases {
		if tc.Input.Query == "" {
			return testcase.Corpus[searchDirectInput, searchDirectExpected]{}, fmt.Errorf("search direct fixture case %q has empty query", tc.Name)
		}
		switch tc.Expected.Kind {
		case "session", "project", "none":
		default:
			return testcase.Corpus[searchDirectInput, searchDirectExpected]{}, fmt.Errorf("search direct fixture case %q has unknown kind %q, want session, project, or none", tc.Name, tc.Expected.Kind)
		}
	}
	return corpus, nil
}

func loadSearchDirectCorpus(t *testing.T) testcase.Corpus[searchDirectInput, searchDirectExpected] {
	t.Helper()
	corpus, err := decodeSearchDirectCorpus(searchDirectYAML)
	if err != nil {
		t.Fatalf("load search direct fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(searchDirectManifestYAML, "search direct")
	if err != nil {
		t.Fatal(err)
	}
	actualNames := make([]string, 0, len(corpus.Cases))
	for _, tc := range corpus.Cases {
		actualNames = append(actualNames, tc.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, actualNames, "search direct"); err != nil {
		t.Fatal(err)
	}
	testassert.RequireMin(t, corpus, len(manifest.RequiredNames))
	testassert.RequireValid(t, corpus)
	return corpus
}

func TestSearchDirectFixtureGuards(t *testing.T) {
	corpus := loadSearchDirectCorpus(t)
	manifest, err := testutil.DecodeRequiredNamesManifest(searchDirectManifestYAML, "search direct")
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.CheckMin(len(manifest.RequiredNames) + 1); err == nil {
		t.Fatal("search direct CheckMin negative control did not fire")
	}
	mutated := corpus
	mutated.Cases = append([]testcase.Case[searchDirectInput, searchDirectExpected](nil), corpus.Cases...)
	mutated.Cases[0].Mutation.Description = ""
	if err := mutated.Validate(); err == nil {
		t.Fatal("search direct non-vacuity mutation unexpectedly validated")
	}

	unknown := bytes.Replace(searchDirectYAML, []byte("query: ingest pipeline"), []byte("query: ingest pipeline\n      unexpected: true"), 1)
	if _, err := decodeSearchDirectCorpus(unknown); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("unknown-field mutation error = %v, want strict rejection", err)
	}
	trailing := append(append([]byte{}, searchDirectYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := decodeSearchDirectCorpus(trailing); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("trailing-document mutation error = %v, want strict rejection", err)
	}
	unknownManifest := bytes.Replace(searchDirectManifestYAML, []byte("requiredNames:"), []byte("unexpected: true\nrequiredNames:"), 1)
	if _, err := testutil.DecodeRequiredNamesManifest(unknownManifest, "search direct"); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("manifest unknown-field mutation error = %v, want strict rejection", err)
	}
	trailingManifest := append(append([]byte{}, searchDirectManifestYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := testutil.DecodeRequiredNamesManifest(trailingManifest, "search direct"); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("manifest trailing-document mutation error = %v, want strict rejection", err)
	}
	names := make([]string, 0, len(corpus.Cases))
	for _, tc := range corpus.Cases {
		names = append(names, tc.Name)
	}
	replaced := bytes.Replace(searchDirectYAML, []byte("name: "+names[0]), []byte("name: replacement_family"), 1)
	replacedCorpus, err := decodeSearchDirectCorpus(replaced)
	if err != nil {
		t.Fatalf("family replacement fixture failed to decode: %v", err)
	}
	replacedNames := make([]string, 0, len(replacedCorpus.Cases))
	for _, tc := range replacedCorpus.Cases {
		replacedNames = append(replacedNames, tc.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, replacedNames, "search direct"); err == nil || !strings.Contains(err.Error(), "missing required case") {
		t.Fatalf("family replacement error = %v, want missing-required-case rejection", err)
	}
	if err := testutil.ValidateRequiredNames(manifest, names[1:], "search direct"); err == nil {
		t.Fatal("manifest deletion mutation unexpectedly validated")
	}
}

func TestClassifyDirectLookup(t *testing.T) {
	corpus := loadSearchDirectCorpus(t)
	for _, tc := range corpus.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			kind, id, hash := classifyDirectLookup(tc.Input.Query)
			var got string
			switch kind {
			case directLookupSession:
				got = "session"
				if id == "" {
					t.Errorf("classifyDirectLookup(%q) = session with empty id", tc.Input.Query)
				}
				if hash != "" {
					t.Errorf("classifyDirectLookup(%q) = session with non-empty hash %q", tc.Input.Query, hash)
				}
			case directLookupProject:
				got = "project"
				if err := hash.Validate(); err != nil {
					t.Errorf("classifyDirectLookup(%q) = project with invalid hash %q: %v", tc.Input.Query, hash, err)
				}
			case directLookupNone:
				got = "none"
			default:
				t.Fatalf("classifyDirectLookup(%q) = unknown kind %d", tc.Input.Query, kind)
			}
			if got != tc.Expected.Kind {
				t.Errorf("classifyDirectLookup(%q) = %q, want %q", tc.Input.Query, got, tc.Expected.Kind)
			}
		})
	}
}
