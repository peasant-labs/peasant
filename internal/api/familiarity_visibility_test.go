package api_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"github.com/peasant-labs/schema/testcase"
	testassert "github.com/peasant-labs/schema/testcase/assert"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/familiarity_visibility.yaml
var familiarityVisibilityYAML []byte

//go:embed testdata/familiarity_visibility_manifest.yaml
var familiarityVisibilityManifestYAML []byte

type familiarityVisibilityInput struct {
	ProjectHash string                 `yaml:"project_hash"`
	Selection   config.SelectionConfig `yaml:"selection"`
	Sessions    []familiaritySession   `yaml:"sessions"`
}

type familiaritySession struct {
	ID          string            `yaml:"id"`
	StartMs     int64             `yaml:"start_ms"`
	DurationMs  int64             `yaml:"duration_ms"`
	GitRemote   string            `yaml:"git_remote"`
	GitBranch   string            `yaml:"git_branch"`
	ProjectName string            `yaml:"project_name"`
	Files       []familiarityFile `yaml:"files"`
}

type familiarityFile struct {
	Path        string                 `yaml:"path"`
	Interaction schema.InteractionType `yaml:"interaction"`
	TurnCount   int                    `yaml:"turn_count"`
	HumanTurns  int                    `yaml:"human_turns"`
}

type familiarityVisibilityExpected struct {
	FilePaths          []string          `yaml:"file_paths"`
	FileSessionCounts  map[string]int    `yaml:"file_session_counts"`
	FileTotalTurns     map[string]int    `yaml:"file_total_turns"`
	LastEngagedSession map[string]string `yaml:"last_engaged_session"`
	TrailSessionIDs    []string          `yaml:"trail_session_ids"`
	SuggestionPaths    []string          `yaml:"suggestion_paths"`
	AbsentSessionIDs   []string          `yaml:"absent_session_ids"`
	ArraysNonNull      bool              `yaml:"arrays_non_null"`
	ErrorContains      string            `yaml:"error_contains"`
}

func decodeFamiliarityVisibilityCorpus(data []byte) (testcase.Corpus[familiarityVisibilityInput, familiarityVisibilityExpected], error) {
	manifest, err := testutil.DecodeSemanticManifest(familiarityVisibilityManifestYAML, "familiarity visibility")
	if err != nil {
		return testcase.Corpus[familiarityVisibilityInput, familiarityVisibilityExpected]{}, err
	}
	var corpus testcase.Corpus[familiarityVisibilityInput, familiarityVisibilityExpected]
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&corpus); err != nil {
		return testcase.Corpus[familiarityVisibilityInput, familiarityVisibilityExpected]{}, fmt.Errorf("decode familiarity visibility fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return testcase.Corpus[familiarityVisibilityInput, familiarityVisibilityExpected]{}, fmt.Errorf("familiarity visibility fixture must contain exactly one YAML document: %v", err)
	}
	names := make(map[string]struct{}, len(corpus.Cases))
	actualNames := make([]string, 0, len(corpus.Cases))
	for _, tc := range corpus.Cases {
		if _, duplicate := names[tc.Name]; duplicate {
			return testcase.Corpus[familiarityVisibilityInput, familiarityVisibilityExpected]{}, fmt.Errorf("familiarity visibility fixture repeats case name %q", tc.Name)
		}
		names[tc.Name] = struct{}{}
		actualNames = append(actualNames, tc.Name)
		if tc.Input.ProjectHash == "" || len(tc.Input.Sessions) == 0 {
			return testcase.Corpus[familiarityVisibilityInput, familiarityVisibilityExpected]{}, fmt.Errorf("familiarity visibility fixture case %q has incomplete input", tc.Name)
		}
		if (tc.Classification == testcase.MustFail) != (tc.Expected.ErrorContains != "") {
			return testcase.Corpus[familiarityVisibilityInput, familiarityVisibilityExpected]{}, fmt.Errorf("familiarity visibility fixture case %q must pair must-fail with error_contains", tc.Name)
		}
	}
	if err := testutil.ValidateSemanticNames(manifest, actualNames, "familiarity visibility"); err != nil {
		return testcase.Corpus[familiarityVisibilityInput, familiarityVisibilityExpected]{}, err
	}
	return corpus, nil
}

