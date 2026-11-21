package discovery_test

import (
	_ "embed"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/grouping_guard.yaml
var groupingGuardData []byte

const groupingGuardFixture = "testdata/grouping_guard.yaml"
const groupingGuardTrees = "testdata/grouping_guard"

type groupingGuardCase struct {
	Name              string   `yaml:"name"`
	Root              string   `yaml:"root"`
	WantDeclarations  []string `yaml:"wantDeclarations"`
	WantDuplicates    []string `yaml:"wantDuplicates"`
	WantViolation     bool     `yaml:"wantViolation"`
	WantErrorContains string   `yaml:"wantErrorContains"`
}

type groupingGuardDocument struct {
	RequiredNames []string            `yaml:"requiredNames"`
	Cases         []groupingGuardCase `yaml:"cases"`
}

// TestGroupingGuardModuleBoundary is the direct helper test for the grouping
// guard's module boundary. The repository root cannot hold the nested-worktree
// shapes this rule handles without changing the real worktrees it already
// contains, so the guard's own scan and policy run over small fixture trees.
// It copies no algorithm and starts no process; each fixture root is itself a
// nested module, so the real module walk skips these fixtures.
func TestGroupingGuardModuleBoundary(t *testing.T) {
	t.Parallel()
	var document groupingGuardDocument
	if err := testutil.DecodeNamedFixtureYAML(groupingGuardData, &document); err != nil {
		t.Fatalf("decode %s: %v", groupingGuardFixture, err)
	}
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(groupingGuardTrees, row.Root)
			scanRoot := root
			if row.WantErrorContains == "" {
				// Scan a copy outside the repository's Git metadata, so every
				// eligible file is untracked: the guard walks the filesystem,
				// not an index.
				scanRoot = t.TempDir()
				if err := os.CopyFS(scanRoot, os.DirFS(root)); err != nil {
					t.Fatalf("copy fixture %s: %v", root, err)
				}
			}
			declarations, duplicates, err := scanGroupingTree(scanRoot)
			if row.WantErrorContains != "" {
				if err == nil {
					t.Fatalf("scan %s succeeded, want an error containing %q", scanRoot, row.WantErrorContains)
				}
				if !strings.Contains(err.Error(), row.WantErrorContains) {
					t.Fatalf("scan %s error = %v, want it to contain %q", scanRoot, err, row.WantErrorContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("scan %s: %v", scanRoot, err)
			}
			gotDeclarations := relativeGuardPaths(t, scanRoot, declarations)
			gotDuplicates := relativeGuardPaths(t, scanRoot, duplicates)
			wantDeclarations := append([]string(nil), row.WantDeclarations...)
			slices.Sort(wantDeclarations)
			wantDuplicates := append([]string(nil), row.WantDuplicates...)
			slices.Sort(wantDuplicates)
			if !slices.Equal(gotDeclarations, wantDeclarations) {
				t.Errorf("BuildForest declarations = %v, want %v", gotDeclarations, wantDeclarations)
			}
			if !slices.Equal(gotDuplicates, wantDuplicates) {
				t.Errorf("buildForest duplicates = %v, want %v", gotDuplicates, wantDuplicates)
			}
			violation := groupingGuardViolation(declarations, duplicates)
			if row.WantViolation && violation == nil {
				t.Errorf("scan reported no violation; want one (declarations=%v, duplicates=%v)", gotDeclarations, gotDuplicates)
			}
			if !row.WantViolation && violation != nil {
				t.Errorf("scan reported a violation: %v", violation)
			}
		})
	}
}

// relativeGuardPaths makes a scan's paths relative to its fixture root and
// sorts them, so a case can name the files it expects in a stable order.
func relativeGuardPaths(t *testing.T, root string, paths []string) []string {
	t.Helper()
	relative := make([]string, 0, len(paths))
	for _, path := range paths {
		value, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("make %s relative to %s: %v", path, root, err)
		}
		relative = append(relative, filepath.ToSlash(value))
	}
	slices.Sort(relative)
	return relative
}
