package kickstart_test

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/touched_selection.yaml
var mountedTouchedSelectionData []byte

type mountedTouchedAction string

const (
	mountedTouchedDown      mountedTouchedAction = "down"
	mountedTouchedToggle    mountedTouchedAction = "toggle"
	mountedTouchedFilter    mountedTouchedAction = "filter"
	mountedTouchedSelectAll mountedTouchedAction = "select-all"
)

type mountedTouchedDocument struct {
	ExpectedCaseCount int                  `yaml:"expectedCaseCount"`
	ExpectedNames     []string             `yaml:"expectedNames"`
	Cases             []mountedTouchedCase `yaml:"cases"`
}

type mountedTouchedCase struct {
	Name                      string                  `yaml:"name"`
	Paths                     []mountedPathFixture    `yaml:"paths"`
	Listings                  []mountedListingFixture `yaml:"listings"`
	Saved                     mountedSelectionFixture `yaml:"saved"`
	Actions                   []mountedTouchedAction  `yaml:"actions"`
	Refresh                   bool                    `yaml:"refresh"`
	ExpectedCheckedSessions   []string                `yaml:"expectedCheckedSessions"`
	ExpectedUncheckedSessions []string                `yaml:"expectedUncheckedSessions"`
	Expected                  mountedSelectionFixture `yaml:"expected"`
}

func loadMountedTouchedDocument(t *testing.T) mountedTouchedDocument {
	t.Helper()
	var document mountedTouchedDocument
	decoder := yaml.NewDecoder(bytes.NewReader(mountedTouchedSelectionData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode kickstart testdata/touched_selection.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("kickstart touched_selection.yaml must hold exactly one document: %v", err)
	}
	if document.ExpectedCaseCount != len(document.Cases) || document.ExpectedCaseCount != len(document.ExpectedNames) || len(document.Cases) == 0 {
		t.Fatalf("mounted fixture manifest count=%d names=%d cases=%d", document.ExpectedCaseCount, len(document.ExpectedNames), len(document.Cases))
	}
	seen := map[string]bool{}
	for index, testCase := range document.Cases {
		testutil.RequireFixtureFields(t, "mounted touched selection", testCase.Name, []testutil.FixtureField{
			{Key: "name", Value: testCase.Name},
			{Key: "saved.mode", Value: testCase.Saved.Mode.String()},
			{Key: "expected.mode", Value: testCase.Expected.Mode.String()},
		})
		if testCase.Name != document.ExpectedNames[index] {
			t.Fatalf("case[%d] name=%q, manifest=%q", index, testCase.Name, document.ExpectedNames[index])
		}
		if seen[testCase.Name] {
			t.Fatalf("duplicate mounted touched-selection case %q", testCase.Name)
		}
		seen[testCase.Name] = true
		if len(testCase.Paths) == 0 || len(testCase.Listings) == 0 || len(testCase.Actions) == 0 {
			t.Fatalf("case %q requires paths, listings, and actions", testCase.Name)
		}
		pathKeys := map[string]struct{}{}
		for _, path := range testCase.Paths {
			validateMountedPathKey(t, testCase.Name, path.Key)
			if _, duplicate := pathKeys[path.Key]; duplicate {
				t.Fatalf("case %q repeats path key %q", testCase.Name, path.Key)
			}
			pathKeys[path.Key] = struct{}{}
			if path.Target != "" {
				validateMountedPathKey(t, testCase.Name, path.Target)
				pathKeys[path.Target] = struct{}{}
			}
		}
		for _, path := range testCase.Paths {
			if path.RepositoryKey != "" {
				requireMountedPathReference(t, testCase.Name, pathKeys, path.RepositoryKey)
			}
		}
		for _, listing := range testCase.Listings {
			requireMountedPathReference(t, testCase.Name, pathKeys, listing.PathKey)
		}
		for _, action := range testCase.Actions {
			if !validMountedTouchedAction(action) {
				t.Fatalf("case %q has unknown action %q", testCase.Name, action)
			}
		}
	}
	return document
}

func validMountedTouchedAction(action mountedTouchedAction) bool {
	switch action {
	case mountedTouchedDown, mountedTouchedToggle, mountedTouchedFilter, mountedTouchedSelectAll:
		return true
	default:
		return false
	}
}

func TestMountedTouchedSelectionActions(t *testing.T) {
	document := loadMountedTouchedDocument(t)
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			paths := materializeMountedPaths(t, testCase.Paths)
			baseline := mountedSelection(testCase.Saved, paths)
			configured := config.BaseConfig()
			configured.Selection = baseline
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := config.SaveAtomic(configPath, configured); err != nil {
				t.Fatalf("save mounted touched-selection baseline: %v", err)
			}
			draft, err := settings.NewDraft(configPath, configured)
			if err != nil {
				t.Fatalf("open mounted touched-selection draft: %v", err)
			}
			realScanner := kickstart.NewScannerTreeSource(
				mountedListings(testCase.Listings, paths),
				kickstart.WithPathIdentityResolver(ingest.NewPhysicalPathResolver()),
				kickstart.WithRepositoryIdentityResolver(mountedRepositoryResolver(t, testCase.Paths, paths)),
			)
			source := &mountedRecordingTreeSource{inner: realScanner}
			program := kickstart.NewProgram(kickstart.ProgramDeps{
				Theme: theme.New(theme.ModeDark), Draft: draft, Source: source,
			})
			program.SetSize(120, 28)
			program = declineOAuth(t, program)

			for _, action := range testCase.Actions {
				program = pressAndDrain(program, mountedTouchedRune(action))
			}
			want := mountedSelection(testCase.Expected, paths)
			assertExactMountedTouchedSelection(t, draft.Working().Selection, want, "after action")

			if testCase.Refresh {
				program = drainProgram(program, program.Init())
				assertExactMountedTouchedSelection(t, draft.Working().Selection, want, "after refresh")
				assertMountedSessionStates(t, source.latest(), testCase.ExpectedCheckedSessions, testCase.ExpectedUncheckedSessions, "after refresh")
			}

			program, commitCmd := advanceToCommit(program)
			program = drainProgram(program, commitCmd)
			if !program.Committed() {
				t.Fatalf("mounted touched selection did not commit: phase=%s", program.Phase())
			}
			reloaded, err := config.Parse(mustReadFile(t, configPath))
			if err != nil {
				t.Fatalf("parse mounted touched-selection commit: %v", err)
			}
			assertExactMountedTouchedSelection(t, reloaded.Selection, want, "after commit")
		})
	}
}

