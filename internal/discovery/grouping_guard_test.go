package discovery_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// moduleRoot locates the module root the guard walks. `go test` runs the test
// binary in its package directory, so the root is two levels up even under
// -trimpath, where runtime.Caller reports a module-relative path that no
// filesystem resolves. The module root is absolute so the scan compares it
// with the walk's paths without depending on the caller's working directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	// The walk below skips hidden directories by matching a "/." segment, so the
	// root itself must be absolute: a "../.." root would make every path look
	// hidden.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve the working directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("locate the module root: %s holds no accessible go.mod: %v", root, err)
	}
	return root
}

// scanGroupingTree walks root for non-test Go files and collects the two
// grouping declaration forms the guard owns. A nested directory with a regular
// go.mod is another module: Go does not build it as part of this module, so the
// walk stops there. Hidden files and directories, and _test.go files, are not
// read. Walk and read errors are returned; no directory is dropped silently.
func scanGroupingTree(root string) (declarations, duplicates []string, err error) {
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != root && strings.HasPrefix(entry.Name(), ".") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if path == root {
				return nil
			}
			nested, nestedErr := nestedModule(path)
			if nestedErr != nil {
				return nestedErr
			}
			if nested {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
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
	return declarations, duplicates, err
}

// nestedModule reports whether dir starts a Go module of its own. A regular
// go.mod file is the boundary evidence: the parent module does not build a
// nested module, so its declarations are not part of this source tree. Any
// error other than the marker being absent is returned, not treated as a
// directory to scan.
func nestedModule(dir string) (bool, error) {
	marker := filepath.Join(dir, "go.mod")
	info, err := os.Stat(marker)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read nested module marker %s: %w", marker, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("nested module marker %s is not a regular file, so the grouping guard cannot tell whether Go builds this directory; make go.mod a file or remove it, then retry", marker)
	}
	return true, nil
}

// groupingGuardViolation applies the guard's policy to one scan.
func groupingGuardViolation(declarations, duplicates []string) error {
	if len(declarations) != 1 || !strings.HasSuffix(filepath.ToSlash(declarations[0]), "internal/discovery/forest.go") {
		return fmt.Errorf("BuildForest declarations=%v, want only internal/discovery/forest.go", declarations)
	}
	if len(duplicates) != 0 {
		return fmt.Errorf("a second grouping implementation remains in %v", duplicates)
	}
	return nil
}

func TestGroupingLivesOnlyInBuildForest(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	declarations, duplicates, err := scanGroupingTree(root)
	if err != nil {
		t.Fatalf("walk grouping declarations: %v", err)
	}
	if err := groupingGuardViolation(declarations, duplicates); err != nil {
		t.Fatal(err)
	}
}
