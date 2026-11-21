package moduleboundary_test

import (
	_ "embed"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
)

const (
	fixtureDirectory  = "testdata"
	goModFile         = "go.mod"
	upgradeImportPath = "github.com/peasant-labs/peasant/internal/upgrade"
)

//go:embed testdata/upgrade_boundary.yaml
var upgradeBoundaryFixtureYAML []byte

type upgradeBoundaryFixture struct {
	RequiredNames []string              `yaml:"requiredNames"`
	Cases         []upgradeBoundaryCase `yaml:"cases"`
}

type upgradeBoundaryCase struct {
	Name      string               `yaml:"name"`
	Files     []upgradeFixtureFile `yaml:"files"`
	Violation string               `yaml:"violation"`
}

type upgradeFixtureFile struct {
	Path    string `yaml:"path"`
	Content string `yaml:"content"`
}

// TestUpgradeImportBoundary prevents the planner from becoming a dependency
// of either side it joins. Only command entry points may consume it.
func TestUpgradeImportBoundary(t *testing.T) {
	if err := walkUpgradeImports(moduleRoot(t)); err != nil {
		t.Fatal(err)
	}
}

// TestUpgradeImportWalkScopesToTheCurrentModule runs the walk over built module
// trees. A nested worktree, repository, or module is another checkout's code, so
// the walk must not enter it, while a forbidden import in the current module
// still fails.
func TestUpgradeImportWalkScopesToTheCurrentModule(t *testing.T) {
	var fixture upgradeBoundaryFixture
	if err := testutil.DecodeNamedFixtureYAML(upgradeBoundaryFixtureYAML, &fixture); err != nil {
		t.Fatalf("decode embedded upgrade boundary fixture: %v", err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			root := t.TempDir()
			writeUpgradeFixtureFile(t, root, upgradeFixtureFile{Path: goModFile, Content: "module github.com/peasant-labs/peasant\n"})
			writeUpgradeFixtureFile(t, root, upgradeFixtureFile{Path: ".git/HEAD", Content: "ref: refs/heads/develop\n"})
			for _, file := range tc.Files {
				writeUpgradeFixtureFile(t, root, file)
			}
			err := walkUpgradeImports(root)
			if tc.Violation == "" {
				if err != nil {
					t.Fatalf("walk reported %v; want no violation", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.Violation) {
				t.Fatalf("walk error = %v; want a violation naming %s", err, tc.Violation)
			}
		})
	}
}

func writeUpgradeFixtureFile(t *testing.T, root string, file upgradeFixtureFile) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(file.Path))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(file.Content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// walkUpgradeImports returns the first walk error or the first Go file under
// root, outside the command entry points and the planner itself, that imports
// the planner. It does not enter a subdirectory that holds its own .git entry
// or go.mod: that is a nested worktree, repository, or module, not this module.
func walkUpgradeImports(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case defaults.ExtGit.String(), "vendor", fixtureDirectory, "node_modules":
				return filepath.SkipDir
			}
			if path == root {
				return nil
			}
			nested, err := holdsNestedCheckout(path)
			if err != nil {
				return err
			}
			if nested {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "cmd/") || strings.HasPrefix(rel, "internal/upgrade/") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("inspect imports in %s: %w", rel, err)
		}
		for _, imported := range file.Imports {
			name, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return fmt.Errorf("read import in %s: %w", rel, err)
			}
			if name == upgradeImportPath || strings.HasPrefix(name, upgradeImportPath+"/") {
				return fmt.Errorf("%s imports planner %s; only command entry points may import it", rel, name)
			}
		}
		return nil
	})
}

// holdsNestedCheckout reports whether dir holds a .git entry (a worktree's
// file or a repository's directory) or a go.mod. Any error other than a
// missing entry fails the scan, so the walk never skips a subtree it cannot
// classify.
func holdsNestedCheckout(dir string) (bool, error) {
	for _, marker := range []string{defaults.ExtGit.String(), goModFile} {
		_, err := os.Lstat(filepath.Join(dir, marker))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("classify %s: %w", dir, err)
		}
	}
	return false, nil
}
