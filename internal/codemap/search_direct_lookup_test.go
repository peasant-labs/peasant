package codemap_test

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/codegraph"
	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"github.com/peasant-labs/schema/testcase"
	testassert "github.com/peasant-labs/schema/testcase/assert"
)

//go:embed testdata/search_direct_lookup.yaml
var searchDirectLookupYAML []byte

//go:embed testdata/search_direct_lookup_manifest.yaml
var searchDirectLookupManifestYAML []byte

type directLookupSession struct {
	ID      string `yaml:"id"`
	Content string `yaml:"content"`
}

type directLookupInput struct {
	Query    string                `yaml:"query"`
	Limit    int                   `yaml:"limit"`
	Offset   int                   `yaml:"offset"`
	Sessions []directLookupSession `yaml:"sessions"`
}

type directLookupExpected struct {
	SessionIDs    []string `yaml:"session_ids"`
	ProjectHashes []string `yaml:"project_hashes"`
	EntryIndexes  []int    `yaml:"entry_indexes"`
	Roles         []string `yaml:"roles"`
	// Snippets pins the exact result labels for direct-lookup hits. A
	// direct-lookup hit carries an empty snippet — the marker the command
	// palette (#351) renders as a distinct id/hash row showing the session
	// or project identity instead of a content excerpt — so direct cases
	// expect [""]. It is empty for FTS-fallthrough cases, whose snippet()
	// markers are covered by the existing search tests; when present it must
	// run parallel to the other expected arms.
	Snippets []string `yaml:"snippets"`
}

func decodeSearchDirectLookupCorpus(data []byte) (testcase.Corpus[directLookupInput, directLookupExpected], error) {
	corpus, err := testcase.LoadCorpus[directLookupInput, directLookupExpected](data)
	if err != nil {
		return testcase.Corpus[directLookupInput, directLookupExpected]{}, fmt.Errorf("decode search direct lookup fixture: %w", err)
	}
	for _, tc := range corpus.Cases {
		if tc.Input.Limit <= 0 {
			return testcase.Corpus[directLookupInput, directLookupExpected]{}, fmt.Errorf("search direct lookup fixture case %q has non-positive limit", tc.Name)
		}
		if tc.Input.Offset < 0 {
			return testcase.Corpus[directLookupInput, directLookupExpected]{}, fmt.Errorf("search direct lookup fixture case %q has negative offset", tc.Name)
		}
		if len(tc.Input.Sessions) == 0 {
			return testcase.Corpus[directLookupInput, directLookupExpected]{}, fmt.Errorf("search direct lookup fixture case %q seeds no sessions", tc.Name)
		}
		for _, session := range tc.Input.Sessions {
			if session.ID == "" {
				return testcase.Corpus[directLookupInput, directLookupExpected]{}, fmt.Errorf("search direct lookup fixture case %q seeds a session with an empty id", tc.Name)
			}
		}
		n := len(tc.Expected.SessionIDs)
		if len(tc.Expected.ProjectHashes) != n || len(tc.Expected.EntryIndexes) != n || len(tc.Expected.Roles) != n {
			return testcase.Corpus[directLookupInput, directLookupExpected]{}, fmt.Errorf("search direct lookup fixture case %q has non-parallel expected arms", tc.Name)
		}
		if len(tc.Expected.Snippets) != 0 && len(tc.Expected.Snippets) != n {
			return testcase.Corpus[directLookupInput, directLookupExpected]{}, fmt.Errorf("search direct lookup fixture case %q has non-parallel snippets arm", tc.Name)
		}
	}
	return corpus, nil
}

