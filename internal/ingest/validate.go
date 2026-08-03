package ingest

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ValidatePathComponent checks that a single path component is safe
// for use in filesystem paths. It rejects empty strings, ".", "..",
// and components containing path separators.
func ValidatePathComponent(component string) error {
	if component == "" {
		return fmt.Errorf("invalid path component: empty string")
	}
	if component == "." || component == ".." {
		return fmt.Errorf("invalid path component %q: must not be '.' or '..'", component)
	}
	if strings.ContainsAny(component, "/\\") {
		return fmt.Errorf("invalid path component %q: must not contain path separators", component)
	}
	return nil
}

// ValidatePathContainment verifies that targetPath is contained within basePath.
// Both paths must be cleaned absolute paths. Returns an error if the target
// escapes the base directory.
func ValidatePathContainment(basePath, targetPath string) error {
	if !filepath.IsAbs(basePath) {
		return fmt.Errorf("basePath %q must be absolute", basePath)
	}
	if !filepath.IsAbs(targetPath) {
		return fmt.Errorf("targetPath %q must be absolute", targetPath)
	}
	rel, err := filepath.Rel(basePath, targetPath)
	if err != nil {
		return fmt.Errorf("path containment check failed: %w", err)
	}
	if strings.HasPrefix(rel, "..") {
		return fmt.Errorf("path %q escapes base directory %q (relative: %q)", targetPath, basePath, rel)
	}
	return nil
}
