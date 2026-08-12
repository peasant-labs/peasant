package ingest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ClonePath is a resolved absolute physical directory path. It is project
// identity data, not display text.
type ClonePath string

// String returns the resolved physical path.
func (p ClonePath) String() string { return string(p) }

// RepositoryCohortKey is opaque transient grouping evidence. It identifies one
// logical repository cohort without claiming to be a filesystem path. Persisted
// selection and every destructive or publishing boundary continue to use
// ClonePath.
type RepositoryCohortKey string

// String returns the opaque repository cohort key.
func (k RepositoryCohortKey) String() string { return string(k) }

// RepositoryPath is a resolved physical Git directory used for diagnostics and
// previews. It is never a grouping key and never enters persisted selection.
type RepositoryPath string

// String returns the resolved physical Git directory path.
func (p RepositoryPath) String() string { return string(p) }

// RepositoryIdentity keeps logical grouping separate from the physical Git
// directory that explains the resolved topology. Values are copied across
// boundaries so callers cannot mutate resolver-owned state.
type RepositoryIdentity struct {
	CohortKey    RepositoryCohortKey
	GitDirectory RepositoryPath
}

// String returns only the opaque grouping key. The physical Git directory must
// be selected explicitly through GitDirectory before it can be displayed or
// passed to path-aware code.
func (i RepositoryIdentity) String() string { return i.CohortKey.String() }

// PathIdentityResolver resolves a directory spelling to its physical clone
// identity.
type PathIdentityResolver interface {
	Resolve(dir string) (ClonePath, error)
}

type physicalPathResolver struct{}

var _ PathIdentityResolver = physicalPathResolver{}

// NewPhysicalPathResolver returns the production physical-path resolver.
func NewPhysicalPathResolver() PathIdentityResolver {
	return physicalPathResolver{}
}

// Resolve returns a cleaned absolute path after resolving symbolic links.
func (physicalPathResolver) Resolve(dir string) (ClonePath, error) {
	if dir == "" {
		return "", newPathIdentityError(dir, "path is empty", "pass an existing absolute project directory and try again", nil)
	}
	if !filepath.IsAbs(dir) {
		return "", newPathIdentityError(dir, "path is relative", "pass the absolute project directory and try again", nil)
	}

	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", newPathIdentityError(dir, "the absolute path could not be calculated", "check the path and current filesystem access, then try again", err)
	}
	physical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", newPathIdentityError(dir, "directory does not exist", "restore the project directory or select an available clone, then try again", err)
		}
		return "", newPathIdentityError(dir, "symbolic links could not be resolved", "repair the symbolic link chain or select the physical project directory, then try again", err)
	}
	physical = filepath.Clean(physical)
	info, err := os.Stat(physical)
	if err != nil {
		return "", newPathIdentityError(dir, "resolved directory could not be inspected", "restore access to the project directory and try again", err)
	}
	if !info.IsDir() {
		return "", newPathIdentityError(dir, "resolved path is not a directory", "pass the absolute path of a project directory and try again", nil)
	}
	return ClonePath(physical), nil
}

func newPathIdentityError(dir, reason, fix string, cause error) error {
	message := fmt.Sprintf(
		"resolve clone path %q: what: Peasant could not create a physical clone identity; why: %s; where: PathIdentityResolver.Resolve in internal/ingest/path_identity.go; when: while preparing project discovery identity; meaning: this path cannot select or distinguish a local clone; fix: %s",
		dir,
		reason,
		fix,
	)
	if cause == nil {
		return errors.New(message)
	}
	return fmt.Errorf("%s: %w", message, cause)
}