func loadSearchDirectLookupCorpus(t *testing.T) testcase.Corpus[directLookupInput, directLookupExpected] {
	t.Helper()
	corpus, err := decodeSearchDirectLookupCorpus(searchDirectLookupYAML)
	if err != nil {
		t.Fatalf("load search direct lookup fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(searchDirectLookupManifestYAML, "search direct lookup")
	if err != nil {
		t.Fatal(err)
	}
	actualNames := make([]string, 0, len(corpus.Cases))
	for _, tc := range corpus.Cases {
		actualNames = append(actualNames, tc.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, actualNames, "search direct lookup"); err != nil {
		t.Fatal(err)
	}
	testassert.RequireMin(t, corpus, len(manifest.RequiredNames))
	testassert.RequireValid(t, corpus)
	return corpus
}

func TestSearchDirectLookupFixtureGuards(t *testing.T) {
	corpus := loadSearchDirectLookupCorpus(t)
	manifest, err := testutil.DecodeRequiredNamesManifest(searchDirectLookupManifestYAML, "search direct lookup")
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.CheckMin(len(manifest.RequiredNames) + 1); err == nil {
		t.Fatal("search direct lookup CheckMin negative control did not fire")
	}
	mutated := corpus
	mutated.Cases = append([]testcase.Case[directLookupInput, directLookupExpected](nil), corpus.Cases...)
	mutated.Cases[0].Mutation.Description = ""
	if err := mutated.Validate(); err == nil {
		t.Fatal("search direct lookup non-vacuity mutation unexpectedly validated")
	}

	unknown := bytes.Replace(searchDirectLookupYAML, []byte("query: harbor"), []byte("query: harbor\n      unexpected: true"), 1)
	if _, err := decodeSearchDirectLookupCorpus(unknown); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("unknown-field mutation error = %v, want strict rejection", err)
	}
	trailing := append(append([]byte{}, searchDirectLookupYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := decodeSearchDirectLookupCorpus(trailing); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("trailing-document mutation error = %v, want strict rejection", err)
	}
	unknownManifest := bytes.Replace(searchDirectLookupManifestYAML, []byte("requiredNames:"), []byte("unexpected: true\nrequiredNames:"), 1)
	if _, err := testutil.DecodeRequiredNamesManifest(unknownManifest, "search direct lookup"); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("manifest unknown-field mutation error = %v, want strict rejection", err)
	}
	trailingManifest := append(append([]byte{}, searchDirectLookupManifestYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := testutil.DecodeRequiredNamesManifest(trailingManifest, "search direct lookup"); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("manifest trailing-document mutation error = %v, want strict rejection", err)
	}
	names := make([]string, 0, len(corpus.Cases))
	for _, tc := range corpus.Cases {
		names = append(names, tc.Name)
	}
	replaced := bytes.Replace(searchDirectLookupYAML, []byte("name: "+names[0]), []byte("name: replacement_family"), 1)
	replacedCorpus, err := decodeSearchDirectLookupCorpus(replaced)
	if err != nil {
		t.Fatalf("family replacement fixture failed to decode: %v", err)
	}
	replacedNames := make([]string, 0, len(replacedCorpus.Cases))
	for _, tc := range replacedCorpus.Cases {
		replacedNames = append(replacedNames, tc.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, replacedNames, "search direct lookup"); err == nil || !strings.Contains(err.Error(), "missing required case") {
		t.Fatalf("family replacement error = %v, want missing-required-case rejection", err)
	}
	if err := testutil.ValidateRequiredNames(manifest, names[1:], "search direct lookup"); err == nil {
		t.Fatal("manifest deletion mutation unexpectedly validated")
	}
}

// seedDirectLookupSessions inserts one session row per fixture session in
// input order; a session with empty content gets no indexed entries. Start
// times increase with input order, so the last session listed is the newest —
// the one a project-hash hit identifies.
func seedDirectLookupSessions(t *testing.T, s *store.Store, sessions []directLookupSession) {
	t.Helper()
	base := fxBase()
	for i, session := range sessions {
		startMs := base + int64(i+1)*1000
		seedSession(t, s, session.ID, "", startMs, startMs+500)
		if session.Content != "" {
			seedEntries(t, s, session.ID, []entrySpec{userTurn(startMs, session.Content)})
		}
	}
}

func TestSearch_DirectLookup(t *testing.T) {
	corpus := loadSearchDirectLookupCorpus(t)
	for _, tc := range corpus.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			s := storetest.Open(t)
			seedDirectLookupSessions(t, s, tc.Input.Sessions)
			svc := codemap.NewService(
				s,
				func(string) gitops.Repository { return noRepo() },
				codegraph.NewGraphBuilder(),
				sessionvisibility.All(),
			)
			ctx := context.Background()
			var payload *schema.SearchPayload
			var err error
			if tc.Input.Offset > 0 {
				payload, err = svc.SearchRankedWindow(ctx, tc.Input.Query, tc.Input.Limit, tc.Input.Offset)
			} else {
				payload, err = svc.Search(ctx, tc.Input.Query, tc.Input.Limit)
			}
			if err != nil {
				t.Fatalf("Search(%q): %v", tc.Input.Query, err)
			}
			if payload.Query != tc.Input.Query {
				t.Errorf("Query = %q, want %q", payload.Query, tc.Input.Query)
			}
			if payload.Results == nil {
				t.Fatal("Results is nil, want a non-null result set")
			}
			gotIDs := make([]string, len(payload.Results))
			gotHashes := make([]string, len(payload.Results))
			gotEntries := make([]int, len(payload.Results))
			gotRoles := make([]string, len(payload.Results))
			for i, result := range payload.Results {
				gotIDs[i] = result.SessionID
				gotHashes[i] = result.ProjectHash.String()
				gotEntries[i] = result.EntryIndex
				gotRoles[i] = result.Role
				if result.Score <= 0 {
					t.Errorf("result %d score = %v, want > 0", i, result.Score)
				}
			}
			if !reflect.DeepEqual(gotIDs, tc.Expected.SessionIDs) {
				t.Errorf("session IDs = %v, want %v", gotIDs, tc.Expected.SessionIDs)
			}
			if !reflect.DeepEqual(gotHashes, tc.Expected.ProjectHashes) {
				t.Errorf("project hashes = %v, want %v", gotHashes, tc.Expected.ProjectHashes)
			}
			if !reflect.DeepEqual(gotEntries, tc.Expected.EntryIndexes) {
				t.Errorf("entry indexes = %v, want %v", gotEntries, tc.Expected.EntryIndexes)
			}
			if !reflect.DeepEqual(gotRoles, tc.Expected.Roles) {
				t.Errorf("roles = %v, want %v", gotRoles, tc.Expected.Roles)
			}
			if len(tc.Expected.Snippets) > 0 {
				gotSnippets := make([]string, len(payload.Results))
				for i, result := range payload.Results {
					gotSnippets[i] = result.Snippet
				}
				if !reflect.DeepEqual(gotSnippets, tc.Expected.Snippets) {
					t.Errorf("snippets = %v, want %v", gotSnippets, tc.Expected.Snippets)
				}
			}
		})
	}
}

// TestSearch_NearMissFallsThroughToFTS proves a 63-hex token never takes the
// direct path: only the FTS read can return indexed content quoting it, and
// the hit carries FTS snippet markers rather than the direct empty marker.
func TestSearch_NearMissFallsThroughToFTS(t *testing.T) {
	t.Parallel()
	s := storetest.Open(t)
	seedDirectLookupSessions(t, s, []directLookupSession{
		{ID: "77777777-7777-7777-7777-777777777777", Content: "release marker abcdef1234abcdef1234abcdef1234abcdef1234abcdef1234abcdef1234567 end"},
	})
	svc := codemap.NewService(
		s,
		func(string) gitops.Repository { return noRepo() },
		codegraph.NewGraphBuilder(),
		sessionvisibility.All(),
	)
	got, err := svc.Search(context.Background(), "abcdef1234abcdef1234abcdef1234abcdef1234abcdef1234abcdef1234567", 20)
	if err != nil {
		t.Fatalf("Search(63-hex token): %v", err)
	}
	if len(got.Results) != 1 {
		t.Fatalf("results for 63-hex token = %d, want 1 via FTS fall-through: %+v", len(got.Results), got.Results)
	}
	if got.Results[0].SessionID != "77777777-7777-7777-7777-777777777777" {
		t.Errorf("sessionId = %q, want the quoting session", got.Results[0].SessionID)
	}
	if !strings.Contains(got.Results[0].Snippet, "[") || !strings.Contains(got.Results[0].Snippet, "]") {
		t.Errorf("snippet missing FTS markers, the direct path must not have served this query: %q", got.Results[0].Snippet)
	}
}