type mountedRepositoryIdentityResolver map[ingest.ClonePath]ingest.RepositoryIdentity

func (r mountedRepositoryIdentityResolver) ResolveRepositoryIdentity(_ context.Context, clonePath ingest.ClonePath) (ingest.RepositoryIdentity, error) {
	path, ok := r[clonePath]
	if !ok {
		return ingest.RepositoryIdentity{}, fmt.Errorf("mounted fixture has no repository identity for clone %q", clonePath)
	}
	return path, nil
}

func mountedRepositoryResolver(t *testing.T, fixtures []mountedPathFixture, paths map[string]string) mountedRepositoryIdentityResolver {
	t.Helper()
	resolver := ingest.NewPhysicalPathResolver()
	result := mountedRepositoryIdentityResolver{}
	for _, fixture := range fixtures {
		clonePath, err := resolver.Resolve(paths[fixture.Key])
		if err != nil {
			continue
		}
		repositoryKey := fixture.RepositoryKey
		if repositoryKey == "" {
			repositoryKey = fixture.Key
		}
		repositoryPath, err := resolver.Resolve(paths[repositoryKey])
		if err != nil {
			t.Fatalf("resolve mounted repository key %q for clone %q: %v", repositoryKey, fixture.Key, err)
		}
		result[clonePath] = ingest.RepositoryIdentity{
			CohortKey:    ingest.RepositoryCohortKey("fixture:" + repositoryPath.String()),
			GitDirectory: ingest.RepositoryPath(repositoryPath.String()),
		}
	}
	return result
}

var _ ingest.RepositoryIdentityResolver = mountedRepositoryIdentityResolver{}

func mountedTouchedRune(action mountedTouchedAction) rune {
	switch action {
	case mountedTouchedDown:
		return 'j'
	case mountedTouchedToggle:
		return ' '
	case mountedTouchedFilter:
		return 'f'
	case mountedTouchedSelectAll:
		return 'a'
	default:
		return 0
	}
}

func assertExactMountedTouchedSelection(t *testing.T, got, want config.SelectionConfig, stage string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mounted touched selection %s mismatch\n got: %#v\nwant: %#v", stage, got, want)
	}
}
