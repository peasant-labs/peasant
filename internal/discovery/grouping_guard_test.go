package discovery_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// moduleRoot locates the module root the guard walks. `go test` runs the test
// binary in its package directory, so the root is two levels up even under
// -trimpath, where runtime.Caller reports a module-relative path that no
// filesystem resolves.
func moduleRoot(t *testing.T) string {
	t.Helper()
	// The walk below skips hidden directories by matching a "/." segment, so the
	// root itself must be absolute: a "../.." root would make every path look
	// hidden.
	working, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve the working directory: %v", err)
	}
	candidates := []string{working}
	if _, file, _, ok := runtime.Caller(0); ok && filepath.IsAbs(file) {
		candidates = append(candidates, filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..")))
	}
	for _, root := range candidates {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			return root
		}
	}
	t.Fatalf("locate the module root: none of %v holds go.mod", candidates)
	return ""
}

func TestGroupingLivesOnlyInBuildForest(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
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
