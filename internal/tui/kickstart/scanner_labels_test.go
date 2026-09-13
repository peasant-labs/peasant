package kickstart_test

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
)

//go:embed testdata/scanner_labels.yaml
var scannerLabelData []byte

// scannerLabelProject is one discovered project row: its working directory
// relative to the run's temporary root, its discovery project name, and its
// recorded Git remote. Git projects are initialized as real repositories whose
// origin is gitRemote, so the runner measures the production repository
// resolver instead of a fixture identity.
type scannerLabelProject struct {
	WorkingDir  string `yaml:"workingDir"`
	ProjectName string `yaml:"projectName"`
	GitRemote   string `yaml:"gitRemote"`
	Git         bool   `yaml:"git"`
}

// scannerLabelCase is one discovery cohort and the exact project labels the
// scanner must render for it. The remaining fields are optional: a case
// declares one only when the label alone cannot carry the behavior it pins.
type scannerLabelCase struct {
	Name                       string                `yaml:"name"`
	Projects                   []scannerLabelProject `yaml:"projects"`
	ExpectedLabels             []string              `yaml:"expectedLabels"`
	ExpectedIdentityPrefix     string                `yaml:"expectedIdentityPrefix"`
	ExpectedRemoteMultiplicity string                `yaml:"expectedRemoteMultiplicity"`
	ExpectedNameMultiplicity   string                `yaml:"expectedNameMultiplicity"`
}

// scannerLabelDocument is the whole label matrix plus its deletion-protection
// manifest. RequiredCaseNames is not a row count: it names every case that
// must remain present, and adding a new case never requires touching this
// list.
type scannerLabelDocument struct {
	RequiredCaseNames []string           `yaml:"requiredCaseNames"`
	Cases             []scannerLabelCase `yaml:"cases"`
}

func loadScannerLabels(t *testing.T) scannerLabelDocument {
	t.Helper()
	var doc scannerLabelDocument
	if err := decodeStrictFixture(scannerLabelData, &doc); err != nil {
		t.Fatalf("decode testdata/scanner_labels.yaml: %v", err)
	}
	present := make(map[string]bool, len(doc.Cases))
	for _, testCase := range doc.Cases {
		if present[testCase.Name] {
			t.Fatalf("scanner label fixture repeats case name %q", testCase.Name)
		}
		present[testCase.Name] = true
	}
	if err := testutil.RequireFixtureNames("scanner label fixture", "case", doc.RequiredCaseNames, present); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range doc.Cases {
		testutil.RequireFixtureFields(t, "scanner label", testCase.Name, []testutil.FixtureField{
			{Key: "name", Value: testCase.Name},
		})
		if len(testCase.Projects) == 0 {
			t.Fatalf("scanner label fixture case %q declares no projects to load", testCase.Name)
		}
		if len(testCase.ExpectedLabels) == 0 {
			t.Fatalf("scanner label fixture case %q declares no expected labels; an empty list turns the case into a guaranteed pass", testCase.Name)
		}
		seenLabels := make(map[string]bool, len(testCase.ExpectedLabels))
		for index, label := range testCase.ExpectedLabels {
			testutil.RequireFixtureFields(t, "scanner label", testCase.Name, []testutil.FixtureField{
				{Key: fmt.Sprintf("expectedLabels[%d]", index), Value: label},
			})
			if seenLabels[label] {
				t.Fatalf("scanner label fixture case %q repeats expected label %q; each label identifies exactly one project", testCase.Name, label)
			}
			seenLabels[label] = true
		}
		for index, project := range testCase.Projects {
			testutil.RequireFixtureFields(t, "scanner label", testCase.Name, []testutil.FixtureField{
				{Key: fmt.Sprintf("projects[%d].workingDir", index), Value: project.WorkingDir},
			})
			if path.IsAbs(project.WorkingDir) || path.Clean(project.WorkingDir) != project.WorkingDir {
				t.Fatalf("scanner label fixture case %q project %d workingDir %q must be a clean path relative to the temporary root", testCase.Name, index, project.WorkingDir)
			}
			if project.Git && project.GitRemote == "" {
				t.Fatalf("scanner label fixture case %q project %d declares git: true with no gitRemote", testCase.Name, index)
			}
			if !project.Git && project.GitRemote != "" {
				t.Fatalf("scanner label fixture case %q project %d declares gitRemote %q without git: true", testCase.Name, index, project.GitRemote)
			}
		}
	}
	return doc
}

// scannerLabelTempRootToken marks the run's temporary root inside a fixture
// gitRemote, so a local-path remote is absolute at load time while the fixture
// itself stays machine-independent.
const scannerLabelTempRootToken = "TEMP_ROOT"