func loadFamiliarityVisibilityCorpus(t *testing.T) testcase.Corpus[familiarityVisibilityInput, familiarityVisibilityExpected] {
	t.Helper()
	corpus, err := decodeFamiliarityVisibilityCorpus(familiarityVisibilityYAML)
	if err != nil {
		t.Fatalf("load familiarity visibility fixture: %v", err)
	}
	manifest, err := testutil.DecodeSemanticManifest(familiarityVisibilityManifestYAML, "familiarity visibility")
	if err != nil {
		t.Fatal(err)
	}
	testassert.RequireMin(t, corpus, manifest.ExpectedCaseCount)
	testassert.RequireValid(t, corpus)
	return corpus
}

func TestFamiliarityVisibilityFixtureGuards(t *testing.T) {
	corpus := loadFamiliarityVisibilityCorpus(t)
	manifest, err := testutil.DecodeSemanticManifest(familiarityVisibilityManifestYAML, "familiarity visibility")
	if err != nil {
		t.Fatal(err)
	}
	if err := corpus.CheckMin(manifest.ExpectedCaseCount + 1); err == nil {
		t.Fatal("familiarity visibility CheckMin negative control did not fire")
	}
	mutated := corpus
	mutated.Cases = append([]testcase.Case[familiarityVisibilityInput, familiarityVisibilityExpected](nil), corpus.Cases...)
	mutated.Cases[0].Provenance.Ref = ""
	if err := mutated.Validate(); err == nil {
		t.Fatal("familiarity visibility non-vacuity mutation unexpectedly validated")
	}
	unknown := bytes.Replace(familiarityVisibilityYAML, []byte("project_hash:"), []byte("unexpected: true\n      project_hash:"), 1)
	if _, err := decodeFamiliarityVisibilityCorpus(unknown); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("unknown-field mutation error = %v, want strict rejection", err)
	}
	trailing := append(append([]byte{}, familiarityVisibilityYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := decodeFamiliarityVisibilityCorpus(trailing); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing-document mutation error = %v, want strict rejection", err)
	}
	unknownManifest := bytes.Replace(familiarityVisibilityManifestYAML, []byte("expectedCaseCount:"), []byte("unexpected: true\nexpectedCaseCount:"), 1)
	if _, err := testutil.DecodeSemanticManifest(unknownManifest, "familiarity visibility"); err == nil || !strings.Contains(err.Error(), "field unexpected not found") {
		t.Fatalf("manifest unknown-field mutation error = %v, want strict rejection", err)
	}
	trailingManifest := append(append([]byte{}, familiarityVisibilityManifestYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := testutil.DecodeSemanticManifest(trailingManifest, "familiarity visibility"); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("manifest trailing-document mutation error = %v, want strict rejection", err)
	}
	for _, family := range manifest.RequiredNames {
		mutatedBytes := bytes.Replace(familiarityVisibilityYAML, []byte("name: "+family), []byte("name: replacement_family"), 1)
		if _, err := decodeFamiliarityVisibilityCorpus(mutatedBytes); err == nil || !strings.Contains(err.Error(), "missing required family") {
			t.Fatalf("family %q replacement error = %v, want count-preserving mutation rejection", family, err)
		}
	}
	if err := testutil.ValidateSemanticNames(manifest, manifest.RequiredNames[1:], "familiarity visibility"); err == nil {
		t.Fatal("manifest deletion mutation unexpectedly validated")
	}
}

func TestProjectFamiliarity_FiltersBeforeAggregationAndCaps(t *testing.T) {
	corpus := loadFamiliarityVisibilityCorpus(t)
	for _, tc := range corpus.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			s := openTestStore(t)
			entries := make([]ingest.StoreEntry, 0, len(tc.Input.Sessions))
			for _, session := range tc.Input.Sessions {
				projectName := session.ProjectName
				if projectName == "" {
					projectName = "repo"
				}
				entry := makeStoreEntry(t, session.ID, tc.Input.ProjectHash, testutil.TestHostSlug, defaults.HarnessClaudeCode, session.StartMs, 1, 1, projectName, 1, 0, session.DurationMs)
				if session.GitRemote != "" {
					entry.Metadata.Git.Remote = &session.GitRemote
				}
				if session.GitBranch != "" {
					entry.Metadata.Git.Branch = &session.GitBranch
				}
				entries = append(entries, entry)
			}
			seedStore(t, s, entries)
			for _, session := range tc.Input.Sessions {
				rows := make([]store.SessionFileRow, len(session.Files))
				for i, file := range session.Files {
					rows[i] = store.SessionFileRow{SessionID: session.ID, FilePath: file.Path, Interaction: file.Interaction.String(), TurnCount: file.TurnCount, HumanTurns: file.HumanTurns}
				}
				if err := s.UpsertSessionFiles(context.Background(), session.ID, rows); err != nil {
					t.Fatalf("UpsertSessionFiles(%s): %v", session.ID, err)
				}
			}
			policy, err := sessionvisibility.New(tc.Input.Selection)
			if err != nil {
				t.Fatalf("selection policy: %v", err)
			}
			provider := api.NewStoreDataProvider(s, policy)
			projectHash, hashErr := schema.NewProjectHash(tc.Input.ProjectHash)
			if hashErr != nil {
				t.Fatalf("fixture project hash: %v", hashErr)
			}
			payload, err := provider.ProjectFamiliarity(context.Background(), projectHash)
			if tc.Classification == testcase.MustFail {
				if err == nil || !sessionvisibility.IsError(err) || !strings.Contains(err.Error(), tc.Expected.ErrorContains) {
					t.Fatalf("ProjectFamiliarity error = %v, want visibility error containing %q", err, tc.Expected.ErrorContains)
				}
				if payload != nil {
					t.Fatalf("ProjectFamiliarity returned partial payload on policy error: %+v", payload)
				}
				return
			}
			if err != nil {
				t.Fatalf("ProjectFamiliarity: %v", err)
			}
			assertFamiliarityVisibility(t, payload, tc.Input, tc.Expected)
		})
	}
}

