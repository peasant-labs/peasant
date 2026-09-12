package kickstart_test

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/touched_selection.yaml
var mountedTouchedSelectionData []byte

type mountedTouchedAction string

const (
	mountedTouchedDown      mountedTouchedAction = "down"
	mountedTouchedExpand    mountedTouchedAction = "expand"
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
	// ExpectedProjectLabel optionally pins the rendered root label. A case
	// whose project has no remote and no discovery name declares the resolved
	// path suffix here so a regression to a display placeholder, or a leaked
	// absolute physical path, fails before any action runs. These cases also
	// carry no declared repository identity, so the same check pins the project
	// identity to the exact resolved physical path instead of a synthetic Git
	// cohort.
	ExpectedProjectLabel string `yaml:"expectedProjectLabel"`
	// ExpectedFlatProject asserts the single root drops the branch level: its
	// children are session rows, no branch row exists anywhere under it, and
	// its preview context counts those direct sessions without publishing a
	// branch context for a session row.
	ExpectedFlatProject bool `yaml:"expectedFlatProject"`
	// ExpectedProjectBranches pins the branch row labels under the single root,
	// in order, for a project that keeps the branch level.
	ExpectedProjectBranches []string `yaml:"expectedProjectBranches"`
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
	case mountedTouchedDown, mountedTouchedExpand, mountedTouchedToggle, mountedTouchedFilter, mountedTouchedSelectAll:
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

			if testCase.ExpectedProjectLabel != "" {
				roots := source.latest()
				if len(roots) != 1 {
					t.Fatalf("mounted touched-selection label case loaded %d project roots, want 1", len(roots))
				}
				label := roots[0].Label
				if label != testCase.ExpectedProjectLabel {
					t.Fatalf("mounted touched-selection project label = %q, want %q", label, testCase.ExpectedProjectLabel)
				}
				if strings.Contains(label, "(unknown project)") || filepath.IsAbs(label) {
					t.Fatalf("mounted touched-selection project label %q is a placeholder or an absolute physical path", label)
				}
				physicalPath := paths[testCase.Listings[0].PathKey]
				if identity := roots[0].Meta[settings.MetaProjectIdentity]; identity != physicalPath {
					t.Fatalf("mounted touched-selection project identity = %q, want the exact physical path %q", identity, physicalPath)
				}
			}
			if testCase.ExpectedFlatProject || len(testCase.ExpectedProjectBranches) > 0 {
				roots := source.latest()
				if len(roots) != 1 {
					t.Fatalf("mounted touched-selection shape case loaded %d project roots, want 1", len(roots))
				}
				assertMountedProjectShape(t, roots[0], testCase.ExpectedFlatProject, testCase.ExpectedProjectBranches)
				if testCase.ExpectedFlatProject {
					context, ok := realScanner.ListingPreviewContext(roots[0].ID)
					if !ok {
						t.Fatal("flattened project has no project preview context")
					}
					if context.Kind != kickstart.ListingPreviewProject || context.SessionCount != len(testCase.Listings) || len(context.Branches) != 0 {
						t.Fatalf("flattened project preview = %#v, want a branchless project context over %d sessions", context, len(testCase.Listings))
					}
					for _, listing := range testCase.Listings {
						if _, found := realScanner.ListingPreviewContext(listing.SessionID); found {
							t.Fatalf("flattened project published a branch preview context for session %q", listing.SessionID)
						}
					}
				}
			}

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
			committed := string(mustReadFile(t, configPath))
			if strings.Contains(committed, "(unknown project)") || strings.Contains(committed, "(unknown branch)") {
				t.Fatalf("mounted touched-selection commit named a display placeholder:\n%s", committed)
			}
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

// mountedRepositoryResolver models the Git-topology boundary of the real
// scanner. A fixture that declares a repositoryKey names a Git cohort (linked
// worktrees or a shared common directory). Every other path has no declared Git
// identity, so the mounted resolver reports none and the production scanner
// derives the exact physical path identity for it, exactly as it does for a
// plain non-Git directory.
func mountedRepositoryResolver(t *testing.T, fixtures []mountedPathFixture, paths map[string]string) mountedRepositoryIdentityResolver {
	t.Helper()
	resolver := ingest.NewPhysicalPathResolver()
	result := mountedRepositoryIdentityResolver{}
	for _, fixture := range fixtures {
		if fixture.RepositoryKey == "" {
			continue
		}
		clonePath, err := resolver.Resolve(paths[fixture.Key])
		if err != nil {
			continue
		}
		repositoryPath, err := resolver.Resolve(paths[fixture.RepositoryKey])
		if err != nil {
			t.Fatalf("resolve mounted repository key %q for clone %q: %v", fixture.RepositoryKey, fixture.Key, err)
		}
		result[clonePath] = ingest.RepositoryIdentity{
			CohortKey:    ingest.RepositoryCohortKey("fixture:" + repositoryPath.String()),
			GitDirectory: ingest.RepositoryPath(repositoryPath.String()),
		}
	}
	return result
}

var _ ingest.RepositoryIdentityResolver = mountedRepositoryIdentityResolver{}

// assertMountedProjectShape pins whether the single project root keeps the
// branch level. A flattened project must have session children and no branch
// row anywhere; a branch-level project must show exactly the declared branch
// rows in order.
func assertMountedProjectShape(t *testing.T, root *kit.TreeNode, flat bool, branches []string) {
	t.Helper()
	if flat {
		branchRows := 0
		walkMountedNodes(root, func(node *kit.TreeNode) {
			if node.Meta[settings.MetaBranch] != "" {
				branchRows++
			}
		})
		if branchRows != 0 {
			t.Fatalf("flattened project %q still renders %d branch rows", root.Label, branchRows)
		}
		if len(root.Children) == 0 {
			t.Fatalf("flattened project %q has no session children", root.Label)
		}
		for _, child := range root.Children {
			if child.Meta[settings.MetaHarness] == "" {
				t.Fatalf("flattened project %q child %q is not a session row", root.Label, child.Label)
			}
		}
		return
	}
	got := make([]string, 0, len(root.Children))
	for _, child := range root.Children {
		got = append(got, child.Meta[settings.MetaBranch])
	}
	if !reflect.DeepEqual(got, branches) {
		t.Fatalf("project %q branch rows = %q, want %q", root.Label, got, branches)
	}
}

func mountedTouchedRune(action mountedTouchedAction) rune {
	switch action {
	case mountedTouchedDown:
		return 'j'
	case mountedTouchedExpand:
		return 'l'
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
