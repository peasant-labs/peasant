package codemap_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"path/filepath"
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
	"github.com/peasant-labs/schema/testcase/assert"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/selection_state.yaml
var selectionStateYAML []byte

//go:embed testdata/selection_state_manifest.yaml
var selectionStateManifestYAML []byte

type selectionStateInput struct {
	Selection config.SelectionConfig  `yaml:"selection"`
	Projects  []selectionStateProject `yaml:"projects"`
}

type selectionStateProject struct {
	CWD      string                  `yaml:"cwd"`
	Sessions []selectionStateSession `yaml:"sessions"`
}

type selectionStateSession struct {
	ID          string `yaml:"id"`
	Branch      string `yaml:"branch"`
	GitWorktree string `yaml:"git_worktree"`
	ParentID    string `yaml:"parent_id"`
}

type selectionStateExpected struct {
	Active              bool `yaml:"active"`
	HiddenProjects      int  `yaml:"hidden_projects"`
	HiddenSessions      int  `yaml:"hidden_sessions"`
	VisibleProjectCount int  `yaml:"visible_project_count"`
	VisibleSessionCount int  `yaml:"visible_session_count"`
}

const selectionStateExpectedCaseCount = 7

// selectionStatePathResolver treats the fixture's clean absolute paths as
// already-resolved physical identities. Production uses
// ingest.NewPhysicalPathResolver; this seam keeps store-backed fixture cases
// independent from the test runner's filesystem.
type selectionStatePathResolver struct{}

var _ ingest.PathIdentityResolver = selectionStatePathResolver{}

func (selectionStatePathResolver) Resolve(dir string) (ingest.ClonePath, error) {
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return "", fmt.Errorf("fixture path resolver: path %q is not a clean absolute directory identity", dir)
	}
	return ingest.ClonePath(dir), nil
}

