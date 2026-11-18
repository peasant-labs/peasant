package moduleboundary_test

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
)

const fixtureDirectory = "testdata"

// TestUpgradeImportBoundary prevents the planner from becoming a dependency
// of either side it joins. Only command entry points may consume it.
func TestUpgradeImportBoundary(t *testing.T) {
	root := moduleRoot(t)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case defaults.ExtGit.String(), "vendor", fixtureDirectory, "node_modules":
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
			if name == "github.com/peasant-labs/peasant/internal/upgrade" ||
				strings.HasPrefix(name, "github.com/peasant-labs/peasant/internal/upgrade/") {
				return fmt.Errorf("%s imports planner %s; only command entry points may import it", rel, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
