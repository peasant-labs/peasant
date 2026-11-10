package discovery_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGroupingLivesOnlyInBuildForest(t *testing.T) {
	t.Parallel()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate grouping guard: caller path is unavailable")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	var declarations []string
	var duplicates []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		if strings.Contains(path, string(filepath.Separator)+".") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := string(data)
		build := "func " + "BuildForest("
		duplicate := "func " + "buildForest("
		if strings.Contains(text, build) {
			declarations = append(declarations, path)
		}
		if strings.Contains(text, duplicate) {
			duplicates = append(duplicates, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk grouping declarations: %v", err)
	}
	if len(declarations) != 1 || !strings.HasSuffix(filepath.ToSlash(declarations[0]), "internal/discovery/forest.go") {
		t.Fatalf("BuildForest declarations=%v, want only internal/discovery/forest.go", declarations)
	}
	if len(duplicates) != 0 {
		t.Fatalf("a second grouping implementation remains in %v", duplicates)
	}
}
