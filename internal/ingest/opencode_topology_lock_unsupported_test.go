//go:build !linux && !darwin

package ingest_test

import (
	"fmt"
	"runtime"
)

const openCodeBuildTopologyOwnershipSupported = false

// acquireOpenCodeBuildTopologyOwnership refuses before it touches the
// filesystem: without a safe exclusive directory lock, the startup sweep could
// delete a package another live run is still using.
func acquireOpenCodeBuildTopologyOwnership(sourceDirectory string) (func() error, error) {
	return nil, fmt.Errorf("acquire exclusive build-topology startup ownership of %q: GOOS %s has no safe exclusive directory lock in these test helpers; no files were opened, read, swept or created, so the build-topology guards cannot run here; run the topology tests in a Linux or macOS checkout or container (separate worktrees alone do not enable this platform)", sourceDirectory, runtime.GOOS)
}