func assertFamiliarityVisibility(t *testing.T, payload *api.FamiliarityPayload, input familiarityVisibilityInput, expected familiarityVisibilityExpected) {
	t.Helper()
	if expected.ArraysNonNull {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal familiarity payload: %v", err)
		}
		for _, field := range []string{"\"files\":null", "\"trails\":null", "\"suggestions\":null"} {
			if bytes.Contains(encoded, []byte(field)) {
				t.Fatalf("familiarity payload contains null collection %s: %s", field, encoded)
			}
		}
	}
	gotPaths := make([]string, len(payload.Files))
	gotCounts := make(map[string]int, len(payload.Files))
	gotTurns := make(map[string]int, len(payload.Files))
	for i, file := range payload.Files {
		gotPaths[i] = file.Path
		gotCounts[file.Path] = file.SessionCount
		gotTurns[file.Path] = file.TotalTurns
		if sessionID, ok := expected.LastEngagedSession[file.Path]; ok {
			session := findFamiliaritySession(t, input.Sessions, sessionID)
			want := time.UnixMilli(session.StartMs + session.DurationMs).UTC().Format(time.RFC3339)
			if file.LastEngagedAt == nil || *file.LastEngagedAt != want {
				t.Fatalf("file %s lastEngagedAt = %v, want %s from visible session %s", file.Path, file.LastEngagedAt, want, sessionID)
			}
		}
	}
	if !reflect.DeepEqual(gotPaths, expected.FilePaths) || !reflect.DeepEqual(gotCounts, expected.FileSessionCounts) || !reflect.DeepEqual(gotTurns, expected.FileTotalTurns) {
		t.Fatalf("familiarity files = paths %v counts %v turns %v; want %v %v %v", gotPaths, gotCounts, gotTurns, expected.FilePaths, expected.FileSessionCounts, expected.FileTotalTurns)
	}
	gotTrails := make([]string, len(payload.Trails))
	for i, trail := range payload.Trails {
		gotTrails[i] = trail.SessionID
	}
	if !reflect.DeepEqual(gotTrails, expected.TrailSessionIDs) {
		t.Fatalf("trail session IDs = %v, want %v", gotTrails, expected.TrailSessionIDs)
	}
	gotSuggestions := make([]string, len(payload.Suggestions))
	for i, suggestion := range payload.Suggestions {
		gotSuggestions[i] = suggestion.Path
	}
	if !reflect.DeepEqual(gotSuggestions, expected.SuggestionPaths) {
		t.Fatalf("suggestion paths = %v, want %v", gotSuggestions, expected.SuggestionPaths)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal familiarity payload: %v", err)
	}
	for _, hiddenID := range expected.AbsentSessionIDs {
		if bytes.Contains(encoded, []byte(hiddenID)) {
			t.Fatalf("familiarity payload leaked hidden session %s: %s", hiddenID, encoded)
		}
	}
}

func findFamiliaritySession(t *testing.T, sessions []familiaritySession, id string) familiaritySession {
	t.Helper()
	for _, session := range sessions {
		if session.ID == id {
			return session
		}
	}
	t.Fatalf("fixture session %s not found", id)
	return familiaritySession{}
}
