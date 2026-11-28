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

const fixtureDirectory = "testdata"

//go:embed testdata/upgrade_boundary.yaml
var upgradeBoundaryYAML []byte

type upgradeBoundaryFile struct {
	Path    string `yaml:"path"`
	Content string `yaml:"content"`
}

type upgradeBoundaryCase struct {
	Name          string                `yaml:"name"`
	Files         []upgradeBoundaryFile `yaml:"files"`
	ErrorContains string                `yaml:"errorContains"`
}

type upgradeBoundaryFixture struct {
	RequiredNames []string              `yaml:"requiredNames"`
	Cases         []upgradeBoundaryCase `yaml:"cases"`
}

// TestUpgradeImportBoundary prevents the planner from becoming a dependency
// of either side it joins. Only command entry points may consume it.
func TestUpgradeImportBoundary(t *testing.T) {
	if err := checkUpgradeImportBoundary(moduleRoot(t)); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeImportBoundaryFixture(t *testing.T) {
	var fixture upgradeBoundaryFixture
	if err := testutil.DecodeFixtureYAML(upgradeBoundaryYAML, &fixture); err != nil {
		t.Fatalf("decode upgrade boundary fixture: %v", err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		if tc.Name == "" || present[tc.Name] {
			t.Fatalf("upgrade boundary fixture has an unnamed or duplicate case %q", tc.Name)
		}
		present[tc.Name] = true
	}
	if err := testutil.RequireFixtureNames("upgrade boundary fixture", "case", fixture.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			root := t.TempDir()
			for _, file := range tc.Files {
				target := filepath.Join(root, filepath.FromSlash(file.Path))
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte(file.Content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := checkUpgradeImportBoundary(root)
			if tc.ErrorContains == "" {
				if err != nil {
					t.Fatalf("checkUpgradeImportBoundary(%s) = %v, want nil", tc.Name, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.ErrorContains) {
				t.Fatalf("checkUpgradeImportBoundary(%s) = %v, want error containing %q", tc.Name, err, tc.ErrorContains)
			}
		})
	}
}

func checkUpgradeImportBoundary(root string) error {
	cleanRoot := filepath.Clean(root)
	return filepath.WalkDir(cleanRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case defaults.ExtGit.String(), "vendor", fixtureDirectory, "node_modules":
				return filepath.SkipDir
			}
			if path != cleanRoot {
				nested, err := isNestedCheckoutOrModule(path)
				if err != nil {
					return err
				}
				if nested {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		rel, err := filepath.Rel(cleanRoot, path)
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
			if name == "github.com/peasant-labs/peasant/internal/upgrade" ||
				strings.HasPrefix(name, "github.com/peasant-labs/peasant/internal/upgrade/") {
				return fmt.Errorf("%s imports planner %s; only command entry points may import it", rel, name)
			}
		}
		return nil
	})
}

func isNestedCheckoutOrModule(dir string) (bool, error) {
	for _, marker := range []string{defaults.ExtGit.String(), "go.mod"} {
		_, err := os.Lstat(filepath.Join(dir, marker))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}
