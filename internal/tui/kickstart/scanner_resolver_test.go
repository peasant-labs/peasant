package kickstart_test

import (
	"fmt"
	"path/filepath"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

// preResolvedFixtureResolver accepts the absolute physical spellings in pure
// YAML fixtures without consulting the test machine. Filesystem behavior is
// covered separately with the production resolver and real temporary paths.
type preResolvedFixtureResolver struct{}

func (preResolvedFixtureResolver) Resolve(dir string) (ingest.ClonePath, error) {
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return "", fmt.Errorf("fixture path %q is not a clean absolute path", dir)
	}
	return ingest.ClonePath(dir), nil
}

func withFixturePathResolver() kickstart.ScannerOption {
	return kickstart.WithPathIdentityResolver(preResolvedFixtureResolver{})
}

var _ ingest.PathIdentityResolver = preResolvedFixtureResolver{}