// TestScannerTreeSource_ProjectLabelMatrix proves the production scanner's
// project-row labels over the complete remote x name matrix the fixture YAML
// holds: the fixture supplies every cohort's working directories, discovery
// names, remotes, and exact expected labels, and the runner asserts the
// rendered label set equals it — every expected label on exactly one root,
// nothing extra. This runner keeps the real temporary-directory and real Git
// repository setup and executes the production ScannerTreeSource.Load.
func TestScannerTreeSource_ProjectLabelMatrix(t *testing.T) {
	t.Parallel()
	doc := loadScannerLabels(t)
	for _, testCase := range doc.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			roots, err := kickstart.NewScannerTreeSource(scannerLabelListings(t, testCase, root)).Load(context.Background())
			if err != nil {
				t.Fatalf("scanner load: %v", err)
			}
			want := make(map[string]bool, len(testCase.ExpectedLabels))
			for _, label := range testCase.ExpectedLabels {
				want[filepath.FromSlash(label)] = true
			}
			if len(roots) != len(want) {
				t.Fatalf("project roots = %d, want %d rendering labels %v", len(roots), len(want), testCase.ExpectedLabels)
			}
			actual := make(map[string]bool, len(roots))
			rendered := make([]string, 0, len(roots))
			for _, project := range roots {
				if actual[project.Label] {
					t.Errorf("project label %q is carried by more than one root; the fixture expects each label on exactly one project", project.Label)
				}
				actual[project.Label] = true
				rendered = append(rendered, project.Label)
				if !want[project.Label] {
					t.Errorf("unexpected project label %q; want one of the fixture labels %v", project.Label, testCase.ExpectedLabels)
				}
				if prefix := testCase.ExpectedIdentityPrefix; prefix != "" && !strings.HasPrefix(project.Meta[settings.MetaProjectIdentity], prefix) {
					t.Errorf("project %q identity = %q, want prefix %q", project.Label, project.Meta[settings.MetaProjectIdentity], prefix)
				}
				if testCase.ExpectedRemoteMultiplicity != "" || testCase.ExpectedNameMultiplicity != "" {
					session := firstScannerLabelSession(project)
					if session == nil {
						t.Fatalf("project %q has no session leaf to carry multiplicity evidence", project.Label)
					}
					if testCase.ExpectedRemoteMultiplicity != "" && session.Meta[settings.MetaRemoteMultiplicity] != testCase.ExpectedRemoteMultiplicity {
						t.Errorf("project %q session %q remote multiplicity = %q, want %q",
							project.Label, session.ID, session.Meta[settings.MetaRemoteMultiplicity], testCase.ExpectedRemoteMultiplicity)
					}
					if testCase.ExpectedNameMultiplicity != "" && session.Meta[settings.MetaNameMultiplicity] != testCase.ExpectedNameMultiplicity {
						t.Errorf("project %q session %q name multiplicity = %q, want %q",
							project.Label, session.ID, session.Meta[settings.MetaNameMultiplicity], testCase.ExpectedNameMultiplicity)
					}
				}
			}
			for _, label := range testCase.ExpectedLabels {
				if !actual[filepath.FromSlash(label)] {
					t.Errorf("missing expected project label %q; the scanner rendered %v", label, rendered)
				}
			}
		})
	}
}

// scannerLabelListings materializes one fixture case against the run's real
// temporary root: it creates each working directory, initializes the declared
// Git repositories, and builds the discovery listings the production scanner
// consumes.
func scannerLabelListings(t *testing.T, testCase scannerLabelCase, root string) []ftue.SessionListing {
	t.Helper()
	listings := make([]ftue.SessionListing, 0, len(testCase.Projects))
	for index, project := range testCase.Projects {
		workingDir := filepath.Join(root, filepath.FromSlash(project.WorkingDir))
		remote := scannerLabelRemote(project.GitRemote, root)
		if project.Git {
			initGitProject(t, workingDir, remote)
		} else if err := os.MkdirAll(workingDir, 0o755); err != nil {
			t.Fatalf("create project directory %q: %v", workingDir, err)
		}
		listings = append(listings, ftue.SessionListing{
			Harness:     "claude-code",
			ProjectName: project.ProjectName,
			GitRemote:   remote,
			Branch:      "main",
			SessionID:   fmt.Sprintf("%s-%d", testCase.Name, index),
			WorkingDir:  workingDir,
		})
	}
	return listings
}

// scannerLabelRemote resolves the fixture's temporary-root token in a recorded
// remote; a canonical remote passes through unchanged.
func scannerLabelRemote(remote, root string) string {
	suffix, local := strings.CutPrefix(remote, scannerLabelTempRootToken+"/")
	if !local {
		return remote
	}
	return filepath.Join(root, filepath.FromSlash(suffix))
}

// firstScannerLabelSession returns the first session leaf under a project row:
// a branch row contributes its children, while a flattened branchless project
// holds its sessions directly.
func firstScannerLabelSession(project *kit.TreeNode) *kit.TreeNode {
	if len(project.Children) == 0 {
		return nil
	}
	child := project.Children[0]
	if child.Meta[settings.MetaHarness] != "" {
		return child
	}
	if len(child.Children) == 0 {
		return nil
	}
	return child.Children[0]
}

// initGitProject creates a real Git working directory at clone and adds remote,
// so scanner labels are measured against the production repository resolver
// rather than a fixture identity.
func initGitProject(t *testing.T, clone, remote string) {
	t.Helper()
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatalf("create Git project directory %q: %v", clone, err)
	}
	runRepositoryGit(t, clone, "init", "--initial-branch=main")
	runRepositoryGit(t, clone, "remote", "add", "origin", remote)
}