// decodeSelectionStateCorpus is the pure (no *testing.T) loader, split out so
// TestSelectionStateFixtureGuards can drive it directly with mutated bytes —
// same split as project_resolution_test.go's decode/load pair.
func decodeSelectionStateCorpus(data []byte) (testcase.Corpus[selectionStateInput, selectionStateExpected], error) {
	manifest, err := testutil.DecodeSemanticManifest(selectionStateManifestYAML, "selection state")
	if err != nil {
		return testcase.Corpus[selectionStateInput, selectionStateExpected]{}, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var corpus testcase.Corpus[selectionStateInput, selectionStateExpected]
	if err := decoder.Decode(&corpus); err != nil {
		return testcase.Corpus[selectionStateInput, selectionStateExpected]{}, fmt.Errorf("decode selection state fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return testcase.Corpus[selectionStateInput, selectionStateExpected]{}, fmt.Errorf("selection state fixture must contain exactly one YAML document: %v", err)
	}
	names := make([]string, 0, len(corpus.Cases))
	for _, c := range corpus.Cases {
		names = append(names, c.Name)
	}
	// EXACT-manifest guard (mirrors project_resolution_manifest.yaml): a
	// count-preserving swap that silently drops a real case and adds a
	// filler with a different name is invisible to assert.RequireMin alone,
	// but not to a manifest that independently names every required case.
	if err := testutil.ValidateSemanticNames(manifest, names, "selection state"); err != nil {
		return testcase.Corpus[selectionStateInput, selectionStateExpected]{}, err
	}
	return corpus, nil
}

func loadSelectionStateCorpus(t *testing.T) testcase.Corpus[selectionStateInput, selectionStateExpected] {
	t.Helper()
	corpus, err := decodeSelectionStateCorpus(selectionStateYAML)
	if err != nil {
		t.Fatalf("load selection state fixture: %v", err)
	}
	assert.RequireMin(t, corpus, selectionStateExpectedCaseCount)
	assert.RequireValid(t, corpus)
	return corpus
}

// TestSelectionStateFixtureGuards proves the exact-manifest guard actually
// rejects the mutations it exists to catch (mirrors
// TestProjectResolutionFixtureGuards's structure).
func TestSelectionStateFixtureGuards(t *testing.T) {
	swapped := bytes.Replace(selectionStateYAML, []byte("name: all_mode_selection_inactive_nothing_hidden"), []byte("name: replacement_case"), 1)
	if _, err := decodeSelectionStateCorpus(swapped); err == nil {
		t.Fatal("expected a count-preserving case-name swap to be rejected by the manifest guard")
	}
	unknownField := bytes.Replace(selectionStateYAML, []byte("cases:"), []byte("cases:\n  - unexpected: true"), 1)
	if _, err := decodeSelectionStateCorpus(unknownField); err == nil {
		t.Fatal("expected an unknown-field mutation to be rejected")
	}
	trailing := append(append([]byte{}, selectionStateYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := decodeSelectionStateCorpus(trailing); err == nil {
		t.Fatal("expected a trailing-document mutation to be rejected")
	}
	unknownManifestField := bytes.Replace(selectionStateManifestYAML, []byte("expectedCaseCount:"), []byte("unexpected: true\nexpectedCaseCount:"), 1)
	if _, err := testutil.DecodeSemanticManifest(unknownManifestField, "selection state"); err == nil {
		t.Fatal("expected an unknown-field manifest mutation to be rejected")
	}
}

// buildSelectionPolicy turns a fixture selection into a real
// sessionvisibility.Policy via the same config.SelectionConfig +
// sessionvisibility.New path production code uses (never a hand-rolled
// double of the matcher logic).
func buildSelectionPolicy(t *testing.T, sel config.SelectionConfig) sessionvisibility.Policy {
	t.Helper()
	policy, err := sessionvisibility.New(sel)
	if err != nil {
		t.Fatalf("selection policy: %v", err)
	}
	return policy
}

// TestProjectSummaries_SelectionState drives ProjectSummaries' SelectionState
// over the fixture corpus: an inactive (mode=all) selection reports nothing
// hidden, an active selection that excludes a whole project reports it in
// HiddenProjects (and all its sessions in HiddenSessions), and an active
// selection that keeps a project's row but excludes some of its sessions
// reports only HiddenSessions — proving the two counters are independent and
// derived from the same visibility pass ProjectSummaries uses to build Projects
// after project-selection usability testing.
func TestProjectSummaries_SelectionState(t *testing.T) {
	corpus := loadSelectionStateCorpus(t)
	for _, fixtureCase := range corpus.Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			database := storetest.Open(t)
			base := fxBase()
			for pIndex, project := range fixtureCase.Input.Projects {
				projectHash := schema.ProjectHash(fmt.Sprintf("%02d%062x", pIndex+1, pIndex+1))
				for sIndex, session := range project.Sessions {
					startMs := base + int64(pIndex*100+sIndex+1)*1000
					endMs := startMs + 500
					ingestedMs := endMs + 1
					sessionID, sessionIDErr := schema.NewSessionID(session.ID)
					if sessionIDErr != nil {
						t.Fatalf("fixture session ID %q: %v", session.ID, sessionIDErr)
					}
					meta := &schema.UnifiedMetadata{
						SessionID:    sessionID,
						ModelHarness: defaults.HarnessClaudeCode,
						Model:        testutil.TestModel,
						HostSlug:     schema.HostSlug(testutil.TestHostSlug),
						Project:      schema.ProjectContext{Hash: projectHash, Name: "project", FilePath: project.CWD},
						Timestamp:    schema.TimestampInfo{Start: startMs, End: endMs, Ingested: &ingestedMs},
						Source:       schema.SourceInfo{FilePath: "/selection.jsonl", Format: schema.SourceFormatJSONL},
					}
					if session.Branch != "" {
						branch := session.Branch
						meta.Git.Branch = &branch
					}
					if session.GitWorktree != "" {
						worktree := session.GitWorktree
						meta.Git.Worktree = &worktree
					}
					if session.ParentID != "" {
						parentID, parentIDErr := schema.NewSessionID(session.ParentID)
						if parentIDErr != nil {
							t.Fatalf("fixture parent session ID %q: %v", session.ParentID, parentIDErr)
						}
						meta.ParentUUID = &parentID
					}
					if err := database.InsertSessions(context.Background(), []ingest.StoreEntry{{Metadata: meta}}); err != nil {
						t.Fatalf("seed project %s session %s: %v", project.CWD, session.ID, err)
					}
				}
			}

			policy := buildSelectionPolicy(t, fixtureCase.Input.Selection)
			service := codemap.NewService(
				database,
				func(string) gitops.Repository { return noRepo() },
				codegraph.NewGraphBuilder(),
				policy,
				codemap.WithPathIdentityResolver(selectionStatePathResolver{}),
			)
			result, err := service.ProjectSummaries(context.Background())
			if err != nil {
				t.Fatalf("ProjectSummaries: %v", err)
			}

			if result.Selection.Active != fixtureCase.Expected.Active {
				t.Errorf("Selection.Active = %v, want %v", result.Selection.Active, fixtureCase.Expected.Active)
			}
			if result.Selection.HiddenProjects != fixtureCase.Expected.HiddenProjects {
				t.Errorf("Selection.HiddenProjects = %d, want %d", result.Selection.HiddenProjects, fixtureCase.Expected.HiddenProjects)
			}
			if result.Selection.HiddenSessions != fixtureCase.Expected.HiddenSessions {
				t.Errorf("Selection.HiddenSessions = %d, want %d", result.Selection.HiddenSessions, fixtureCase.Expected.HiddenSessions)
			}
			if len(result.Projects) != fixtureCase.Expected.VisibleProjectCount {
				t.Errorf("len(Projects) = %d, want %d", len(result.Projects), fixtureCase.Expected.VisibleProjectCount)
			}
			visibleSessionCount := 0
			for _, project := range result.Projects {
				visibleSessionCount += project.Sessions
			}
			if visibleSessionCount != fixtureCase.Expected.VisibleSessionCount {
				t.Errorf("visible project session count = %d, want %d", visibleSessionCount, fixtureCase.Expected.VisibleSessionCount)
			}
		})
	}
}
